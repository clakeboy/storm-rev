package storm

import (
	"bytes"
	"encoding/base64"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
	blevequery "github.com/blevesearch/bleve/v2/search/query"
	"github.com/blevesearch/bleve/v2/util"
	"github.com/clakeboy/storm-rev/v2/codec"
	"github.com/clakeboy/storm-rev/v2/index"
	bolt "go.etcd.io/bbolt"
)

const (
	bleveIndexDirSuffix = "_index"
	bleveIndexSuffix    = ".bleve"
	bleveMaxResults     = math.MaxInt32

	bleveExactPrefix     = "storm_exact_"
	bleveValuePrefix     = "storm_value_"
	bleveTextPrefix      = "storm_text_"
	bleveHasPrefix       = "storm_has_"
	bleveCompositePrefix = "storm_composite_"
)

// bleveIndexManager owns all external Bleve indexes for one DB instance.
// Bolt remains the source of truth; this manager only maintains rebuildable search data.
type bleveIndexManager struct {
	root  string
	codec codec.MarshalUnmarshaler

	mu      sync.Mutex
	indexes map[string]bleve.Index
	dirty   map[string]bool
}

// newBleveIndexManager builds the external index manager rooted next to the Bolt DB file.
func newBleveIndexManager(dbPath string, c codec.MarshalUnmarshaler) *bleveIndexManager {
	return &bleveIndexManager{
		root:    indexRootDir(dbPath),
		codec:   c,
		indexes: make(map[string]bleve.Index),
		dirty:   make(map[string]bool),
	}
}

// indexRootDir returns the per-database index root directory.
// Example: /path/app.db -> /path/app_db_index.
func indexRootDir(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), safeIndexName(filepath.Base(dbPath))+bleveIndexDirSuffix)
}

// close releases all opened Bleve indexes before the Bolt database is closed.
func (m *bleveIndexManager) close() error {
	if m == nil {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	var firstErr error
	for name, idx := range m.indexes {
		if err := idx.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(m.indexes, name)
	}
	return firstErr
}

// initTable creates or opens the Bleve index for a table.
func (m *bleveIndexManager) initTable(cfg *structConfig) error {
	if m == nil {
		return nil
	}

	_, err := m.tableIndex(cfg)
	return err
}

// dropTable closes and removes the external index for a table.
func (m *bleveIndexManager) dropTable(tableName string) error {
	if m == nil {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if idx := m.indexes[tableName]; idx != nil {
		if err := idx.Close(); err != nil {
			return err
		}
		delete(m.indexes, tableName)
	}
	delete(m.dirty, tableName)
	return os.RemoveAll(m.tablePath(tableName))
}

// markDirty records that a table index may not match Bolt and should be rebuilt.
func (m *bleveIndexManager) markDirty(tableName string) {
	if m == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.dirty[tableName] = true
}

// clearDirty marks a rebuilt table index as consistent again.
func (m *bleveIndexManager) clearDirty(tableName string) {
	if m == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.dirty, tableName)
}

// isDirty reports whether a table should avoid trusting Bleve query results.
func (m *bleveIndexManager) isDirty(tableName string) bool {
	if m == nil {
		return true
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dirty[tableName]
}

// tableIndex opens an existing index or creates a new one with the current mapping.
func (m *bleveIndexManager) tableIndex(cfg *structConfig) (bleve.Index, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if idx := m.indexes[cfg.Name]; idx != nil {
		return idx, ensureBleveMappingFields(idx, cfg)
	}

	if err := os.MkdirAll(m.root, 0o755); err != nil {
		return nil, err
	}

	path := m.tablePath(cfg.Name)
	idx, err := bleve.Open(path)
	if err != nil {
		idx, err = bleve.New(path, buildBleveMapping(cfg))
		if err != nil {
			return nil, err
		}
	} else if err := ensureBleveMappingFields(idx, cfg); err != nil {
		if closeErr := idx.Close(); closeErr != nil {
			return nil, closeErr
		}
		return nil, err
	}

	m.indexes[cfg.Name] = idx
	return idx, nil
}

// recreateTable drops a table index directory and recreates it with a fresh mapping.
func (m *bleveIndexManager) recreateTable(cfg *structConfig) (bleve.Index, error) {
	if m == nil {
		return nil, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if idx := m.indexes[cfg.Name]; idx != nil {
		if err := idx.Close(); err != nil {
			return nil, err
		}
		delete(m.indexes, cfg.Name)
	}

	path := m.tablePath(cfg.Name)
	if err := os.RemoveAll(path); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(m.root, 0o755); err != nil {
		return nil, err
	}

	idx, err := bleve.New(path, buildBleveMapping(cfg))
	if err != nil {
		return nil, err
	}
	m.indexes[cfg.Name] = idx
	delete(m.dirty, cfg.Name)
	return idx, nil
}

// tablePath returns the filesystem path for one table's index directory.
func (m *bleveIndexManager) tablePath(tableName string) string {
	return filepath.Join(m.root, safeIndexName(tableName)+bleveIndexSuffix)
}

// safeIndexName keeps table-derived filenames stable and path-safe.
func safeIndexName(name string) string {
	if name == "" {
		return "_"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// buildBleveMapping creates exact, sortable, and marker fields for all declared indexes.
func buildBleveMapping(cfg *structConfig) *mapping.IndexMappingImpl {
	indexMapping := bleve.NewIndexMapping()
	docMapping := bleve.NewDocumentStaticMapping()

	for field, fm := range requiredBleveFieldMappings(cfg) {
		docMapping.AddFieldMappingsAt(field, fm)
	}

	indexMapping.DefaultMapping = docMapping
	return indexMapping
}

// requiredBleveFieldMappings returns the exact Bleve fields needed to index cfg.
func requiredBleveFieldMappings(cfg *structConfig) map[string]*mapping.FieldMapping {
	fields := make(map[string]*mapping.FieldMapping)
	add := func(field string, fm *mapping.FieldMapping) {
		if fields[field] != nil {
			return
		}
		fm.Store = false
		fm.IncludeInAll = false
		fields[field] = fm
	}

	for _, field := range cfg.Fields {
		if !field.Indexed() {
			continue
		}
		add(exactField(field.Name), bleve.NewKeywordFieldMapping())
		add(valueField(field.Name), valueFieldMapping(cfg, field))
		if field.Index == tagFullText {
			add(textField(field.Name), bleve.NewTextFieldMapping())
		}
		add(hasField(field.Name), bleve.NewBooleanFieldMapping())
	}

	for name := range cfg.CompositeIndexes {
		add(compositeField(name), bleve.NewBooleanFieldMapping())
	}

	return fields
}

// ensureBleveMappingFields extends an existing index mapping for newly added indexed fields.
func ensureBleveMappingFields(idx bleve.Index, cfg *structConfig) error {
	indexMapping, ok := idx.Mapping().(*mapping.IndexMappingImpl)
	if !ok {
		return ErrIncompatibleValue
	}
	if indexMapping.DefaultMapping == nil {
		return ErrIncompatibleValue
	}

	var changed bool
	for name, required := range requiredBleveFieldMappings(cfg) {
		current := bleveFieldMapping(indexMapping.DefaultMapping, name)
		if current == nil {
			indexMapping.DefaultMapping.AddFieldMappingsAt(name, required)
			changed = true
			continue
		}
		if !compatibleBleveFieldMapping(current, required) {
			return ErrIncompatibleValue
		}
	}
	if !changed {
		return nil
	}
	if err := indexMapping.Validate(); err != nil {
		return err
	}

	raw, err := util.MarshalJSON(indexMapping)
	if err != nil {
		return err
	}
	return idx.SetInternal(util.MappingInternalKey, raw)
}

// bleveFieldMapping finds the single top-level field mapping created by buildBleveMapping.
func bleveFieldMapping(docMapping *mapping.DocumentMapping, name string) *mapping.FieldMapping {
	if docMapping == nil || docMapping.Properties == nil {
		return nil
	}

	property := docMapping.Properties[name]
	if property == nil || len(property.Fields) == 0 {
		return nil
	}
	return property.Fields[0]
}

// compatibleBleveFieldMapping rejects field type changes that need an explicit ReIndex.
func compatibleBleveFieldMapping(current, required *mapping.FieldMapping) bool {
	if current == nil || required == nil {
		return current == required
	}
	return current.Type == required.Type &&
		current.Analyzer == required.Analyzer &&
		current.DateFormat == required.DateFormat &&
		current.Index == required.Index
}

// valueFieldMapping chooses the field mapping used for range and prefix operations.
func valueFieldMapping(cfg *structConfig, field *fieldConfig) *mapping.FieldMapping {
	typ := fieldValueType(cfg, field)
	if typ == nil {
		return bleve.NewKeywordFieldMapping()
	}

	if typ == reflect.TypeOf(time.Time{}) {
		return bleve.NewDateTimeFieldMapping()
	}

	switch typ.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return bleve.NewNumericFieldMapping()
	case reflect.Bool:
		return bleve.NewBooleanFieldMapping()
	default:
		return bleve.NewKeywordFieldMapping()
	}
}

// fieldValueType returns the declared field type, unwrapping pointers without reading values.
func fieldValueType(cfg *structConfig, field *fieldConfig) reflect.Type {
	if field == nil {
		return nil
	}

	if field.Value != nil && field.Value.IsValid() {
		return derefType(field.Value.Type())
	}
	if cfg == nil || cfg.Type == nil {
		return nil
	}

	if sf, ok := cfg.Type.FieldByName(field.Name); ok {
		return derefType(sf.Type)
	}

	return nil
}

// derefType unwraps pointer declarations while preserving the underlying field type.
func derefType(typ reflect.Type) reflect.Type {
	for typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	return typ
}

// indexRecord writes one record into the table index, replacing any previous version.
func (m *bleveIndexManager) indexRecord(cfg *structConfig, data any, id []byte) error {
	if m == nil {
		return nil
	}

	idx, err := m.tableIndex(cfg)
	if err != nil {
		return err
	}

	doc, err := m.document(cfg, data)
	if err != nil {
		return err
	}

	return idx.Index(encodeDocID(id), doc)
}

// indexRecords writes a group of records for one table through one Bleve batch.
func (m *bleveIndexManager) indexRecords(cfg *structConfig, records []*savedRecord) error {
	if m == nil || len(records) == 0 {
		return nil
	}

	idx, err := m.tableIndex(cfg)
	if err != nil {
		return err
	}

	batch := idx.NewBatch()
	for _, record := range records {
		if record == nil {
			continue
		}
		doc, err := m.document(record.cfg, record.data)
		if err != nil {
			return err
		}
		if err := batch.Index(encodeDocID(record.id), doc); err != nil {
			return err
		}
	}
	return idx.Batch(batch)
}

// deleteRecord removes one document from the table index.
func (m *bleveIndexManager) deleteRecord(cfg *structConfig, id []byte) error {
	if m == nil {
		return nil
	}

	idx, err := m.tableIndex(cfg)
	if err != nil {
		return err
	}
	return idx.Delete(encodeDocID(id))
}

// deleteRecords removes a group of records for one table through one Bleve batch.
func (m *bleveIndexManager) deleteRecords(cfg *structConfig, records []*deletedRecord) error {
	if m == nil || len(records) == 0 {
		return nil
	}

	idx, err := m.tableIndex(cfg)
	if err != nil {
		return err
	}

	batch := idx.NewBatch()
	for _, record := range records {
		if record == nil {
			continue
		}
		batch.Delete(encodeDocID(record.id))
	}
	return idx.Batch(batch)
}

// document converts a model value into the small index document Bleve needs.
func (m *bleveIndexManager) document(cfg *structConfig, data any) (map[string]any, error) {
	ref := reflect.Indirect(reflect.ValueOf(data))
	doc := make(map[string]any)

	for name, field := range cfg.Fields {
		if !field.Indexed() {
			continue
		}

		value := ref.FieldByName(name)
		if !value.IsValid() || isZero(&value) {
			continue
		}

		token, err := exactToken(value.Interface(), m.codec)
		if err != nil {
			return nil, err
		}
		doc[exactField(name)] = token
		doc[hasField(name)] = true

		if v, ok := bleveValue(value); ok {
			doc[valueField(name)] = v
		}
		if field.Index == tagFullText {
			if text, ok := fullTextValue(value); ok {
				doc[textField(name)] = text
			}
		}
	}

	for name, composite := range cfg.CompositeIndexes {
		if compositeReady(ref, composite) {
			doc[compositeField(name)] = true
		}
	}

	return doc, nil
}

// uniqueExists checks a unique index and allows the current document ID to overwrite itself.
func (m *bleveIndexManager) uniqueExists(cfg *structConfig, fieldName string, value any, id []byte) (bool, error) {
	ids, err := m.searchExact(cfg, fieldName, value)
	if err != nil {
		return false, err
	}
	for _, existingID := range ids {
		if !bytes.Equal(existingID, id) {
			return true, nil
		}
	}
	return false, nil
}

// searchExact returns all document IDs matching one exact field value.
func (m *bleveIndexManager) searchExact(cfg *structConfig, fieldName string, value any) ([][]byte, error) {
	token, err := exactToken(value, m.codec)
	if err != nil {
		return nil, err
	}
	query := bleve.NewTermQuery(token)
	query.SetField(exactField(fieldName))
	return m.search(cfg, query)
}

// searchAllByIndex returns all IDs that contain the requested indexed field.
func (m *bleveIndexManager) searchAllByIndex(cfg *structConfig, fieldName string) ([][]byte, error) {
	query := bleve.NewBoolFieldQuery(true)
	query.SetField(hasField(fieldName))
	return m.search(cfg, query)
}

// searchSortedByIndex returns one sorted page only when the persisted coverage
// snapshot proves every primary record has this sortable indexed field.
func (m *bleveIndexManager) searchSortedByIndex(cfg *structConfig, fieldName string, coverage *indexCoverage, reverse bool, limit, skip int) ([][]byte, bool) {
	if m == nil || cfg == nil || coverage == nil || m.isDirty(cfg.Name) {
		return nil, false
	}
	fieldCount, ok := coverage.Fields[fieldName]
	if !ok || fieldCount != coverage.Records {
		return nil, false
	}

	idx, err := m.tableIndex(cfg)
	if err != nil {
		return nil, false
	}
	docCount, err := idx.DocCount()
	if err != nil || docCount != coverage.Records {
		return nil, false
	}

	query := bleve.NewBoolFieldQuery(true)
	query.SetField(hasField(fieldName))
	check := bleve.NewSearchRequestOptions(query, 0, 0, false)
	checkResult, err := idx.Search(check)
	if err != nil || checkResult.Total != fieldCount {
		return nil, false
	}

	req := bleve.NewSearchRequestOptions(query, limit, skip, false)
	sortField := valueField(fieldName)
	if reverse {
		sortField = "-" + sortField
	}
	req.SortBy([]string{sortField})
	result, err := idx.Search(req)
	if err != nil {
		return nil, false
	}

	ids := make([][]byte, 0, len(result.Hits))
	for _, hit := range result.Hits {
		id, err := decodeDocID(hit.ID)
		if err != nil {
			return nil, false
		}
		ids = append(ids, id)
	}
	return ids, true
}

// searchRange returns IDs whose typed field value is within the inclusive range.
func (m *bleveIndexManager) searchRange(cfg *structConfig, fieldName string, min, max any) ([][]byte, error) {
	query, err := rangeQuery(fieldName, min, max)
	if err != nil {
		return nil, err
	}
	return m.search(cfg, query)
}

// searchPrefix returns IDs whose string field starts with prefix.
func (m *bleveIndexManager) searchPrefix(cfg *structConfig, fieldName string, prefix string) ([][]byte, error) {
	query := bleve.NewPrefixQuery(prefix)
	query.SetField(valueField(fieldName))
	return m.search(cfg, query)
}

// searchFullText returns IDs matching a full-text query in Bleve relevance order.
func (m *bleveIndexManager) searchFullText(cfg *structConfig, fieldName string, text string) ([][]byte, error) {
	query := bleve.NewMatchQuery(text)
	query.SetField(textField(fieldName))
	return m.search(cfg, query)
}

// searchComposite returns IDs matching a full equality composite index.
func (m *bleveIndexManager) searchComposite(cfg *structConfig, indexName string, values []any) ([][]byte, error) {
	composite, ok := cfg.CompositeIndexes[indexName]
	if !ok {
		return nil, ErrIdxNotFound
	}
	if len(values) != len(composite.Fields) {
		return nil, ErrIncompatibleValue
	}

	queries := make([]blevequery.Query, 0, len(values)+1)
	marker := bleve.NewBoolFieldQuery(true)
	marker.SetField(compositeField(indexName))
	queries = append(queries, marker)

	for i, field := range composite.Fields {
		token, err := exactToken(values[i], m.codec)
		if err != nil {
			return nil, err
		}
		query := bleve.NewTermQuery(token)
		query.SetField(exactField(field.Name))
		queries = append(queries, query)
	}

	return m.search(cfg, bleve.NewConjunctionQuery(queries...))
}

// search runs a Bleve query and decodes matched document IDs.
func (m *bleveIndexManager) search(cfg *structConfig, query blevequery.Query) ([][]byte, error) {
	if m == nil || m.isDirty(cfg.Name) {
		return nil, index.ErrNotFound
	}

	idx, err := m.tableIndex(cfg)
	if err != nil {
		return nil, err
	}

	req := bleve.NewSearchRequestOptions(query, bleveMaxResults, 0, false)
	res, err := idx.Search(req)
	if err != nil {
		return nil, err
	}

	ids := make([][]byte, 0, len(res.Hits))
	for _, hit := range res.Hits {
		id, err := decodeDocID(hit.ID)
		if err != nil {
			continue
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// rangeQuery creates the correct Bleve range query for supported field values.
func rangeQuery(fieldName string, min, max any) (blevequery.Query, error) {
	field := valueField(fieldName)
	switch mn := min.(type) {
	case time.Time:
		mx, ok := max.(time.Time)
		if !ok {
			return nil, ErrIncompatibleValue
		}
		query := bleve.NewDateRangeInclusiveQuery(mn, mx, boolPtr(true), boolPtr(true))
		query.SetField(field)
		return query, nil
	case string:
		mx, ok := max.(string)
		if !ok {
			return nil, ErrIncompatibleValue
		}
		query := bleve.NewTermRangeInclusiveQuery(mn, mx, boolPtr(true), boolPtr(true))
		query.SetField(field)
		return query, nil
	default:
		minFloat, minOK := numericFloat(min)
		maxFloat, maxOK := numericFloat(max)
		if !minOK || !maxOK {
			return nil, index.ErrNotFound
		}
		query := bleve.NewNumericRangeInclusiveQuery(&minFloat, &maxFloat, boolPtr(true), boolPtr(true))
		query.SetField(field)
		return query, nil
	}
}

// rebuildFromBolt recreates the table index from Bolt records in one batch and
// returns the matching coverage snapshot from that same decoded record stream.
func (m *bleveIndexManager) rebuildFromBolt(cfg *structConfig, bucket *bolt.Bucket, n *node) (*indexCoverage, error) {
	coverage := newIndexCoverage(cfg)
	if m == nil {
		return coverage, nil
	}

	idx, err := m.recreateTable(cfg)
	if err != nil {
		m.markDirty(cfg.Name)
		return nil, err
	}

	batch := idx.NewBatch()
	c := bucket.Cursor()
	for k, raw := c.First(); k != nil; k, raw = c.Next() {
		if raw == nil || bytes.Equal(k, []byte(metadataBucket)) {
			continue
		}

		elem := reflect.New(cfg.Type)
		if err := n.codec.Unmarshal(raw, elem.Interface()); err != nil {
			m.markDirty(cfg.Name)
			return nil, err
		}
		doc, err := m.document(cfg, elem.Interface())
		if err != nil {
			m.markDirty(cfg.Name)
			return nil, err
		}
		if err := batch.Index(encodeDocID(k), doc); err != nil {
			m.markDirty(cfg.Name)
			return nil, err
		}
		coverage.addRecord(cfg, elem.Elem())
	}

	if err := idx.Batch(batch); err != nil {
		m.markDirty(cfg.Name)
		return nil, err
	}
	m.clearDirty(cfg.Name)
	return coverage, nil
}

// collectRecords sorts index hits according to the old Bolt index ordering and applies options.
func collectRecords(n *node, bucket *bolt.Bucket, cfg *structConfig, ids [][]byte, opts *index.Options, less func([]byte, []byte) bool) ([][]byte, error) {
	filtered := make([][]byte, 0, len(ids))
	for _, id := range ids {
		if raw := bucket.Get(id); raw != nil {
			cp := append([]byte(nil), id...)
			filtered = append(filtered, cp)
			continue
		}
		n.s.indexer.markDirty(cfg.Name)
	}

	sort.Slice(filtered, func(i, j int) bool {
		return less(filtered[i], filtered[j])
	})
	if opts != nil && opts.Reverse {
		for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
			filtered[i], filtered[j] = filtered[j], filtered[i]
		}
	}

	start := 0
	if opts != nil && opts.Skip > 0 {
		start = opts.Skip
	}
	if start >= len(filtered) {
		return nil, nil
	}

	end := len(filtered)
	if opts != nil && opts.Limit >= 0 && start+opts.Limit < end {
		end = start + opts.Limit
	}

	records := make([][]byte, 0, end-start)
	for _, id := range filtered[start:end] {
		raw := bucket.Get(id)
		if raw == nil {
			n.s.indexer.markDirty(cfg.Name)
			continue
		}
		records = append(records, raw)
	}
	return records, nil
}

// collectRecordsInOrder keeps Bleve hit order, then applies Reverse, Skip, and Limit.
func collectRecordsInOrder(n *node, bucket *bolt.Bucket, cfg *structConfig, ids [][]byte, opts *index.Options) ([][]byte, error) {
	filtered := make([][]byte, 0, len(ids))
	for _, id := range ids {
		if raw := bucket.Get(id); raw != nil {
			filtered = append(filtered, append([]byte(nil), id...))
			continue
		}
		n.s.indexer.markDirty(cfg.Name)
	}

	if opts != nil && opts.Reverse {
		for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
			filtered[i], filtered[j] = filtered[j], filtered[i]
		}
	}

	start := 0
	if opts != nil && opts.Skip > 0 {
		start = opts.Skip
	}
	if start >= len(filtered) {
		return nil, nil
	}

	end := len(filtered)
	if opts != nil && opts.Limit >= 0 && start+opts.Limit < end {
		end = start + opts.Limit
	}

	records := make([][]byte, 0, end-start)
	for _, id := range filtered[start:end] {
		raw := bucket.Get(id)
		if raw == nil {
			n.s.indexer.markDirty(cfg.Name)
			continue
		}
		records = append(records, raw)
	}
	return records, nil
}

// lessByField returns the old index ordering: encoded field value followed by encoded ID.
func lessByField(n *node, bucket *bolt.Bucket, cfg *structConfig, fieldName string) func([]byte, []byte) bool {
	return func(leftID, rightID []byte) bool {
		leftRaw := bucket.Get(leftID)
		rightRaw := bucket.Get(rightID)
		if leftRaw == nil || rightRaw == nil {
			return bytes.Compare(leftID, rightID) < 0
		}

		leftValue, leftErr := indexedFieldBytes(n, cfg, leftRaw, fieldName)
		rightValue, rightErr := indexedFieldBytes(n, cfg, rightRaw, fieldName)
		if leftErr != nil || rightErr != nil {
			return bytes.Compare(leftID, rightID) < 0
		}
		if cmp := bytes.Compare(leftValue, rightValue); cmp != 0 {
			return cmp < 0
		}
		return bytes.Compare(leftID, rightID) < 0
	}
}

// lessByComposite orders records by each composite field and then ID.
func lessByComposite(n *node, bucket *bolt.Bucket, cfg *structConfig, composite *compositeIndexConfig) func([]byte, []byte) bool {
	return func(leftID, rightID []byte) bool {
		leftRaw := bucket.Get(leftID)
		rightRaw := bucket.Get(rightID)
		if leftRaw == nil || rightRaw == nil {
			return bytes.Compare(leftID, rightID) < 0
		}

		for _, field := range composite.Fields {
			leftValue, leftErr := indexedFieldBytes(n, cfg, leftRaw, field.Name)
			rightValue, rightErr := indexedFieldBytes(n, cfg, rightRaw, field.Name)
			if leftErr != nil || rightErr != nil {
				continue
			}
			if cmp := bytes.Compare(leftValue, rightValue); cmp != 0 {
				return cmp < 0
			}
		}
		return bytes.Compare(leftID, rightID) < 0
	}
}

// indexedFieldBytes decodes one record and returns the old encoded field value.
func indexedFieldBytes(n *node, cfg *structConfig, raw []byte, fieldName string) ([]byte, error) {
	elem := reflect.New(cfg.Type)
	if err := n.codec.Unmarshal(raw, elem.Interface()); err != nil {
		return nil, err
	}

	field := elem.Elem().FieldByName(fieldName)
	if !field.IsValid() {
		return nil, ErrNotFound
	}
	return toBytes(field.Interface(), n.codec)
}

func boolPtr(v bool) *bool {
	return &v
}

func exactField(name string) string {
	return bleveExactPrefix + encodedName(name)
}

func valueField(name string) string {
	return bleveValuePrefix + encodedName(name)
}

func textField(name string) string {
	return bleveTextPrefix + encodedName(name)
}

func hasField(name string) string {
	return bleveHasPrefix + encodedName(name)
}

func compositeField(name string) string {
	return bleveCompositePrefix + encodedName(name)
}

func encodedName(name string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(name))
}

func exactToken(value any, c codec.MarshalUnmarshaler) (string, error) {
	if rv := derefValue(reflect.ValueOf(value)); rv.IsValid() {
		value = rv.Interface()
	}
	raw, err := toBytes(value, c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func encodeDocID(id []byte) string {
	return base64.RawURLEncoding.EncodeToString(id)
}

func decodeDocID(id string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(id)
}

func derefValue(v reflect.Value) reflect.Value {
	for v.IsValid() && v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	return v
}

func bleveValue(v reflect.Value) (any, bool) {
	v = derefValue(v)
	if !v.IsValid() {
		return nil, false
	}
	if t, ok := v.Interface().(time.Time); ok {
		return t, true
	}

	switch v.Kind() {
	case reflect.String:
		return v.String(), true
	case reflect.Bool:
		return v.Bool(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(v.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(v.Uint()), true
	case reflect.Float32, reflect.Float64:
		return v.Float(), true
	default:
		return nil, false
	}
}

func fullTextValue(v reflect.Value) (string, bool) {
	v = derefValue(v)
	if !v.IsValid() {
		return "", false
	}

	switch v.Kind() {
	case reflect.String:
		return v.String(), true
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return string(v.Bytes()), true
		}
	}
	return "", false
}

func numericFloat(v any) (float64, bool) {
	rv := derefValue(reflect.ValueOf(v))
	if !rv.IsValid() {
		return 0, false
	}

	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		return rv.Float(), true
	default:
		return 0, false
	}
}

func compositeReady(ref reflect.Value, composite *compositeIndexConfig) bool {
	for _, field := range composite.Fields {
		value := ref.FieldByName(field.Name)
		if !value.IsValid() || isZero(&value) {
			return false
		}
	}
	return true
}

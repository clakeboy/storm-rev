package storm

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"

	bolt "go.etcd.io/bbolt"
)

const (
	metaCodec         = "codec"
	metaSchema        = "schema"
	metaIndexCoverage = "index_coverage"
)

func newMeta(b *bolt.Bucket, n Node) (*meta, error) {
	m := b.Bucket([]byte(metadataBucket))
	if m != nil {
		name := m.Get([]byte(metaCodec))
		if name == nil {
			m.Put([]byte(metaCodec), []byte(n.Codec().Name()))
		} else if string(name) != n.Codec().Name() {
			return nil, ErrDifferentCodec
		}
		return &meta{
			node:   n,
			bucket: m,
		}, nil
	}

	m, err := b.CreateBucket([]byte(metadataBucket))
	if err != nil {
		return nil, err
	}

	m.Put([]byte(metaCodec), []byte(n.Codec().Name()))
	return &meta{
		node:   n,
		bucket: m,
	}, nil
}

type meta struct {
	node   Node
	bucket *bolt.Bucket
}

type storedSchema struct {
	Table  string              `json:"table"`
	Fields []storedSchemaField `json:"fields"`
	ID     string              `json:"id"`
}

type storedSchemaField struct {
	Name           string         `json:"name"`
	JSON           string         `json:"json"`
	Type           string         `json:"type"`
	Index          string         `json:"index,omitempty"`
	ID             bool           `json:"id,omitempty"`
	Increment      bool           `json:"increment,omitempty"`
	IncrementStart int64          `json:"increment_start,omitempty"`
	Integer        bool           `json:"integer,omitempty"`
	Composites     map[string]int `json:"composites,omitempty"`
}

// indexCoverage records the table rows and non-zero indexed values represented
// by the external index. Its absence means sorted index reads must fall back.
type indexCoverage struct {
	Records uint64            `json:"records"`
	Fields  map[string]uint64 `json:"fields"`
}

// setStoredSchema persists a complete table schema as metadata.
func (m *meta) setStoredSchema(schema storedSchema) error {
	raw, err := json.Marshal(schema)
	if err != nil {
		return err
	}
	if bytes.Equal(m.bucket.Get([]byte(metaSchema)), raw) {
		return nil
	}
	if err := m.bucket.Put([]byte(metaSchema), raw); err != nil {
		return err
	}
	return m.invalidateIndexCoverage()
}

// setSchema persists enough struct metadata for SQL map/DTO queries without registration.
func (m *meta) setSchema(cfg *structConfig) error {
	return m.setStoredSchema(storedSchemaFromConfig(cfg))
}

// ensureSchema persists table schema once so bucket-list APIs can identify tables.
func (m *meta) ensureSchema(cfg *structConfig) error {
	if m.bucket.Get([]byte(metaSchema)) != nil {
		return nil
	}
	return m.setSchema(cfg)
}

// hasSchema reports whether this metadata bucket belongs to a Storm table.
func (m *meta) hasSchema() bool {
	return m != nil && m.bucket != nil && m.bucket.Get([]byte(metaSchema)) != nil
}

// indexCoverage returns a valid coverage snapshot when one has been rebuilt or
// incrementally maintained by Storm writes.
func (m *meta) indexCoverage() (*indexCoverage, bool) {
	if m == nil || m.bucket == nil {
		return nil, false
	}
	raw := m.bucket.Get([]byte(metaIndexCoverage))
	if raw == nil {
		return nil, false
	}
	coverage := &indexCoverage{}
	if err := json.Unmarshal(raw, coverage); err != nil || coverage.Fields == nil {
		return nil, false
	}
	return coverage, true
}

// setIndexCoverage persists one complete coverage snapshot inside the table's
// Bolt transaction so it changes atomically with the primary records.
func (m *meta) setIndexCoverage(coverage *indexCoverage) error {
	if coverage == nil {
		return m.invalidateIndexCoverage()
	}
	if coverage.Fields == nil {
		coverage.Fields = make(map[string]uint64)
	}
	raw, err := json.Marshal(coverage)
	if err != nil {
		return err
	}
	return m.bucket.Put([]byte(metaIndexCoverage), raw)
}

// invalidateIndexCoverage disables sorted index reads after an unmanaged write
// or schema change until ReIndex can establish a new complete snapshot.
func (m *meta) invalidateIndexCoverage() error {
	if m == nil || m.bucket == nil {
		return nil
	}
	return m.bucket.Delete([]byte(metaIndexCoverage))
}

// initializeIndexCoverage marks an empty table as fully covered so subsequent
// normal Save calls can maintain exact counts without an initial ReIndex.
func (m *meta) initializeIndexCoverage(cfg *structConfig) error {
	return m.setIndexCoverage(newIndexCoverage(cfg))
}

// newIndexCoverage allocates counters for standalone fields that can supply a
// raw SQL sort order; composite-only fields are intentionally excluded.
func newIndexCoverage(cfg *structConfig) *indexCoverage {
	coverage := &indexCoverage{Fields: make(map[string]uint64)}
	if cfg == nil {
		return coverage
	}
	for name, field := range cfg.Fields {
		if field != nil && field.Index != "" {
			coverage.Fields[name] = 0
		}
	}
	return coverage
}

// addRecord accumulates one decoded primary record while an index is rebuilt.
func (coverage *indexCoverage) addRecord(cfg *structConfig, record reflect.Value) {
	if coverage == nil || cfg == nil {
		return
	}
	coverage.Records++
	for name := range coverage.Fields {
		value := record.FieldByName(name)
		if value.IsValid() && !isZero(&value) {
			coverage.Fields[name]++
		}
	}
}

// updateIndexCoverageAfterSave applies a primary-record replacement to a
// maintained snapshot. Invalid or unreadable old data disables the optimization.
func (m *meta) updateIndexCoverageAfterSave(cfg *structConfig, oldRaw []byte) error {
	coverage, ok := m.indexCoverage()
	if !ok {
		return nil
	}
	oldFields, err := indexedFieldPresence(cfg, oldRaw, m.node.Codec())
	if err != nil {
		return m.invalidateIndexCoverage()
	}
	newFields := indexedFieldPresenceFromConfig(cfg)
	if oldRaw == nil {
		coverage.Records++
	}
	for name := range coverage.Fields {
		oldPresent := oldFields[name]
		newPresent := newFields[name]
		switch {
		case oldPresent && !newPresent:
			if coverage.Fields[name] == 0 {
				return m.invalidateIndexCoverage()
			}
			coverage.Fields[name]--
		case !oldPresent && newPresent:
			coverage.Fields[name]++
		}
	}
	return m.setIndexCoverage(coverage)
}

// updateIndexCoverageAfterDelete removes one record from a maintained snapshot.
func (m *meta) updateIndexCoverageAfterDelete(cfg *structConfig, raw []byte) error {
	coverage, ok := m.indexCoverage()
	if !ok {
		return nil
	}
	fields, err := indexedFieldPresence(cfg, raw, m.node.Codec())
	if err != nil || coverage.Records == 0 {
		return m.invalidateIndexCoverage()
	}
	coverage.Records--
	for name, present := range fields {
		if !present {
			continue
		}
		if coverage.Fields[name] == 0 {
			return m.invalidateIndexCoverage()
		}
		coverage.Fields[name]--
	}
	return m.setIndexCoverage(coverage)
}

// indexedFieldPresenceFromConfig reports the current values that Bleve would
// retain for standalone indexed fields without decoding the newly saved record.
func indexedFieldPresenceFromConfig(cfg *structConfig) map[string]bool {
	present := make(map[string]bool)
	if cfg == nil {
		return present
	}
	for name, field := range cfg.Fields {
		if field == nil || field.Index == "" || field.Value == nil || !field.Value.IsValid() {
			continue
		}
		present[name] = !isZero(field.Value)
	}
	return present
}

// indexedFieldPresence decodes an existing record so replacement and delete
// operations can update coverage with the same zero-value rule as Bleve.
func indexedFieldPresence(cfg *structConfig, raw []byte, c interface{ Unmarshal([]byte, any) error }) (map[string]bool, error) {
	present := make(map[string]bool)
	if cfg == nil || raw == nil {
		return present, nil
	}
	if cfg.Type == nil {
		return nil, ErrBadType
	}
	record := reflect.New(cfg.Type)
	if err := c.Unmarshal(raw, record.Interface()); err != nil {
		return nil, err
	}
	for name, field := range cfg.Fields {
		if field == nil || field.Index == "" {
			continue
		}
		value := record.Elem().FieldByName(name)
		if value.IsValid() {
			present[name] = !isZero(&value)
		}
	}
	return present, nil
}

// bucketHasRawRecords stops at the first primary value so new tables can begin
// coverage tracking without treating legacy records as already indexed.
func bucketHasRawRecords(bucket *bolt.Bucket) bool {
	if bucket == nil {
		return false
	}
	cursor := bucket.Cursor()
	for key, raw := cursor.First(); key != nil; key, raw = cursor.Next() {
		if raw != nil && !bytes.Equal(key, []byte(metadataBucket)) {
			return true
		}
	}
	return false
}

func readStoredSchema(bucket *bolt.Bucket) (*storedSchema, error) {
	if bucket == nil {
		return nil, ErrNotFound
	}
	metaBucket := bucket.Bucket([]byte(metadataBucket))
	if metaBucket == nil {
		return nil, ErrNotFound
	}
	raw := metaBucket.Get([]byte(metaSchema))
	if raw == nil {
		return nil, ErrNotFound
	}
	var schema storedSchema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, err
	}
	return &schema, nil
}

// structConfigFromStoredSchema rebuilds the runtime table configuration saved in metadata.
// It is used when callers want to rebuild indexes without passing a concrete struct value.
func structConfigFromStoredSchema(schema *storedSchema) *structConfig {
	dynamicType := dynamicStructType(schema)
	cfg := &structConfig{
		Name:   schema.Table,
		Type:   dynamicType,
		Fields: make(map[string]*fieldConfig, len(schema.Fields)),
	}
	for _, stored := range schema.Fields {
		field := &fieldConfig{
			Name:           stored.Name,
			Index:          stored.Index,
			IsID:           stored.ID,
			Increment:      stored.Increment,
			IncrementStart: stored.IncrementStart,
			IsInteger:      stored.Integer,
			JsonFieldName:  stored.JSON,
			Composites:     stored.Composites,
		}
		if field.JsonFieldName == "" {
			field.JsonFieldName = field.Name
		}
		cfg.Fields[field.Name] = field
		if field.IsID || schema.ID == field.Name {
			cfg.ID = field
		}
	}
	_ = validateCompositeIndexes(cfg)
	return cfg
}

func storedSchemaFromConfig(cfg *structConfig) storedSchema {
	schema := storedSchema{
		Table: cfg.Name,
	}
	if cfg.ID != nil {
		schema.ID = cfg.ID.Name
	}
	names := make([]string, 0, len(cfg.Fields))
	for name := range cfg.Fields {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		field := cfg.Fields[name]
		stored := storedSchemaField{
			Name:           field.Name,
			JSON:           field.JsonFieldName,
			Index:          field.Index,
			ID:             field.IsID,
			Increment:      field.Increment,
			IncrementStart: field.IncrementStart,
			Integer:        field.IsInteger,
			Composites:     field.Composites,
		}
		if field.Value != nil && field.Value.IsValid() {
			stored.Type = field.Value.Type().String()
		}
		schema.Fields = append(schema.Fields, stored)
	}
	return schema
}

func (m *meta) increment(field *fieldConfig) error {
	var err error
	counter := field.IncrementStart

	raw := m.bucket.Get([]byte(field.Name + "counter"))
	if raw != nil {
		counter, err = numberfromb(raw)
		if err != nil {
			return err
		}
		counter++
	}

	raw, err = numbertob(counter)
	if err != nil {
		return err
	}

	err = m.bucket.Put([]byte(field.Name+"counter"), raw)
	if err != nil {
		return err
	}

	field.Value.Set(reflect.ValueOf(counter).Convert(field.Value.Type()))
	field.IsZero = false
	return nil
}

func (m *meta) setIncrement(field *fieldConfig, counter []byte) error {
	// raw, err := numbertob(counter)
	// if err != nil {
	// 	return err
	// }
	err := m.bucket.Put([]byte(field.Name+"counter"), counter)
	if err != nil {
		return err
	}
	return nil
}

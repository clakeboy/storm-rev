package storm

import (
	"bytes"
	"reflect"

	bolt "go.etcd.io/bbolt"
)

// TypeStore stores user defined types in BoltDB.
type TypeStore interface {
	Finder
	// Init creates the indexes and buckets for a given structure
	Init(data any) error

	// ReIndex rebuilds all the indexes of a bucket
	ReIndex(data any) error

	// Save a structure
	Save(data any) error

	// SaveAll saves a slice of structures in one write transaction.
	SaveAll(data any) error

	// Update a structure
	Update(data any) error

	// UpdateField updates a single field
	UpdateField(data any, fieldName string, value any) error

	// Drop a bucket
	Drop(data any) error

	// DeleteStruct deletes a structure from the associated bucket
	DeleteStruct(data any) error
}

// Init creates the indexes and buckets for a given structure
func (n *node) Init(data any) error {
	v := reflect.ValueOf(data)
	cfg, err := extract(&v)
	if err != nil {
		return err
	}

	return n.readWriteTx(func(tx *bolt.Tx) error {
		return n.init(tx, cfg)
	})
}

func (n *node) init(tx *bolt.Tx, cfg *structConfig) error {
	bucket, err := n.CreateBucketIfNotExists(tx, cfg.Name)
	if err != nil {
		return err
	}

	// save node configuration in the bucket
	meta, err := newMeta(bucket, n)
	if err != nil {
		return err
	}
	if err := meta.setSchema(cfg); err != nil {
		return err
	}

	return n.s.indexer.initTable(cfg)
}

func (n *node) ReIndex(data any) error {
	ref := reflect.ValueOf(data)

	if !ref.IsValid() || ref.Kind() != reflect.Ptr || ref.Elem().Kind() != reflect.Struct {
		return ErrStructPtrNeeded
	}

	cfg, err := extract(&ref)
	if err != nil {
		return err
	}

	return n.readWriteTx(func(tx *bolt.Tx) error {
		return n.reIndex(tx, data, cfg)
	})
}

func (n *node) reIndex(tx *bolt.Tx, data any, cfg *structConfig) error {
	bucket := n.GetBucket(tx, cfg.Name)
	if bucket == nil {
		return ErrNotFound
	}

	if err := n.s.indexer.rebuildFromBolt(cfg, bucket, n); err != nil {
		return err
	}

	if cfg.ID != nil && cfg.ID.Increment {
		var lastID []byte
		c := bucket.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if v == nil || bytes.Equal(k, []byte(metadataBucket)) {
				continue
			}
			lastID = append(lastID[:0], k...)
		}
		if lastID != nil {
			meta, err := newMeta(bucket, n)
			if err != nil {
				return err
			}
			return meta.setIncrement(cfg.ID, lastID)
		}
	}
	return nil
}

type savedRecord struct {
	cfg  *structConfig
	data any
	id   []byte
}

type deletedRecord struct {
	cfg *structConfig
	id  []byte
}

type saveInput struct {
	cfg  *structConfig
	data any
}

type savedRecordGroup struct {
	cfg     *structConfig
	records []*savedRecord
}

type deletedRecordGroup struct {
	cfg     *structConfig
	records []*deletedRecord
}

// Save a structure
func (n *node) Save(data any) error {
	cfg, err := saveConfig(data)
	if err != nil {
		return err
	}

	if err := validateSaveConfig(cfg); err != nil {
		return err
	}

	return n.readWriteTx(func(tx *bolt.Tx) error {
		record, err := n.save(tx, cfg, data, false, nil)
		if err != nil {
			return err
		}
		if n.tx != nil {
			n.markTxIndexRecord(record)
			return nil
		}
		return n.indexSavedRecords([]*savedRecord{record})
	})
}

// SaveAll saves all structures in one Bolt write transaction and indexes them in batches.
func (n *node) SaveAll(data any) error {
	inputs, err := saveAllInputs(data)
	if err != nil || len(inputs) == 0 {
		return err
	}

	var records []*savedRecord
	err = n.readWriteTx(func(tx *bolt.Tx) error {
		uniqueState := newSaveAllUniqueState(n)
		records = records[:0]

		for _, input := range inputs {
			record, err := n.save(tx, input.cfg, input.data, false, uniqueState)
			if err != nil {
				return err
			}
			records = append(records, record)
		}

		if n.tx != nil {
			for _, record := range records {
				n.markTxIndexRecord(record)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if n.tx != nil {
		return nil
	}
	return n.indexSavedRecords(records)
}

// saveConfig validates the public Save input and extracts per-record field metadata.
func saveConfig(data any) (*structConfig, error) {
	ref := reflect.ValueOf(data)

	if !ref.IsValid() || ref.Kind() != reflect.Ptr || ref.Elem().Kind() != reflect.Struct {
		return nil, ErrStructPtrNeeded
	}

	return extract(&ref)
}

// validateSaveConfig keeps Save and SaveAll aligned on zero-ID validation.
func validateSaveConfig(cfg *structConfig) error {
	if cfg.ID.IsZero && (!cfg.ID.IsInteger || !cfg.ID.Increment) {
		return ErrZeroID
	}
	return nil
}

// saveAllInputs normalizes []T and []*T into pointer values so increments mutate the caller's slice.
func saveAllInputs(data any) ([]saveInput, error) {
	ref := reflect.ValueOf(data)
	if !ref.IsValid() || ref.Kind() != reflect.Slice || ref.IsNil() {
		return nil, ErrSlicePtrNeeded
	}
	if ref.Len() == 0 {
		return nil, nil
	}

	elemType := ref.Type().Elem()
	ptrElem := elemType.Kind() == reflect.Ptr
	if ptrElem {
		elemType = elemType.Elem()
	}
	if elemType.Kind() != reflect.Struct {
		return nil, ErrStructPtrNeeded
	}

	inputs := make([]saveInput, 0, ref.Len())
	for i := 0; i < ref.Len(); i++ {
		elem := ref.Index(i)
		if ptrElem {
			if elem.IsNil() {
				return nil, ErrStructPtrNeeded
			}
		} else {
			if !elem.CanAddr() {
				return nil, ErrStructPtrNeeded
			}
			elem = elem.Addr()
		}

		cfg, err := extract(&elem)
		if err != nil {
			return nil, err
		}
		if err := validateSaveConfig(cfg); err != nil {
			return nil, err
		}
		inputs = append(inputs, saveInput{cfg: cfg, data: elem.Interface()})
	}
	return inputs, nil
}

// save writes one record to Bolt and returns the data needed to update external indexes later.
func (n *node) save(tx *bolt.Tx, cfg *structConfig, data any, update bool, uniqueState *saveAllUniqueState) (*savedRecord, error) {
	bucket, err := n.CreateBucketIfNotExists(tx, cfg.Name)
	if err != nil {
		return nil, err
	}

	// save node configuration in the bucket
	meta, err := newMeta(bucket, n)
	if err != nil {
		return nil, err
	}
	if err := meta.ensureSchema(cfg); err != nil {
		return nil, err
	}

	if cfg.ID.IsZero {
		err = meta.increment(cfg.ID)
		if err != nil {
			return nil, err
		}
	}

	id, err := toBytes(cfg.ID.Value.Interface(), n.codec)
	if err != nil {
		return nil, err
	}

	for fieldName, fieldCfg := range cfg.Fields {
		if !update && !fieldCfg.IsID && fieldCfg.Increment && fieldCfg.IsInteger && fieldCfg.IsZero {
			err = meta.increment(fieldCfg)
			if err != nil {
				return nil, err
			}
		}

		if fieldCfg.Index == "" {
			continue
		}

		if update && fieldCfg.IsZero && !fieldCfg.ForceUpdate {
			continue
		}

		if fieldCfg.IsZero {
			continue
		}

		if fieldCfg.Index == tagUniqueIdx {
			exists, err := n.saveUniqueExists(tx, bucket, cfg, fieldName, fieldCfg.Value.Interface(), id, uniqueState)
			if err != nil {
				return nil, err
			}
			if exists {
				return nil, ErrAlreadyExists
			}
		}
	}

	raw, err := n.codec.Marshal(data)
	if err != nil {
		return nil, err
	}

	if err := bucket.Put(id, raw); err != nil {
		return nil, err
	}

	record := &savedRecord{
		cfg:  cfg,
		data: data,
		id:   append([]byte(nil), id...),
	}
	if uniqueState != nil {
		if err := uniqueState.recordSave(cfg, record.id); err != nil {
			return nil, err
		}
	}
	return record, nil
}

// saveUniqueExists switches between normal unique checks and SaveAll's batch-local view.
func (n *node) saveUniqueExists(tx *bolt.Tx, bucket *bolt.Bucket, cfg *structConfig, fieldName string, value any, id []byte, uniqueState *saveAllUniqueState) (bool, error) {
	if uniqueState != nil {
		return uniqueState.uniqueExists(bucket, cfg, fieldName, value, id)
	}
	return n.uniqueExists(tx, bucket, cfg, fieldName, value, id)
}

// indexSavedRecords groups saved records by table and writes each table with one Bleve batch.
func (n *node) indexSavedRecords(records []*savedRecord) error {
	if len(records) == 0 {
		return nil
	}

	groups := make(map[string]*savedRecordGroup)
	for _, record := range records {
		if record == nil {
			continue
		}
		group := groups[record.cfg.Name]
		if group == nil {
			group = &savedRecordGroup{cfg: record.cfg}
			groups[record.cfg.Name] = group
		}
		group.records = append(group.records, record)
	}

	for _, group := range groups {
		if err := n.s.indexer.indexRecords(group.cfg, group.records); err != nil {
			n.s.indexer.markDirty(group.cfg.Name)
			return err
		}
	}
	return nil
}

// deleteIndexedRecords groups transactional deletes by table and removes them with Bleve batches.
func (n *node) deleteIndexedRecords(records []*deletedRecord) error {
	if len(records) == 0 {
		return nil
	}

	groups := make(map[string]*deletedRecordGroup)
	for _, record := range records {
		if record == nil {
			continue
		}
		group := groups[record.cfg.Name]
		if group == nil {
			group = &deletedRecordGroup{cfg: record.cfg}
			groups[record.cfg.Name] = group
		}
		group.records = append(group.records, record)
	}

	for _, group := range groups {
		if err := n.s.indexer.deleteRecords(group.cfg, group.records); err != nil {
			n.s.indexer.markDirty(group.cfg.Name)
			return err
		}
	}
	return nil
}

// Update a structure
func (n *node) Update(data any) error {
	return n.update(data, func(ref *reflect.Value, current *reflect.Value, cfg *structConfig) error {
		numfield := ref.NumField()
		for i := range numfield {
			f := ref.Field(i)
			if ref.Type().Field(i).PkgPath != "" {
				continue
			}
			zero := reflect.Zero(f.Type()).Interface()
			actual := f.Interface()
			if !reflect.DeepEqual(actual, zero) {
				cf := current.Field(i)
				cf.Set(f)
				idxInfo, ok := cfg.Fields[ref.Type().Field(i).Name]
				if ok {
					idxInfo.Value = &cf
				}
			}
		}
		return nil
	})
}

// UpdateField updates a single field
func (n *node) UpdateField(data any, fieldName string, value any) error {
	return n.update(data, func(ref *reflect.Value, current *reflect.Value, cfg *structConfig) error {
		f := current.FieldByName(fieldName)
		if !f.IsValid() {
			return ErrNotFound
		}
		tf, _ := current.Type().FieldByName(fieldName)
		if tf.PkgPath != "" {
			return ErrNotFound
		}
		v := reflect.ValueOf(value)
		if v.Kind() != f.Kind() {
			return ErrIncompatibleValue
		}
		f.Set(v)
		idxInfo, ok := cfg.Fields[fieldName]
		if ok {
			idxInfo.Value = &f
			idxInfo.IsZero = isZero(idxInfo.Value)
			idxInfo.ForceUpdate = true
		}
		return nil
	})
}

func (n *node) update(data any, fn func(*reflect.Value, *reflect.Value, *structConfig) error) error {
	ref := reflect.ValueOf(data)
	if !ref.IsValid() || ref.Kind() != reflect.Ptr || ref.Elem().Kind() != reflect.Struct {
		return ErrStructPtrNeeded
	}

	cfg, err := extract(&ref)
	if err != nil {
		return err
	}

	if cfg.ID.IsZero {
		return ErrNoID
	}

	current := reflect.New(reflect.Indirect(ref).Type())

	return n.readWriteTx(func(tx *bolt.Tx) error {
		err = n.WithTransaction(tx).One(cfg.ID.Name, cfg.ID.Value.Interface(), current.Interface())
		if err != nil {
			return err
		}

		ref := reflect.ValueOf(data).Elem()
		cref := current.Elem()
		err = fn(&ref, &cref, cfg)
		if err != nil {
			return err
		}

		record, err := n.save(tx, cfg, current.Interface(), true, nil)
		if err != nil {
			return err
		}
		if n.tx != nil {
			n.markTxIndexRecord(record)
			return nil
		}
		return n.indexSavedRecords([]*savedRecord{record})
	})
}

// Drop a bucket
func (n *node) Drop(data any) error {
	var bucketName string

	v := reflect.ValueOf(data)
	if v.Kind() != reflect.String {
		info, err := extract(&v)
		if err != nil {
			return err
		}

		bucketName = info.Name
	} else {
		bucketName = v.Interface().(string)
	}

	return n.readWriteTx(func(tx *bolt.Tx) error {
		return n.drop(tx, bucketName)
	})
}

func (n *node) drop(tx *bolt.Tx, bucketName string) error {
	bucket := n.GetBucket(tx)
	var err error
	if bucket == nil {
		err = tx.DeleteBucket([]byte(bucketName))
	} else {
		err = bucket.DeleteBucket([]byte(bucketName))
	}

	if err != nil {
		return err
	}
	if n.tx != nil {
		if n.txIndexDrops != nil {
			n.txIndexDrops[bucketName] = true
		}
		return nil
	}
	return n.s.indexer.dropTable(bucketName)
}

// DeleteStruct deletes a structure from the associated bucket
func (n *node) DeleteStruct(data any) error {
	ref := reflect.ValueOf(data)

	if !ref.IsValid() || ref.Kind() != reflect.Ptr || ref.Elem().Kind() != reflect.Struct {
		return ErrStructPtrNeeded
	}

	cfg, err := extract(&ref)
	if err != nil {
		return err
	}

	id, err := toBytes(cfg.ID.Value.Interface(), n.codec)
	if err != nil {
		return err
	}

	return n.readWriteTx(func(tx *bolt.Tx) error {
		return n.deleteStruct(tx, cfg, id)
	})
}

func (n *node) deleteStruct(tx *bolt.Tx, cfg *structConfig, id []byte) error {
	bucket := n.GetBucket(tx, cfg.Name)
	if bucket == nil {
		return ErrNotFound
	}

	raw := bucket.Get(id)
	if raw == nil {
		return ErrNotFound
	}

	if err := bucket.Delete(id); err != nil {
		return err
	}

	if n.tx != nil {
		n.markTxIndexDelete(&deletedRecord{
			cfg: cfg,
			id:  append([]byte(nil), id...),
		})
		return nil
	}
	if err := n.s.indexer.deleteRecord(cfg, id); err != nil {
		n.s.indexer.markDirty(cfg.Name)
		return err
	}
	return nil
}

func (n *node) markTxIndexDirty(cfg *structConfig) {
	if n.txIndexDirty != nil {
		n.txIndexDirty[cfg.Name] = cfg
	}
}

// markTxIndexRecord remembers a transactional save for post-commit batch indexing.
func (n *node) markTxIndexRecord(record *savedRecord) {
	if record == nil || n.txIndexRecords == nil {
		return
	}
	if n.txIndexDeletes != nil {
		n.txIndexDeletes[record.cfg.Name] = removeDeletedRecord(n.txIndexDeletes[record.cfg.Name], record.id)
	}
	n.txIndexRecords[record.cfg.Name] = removeSavedRecord(n.txIndexRecords[record.cfg.Name], record.id)
	n.txIndexRecords[record.cfg.Name] = append(n.txIndexRecords[record.cfg.Name], record)
}

// markTxIndexDelete remembers a transactional delete and cancels any prior pending save for that id.
func (n *node) markTxIndexDelete(record *deletedRecord) {
	if record == nil || n.txIndexDeletes == nil {
		return
	}
	if n.txIndexRecords != nil {
		n.txIndexRecords[record.cfg.Name] = removeSavedRecord(n.txIndexRecords[record.cfg.Name], record.id)
	}
	n.txIndexDeletes[record.cfg.Name] = removeDeletedRecord(n.txIndexDeletes[record.cfg.Name], record.id)
	n.txIndexDeletes[record.cfg.Name] = append(n.txIndexDeletes[record.cfg.Name], record)
}

func removeSavedRecord(records []*savedRecord, id []byte) []*savedRecord {
	kept := records[:0]
	for _, record := range records {
		if record != nil && bytes.Equal(record.id, id) {
			continue
		}
		kept = append(kept, record)
	}
	return kept
}

func removeDeletedRecord(records []*deletedRecord, id []byte) []*deletedRecord {
	kept := records[:0]
	for _, record := range records {
		if record != nil && bytes.Equal(record.id, id) {
			continue
		}
		kept = append(kept, record)
	}
	return kept
}

func (n *node) uniqueExists(tx *bolt.Tx, bucket *bolt.Bucket, cfg *structConfig, fieldName string, value any, id []byte) (bool, error) {
	if n.tx == nil && !n.s.indexer.isDirty(cfg.Name) {
		exists, err := n.s.indexer.uniqueExists(cfg, fieldName, value, id)
		if err == nil {
			return exists, nil
		}
	}
	return n.uniqueExistsByScan(bucket, cfg, fieldName, value, id)
}

func (n *node) uniqueExistsByScan(bucket *bolt.Bucket, cfg *structConfig, fieldName string, value any, id []byte) (bool, error) {
	expected, err := toBytes(value, n.codec)
	if err != nil {
		return false, err
	}

	c := bucket.Cursor()
	for k, raw := c.First(); k != nil; k, raw = c.Next() {
		if raw == nil || bytes.Equal(k, []byte(metadataBucket)) || bytes.Equal(k, id) {
			continue
		}

		elem := reflect.New(cfg.Type)
		if err := n.codec.Unmarshal(raw, elem.Interface()); err != nil {
			return false, err
		}
		field := elem.Elem().FieldByName(fieldName)
		if !field.IsValid() || isZero(&field) {
			continue
		}
		actual, err := toBytes(field.Interface(), n.codec)
		if err != nil {
			return false, err
		}
		if bytes.Equal(actual, expected) {
			return true, nil
		}
	}
	return false, nil
}

type saveAllUniqueState struct {
	node   *node
	values map[string]map[string]map[string][][]byte
}

func newSaveAllUniqueState(n *node) *saveAllUniqueState {
	return &saveAllUniqueState{
		node:   n,
		values: make(map[string]map[string]map[string][][]byte),
	}
}

// uniqueExists checks a batch-local unique map so earlier saves in the same batch are visible.
func (s *saveAllUniqueState) uniqueExists(bucket *bolt.Bucket, cfg *structConfig, fieldName string, value any, id []byte) (bool, error) {
	values, err := s.fieldValues(bucket, cfg, fieldName)
	if err != nil {
		return false, err
	}

	token, err := uniqueToken(value, s.node)
	if err != nil {
		return false, err
	}
	for _, existingID := range values[token] {
		if !bytes.Equal(existingID, id) {
			return true, nil
		}
	}
	return false, nil
}

// fieldValues lazily scans one unique field and reuses the result for the rest of the batch.
func (s *saveAllUniqueState) fieldValues(bucket *bolt.Bucket, cfg *structConfig, fieldName string) (map[string][][]byte, error) {
	tableValues := s.values[cfg.Name]
	if tableValues == nil {
		tableValues = make(map[string]map[string][][]byte)
		s.values[cfg.Name] = tableValues
	}
	if values, ok := tableValues[fieldName]; ok {
		return values, nil
	}

	values := make(map[string][][]byte)
	c := bucket.Cursor()
	for k, raw := c.First(); k != nil; k, raw = c.Next() {
		if raw == nil || bytes.Equal(k, []byte(metadataBucket)) {
			continue
		}

		elem := reflect.New(cfg.Type)
		if err := s.node.codec.Unmarshal(raw, elem.Interface()); err != nil {
			return nil, err
		}
		field := elem.Elem().FieldByName(fieldName)
		if !field.IsValid() || isZero(&field) {
			continue
		}
		token, err := uniqueToken(field.Interface(), s.node)
		if err != nil {
			return nil, err
		}
		values[token] = append(values[token], append([]byte(nil), k...))
	}
	tableValues[fieldName] = values
	return values, nil
}

// recordSave updates every loaded unique map after Bolt has accepted one record.
func (s *saveAllUniqueState) recordSave(cfg *structConfig, id []byte) error {
	tableValues := s.values[cfg.Name]
	if tableValues == nil {
		return nil
	}

	for fieldName, fieldCfg := range cfg.Fields {
		if fieldCfg.Index != tagUniqueIdx {
			continue
		}
		values, ok := tableValues[fieldName]
		if !ok {
			continue
		}

		removeUniqueID(values, id)
		if fieldCfg.Value == nil || isZero(fieldCfg.Value) {
			continue
		}
		token, err := uniqueToken(fieldCfg.Value.Interface(), s.node)
		if err != nil {
			return err
		}
		values[token] = append(values[token], append([]byte(nil), id...))
	}
	return nil
}

// uniqueToken uses the same encoding as the scan-based unique check.
func uniqueToken(value any, n *node) (string, error) {
	raw, err := toBytes(value, n.codec)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// removeUniqueID frees any previous unique values owned by an overwritten record.
func removeUniqueID(values map[string][][]byte, id []byte) {
	for token, ids := range values {
		kept := ids[:0]
		for _, existingID := range ids {
			if !bytes.Equal(existingID, id) {
				kept = append(kept, existingID)
			}
		}
		if len(kept) == 0 {
			delete(values, token)
			continue
		}
		values[token] = kept
	}
}

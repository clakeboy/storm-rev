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
	if err := n.s.indexer.initTable(cfg); err != nil {
		return err
	}
	if _, ok := meta.indexCoverage(); !ok && !bucketHasRawRecords(bucket) {
		return meta.initializeIndexCoverage(cfg)
	}
	return nil
}

func (n *node) ReIndex(data any) error {
	var concrete *structConfig
	if data != nil {
		ref := reflect.ValueOf(data)
		if !ref.IsValid() || ref.Kind() != reflect.Ptr || ref.Elem().Kind() != reflect.Struct {
			return ErrStructPtrNeeded
		}
		cfg, err := extract(&ref)
		if err != nil {
			return err
		}
		concrete = cfg
	}

	if n.tx != nil {
		targets, err := n.collectReindexTargets(n.tx, concrete)
		if err != nil {
			return err
		}
		for _, target := range targets {
			if concrete != nil {
				bucket := n.GetBucket(n.tx, target.children...)
				meta, err := newMeta(bucket, n)
				if err != nil {
					return err
				}
				if err := meta.setSchema(target.cfg); err != nil {
					return err
				}
			}
			n.markTxIndexDirty(target.cfg)
		}
		return nil
	}

	if concrete != nil {
		n.s.indexCommitMu.Lock()
		err := n.s.Bolt.Update(func(tx *bolt.Tx) error {
			bucket := n.GetBucket(tx, concrete.Name)
			if bucket == nil {
				return ErrNotFound
			}
			meta, err := newMeta(bucket, n)
			if err != nil {
				return err
			}
			return meta.setSchema(concrete)
		})
		n.s.indexCommitMu.Unlock()
		if err != nil {
			return err
		}
	}

	var targets []reindexTarget
	err := n.s.Bolt.View(func(tx *bolt.Tx) error {
		var err error
		targets, err = n.collectReindexTargets(tx, concrete)
		return err
	})
	if err != nil {
		return err
	}
	for _, target := range targets {
		target := target
		n.s.indexCommitMu.Lock()
		var through uint64
		err := n.s.Bolt.View(func(tx *bolt.Tx) error {
			through = durableOutboxSequence(tx)
			return nil
		})
		if err != nil {
			n.s.indexCommitMu.Unlock()
			return err
		}
		req := n.s.indexer.submitRebuild(target.cfg.Name, func() error {
			return n.reindexSnapshot(target, through)
		})
		n.s.indexCommitMu.Unlock()
		if err := req.wait(); err != nil {
			return err
		}
	}
	return nil
}

type reindexTarget struct {
	children []string
	cfg      *structConfig
}

func (n *node) collectReindexTargets(tx *bolt.Tx, concrete *structConfig) ([]reindexTarget, error) {
	if concrete != nil {
		if n.GetBucket(tx, concrete.Name) == nil {
			return nil, ErrNotFound
		}
		return []reindexTarget{{children: []string{concrete.Name}, cfg: concrete}}, nil
	}

	parent := n.GetBucket(tx)
	if len(n.rootBucket) > 0 && parent == nil {
		return nil, ErrNotFound
	}
	if len(n.rootBucket) > 0 && isStormTableBucket(parent) {
		schema, err := readStoredSchema(parent)
		if err != nil {
			return nil, err
		}
		return []reindexTarget{{cfg: structConfigFromStoredSchema(schema)}}, nil
	}

	var targets []reindexTarget
	appendBucket := func(name string, bucket *bolt.Bucket) error {
		if !isStormTableBucket(bucket) {
			return nil
		}
		schema, err := readStoredSchema(bucket)
		if err != nil {
			return err
		}
		targets = append(targets, reindexTarget{
			children: []string{name},
			cfg:      structConfigFromStoredSchema(schema),
		})
		return nil
	}

	if len(n.rootBucket) == 0 {
		cursor := tx.Cursor()
		for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
			name := string(key)
			if value != nil || isStormSystemBucket(name) {
				continue
			}
			if err := appendBucket(name, tx.Bucket(key)); err != nil {
				return nil, err
			}
		}
	} else {
		cursor := parent.Cursor()
		for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
			name := string(key)
			if value != nil || isStormSystemBucket(name) {
				continue
			}
			if err := appendBucket(name, parent.Bucket(key)); err != nil {
				return nil, err
			}
		}
	}
	if len(targets) == 0 {
		return nil, ErrNotFound
	}
	return targets, nil
}

func (n *node) reindexSnapshot(target reindexTarget, through uint64) error {
	if err := markDurableIndexTableDirty(n.s.Bolt, target.cfg.Name); err != nil {
		return err
	}
	n.s.indexer.markDirty(target.cfg.Name)
	var coverage *indexCoverage
	err := n.s.Bolt.View(func(tx *bolt.Tx) error {
		bucket := n.GetBucket(tx, target.children...)
		if bucket == nil {
			return ErrNotFound
		}
		var err error
		coverage, err = n.s.indexer.rebuildFromBolt(target.cfg, bucket, n)
		return err
	})
	if err != nil {
		return err
	}

	clean := false
	err = n.s.Bolt.Update(func(tx *bolt.Tx) error {
		bucket := n.GetBucket(tx, target.children...)
		if bucket == nil {
			return ErrNotFound
		}
		meta, err := newMeta(bucket, n)
		if err != nil {
			return err
		}
		if err := completeDurableReindex(tx, n.rootBucket, target.cfg.Name, through); err != nil {
			return err
		}
		state, err := durableIndexTableState(tx, target.cfg.Name)
		if err != nil {
			return err
		}
		if state.Desired > through {
			return meta.invalidateIndexCoverage()
		}
		clean = !state.Dirty
		if err := meta.setIndexCoverage(coverage); err != nil {
			return err
		}
		return updateReindexIncrement(meta, bucket, target.cfg)
	})
	if err == nil && clean {
		n.s.indexer.clearDirty(target.cfg.Name)
	}
	return err
}

func (n *node) reindexDurableTargets(targets []durableReindexTarget, through uint64) error {
	for _, durableTarget := range targets {
		targetNode := *n
		targetNode.rootBucket = append([]string(nil), durableTarget.Root...)
		var target reindexTarget
		err := targetNode.s.Bolt.View(func(tx *bolt.Tx) error {
			bucket := targetNode.GetBucket(tx, durableTarget.Table)
			if bucket == nil {
				return ErrNotFound
			}
			schema, err := readStoredSchema(bucket)
			if err != nil {
				return err
			}
			target = reindexTarget{
				children: []string{durableTarget.Table},
				cfg:      structConfigFromStoredSchema(schema),
			}
			return nil
		})
		if err != nil {
			return err
		}
		if err := targetNode.reindexSnapshot(target, through); err != nil {
			return err
		}
	}
	return nil
}

func updateReindexIncrement(meta *meta, bucket *bolt.Bucket, cfg *structConfig) error {
	if cfg.ID == nil || !cfg.ID.Increment {
		return nil
	}
	var lastID []byte
	cursor := bucket.Cursor()
	for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
		if value == nil || bytes.Equal(key, []byte(metadataBucket)) {
			continue
		}
		lastID = append(lastID[:0], key...)
	}
	if lastID == nil {
		return nil
	}
	return meta.setIncrement(cfg.ID, lastID)
}

type savedRecord struct {
	cfg  *structConfig
	data any
	id   []byte
	doc  map[string]any
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

	if n.tx != nil {
		return n.readWriteTx(func(tx *bolt.Tx) error {
			record, err := n.save(tx, cfg, data, false, nil)
			if err != nil {
				return err
			}
			n.markTxIndexRecord(record)
			return nil
		})
	}

	n.s.indexCommitMu.Lock()
	var record *savedRecord
	var job *durableIndexJob
	err = n.readWriteTx(func(tx *bolt.Tx) error {
		var err error
		record, err = n.save(tx, cfg, data, false, nil)
		if err != nil {
			return err
		}
		job, err = persistDurableIndexJob(tx, n.rootBucket, []*savedRecord{record}, nil, nil)
		return err
	})
	if err != nil {
		n.s.indexCommitMu.Unlock()
		return err
	}
	return n.submitCommittedDurableIndexJob(job)
}

// SaveAll saves all structures in one Bolt write transaction and indexes them in batches.
func (n *node) SaveAll(data any) error {
	inputs, err := saveAllInputs(data)
	if err != nil || len(inputs) == 0 {
		return err
	}

	if n.tx != nil {
		return n.readWriteTx(func(tx *bolt.Tx) error {
			uniqueState := newSaveAllUniqueState(n)
			for _, input := range inputs {
				record, err := n.save(tx, input.cfg, input.data, false, uniqueState)
				if err != nil {
					return err
				}
				n.markTxIndexRecord(record)
			}
			return nil
		})
	}

	n.s.indexCommitMu.Lock()
	var records []*savedRecord
	var job *durableIndexJob
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

		var err error
		job, err = persistDurableIndexJob(tx, n.rootBucket, records, nil, nil)
		return err
	})
	if err != nil {
		n.s.indexCommitMu.Unlock()
		return err
	}
	return n.submitCommittedDurableIndexJob(job)
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
	if _, ok := meta.indexCoverage(); !ok && !bucketHasRawRecords(bucket) {
		if err := meta.initializeIndexCoverage(cfg); err != nil {
			return nil, err
		}
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
	oldRaw := bucket.Get(id)
	if oldRaw != nil {
		oldRaw = append([]byte(nil), oldRaw...)
	}

	record, err := n.prepareSavedRecord(cfg, data, id, oldRaw)
	if err != nil {
		return nil, err
	}

	if err := bucket.Put(id, raw); err != nil {
		return nil, err
	}
	if err := meta.updateIndexCoverageAfterSave(cfg, oldRaw, raw); err != nil {
		return nil, err
	}

	if uniqueState != nil {
		if err := uniqueState.recordSave(cfg, id); err != nil {
			return nil, err
		}
	}
	return record, nil
}

// prepareSavedRecord builds the exact Bleve document while the caller still
// owns the Bolt transaction. Existing rows are decoded only to avoid emitting
// an outbox mutation when every indexed value is unchanged.
func (n *node) prepareSavedRecord(cfg *structConfig, data any, id, oldRaw []byte) (*savedRecord, error) {
	if !configUsesBleve(cfg) {
		return nil, nil
	}

	doc, err := n.s.indexer.document(cfg, data)
	if err != nil {
		return nil, err
	}
	if oldRaw != nil {
		old := reflect.New(cfg.Type)
		if err := n.codec.Unmarshal(oldRaw, old.Interface()); err != nil {
			return nil, err
		}
		oldDoc, err := n.s.indexer.document(cfg, old.Interface())
		if err != nil {
			return nil, err
		}
		if reflect.DeepEqual(oldDoc, doc) {
			return nil, nil
		}
	}

	return &savedRecord{
		cfg:  cfg,
		data: data,
		id:   append([]byte(nil), id...),
		doc:  doc,
	}, nil
}

func configUsesBleve(cfg *structConfig) bool {
	if cfg == nil {
		return false
	}
	for _, field := range cfg.Fields {
		if field.Indexed() {
			return true
		}
	}
	return len(cfg.CompositeIndexes) > 0
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

	if n.tx != nil {
		return n.readWriteTx(func(tx *bolt.Tx) error {
			if err := n.WithTransaction(tx).One(cfg.ID.Name, cfg.ID.Value.Interface(), current.Interface()); err != nil {
				return err
			}

			ref := reflect.ValueOf(data).Elem()
			cref := current.Elem()
			if err := fn(&ref, &cref, cfg); err != nil {
				return err
			}

			record, err := n.save(tx, cfg, current.Interface(), true, nil)
			if err != nil {
				return err
			}
			n.markTxIndexRecord(record)
			return nil
		})
	}

	n.s.indexCommitMu.Lock()
	var record *savedRecord
	var job *durableIndexJob
	err = n.readWriteTx(func(tx *bolt.Tx) error {
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

		record, err = n.save(tx, cfg, current.Interface(), true, nil)
		if err != nil {
			return err
		}
		job, err = persistDurableIndexJob(tx, n.rootBucket, []*savedRecord{record}, nil, nil)
		return err
	})
	if err != nil {
		n.s.indexCommitMu.Unlock()
		return err
	}
	return n.submitCommittedDurableIndexJob(job)
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

	if n.tx != nil {
		return n.readWriteTx(func(tx *bolt.Tx) error {
			return n.drop(tx, bucketName)
		})
	}

	n.s.indexCommitMu.Lock()
	var job *durableIndexJob
	err := n.readWriteTx(func(tx *bolt.Tx) error {
		if err := n.drop(tx, bucketName); err != nil {
			return err
		}
		var err error
		job, err = persistDurableIndexJob(tx, n.rootBucket, nil, nil, []string{bucketName})
		return err
	})
	if err != nil {
		n.s.indexCommitMu.Unlock()
		return err
	}
	return n.submitCommittedDurableIndexJob(job)
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
	}
	return nil
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

	if n.tx != nil {
		return n.readWriteTx(func(tx *bolt.Tx) error {
			record, err := n.deleteStruct(tx, cfg, id)
			if err != nil {
				return err
			}
			n.markTxIndexDelete(record)
			return nil
		})
	}

	n.s.indexCommitMu.Lock()
	var record *deletedRecord
	var job *durableIndexJob
	err = n.readWriteTx(func(tx *bolt.Tx) error {
		var err error
		record, err = n.deleteStruct(tx, cfg, id)
		if err != nil {
			return err
		}
		job, err = persistDurableIndexJob(tx, n.rootBucket, nil, []*deletedRecord{record}, nil)
		return err
	})
	if err != nil {
		n.s.indexCommitMu.Unlock()
		return err
	}
	return n.submitCommittedDurableIndexJob(job)
}

func (n *node) deleteStruct(tx *bolt.Tx, cfg *structConfig, id []byte) (*deletedRecord, error) {
	bucket := n.GetBucket(tx, cfg.Name)
	if bucket == nil {
		return nil, ErrNotFound
	}

	raw := bucket.Get(id)
	if raw == nil {
		return nil, ErrNotFound
	}
	meta, err := newMeta(bucket, n)
	if err != nil {
		return nil, err
	}
	if err := meta.updateIndexCoverageAfterDelete(cfg, raw); err != nil {
		return nil, err
	}

	if err := bucket.Delete(id); err != nil {
		return nil, err
	}
	return &deletedRecord{cfg: cfg, id: append([]byte(nil), id...)}, nil
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

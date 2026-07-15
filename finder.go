package storm

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"

	"github.com/clakeboy/storm-rev/v2/index"
	"github.com/clakeboy/storm-rev/v2/q"
	bolt "go.etcd.io/bbolt"
)

// A Finder can fetch types from BoltDB.
type Finder interface {
	// One returns one record by the specified index
	One(fieldName string, value any, to any) error

	// Find returns one or more records by the specified index
	Find(fieldName string, value any, to any, options ...func(q *index.Options)) error

	// FindByIndex returns records by a named composite index and full equality values.
	FindByIndex(indexName string, values []any, to any, options ...func(q *index.Options)) error

	// Search returns records by a full-text indexed field.
	Search(fieldName string, text string, to any, options ...func(q *index.Options)) error

	// AllByIndex gets all the records of a bucket that are indexed in the specified index
	AllByIndex(fieldName string, to any, options ...func(*index.Options)) error

	// All gets all the records of a bucket.
	// If there are no records it returns no error and the 'to' parameter is set to an empty slice.
	All(to any, options ...func(*index.Options)) error

	// Select a list of records that match a list of matchers. Doesn't use indexes.
	Select(matchers ...q.Matcher) Query

	// Range returns one or more records by the specified index within the specified range
	Range(fieldName string, min, max, to any, options ...func(*index.Options)) error

	// Prefix returns one or more records whose given field starts with the specified prefix.
	Prefix(fieldName string, prefix string, to any, options ...func(*index.Options)) error

	// Count counts all the records of a bucket
	Count(data any) (int, error)
}

// One returns one record by the specified index
func (n *node) One(fieldName string, value any, to any) error {
	sink, err := newFirstSink(n, to)
	if err != nil {
		return err
	}

	bucketName := sink.bucketName()
	if bucketName == "" {
		return ErrNoName
	}

	if fieldName == "" {
		return ErrNotFound
	}

	ref := reflect.Indirect(sink.ref)
	cfg, err := extract(&ref)
	if err != nil {
		return err
	}

	field, ok := cfg.Fields[fieldName]
	if !ok {
		return fmt.Errorf("field %s not found", fieldName)
	}
	if !ok || (!field.IsID && field.Index == "") {
		query := newQuery(n, q.StrictEq(fieldName, value))
		query.Limit(1)

		if n.tx != nil {
			err = query.query(n.tx, sink)
		} else {
			err = n.s.Bolt.View(func(tx *bolt.Tx) error {
				return query.query(tx, sink)
			})
		}

		if err != nil {
			return err
		}

		return sink.flush()
	}

	val, err := toBytes(value, n.codec)
	if err != nil {
		return err
	}

	return n.readTx(func(tx *bolt.Tx) error {
		return n.one(tx, bucketName, fieldName, cfg, to, val, value, field.IsID)
	})
}

func (n *node) one(tx *bolt.Tx, bucketName, fieldName string, cfg *structConfig, to any, val []byte, value any, skipIndex bool) error {
	bucket := n.GetBucket(tx, bucketName)
	if bucket == nil {
		return ErrNotFound
	}

	if skipIndex {
		raw := bucket.Get(val)
		if raw == nil {
			return ErrNotFound
		}
		return n.codec.Unmarshal(raw, to)
	}

	if n.tx != nil || n.s.indexer.isDirty(cfg.Name) {
		query := newQuery(n, q.StrictEq(fieldName, value))
		query.Limit(1)
		return query.query(tx, &firstSink{node: n, ref: reflect.ValueOf(to)})
	}

	ids, err := n.s.indexer.searchExact(cfg, fieldName, value)
	if err != nil {
		return err
	}
	records, err := collectRecords(n, bucket, cfg, ids, &index.Options{Limit: 1}, lessByField(n, bucket, cfg, fieldName))
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return ErrNotFound
	}
	return n.codec.Unmarshal(records[0], to)
}

// Find returns one or more records by the specified index
func (n *node) Find(fieldName string, value any, to any, options ...func(q *index.Options)) error {
	sink, err := newListSink(n, to)
	if err != nil {
		return err
	}
	bucketName := sink.bucketName()
	if bucketName == "" {
		return ErrNoName
	}

	ref := reflect.Indirect(reflect.New(sink.elemType))
	cfg, err := extract(&ref)
	if err != nil {
		return err
	}

	opts := index.NewOptions()
	for _, fn := range options {
		fn(opts)
	}

	field, ok := cfg.Fields[fieldName]
	if !ok {
		return fmt.Errorf("field %s not found", fieldName)
	}
	if !ok || (!field.IsID && (field.Index == "" || value == nil)) {
		query := newQuery(n, q.Eq(fieldName, value))
		query.Skip(opts.Skip).Limit(opts.Limit)

		if opts.Reverse {
			query.Reverse()
		}

		err = n.readTx(func(tx *bolt.Tx) error {
			return query.query(tx, sink)
		})

		if err != nil {
			return err
		}

		return sink.flush()
	}

	val, err := toBytes(value, n.codec)
	if err != nil {
		return err
	}

	return n.readTx(func(tx *bolt.Tx) error {
		return n.find(tx, bucketName, fieldName, cfg, sink, val, value, opts)
	})
}

func (n *node) find(tx *bolt.Tx, bucketName, fieldName string, cfg *structConfig, sink *listSink, val []byte, value any, opts *index.Options) error {
	bucket := n.GetBucket(tx, bucketName)
	if bucket == nil {
		return ErrNotFound
	}

	if cfg.Fields[fieldName].IsID {
		raw := bucket.Get(val)
		if raw == nil {
			return ErrNotFound
		}
		return recordsToSink([][]byte{raw}, sink)
	}

	if n.tx != nil || n.s.indexer.isDirty(cfg.Name) {
		query := newQuery(n, q.Eq(fieldName, value))
		query.Skip(opts.Skip).Limit(opts.Limit)
		if opts.Reverse {
			query.Reverse()
		}
		if err := query.query(tx, sink); err != nil {
			return err
		}
		return sink.flush()
	}

	ids, err := n.s.indexer.searchExact(cfg, fieldName, value)
	if err != nil {
		if err == index.ErrNotFound {
			return ErrNotFound
		}
		return err
	}

	records, err := collectRecords(n, bucket, cfg, ids, opts, lessByField(n, bucket, cfg, fieldName))
	if err != nil {
		return err
	}
	return recordsToSink(records, sink)
}

// FindByIndex returns records by a named composite index and full equality values.
func (n *node) FindByIndex(indexName string, values []any, to any, options ...func(q *index.Options)) error {
	sink, err := newListSink(n, to)
	if err != nil {
		return err
	}

	ref := reflect.Indirect(reflect.New(sink.elemType))
	cfg, err := extract(&ref)
	if err != nil {
		return err
	}
	composite, ok := cfg.CompositeIndexes[indexName]
	if !ok {
		return ErrIdxNotFound
	}
	if len(values) != len(composite.Fields) {
		return ErrIncompatibleValue
	}

	opts := index.NewOptions()
	for _, fn := range options {
		fn(opts)
	}

	return n.readTx(func(tx *bolt.Tx) error {
		bucket := n.GetBucket(tx, cfg.Name)
		if bucket == nil {
			return ErrNotFound
		}
		if n.tx != nil || n.s.indexer.isDirty(cfg.Name) {
			return n.findByIndexScan(tx, cfg, composite, values, sink, opts)
		}
		ids, err := n.s.indexer.searchComposite(cfg, indexName, values)
		if err != nil {
			return err
		}
		records, err := collectRecords(n, bucket, cfg, ids, opts, lessByComposite(n, bucket, cfg, composite))
		if err != nil {
			return err
		}
		return recordsToSink(records, sink)
	})
}

// Search returns records by a full-text indexed field.
func (n *node) Search(fieldName string, text string, to any, options ...func(q *index.Options)) error {
	sink, err := newListSink(n, to)
	if err != nil {
		return err
	}
	bucketName := sink.bucketName()
	if bucketName == "" {
		return ErrNoName
	}

	ref := reflect.Indirect(reflect.New(sink.elemType))
	cfg, err := extract(&ref)
	if err != nil {
		return err
	}

	field, ok := cfg.Fields[fieldName]
	if !ok {
		return fmt.Errorf("field %s not found", fieldName)
	}
	if field.Index != tagFullText {
		return ErrIdxNotFound
	}

	opts := index.NewOptions()
	for _, fn := range options {
		fn(opts)
	}

	return n.readTx(func(tx *bolt.Tx) error {
		return n.search(tx, bucketName, fieldName, cfg, sink, text, opts)
	})
}

func (n *node) search(tx *bolt.Tx, bucketName, fieldName string, cfg *structConfig, sink *listSink, text string, opts *index.Options) error {
	bucket := n.GetBucket(tx, bucketName)
	if bucket == nil {
		return ErrNotFound
	}

	if n.tx != nil || n.s.indexer.isDirty(cfg.Name) {
		return n.searchFullTextScan(tx, fieldName, cfg, sink, text, opts)
	}

	ids, err := n.s.indexer.searchFullText(cfg, fieldName, text)
	if err != nil {
		return err
	}
	records, err := collectRecordsInOrder(n, bucket, cfg, ids, opts)
	if err != nil {
		return err
	}
	return recordsToSink(records, sink)
}

// AllByIndex gets all the records of a bucket that are indexed in the specified index
func (n *node) AllByIndex(fieldName string, to any, options ...func(*index.Options)) error {
	if fieldName == "" {
		return n.All(to, options...)
	}

	ref := reflect.ValueOf(to)

	if ref.Kind() != reflect.Ptr || ref.Elem().Kind() != reflect.Slice {
		return ErrSlicePtrNeeded
	}

	typ := reflect.Indirect(ref).Type().Elem()

	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}

	newElem := reflect.New(typ)

	cfg, err := extract(&newElem)
	if err != nil {
		return err
	}

	if cfg.ID.Name == fieldName {
		return n.All(to, options...)
	}

	opts := index.NewOptions()
	for _, fn := range options {
		fn(opts)
	}

	return n.readTx(func(tx *bolt.Tx) error {
		return n.allByIndex(tx, fieldName, cfg, &ref, opts)
	})
}

func (n *node) allByIndex(tx *bolt.Tx, fieldName string, cfg *structConfig, ref *reflect.Value, opts *index.Options) error {
	bucket := n.GetBucket(tx, cfg.Name)
	if bucket == nil {
		return ErrNotFound
	}

	fieldCfg, ok := cfg.Fields[fieldName]
	if !ok {
		return ErrNotFound
	}
	if fieldCfg.Index == "" {
		return ErrIdxNotFound
	}

	if n.tx != nil || n.s.indexer.isDirty(cfg.Name) {
		return n.allByIndexScan(tx, fieldName, cfg, ref, opts)
	}

	ids, err := n.s.indexer.searchAllByIndex(cfg, fieldName)
	if err != nil {
		if err == index.ErrNotFound {
			return ErrNotFound
		}
		return err
	}

	records, err := collectRecords(n, bucket, cfg, ids, opts, lessByField(n, bucket, cfg, fieldName))
	if err != nil {
		return err
	}

	return setSliceFromRecords(n, ref, records)
}

// All gets all the records of a bucket.
// If there are no records it returns no error and the 'to' parameter is set to an empty slice.
func (n *node) All(to any, options ...func(*index.Options)) error {
	opts := index.NewOptions()
	for _, fn := range options {
		fn(opts)
	}

	query := newQuery(n, nil).Limit(opts.Limit).Skip(opts.Skip)
	if opts.Reverse {
		query.Reverse()
	}

	err := query.Find(to)
	if err != nil && err != ErrNotFound {
		return err
	}

	if err == ErrNotFound {
		ref := reflect.ValueOf(to)
		results := reflect.MakeSlice(reflect.Indirect(ref).Type(), 0, 0)
		reflect.Indirect(ref).Set(results)
	}
	return nil
}

// Range returns one or more records by the specified index within the specified range
func (n *node) Range(fieldName string, min, max, to any, options ...func(*index.Options)) error {
	sink, err := newListSink(n, to)
	if err != nil {
		return err
	}

	bucketName := sink.bucketName()
	if bucketName == "" {
		return ErrNoName
	}

	ref := reflect.Indirect(reflect.New(sink.elemType))
	cfg, err := extract(&ref)
	if err != nil {
		return err
	}

	opts := index.NewOptions()
	for _, fn := range options {
		fn(opts)
	}

	field, ok := cfg.Fields[fieldName]
	if !ok {
		return fmt.Errorf("field %s not found", fieldName)
	}
	if !ok || (!field.IsID && field.Index == "") {
		query := newQuery(n, q.And(q.Gte(fieldName, min), q.Lte(fieldName, max)))
		query.Skip(opts.Skip).Limit(opts.Limit)

		if opts.Reverse {
			query.Reverse()
		}

		err = n.readTx(func(tx *bolt.Tx) error {
			return query.query(tx, sink)
		})

		if err != nil {
			return err
		}

		return sink.flush()
	}

	return n.readTx(func(tx *bolt.Tx) error {
		return n.rnge(tx, bucketName, fieldName, cfg, sink, min, max, opts)
	})
}

func (n *node) rnge(tx *bolt.Tx, bucketName, fieldName string, cfg *structConfig, sink *listSink, min, max any, opts *index.Options) error {
	bucket := n.GetBucket(tx, bucketName)
	if bucket == nil {
		reflect.Indirect(sink.ref).SetLen(0)
		return nil
	}

	if n.tx != nil || n.s.indexer.isDirty(cfg.Name) {
		query := newQuery(n, q.And(q.Gte(fieldName, min), q.Lte(fieldName, max)))
		query.Skip(opts.Skip).Limit(opts.Limit)
		if opts.Reverse {
			query.Reverse()
		}
		if err := query.query(tx, sink); err != nil {
			return err
		}
		return sink.flush()
	}

	ids, err := n.s.indexer.searchRange(cfg, fieldName, min, max)
	if err != nil {
		return err
	}

	records, err := collectRecords(n, bucket, cfg, ids, opts, lessByField(n, bucket, cfg, fieldName))
	if err != nil {
		return err
	}
	return recordsToSink(records, sink)
}

// Prefix returns one or more records whose given field starts with the specified prefix.
func (n *node) Prefix(fieldName string, prefix string, to any, options ...func(*index.Options)) error {
	sink, err := newListSink(n, to)
	if err != nil {
		return err
	}

	bucketName := sink.bucketName()
	if bucketName == "" {
		return ErrNoName
	}

	ref := reflect.Indirect(reflect.New(sink.elemType))
	cfg, err := extract(&ref)
	if err != nil {
		return err
	}

	opts := index.NewOptions()
	for _, fn := range options {
		fn(opts)
	}

	field, ok := cfg.Fields[fieldName]
	if !ok {
		return fmt.Errorf("field %s not found", fieldName)
	}
	if !ok || (!field.IsID && field.Index == "") {
		query := newQuery(n, q.Re(fieldName, fmt.Sprintf("^%s", prefix)))
		query.Skip(opts.Skip).Limit(opts.Limit)

		if opts.Reverse {
			query.Reverse()
		}

		err = n.readTx(func(tx *bolt.Tx) error {
			return query.query(tx, sink)
		})

		if err != nil {
			return err
		}

		return sink.flush()
	}

	return n.readTx(func(tx *bolt.Tx) error {
		return n.prefix(tx, bucketName, fieldName, cfg, sink, prefix, opts)
	})
}

func (n *node) prefix(tx *bolt.Tx, bucketName, fieldName string, cfg *structConfig, sink *listSink, prefix string, opts *index.Options) error {
	bucket := n.GetBucket(tx, bucketName)
	if bucket == nil {
		reflect.Indirect(sink.ref).SetLen(0)
		return nil
	}

	if n.tx != nil || n.s.indexer.isDirty(cfg.Name) {
		query := newQuery(n, q.Re(fieldName, fmt.Sprintf("^%s", prefix)))
		query.Skip(opts.Skip).Limit(opts.Limit)
		if opts.Reverse {
			query.Reverse()
		}
		if err := query.query(tx, sink); err != nil {
			return err
		}
		return sink.flush()
	}

	ids, err := n.s.indexer.searchPrefix(cfg, fieldName, prefix)
	if err != nil {
		return err
	}

	records, err := collectRecords(n, bucket, cfg, ids, opts, lessByField(n, bucket, cfg, fieldName))
	if err != nil {
		return err
	}
	return recordsToSink(records, sink)
}

// recordsToSink feeds raw Bolt records into an existing sink and preserves sink errors.
func recordsToSink(records [][]byte, sink sink) error {
	for _, raw := range records {
		if err := sink.add(&item{v: raw}); err != nil {
			return err
		}
	}
	return sink.flush()
}

// allByIndexScan is the dirty/transaction fallback for AllByIndex.
func (n *node) allByIndexScan(tx *bolt.Tx, fieldName string, cfg *structConfig, ref *reflect.Value, opts *index.Options) error {
	bucket := n.GetBucket(tx, cfg.Name)
	if bucket == nil {
		return ErrNotFound
	}

	ids := make([][]byte, 0)
	c := bucket.Cursor()
	for k, raw := c.First(); k != nil; k, raw = c.Next() {
		if raw == nil {
			continue
		}
		elem := reflect.New(cfg.Type)
		if err := n.codec.Unmarshal(raw, elem.Interface()); err != nil {
			return err
		}
		field := elem.Elem().FieldByName(fieldName)
		if !field.IsValid() || isZero(&field) {
			continue
		}
		ids = append(ids, append([]byte(nil), k...))
	}

	records, err := collectRecords(n, bucket, cfg, ids, opts, lessByField(n, bucket, cfg, fieldName))
	if err != nil {
		return err
	}
	return setSliceFromRecords(n, ref, records)
}

// findByIndexScan is the dirty/transaction fallback for composite equality queries.
func (n *node) findByIndexScan(tx *bolt.Tx, cfg *structConfig, composite *compositeIndexConfig, values []any, sink *listSink, opts *index.Options) error {
	bucket := n.GetBucket(tx, cfg.Name)
	if bucket == nil {
		return ErrNotFound
	}

	expected := make([][]byte, len(values))
	for i, value := range values {
		raw, err := toBytes(value, n.codec)
		if err != nil {
			return err
		}
		expected[i] = raw
	}

	ids := make([][]byte, 0)
	c := bucket.Cursor()
	for k, raw := c.First(); k != nil; k, raw = c.Next() {
		if raw == nil {
			continue
		}
		elem := reflect.New(cfg.Type)
		if err := n.codec.Unmarshal(raw, elem.Interface()); err != nil {
			return err
		}
		matched := true
		for i, fieldCfg := range composite.Fields {
			field := elem.Elem().FieldByName(fieldCfg.Name)
			if !field.IsValid() || isZero(&field) {
				matched = false
				break
			}
			actual, err := toBytes(field.Interface(), n.codec)
			if err != nil {
				return err
			}
			if !bytes.Equal(actual, expected[i]) {
				matched = false
				break
			}
		}
		if matched {
			ids = append(ids, append([]byte(nil), k...))
		}
	}

	records, err := collectRecords(n, bucket, cfg, ids, opts, lessByComposite(n, bucket, cfg, composite))
	if err != nil {
		return err
	}
	return recordsToSink(records, sink)
}

// searchFullTextScan is the dirty/transaction fallback for full-text queries.
// It intentionally uses a simple case-insensitive substring match because Bolt
// records do not have Bleve analyzer metadata.
func (n *node) searchFullTextScan(tx *bolt.Tx, fieldName string, cfg *structConfig, sink *listSink, text string, opts *index.Options) error {
	bucket := n.GetBucket(tx, cfg.Name)
	if bucket == nil {
		return ErrNotFound
	}

	needle := strings.ToLower(text)
	ids := make([][]byte, 0)
	c := bucket.Cursor()
	for k, raw := c.First(); k != nil; k, raw = c.Next() {
		if raw == nil {
			continue
		}
		elem := reflect.New(cfg.Type)
		if err := n.codec.Unmarshal(raw, elem.Interface()); err != nil {
			return err
		}
		field := elem.Elem().FieldByName(fieldName)
		if !field.IsValid() || isZero(&field) {
			continue
		}
		haystack, ok := fullTextValue(field)
		if !ok {
			continue
		}
		if strings.Contains(strings.ToLower(haystack), needle) {
			ids = append(ids, append([]byte(nil), k...))
		}
	}

	records, err := collectRecordsInOrder(n, bucket, cfg, ids, opts)
	if err != nil {
		return err
	}
	return recordsToSink(records, sink)
}

// setSliceFromRecords fills a result slice directly, matching AllByIndex's empty-result behavior.
func setSliceFromRecords(n *node, ref *reflect.Value, records [][]byte) error {
	sliceType := reflect.Indirect(*ref).Type()
	elemType := sliceType.Elem()
	isPtr := elemType.Kind() == reflect.Ptr
	if isPtr {
		elemType = elemType.Elem()
	}

	results := reflect.MakeSlice(sliceType, len(records), len(records))
	for i, raw := range records {
		elem := reflect.New(elemType)
		if err := n.codec.Unmarshal(raw, elem.Interface()); err != nil {
			return err
		}
		if isPtr {
			results.Index(i).Set(elem)
		} else {
			results.Index(i).Set(elem.Elem())
		}
	}
	reflect.Indirect(*ref).Set(results)
	return nil
}

// Count counts all the records of a bucket
func (n *node) Count(data any) (int, error) {
	return n.Select().Count(data)
}

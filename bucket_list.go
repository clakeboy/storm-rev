package storm

import bolt "go.etcd.io/bbolt"

// ColumnInfo describes one persisted column in a Storm table schema.
type ColumnInfo struct {
	// Name is the Go struct field name.
	Name string `json:"name"`

	// JSON is the JSON field name used when the record is encoded.
	JSON string `json:"json"`

	// Type is the Go type name stored in the table schema.
	Type string `json:"type"`

	// Index is the Storm index tag value, such as index or unique.
	Index string `json:"index,omitempty"`

	// ID reports whether this column is the table ID column.
	ID bool `json:"id,omitempty"`

	// Increment reports whether Storm auto-increments this column.
	Increment bool `json:"increment,omitempty"`

	// IncrementStart is the configured starting value for an increment column.
	IncrementStart int64 `json:"increment_start,omitempty"`

	// Integer reports whether the column stores an integer type.
	Integer bool `json:"integer,omitempty"`

	// Composites records composite-index names and field order.
	Composites map[string]int `json:"composites,omitempty"`
}

// ListFroms returns direct From bucket names under the current DB root.
func (s *DB) ListFroms() ([]string, error) {
	var froms []string

	err := s.Bolt.View(func(tx *bolt.Tx) error {
		n := s.Node.(*node)
		c := n.cursor(tx)
		if c == nil {
			return nil
		}

		for k, v := c.First(); k != nil; k, v = c.Next() {
			name := string(k)
			if v != nil || isStormSystemBucket(name) {
				continue
			}

			if bucketHasDirectTable(n.GetBucket(tx, name)) {
				froms = append(froms, name)
			}
		}

		return nil
	})

	return froms, err
}

// ListTables returns direct table bucket names below the given From bucket.
func (s *DB) ListTables(from string) ([]string, error) {
	var tables []string

	if isStormSystemBucket(from) {
		return nil, ErrNotFound
	}

	err := s.Bolt.View(func(tx *bolt.Tx) error {
		n := s.Node.(*node)
		bucket := n.GetBucket(tx, from)
		if bucket == nil {
			return ErrNotFound
		}

		tables = appendDirectTables(tables, bucket)
		return nil
	})

	return tables, err
}

// ListColumns returns persisted column metadata for a table below the given From bucket.
func (s *DB) ListColumns(from, table string) ([]ColumnInfo, error) {
	if isStormSystemBucket(from) || isStormSystemBucket(table) {
		return nil, ErrNotFound
	}

	var columns []ColumnInfo
	err := s.Bolt.View(func(tx *bolt.Tx) error {
		n := s.Node.(*node)
		bucket := tableBucket(tx, n, from, table)
		if bucket == nil {
			return ErrNotFound
		}

		schema, err := readStoredSchema(bucket)
		if err != nil {
			return err
		}

		columns = columnInfoFromStoredSchema(schema)
		return nil
	})

	return columns, err
}

// tableBucket returns a table bucket below from, or below the DB root when from is empty.
func tableBucket(tx *bolt.Tx, n *node, from, table string) *bolt.Bucket {
	if from == "" {
		return n.GetBucket(tx, table)
	}
	return n.GetBucket(tx, from, table)
}

// columnInfoFromStoredSchema converts internal schema fields into the public API type.
func columnInfoFromStoredSchema(schema *storedSchema) []ColumnInfo {
	if schema == nil {
		return nil
	}

	columns := make([]ColumnInfo, 0, len(schema.Fields))
	for _, field := range schema.Fields {
		columns = append(columns, ColumnInfo{
			Name:           field.Name,
			JSON:           field.JSON,
			Type:           field.Type,
			Index:          field.Index,
			ID:             field.ID,
			Increment:      field.Increment,
			IncrementStart: field.IncrementStart,
			Integer:        field.Integer,
			Composites:     copyCompositeInfo(field.Composites),
		})
	}
	return columns
}

// copyCompositeInfo protects schema metadata from caller-side map mutation.
func copyCompositeInfo(composites map[string]int) map[string]int {
	if len(composites) == 0 {
		return nil
	}

	clone := make(map[string]int, len(composites))
	for name, order := range composites {
		clone[name] = order
	}
	return clone
}

// bucketHasDirectTable reports whether b contains at least one direct table bucket.
func bucketHasDirectTable(b *bolt.Bucket) bool {
	if b == nil {
		return false
	}

	c := b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		if v != nil || isStormSystemBucket(string(k)) {
			continue
		}
		if isStormTableBucket(b.Bucket(k)) {
			return true
		}
	}
	return false
}

// appendDirectTables appends direct child buckets that carry Storm schema metadata.
func appendDirectTables(tables []string, b *bolt.Bucket) []string {
	if b == nil {
		return tables
	}

	c := b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		name := string(k)
		if v != nil || isStormSystemBucket(name) {
			continue
		}
		if isStormTableBucket(b.Bucket(k)) {
			tables = append(tables, name)
		}
	}
	return tables
}

// isStormTableBucket reports whether b has persisted struct schema metadata.
func isStormTableBucket(b *bolt.Bucket) bool {
	if b == nil {
		return false
	}

	meta := b.Bucket([]byte(metadataBucket))
	return meta != nil && meta.Get([]byte(metaSchema)) != nil
}

// isStormSystemBucket reports whether name is reserved for Storm internal data.
func isStormSystemBucket(name string) bool {
	return name == dbinfo || name == metadataBucket ||
		name == bleveOutboxBucket || name == bleveStateBucket
}

package storm

import (
	"fmt"
	"go/token"
	"reflect"
	"unicode"

	bolt "go.etcd.io/bbolt"
)

// SetTableMetadata replaces the persisted column metadata for an existing table.
//
// The table bucket must already exist, but it does not need to have
// __storm_metadata/schema yet. External indexes are not rebuilt here; call
// ReIndex after changing indexed fields when the new metadata should take effect.
func (s *DB) SetTableMetadata(from, table string, columns []ColumnInfo) error {
	if isStormSystemBucket(from) || isStormSystemBucket(table) {
		return ErrNotFound
	}

	schema, err := storedSchemaFromColumns(table, columns)
	if err != nil {
		return err
	}
	cfg := structConfigFromStoredSchema(schema)
	if err := validateCompositeIndexes(cfg); err != nil {
		return err
	}

	return s.Bolt.Update(func(tx *bolt.Tx) error {
		n := s.Node.(*node)
		bucket := tableBucket(tx, n, from, table)
		if bucket == nil {
			return ErrNotFound
		}

		meta, err := newMeta(bucket, n)
		if err != nil {
			return err
		}
		if err := meta.setStoredSchema(*schema); err != nil {
			return err
		}

		return nil
	})
}

// storedSchemaFromColumns validates the public metadata shape and converts it to
// the internal persisted schema format.
func storedSchemaFromColumns(table string, columns []ColumnInfo) (*storedSchema, error) {
	if table == "" {
		return nil, ErrNoName
	}
	if len(columns) == 0 {
		return nil, ErrNilParam
	}

	explicitID := ""
	for _, column := range columns {
		if column.ID {
			if explicitID != "" {
				return nil, fmt.Errorf("%w: multiple id fields", ErrNoID)
			}
			explicitID = column.Name
		}
	}

	schema := &storedSchema{
		Table:  table,
		Fields: make([]storedSchemaField, 0, len(columns)),
	}
	seen := make(map[string]bool, len(columns))
	for _, column := range columns {
		if !validMetadataFieldName(column.Name) {
			return nil, fmt.Errorf("%w: invalid column name %q", ErrNoName, column.Name)
		}
		if seen[column.Name] {
			return nil, fmt.Errorf("%w: duplicate column %q", ErrNoName, column.Name)
		}
		seen[column.Name] = true
		if err := validateMetadataIndex(column.Index); err != nil {
			return nil, err
		}

		field := storedSchemaField{
			Name:           column.Name,
			JSON:           column.JSON,
			Type:           column.Type,
			Index:          column.Index,
			ID:             column.ID || (explicitID == "" && column.Name == "ID"),
			Increment:      column.Increment,
			IncrementStart: column.IncrementStart,
			Integer:        column.Integer || storedTypeIsInteger(column.Type),
			Composites:     copyCompositeInfo(column.Composites),
		}
		if field.JSON == "" {
			field.JSON = field.Name
		}
		if field.ID {
			field.Index = tagUniqueIdx
			schema.ID = field.Name
		}
		schema.Fields = append(schema.Fields, field)
	}
	if schema.ID == "" {
		return nil, ErrNoID
	}

	return schema, nil
}

// validMetadataFieldName keeps metadata safe for reflect.StructOf.
func validMetadataFieldName(name string) bool {
	if !token.IsIdentifier(name) {
		return false
	}
	for _, r := range name {
		return unicode.IsUpper(r)
	}
	return false
}

// validateMetadataIndex accepts the same index tags the struct extractor accepts.
func validateMetadataIndex(index string) error {
	switch index {
	case "", tagIdx, tagUniqueIdx, tagFullText:
		return nil
	default:
		return ErrUnknownTag
	}
}

// storedTypeIsInteger infers integer metadata when callers provide a known Go type.
func storedTypeIsInteger(name string) bool {
	typ := storedFieldType(name)
	if typ == nil {
		return false
	}
	switch typ.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	default:
		return false
	}
}

package storm

import (
	"encoding/json"
	"reflect"
	"sort"

	bolt "go.etcd.io/bbolt"
)

const (
	metaCodec  = "codec"
	metaSchema = "schema"
)

func newMeta(b *bolt.Bucket, n Node) (*meta, error) {
	m := b.Bucket([]byte(metadataBucket))
	if m != nil {
		name := m.Get([]byte(metaCodec))
		if string(name) != n.Codec().Name() {
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

// setSchema persists enough struct metadata for SQL map/DTO queries without registration.
func (m *meta) setSchema(cfg *structConfig) error {
	raw, err := json.Marshal(storedSchemaFromConfig(cfg))
	if err != nil {
		return err
	}
	return m.bucket.Put([]byte(metaSchema), raw)
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

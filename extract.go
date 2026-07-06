package storm

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// Storm tags
const (
	tagID        = "id"
	tagIdx       = "index"
	tagUniqueIdx = "unique"
	tagFullText  = "fulltext"
	tagInline    = "inline"
	tagIncrement = "increment"
	tagComposite = "composite"
	indexPrefix  = "__storm_index_"
)

type fieldConfig struct {
	Name           string
	Index          string
	IsZero         bool
	IsID           bool
	Increment      bool
	IncrementStart int64
	IsInteger      bool
	Value          *reflect.Value
	ForceUpdate    bool
	JsonFieldName  string
	Composites     map[string]int
}

// structConfig is a structure gathering all the relevant informations about a model
type structConfig struct {
	Name             string
	Type             reflect.Type
	Fields           map[string]*fieldConfig
	ID               *fieldConfig
	CompositeIndexes map[string]*compositeIndexConfig
}

type compositeIndexConfig struct {
	Name   string
	Fields []*fieldConfig
}

func extract(s *reflect.Value, mi ...*structConfig) (*structConfig, error) {
	if s.Kind() == reflect.Ptr {
		e := s.Elem()
		s = &e
	}
	if s.Kind() != reflect.Struct {
		return nil, ErrBadType
	}

	typ := s.Type()

	var child bool

	var m *structConfig
	if len(mi) > 0 {
		m = mi[0]
		child = true
	} else {
		m = &structConfig{}
		m.Fields = make(map[string]*fieldConfig)
	}

	if m.Name == "" {
		m.Name = typ.Name()
	}
	if m.Type == nil {
		m.Type = typ
	}

	numFields := s.NumField()
	for i := 0; i < numFields; i++ {
		field := typ.Field(i)
		value := s.Field(i)

		if field.PkgPath != "" {
			continue
		}

		err := extractField(&value, &field, m, child)
		if err != nil {
			return nil, err
		}
	}

	if child {
		return m, nil
	}

	if m.ID == nil {
		return nil, ErrNoID
	}

	if m.Name == "" {
		return nil, ErrNoName
	}

	if err := validateCompositeIndexes(m); err != nil {
		return nil, err
	}

	return m, nil
}

func extractField(value *reflect.Value, field *reflect.StructField, m *structConfig, isChild bool) error {
	var f *fieldConfig
	var err error

	tag := field.Tag.Get("storm")
	if tag != "" {
		f = &fieldConfig{
			Name:           field.Name,
			IsZero:         isZero(value),
			IsInteger:      isInteger(value),
			Value:          value,
			IncrementStart: 1,
			JsonFieldName:  jsonFieldName(field),
		}

		tags := strings.Split(tag, ",")

		for _, tag := range tags {
			switch tag {
			case "id":
				f.IsID = true
				f.Index = tagUniqueIdx
			case tagUniqueIdx, tagIdx, tagFullText:
				f.Index = tag
			case tagInline:
				if value.Kind() == reflect.Ptr {
					e := value.Elem()
					value = &e
				}
				if value.Kind() == reflect.Struct {
					a := value.Addr()
					_, err := extract(&a, m)
					if err != nil {
						return err
					}
				}
				// we don't need to save this field
				return nil
			default:
				if strings.HasPrefix(tag, tagComposite+"=") {
					if err := parseCompositeTag(f, tag); err != nil {
						return err
					}
				} else if strings.HasPrefix(tag, tagIncrement) {
					f.Increment = true
					parts := strings.Split(tag, "=")
					if parts[0] != tagIncrement {
						return ErrUnknownTag
					}
					if len(parts) > 1 {
						f.IncrementStart, err = strconv.ParseInt(parts[1], 0, 64)
						if err != nil {
							return err
						}
					}
				} else {
					return ErrUnknownTag
				}
			}
		}

		if _, ok := m.Fields[f.Name]; !ok || !isChild {
			m.Fields[f.Name] = f
		}
	}

	if m.ID == nil && f != nil && f.IsID {
		m.ID = f
	}

	// the field is named ID and no ID field has been detected before
	if m.ID == nil && field.Name == "ID" {
		if f == nil {
			f = &fieldConfig{
				Index:          tagUniqueIdx,
				Name:           field.Name,
				IsZero:         isZero(value),
				IsInteger:      isInteger(value),
				IsID:           true,
				Value:          value,
				IncrementStart: 1,
				JsonFieldName:  jsonFieldName(field),
			}
			m.Fields[field.Name] = f
		}
		m.ID = f
	}
	if _, ok := m.Fields[field.Name]; !ok {
		// Keep non-indexed fields complete for persisted schema metadata.
		f = &fieldConfig{
			Name:          field.Name,
			IsZero:        isZero(value),
			IsInteger:     isInteger(value),
			Value:         value,
			JsonFieldName: jsonFieldName(field),
		}
		m.Fields[field.Name] = f
	}

	return nil
}

func jsonFieldName(field *reflect.StructField) string {
	name := strings.Split(field.Tag.Get("json"), ",")[0]
	if name == "" {
		return field.Name
	}
	return name
}

func extractSingleField(ref *reflect.Value, fieldName string) (*structConfig, error) {
	var cfg structConfig
	cfg.Fields = make(map[string]*fieldConfig)

	f, ok := ref.Type().FieldByName(fieldName)
	if !ok || f.PkgPath != "" {
		return nil, fmt.Errorf("field %s not found", fieldName)
	}

	v := ref.FieldByName(fieldName)
	err := extractField(&v, &f, &cfg, false)
	if err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Indexed reports whether this field must be present in the external Bleve index.
func (f *fieldConfig) Indexed() bool {
	return f != nil && (f.Index != "" || len(f.Composites) > 0)
}

// parseCompositeTag parses storm:"composite=name:order[;other:order]".
func parseCompositeTag(f *fieldConfig, tag string) error {
	raw := strings.TrimPrefix(tag, tagComposite+"=")
	if raw == "" {
		return ErrUnknownTag
	}
	if f.Composites == nil {
		f.Composites = make(map[string]int)
	}

	for _, part := range strings.Split(raw, ";") {
		name, orderText, ok := strings.Cut(part, ":")
		if !ok || name == "" || orderText == "" {
			return ErrUnknownTag
		}
		order, err := strconv.Atoi(orderText)
		if err != nil || order <= 0 {
			return ErrUnknownTag
		}
		f.Composites[name] = order
	}
	return nil
}

// validateCompositeIndexes builds ordered composite index metadata and rejects ambiguous tags.
func validateCompositeIndexes(cfg *structConfig) error {
	grouped := make(map[string]map[int]*fieldConfig)
	for _, field := range cfg.Fields {
		for name, order := range field.Composites {
			if grouped[name] == nil {
				grouped[name] = make(map[int]*fieldConfig)
			}
			if grouped[name][order] != nil {
				return fmt.Errorf("composite index %s has duplicate order %d", name, order)
			}
			grouped[name][order] = field
		}
	}
	if len(grouped) == 0 {
		return nil
	}

	cfg.CompositeIndexes = make(map[string]*compositeIndexConfig, len(grouped))
	for name, fieldsByOrder := range grouped {
		orders := make([]int, 0, len(fieldsByOrder))
		for order := range fieldsByOrder {
			orders = append(orders, order)
		}
		sort.Ints(orders)
		for i, order := range orders {
			if order != i+1 {
				return fmt.Errorf("composite index %s must use continuous order starting at 1", name)
			}
		}

		composite := &compositeIndexConfig{Name: name, Fields: make([]*fieldConfig, 0, len(orders))}
		for _, order := range orders {
			composite.Fields = append(composite.Fields, fieldsByOrder[order])
		}
		cfg.CompositeIndexes[name] = composite
	}
	return nil
}

func isZero(v *reflect.Value) bool {
	zero := reflect.Zero(v.Type()).Interface()
	current := v.Interface()
	return reflect.DeepEqual(current, zero)
}

func isInteger(v *reflect.Value) bool {
	if v == nil {
		return false
	}
	kind := v.Kind()
	return kind >= reflect.Int && kind <= reflect.Uint64
}

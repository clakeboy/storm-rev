package q

import (
	"errors"
	"fmt"
	"go/token"
	"reflect"
	"strings"

	"github.com/tidwall/gjson"
)

// ErrUnknownField is returned when an unknown field is passed.
var ErrUnknownField = errors.New("unknown field")

type fieldMatcherDelegate struct {
	FieldMatcher
	Field string
}

// NewFieldMatcher creates a Matcher for a given field.
func NewFieldMatcher(field string, fm FieldMatcher) Matcher {
	return &fieldMatcherDelegate{Field: field, FieldMatcher: fm}
}

// FieldMatcher can be used in NewFieldMatcher as a simple way to create the
// most common Matcher: A Matcher that evaluates one field's value.
// For more complex scenarios, implement the Matcher interface directly.
type FieldMatcher interface {
	MatchField(v any) (bool, error)
}

func (r fieldMatcherDelegate) Match(i any) (bool, error) {
	// v := reflect.Indirect(reflect.ValueOf(i))
	return r.MatchValue(i)
}

func (r fieldMatcherDelegate) MatchValue(v any) (bool, error) {
	// field := v.FieldByName(r.Field)
	// if !field.IsValid() {
	// 	return false, ErrUnknownField
	// }
	res := gjson.GetBytes(v.([]byte), strings.ToLower(r.Field))
	if !res.Exists() {
		return false, ErrUnknownField
	}
	var val any
	switch res.Type {
	case gjson.String:
		val = res.Str
	case gjson.Number:
		val = res.Num
	case gjson.True:
		val = true
	case gjson.False:
		val = false
	case gjson.JSON:
		val = res.Raw
	}
	return r.MatchField(val)
}

func (r fieldMatcherDelegate) GetField() string {
	return r.Field
}

func (r *fieldMatcherDelegate) SetField(field string) {
	r.Field = field
}

// NewField2FieldMatcher creates a Matcher for a given field1 and field2.
func NewField2FieldMatcher(field1, field2 string, tok token.Token) Matcher {
	return &field2fieldMatcherDelegate{Field1: field1, Field2: field2, Tok: tok}
}

type field2fieldMatcherDelegate struct {
	Field1, Field2 string
	Tok            token.Token
}

func (r field2fieldMatcherDelegate) Match(i any) (bool, error) {
	v := reflect.Indirect(reflect.ValueOf(i))
	return r.MatchValue(&v)
}

func (r field2fieldMatcherDelegate) MatchValue(v *reflect.Value) (bool, error) {
	field1 := v.FieldByName(r.Field1)
	if !field1.IsValid() {
		return false, ErrUnknownField
	}
	field2 := v.FieldByName(r.Field2)
	if !field2.IsValid() {
		return false, ErrUnknownField
	}
	return compare(field1.Interface(), field2.Interface(), r.Tok), nil
}

func (r field2fieldMatcherDelegate) GetField() string {
	return fmt.Sprintf("%s_%s", r.Field1, r.Field2)
}

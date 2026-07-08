package storm

import (
	"bytes"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/clakeboy/storm-rev/q"
	"github.com/tidwall/gjson"
	"github.com/xwb1989/sqlparser"
	bolt "go.etcd.io/bbolt"
)

var timeType = reflect.TypeOf(time.Time{})

// SQLResult describes the effect of a SQL write statement.
type SQLResult struct {
	RowsAffected int
	LastInsertID any
}

// SQL interprets a small single-table SQL subset on top of Storm's typed APIs.
type SQL struct {
	node                *node
	models              map[string]*sqlModel
	allowFullTableWrite bool
}

type sqlModel struct {
	table      string
	typ        reflect.Type
	cfg        *structConfig
	fields     map[string]*fieldConfig
	fieldTypes map[string]reflect.Type
	metadata   bool
}

type sqlTableRef struct {
	name  string
	alias string
}

type sqlSelectPlan struct {
	model      *sqlModel
	table      sqlTableRef
	matcher    q.Matcher
	whereEmpty bool
	orderBy    []string
	reverse    bool
	limit      int
	skip       int
}

type sqlProjection struct {
	field *fieldConfig
	name  string
}

type sqlAssignment struct {
	field *fieldConfig
	value any
}

type sqlArgReader struct {
	args []any
	pos  int
}

type sqlRawRecord struct {
	raw []byte
}

// SQL returns a SQL interpreter bound to this node and the provided models.
func (n *node) SQL(models ...any) (*SQL, error) {
	s := &SQL{
		node:   n,
		models: make(map[string]*sqlModel),
	}
	if err := s.Register(models...); err != nil {
		return nil, err
	}
	return s, nil
}

// Register adds model metadata used to bind SQL table and column names.
func (s *SQL) Register(models ...any) error {
	if s.models == nil {
		s.models = make(map[string]*sqlModel)
	}
	for _, model := range models {
		info, err := newSQLModel(model)
		if err != nil {
			return err
		}
		s.models[info.table] = info
	}
	return nil
}

// WithAllowFullTableWrite returns a copy that allows UPDATE/DELETE without WHERE.
func (s *SQL) WithAllowFullTableWrite(allow bool) *SQL {
	clone := *s
	clone.allowFullTableWrite = allow
	return &clone
}

// Find executes a SELECT * query and decodes full model records into to.
func (s *SQL) Find(query string, to any, args ...any) error {
	stmt, reader, err := s.parseSelect(query, args)
	if err != nil {
		return err
	}
	if !selectHasOnlyStar(stmt) {
		return fmt.Errorf("%w: use Project for selected columns", ErrUnsupportedSQL)
	}
	if err := s.registerSelectTarget(stmt, to); err != nil {
		return err
	}
	plan, err := s.buildSelectPlan(stmt, reader)
	if err != nil {
		return err
	}
	if err := reader.done(); err != nil {
		return err
	}
	qr := s.queryFromPlan(plan)
	return qr.Find(to)
}

// First executes a SELECT * query and decodes the first full model record into to.
func (s *SQL) First(query string, to any, args ...any) error {
	stmt, reader, err := s.parseSelect(query, args)
	if err != nil {
		return err
	}
	if !selectHasOnlyStar(stmt) {
		return fmt.Errorf("%w: use Project for selected columns", ErrUnsupportedSQL)
	}
	if err := s.registerSelectTarget(stmt, to); err != nil {
		return err
	}
	plan, err := s.buildSelectPlan(stmt, reader)
	if err != nil {
		return err
	}
	if err := reader.done(); err != nil {
		return err
	}
	qr := s.queryFromPlan(plan)
	return qr.First(to)
}

// Project executes a SELECT query and fills either []map[string]any or a DTO slice.
func (s *SQL) Project(query string, to any, args ...any) error {
	stmt, reader, err := s.parseSelect(query, args)
	if err != nil {
		return err
	}
	if selectIsCount(stmt) {
		return fmt.Errorf("%w: use Count for count(*)", ErrUnsupportedSQL)
	}
	plan, err := s.buildSelectPlan(stmt, reader)
	if err != nil {
		return err
	}
	projections, err := s.buildProjections(stmt.SelectExprs, plan.model, plan.table)
	if err != nil {
		return err
	}
	if err := reader.done(); err != nil {
		return err
	}
	if plan.model.metadataOnly() {
		records, err := s.selectRawRecords(plan)
		if err != nil {
			return err
		}
		return fillRawProjection(to, records, projections, plan.model)
	}
	records, err := s.selectRecords(plan)
	if err != nil {
		return err
	}
	return fillProjection(to, records, projections)
}

// Count executes a SELECT COUNT(*) query and returns the matched record count.
func (s *SQL) Count(query string, args ...any) (int, error) {
	stmt, reader, err := s.parseSelect(query, args)
	if err != nil {
		return 0, err
	}
	if !selectIsCount(stmt) {
		return 0, fmt.Errorf("%w: Count requires select count(*)", ErrUnsupportedSQL)
	}
	plan, err := s.buildSelectPlan(stmt, reader)
	if err != nil {
		return 0, err
	}
	if err := reader.done(); err != nil {
		return 0, err
	}
	if plan.model.metadataOnly() {
		return s.countRawRecords(plan)
	}
	return s.queryFromPlan(plan).Count(reflect.New(plan.model.typ).Interface())
}

// Exec executes INSERT, UPDATE, or DELETE and returns affected row metadata.
func (s *SQL) Exec(query string, args ...any) (SQLResult, error) {
	stmt, err := sqlparser.Parse(query)
	if err != nil {
		return SQLResult{}, err
	}
	reader := &sqlArgReader{args: args}
	var result SQLResult
	switch stmt := stmt.(type) {
	case *sqlparser.Insert:
		result, err = s.execInsert(stmt, reader)
	case *sqlparser.Update:
		result, err = s.execUpdate(stmt, reader)
	case *sqlparser.Delete:
		result, err = s.execDelete(stmt, reader)
	default:
		err = fmt.Errorf("%w: %T", ErrUnsupportedSQL, stmt)
	}
	if err != nil {
		return SQLResult{}, err
	}
	if err := reader.done(); err != nil {
		return SQLResult{}, err
	}
	return result, nil
}

func newSQLModel(model any) (*sqlModel, error) {
	ref := reflect.ValueOf(model)
	if !ref.IsValid() {
		return nil, ErrBadType
	}
	if ref.Kind() == reflect.Ptr {
		if ref.Type().Elem().Kind() != reflect.Struct {
			return nil, ErrBadType
		}
		if ref.IsNil() {
			ref = reflect.New(ref.Type().Elem()).Elem()
		} else {
			ref = reflect.Indirect(ref)
		}
	}
	if ref.Kind() != reflect.Struct {
		return nil, ErrBadType
	}
	cfg, err := extract(&ref)
	if err != nil {
		return nil, err
	}
	info := &sqlModel{
		table:      cfg.Name,
		typ:        cfg.Type,
		cfg:        cfg,
		fields:     make(map[string]*fieldConfig),
		fieldTypes: make(map[string]reflect.Type),
	}
	for _, field := range cfg.Fields {
		info.addFieldName(field.Name, field)
		info.addFieldName(field.JsonFieldName, field)
		if typ := info.structFieldType(field.Name); typ != nil {
			info.fieldTypes[field.Name] = typ
		}
	}
	return info, nil
}

func newSQLModelFromSchema(schema *storedSchema) *sqlModel {
	cfg := structConfigFromStoredSchema(schema)
	info := &sqlModel{
		table:      schema.Table,
		typ:        cfg.Type,
		cfg:        cfg,
		fields:     make(map[string]*fieldConfig),
		fieldTypes: make(map[string]reflect.Type),
		metadata:   true,
	}
	for _, stored := range schema.Fields {
		field := cfg.Fields[stored.Name]
		if field == nil {
			continue
		}
		info.addFieldName(field.Name, field)
		info.addFieldName(field.JsonFieldName, field)
		if typ := storedFieldType(stored.Type); typ != nil {
			info.fieldTypes[field.Name] = typ
		}
	}
	return info
}

func dynamicStructType(schema *storedSchema) reflect.Type {
	fields := make([]reflect.StructField, 0, len(schema.Fields))
	for _, field := range schema.Fields {
		typ := storedFieldType(field.Type)
		if typ == nil {
			typ = reflect.TypeOf((*any)(nil)).Elem()
		}
		jsonName := field.JSON
		if jsonName == "" {
			jsonName = field.Name
		}
		fields = append(fields, reflect.StructField{
			Name: field.Name,
			Type: typ,
			Tag:  reflect.StructTag(fmt.Sprintf(`json:"%s"`, jsonName)),
		})
	}
	return reflect.StructOf(fields)
}

func (m *sqlModel) addFieldName(name string, field *fieldConfig) {
	if name == "" || name == "-" {
		return
	}
	m.fields[strings.ToLower(name)] = field
}

func (m *sqlModel) lookupField(name string) (*fieldConfig, bool) {
	field, ok := m.fields[strings.ToLower(name)]
	return field, ok
}

func (m *sqlModel) fieldType(field *fieldConfig) reflect.Type {
	if typ := m.structFieldType(field.Name); typ != nil {
		return typ
	}
	if field.Value != nil {
		return field.Value.Type()
	}
	if typ := m.fieldTypes[field.Name]; typ != nil {
		return typ
	}
	return nil
}

func (m *sqlModel) metadataOnly() bool {
	return m == nil || m.metadata
}

func (m *sqlModel) queryFieldName(field *fieldConfig) string {
	if m.metadataOnly() {
		return field.JsonFieldName
	}
	return field.Name
}

func (m *sqlModel) structFieldType(fieldName string) reflect.Type {
	if m.typ == nil {
		return nil
	}
	if sf, ok := m.typ.FieldByName(fieldName); ok {
		return sf.Type
	}
	return nil
}

// configForValue binds metadata field definitions to one concrete record value.
func (m *sqlModel) configForValue(record reflect.Value) (*structConfig, error) {
	if record.Kind() == reflect.Ptr {
		record = record.Elem()
	}
	cfg := &structConfig{
		Name:   m.cfg.Name,
		Type:   record.Type(),
		Fields: make(map[string]*fieldConfig, len(m.cfg.Fields)),
	}
	for name, base := range m.cfg.Fields {
		fieldValue := record.FieldByName(name)
		field := &fieldConfig{
			Name:           base.Name,
			Index:          base.Index,
			IsID:           base.IsID,
			Increment:      base.Increment,
			IncrementStart: base.IncrementStart,
			IsInteger:      base.IsInteger,
			JsonFieldName:  base.JsonFieldName,
			Composites:     base.Composites,
		}
		if fieldValue.IsValid() {
			field.Value = &fieldValue
			field.IsZero = isZero(&fieldValue)
		}
		cfg.Fields[name] = field
		if field.IsID || (m.cfg.ID != nil && m.cfg.ID.Name == field.Name) {
			cfg.ID = field
		}
	}
	if err := validateCompositeIndexes(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (s *SQL) parseSelect(query string, args []any) (*sqlparser.Select, *sqlArgReader, error) {
	stmt, err := sqlparser.Parse(query)
	if err != nil {
		return nil, nil, err
	}
	selectStmt, ok := stmt.(*sqlparser.Select)
	if !ok {
		return nil, nil, fmt.Errorf("%w: expected select", ErrUnsupportedSQL)
	}
	return selectStmt, &sqlArgReader{args: args}, nil
}

func (s *SQL) buildSelectPlan(stmt *sqlparser.Select, reader *sqlArgReader) (*sqlSelectPlan, error) {
	if stmt.Distinct != "" || stmt.Having != nil || len(stmt.GroupBy) > 0 || stmt.Lock != "" {
		return nil, fmt.Errorf("%w: select option", ErrUnsupportedSQL)
	}
	model, table, err := s.singleTable(stmt.From)
	if err != nil {
		return nil, err
	}
	matcher, err := s.matcherFromWhere(stmt.Where, model, table, reader)
	if err != nil {
		return nil, err
	}
	orderBy, reverse, err := s.orderBy(stmt.OrderBy, model, table)
	if err != nil {
		return nil, err
	}
	limit, skip, err := s.limit(stmt.Limit, reader)
	if err != nil {
		return nil, err
	}
	return &sqlSelectPlan{
		model:      model,
		table:      table,
		matcher:    matcher,
		whereEmpty: stmt.Where == nil,
		orderBy:    orderBy,
		reverse:    reverse,
		limit:      limit,
		skip:       skip,
	}, nil
}

func (s *SQL) queryFromPlan(plan *sqlSelectPlan) Query {
	qr := s.node.Select(plan.matcher)
	if len(plan.orderBy) > 0 {
		qr = qr.OrderBy(plan.orderBy...)
	}
	if plan.reverse {
		qr = qr.Reverse()
	}
	if plan.limit >= 0 {
		qr = qr.Limit(plan.limit)
	}
	if plan.skip > 0 {
		qr = qr.Skip(plan.skip)
	}
	return qr
}

func (s *SQL) selectRecords(plan *sqlSelectPlan) (reflect.Value, error) {
	sliceType := reflect.SliceOf(reflect.PtrTo(plan.model.typ))
	slicePtr := reflect.New(sliceType)
	err := s.queryFromPlan(plan).Find(slicePtr.Interface())
	if err == ErrNotFound {
		return slicePtr.Elem(), nil
	}
	if err != nil {
		return reflect.Value{}, err
	}
	return slicePtr.Elem(), nil
}

func (s *SQL) selectRawRecords(plan *sqlSelectPlan) ([]sqlRawRecord, error) {
	if records, used, err := s.selectRawRecordsByIDCursor(plan); used || err != nil {
		return records, err
	}

	var records []sqlRawRecord
	err := s.node.readTx(func(tx *bolt.Tx) error {
		bucket := s.node.GetBucket(tx, plan.model.table)
		if bucket == nil {
			return nil
		}
		cursor := bucket.Cursor()
		for key, raw := cursor.First(); key != nil; key, raw = cursor.Next() {
			if raw == nil || bytes.Equal(key, []byte(metadataBucket)) {
				continue
			}
			ok, err := plan.matcher.Match(raw)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			records = append(records, sqlRawRecord{raw: append([]byte(nil), raw...)})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortRawRecords(records, plan.orderBy, plan.reverse)
	return sliceRawRecords(records, plan.skip, plan.limit), nil
}

// selectRawRecordsByIDCursor handles the narrow metadata-only case where SQL
// order is exactly the Bolt primary-key order, so LIMIT/OFFSET can stop early.
func (s *SQL) selectRawRecordsByIDCursor(plan *sqlSelectPlan) ([]sqlRawRecord, bool, error) {
	if !plan.canUseRawIDCursorOrder() {
		return nil, false, nil
	}
	if plan.limit == 0 {
		return nil, true, nil
	}

	var records []sqlRawRecord
	err := s.node.readTx(func(tx *bolt.Tx) error {
		bucket := s.node.GetBucket(tx, plan.model.table)
		if bucket == nil {
			return nil
		}

		cursor := bucket.Cursor()
		key, raw := cursor.First()
		if plan.reverse {
			key, raw = cursor.Last()
		}

		skipped := 0
		for key != nil {
			if raw != nil && !bytes.Equal(key, []byte(metadataBucket)) {
				if skipped < plan.skip {
					skipped++
				} else {
					records = append(records, sqlRawRecord{raw: append([]byte(nil), raw...)})
					if len(records) >= plan.limit {
						return nil
					}
				}
			}

			if plan.reverse {
				key, raw = cursor.Prev()
			} else {
				key, raw = cursor.Next()
			}
		}
		return nil
	})
	return records, true, err
}

// canUseRawIDCursorOrder is intentionally strict: it only allows SQL shapes
// where reading the main bucket cursor is equivalent to ORDER BY id.
func (plan *sqlSelectPlan) canUseRawIDCursorOrder() bool {
	if plan == nil || plan.model == nil || !plan.model.metadataOnly() {
		return false
	}
	if !plan.whereEmpty || plan.limit < 0 || len(plan.orderBy) != 1 {
		return false
	}

	idField := plan.model.cfg.ID
	if idField == nil || plan.orderBy[0] != plan.model.queryFieldName(idField) {
		return false
	}
	return plan.model.idFieldUsesBoltKeyOrder(idField)
}

// idFieldUsesBoltKeyOrder guards against changing SQL sort semantics for ID
// types whose JSON value order does not match their stored Bolt key order.
func (m *sqlModel) idFieldUsesBoltKeyOrder(field *fieldConfig) bool {
	typ := m.fieldType(field)
	if typ == nil {
		return false
	}

	switch typ.Kind() {
	case reflect.String:
		return true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return field.IsInteger && field.Increment && field.IncrementStart >= 0
	default:
		return false
	}
}

func (s *SQL) countRawRecords(plan *sqlSelectPlan) (int, error) {
	records, err := s.selectRawRecords(plan)
	if err != nil {
		return 0, err
	}
	return len(records), nil
}

func sortRawRecords(records []sqlRawRecord, orderBy []string, reverse bool) {
	if len(orderBy) == 0 {
		return
	}
	sort.Slice(records, func(i, j int) bool {
		for _, field := range orderBy {
			cmp := compareRawJSON(gjson.GetBytes(records[i].raw, field), gjson.GetBytes(records[j].raw, field))
			if cmp == 0 {
				continue
			}
			if reverse {
				return cmp > 0
			}
			return cmp < 0
		}
		return false
	})
}

func sliceRawRecords(records []sqlRawRecord, skip, limit int) []sqlRawRecord {
	if skip < 0 {
		skip = 0
	}
	if skip >= len(records) {
		return records[:0]
	}
	records = records[skip:]
	if limit >= 0 && limit < len(records) {
		return records[:limit]
	}
	return records
}

func compareRawJSON(left, right gjson.Result) int {
	if !left.Exists() || !right.Exists() {
		if left.Exists() {
			return 1
		}
		if right.Exists() {
			return -1
		}
		return 0
	}
	if left.Type == gjson.Number && right.Type == gjson.Number {
		return compareFloat(left.Num, right.Num)
	}
	if left.Type == gjson.True || left.Type == gjson.False || right.Type == gjson.True || right.Type == gjson.False {
		return compareBool(left.Bool(), right.Bool())
	}
	return strings.Compare(left.String(), right.String())
}

func compareFloat(left, right float64) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

func compareBool(left, right bool) int {
	switch {
	case !left && right:
		return -1
	case left && !right:
		return 1
	default:
		return 0
	}
}

func (s *SQL) registerSelectTarget(stmt *sqlparser.Select, to any) error {
	table, err := tableRefFromExprs(stmt.From)
	if err != nil {
		return err
	}
	if model, ok := s.models[table.name]; ok && !model.metadataOnly() {
		return nil
	}
	model, err := newSQLModelFromTarget(to)
	if err != nil {
		return err
	}
	if model.table != table.name {
		return nil
	}
	s.models[model.table] = model
	return nil
}

func newSQLModelFromTarget(to any) (*sqlModel, error) {
	ref := reflect.ValueOf(to)
	if !ref.IsValid() || ref.Kind() != reflect.Ptr {
		return nil, ErrPtrNeeded
	}
	elem := ref.Elem()
	switch elem.Kind() {
	case reflect.Slice:
		elemType := elem.Type().Elem()
		if elemType.Kind() == reflect.Ptr {
			elemType = elemType.Elem()
		}
		if elemType.Kind() != reflect.Struct {
			return nil, ErrSlicePtrNeeded
		}
		return newSQLModel(reflect.New(elemType).Interface())
	case reflect.Struct:
		return newSQLModel(to)
	default:
		return nil, ErrPtrNeeded
	}
}

func (s *SQL) singleTable(exprs sqlparser.TableExprs) (*sqlModel, sqlTableRef, error) {
	table, err := tableRefFromExprs(exprs)
	if err != nil {
		return nil, sqlTableRef{}, err
	}
	model, err := s.modelForTable(table.name)
	if err != nil {
		return nil, sqlTableRef{}, err
	}
	return model, table, nil
}

func (s *SQL) modelByTableName(tableName sqlparser.TableName) (*sqlModel, sqlTableRef, error) {
	if tableName.IsEmpty() || !tableName.Qualifier.IsEmpty() {
		return nil, sqlTableRef{}, fmt.Errorf("%w: table expression", ErrUnsupportedSQL)
	}
	table := sqlTableRef{name: tableName.Name.String()}
	model, err := s.modelForTable(table.name)
	if err != nil {
		return nil, sqlTableRef{}, err
	}
	return model, table, nil
}

func tableRefFromExprs(exprs sqlparser.TableExprs) (sqlTableRef, error) {
	if len(exprs) != 1 {
		return sqlTableRef{}, fmt.Errorf("%w: only single table is supported", ErrUnsupportedSQL)
	}
	aliased, ok := exprs[0].(*sqlparser.AliasedTableExpr)
	if !ok || aliased.Hints != nil || len(aliased.Partitions) > 0 {
		return sqlTableRef{}, fmt.Errorf("%w: table expression", ErrUnsupportedSQL)
	}
	tableName, ok := aliased.Expr.(sqlparser.TableName)
	if !ok || !tableName.Qualifier.IsEmpty() {
		return sqlTableRef{}, fmt.Errorf("%w: table expression", ErrUnsupportedSQL)
	}
	return sqlTableRef{name: tableName.Name.String(), alias: aliased.As.String()}, nil
}

func (s *SQL) modelForTable(tableName string) (*sqlModel, error) {
	if s.models == nil {
		s.models = make(map[string]*sqlModel)
	}
	if model, ok := s.models[tableName]; ok {
		return model, nil
	}
	model, err := s.loadModelFromMetadata(tableName)
	if err != nil {
		if err == ErrNotFound {
			return nil, fmt.Errorf("%w: %s", ErrSQLTableNotRegistered, tableName)
		}
		return nil, err
	}
	s.models[tableName] = model
	return model, nil
}

func (s *SQL) loadModelFromMetadata(tableName string) (*sqlModel, error) {
	var schema *storedSchema
	err := s.node.readTx(func(tx *bolt.Tx) error {
		bucket := s.node.GetBucket(tx, tableName)
		var err error
		schema, err = readStoredSchema(bucket)
		return err
	})
	if err != nil {
		return nil, err
	}
	return newSQLModelFromSchema(schema), nil
}

func (s *SQL) matcherFromWhere(where *sqlparser.Where, model *sqlModel, table sqlTableRef, reader *sqlArgReader) (q.Matcher, error) {
	if where == nil {
		return q.True(), nil
	}
	return s.matcherFromExpr(where.Expr, model, table, reader)
}

// matcherFromExpr recursively converts the supported WHERE AST into q matchers.
func (s *SQL) matcherFromExpr(expr sqlparser.Expr, model *sqlModel, table sqlTableRef, reader *sqlArgReader) (q.Matcher, error) {
	switch node := expr.(type) {
	case *sqlparser.AndExpr:
		left, err := s.matcherFromExpr(node.Left, model, table, reader)
		if err != nil {
			return nil, err
		}
		right, err := s.matcherFromExpr(node.Right, model, table, reader)
		if err != nil {
			return nil, err
		}
		return q.And(left, right), nil
	case *sqlparser.OrExpr:
		left, err := s.matcherFromExpr(node.Left, model, table, reader)
		if err != nil {
			return nil, err
		}
		right, err := s.matcherFromExpr(node.Right, model, table, reader)
		if err != nil {
			return nil, err
		}
		return q.Or(left, right), nil
	case *sqlparser.NotExpr:
		child, err := s.matcherFromExpr(node.Expr, model, table, reader)
		if err != nil {
			return nil, err
		}
		return q.Not(child), nil
	case *sqlparser.ParenExpr:
		return s.matcherFromExpr(node.Expr, model, table, reader)
	case *sqlparser.ComparisonExpr:
		return s.matcherFromComparison(node, model, table, reader)
	default:
		return nil, fmt.Errorf("%w: where expression %s", ErrUnsupportedSQL, sqlparser.String(expr))
	}
}

// matcherFromComparison binds one SQL column comparison to the existing matcher set.
func (s *SQL) matcherFromComparison(expr *sqlparser.ComparisonExpr, model *sqlModel, table sqlTableRef, reader *sqlArgReader) (q.Matcher, error) {
	field, err := s.fieldFromExpr(expr.Left, model, table)
	if err != nil {
		return nil, err
	}
	fieldType := model.fieldType(field)
	switch expr.Operator {
	case sqlparser.EqualStr:
		value, err := s.valueFromExpr(expr.Right, fieldType, reader)
		if err != nil {
			return nil, err
		}
		return q.Eq(model.queryFieldName(field), value), nil
	case sqlparser.NotEqualStr:
		value, err := s.valueFromExpr(expr.Right, fieldType, reader)
		if err != nil {
			return nil, err
		}
		return q.Not(q.Eq(model.queryFieldName(field), value)), nil
	case sqlparser.GreaterThanStr:
		value, err := s.valueFromExpr(expr.Right, fieldType, reader)
		if err != nil {
			return nil, err
		}
		return q.Gt(model.queryFieldName(field), value), nil
	case sqlparser.GreaterEqualStr:
		value, err := s.valueFromExpr(expr.Right, fieldType, reader)
		if err != nil {
			return nil, err
		}
		return q.Gte(model.queryFieldName(field), value), nil
	case sqlparser.LessThanStr:
		value, err := s.valueFromExpr(expr.Right, fieldType, reader)
		if err != nil {
			return nil, err
		}
		return q.Lt(model.queryFieldName(field), value), nil
	case sqlparser.LessEqualStr:
		value, err := s.valueFromExpr(expr.Right, fieldType, reader)
		if err != nil {
			return nil, err
		}
		return q.Lte(model.queryFieldName(field), value), nil
	case sqlparser.InStr, sqlparser.NotInStr:
		values, err := s.valuesFromTuple(expr.Right, fieldType, reader)
		if err != nil {
			return nil, err
		}
		matcher := q.In(model.queryFieldName(field), values)
		if expr.Operator == sqlparser.NotInStr {
			return q.Not(matcher), nil
		}
		return matcher, nil
	default:
		return nil, fmt.Errorf("%w: comparison %s", ErrUnsupportedSQL, expr.Operator)
	}
}

func (s *SQL) fieldFromExpr(expr sqlparser.Expr, model *sqlModel, table sqlTableRef) (*fieldConfig, error) {
	col, ok := expr.(*sqlparser.ColName)
	if !ok {
		return nil, fmt.Errorf("%w: column expression", ErrUnsupportedSQL)
	}
	return s.fieldFromCol(col, model, table)
}

func (s *SQL) fieldFromCol(col *sqlparser.ColName, model *sqlModel, table sqlTableRef) (*fieldConfig, error) {
	if !col.Qualifier.IsEmpty() {
		if !col.Qualifier.Qualifier.IsEmpty() {
			return nil, fmt.Errorf("%w: qualified column", ErrUnsupportedSQL)
		}
		qualifier := col.Qualifier.Name.String()
		if qualifier != table.name && qualifier != table.alias {
			return nil, fmt.Errorf("%w: %s", ErrSQLUnknownField, sqlparser.String(col))
		}
	}
	field, ok := model.lookupField(col.Name.String())
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrSQLUnknownField, col.Name.String())
	}
	return field, nil
}

// valueFromExpr normalizes SQL literals and ? placeholders into Go values.
func (s *SQL) valueFromExpr(expr sqlparser.Expr, targetType reflect.Type, reader *sqlArgReader) (any, error) {
	var value any
	switch node := expr.(type) {
	case *sqlparser.SQLVal:
		switch node.Type {
		case sqlparser.StrVal:
			value = string(node.Val)
		case sqlparser.IntVal:
			parsed, err := strconv.ParseInt(string(node.Val), 10, 64)
			if err != nil {
				return nil, err
			}
			value = parsed
		case sqlparser.FloatVal:
			parsed, err := strconv.ParseFloat(string(node.Val), 64)
			if err != nil {
				return nil, err
			}
			value = parsed
		case sqlparser.ValArg:
			arg, err := reader.next()
			if err != nil {
				return nil, err
			}
			value = arg
		default:
			return nil, fmt.Errorf("%w: sql value", ErrUnsupportedSQL)
		}
	case *sqlparser.NullVal:
		value = nil
	case sqlparser.BoolVal:
		value = bool(node)
	default:
		return nil, fmt.Errorf("%w: value expression %s", ErrUnsupportedSQL, sqlparser.String(expr))
	}
	if targetType == nil {
		return value, nil
	}
	return convertSQLValue(value, targetType)
}

func (s *SQL) valuesFromTuple(expr sqlparser.Expr, targetType reflect.Type, reader *sqlArgReader) (any, error) {
	tuple, ok := expr.(sqlparser.ValTuple)
	if !ok {
		return nil, fmt.Errorf("%w: in tuple", ErrUnsupportedSQL)
	}
	values := make([]any, 0, len(tuple))
	for _, child := range tuple {
		value, err := s.valueFromExpr(child, targetType, reader)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	if len(values) == 1 && isSliceValue(values[0]) {
		return values[0], nil
	}
	return values, nil
}

func (s *SQL) orderBy(orderBy sqlparser.OrderBy, model *sqlModel, table sqlTableRef) ([]string, bool, error) {
	if len(orderBy) == 0 {
		return nil, false, nil
	}
	fields := make([]string, 0, len(orderBy))
	reverse := false
	directionSet := false
	for _, order := range orderBy {
		field, err := s.fieldFromExpr(order.Expr, model, table)
		if err != nil {
			return nil, false, err
		}
		direction := strings.ToLower(order.Direction)
		desc := direction == sqlparser.DescScr
		if direction != "" && direction != sqlparser.AscScr && direction != sqlparser.DescScr {
			return nil, false, fmt.Errorf("%w: order direction", ErrUnsupportedSQL)
		}
		if directionSet && reverse != desc {
			return nil, false, fmt.Errorf("%w: mixed order directions", ErrUnsupportedSQL)
		}
		directionSet = true
		reverse = desc
		fields = append(fields, model.queryFieldName(field))
	}
	return fields, reverse, nil
}

func (s *SQL) limit(limit *sqlparser.Limit, reader *sqlArgReader) (int, int, error) {
	if limit == nil {
		return -1, 0, nil
	}
	rowCount, err := s.intFromExpr(limit.Rowcount, reader)
	if err != nil {
		return 0, 0, err
	}
	offset := 0
	if limit.Offset != nil {
		offset, err = s.intFromExpr(limit.Offset, reader)
		if err != nil {
			return 0, 0, err
		}
	}
	if rowCount < 0 || offset < 0 {
		return 0, 0, fmt.Errorf("%w: negative limit", ErrUnsupportedSQL)
	}
	return rowCount, offset, nil
}

func (s *SQL) intFromExpr(expr sqlparser.Expr, reader *sqlArgReader) (int, error) {
	value, err := s.valueFromExpr(expr, nil, reader)
	if err != nil {
		return 0, err
	}
	i, err := anyToInt(value)
	if err != nil {
		return 0, err
	}
	return i, nil
}

// buildProjections validates SELECT columns and records their output names.
func (s *SQL) buildProjections(exprs sqlparser.SelectExprs, model *sqlModel, table sqlTableRef) ([]sqlProjection, error) {
	if len(exprs) == 0 {
		return nil, fmt.Errorf("%w: select list", ErrUnsupportedSQL)
	}
	var projections []sqlProjection
	for _, expr := range exprs {
		switch node := expr.(type) {
		case *sqlparser.StarExpr:
			if !node.TableName.IsEmpty() {
				if node.TableName.Name.String() != table.name && node.TableName.Name.String() != table.alias {
					return nil, fmt.Errorf("%w: %s", ErrSQLUnknownField, sqlparser.String(node))
				}
			}
			fields := make([]*fieldConfig, 0, len(model.cfg.Fields))
			for _, field := range model.cfg.Fields {
				fields = append(fields, field)
			}
			sort.Slice(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })
			for _, field := range fields {
				projections = append(projections, sqlProjection{field: field, name: field.JsonFieldName})
			}
		case *sqlparser.AliasedExpr:
			col, ok := node.Expr.(*sqlparser.ColName)
			if !ok {
				return nil, fmt.Errorf("%w: projection expression %s", ErrUnsupportedSQL, sqlparser.String(node.Expr))
			}
			field, err := s.fieldFromCol(col, model, table)
			if err != nil {
				return nil, err
			}
			name := col.Name.String()
			if !node.As.IsEmpty() {
				name = node.As.String()
			}
			projections = append(projections, sqlProjection{field: field, name: name})
		default:
			return nil, fmt.Errorf("%w: projection", ErrUnsupportedSQL)
		}
	}
	return projections, nil
}

// execInsert builds typed model values from INSERT VALUES and delegates persistence to SaveAll.
func (s *SQL) execInsert(stmt *sqlparser.Insert, reader *sqlArgReader) (SQLResult, error) {
	if stmt.Action != sqlparser.InsertStr || stmt.Ignore != "" || len(stmt.Partitions) > 0 || len(stmt.OnDup) > 0 {
		return SQLResult{}, fmt.Errorf("%w: insert option", ErrUnsupportedSQL)
	}
	model, table, err := s.modelByTableName(stmt.Table)
	if err != nil {
		return SQLResult{}, err
	}
	if len(stmt.Columns) == 0 {
		return SQLResult{}, fmt.Errorf("%w: insert columns required", ErrUnsupportedSQL)
	}
	rows, ok := stmt.Rows.(sqlparser.Values)
	if !ok {
		return SQLResult{}, fmt.Errorf("%w: insert rows", ErrUnsupportedSQL)
	}
	if model.metadataOnly() {
		return s.execMetadataInsert(model, table, stmt.Columns, rows, reader)
	}
	sliceType := reflect.SliceOf(reflect.PtrTo(model.typ))
	records := reflect.MakeSlice(sliceType, 0, len(rows))
	for _, row := range rows {
		if len(row) != len(stmt.Columns) {
			return SQLResult{}, fmt.Errorf("%w: insert value count", ErrUnsupportedSQL)
		}
		record := reflect.New(model.typ)
		for i, column := range stmt.Columns {
			field, err := s.fieldFromCol(&sqlparser.ColName{Name: column}, model, table)
			if err != nil {
				return SQLResult{}, err
			}
			value, err := s.valueFromExpr(row[i], model.fieldType(field), reader)
			if err != nil {
				return SQLResult{}, err
			}
			if err := setStructField(record.Elem(), field.Name, value); err != nil {
				return SQLResult{}, err
			}
		}
		records = reflect.Append(records, record)
	}
	if records.Len() == 0 {
		return SQLResult{}, nil
	}
	if err := s.node.SaveAll(records.Interface()); err != nil {
		return SQLResult{}, err
	}
	last := records.Index(records.Len() - 1).Elem().FieldByName(model.cfg.ID.Name).Interface()
	return SQLResult{RowsAffected: records.Len(), LastInsertID: last}, nil
}

func (s *SQL) execMetadataInsert(model *sqlModel, table sqlTableRef, columns sqlparser.Columns, rows sqlparser.Values, reader *sqlArgReader) (SQLResult, error) {
	records := make([]reflect.Value, 0, len(rows))
	for _, row := range rows {
		if len(row) != len(columns) {
			return SQLResult{}, fmt.Errorf("%w: insert value count", ErrUnsupportedSQL)
		}
		record := reflect.New(model.typ)
		for i, column := range columns {
			field, err := s.fieldFromCol(&sqlparser.ColName{Name: column}, model, table)
			if err != nil {
				return SQLResult{}, err
			}
			value, err := s.valueFromExpr(row[i], model.fieldType(field), reader)
			if err != nil {
				return SQLResult{}, err
			}
			if err := setStructField(record.Elem(), field.Name, value); err != nil {
				return SQLResult{}, err
			}
		}
		records = append(records, record)
	}
	if len(records) == 0 {
		return SQLResult{}, nil
	}

	var saved []*savedRecord
	err := s.node.readWriteTx(func(tx *bolt.Tx) error {
		uniqueState := newSaveAllUniqueState(s.node)
		for _, record := range records {
			cfg, err := model.configForValue(record)
			if err != nil {
				return err
			}
			if err := validateSaveConfig(cfg); err != nil {
				return err
			}
			savedRecord, err := s.node.save(tx, cfg, record.Interface(), false, uniqueState)
			if err != nil {
				return err
			}
			if s.node.tx != nil {
				s.node.markTxIndexRecord(savedRecord)
			} else {
				saved = append(saved, savedRecord)
			}
		}
		return nil
	})
	if err != nil {
		return SQLResult{}, err
	}
	if s.node.tx == nil {
		if err := s.node.indexSavedRecords(saved); err != nil {
			return SQLResult{}, err
		}
	}

	last := records[len(records)-1].Elem().FieldByName(model.cfg.ID.Name).Interface()
	return SQLResult{RowsAffected: len(records), LastInsertID: last}, nil
}

// execUpdate validates SET clauses before scanning and updating matching records.
func (s *SQL) execUpdate(stmt *sqlparser.Update, reader *sqlArgReader) (SQLResult, error) {
	if len(stmt.OrderBy) > 0 || stmt.Limit != nil || len(stmt.Exprs) == 0 {
		return SQLResult{}, fmt.Errorf("%w: update option", ErrUnsupportedSQL)
	}
	model, table, err := s.singleTable(stmt.TableExprs)
	if err != nil {
		return SQLResult{}, err
	}
	assignments := make([]sqlAssignment, 0, len(stmt.Exprs))
	for _, expr := range stmt.Exprs {
		field, err := s.fieldFromCol(expr.Name, model, table)
		if err != nil {
			return SQLResult{}, err
		}
		if field.IsID {
			return SQLResult{}, fmt.Errorf("%w: updating id", ErrUnsupportedSQL)
		}
		value, err := s.valueFromExpr(expr.Expr, model.fieldType(field), reader)
		if err != nil {
			return SQLResult{}, err
		}
		assignments = append(assignments, sqlAssignment{field: field, value: value})
	}
	if stmt.Where == nil && !s.allowFullTableWrite {
		return SQLResult{}, ErrSQLUnsafeWrite
	}
	matcher, err := s.matcherFromWhere(stmt.Where, model, table, reader)
	if err != nil {
		return SQLResult{}, err
	}
	if model.metadataOnly() {
		return s.updateMetadataRecords(model, matcher, assignments)
	}
	return s.updateRecords(model, matcher, assignments)
}

// updateRecords keeps one SQL UPDATE in one Bolt write transaction and batches index updates.
func (s *SQL) updateRecords(model *sqlModel, matcher q.Matcher, assignments []sqlAssignment) (SQLResult, error) {
	affected := 0
	var saved []*savedRecord
	err := s.node.readWriteTx(func(tx *bolt.Tx) error {
		sliceType := reflect.SliceOf(reflect.PtrTo(model.typ))
		slicePtr := reflect.New(sliceType)
		sink, err := newListSink(s.node, slicePtr.Interface())
		if err != nil {
			return err
		}
		if err := newQuery(s.node, matcher).query(tx, sink); err != nil {
			if err == ErrNotFound {
				return nil
			}
			return err
		}
		records := slicePtr.Elem()
		for i := 0; i < records.Len(); i++ {
			record := records.Index(i)
			elem := record.Elem()
			for _, assignment := range assignments {
				if err := setStructField(elem, assignment.field.Name, assignment.value); err != nil {
					return err
				}
			}
			cfg, err := saveConfig(record.Interface())
			if err != nil {
				return err
			}
			for _, assignment := range assignments {
				if field := cfg.Fields[assignment.field.Name]; field != nil {
					field.ForceUpdate = true
				}
			}
			savedRecord, err := s.node.save(tx, cfg, record.Interface(), true, nil)
			if err != nil {
				return err
			}
			affected++
			if s.node.tx != nil {
				s.node.markTxIndexRecord(savedRecord)
			} else {
				saved = append(saved, savedRecord)
			}
		}
		return nil
	})
	if err != nil {
		return SQLResult{}, err
	}
	if s.node.tx == nil {
		if err := s.node.indexSavedRecords(saved); err != nil {
			return SQLResult{}, err
		}
	}
	return SQLResult{RowsAffected: affected}, nil
}

func (s *SQL) updateMetadataRecords(model *sqlModel, matcher q.Matcher, assignments []sqlAssignment) (SQLResult, error) {
	affected := 0
	var saved []*savedRecord
	err := s.node.readWriteTx(func(tx *bolt.Tx) error {
		bucket := s.node.GetBucket(tx, model.table)
		if bucket == nil {
			return nil
		}
		var raws [][]byte
		cursor := bucket.Cursor()
		for key, raw := cursor.First(); key != nil; key, raw = cursor.Next() {
			if raw == nil || bytes.Equal(key, []byte(metadataBucket)) {
				continue
			}
			ok, err := matcher.Match(raw)
			if err != nil {
				return err
			}
			if ok {
				raws = append(raws, append([]byte(nil), raw...))
			}
		}
		for _, raw := range raws {
			record := reflect.New(model.typ)
			if err := s.node.codec.Unmarshal(raw, record.Interface()); err != nil {
				return err
			}
			for _, assignment := range assignments {
				if err := setStructField(record.Elem(), assignment.field.Name, assignment.value); err != nil {
					return err
				}
			}
			cfg, err := model.configForValue(record)
			if err != nil {
				return err
			}
			for _, assignment := range assignments {
				if field := cfg.Fields[assignment.field.Name]; field != nil {
					field.ForceUpdate = true
				}
			}
			savedRecord, err := s.node.save(tx, cfg, record.Interface(), true, nil)
			if err != nil {
				return err
			}
			affected++
			if s.node.tx != nil {
				s.node.markTxIndexRecord(savedRecord)
			} else {
				saved = append(saved, savedRecord)
			}
		}
		return nil
	})
	if err != nil {
		return SQLResult{}, err
	}
	if s.node.tx == nil {
		if err := s.node.indexSavedRecords(saved); err != nil {
			return SQLResult{}, err
		}
	}
	return SQLResult{RowsAffected: affected}, nil
}

// execDelete runs a matcher-backed delete while preserving the no-WHERE safety default.
func (s *SQL) execDelete(stmt *sqlparser.Delete, reader *sqlArgReader) (SQLResult, error) {
	if len(stmt.Targets) > 0 || len(stmt.Partitions) > 0 || len(stmt.OrderBy) > 0 || stmt.Limit != nil {
		return SQLResult{}, fmt.Errorf("%w: delete option", ErrUnsupportedSQL)
	}
	model, table, err := s.singleTable(stmt.TableExprs)
	if err != nil {
		return SQLResult{}, err
	}
	if stmt.Where == nil && !s.allowFullTableWrite {
		return SQLResult{}, ErrSQLUnsafeWrite
	}
	matcher, err := s.matcherFromWhere(stmt.Where, model, table, reader)
	if err != nil {
		return SQLResult{}, err
	}
	if model.metadataOnly() {
		return s.deleteMetadataRecords(model, matcher)
	}
	return s.deleteRecords(model, matcher)
}

func (s *SQL) deleteRecords(model *sqlModel, matcher q.Matcher) (SQLResult, error) {
	kind := reflect.New(model.typ).Interface()
	var sink *deleteSink
	err := s.node.readWriteTx(func(tx *bolt.Tx) error {
		var err error
		sink, err = newDeleteSink(s.node, kind)
		if err != nil {
			return err
		}
		if err := newQuery(s.node, matcher).query(tx, sink); err != nil {
			if err == ErrNotFound {
				return nil
			}
			return err
		}
		return nil
	})
	if err != nil {
		return SQLResult{}, err
	}
	if sink == nil {
		return SQLResult{}, nil
	}
	if s.node.tx == nil {
		if err := s.node.deleteIndexedRecords(sink.records); err != nil {
			return SQLResult{}, err
		}
	}
	return SQLResult{RowsAffected: sink.removed}, nil
}

func (s *SQL) deleteMetadataRecords(model *sqlModel, matcher q.Matcher) (SQLResult, error) {
	affected := 0
	var records []*deletedRecord
	err := s.node.readWriteTx(func(tx *bolt.Tx) error {
		bucket := s.node.GetBucket(tx, model.table)
		if bucket == nil {
			return nil
		}
		var keys [][]byte
		cursor := bucket.Cursor()
		for key, raw := cursor.First(); key != nil; key, raw = cursor.Next() {
			if raw == nil || bytes.Equal(key, []byte(metadataBucket)) {
				continue
			}
			ok, err := matcher.Match(raw)
			if err != nil {
				return err
			}
			if ok {
				keys = append(keys, append([]byte(nil), key...))
			}
		}
		for _, key := range keys {
			if err := bucket.Delete(key); err != nil {
				return err
			}
			record := &deletedRecord{
				cfg: model.cfg,
				id:  append([]byte(nil), key...),
			}
			if s.node.tx != nil {
				s.node.markTxIndexDelete(record)
			} else {
				records = append(records, record)
			}
			affected++
		}
		return nil
	})
	if err != nil {
		return SQLResult{}, err
	}
	if s.node.tx == nil {
		if err := s.node.deleteIndexedRecords(records); err != nil {
			return SQLResult{}, err
		}
	}
	return SQLResult{RowsAffected: affected}, nil
}

func (r *sqlArgReader) next() (any, error) {
	if r.pos >= len(r.args) {
		return nil, ErrSQLArguments
	}
	value := r.args[r.pos]
	r.pos++
	return value, nil
}

func (r *sqlArgReader) done() error {
	if r.pos != len(r.args) {
		return ErrSQLArguments
	}
	return nil
}

func selectHasOnlyStar(stmt *sqlparser.Select) bool {
	return len(stmt.SelectExprs) == 1 && isStarExpr(stmt.SelectExprs[0])
}

func isStarExpr(expr sqlparser.SelectExpr) bool {
	_, ok := expr.(*sqlparser.StarExpr)
	return ok
}

func selectIsCount(stmt *sqlparser.Select) bool {
	if len(stmt.SelectExprs) != 1 {
		return false
	}
	aliased, ok := stmt.SelectExprs[0].(*sqlparser.AliasedExpr)
	if !ok {
		return false
	}
	fn, ok := aliased.Expr.(*sqlparser.FuncExpr)
	if !ok || !fn.Name.EqualString("count") || fn.Distinct || len(fn.Exprs) != 1 {
		return false
	}
	return isStarExpr(fn.Exprs[0])
}

func isSliceValue(value any) bool {
	if value == nil {
		return false
	}
	kind := reflect.TypeOf(value).Kind()
	return kind == reflect.Slice || kind == reflect.Array
}

func anyToInt(value any) (int, error) {
	converted, err := convertSQLValue(value, reflect.TypeOf(int(0)))
	if err != nil {
		return 0, err
	}
	return converted.(int), nil
}

func storedFieldType(name string) reflect.Type {
	if strings.HasPrefix(name, "*") {
		elem := storedFieldType(strings.TrimPrefix(name, "*"))
		if elem == nil {
			return nil
		}
		return reflect.PointerTo(elem)
	}
	switch name {
	case "bool":
		return reflect.TypeOf(false)
	case "string":
		return reflect.TypeOf("")
	case "int":
		return reflect.TypeOf(int(0))
	case "int8":
		return reflect.TypeOf(int8(0))
	case "int16":
		return reflect.TypeOf(int16(0))
	case "int32":
		return reflect.TypeOf(int32(0))
	case "int64":
		return reflect.TypeOf(int64(0))
	case "uint":
		return reflect.TypeOf(uint(0))
	case "uint8":
		return reflect.TypeOf(uint8(0))
	case "uint16":
		return reflect.TypeOf(uint16(0))
	case "uint32":
		return reflect.TypeOf(uint32(0))
	case "uint64":
		return reflect.TypeOf(uint64(0))
	case "float32":
		return reflect.TypeOf(float32(0))
	case "float64":
		return reflect.TypeOf(float64(0))
	case "time.Time":
		return timeType
	case "[]uint8":
		return reflect.TypeOf([]byte(nil))
	default:
		return nil
	}
}

// convertSQLValue performs the narrow conversions needed by SQL literals and placeholders.
func convertSQLValue(value any, target reflect.Type) (any, error) {
	if target == nil {
		return value, nil
	}
	if value == nil {
		switch target.Kind() {
		case reflect.Ptr, reflect.Slice, reflect.Map, reflect.Interface:
			return reflect.Zero(target).Interface(), nil
		default:
			return nil, ErrIncompatibleValue
		}
	}
	if target.Kind() == reflect.Ptr {
		converted, err := convertSQLValue(value, target.Elem())
		if err != nil {
			return nil, err
		}
		ptr := reflect.New(target.Elem())
		ptr.Elem().Set(reflect.ValueOf(converted))
		return ptr.Interface(), nil
	}
	source := reflect.ValueOf(value)
	if source.IsValid() && source.Type().AssignableTo(target) {
		return value, nil
	}
	if source.IsValid() && source.Type().ConvertibleTo(target) {
		converted := source.Convert(target)
		return converted.Interface(), nil
	}
	if target == timeType {
		if text, ok := value.(string); ok {
			parsed, err := time.Parse(time.RFC3339, text)
			if err != nil {
				return nil, err
			}
			return parsed, nil
		}
	}
	switch target.Kind() {
	case reflect.String:
		if text, ok := value.(string); ok {
			return text, nil
		}
	case reflect.Bool:
		switch v := value.(type) {
		case string:
			parsed, err := strconv.ParseBool(v)
			if err != nil {
				return nil, err
			}
			return parsed, nil
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		i, ok := numericToInt64(value)
		if !ok {
			return nil, ErrIncompatibleValue
		}
		converted := reflect.New(target).Elem()
		if converted.OverflowInt(i) {
			return nil, ErrIncompatibleValue
		}
		converted.SetInt(i)
		return converted.Interface(), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		u, ok := numericToUint64(value)
		if !ok {
			return nil, ErrIncompatibleValue
		}
		converted := reflect.New(target).Elem()
		if converted.OverflowUint(u) {
			return nil, ErrIncompatibleValue
		}
		converted.SetUint(u)
		return converted.Interface(), nil
	case reflect.Float32, reflect.Float64:
		f, ok := numericToFloat64(value)
		if !ok {
			return nil, ErrIncompatibleValue
		}
		converted := reflect.New(target).Elem()
		if converted.OverflowFloat(f) {
			return nil, ErrIncompatibleValue
		}
		converted.SetFloat(f)
		return converted.Interface(), nil
	}
	return nil, ErrIncompatibleValue
}

func numericToInt64(value any) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int8:
		return int64(v), true
	case int16:
		return int64(v), true
	case int32:
		return int64(v), true
	case int64:
		return v, true
	case uint:
		if uint64(v) > uint64(^uint64(0)>>1) {
			return 0, false
		}
		return int64(v), true
	case uint8:
		return int64(v), true
	case uint16:
		return int64(v), true
	case uint32:
		return int64(v), true
	case uint64:
		if v > uint64(^uint64(0)>>1) {
			return 0, false
		}
		return int64(v), true
	case float32:
		return int64(v), true
	case float64:
		return int64(v), true
	case string:
		i, err := strconv.ParseInt(v, 10, 64)
		return i, err == nil
	default:
		return 0, false
	}
}

func numericToUint64(value any) (uint64, bool) {
	switch v := value.(type) {
	case int:
		if v < 0 {
			return 0, false
		}
		return uint64(v), true
	case int8:
		if v < 0 {
			return 0, false
		}
		return uint64(v), true
	case int16:
		if v < 0 {
			return 0, false
		}
		return uint64(v), true
	case int32:
		if v < 0 {
			return 0, false
		}
		return uint64(v), true
	case int64:
		if v < 0 {
			return 0, false
		}
		return uint64(v), true
	case uint:
		return uint64(v), true
	case uint8:
		return uint64(v), true
	case uint16:
		return uint64(v), true
	case uint32:
		return uint64(v), true
	case uint64:
		return v, true
	case float32:
		if v < 0 {
			return 0, false
		}
		return uint64(v), true
	case float64:
		if v < 0 {
			return 0, false
		}
		return uint64(v), true
	case string:
		u, err := strconv.ParseUint(v, 10, 64)
		return u, err == nil
	default:
		return 0, false
	}
}

func numericToFloat64(value any) (float64, bool) {
	switch v := value.(type) {
	case int:
		return float64(v), true
	case int8:
		return float64(v), true
	case int16:
		return float64(v), true
	case int32:
		return float64(v), true
	case int64:
		return float64(v), true
	case uint:
		return float64(v), true
	case uint8:
		return float64(v), true
	case uint16:
		return float64(v), true
	case uint32:
		return float64(v), true
	case uint64:
		return float64(v), true
	case float32:
		return float64(v), true
	case float64:
		return v, true
	case string:
		f, err := strconv.ParseFloat(v, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func setStructField(record reflect.Value, fieldName string, value any) error {
	field := record.FieldByName(fieldName)
	if !field.IsValid() || !field.CanSet() {
		return ErrSQLUnknownField
	}
	converted, err := convertSQLValue(value, field.Type())
	if err != nil {
		return err
	}
	if converted == nil {
		field.Set(reflect.Zero(field.Type()))
		return nil
	}
	field.Set(reflect.ValueOf(converted))
	return nil
}

// fillProjection maps selected columns into either []map[string]any or a DTO slice.
func fillProjection(to any, records reflect.Value, projections []sqlProjection) error {
	target := reflect.ValueOf(to)
	if !target.IsValid() || target.Kind() != reflect.Ptr || target.Elem().Kind() != reflect.Slice {
		return ErrSQLBadProjectionTarget
	}
	slice := target.Elem()
	elemType := slice.Type().Elem()
	result := reflect.MakeSlice(slice.Type(), 0, records.Len())
	switch {
	case elemType.Kind() == reflect.Map:
		if elemType.Key().Kind() != reflect.String {
			return ErrSQLBadProjectionTarget
		}
		for i := 0; i < records.Len(); i++ {
			row, err := projectMap(records.Index(i), projections, elemType)
			if err != nil {
				return err
			}
			result = reflect.Append(result, row)
		}
	case elemType.Kind() == reflect.Struct || (elemType.Kind() == reflect.Ptr && elemType.Elem().Kind() == reflect.Struct):
		dtoType := elemType
		ptrResult := elemType.Kind() == reflect.Ptr
		if ptrResult {
			dtoType = elemType.Elem()
		}
		lookup := projectionTargetFields(dtoType)
		for i := 0; i < records.Len(); i++ {
			row, err := projectDTO(records.Index(i), projections, dtoType, lookup)
			if err != nil {
				return err
			}
			if ptrResult {
				result = reflect.Append(result, row)
			} else {
				result = reflect.Append(result, row.Elem())
			}
		}
	default:
		return ErrSQLBadProjectionTarget
	}
	slice.Set(result)
	return nil
}

// fillRawProjection fills projection results when SQL only has metadata schema.
func fillRawProjection(to any, records []sqlRawRecord, projections []sqlProjection, model *sqlModel) error {
	target := reflect.ValueOf(to)
	if !target.IsValid() || target.Kind() != reflect.Ptr || target.Elem().Kind() != reflect.Slice {
		return ErrSQLBadProjectionTarget
	}
	slice := target.Elem()
	elemType := slice.Type().Elem()
	result := reflect.MakeSlice(slice.Type(), 0, len(records))
	switch {
	case elemType.Kind() == reflect.Map:
		if elemType.Key().Kind() != reflect.String {
			return ErrSQLBadProjectionTarget
		}
		for _, record := range records {
			row, err := projectRawMap(record, projections, elemType, model)
			if err != nil {
				return err
			}
			result = reflect.Append(result, row)
		}
	case elemType.Kind() == reflect.Struct || (elemType.Kind() == reflect.Ptr && elemType.Elem().Kind() == reflect.Struct):
		dtoType := elemType
		ptrResult := elemType.Kind() == reflect.Ptr
		if ptrResult {
			dtoType = elemType.Elem()
		}
		lookup := projectionTargetFields(dtoType)
		for _, record := range records {
			row, err := projectRawDTO(record, projections, dtoType, lookup, model)
			if err != nil {
				return err
			}
			if ptrResult {
				result = reflect.Append(result, row)
			} else {
				result = reflect.Append(result, row.Elem())
			}
		}
	default:
		return ErrSQLBadProjectionTarget
	}
	slice.Set(result)
	return nil
}

func projectMap(record reflect.Value, projections []sqlProjection, mapType reflect.Type) (reflect.Value, error) {
	row := reflect.MakeMapWithSize(mapType, len(projections))
	for _, projection := range projections {
		value := projectionValue(record, projection.field)
		mapValue, err := convertProjectionValue(value, mapType.Elem())
		if err != nil {
			return reflect.Value{}, err
		}
		row.SetMapIndex(reflect.ValueOf(projection.name).Convert(mapType.Key()), mapValue)
	}
	return row, nil
}

func projectRawMap(record sqlRawRecord, projections []sqlProjection, mapType reflect.Type, model *sqlModel) (reflect.Value, error) {
	row := reflect.MakeMapWithSize(mapType, len(projections))
	for _, projection := range projections {
		value, err := rawProjectionValue(record.raw, projection.field, model)
		if err != nil {
			return reflect.Value{}, err
		}
		mapValue, err := convertProjectionValue(value, mapType.Elem())
		if err != nil {
			return reflect.Value{}, err
		}
		row.SetMapIndex(reflect.ValueOf(projection.name).Convert(mapType.Key()), mapValue)
	}
	return row, nil
}

func projectDTO(record reflect.Value, projections []sqlProjection, dtoType reflect.Type, lookup map[string]int) (reflect.Value, error) {
	row := reflect.New(dtoType)
	for _, projection := range projections {
		index, ok := lookup[strings.ToLower(projection.name)]
		if !ok {
			index, ok = lookup[strings.ToLower(projection.field.Name)]
		}
		if !ok {
			index, ok = lookup[strings.ToLower(projection.field.JsonFieldName)]
		}
		if !ok {
			continue
		}
		target := row.Elem().Field(index)
		value := projectionValue(record, projection.field)
		converted, err := convertProjectionValue(value, target.Type())
		if err != nil {
			return reflect.Value{}, err
		}
		target.Set(converted)
	}
	return row, nil
}

func projectRawDTO(record sqlRawRecord, projections []sqlProjection, dtoType reflect.Type, lookup map[string]int, model *sqlModel) (reflect.Value, error) {
	row := reflect.New(dtoType)
	for _, projection := range projections {
		index, ok := lookup[strings.ToLower(projection.name)]
		if !ok {
			index, ok = lookup[strings.ToLower(projection.field.Name)]
		}
		if !ok {
			index, ok = lookup[strings.ToLower(projection.field.JsonFieldName)]
		}
		if !ok {
			continue
		}
		value, err := rawProjectionValue(record.raw, projection.field, model)
		if err != nil {
			return reflect.Value{}, err
		}
		converted, err := convertProjectionValue(value, row.Elem().Field(index).Type())
		if err != nil {
			return reflect.Value{}, err
		}
		row.Elem().Field(index).Set(converted)
	}
	return row, nil
}

func projectionValue(record reflect.Value, field *fieldConfig) any {
	if record.Kind() == reflect.Ptr {
		record = record.Elem()
	}
	return record.FieldByName(field.Name).Interface()
}

func rawProjectionValue(raw []byte, field *fieldConfig, model *sqlModel) (any, error) {
	result := gjson.GetBytes(raw, field.JsonFieldName)
	if !result.Exists() {
		return nil, nil
	}
	var value any
	switch result.Type {
	case gjson.String:
		value = result.Str
	case gjson.Number:
		value = result.Num
	case gjson.True:
		value = true
	case gjson.False:
		value = false
	case gjson.JSON:
		value = []byte(result.Raw)
	default:
		value = nil
	}
	if typ := model.fieldType(field); typ != nil {
		return convertSQLValue(value, typ)
	}
	return value, nil
}

func convertProjectionValue(value any, target reflect.Type) (reflect.Value, error) {
	if value == nil {
		return reflect.Zero(target), nil
	}
	source := reflect.ValueOf(value)
	if source.Type().AssignableTo(target) {
		return source, nil
	}
	if target.Kind() == reflect.Interface && source.Type().AssignableTo(target) {
		return source, nil
	}
	if target.Kind() == reflect.Interface {
		return source, nil
	}
	converted, err := convertSQLValue(value, target)
	if err != nil {
		return reflect.Value{}, err
	}
	if converted == nil {
		return reflect.Zero(target), nil
	}
	return reflect.ValueOf(converted), nil
}

func projectionTargetFields(typ reflect.Type) map[string]int {
	fields := make(map[string]int)
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.PkgPath != "" {
			continue
		}
		fields[strings.ToLower(field.Name)] = i
		if jsonName := sqlJSONFieldName(field); jsonName != "" {
			fields[strings.ToLower(jsonName)] = i
		}
	}
	return fields
}

func sqlJSONFieldName(field reflect.StructField) string {
	name := strings.Split(field.Tag.Get("json"), ",")[0]
	if name == "-" {
		return ""
	}
	return name
}

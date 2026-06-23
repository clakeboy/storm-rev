package storm

import (
	"bytes"
	"reflect"
	"sort"

	"github.com/clakeboy/storm-rev/index"
	"github.com/clakeboy/storm-rev/q"
	bolt "go.etcd.io/bbolt"
)

type selectIndexPlanKind int

const (
	selectIndexPlanExact selectIndexPlanKind = iota
	selectIndexPlanIn
	selectIndexPlanRange
	selectIndexPlanComposite
	selectIndexPlanUnion
)

type selectIndexPlan struct {
	kind      selectIndexPlanKind
	cfg       *structConfig
	field     string
	value     any
	values    []any
	min       any
	max       any
	indexName string
	children  []*selectIndexPlan
	score     int
}

// selectIndexPlan returns an index-backed candidate plan for Select when the
// external index is safe to trust. The original matcher still performs final filtering.
func (qr *query) selectIndexPlan(cfg *structConfig) (*selectIndexPlan, bool) {
	if qr.tree == nil || qr.node == nil || qr.node.tx != nil || qr.node.s == nil || qr.node.s.indexer == nil {
		return nil, false
	}
	if qr.node.s.indexer.isDirty(cfg.Name) {
		return nil, false
	}

	expr, ok := q.Explain(qr.tree)
	if !ok {
		return nil, false
	}
	return selectIndexPlanForConfig(cfg, expr)
}

func selectIndexPlanForConfig(cfg *structConfig, expr q.Expr) (*selectIndexPlan, bool) {
	switch expr.Op {
	case q.ExprAnd:
		return selectIndexPlanForAnd(cfg, expr)
	case q.ExprOr:
		return selectIndexPlanForOr(cfg, expr)
	default:
		return selectIndexPlanForLeaf(cfg, expr)
	}
}

// selectIndexPlanForAnd chooses the narrowest safe candidate source from an AND tree.
func selectIndexPlanForAnd(cfg *structConfig, expr q.Expr) (*selectIndexPlan, bool) {
	children := flattenSelectExpr(expr, q.ExprAnd)
	if plan, ok := selectCompositePlan(cfg, children); ok {
		return plan, true
	}

	var best *selectIndexPlan
	if plan, ok := selectRangePlan(cfg, children); ok {
		best = plan
	}

	for _, child := range children {
		plan, ok := selectIndexPlanForConfig(cfg, child)
		if !ok {
			continue
		}
		if best == nil || plan.score > best.score {
			best = plan
		}
	}
	if best == nil {
		return nil, false
	}
	return best, true
}

// selectIndexPlanForOr only uses indexes when every branch can supply safe candidates.
func selectIndexPlanForOr(cfg *structConfig, expr q.Expr) (*selectIndexPlan, bool) {
	if len(expr.Children) == 0 {
		return nil, false
	}

	children := make([]*selectIndexPlan, 0, len(expr.Children))
	for _, child := range expr.Children {
		plan, ok := selectIndexPlanForConfig(cfg, child)
		if !ok {
			return nil, false
		}
		children = append(children, plan)
	}

	return &selectIndexPlan{
		kind:     selectIndexPlanUnion,
		cfg:      cfg,
		children: children,
		score:    1500,
	}, true
}

// selectIndexPlanForLeaf maps simple field matchers to single-field index lookups.
func selectIndexPlanForLeaf(cfg *structConfig, expr q.Expr) (*selectIndexPlan, bool) {
	switch expr.Op {
	case q.ExprEq, q.ExprStrictEq:
		if !fieldCanUseSelectIndex(cfg, expr.Field) || selectValueMissingFromIndex(expr.Value) {
			return nil, false
		}
		return &selectIndexPlan{
			kind:  selectIndexPlanExact,
			cfg:   cfg,
			field: expr.Field,
			value: expr.Value,
			score: selectExactScore(cfg.Fields[expr.Field]),
		}, true
	case q.ExprIn:
		if !fieldCanUseSelectIndex(cfg, expr.Field) {
			return nil, false
		}
		values, ok := selectInValues(expr.Value)
		if !ok {
			return nil, false
		}
		return &selectIndexPlan{
			kind:   selectIndexPlanIn,
			cfg:    cfg,
			field:  expr.Field,
			values: values,
			score:  2500,
		}, true
	default:
		return nil, false
	}
}

// selectCompositePlan prefers complete composite equality matches over single-field plans.
func selectCompositePlan(cfg *structConfig, exprs []q.Expr) (*selectIndexPlan, bool) {
	if len(cfg.CompositeIndexes) == 0 {
		return nil, false
	}

	names := make([]string, 0, len(cfg.CompositeIndexes))
	for name := range cfg.CompositeIndexes {
		names = append(names, name)
	}
	sort.Strings(names)

	var best *selectIndexPlan
	for _, name := range names {
		composite := cfg.CompositeIndexes[name]
		values := make([]any, len(composite.Fields))
		matched := true
		for i, field := range composite.Fields {
			value, ok := selectEqualityValue(exprs, field.Name)
			if !ok || selectValueMissingFromIndex(value) {
				matched = false
				break
			}
			values[i] = value
		}
		if !matched {
			continue
		}

		plan := &selectIndexPlan{
			kind:      selectIndexPlanComposite,
			cfg:       cfg,
			values:    values,
			indexName: name,
			score:     4000 + len(composite.Fields),
		}
		if best == nil || plan.score > best.score {
			best = plan
		}
	}
	if best == nil {
		return nil, false
	}
	return best, true
}

// selectRangePlan supports closed Gte/Lte ranges whose bounds are present in the index.
func selectRangePlan(cfg *structConfig, exprs []q.Expr) (*selectIndexPlan, bool) {
	type bounds struct {
		min    any
		max    any
		hasMin bool
		hasMax bool
	}

	byField := make(map[string]*bounds)
	for _, expr := range exprs {
		if selectValueMissingFromIndex(expr.Value) || !fieldCanUseSelectIndex(cfg, expr.Field) {
			continue
		}
		b := byField[expr.Field]
		if b == nil {
			b = &bounds{}
			byField[expr.Field] = b
		}
		switch expr.Op {
		case q.ExprGte:
			b.min = expr.Value
			b.hasMin = true
		case q.ExprLte:
			b.max = expr.Value
			b.hasMax = true
		}
	}

	fields := make([]string, 0, len(byField))
	for field, b := range byField {
		if b.hasMin && b.hasMax {
			fields = append(fields, field)
		}
	}
	sort.Strings(fields)
	if len(fields) == 0 {
		return nil, false
	}

	field := fields[0]
	b := byField[field]
	return &selectIndexPlan{
		kind:  selectIndexPlanRange,
		cfg:   cfg,
		field: field,
		min:   b.min,
		max:   b.max,
		score: 2000,
	}, true
}

func selectEqualityValue(exprs []q.Expr, field string) (any, bool) {
	for _, expr := range exprs {
		if expr.Field != field {
			continue
		}
		switch expr.Op {
		case q.ExprEq, q.ExprStrictEq:
			return expr.Value, true
		}
	}
	return nil, false
}

func flattenSelectExpr(expr q.Expr, op q.ExprOp) []q.Expr {
	if expr.Op != op {
		return []q.Expr{expr}
	}

	exprs := make([]q.Expr, 0, len(expr.Children))
	for _, child := range expr.Children {
		exprs = append(exprs, flattenSelectExpr(child, op)...)
	}
	return exprs
}

func fieldCanUseSelectIndex(cfg *structConfig, fieldName string) bool {
	field, ok := cfg.Fields[fieldName]
	return ok && (field.IsID || field.Index != "")
}

func selectExactScore(field *fieldConfig) int {
	if field != nil && (field.IsID || field.Index == tagUniqueIdx) {
		return 3500
	}
	return 3000
}

func selectInValues(value any) ([]any, bool) {
	ref := reflect.ValueOf(value)
	if !ref.IsValid() {
		return nil, false
	}
	if ref.Kind() != reflect.Slice && ref.Kind() != reflect.Array {
		return nil, false
	}
	if ref.Kind() == reflect.Slice && ref.IsNil() {
		return nil, false
	}

	values := make([]any, 0, ref.Len())
	for i := 0; i < ref.Len(); i++ {
		value := ref.Index(i).Interface()
		if selectValueMissingFromIndex(value) {
			return nil, false
		}
		values = append(values, value)
	}
	return values, true
}

func isNilAny(value any) bool {
	if value == nil {
		return true
	}
	ref := reflect.ValueOf(value)
	switch ref.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return ref.IsNil()
	default:
		return false
	}
}

// selectValueMissingFromIndex reports values skipped by indexRecord, such as zero values.
func selectValueMissingFromIndex(value any) bool {
	if isNilAny(value) {
		return true
	}
	ref := reflect.ValueOf(value)
	return isZero(&ref)
}

// candidateIDs reads index hits only as candidate Bolt keys; query filtering
// remains the source of truth for whether a record is returned.
func (p *selectIndexPlan) candidateIDs(qr *query, bucket *bolt.Bucket) ([][]byte, bool, error) {
	switch p.kind {
	case selectIndexPlanExact:
		return p.exactCandidateIDs(qr, bucket, p.value)
	case selectIndexPlanIn:
		var ids [][]byte
		for _, value := range p.values {
			next, ok, err := p.exactCandidateIDs(qr, bucket, value)
			if err != nil || !ok {
				return nil, ok, err
			}
			ids = append(ids, next...)
		}
		return dedupeSelectIndexIDs(ids), true, nil
	case selectIndexPlanRange:
		ids, err := qr.node.s.indexer.searchRange(p.cfg, p.field, p.min, p.max)
		if shouldFallbackSelectIndex(err) {
			return nil, false, nil
		}
		return copySelectIndexIDs(ids), true, err
	case selectIndexPlanComposite:
		ids, err := qr.node.s.indexer.searchComposite(p.cfg, p.indexName, p.values)
		if shouldFallbackSelectIndex(err) {
			return nil, false, nil
		}
		return copySelectIndexIDs(ids), true, err
	case selectIndexPlanUnion:
		var ids [][]byte
		for _, child := range p.children {
			next, ok, err := child.candidateIDs(qr, bucket)
			if err != nil || !ok {
				return nil, ok, err
			}
			ids = append(ids, next...)
		}
		return dedupeSelectIndexIDs(ids), true, nil
	default:
		return nil, false, nil
	}
}

func (p *selectIndexPlan) exactCandidateIDs(qr *query, bucket *bolt.Bucket, value any) ([][]byte, bool, error) {
	field := p.cfg.Fields[p.field]
	if field.IsID {
		id, err := toBytes(value, qr.node.codec)
		if err != nil {
			return nil, false, err
		}
		if id == nil || bucket.Get(id) == nil {
			return nil, true, nil
		}
		return [][]byte{append([]byte(nil), id...)}, true, nil
	}

	ids, err := qr.node.s.indexer.searchExact(p.cfg, p.field, value)
	if shouldFallbackSelectIndex(err) {
		return nil, false, nil
	}
	return copySelectIndexIDs(ids), true, err
}

func shouldFallbackSelectIndex(err error) bool {
	return err == index.ErrNotFound || err == ErrIdxNotFound || err == ErrIncompatibleValue
}

func copySelectIndexIDs(ids [][]byte) [][]byte {
	copied := make([][]byte, 0, len(ids))
	for _, id := range ids {
		copied = append(copied, append([]byte(nil), id...))
	}
	return copied
}

func dedupeSelectIndexIDs(ids [][]byte) [][]byte {
	seen := make(map[string]bool, len(ids))
	deduped := make([][]byte, 0, len(ids))
	for _, id := range ids {
		key := string(id)
		if seen[key] {
			continue
		}
		seen[key] = true
		deduped = append(deduped, id)
	}
	return deduped
}

func sortSelectIndexIDs(ids [][]byte, reverse bool) {
	sort.Slice(ids, func(i, j int) bool {
		cmp := bytes.Compare(ids[i], ids[j])
		if reverse {
			return cmp > 0
		}
		return cmp < 0
	})
}

// runSelectIndexPlan feeds candidate records through the normal sorter/filter pipeline.
func (qr *query) runSelectIndexPlan(bucket *bolt.Bucket, sorter *sorter, plan *selectIndexPlan) (bool, int64, error) {
	ids, ok, err := plan.candidateIDs(qr, bucket)
	if err != nil || !ok {
		return ok, 0, err
	}
	sortSelectIndexIDs(ids, qr.reverse)

	var scanned int64
	for _, id := range ids {
		raw := bucket.Get(id)
		if raw == nil {
			qr.node.s.indexer.markDirty(plan.cfg.Name)
			continue
		}

		scanned++
		stop, err := sorter.filter(qr.tree, bucket, id, raw)
		if err != nil {
			return true, scanned, err
		}
		if stop {
			break
		}
	}
	return true, scanned, nil
}

package q

import "go/token"

// ExprOp identifies the built-in matcher shape returned by Explain.
type ExprOp int

const (
	ExprUnknown ExprOp = iota
	ExprTrue
	ExprAnd
	ExprOr
	ExprNot
	ExprEq
	ExprStrictEq
	ExprIn
	ExprGt
	ExprGte
	ExprLt
	ExprLte
	ExprRe
)

// Expr is a read-only description of a built-in matcher tree.
type Expr struct {
	Op       ExprOp
	Field    string
	Value    any
	Children []Expr
}

// Explain exposes the structure of built-in matchers without changing how they match.
// Unknown nested matchers are preserved as ExprUnknown so callers can still use
// indexable siblings as a candidate set and let the original matcher do final filtering.
func Explain(m Matcher) (Expr, bool) {
	return explain(m, false)
}

func explain(m Matcher, nested bool) (Expr, bool) {
	if m == nil {
		return Expr{Op: ExprTrue}, true
	}

	switch matcher := m.(type) {
	case *trueMatcher:
		return Expr{Op: ExprTrue}, true
	case *and:
		return explainChildren(ExprAnd, matcher.children)
	case *or:
		return explainChildren(ExprOr, matcher.children)
	case *not:
		return explainChildren(ExprNot, matcher.children)
	case *fieldMatcherDelegate:
		return explainFieldMatcher(matcher.Field, matcher.FieldMatcher), true
	case fieldMatcherDelegate:
		return explainFieldMatcher(matcher.Field, matcher.FieldMatcher), true
	default:
		if nested {
			return Expr{Op: ExprUnknown}, true
		}
		return Expr{}, false
	}
}

func explainChildren(op ExprOp, matchers []Matcher) (Expr, bool) {
	children := make([]Expr, 0, len(matchers))
	for _, matcher := range matchers {
		child, ok := explain(matcher, true)
		if !ok {
			child = Expr{Op: ExprUnknown}
		}
		children = append(children, child)
	}
	return Expr{Op: op, Children: children}, true
}

func explainFieldMatcher(field string, matcher FieldMatcher) Expr {
	switch m := matcher.(type) {
	case *cmp:
		return Expr{Op: exprOpForToken(m.token), Field: field, Value: m.value}
	case *strictEq:
		return Expr{Op: ExprStrictEq, Field: field, Value: m.value}
	case *in:
		return Expr{Op: ExprIn, Field: field, Value: m.list}
	case *regexpMatcher:
		pattern := ""
		if m.r != nil {
			pattern = m.r.String()
		}
		return Expr{Op: ExprRe, Field: field, Value: pattern}
	default:
		return Expr{Op: ExprUnknown, Field: field}
	}
}

func exprOpForToken(tok token.Token) ExprOp {
	switch tok {
	case token.EQL:
		return ExprEq
	case token.GTR:
		return ExprGt
	case token.GEQ:
		return ExprGte
	case token.LSS:
		return ExprLt
	case token.LEQ:
		return ExprLte
	default:
		return ExprUnknown
	}
}

package gormx

import (
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

// Order is a schema-validated ordering bound to a model.
type Order[T any] struct {
	ref  columnRef[T]
	desc bool
}

// Wrapper is immutable: every chain method returns a new wrapper.
// Keep the returned value when building conditions dynamically.
type Wrapper[T any] struct {
	predicates []Predicate[T]
	orders     []Order[T]
}

func NewWrapper[T any]() *Wrapper[T] { return &Wrapper[T]{} }

// NewLambda starts the same query engine using Field(...).Eq(...) predicates.
func NewLambda[T any]() *Wrapper[T] { return NewWrapper[T]() }

func (w *Wrapper[T]) clone() *Wrapper[T] {
	if w == nil {
		return NewWrapper[T]()
	}
	return &Wrapper[T]{
		predicates: append([]Predicate[T](nil), w.predicates...),
		orders:     append([]Order[T](nil), w.orders...),
	}
}

func (w *Wrapper[T]) Where(predicates ...Predicate[T]) *Wrapper[T] {
	next := w.clone()
	next.predicates = append(next.predicates, predicates...)
	return next
}

// WhereIf conditionally appends predicates. Go still evaluates its arguments first.
func (w *Wrapper[T]) WhereIf(condition bool, predicates ...Predicate[T]) *Wrapper[T] {
	if !condition {
		return w.clone()
	}
	return w.Where(predicates...)
}

func (w *Wrapper[T]) Eq(field string, value any) *Wrapper[T] {
	return w.Where(comparison(stringColumn[T](field), "eq", value))
}

func (w *Wrapper[T]) EqIf(condition bool, field string, value any) *Wrapper[T] {
	return w.WhereIf(condition, comparison(stringColumn[T](field), "eq", value))
}

func (w *Wrapper[T]) Ne(field string, value any) *Wrapper[T] {
	return w.Where(comparison(stringColumn[T](field), "ne", value))
}

func (w *Wrapper[T]) Gt(field string, value any) *Wrapper[T] {
	return w.Where(comparison(stringColumn[T](field), "gt", value))
}

func (w *Wrapper[T]) Ge(field string, value any) *Wrapper[T] {
	return w.Where(comparison(stringColumn[T](field), "ge", value))
}

func (w *Wrapper[T]) Lt(field string, value any) *Wrapper[T] {
	return w.Where(comparison(stringColumn[T](field), "lt", value))
}

func (w *Wrapper[T]) Le(field string, value any) *Wrapper[T] {
	return w.Where(comparison(stringColumn[T](field), "le", value))
}

func (w *Wrapper[T]) Like(field string, pattern string) *Wrapper[T] {
	return w.Where(comparison(stringColumn[T](field), "like", pattern))
}

func (w *Wrapper[T]) LikeIf(condition bool, field string, pattern string) *Wrapper[T] {
	return w.WhereIf(condition, comparison(stringColumn[T](field), "like", pattern))
}

func (w *Wrapper[T]) NotLike(field string, pattern string) *Wrapper[T] {
	return w.Where(comparison(stringColumn[T](field), "not_like", pattern))
}

func (w *Wrapper[T]) In(field string, values any) *Wrapper[T] {
	return w.Where(membership(stringColumn[T](field), values, false))
}

func (w *Wrapper[T]) NotIn(field string, values any) *Wrapper[T] {
	return w.Where(membership(stringColumn[T](field), values, true))
}

func (w *Wrapper[T]) Between(field string, lower, upper any) *Wrapper[T] {
	ref := stringColumn[T](field)
	return w.Where(All(comparison(ref, "ge", lower), comparison(ref, "le", upper)))
}

func (w *Wrapper[T]) IsNull(field string) *Wrapper[T] {
	return w.Eq(field, nil)
}

func (w *Wrapper[T]) IsNotNull(field string) *Wrapper[T] {
	return w.Ne(field, nil)
}

// And appends a nested AND group. Ordering inside groups is not supported.
func (w *Wrapper[T]) And(build func(*Wrapper[T]) *Wrapper[T]) *Wrapper[T] {
	return w.nested(false, build)
}

// Or appends a nested OR group, joined to other top-level conditions with AND.
//
//	Example: Eq("active", true).Or(func(q *Wrapper[T]) *Wrapper[T] {
//	    return q.Eq("role", "admin").Eq("role", "editor")
//	}) means active = true AND (role = 'admin' OR role = 'editor').
func (w *Wrapper[T]) Or(build func(*Wrapper[T]) *Wrapper[T]) *Wrapper[T] {
	return w.nested(true, build)
}

func (w *Wrapper[T]) nested(or bool, build func(*Wrapper[T]) *Wrapper[T]) *Wrapper[T] {
	if build == nil {
		return w.Where(Predicate[T]{})
	}
	child := build(NewWrapper[T]())
	if child == nil || len(child.orders) != 0 {
		return w.Where(Predicate[T]{})
	}
	return w.Where(group(or, child.predicates))
}

func (w *Wrapper[T]) OrderBy(orders ...Order[T]) *Wrapper[T] {
	next := w.clone()
	next.orders = append(next.orders, orders...)
	return next
}

func (w *Wrapper[T]) OrderByAsc(fields ...string) *Wrapper[T] {
	next := w.clone()
	for _, field := range fields {
		next.orders = append(next.orders, Order[T]{ref: stringColumn[T](field)})
	}
	return next
}

func (w *Wrapper[T]) OrderByDesc(fields ...string) *Wrapper[T] {
	next := w.clone()
	for _, field := range fields {
		next.orders = append(next.orders, Order[T]{ref: stringColumn[T](field), desc: true})
	}
	return next
}

type querySpec struct {
	expressions []clause.Expression
	orders      []clause.OrderByColumn
	restrictive bool
}

func (w *Wrapper[T]) compile(s *schema.Schema) (querySpec, error) {
	var spec querySpec
	if w == nil {
		return spec, nil
	}
	for _, p := range w.predicates {
		if p.build == nil {
			return spec, ErrInvalidQuery
		}
		expr, err := p.build(s)
		if err != nil {
			return spec, err
		}
		spec.expressions = append(spec.expressions, expr)
		spec.restrictive = spec.restrictive || p.restrictive
	}
	for _, order := range w.orders {
		if order.ref == nil {
			return spec, ErrInvalidField
		}
		col, err := order.ref(s)
		if err != nil {
			return spec, err
		}
		spec.orders = append(spec.orders, clause.OrderByColumn{Column: col, Desc: order.desc})
	}
	return spec, nil
}

func (s querySpec) apply(db *gorm.DB, ordering bool) *gorm.DB {
	if len(s.expressions) > 0 {
		db = db.Clauses(clause.Where{Exprs: s.expressions})
	}
	if ordering && len(s.orders) > 0 {
		db = db.Clauses(clause.OrderBy{Columns: s.orders})
	}
	return db
}

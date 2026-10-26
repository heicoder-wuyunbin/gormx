package gormx

import (
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

// Order is a schema-validated ordering bound to model T.
//
// Build it with Column.Asc/Desc or the OrderByAsc/OrderByDesc helpers. The zero
// value is invalid and fails when the query compiles. Ordering is applied by
// First, List and SelectPage and ignored by Count, Update and Delete.
type Order[T any] struct {
	ref  columnRef[T]
	desc bool
}

// Wrapper is an immutable collection of predicates and orderings for model T.
//
// Every chain method returns a new wrapper and leaves the receiver untouched,
// so a base wrapper can be shared and branched; when building conditions
// dynamically, keep the returned value. The zero value is usable as "no
// conditions", and methods tolerate a nil receiver by starting from an empty
// wrapper. A nil wrapper passed to Update or Delete is rejected with
// ErrUnsafeWrite because it contains no restrictive condition.
//
// Conditions are validated when a DAO executes them, not while chaining, so an
// unknown field surfaces as ErrInvalidField before SQL reaches the database.
// String-based methods accept either a Go field name or a database column name.
// Prefer Predicate/Column selectors when compile-time value typing matters.
type Wrapper[T any] struct {
	predicates []Predicate[T]
	orders     []Order[T]
}

// NewWrapper returns an empty wrapper, equivalent to the zero value. It exists
// for readability at the start of a chain.
func NewWrapper[T any]() *Wrapper[T] { return &Wrapper[T]{} }

// NewLambda starts the same query engine using Field(...).Eq(...) predicates
// rather than string field names. It is an alias for NewWrapper.
func NewLambda[T any]() *Wrapper[T] { return NewWrapper[T]() }

// clone returns a shallow copy with copied predicate and order slices. A nil
// receiver clones to an empty wrapper, which is why chaining on nil is safe.
func (w *Wrapper[T]) clone() *Wrapper[T] {
	if w == nil {
		return NewWrapper[T]()
	}
	return &Wrapper[T]{
		predicates: append([]Predicate[T](nil), w.predicates...),
		orders:     append([]Order[T](nil), w.orders...),
	}
}

// Where appends predicates at the top level. All top-level predicates are
// combined with AND, so adding a condition can only narrow the result set.
// Passed slices are not aliased; later changes to the argument list do not
// affect the returned wrapper.
func (w *Wrapper[T]) Where(predicates ...Predicate[T]) *Wrapper[T] {
	next := w.clone()
	next.predicates = append(next.predicates, predicates...)
	return next
}

// WhereIf appends predicates only when condition is true; otherwise it returns
// an unchanged copy. Go evaluates the predicate arguments before the call, so
// building a predicate can still be expensive or have side effects even when
// condition is false.
func (w *Wrapper[T]) WhereIf(condition bool, predicates ...Predicate[T]) *Wrapper[T] {
	if !condition {
		return w.clone()
	}
	return w.Where(predicates...)
}

// Eq requires field = value. Passing nil produces IS NULL; IsNull is the
// clearer spelling. field may be a Go field name or a database column name. The
// condition is restrictive and can authorize Update/Delete.
func (w *Wrapper[T]) Eq(field string, value any) *Wrapper[T] {
	return w.Where(comparison(stringColumn[T](field), "eq", value))
}

// EqIf is Eq applied only when condition is true. The arguments are evaluated
// before the call regardless of condition.
func (w *Wrapper[T]) EqIf(condition bool, field string, value any) *Wrapper[T] {
	return w.WhereIf(condition, comparison(stringColumn[T](field), "eq", value))
}

// Ne requires field <> value. Passing nil produces IS NOT NULL; IsNotNull is the
// clearer spelling. Rows whose column is NULL never match a non-nil Ne because
// SQL NULL comparisons are unknown.
func (w *Wrapper[T]) Ne(field string, value any) *Wrapper[T] {
	return w.Where(comparison(stringColumn[T](field), "ne", value))
}

// Gt requires field > value. Rows with a NULL field do not match. Type
// compatibility between value and column is decided by the database.
func (w *Wrapper[T]) Gt(field string, value any) *Wrapper[T] {
	return w.Where(comparison(stringColumn[T](field), "gt", value))
}

// Ge requires field >= value. Rows with a NULL field do not match.
func (w *Wrapper[T]) Ge(field string, value any) *Wrapper[T] {
	return w.Where(comparison(stringColumn[T](field), "ge", value))
}

// Lt requires field < value. Rows with a NULL field do not match.
func (w *Wrapper[T]) Lt(field string, value any) *Wrapper[T] {
	return w.Where(comparison(stringColumn[T](field), "lt", value))
}

// Le requires field <= value. Rows with a NULL field do not match.
func (w *Wrapper[T]) Le(field string, value any) *Wrapper[T] {
	return w.Where(comparison(stringColumn[T](field), "le", value))
}

// Like requires field LIKE pattern. Wildcards % and _ keep their SQL meaning and
// pattern is bound as a parameter; escape user input when a literal match is
// intended. Case sensitivity follows the database collation. The column's type
// is not checked here, so the database decides whether LIKE is valid.
func (w *Wrapper[T]) Like(field string, pattern string) *Wrapper[T] {
	return w.Where(comparison(stringColumn[T](field), "like", pattern))
}

// LikeIf is Like applied only when condition is true. The arguments are
// evaluated before the call regardless of condition.
func (w *Wrapper[T]) LikeIf(condition bool, field string, pattern string) *Wrapper[T] {
	return w.WhereIf(condition, comparison(stringColumn[T](field), "like", pattern))
}

// NotLike requires field NOT LIKE pattern, negating Like. NULL fields do not
// match. Wildcards and escaping rules are the same as Like.
func (w *Wrapper[T]) NotLike(field string, pattern string) *Wrapper[T] {
	return w.Where(comparison(stringColumn[T](field), "not_like", pattern))
}

// In requires field IN (values...). values must be a slice or array; any other
// non-nil value fails with ErrInvalidQuery when the query is built. A nil or
// empty list compiles to a contradiction that matches nothing and remains
// restrictive, so it is safe for writes.
func (w *Wrapper[T]) In(field string, values any) *Wrapper[T] {
	return w.Where(membership(stringColumn[T](field), values, false))
}

// NotIn requires field NOT IN (values...). values must be a slice or array. A
// nil or empty list compiles to a tautology (1 = 1) that matches every row and
// is NOT restrictive, so it cannot authorize an Update or Delete by itself.
func (w *Wrapper[T]) NotIn(field string, values any) *Wrapper[T] {
	return w.Where(membership(stringColumn[T](field), values, true))
}

// Between requires lower <= field <= upper, both bounds inclusive. It is built
// as field >= lower AND field <= upper; swapped bounds match no rows rather than
// returning an error. Bounds are untyped here, so the database decides type
// compatibility.
func (w *Wrapper[T]) Between(field string, lower, upper any) *Wrapper[T] {
	ref := stringColumn[T](field)
	return w.Where(All(comparison(ref, "ge", lower), comparison(ref, "le", upper)))
}

// IsNull requires field IS NULL. It is Eq with a nil value and is restrictive.
func (w *Wrapper[T]) IsNull(field string) *Wrapper[T] {
	return w.Eq(field, nil)
}

// IsNotNull requires field IS NOT NULL. It is Ne with a nil value and is
// restrictive.
func (w *Wrapper[T]) IsNotNull(field string) *Wrapper[T] {
	return w.Ne(field, nil)
}

// And appends a nested AND group; the group is ANDed with the other top-level
// conditions and is restrictive when any member is restrictive.
//
// The build callback receives an empty wrapper and must return the group. A nil
// callback, or a group that adds ordering, produces an invalid predicate and
// makes the query fail with ErrInvalidQuery. Ordering is only supported at the
// top level.
func (w *Wrapper[T]) And(build func(*Wrapper[T]) *Wrapper[T]) *Wrapper[T] {
	return w.nested(false, build)
}

// Or appends a nested OR group, joined to other top-level conditions with AND.
//
//	Example: Eq("active", true).Or(func(q *Wrapper[T]) *Wrapper[T] {
//	    return q.Eq("role", "admin").Eq("role", "editor")
//	}) means active = true AND (role = 'admin' OR role = 'editor').
//
// The group is restrictive only when every branch is restrictive, because one
// tautological branch would make the whole group match everything. A nil
// callback or ordering inside the group produces an invalid predicate and makes
// the query fail with ErrInvalidQuery.
func (w *Wrapper[T]) Or(build func(*Wrapper[T]) *Wrapper[T]) *Wrapper[T] {
	return w.nested(true, build)
}

// nested runs build against an empty wrapper and appends the result as one
// group. Invalid callbacks intentionally become a zero Predicate so the failure
// surfaces at query build time instead of silently widening the condition.
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

// OrderBy appends typed orderings. Each Order is validated against the schema
// when the query executes; the zero Order fails with ErrInvalidField. Ordering
// is ignored by Count, Update and Delete.
func (w *Wrapper[T]) OrderBy(orders ...Order[T]) *Wrapper[T] {
	next := w.clone()
	next.orders = append(next.orders, orders...)
	return next
}

// OrderByAsc appends ascending ordering by field names, which may be Go field
// names or database column names. Unknown or unmapped fields fail with
// ErrInvalidField when the query executes.
func (w *Wrapper[T]) OrderByAsc(fields ...string) *Wrapper[T] {
	next := w.clone()
	for _, field := range fields {
		next.orders = append(next.orders, Order[T]{ref: stringColumn[T](field)})
	}
	return next
}

// OrderByDesc appends descending ordering by field names, which may be Go field
// names or database column names.
func (w *Wrapper[T]) OrderByDesc(fields ...string) *Wrapper[T] {
	next := w.clone()
	for _, field := range fields {
		next.orders = append(next.orders, Order[T]{ref: stringColumn[T](field), desc: true})
	}
	return next
}

// querySpec is the compiled, schema-bound form of a Wrapper. It is built fresh
// per statement and is not part of the public API.
type querySpec struct {
	expressions []clause.Expression
	orders      []clause.OrderByColumn
	restrictive bool
}

// compile resolves every predicate and ordering against s. A nil wrapper
// compiles to an empty spec, meaning "all rows". Each predicate is built once;
// the first error aborts compilation. A spec is restrictive when at least one
// top-level predicate is restrictive, because top-level predicates are ANDed.
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

// apply attaches the compiled conditions, and optionally the ordering, to db.
// Write and count paths pass ordering=false so ORDER BY cannot leak into
// statements where it is meaningless.
func (s querySpec) apply(db *gorm.DB, ordering bool) *gorm.DB {
	if len(s.expressions) > 0 {
		db = db.Clauses(clause.Where{Exprs: s.expressions})
	}
	if ordering && len(s.orders) > 0 {
		db = db.Clauses(clause.OrderBy{Columns: s.orders})
	}
	return db
}

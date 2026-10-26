package gormx

import (
	"fmt"
	"reflect"
	"regexp"

	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

var joinIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)

// JoinTable is a model-bound table instance created by TableOf.
// Reuse the same instance when referencing its fields.
type JoinTable interface {
	joinTable() *tableBinding
}

type tableBinding struct {
	alias string
	model reflect.Type
}

type TableRef[T any] struct {
	binding *tableBinding
}

// TableOf creates an identity for a model and alias, not a database table.
// Use separate instances with different aliases for self joins.
func TableOf[T any](alias string) TableRef[T] {
	return TableRef[T]{binding: &tableBinding{
		alias: alias,
		model: reflect.TypeOf((*T)(nil)).Elem(),
	}}
}

func (t TableRef[T]) joinTable() *tableBinding { return t.binding }

// Col selects a field by Go name or database column name.
func (t TableRef[T]) Col(name string) JoinColumn[any] {
	return Bind(t, Column[T, any]{ref: stringColumn[T](name)})
}

// Bind attaches an existing typed selector to a specific table instance.
// The table and selector must refer to the same model type.
func Bind[T, V any](table TableRef[T], column Column[T, V]) JoinColumn[V] {
	ref := joinedColumn{table: table.binding}
	if column.ref != nil {
		ref.resolve = func(s *schema.Schema) (clause.Column, error) { return column.ref(s) }
	}
	return JoinColumn[V]{ref: ref}
}

// JoinField is implemented by JoinColumn[V] for any V.
type JoinField interface {
	joinColumn() joinedColumn
}

type joinedColumn struct {
	table   *tableBinding
	resolve func(*schema.Schema) (clause.Column, error)
}

type joinScope map[*tableBinding]*schema.Schema

func (c joinedColumn) column(scope joinScope) (clause.Column, error) {
	s, exists := scope[c.table]
	if !exists || c.table == nil || c.resolve == nil {
		return clause.Column{}, fmt.Errorf("%w: unbound or out-of-scope join field", ErrInvalidField)
	}
	column, err := c.resolve(s)
	if err != nil {
		return clause.Column{}, err
	}
	column.Table = c.table.alias
	return column, nil
}

// JoinColumn retains the value type of a bound selector.
type JoinColumn[V any] struct {
	ref joinedColumn
}

func (c JoinColumn[V]) joinColumn() joinedColumn { return c.ref }

// Projection must have a unique explicit alias matching a readable DTO DB field.
type Projection struct {
	ref   joinedColumn
	alias string
}

func (c JoinColumn[V]) As(alias string) Projection {
	return Projection{ref: c.ref, alias: alias}
}

type JoinOrder struct {
	ref  joinedColumn
	desc bool
}

func (c JoinColumn[V]) Asc() JoinOrder  { return JoinOrder{ref: c.ref} }
func (c JoinColumn[V]) Desc() JoinOrder { return JoinOrder{ref: c.ref, desc: true} }

func (c JoinColumn[V]) Eq(value V) JoinPredicate { return joinComparison(c.ref, "eq", value) }
func (c JoinColumn[V]) Ne(value V) JoinPredicate { return joinComparison(c.ref, "ne", value) }
func (c JoinColumn[V]) Gt(value V) JoinPredicate { return joinComparison(c.ref, "gt", value) }
func (c JoinColumn[V]) Ge(value V) JoinPredicate { return joinComparison(c.ref, "ge", value) }
func (c JoinColumn[V]) Lt(value V) JoinPredicate { return joinComparison(c.ref, "lt", value) }
func (c JoinColumn[V]) Le(value V) JoinPredicate { return joinComparison(c.ref, "le", value) }
func (c JoinColumn[V]) IsNull() JoinPredicate    { return joinComparison(c.ref, "eq", nil) }
func (c JoinColumn[V]) IsNotNull() JoinPredicate { return joinComparison(c.ref, "ne", nil) }

func (c JoinColumn[V]) Like(pattern string) JoinPredicate {
	return joinComparison(c.ref, "like", pattern)
}

func (c JoinColumn[V]) NotLike(pattern string) JoinPredicate {
	return joinComparison(c.ref, "not_like", pattern)
}

func (c JoinColumn[V]) Between(lower, upper V) JoinPredicate {
	return JoinAll(c.Ge(lower), c.Le(upper))
}

func (c JoinColumn[V]) In(values ...V) JoinPredicate    { return joinMembership(c.ref, values, false) }
func (c JoinColumn[V]) NotIn(values ...V) JoinPredicate { return joinMembership(c.ref, values, true) }

// JoinPredicate is an immutable condition scoped to table instances.
type JoinPredicate struct {
	build func(joinScope) (clause.Expression, error)
}

func joinComparison(ref joinedColumn, op string, value any) JoinPredicate {
	return JoinPredicate{build: func(scope joinScope) (clause.Expression, error) {
		column, err := ref.column(scope)
		if err != nil {
			return nil, err
		}
		// Reuse single-table operator and parameter binding semantics.
		p := comparison[any](func(*schema.Schema) (clause.Column, error) { return column, nil }, op, value)
		return p.build(nil)
	}}
}

func joinMembership[V any](ref joinedColumn, values []V, negate bool) JoinPredicate {
	copied := append([]V(nil), values...)
	return JoinPredicate{build: func(scope joinScope) (clause.Expression, error) {
		column, err := ref.column(scope)
		if err != nil {
			return nil, err
		}
		p := membership[any](func(*schema.Schema) (clause.Column, error) { return column, nil }, copied, negate)
		return p.build(nil)
	}}
}

func JoinAll(predicates ...JoinPredicate) JoinPredicate { return joinGroup(false, predicates) }
func JoinAny(predicates ...JoinPredicate) JoinPredicate { return joinGroup(true, predicates) }

func joinGroup(or bool, predicates []JoinPredicate) JoinPredicate {
	ps := append([]JoinPredicate(nil), predicates...)
	return JoinPredicate{build: func(scope joinScope) (clause.Expression, error) {
		if len(ps) == 0 {
			return nil, fmt.Errorf("%w: empty join condition group", ErrInvalidQuery)
		}
		expressions := make([]clause.Expression, 0, len(ps))
		for _, p := range ps {
			if p.build == nil {
				return nil, ErrInvalidQuery
			}
			expr, err := p.build(scope)
			if err != nil {
				return nil, err
			}
			expressions = append(expressions, expr)
		}
		if or {
			return clause.Or(expressions...), nil
		}
		return clause.And(expressions...), nil
	}}
}

// JoinEqual compares two mapped columns, including nullable foreign keys.
// SQL type compatibility between the columns is the caller's responsibility.
func JoinEqual(left, right JoinField) JoinPredicate {
	return JoinPredicate{build: func(scope joinScope) (clause.Expression, error) {
		if nilJoinValue(left) || nilJoinValue(right) {
			return nil, ErrInvalidField
		}
		l, err := left.joinColumn().column(scope)
		if err != nil {
			return nil, err
		}
		r, err := right.joinColumn().column(scope)
		if err != nil {
			return nil, err
		}
		return clause.Eq{Column: l, Value: r}, nil
	}}
}

// JoinOn requires one equality linking the new table to an earlier table.
// Additional conditions are always ANDed with this link.
type JoinOn struct {
	left       JoinField
	right      JoinField
	predicates []JoinPredicate
}

func On(left, right JoinField) JoinOn { return JoinOn{left: left, right: right} }

func (on JoinOn) And(predicates ...JoinPredicate) JoinOn {
	on.predicates = append(append([]JoinPredicate(nil), on.predicates...), predicates...)
	return on
}

package gormx

import (
	"fmt"
	"reflect"
	"regexp"

	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

var joinIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)

// JoinTable is the model-bound table identity used by JoinQuery. Create one with
// TableOf.
//
// The identity is not a database handle: it names a model type and an alias
// inside one query. Reuse the same instance whenever referring to that table's
// columns; a separate instance with the same alias is a different identity and
// will not resolve against the query's scope.
type JoinTable interface {
	joinTable() *tableBinding
}

// tableBinding is the internal identity that joins and projections reference.
type tableBinding struct {
	alias string
	model reflect.Type
}

// TableRef is the concrete JoinTable returned by TableOf. It carries no data and
// only identifies a model type plus an alias within one query.
type TableRef[T any] struct {
	binding *tableBinding
}

// TableOf creates a table identity for model T under alias. It does not create
// or open a database table.
//
// The alias must match ^[A-Za-z_][A-Za-z0-9_]{0,62}$ and be unique within a
// query, compared case-insensitively. For a self join, create separate instances
// with different aliases, for example TableOf[User]("manager") and
// TableOf[User]("report").
func TableOf[T any](alias string) TableRef[T] {
	return TableRef[T]{binding: &tableBinding{
		alias: alias,
		model: reflect.TypeOf((*T)(nil)).Elem(),
	}}
}

func (t TableRef[T]) joinTable() *tableBinding { return t.binding }

// Col selects a column on this table by Go field name or database column name.
// Resolution is deferred: a zero TableRef or an unknown field fails with
// ErrInvalidField when the query compiles, and the returned JoinColumn[any]
// accepts untyped values.
func (t TableRef[T]) Col(name string) JoinColumn[any] {
	return Bind(t, Column[T, any]{ref: stringColumn[T](name)})
}

// Bind attaches an existing typed selector, usually created by Field, to a
// specific table instance, preserving the selector's value type.
//
// The column and table must refer to the same model type. Bind does not verify
// this at construction; a mismatch fails when the query compiles.
func Bind[T, V any](table TableRef[T], column Column[T, V]) JoinColumn[V] {
	ref := joinedColumn{table: table.binding}
	if column.ref != nil {
		ref.resolve = func(s *schema.Schema) (clause.Column, error) { return column.ref(s) }
	}
	return JoinColumn[V]{ref: ref}
}

// JoinField is the untyped view implemented by JoinColumn[V] for any V. It lets
// JoinEqual and JoinOn accept columns with different value types while the
// public JoinColumn API stays typed.
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

// JoinColumn is a table-bound selector that retains the value type V, so
// comparison methods accept only values of that type. Create it with
// TableRef.Col or Bind; the zero value is unbound and fails at compile time.
type JoinColumn[V any] struct {
	ref joinedColumn
}

func (c JoinColumn[V]) joinColumn() joinedColumn { return c.ref }

// Projection is one SELECT output. Its alias must be unique within the query,
// compared case-insensitively, and must match a readable database field name on
// the DTO passed to JoinList or JoinPage. Build it with JoinColumn.As.
type Projection struct {
	ref   joinedColumn
	alias string
}

// As names this column in the SELECT list. The alias is looked up as a DTO
// database field name, so it must match a readable field; it is never treated as
// a SQL expression and wildcards are not supported.
func (c JoinColumn[V]) As(alias string) Projection {
	return Projection{ref: c.ref, alias: alias}
}

// JoinOrder orders joined rows by one table-bound column.
type JoinOrder struct {
	ref  joinedColumn
	desc bool
}

// Asc orders ascending by this column. Ordering applies to JoinList and
// JoinPage; Count ignores it.
func (c JoinColumn[V]) Asc() JoinOrder { return JoinOrder{ref: c.ref} }

// Desc orders descending by this column. Ordering applies to JoinList and
// JoinPage; Count ignores it.
func (c JoinColumn[V]) Desc() JoinOrder { return JoinOrder{ref: c.ref, desc: true} }

// Eq requires the column to equal value (IS NULL when value is nil). V is
// checked at compile time; whether the database can compare V with the column
// type remains a schema concern.
func (c JoinColumn[V]) Eq(value V) JoinPredicate { return joinComparison(c.ref, "eq", value) }

// Ne requires the column to differ from value (IS NOT NULL when value is nil).
// NULL columns never match a non-nil Ne.
func (c JoinColumn[V]) Ne(value V) JoinPredicate { return joinComparison(c.ref, "ne", value) }

// Gt requires the column to be greater than value. NULL columns do not match.
func (c JoinColumn[V]) Gt(value V) JoinPredicate { return joinComparison(c.ref, "gt", value) }

// Ge requires the column to be greater than or equal to value. NULL columns do
// not match.
func (c JoinColumn[V]) Ge(value V) JoinPredicate { return joinComparison(c.ref, "ge", value) }

// Lt requires the column to be less than value. NULL columns do not match.
func (c JoinColumn[V]) Lt(value V) JoinPredicate { return joinComparison(c.ref, "lt", value) }

// Le requires the column to be less than or equal to value. NULL columns do not
// match.
func (c JoinColumn[V]) Le(value V) JoinPredicate { return joinComparison(c.ref, "le", value) }

// IsNull requires the column to be NULL.
func (c JoinColumn[V]) IsNull() JoinPredicate { return joinComparison(c.ref, "eq", nil) }

// IsNotNull requires the column to be non-NULL.
func (c JoinColumn[V]) IsNotNull() JoinPredicate { return joinComparison(c.ref, "ne", nil) }

// Like requires the column to match pattern using SQL LIKE. Wildcards % and _
// keep their meaning and pattern is bound as a parameter. The method exists for
// all V, so the caller must ensure the column is textual; the database decides.
func (c JoinColumn[V]) Like(pattern string) JoinPredicate {
	return joinComparison(c.ref, "like", pattern)
}

// NotLike requires the column not to match pattern, negating Like. NULL columns
// do not match.
func (c JoinColumn[V]) NotLike(pattern string) JoinPredicate {
	return joinComparison(c.ref, "not_like", pattern)
}

// Between requires lower <= column <= upper, with both bounds inclusive. Swapped
// bounds match no rows rather than returning an error.
func (c JoinColumn[V]) Between(lower, upper V) JoinPredicate {
	return JoinAll(c.Ge(lower), c.Le(upper))
}

// In requires the column to equal any of values. An empty list matches nothing.
func (c JoinColumn[V]) In(values ...V) JoinPredicate {
	return joinMembership(c.ref, values, false)
}

// NotIn requires the column to equal none of values. An empty list matches every
// row.
func (c JoinColumn[V]) NotIn(values ...V) JoinPredicate {
	return joinMembership(c.ref, values, true)
}

// JoinPredicate is an immutable condition scoped to join table instances.
//
// Predicates are built lazily, when the whole table scope is known, so referring
// to a table that was never joined fails with ErrInvalidField at execution. The
// zero value is invalid and fails when built.
type JoinPredicate struct {
	build func(joinScope) (clause.Expression, error)
}

// joinComparison binds a single-table comparison to a table-scoped column.
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

// JoinAll groups predicates with AND, copying the arguments. An empty group is
// rejected with ErrInvalidQuery when the query is built.
func JoinAll(predicates ...JoinPredicate) JoinPredicate { return joinGroup(false, predicates) }

// JoinAny groups predicates with OR, copying the arguments. An empty group is
// rejected with ErrInvalidQuery when the query is built.
func JoinAny(predicates ...JoinPredicate) JoinPredicate { return joinGroup(true, predicates) }

// joinGroup builds an AND/OR node and fails on an empty or invalid member list.
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

// JoinEqual compares two mapped columns, for example a foreign key with a
// primary key. Either side may be nullable.
//
// SQL type compatibility is not checked: comparing incompatible types is the
// caller's responsibility and may fail or produce surprising matches depending
// on the database. Nil fields return ErrInvalidField.
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

// JoinOn describes the ON clause of a join.
//
// It requires exactly one equality linking the newly added table to a table
// added earlier; the direction is validated during compilation, which prevents
// accidental cross joins. Additional predicates added with And are always ANDed
// with that link. Build it with On and extend it with And; the fields are private
// so the required link cannot be omitted.
type JoinOn struct {
	left       JoinField
	right      JoinField
	predicates []JoinPredicate
}

// On creates a join condition from the required equality link between a column
// of the new table and a column of an earlier table. Additional conditions can be
// added with JoinOn.And.
func On(left, right JoinField) JoinOn { return JoinOn{left: left, right: right} }

// And returns a copy with predicates ANDed to the required equality link. The
// receiver is not mutated, so an On value can be reused as a base. The added
// predicates must reference tables already in scope when the join compiles.
func (on JoinOn) And(predicates ...JoinPredicate) JoinOn {
	on.predicates = append(append([]JoinPredicate(nil), on.predicates...), predicates...)
	return on
}

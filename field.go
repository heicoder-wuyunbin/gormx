package gormx

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

// Column binds a model type T and a value type V. Comparison methods accept only
// values of type V, so mismatched literals are compile-time errors instead of
// runtime SQL surprises.
//
// Create columns with Field. The zero value is invalid: every method on it
// produces a predicate that fails at query compile time with ErrInvalidField.
// A Column can be reused across queries and goroutines because field resolution
// is cached per schema and only immutable clause values are returned.
type Column[T, V any] struct {
	ref columnRef[T]
}

// Field turns a field selector into a schema-validated column.
//
// The selector must return the address of a field GORM maps, for example
// Field(func(u *User) *int { return &u.Age }). Embedded fields, including their
// pointer form, and column tags are honored; resolution follows the model's
// naming strategy, unlike raw string field names.
//
// Selectors must be pure and stable: return the address of a real model field
// and do not inspect values, allocate, or return a pointer to anything outside
// the model. A nil selector, a nil result, a panic, or an address matching no
// mapped field yields ErrInvalidField when the query is compiled.
//
// Resolution is cached per *schema.Schema, so one Column may safely be used
// with several naming strategies. Field itself performs no database access and
// does not validate the selector eagerly.
func Field[T, V any](selector func(*T) *V) Column[T, V] {
	var cache sync.Map
	type resolution struct {
		name string
		err  error
	}
	return Column[T, V]{ref: func(s *schema.Schema) (clause.Column, error) {
		cached, ok := cache.Load(s)
		if !ok {
			name, err := resolveField(s, selector)
			cached, _ = cache.LoadOrStore(s, resolution{name: name, err: err})
		}
		result := cached.(resolution)
		return clause.Column{Table: clause.CurrentTable, Name: result.name}, result.err
	}}
}

// resolveField runs the selector against a fresh model value and maps the
// returned address back to its column. It initializes embedded pointer fields
// first, recovering panics into ErrInvalidField so a bad selector can never
// crash a query.
func resolveField[T, V any](s *schema.Schema, selector func(*T) *V) (name string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			name = ""
			err = fmt.Errorf("%w: field selector panicked: %v", ErrInvalidField, recovered)
		}
	}()
	if selector == nil {
		return "", fmt.Errorf("%w: nil field selector", ErrInvalidField)
	}
	model := new(T)
	rv := reflect.ValueOf(model)
	// GORM initializes nil embedded pointers as it resolves their fields.
	for _, f := range s.Fields {
		if f.DBName != "" {
			f.ReflectValueOf(context.Background(), rv)
		}
	}
	selected := selector(model)
	if selected == nil {
		return "", fmt.Errorf("%w: selector returned nil", ErrInvalidField)
	}
	for _, f := range s.Fields {
		if f.DBName == "" {
			continue
		}
		value := f.ReflectValueOf(context.Background(), rv)
		if value.CanAddr() && value.Addr().CanInterface() && value.Addr().Interface() == any(selected) {
			return f.DBName, nil
		}
	}
	return "", fmt.Errorf("%w: selector must return the address of a mapped field", ErrInvalidField)
}

// Eq requires the column to equal value, producing column = value. Passing nil
// produces IS NULL; prefer IsNull for readability. The predicate is restrictive,
// so it can authorize Update and Delete.
func (c Column[T, V]) Eq(value V) Predicate[T] {
	return comparison(c.ref, "eq", value)
}

// Ne requires the column to differ from value, producing column <> value.
// Passing nil produces IS NOT NULL, which also excludes NULL rows; prefer
// IsNotNull for readability. Rows whose column is NULL never match a non-nil
// Ne, because SQL treats NULL comparisons as unknown.
func (c Column[T, V]) Ne(value V) Predicate[T] {
	return comparison(c.ref, "ne", value)
}

// Gt requires the column to be greater than value. Rows with a NULL column do
// not match. The value type V is checked at compile time; whether the database
// can compare V with the column type remains the schema's responsibility.
func (c Column[T, V]) Gt(value V) Predicate[T] {
	return comparison(c.ref, "gt", value)
}

// Ge requires the column to be greater than or equal to value. Rows with a
// NULL column do not match.
func (c Column[T, V]) Ge(value V) Predicate[T] {
	return comparison(c.ref, "ge", value)
}

// Lt requires the column to be less than value. Rows with a NULL column do not
// match.
func (c Column[T, V]) Lt(value V) Predicate[T] {
	return comparison(c.ref, "lt", value)
}

// Le requires the column to be less than or equal to value. Rows with a NULL
// column do not match.
func (c Column[T, V]) Le(value V) Predicate[T] {
	return comparison(c.ref, "le", value)
}

// Between requires lower <= column <= upper, with both bounds inclusive. It is
// built as Ge(lower) AND Le(upper); swapped bounds simply match no rows rather
// than returning an error.
func (c Column[T, V]) Between(lower, upper V) Predicate[T] {
	return All(c.Ge(lower), c.Le(upper))
}

// In requires the column to equal any of values, producing IN (...). An empty
// list compiles to a contradiction (1 = 0) that matches nothing and still
// counts as restrictive, so it is safe for writes.
func (c Column[T, V]) In(values ...V) Predicate[T] {
	return membership(c.ref, values, false)
}

// NotIn requires the column to equal none of values, producing NOT IN (...). An
// empty list compiles to a tautology (1 = 1) that matches every row and is NOT
// restrictive, so an Update or Delete whose only condition is an empty NotIn is
// rejected with ErrUnsafeWrite.
func (c Column[T, V]) NotIn(values ...V) Predicate[T] {
	return membership(c.ref, values, true)
}

// IsNull requires the column to be NULL. It is equivalent to Eq with a nil
// value and is restrictive.
func (c Column[T, V]) IsNull() Predicate[T] { return comparison(c.ref, "eq", nil) }

// IsNotNull requires the column to be non-NULL. It is equivalent to Ne with a
// nil value and is restrictive.
func (c Column[T, V]) IsNotNull() Predicate[T] { return comparison(c.ref, "ne", nil) }

// Asc orders by this column ascending. Ordering applies to First, List and
// SelectPage; Count, Update and Delete ignore it. The column is still validated
// when the query compiles.
func (c Column[T, V]) Asc() Order[T] { return Order[T]{ref: c.ref} }

// Desc orders by this column descending. Ordering applies to First, List and
// SelectPage; Count, Update and Delete ignore it.
func (c Column[T, V]) Desc() Order[T] { return Order[T]{ref: c.ref, desc: true} }

// Like requires a string-valued column to match pattern, producing LIKE pattern.
//
// The type restriction is enforced at compile time: only Column[T, string] can
// be passed. Wildcards % and _ in pattern keep their SQL meaning and the value
// is bound as a parameter, not interpolated. Escape user-supplied wildcards when
// a literal match is intended; case sensitivity follows the database collation.
// Like is a package function rather than a Column method because it must narrow
// the value type to string.
func Like[T any](column Column[T, string], pattern string) Predicate[T] {
	return comparison(column.ref, "like", pattern)
}

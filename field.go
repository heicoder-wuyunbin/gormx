package gormx

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

// Column binds both model T and value V. Eq/In/Between enforce V at compile time.
// The zero value is invalid.
type Column[T, V any] struct {
	ref columnRef[T]
}

// Field selects an actual model field, e.g. Field(func(u *User) *int { return &u.Age }).
// Selectors must be pure; never inspect values or return pointers outside the model.
// Schema resolution honors column tags, embedded fields and the DB naming strategy.
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

func (c Column[T, V]) Eq(value V) Predicate[T] {
	return comparison(c.ref, "eq", value)
}

func (c Column[T, V]) Ne(value V) Predicate[T] {
	return comparison(c.ref, "ne", value)
}

func (c Column[T, V]) Gt(value V) Predicate[T] {
	return comparison(c.ref, "gt", value)
}

func (c Column[T, V]) Ge(value V) Predicate[T] {
	return comparison(c.ref, "ge", value)
}

func (c Column[T, V]) Lt(value V) Predicate[T] {
	return comparison(c.ref, "lt", value)
}

func (c Column[T, V]) Le(value V) Predicate[T] {
	return comparison(c.ref, "le", value)
}

func (c Column[T, V]) Between(lower, upper V) Predicate[T] {
	return All(c.Ge(lower), c.Le(upper))
}

func (c Column[T, V]) In(values ...V) Predicate[T] {
	return membership(c.ref, values, false)
}

func (c Column[T, V]) NotIn(values ...V) Predicate[T] {
	return membership(c.ref, values, true)
}

func (c Column[T, V]) IsNull() Predicate[T] { return comparison(c.ref, "eq", nil) }

func (c Column[T, V]) IsNotNull() Predicate[T] { return comparison(c.ref, "ne", nil) }

func (c Column[T, V]) Asc() Order[T] { return Order[T]{ref: c.ref} }

func (c Column[T, V]) Desc() Order[T] { return Order[T]{ref: c.ref, desc: true} }

// Like only accepts a string-valued Column, unlike comparison methods.
// SQL wildcard characters in pattern retain their usual meaning.
func Like[T any](column Column[T, string], pattern string) Predicate[T] {
	return comparison(column.ref, "like", pattern)
}

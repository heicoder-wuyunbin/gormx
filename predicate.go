package gormx

import (
	"errors"
	"fmt"
	"reflect"

	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

var (
	ErrInvalidField = errors.New("gormx: invalid model field")
	ErrInvalidQuery = errors.New("gormx: invalid query")
	ErrUnsafeWrite  = errors.New("gormx: write requires a restrictive condition")
	ErrInvalidPage  = errors.New("gormx: invalid pagination")
	ErrInvalidModel = errors.New("gormx: model must be a non-pointer struct")
	ErrInvalidID    = errors.New("gormx: invalid primary key")
)

type columnRef[T any] func(*schema.Schema) (clause.Column, error)

// Predicate binds a condition to its model type. Its zero value is invalid.
type Predicate[T any] struct {
	build func(*schema.Schema) (clause.Expression, error)
	// restrictive tracks obvious tautologies, so NotIn(empty) cannot enable a full-table write.
	restrictive bool
}

func stringColumn[T any](name string) columnRef[T] {
	return func(s *schema.Schema) (clause.Column, error) {
		field := s.LookUpField(name)
		if field == nil || field.DBName == "" {
			return clause.Column{}, fmt.Errorf("%w: %q", ErrInvalidField, name)
		}
		return clause.Column{Table: clause.CurrentTable, Name: field.DBName}, nil
	}
}

func comparison[T any](ref columnRef[T], op string, value any) Predicate[T] {
	return Predicate[T]{
		restrictive: true,
		build: func(s *schema.Schema) (clause.Expression, error) {
			if ref == nil {
				return nil, ErrInvalidField
			}
			col, err := ref(s)
			if err != nil {
				return nil, err
			}
			switch op {
			case "eq":
				return clause.Eq{Column: col, Value: value}, nil
			case "ne":
				return clause.Neq{Column: col, Value: value}, nil
			case "gt":
				return clause.Gt{Column: col, Value: value}, nil
			case "ge":
				return clause.Gte{Column: col, Value: value}, nil
			case "lt":
				return clause.Lt{Column: col, Value: value}, nil
			case "le":
				return clause.Lte{Column: col, Value: value}, nil
			case "like":
				return clause.Like{Column: col, Value: value}, nil
			case "not_like":
				return clause.Not(clause.Like{Column: col, Value: value}), nil
			default:
				return nil, ErrInvalidQuery
			}
		},
	}
}

func membership[T any](ref columnRef[T], values any, negate bool) Predicate[T] {
	rv := reflect.ValueOf(values)
	var copied []any
	var invalid bool
	if rv.IsValid() && (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array) {
		copied = make([]any, rv.Len())
		for i := range copied {
			copied[i] = rv.Index(i).Interface()
		}
	} else if values != nil {
		invalid = true
	}
	return Predicate[T]{
		restrictive: !negate || len(copied) > 0,
		build: func(s *schema.Schema) (clause.Expression, error) {
			if invalid {
				return nil, fmt.Errorf("%w: IN expects a slice or array", ErrInvalidQuery)
			}
			if ref == nil {
				return nil, ErrInvalidField
			}
			col, err := ref(s)
			if err != nil {
				return nil, err
			}
			if len(copied) == 0 {
				if negate {
					return clause.Expr{SQL: "1 = 1"}, nil
				}
				return clause.Expr{SQL: "1 = 0"}, nil
			}
			expr := clause.IN{Column: col, Values: copied}
			if negate {
				return clause.Not(expr), nil
			}
			return expr, nil
		},
	}
}

// All groups conditions with AND. An empty group is rejected.
func All[T any](predicates ...Predicate[T]) Predicate[T] {
	return group(false, predicates)
}

// Any groups conditions with OR. An empty group is rejected.
func Any[T any](predicates ...Predicate[T]) Predicate[T] {
	return group(true, predicates)
}

func group[T any](or bool, predicates []Predicate[T]) Predicate[T] {
	ps := append([]Predicate[T](nil), predicates...)
	restrictive := or && len(ps) > 0
	for _, p := range ps {
		if or {
			restrictive = restrictive && p.restrictive
		} else {
			restrictive = restrictive || p.restrictive
		}
	}
	return Predicate[T]{
		restrictive: restrictive,
		build: func(s *schema.Schema) (clause.Expression, error) {
			if len(ps) == 0 {
				return nil, fmt.Errorf("%w: empty condition group", ErrInvalidQuery)
			}
			exprs := make([]clause.Expression, 0, len(ps))
			for _, p := range ps {
				if p.build == nil {
					return nil, ErrInvalidQuery
				}
				expr, err := p.build(s)
				if err != nil {
					return nil, err
				}
				exprs = append(exprs, expr)
			}
			if or {
				return clause.Or(exprs...), nil
			}
			return clause.And(exprs...), nil
		},
	}
}

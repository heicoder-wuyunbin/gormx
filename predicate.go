package gormx

import (
	"errors"
	"fmt"
	"reflect"

	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

var (
	// ErrInvalidField reports an unknown, unmapped or unusable model field. It
	// also covers selectors that do not return the address of a mapped field,
	// primary keys passed to Update, and unbound or out-of-scope join columns.
	// Match it with errors.Is.
	ErrInvalidField = errors.New("gormx: invalid model field")
	// ErrInvalidQuery reports a malformed query: an empty condition group, a
	// join whose ON clause does not link a new table to an earlier one, duplicate
	// aliases, an unsupported model query clause, or an attempt to bypass the
	// typed API with raw SQL.
	ErrInvalidQuery = errors.New("gormx: invalid query")
	// ErrUnsafeWrite reports an Update or Delete whose conditions contain no
	// restrictive predicate, so the statement could affect the whole table.
	ErrUnsafeWrite = errors.New("gormx: write requires a restrictive condition")
	// ErrInvalidPage reports pagination input rejected by PageRequest.normalize:
	// negative values, Size above MaxPageSize, or an offset that overflows int.
	ErrInvalidPage = errors.New("gormx: invalid pagination")
	// ErrInvalidModel reports a model that is not a non-pointer struct, a nil
	// join table, or a DTO without the required struct shape.
	ErrInvalidModel = errors.New("gormx: model must be a non-pointer struct")
	// ErrInvalidID reports an invalid primary key for the ByID methods: nil,
	// empty, unsupported type, or a malformed numeric string.
	ErrInvalidID = errors.New("gormx: invalid primary key")
)

// columnRef lazily resolves a clause column against a parsed schema. It is the
// shared indirection that lets predicates remain schema-independent until
// execution.
type columnRef[T any] func(*schema.Schema) (clause.Column, error)

// Predicate is an immutable, lazily-built condition bound to model T.
//
// Predicates are not compiled at construction; they are resolved against the
// schema when a DAO executes the query. This lets one predicate be reused across
// handles with different naming strategies. The zero value is invalid: passing
// one to Where, All or Any makes compilation fail with ErrInvalidQuery.
//
// restrictive marks conditions that cannot be a tautology. Update and Delete
// require at least one restrictive top-level predicate so expressions such as
// NotIn(empty) cannot silently become a full-table write. Comparisons and IsNull
// are always restrictive; In(empty) is a contradiction and stays restrictive;
// NotIn(empty) is a tautology and is not restrictive. An OR group is restrictive
// only when it has members and every member is restrictive.
type Predicate[T any] struct {
	build func(*schema.Schema) (clause.Expression, error)
	// restrictive tracks obvious tautologies, so NotIn(empty) cannot enable a full-table write.
	restrictive bool
}

// stringColumn resolves a Go field name or database column name against the
// schema and always targets the current table. Unknown or unmapped names return
// ErrInvalidField when the predicate is built.
func stringColumn[T any](name string) columnRef[T] {
	return func(s *schema.Schema) (clause.Column, error) {
		field := s.LookUpField(name)
		if field == nil || field.DBName == "" {
			return clause.Column{}, fmt.Errorf("%w: %q", ErrInvalidField, name)
		}
		return clause.Column{Table: clause.CurrentTable, Name: field.DBName}, nil
	}
}

// comparison builds a lazy, always-restrictive comparison. A nil ref or an
// unknown operator surfaces as ErrInvalidField/ErrInvalidQuery at build time.
// A nil value produces IS NULL for "eq" and IS NOT NULL for "ne".
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

// membership builds a lazy IN/NOT IN predicate. Values are copied at
// construction so later caller mutation cannot change the query. An empty list
// compiles to 1 = 0 for IN (matches nothing, restrictive) and 1 = 1 for NOT IN
// (matches everything, non-restrictive). A non-slice, non-nil value is rejected
// at build time.
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

// All groups predicates with AND, copying the arguments.
//
// An empty group is rejected with ErrInvalidQuery when the query is built. The
// group is restrictive if any member is restrictive, because an AND can only
// narrow the result set.
func All[T any](predicates ...Predicate[T]) Predicate[T] {
	return group(false, predicates)
}

// Any groups predicates with OR, copying the arguments.
//
// An empty group is rejected with ErrInvalidQuery when the query is built. The
// group is restrictive only when it has members and every member is
// restrictive, because a single tautological branch would make the whole OR
// match everything.
func Any[T any](predicates ...Predicate[T]) Predicate[T] {
	return group(true, predicates)
}

// group builds an AND/OR node and derives its restrictive flag from the members.
// The zero value of a member (a nil build function) fails when the group builds.
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

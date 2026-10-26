package gormx

import (
	"context"
	"fmt"
	"reflect"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// FillRule supplies a context-aware value for one field on write statements.
//
// A rule may target Create, Update or both. Unmapped fields are skipped
// silently, so one audit rule set can be shared across heterogeneous models;
// primary keys, read-only fields and columns excluded by GORM's Select/Omit are
// skipped too. With Overwrite=false, values already supplied by the caller are
// preserved: an explicit map key (even a zero value) and a non-zero struct
// field both win over the rule.
type FillRule struct {
	// Field is the Go field name or database column name to fill. It must be
	// mapped on the model; unmapped names are ignored rather than treated as
	// errors.
	Field string
	// OnCreate enables the rule for Create and CreateBatch.
	OnCreate bool
	// OnUpdate enables the rule for Update and Updates.
	OnUpdate bool
	// Overwrite replaces values the caller already supplied. When false, an
	// explicit map entry (including false, 0 and "") and a non-zero struct field
	// are left untouched.
	Overwrite bool
	// Value produces the field value lazily. It is called at most once per rule
	// per write statement, and the same value is applied to every entity in a
	// batch. The context is the statement context, so cancellation and deadlines
	// propagate. Returning an error aborts the write and is surfaced through the
	// GORM statement error.
	Value func(context.Context) (any, error)
}

// FillPlugin applies FillRule values to Create and Update statements.
//
// Create the plugin with NewFillPlugin and install it once per connection using
// db.Use. The plugin keeps its own copy of the rules, is immutable after
// construction, and may serve concurrent statements once registration has
// completed.
type FillPlugin struct {
	rules []FillRule
}

// NewFillPlugin copies rules and returns a plugin ready for db.Use. Copying
// isolates the plugin from later mutation of the caller's slice. Rules are
// validated by Initialize, not here, so construction itself cannot fail.
func NewFillPlugin(rules ...FillRule) *FillPlugin {
	return &FillPlugin{rules: append([]FillRule(nil), rules...)}
}

// Name returns the GORM plugin name "gormx:fill". GORM uses it to reject a
// duplicate registration, so install only one instance per handle.
func (p *FillPlugin) Name() string { return "gormx:fill" }

// Initialize validates the rules and registers the create and update callbacks.
//
// Create filling runs before gorm:before_create, and update filling runs after
// gorm:setup_reflect_value but before gorm:before_update, so model hooks observe
// the filled audit values. A rule without Field, without Value, or with neither
// OnCreate nor OnUpdate aborts installation with an error.
//
// Install once during startup, before serving concurrent requests: GORM
// callback registration is not meant to be mutated while queries are running,
// and the same plugin name cannot be registered twice.
func (p *FillPlugin) Initialize(db *gorm.DB) error {
	for _, rule := range p.rules {
		if rule.Field == "" || rule.Value == nil || (!rule.OnCreate && !rule.OnUpdate) {
			return fmt.Errorf("gormx: invalid fill rule for %q", rule.Field)
		}
	}
	if err := db.Callback().Create().Before("gorm:before_create").
		Register("gormx:fill_create", func(tx *gorm.DB) { p.fill(tx, true) }); err != nil {
		return err
	}
	return db.Callback().Update().After("gorm:setup_reflect_value").Before("gorm:before_update").
		Register("gormx:fill_update", func(tx *gorm.DB) { p.fill(tx, false) })
}

// fill applies every rule eligible for the current statement. Skipped
// statements (errors, no schema, SkipHooks) and excluded columns are ignored;
// the first destination failure aborts filling and is recorded on tx.Error so
// the write fails instead of proceeding with partial values.
func (p *FillPlugin) fill(tx *gorm.DB, create bool) {
	stmt := tx.Statement
	if tx.Error != nil || stmt.Schema == nil || stmt.SkipHooks {
		return
	}
	selected, restricted := stmt.SelectAndOmitColumns(create, !create)
	for _, rule := range p.rules {
		if (create && !rule.OnCreate) || (!create && !rule.OnUpdate) {
			continue
		}
		field := stmt.Schema.LookUpField(rule.Field)
		if field == nil || field.DBName == "" || field.PrimaryKey {
			continue
		}
		if (create && !field.Creatable) || (!create && !field.Updatable) {
			continue
		}
		if allowed, exists := selected[field.DBName]; (exists && !allowed) || (!exists && restricted) {
			continue
		}
		var value any
		var resolved bool
		resolve := func() (any, error) {
			if !resolved {
				var err error
				value, err = rule.Value(stmt.Context)
				if err != nil {
					return nil, err
				}
				resolved = true
			}
			return value, nil
		}
		if err := fillDestination(stmt.Context, reflect.ValueOf(stmt.Dest), stmt.Schema.ModelType, field, rule.Overwrite, resolve); err != nil {
			tx.AddError(fmt.Errorf("gormx: fill %s: %w", rule.Field, err))
			return
		}
	}
}

// fillDestination writes the resolved value into a struct, into every element
// of a batch slice or array, or into a map[string]any keyed by column name.
// Unsupported destinations fail with gorm.ErrInvalidData; when overwrite is
// false, an existing map entry or non-zero struct field is left untouched.
func fillDestination(ctx context.Context, rv reflect.Value, modelType reflect.Type, field *schema.Field, overwrite bool, resolve func() (any, error)) error {
	for rv.IsValid() && (rv.Kind() == reflect.Ptr || rv.Kind() == reflect.Interface) {
		if rv.IsNil() {
			return gorm.ErrInvalidValue
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() {
		return gorm.ErrInvalidValue
	}
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			if err := fillDestination(ctx, rv.Index(i), modelType, field, overwrite, resolve); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		dest, ok := rv.Interface().(map[string]any)
		if !ok || dest == nil {
			return gorm.ErrInvalidData
		}
		_, named := dest[field.Name]
		_, column := dest[field.DBName]
		if !overwrite && (named || column) {
			return nil
		}
		value, err := resolve()
		if err != nil {
			return err
		}
		delete(dest, field.Name)
		dest[field.DBName] = value
		return nil
	case reflect.Struct:
		if !rv.CanAddr() || rv.Type() != modelType {
			return gorm.ErrInvalidData
		}
		if _, zero := field.ValueOf(ctx, rv); !overwrite && !zero {
			return nil
		}
		value, err := resolve()
		if err != nil {
			return err
		}
		return field.Set(ctx, rv, value)
	default:
		return gorm.ErrInvalidData
	}
}

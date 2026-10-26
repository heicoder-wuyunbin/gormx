package gormx

import (
	"context"
	"fmt"
	"reflect"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// FillRule supplies a context-aware value once per write statement.
// Unmapped fields are skipped, allowing shared audit rules across models.
// Overwrite=false preserves supplied map keys (including zero values), and
// nonzero struct fields. GORM's Select/Omit and field permissions are respected.
type FillRule struct {
	Field     string
	OnCreate  bool
	OnUpdate  bool
	Overwrite bool
	Value     func(context.Context) (any, error)
}

type FillPlugin struct {
	rules []FillRule
}

func NewFillPlugin(rules ...FillRule) *FillPlugin {
	return &FillPlugin{rules: append([]FillRule(nil), rules...)}
}

func (p *FillPlugin) Name() string { return "gormx:fill" }

// Initialize registers before model hooks so hooks observe filled audit fields.
// Install once with db.Use before serving concurrent requests.
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

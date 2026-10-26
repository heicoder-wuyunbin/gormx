// Package gormx adds immutable queries, typed field selectors and generic CRUD to GORM.
package gormx

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

// DAO must be constructed from a root DB or transaction handle.
// NewDAO clears pre-existing query clauses; put filters in a Wrapper.
// Connection, transaction, plugins and naming strategy are retained.
type DAO[T any] struct {
	db     *gorm.DB
	schema *schema.Schema
}

func NewDAO[T any](db *gorm.DB) (*DAO[T], error) {
	typ := reflect.TypeOf((*T)(nil)).Elem()
	if typ.Kind() != reflect.Struct {
		return nil, ErrInvalidModel
	}
	if db == nil {
		return nil, errors.New("gormx: nil database")
	}
	if db.Error != nil {
		return nil, db.Error
	}
	clean := db.Session(&gorm.Session{NewDB: true}).Model(new(T))
	stmt := &gorm.Statement{DB: clean}
	if err := stmt.Parse(new(T)); err != nil {
		return nil, err
	}
	return &DAO[T]{db: clean, schema: stmt.Schema}, nil
}

func (d *DAO[T]) base(ctx context.Context) *gorm.DB {
	// GORM writes assignments back into Model; a shared model pointer would race.
	return d.db.Session(&gorm.Session{}).WithContext(ctx).Model(new(T))
}

func (d *DAO[T]) query(ctx context.Context, w *Wrapper[T], ordering bool) (*gorm.DB, querySpec, error) {
	spec, err := w.compile(d.schema)
	if err != nil {
		return nil, spec, err
	}
	return spec.apply(d.base(ctx), ordering), spec, nil
}

func (d *DAO[T]) Create(ctx context.Context, entity *T) error {
	if entity == nil {
		return errors.New("gormx: nil entity")
	}
	return d.base(ctx).Create(entity).Error
}

// CreateBatch populates generated IDs in entities. GORM's default write
// transaction makes the batches atomic unless SkipDefaultTransaction is enabled.
func (d *DAO[T]) CreateBatch(ctx context.Context, entities []T, batchSize int) error {
	if batchSize <= 0 {
		return errors.New("gormx: batch size must be positive")
	}
	if len(entities) == 0 {
		return nil
	}
	return d.base(ctx).CreateInBatches(&entities, batchSize).Error
}

func (d *DAO[T]) primaryQuery(id any) (*Wrapper[T], error) {
	if len(d.schema.PrimaryFields) != 1 {
		return nil, errors.New("gormx: ByID methods require exactly one primary key; use a Wrapper")
	}
	if id == nil {
		return nil, ErrInvalidID
	}
	field := d.schema.PrimaryFields[0]
	value := reflect.ValueOf(id)
	var text string
	switch value.Kind() {
	case reflect.String:
		text = value.String()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		text = strconv.FormatInt(value.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		text = strconv.FormatUint(value.Uint(), 10)
	default:
		return nil, ErrInvalidID
	}
	if text == "" {
		return nil, ErrInvalidID
	}
	// Avoid MySQL coercing malformed numeric strings into a matching integer ID.
	switch field.DataType {
	case schema.Int:
		parsed, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidID, err)
		}
		id = parsed
	case schema.Uint:
		parsed, err := strconv.ParseUint(text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidID, err)
		}
		id = parsed
	case schema.String:
		id = text
	default:
		return nil, fmt.Errorf("%w: ByID supports integer and string keys; use a Wrapper", ErrInvalidID)
	}
	return NewWrapper[T]().Eq(field.DBName, id), nil
}

func (d *DAO[T]) GetByID(ctx context.Context, id any) (*T, error) {
	w, err := d.primaryQuery(id)
	if err != nil {
		return nil, err
	}
	return d.First(ctx, w)
}

// First preserves gorm.ErrRecordNotFound for errors.Is checks.
func (d *DAO[T]) First(ctx context.Context, w *Wrapper[T]) (*T, error) {
	db, _, err := d.query(ctx, w, true)
	if err != nil {
		return nil, err
	}
	var entity T
	if err := db.First(&entity).Error; err != nil {
		return nil, err
	}
	return &entity, nil
}

// List has no implicit limit. Use SelectPage for request-facing list endpoints.
func (d *DAO[T]) List(ctx context.Context, w *Wrapper[T]) ([]T, error) {
	db, _, err := d.query(ctx, w, true)
	if err != nil {
		return nil, err
	}
	entities := make([]T, 0)
	err = db.Find(&entities).Error
	return entities, err
}

func (d *DAO[T]) Count(ctx context.Context, w *Wrapper[T]) (int64, error) {
	db, _, err := d.query(ctx, w, false)
	if err != nil {
		return 0, err
	}
	var total int64
	err = db.Count(&total).Error
	return total, err
}

// SelectPage always performs COUNT followed by a paginated SELECT on success.
// Two independent statements prevent count, limit and order clause leakage.
// Use a repeatable-read transaction when both statements need a shared snapshot.
func (d *DAO[T]) SelectPage(ctx context.Context, w *Wrapper[T], request PageRequest) (*Page[T], error) {
	r, offset, err := request.normalize()
	if err != nil {
		return nil, err
	}
	spec, err := w.compile(d.schema)
	if err != nil {
		return nil, err
	}
	page := &Page[T]{Records: make([]T, 0), Current: r.Current, Size: r.Size}
	if err := spec.apply(d.base(ctx), false).Count(&page.Total).Error; err != nil {
		return nil, err
	}
	page.Pages = page.Total / int64(r.Size)
	if page.Total%int64(r.Size) != 0 {
		page.Pages++
	}
	// Append primary keys as tie-breakers for stable offset pagination.
	for _, field := range d.schema.PrimaryFields {
		found := false
		for _, order := range spec.orders {
			found = found || order.Column.Name == field.DBName
		}
		if !found {
			spec.orders = append(spec.orders, clause.OrderByColumn{
				Column: clause.Column{Table: clause.CurrentTable, Name: field.DBName},
			})
		}
	}
	db := spec.apply(d.base(ctx), true).Limit(r.Size).Offset(offset)
	if err := db.Find(&page.Records).Error; err != nil {
		return nil, err
	}
	return page, nil
}

// Update uses a map so false, 0 and empty strings are not silently skipped.
// Fields are schema-validated; primary keys and non-updatable fields are rejected.
func (d *DAO[T]) Update(ctx context.Context, w *Wrapper[T], changes map[string]any) (int64, error) {
	db, spec, err := d.query(ctx, w, false)
	if err != nil {
		return 0, err
	}
	if !spec.restrictive {
		return 0, ErrUnsafeWrite
	}
	if len(changes) == 0 {
		return 0, errors.New("gormx: empty update")
	}
	normalized := make(map[string]any, len(changes))
	for name, value := range changes {
		field := d.schema.LookUpField(name)
		if field == nil || field.DBName == "" || !field.Updatable || field.PrimaryKey {
			return 0, fmt.Errorf("%w: field %q cannot be updated", ErrInvalidField, name)
		}
		if _, exists := normalized[field.DBName]; exists {
			return 0, fmt.Errorf("%w: duplicate update field %q", ErrInvalidField, name)
		}
		normalized[field.DBName] = value
	}
	result := db.Updates(normalized)
	return result.RowsAffected, result.Error
}

func (d *DAO[T]) UpdateByID(ctx context.Context, id any, changes map[string]any) (int64, error) {
	w, err := d.primaryQuery(id)
	if err != nil {
		return 0, err
	}
	return d.Update(ctx, w, changes)
}

// Delete respects gorm.DeletedAt. Models without it are physically deleted.
// Logical deletion is supplied by GORM itself, not a second competing callback.
func (d *DAO[T]) Delete(ctx context.Context, w *Wrapper[T]) (int64, error) {
	db, spec, err := d.query(ctx, w, false)
	if err != nil {
		return 0, err
	}
	if !spec.restrictive {
		return 0, ErrUnsafeWrite
	}
	result := db.Delete(new(T))
	return result.RowsAffected, result.Error
}

func (d *DAO[T]) DeleteByID(ctx context.Context, id any) (int64, error) {
	w, err := d.primaryQuery(id)
	if err != nil {
		return 0, err
	}
	return d.Delete(ctx, w)
}

// Transaction binds all operations in fn to the same transaction.
// Cross-model transactions can use NewDAO[Other](tx) inside db.Transaction.
func (d *DAO[T]) Transaction(ctx context.Context, fn func(*DAO[T]) error) error {
	if fn == nil {
		return errors.New("gormx: nil transaction callback")
	}
	return d.base(ctx).Transaction(func(tx *gorm.DB) error {
		dao, err := NewDAO[T](tx)
		if err != nil {
			return err
		}
		return fn(dao)
	})
}

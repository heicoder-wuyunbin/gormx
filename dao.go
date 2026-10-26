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

// DAO is a generic CRUD facade bound to one model type T.
//
// Construct it with NewDAO; the zero value is unusable. A DAO is safe for
// concurrent use to the extent the underlying *gorm.DB is: every method
// derives a fresh session and a fresh model value instead of mutating shared
// state.
//
// Pre-existing clauses and Scopes on the supplied handle are discarded.
// Express filters through Wrapper[T] so they are schema-validated and clearly
// separate reads from writes. Connection settings, the transaction (if any),
// plugins and naming strategy are retained.
type DAO[T any] struct {
	db     *gorm.DB
	schema *schema.Schema
}

// NewDAO binds model T to db and prepares a reusable CRUD facade.
//
// T must be a non-pointer struct; pointer, interface, slice, map and scalar
// types return ErrInvalidModel. A nil db, or a db that already carries an
// error, is rejected before anything is executed.
//
// db may be a root handle or a transaction handle. The connection, transaction
// state, plugins, logger and naming strategy are retained, but every pending
// query clause (Where, Order, Limit, Select, Scopes, ...) is cleared by
// starting from gorm.Session{NewDB: true}. Put conditions in a Wrapper[T]
// instead: otherwise they are silently dropped and may turn a filtered read or
// write into a full-table operation.
//
// Schema parsing happens once here, so tag and mapping errors surface
// immediately rather than on first use. The returned DAO can be shared between
// goroutines; sessions are created per call.
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

// base derives a fresh session with a fresh model value. GORM writes
// reflections back into Model during queries, so sharing a model pointer would
// race between concurrent calls. Do not cache the returned *gorm.DB.
func (d *DAO[T]) base(ctx context.Context) *gorm.DB {
	// GORM writes assignments back into Model; a shared model pointer would race.
	return d.db.Session(&gorm.Session{}).WithContext(ctx).Model(new(T))
}

// query compiles w against the DAO schema and applies its predicates.
// ordering controls whether ORDER BY is included; count and write paths pass
// false so stale ordering cannot leak into those statements.
func (d *DAO[T]) query(ctx context.Context, w *Wrapper[T], ordering bool) (*gorm.DB, querySpec, error) {
	spec, err := w.compile(d.schema)
	if err != nil {
		return nil, spec, err
	}
	return spec.apply(d.base(ctx), ordering), spec, nil
}

// Create inserts one entity and writes database-generated values (primary key,
// defaults, timestamps) back into it. A nil entity is rejected before any SQL
// is sent.
//
// Create and CreateBatch are the only writes that need no condition: they
// cannot affect existing rows. GORM hooks, field permissions and the fill
// plugin, if installed, run normally. Unless SkipDefaultTransaction is enabled
// on the handle, GORM wraps the insert in its own transaction.
func (d *DAO[T]) Create(ctx context.Context, entity *T) error {
	if entity == nil {
		return errors.New("gormx: nil entity")
	}
	return d.base(ctx).Create(entity).Error
}

// CreateBatch inserts entities in chunks of batchSize and populates the
// generated IDs on the original slice. batchSize must be positive; a nil or
// empty slice is a successful no-op.
//
// GORM's default write transaction makes the whole batch atomic unless
// SkipDefaultTransaction is enabled, in which case an error in a later chunk
// leaves earlier chunks committed. Hooks, field permissions and the fill
// plugin run as for Create.
func (d *DAO[T]) CreateBatch(ctx context.Context, entities []T, batchSize int) error {
	if batchSize <= 0 {
		return errors.New("gormx: batch size must be positive")
	}
	if len(entities) == 0 {
		return nil
	}
	return d.base(ctx).CreateInBatches(&entities, batchSize).Error
}

// primaryQuery validates a ByID key and builds the equivalent equality wrapper.
// It requires exactly one primary key, and only integer or string key types are
// supported; everything else returns ErrInvalidID with a hint to use a Wrapper.
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

// GetByID returns the row whose single primary key equals id, or
// gorm.ErrRecordNotFound when no row matches.
//
// The model must have exactly one primary key; composite keys return an error
// directing the caller to use a Wrapper. id may be a string, or any signed or
// unsigned integer type, or a string containing one. Numeric strings are parsed
// before reaching SQL so a malformed value cannot be coerced by the database
// into an unrelated key. nil, empty strings, bools and unsupported key types
// return ErrInvalidID.
func (d *DAO[T]) GetByID(ctx context.Context, id any) (*T, error) {
	w, err := d.primaryQuery(id)
	if err != nil {
		return nil, err
	}
	return d.First(ctx, w)
}

// First returns the first matching row, or gorm.ErrRecordNotFound when nothing
// matches. gorm.ErrRecordNotFound is preserved unchanged for errors.Is checks;
// it is never converted into a nil result.
//
// Filters and ordering come from w. Without an explicit Order, GORM orders by
// the primary key ascending before taking one row. Soft-delete models are
// filtered by GORM.
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

// List returns every matching row in the wrapper's order. It has no implicit
// limit, so a large table or a missing condition loads the entire result set
// into memory; use SelectPage for request-facing endpoints and reserve List for
// exports or internal batch jobs.
//
// The returned slice is always non-nil and serializes as [] when empty.
// Conditions are combined with AND. Soft-delete models are filtered by GORM.
func (d *DAO[T]) List(ctx context.Context, w *Wrapper[T]) ([]T, error) {
	db, _, err := d.query(ctx, w, true)
	if err != nil {
		return nil, err
	}
	entities := make([]T, 0)
	err = db.Find(&entities).Error
	return entities, err
}

// Count returns the number of matching rows via COUNT(*).
//
// Ordering is ignored and only filters from w are applied; a nil wrapper counts
// the whole table. Count and a following List or SelectPage are separate
// statements, so concurrent writes may make the count differ from the rows that
// are later returned.
func (d *DAO[T]) Count(ctx context.Context, w *Wrapper[T]) (int64, error) {
	db, _, err := d.query(ctx, w, false)
	if err != nil {
		return 0, err
	}
	var total int64
	err = db.Count(&total).Error
	return total, err
}

// SelectPage returns one normalized page of rows plus the exact total.
//
// On success it always executes two statements: COUNT with the filters only,
// then SELECT with filters, ordering, LIMIT and OFFSET. COUNT deliberately omits
// ordering and pagination so those clauses cannot affect the total, and each
// statement uses an independent session so count state cannot pollute the
// select. If either statement fails, the result is nil; no partial page is
// returned.
//
// The request is validated and defaulted by PageRequest.normalize: negative
// values, Size > MaxPageSize and offset overflow fail with an error wrapping
// ErrInvalidPage, while a page past the last page succeeds with empty Records.
// Primary keys not already covered by the wrapper's ordering are appended as
// tie-breakers so OFFSET paging remains stable when sort values repeat.
//
// Records is never nil. The two statements do not share a snapshot; wrap them in
// a repeatable-read transaction when total and rows must be consistent.
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

// Update applies changes through a map so explicit false, zero and empty
// string values are written instead of being skipped as "zero values".
//
// Keys may be Go field names or database column names; each is resolved against
// the schema and translated to its column name. Unknown fields, primary keys,
// read-only fields and two keys that normalize to the same column return
// ErrInvalidField. An empty map is an error.
//
// w must contain at least one restrictive condition, otherwise ErrUnsafeWrite
// prevents an accidental full-table update. Only filters are used; ordering is
// ignored. The returned count is RowsAffected, which is zero when the matching
// rows already held the requested values.
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

// UpdateByID is Update restricted to the row with the given single primary key.
// It shares GetByID's key validation, so nil, empty or unsupported keys return
// ErrInvalidID, and the equality condition makes the write inherently safe.
func (d *DAO[T]) UpdateByID(ctx context.Context, id any, changes map[string]any) (int64, error) {
	w, err := d.primaryQuery(id)
	if err != nil {
		return 0, err
	}
	return d.Update(ctx, w, changes)
}

// Delete removes matching rows and returns RowsAffected.
//
// w must contain at least one restrictive condition, otherwise ErrUnsafeWrite
// prevents an accidental full-table delete. Models embedding gorm.DeletedAt are
// soft-deleted (GORM issues an UPDATE of the delete marker); models without it
// are physically deleted. Soft deletion comes from GORM's own callback, not a
// second competing implementation, so its hooks and filters stay intact.
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

// DeleteByID is Delete restricted to the row with the given single primary key.
// It shares GetByID's key validation, so invalid keys return ErrInvalidID.
func (d *DAO[T]) DeleteByID(ctx context.Context, id any) (int64, error) {
	w, err := d.primaryQuery(id)
	if err != nil {
		return 0, err
	}
	return d.Delete(ctx, w)
}

// Transaction runs fn inside one database transaction, rolling back when fn
// returns a non-nil error.
//
// fn receives a DAO bound to the transaction, so every operation it performs
// shares that transaction; the receiver is left untouched. Because Go methods
// cannot introduce a new type parameter, cross-model transactions must call
// NewDAO[Other](tx) inside a plain db.Transaction instead.
//
// A nil fn is rejected before a transaction is started.
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

package gormx

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

type joinStep struct {
	table *tableBinding
	kind  clause.JoinType
	on    JoinOn
}

// JoinQuery is an immutable, read-only flat-result query.
// Use root DB/transaction handles and express all permission filters explicitly.
// As with NewDAO, pre-existing GORM query clauses and Scopes are not retained.
type JoinQuery struct {
	db          *gorm.DB
	root        *tableBinding
	joins       []joinStep
	predicates  []JoinPredicate
	projections []Projection
	orders      []JoinOrder
	err         error
}

func NewJoinQuery(db *gorm.DB, root JoinTable) (*JoinQuery, error) {
	if db == nil {
		return nil, errors.New("gormx: nil database")
	}
	if db.Error != nil {
		return nil, db.Error
	}
	if nilJoinValue(root) {
		return nil, fmt.Errorf("%w: nil join table", ErrInvalidModel)
	}
	table := root.joinTable()
	if err := validateJoinTable(table); err != nil {
		return nil, err
	}
	q := &JoinQuery{db: db.Session(&gorm.Session{NewDB: true}).Table(""), root: table}
	if _, err := q.parseTable(table); err != nil {
		return nil, err
	}
	return q, nil
}

func nilJoinValue(value any) bool {
	return value == nil || (reflect.ValueOf(value).Kind() == reflect.Ptr && reflect.ValueOf(value).IsNil())
}

func validateJoinTable(table *tableBinding) error {
	if table == nil || table.model == nil || table.model.Kind() != reflect.Struct {
		return ErrInvalidModel
	}
	if !joinIdentifier.MatchString(table.alias) {
		return fmt.Errorf("%w: invalid table alias %q", ErrInvalidQuery, table.alias)
	}
	return nil
}

func (q *JoinQuery) clone() *JoinQuery {
	if q == nil {
		return &JoinQuery{err: fmt.Errorf("%w: nil JoinQuery", ErrInvalidQuery)}
	}
	next := *q
	next.joins = append([]joinStep(nil), q.joins...)
	next.predicates = append([]JoinPredicate(nil), q.predicates...)
	next.projections = append([]Projection(nil), q.projections...)
	next.orders = append([]JoinOrder(nil), q.orders...)
	return &next
}

func (q *JoinQuery) LeftJoin(table JoinTable, on JoinOn) *JoinQuery {
	return q.addJoin(table, on, clause.LeftJoin)
}

func (q *JoinQuery) InnerJoin(table JoinTable, on JoinOn) *JoinQuery {
	return q.addJoin(table, on, clause.InnerJoin)
}

func (q *JoinQuery) addJoin(table JoinTable, on JoinOn, kind clause.JoinType) *JoinQuery {
	next := q.clone()
	if nilJoinValue(table) {
		next.err = fmt.Errorf("%w: nil join table", ErrInvalidModel)
		return next
	}
	next.joins = append(next.joins, joinStep{table: table.joinTable(), kind: kind, on: on})
	return next
}

func (q *JoinQuery) Where(predicates ...JoinPredicate) *JoinQuery {
	next := q.clone()
	next.predicates = append(next.predicates, predicates...)
	return next
}

func (q *JoinQuery) WhereIf(condition bool, predicates ...JoinPredicate) *JoinQuery {
	if !condition {
		return q.clone()
	}
	return q.Where(predicates...)
}

// Select replaces the projection. Wildcards and raw SQL are intentionally unsupported.
func (q *JoinQuery) Select(projections ...Projection) *JoinQuery {
	next := q.clone()
	next.projections = append([]Projection(nil), projections...)
	return next
}

func (q *JoinQuery) OrderBy(orders ...JoinOrder) *JoinQuery {
	next := q.clone()
	next.orders = append(next.orders, orders...)
	return next
}

type joinPlan struct {
	from    clause.From
	where   []clause.Expression
	selects []clause.Column
	orders  []clause.OrderByColumn
	tables  []*tableBinding
	scope   joinScope
}

func (q *JoinQuery) parseTable(table *tableBinding) (*schema.Schema, error) {
	if err := validateJoinTable(table); err != nil {
		return nil, err
	}
	stmt := &gorm.Statement{DB: q.db}
	if err := stmt.Parse(reflect.New(table.model).Interface()); err != nil {
		return nil, err
	}
	return stmt.Schema, nil
}

func joinSoftDelete(table *tableBinding, s *schema.Schema) ([]clause.Expression, error) {
	var filters []clause.Expression
	for _, queryClause := range s.QueryClauses {
		switch soft := queryClause.(type) {
		case gorm.SoftDeleteQueryClause:
			filters = append(filters, clause.Eq{
				Column: clause.Column{Table: table.alias, Name: soft.Field.DBName},
				Value:  soft.ZeroValue,
			})
		default:
			// Never silently drop an unsupported model-level visibility constraint.
			return nil, fmt.Errorf("%w: unsupported query clause on %s; use native GORM", ErrInvalidQuery, table.alias)
		}
	}
	return filters, nil
}

func (q *JoinQuery) compile() (joinPlan, error) {
	plan := joinPlan{scope: make(joinScope)}
	if q == nil || q.db == nil || q.root == nil {
		return plan, fmt.Errorf("%w: uninitialized JoinQuery", ErrInvalidQuery)
	}
	if q.err != nil {
		return plan, q.err
	}
	rootSchema, err := q.parseTable(q.root)
	if err != nil {
		return plan, err
	}
	plan.scope[q.root] = rootSchema
	plan.tables = append(plan.tables, q.root)
	plan.from.Tables = []clause.Table{{Name: rootSchema.Table, Alias: q.root.alias}}
	plan.where, err = joinSoftDelete(q.root, rootSchema)
	if err != nil {
		return plan, err
	}
	aliases := map[string]bool{strings.ToLower(q.root.alias): true}
	for _, step := range q.joins {
		s, err := q.parseTable(step.table)
		if err != nil {
			return plan, err
		}
		alias := strings.ToLower(step.table.alias)
		if aliases[alias] {
			return plan, fmt.Errorf("%w: duplicate table alias %q", ErrInvalidQuery, step.table.alias)
		}
		if nilJoinValue(step.on.left) || nilJoinValue(step.on.right) {
			return plan, fmt.Errorf("%w: join requires ON columns", ErrInvalidQuery)
		}
		left, right := step.on.left.joinColumn(), step.on.right.joinColumn()
		_, leftKnown := plan.scope[left.table]
		_, rightKnown := plan.scope[right.table]
		if !((left.table == step.table && rightKnown) || (right.table == step.table && leftKnown)) {
			return plan, fmt.Errorf("%w: ON must link the new table to an earlier table", ErrInvalidQuery)
		}
		aliases[alias] = true
		plan.scope[step.table] = s
		plan.tables = append(plan.tables, step.table)
		predicates := []JoinPredicate{JoinEqual(step.on.left, step.on.right)}
		predicates = append(predicates, step.on.predicates...)
		on, err := JoinAll(predicates...).build(plan.scope)
		if err != nil {
			return plan, err
		}
		filters, err := joinSoftDelete(step.table, s)
		if err != nil {
			return plan, err
		}
		plan.from.Joins = append(plan.from.Joins, clause.Join{
			Type:  step.kind,
			Table: clause.Table{Name: s.Table, Alias: step.table.alias},
			ON:    clause.Where{Exprs: append([]clause.Expression{on}, filters...)},
		})
	}
	for _, p := range q.predicates {
		if p.build == nil {
			return plan, ErrInvalidQuery
		}
		expr, err := p.build(plan.scope)
		if err != nil {
			return plan, err
		}
		plan.where = append(plan.where, expr)
	}
	outputs := make(map[string]bool)
	for _, p := range q.projections {
		if !joinIdentifier.MatchString(p.alias) || outputs[strings.ToLower(p.alias)] {
			return plan, fmt.Errorf("%w: invalid or duplicate projection alias %q", ErrInvalidQuery, p.alias)
		}
		col, err := p.ref.column(plan.scope)
		if err != nil {
			return plan, err
		}
		col.Alias = p.alias
		plan.selects = append(plan.selects, col)
		outputs[strings.ToLower(p.alias)] = true
	}
	for _, order := range q.orders {
		col, err := order.ref.column(plan.scope)
		if err != nil {
			return plan, err
		}
		plan.orders = append(plan.orders, clause.OrderByColumn{Column: col, Desc: order.desc})
	}
	return plan, nil
}

func (q *JoinQuery) base(ctx context.Context, plan joinPlan) *gorm.DB {
	// No source Model or DTO Model: visibility is handled per table above.
	// Scan therefore does not apply DTO soft-delete clauses or AfterFind hooks.
	db := q.db.Session(&gorm.Session{}).WithContext(ctx).Table(q.root.alias).Clauses(plan.from)
	if len(plan.where) > 0 {
		db = db.Clauses(clause.Where{Exprs: plan.where})
	}
	return db
}

// Count counts joined result rows, not distinct root entities.
// A projection is not required for Count.
func (q *JoinQuery) Count(ctx context.Context) (int64, error) {
	plan, err := q.compile()
	if err != nil {
		return 0, err
	}
	return q.count(ctx, plan)
}

func (q *JoinQuery) count(ctx context.Context, plan joinPlan) (int64, error) {
	var total int64
	err := q.base(ctx, plan).Count(&total).Error
	return total, err
}

func validateJoinDTO[R any](q *JoinQuery, plan joinPlan) error {
	if reflect.TypeOf((*R)(nil)).Elem().Kind() != reflect.Struct {
		return ErrInvalidModel
	}
	if len(plan.selects) == 0 {
		return fmt.Errorf("%w: join DTO queries require an explicit projection", ErrInvalidQuery)
	}
	stmt := &gorm.Statement{DB: q.db}
	if err := stmt.Parse(new(R)); err != nil {
		return err
	}
	for _, column := range plan.selects {
		field := stmt.Schema.FieldsByDBName[column.Alias]
		if field == nil || !field.Readable {
			return fmt.Errorf("%w: output %q has no readable DTO field", ErrInvalidField, column.Alias)
		}
	}
	return nil
}

func joinRead[R any](ctx context.Context, q *JoinQuery, plan joinPlan, limit, offset int) ([]R, error) {
	records := make([]R, 0)
	db := q.base(ctx, plan).Clauses(clause.Select{Columns: plan.selects})
	if len(plan.orders) > 0 {
		db = db.Clauses(clause.OrderBy{Columns: plan.orders})
	}
	if limit > 0 {
		db = db.Limit(limit).Offset(offset)
	}
	if err := db.Scan(&records).Error; err != nil {
		return nil, err
	}
	return records, nil
}

// JoinList scans explicit output aliases into a flat DTO. It has no implicit limit.
func JoinList[R any](ctx context.Context, q *JoinQuery) ([]R, error) {
	plan, err := q.compile()
	if err != nil {
		return nil, err
	}
	if err := validateJoinDTO[R](q, plan); err != nil {
		return nil, err
	}
	return joinRead[R](ctx, q, plan, 0, 0)
}

// JoinPage paginates joined rows, never deduplicates root entities and always
// performs COUNT then SELECT on success. Use nullable DTO fields for LEFT JOINs.
// Primary keys of all participating tables provide ordering tie-breakers.
func JoinPage[R any](ctx context.Context, q *JoinQuery, request PageRequest) (*Page[R], error) {
	r, offset, err := request.normalize()
	if err != nil {
		return nil, err
	}
	plan, err := q.compile()
	if err != nil {
		return nil, err
	}
	if err := validateJoinDTO[R](q, plan); err != nil {
		return nil, err
	}
	for _, table := range plan.tables {
		for _, field := range plan.scope[table].PrimaryFields {
			found := false
			for _, order := range plan.orders {
				found = found || (order.Column.Table == table.alias && order.Column.Name == field.DBName)
			}
			if !found {
				plan.orders = append(plan.orders, clause.OrderByColumn{
					Column: clause.Column{Table: table.alias, Name: field.DBName},
				})
			}
		}
	}
	total, err := q.count(ctx, plan)
	if err != nil {
		return nil, err
	}
	records, err := joinRead[R](ctx, q, plan, r.Size, offset)
	if err != nil {
		return nil, err
	}
	pages := total / int64(r.Size)
	if total%int64(r.Size) != 0 {
		pages++
	}
	return &Page[R]{Records: records, Total: total, Current: r.Current, Size: r.Size, Pages: pages}, nil
}

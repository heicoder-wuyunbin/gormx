package gormx_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/heicoder-wuyunbin/gormx"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

type joinDepartment struct {
	ID        uint
	Name      string `gorm:"size:100;column:dept_name"`
	TenantID  uint
	DeletedAt gorm.DeletedAt
}

type joinEmployee struct {
	ID           uint
	Name         string `gorm:"size:100"`
	DepartmentID *uint
	ManagerID    *uint
	TenantID     uint
	DeletedAt    gorm.DeletedAt
}

type joinView struct {
	ID         uint
	Name       string
	Department *string `gorm:"column:department_name"`
}

func joinFixture(t *testing.T, integration bool) (*gorm.DB, gormx.TableRef[joinEmployee], gormx.TableRef[joinDepartment], *gormx.JoinQuery) {
	t.Helper()
	db := openDB(t, integration)
	migrateExtra(t, db, &joinEmployee{}, &joinDepartment{})
	departments := []joinDepartment{
		{ID: 1, Name: "engineering", TenantID: 1},
		{ID: 2, Name: "deleted", TenantID: 1},
		{ID: 3, Name: "another tenant", TenantID: 2},
	}
	if err := db.Create(&departments).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(&departments[1]).Error; err != nil {
		t.Fatal(err)
	}
	ptr := func(id uint) *uint { return &id }
	employees := []joinEmployee{
		{ID: 1, Name: "alice", TenantID: 1, DepartmentID: ptr(1)},
		{ID: 2, Name: "bob", TenantID: 1, DepartmentID: ptr(2), ManagerID: ptr(1)},
		{ID: 3, Name: "carol", TenantID: 1, ManagerID: ptr(1)},
		{ID: 4, Name: "dave", TenantID: 1, DepartmentID: ptr(999), ManagerID: ptr(1)},
		{ID: 5, Name: "erin", TenantID: 1, DepartmentID: ptr(3), ManagerID: ptr(1)},
		{ID: 6, Name: "deleted employee", TenantID: 1, DepartmentID: ptr(1)},
		{ID: 7, Name: "foreign employee", TenantID: 2, DepartmentID: ptr(3)},
	}
	if err := db.Create(&employees).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(&employees[5]).Error; err != nil {
		t.Fatal(err)
	}
	e := gormx.TableOf[joinEmployee]("e")
	d := gormx.TableOf[joinDepartment]("d")
	base, err := gormx.NewJoinQuery(db, e)
	if err != nil {
		t.Fatal(err)
	}
	query := base.LeftJoin(d, gormx.On(e.Col("DepartmentID"), d.Col("ID")).And(d.Col("TenantID").Eq(1))).
		Where(e.Col("TenantID").Eq(1)).
		Select(e.Col("ID").As("id"), e.Col("Name").As("name"), d.Col("Name").As("department_name")).
		OrderBy(e.Col("ID").Asc())
	return db, e, d, query
}

func TestSQLiteJoinContract(t *testing.T) { runJoinContract(t, false) }
func TestMySQLJoinContract(t *testing.T)  { runJoinContract(t, true) }

func runJoinContract(t *testing.T, integration bool) {
	ctx := context.Background()
	t.Run("left_join_visibility_and_projection", func(t *testing.T) {
		db, e, d, query := joinFixture(t, integration)
		rows, err := gormx.JoinList[joinView](ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 5 || rows[0].Department == nil || *rows[0].Department != "engineering" {
			t.Fatalf("left join result: %+v", rows)
		}
		for _, row := range rows[1:] {
			if row.Department != nil {
				t.Fatalf("unmatched/deleted/cross-tenant department should be NULL: %+v", row)
			}
		}
		base, err := gormx.NewJoinQuery(db, e)
		if err != nil {
			t.Fatal(err)
		}
		inner := base.InnerJoin(d, gormx.On(d.Col("ID"), e.Col("DepartmentID")).And(d.Col("TenantID").Eq(1))).
			Where(e.Col("TenantID").Eq(1)).
			Select(e.Col("ID").As("id"), e.Col("Name").As("name"), d.Col("Name").As("department_name"))
		innerRows, err := gormx.JoinList[joinView](ctx, inner)
		if err != nil || len(innerRows) != 1 || innerRows[0].Name != "alice" {
			t.Fatalf("inner join: %+v %v", innerRows, err)
		}
		// A right-hand WHERE filter intentionally excludes NULL-extended rows.
		if n, err := query.Where(d.Col("Name").Eq("engineering")).Count(ctx); err != nil || n != 1 {
			t.Fatalf("right-hand WHERE: %d %v", n, err)
		}
		if n, err := base.Count(ctx); err != nil || n != 6 {
			t.Fatalf("root soft delete without projection: %d %v", n, err)
		}
	})

	t.Run("typed_conditions_and_immutability", func(t *testing.T) {
		_, e, d, query := joinFixture(t, integration)
		id := gormx.Bind(e, gormx.Field(func(m *joinEmployee) *uint { return &m.ID }))
		name := gormx.Bind(e, gormx.Field(func(m *joinEmployee) *string { return &m.Name }))
		deptName := gormx.Bind(d, gormx.Field(func(m *joinDepartment) *string { return &m.Name }))
		values := []uint{1, 2}
		copied := query.Where(id.In(values...))
		values[0] = 999
		cases := []struct {
			name string
			q    *gormx.JoinQuery
			want int64
		}{
			{"eq", query.Where(id.Eq(1)), 1},
			{"ne", query.Where(id.Ne(1)), 4},
			{"gt", query.Where(id.Gt(3)), 2},
			{"ge", query.Where(id.Ge(3)), 3},
			{"lt", query.Where(id.Lt(3)), 2},
			{"le", query.Where(id.Le(3)), 3},
			{"between", query.Where(id.Between(2, 4)), 3},
			{"in_copy", copied, 2},
			{"not_in", query.Where(id.NotIn(1, 2)), 3},
			{"empty_in", query.Where(id.In()), 0},
			{"empty_not_in", query.Where(id.NotIn()), 5},
			{"null", query.Where(deptName.IsNull()), 4},
			{"not_null", query.Where(deptName.IsNotNull()), 1},
			{"like", query.Where(name.Like("a%")), 1},
			{"not_like", query.Where(name.NotLike("a%")), 4},
			{"sql_value", query.Where(name.Eq("' OR 1=1 --")), 0},
			{"conditional_false", query.WhereIf(false, gormx.JoinPredicate{}), 5},
			{"conditional_true", query.WhereIf(true, name.Eq("alice")), 1},
			{"nested", query.Where(gormx.JoinAny(name.Eq("alice"), gormx.JoinAll(id.Ge(3), id.Le(4)))), 3},
			{"column_equality", query.Where(gormx.JoinEqual(e.Col("TenantID"), d.Col("TenantID"))), 1},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if n, err := tc.q.Count(ctx); err != nil || n != tc.want {
					t.Fatalf("count=%d err=%v want=%d", n, err, tc.want)
				}
			})
		}
		var wg sync.WaitGroup
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				page, err := gormx.JoinPage[joinView](ctx, query.Where(id.Ge(2)), gormx.PageRequest{Size: 2})
				if err != nil || page.Total != 4 || len(page.Records) != 2 {
					t.Errorf("concurrent join query: %+v %v", page, err)
				}
			}()
		}
		wg.Wait()
		if n, err := query.Count(ctx); err != nil || n != 5 {
			t.Fatalf("base query was mutated: %d %v", n, err)
		}
	})

	t.Run("page_count_and_select", func(t *testing.T) {
		db, e, _, query := joinFixture(t, integration)
		var queries []string
		trace := func(tx *gorm.DB) { queries = append(queries, tx.Statement.SQL.String()) }
		if err := db.Callback().Query().After("gorm:query").Register("test:join_query", trace); err != nil {
			t.Fatal(err)
		}
		if err := db.Callback().Row().After("gorm:row").Register("test:join_row", trace); err != nil {
			t.Fatal(err)
		}
		for _, request := range []gormx.PageRequest{{Current: 2, Size: 2}, {Current: 100, Size: 2}, {}} {
			queries = nil
			page, err := gormx.JoinPage[joinView](ctx, query, request)
			if err != nil || page.Total != 5 || page.Records == nil {
				t.Fatalf("page=%+v err=%v", page, err)
			}
			if len(queries) != 2 || !strings.Contains(strings.ToLower(queries[0]), "count(") ||
				strings.Contains(strings.ToUpper(queries[0]), "LIMIT") ||
				strings.Contains(strings.ToUpper(queries[0]), "ORDER BY") ||
				!strings.Contains(strings.ToUpper(queries[1]), "LIMIT") {
				t.Fatalf("expected clean count then limited select: %v", queries)
			}
			if request.Current == 2 && (page.Pages != 3 || len(page.Records) != 2 || page.Records[0].ID != 3) {
				t.Fatalf("page metadata: %+v", page)
			}
			if request.Current == 100 && len(page.Records) != 0 {
				t.Fatalf("out of range page: %+v", page)
			}
		}
		queries = nil
		empty, err := gormx.JoinPage[joinView](ctx, query.Where(e.Col("ID").In()), gormx.PageRequest{})
		if err != nil || empty.Total != 0 || empty.Pages != 0 || len(queries) != 2 || empty.Records == nil {
			t.Fatalf("empty page: %+v %v SQL=%v", empty, err, queries)
		}
	})

	t.Run("self_join_and_multi_join", func(t *testing.T) {
		db, e, d, _ := joinFixture(t, integration)
		manager := gormx.TableOf[joinEmployee]("manager")
		base, err := gormx.NewJoinQuery(db, e)
		if err != nil {
			t.Fatal(err)
		}
		type dto struct {
			ID         uint
			Name       string
			Manager    *string
			Department *string
		}
		q := base.LeftJoin(manager, gormx.On(e.Col("ManagerID"), manager.Col("ID"))).
			LeftJoin(d, gormx.On(manager.Col("DepartmentID"), d.Col("ID"))).
			Where(e.Col("TenantID").Eq(1)).
			Select(e.Col("ID").As("id"), e.Col("Name").As("name"), manager.Col("Name").As("manager"), d.Col("Name").As("department")).
			OrderBy(e.Col("ID").Asc())
		rows, err := gormx.JoinList[dto](ctx, q)
		if err != nil || len(rows) != 5 || rows[0].Manager != nil || rows[1].Manager == nil || *rows[1].Manager != "alice" ||
			rows[1].Department == nil || *rows[1].Department != "engineering" {
			t.Fatalf("self/multi join: %+v %v", rows, err)
		}
	})

	t.Run("row_pagination_does_not_deduplicate", func(t *testing.T) {
		db, e, _, _ := joinFixture(t, integration)
		migrateExtra(t, db, &eventRecord{})
		events := []eventRecord{{UserID: 1, Action: "one"}, {UserID: 1, Action: "two"}, {UserID: 2, Action: "three"}}
		if err := db.Create(&events).Error; err != nil {
			t.Fatal(err)
		}
		event := gormx.TableOf[eventRecord]("event")
		base, err := gormx.NewJoinQuery(db, e)
		if err != nil {
			t.Fatal(err)
		}
		type dto struct{ ID, EventID uint }
		q := base.InnerJoin(event, gormx.On(e.Col("ID"), event.Col("UserID"))).
			Select(e.Col("ID").As("id"), event.Col("ID").As("event_id"))
		page, err := gormx.JoinPage[dto](ctx, q, gormx.PageRequest{Size: 1, Current: 2})
		if err != nil || page.Total != 3 || page.Pages != 3 || len(page.Records) != 1 ||
			page.Records[0].ID != 1 || page.Records[0].EventID != 2 {
			t.Fatalf("row pagination contract: %+v %v", page, err)
		}
	})

	t.Run("transaction_and_context", func(t *testing.T) {
		db, e, d, _ := joinFixture(t, integration)
		sentinel := errors.New("rollback")
		err := db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(&joinEmployee{Name: "uncommitted", TenantID: 1}).Error; err != nil {
				return err
			}
			q, err := gormx.NewJoinQuery(tx, e)
			if err != nil {
				return err
			}
			n, err := q.LeftJoin(d, gormx.On(e.Col("DepartmentID"), d.Col("ID"))).Where(e.Col("TenantID").Eq(1)).Count(ctx)
			if err != nil || n != 6 {
				return errors.New("join escaped transaction")
			}
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatal(err)
		}
		q, err := gormx.NewJoinQuery(db, e)
		if err != nil {
			t.Fatal(err)
		}
		q = q.Where(e.Col("TenantID").Eq(1)).Select(e.Col("ID").As("id"))
		if n, err := q.Count(ctx); err != nil || n != 5 {
			t.Fatalf("rollback: %d %v", n, err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := q.Count(canceled); !errors.Is(err, context.Canceled) {
			t.Fatalf("count cancellation: %v", err)
		}
		if _, err := gormx.JoinList[joinView](canceled, q); !errors.Is(err, context.Canceled) {
			t.Fatalf("list cancellation: %v", err)
		}
	})
}

func TestJoinValidation(t *testing.T) {
	db, e, d, valid := joinFixture(t, false)
	ctx := context.Background()
	base, err := gormx.NewJoinQuery(db, e)
	if err != nil {
		t.Fatal(err)
	}
	unbound := gormx.TableOf[joinDepartment]("d")
	future := gormx.TableOf[joinDepartment]("future")
	duplicate := gormx.TableOf[joinDepartment]("E")
	invalid := []*gormx.JoinQuery{
		nil, new(gormx.JoinQuery),
		base.LeftJoin(nil, gormx.JoinOn{}),
		base.LeftJoin(d, gormx.JoinOn{}),
		base.LeftJoin(duplicate, gormx.On(e.Col("ID"), duplicate.Col("ID"))),
		valid.LeftJoin(d, gormx.On(e.Col("ID"), d.Col("ID"))),
		base.LeftJoin(d, gormx.On(e.Col("ID"), e.Col("ID"))),
		base.LeftJoin(d, gormx.On(d.Col("ID"), d.Col("ID"))),
		base.LeftJoin(d, gormx.On(future.Col("ID"), d.Col("ID"))),
		base.LeftJoin(d, gormx.On(e.Col("DepartmentID"), d.Col("ID")).And(future.Col("ID").Eq(1))),
		valid.Where(unbound.Col("ID").Eq(1)),
		valid.Where(e.Col("ID OR 1=1").Eq(1)),
		valid.Where(gormx.JoinPredicate{}),
		valid.Where(gormx.JoinAll()),
		valid.Where(gormx.JoinAny()),
		valid.Where(gormx.JoinEqual(nil, e.Col("ID"))),
		valid.Where(gormx.JoinColumn[int]{}.Eq(1)),
		valid.Select(e.Col("ID").As("id"), d.Col("ID").As("ID")),
		valid.Select(e.Col("ID").As("id; DROP TABLE users")),
		valid.Select(e.Col("missing").As("id")),
		valid.OrderBy(gormx.JoinOrder{}),
		valid.OrderBy(future.Col("ID").Desc()),
	}
	queries := 0
	if err := db.Callback().Query().Before("gorm:query").Register("test:no_query", func(*gorm.DB) { queries++ }); err != nil {
		t.Fatal(err)
	}
	for i, q := range invalid {
		if _, err := gormx.JoinPage[joinView](ctx, q, gormx.PageRequest{}); err == nil {
			t.Fatalf("invalid query %d accepted", i)
		}
	}
	if queries != 0 {
		t.Fatalf("invalid input reached the database: %d queries", queries)
	}
	if _, err := gormx.JoinList[joinView](ctx, base); !errors.Is(err, gormx.ErrInvalidQuery) {
		t.Fatalf("implicit SELECT * accepted: %v", err)
	}
	if _, err := gormx.JoinList[struct{ Other uint }](ctx, valid); !errors.Is(err, gormx.ErrInvalidField) {
		t.Fatalf("unknown DTO output accepted: %v", err)
	}
	if _, err := gormx.JoinList[*joinView](ctx, valid); !errors.Is(err, gormx.ErrInvalidModel) {
		t.Fatalf("pointer DTO accepted: %v", err)
	}
	if _, err := gormx.JoinPage[joinView](ctx, valid, gormx.PageRequest{Size: 501}); !errors.Is(err, gormx.ErrInvalidPage) {
		t.Fatalf("invalid page: %v", err)
	}
	for _, table := range []gormx.JoinTable{nil, gormx.TableRef[joinEmployee]{}, gormx.TableOf[*joinEmployee]("e"), gormx.TableOf[joinEmployee]("e; DROP")} {
		if _, err := gormx.NewJoinQuery(db, table); err == nil {
			t.Fatal("invalid table accepted")
		}
	}
	if _, err := gormx.NewJoinQuery(nil, e); err == nil {
		t.Fatal("nil DB accepted")
	}
}

type restrictedValue string

func (restrictedValue) QueryClauses(*schema.Field) []clause.Interface {
	return []clause.Interface{clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "1 = 0"}}}}
}

type customVisibility struct {
	ID     uint
	Secret restrictedValue
}

func TestJoinCustomVisibilityFailsClosed(t *testing.T) {
	db := openDB(t, false)
	table := gormx.TableOf[customVisibility]("r")
	q, err := gormx.NewJoinQuery(db, table)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Count(context.Background()); !errors.Is(err, gormx.ErrInvalidQuery) {
		t.Fatalf("custom visibility silently dropped: %v", err)
	}
}

func TestJoinPageQueryErrors(t *testing.T) {
	for _, failCount := range []bool{true, false} {
		t.Run(map[bool]string{true: "count", false: "select"}[failCount], func(t *testing.T) {
			db, _, _, query := joinFixture(t, false)
			sentinel := errors.New("join query failed")
			if failCount {
				if err := db.Callback().Query().Before("gorm:query").Register("test:fail_count", func(tx *gorm.DB) {
					tx.AddError(sentinel)
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := db.Callback().Row().Before("gorm:row").Register("test:fail_select", func(tx *gorm.DB) {
					tx.AddError(sentinel)
				}); err != nil {
					t.Fatal(err)
				}
			}
			page, err := gormx.JoinPage[joinView](context.Background(), query, gormx.PageRequest{})
			if !errors.Is(err, sentinel) || page != nil {
				t.Fatalf("page=%+v err=%v", page, err)
			}
		})
	}
}

func TestJoinDTOIsOnlyAProjection(t *testing.T) {
	_, e, _, query := joinFixture(t, false)
	// A DeletedAt field in the DTO must not add a filter against the root alias.
	type dto struct {
		ID        uint
		DeletedAt gorm.DeletedAt
	}
	rows, err := gormx.JoinList[dto](context.Background(), query.Select(e.Col("ID").As("id")))
	if err != nil || len(rows) != 5 || !reflect.DeepEqual(rows[0].DeletedAt, gorm.DeletedAt{}) {
		t.Fatalf("DTO polluted query semantics: %+v %v", rows, err)
	}
}

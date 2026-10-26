package gormx_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/heicoder-wuyunbin/gormx"

	"gorm.io/driver/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

type user struct {
	ID        uint   `gorm:"primaryKey"`
	Name      string `gorm:"column:display_name;size:100;uniqueIndex"`
	Age       int
	Active    bool
	Note      *string
	CreatedBy string
	UpdatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt gorm.DeletedAt `gorm:"index"`
	Ignored   string         `gorm:"-"`
}

var (
	nameField = gormx.Field(func(u *user) *string { return &u.Name })
	ageField  = gormx.Field(func(u *user) *int { return &u.Age })
	idField   = gormx.Field(func(u *user) *uint { return &u.ID })
)

func openDB(t *testing.T, integration bool) *gorm.DB {
	t.Helper()
	config := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	var db *gorm.DB
	var err error
	if integration {
		dsn := os.Getenv("GORMX_TEST_DSN")
		if dsn == "" {
			t.Skip("set GORMX_TEST_DSN to run the MySQL integration suite")
		}
		config.NamingStrategy = schema.NamingStrategy{TablePrefix: fmt.Sprintf("gx_%d_", time.Now().UnixNano())}
		db, err = gorm.Open(mysql.Open(dsn), config)
	} else {
		db, err = gorm.Open(sqlite.Open(":memory:"), config)
	}
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if integration {
		t.Cleanup(func() {
			if err := db.Migrator().DropTable(&user{}); err != nil {
				t.Error(err)
			}
		})
	}
	if err := db.AutoMigrate(&user{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func newDAO(t *testing.T, db *gorm.DB) *gormx.DAO[user] {
	t.Helper()
	dao, err := gormx.NewDAO[user](db)
	if err != nil {
		t.Fatal(err)
	}
	return dao
}

func seed(t *testing.T, dao *gormx.DAO[user]) []user {
	t.Helper()
	note := "present"
	users := []user{
		{Name: "alice", Age: 18, Active: true},
		{Name: "bob", Age: 25, Active: true, Note: &note},
		{Name: "carol", Age: 30},
		{Name: "dave", Age: 25, Active: true},
	}
	if err := dao.CreateBatch(context.Background(), users, 2); err != nil {
		t.Fatal(err)
	}
	if users[0].ID == 0 || users[3].ID == 0 {
		t.Fatal("batch IDs were not populated")
	}
	return users
}

func TestSQLiteContract(t *testing.T) { runContract(t, false) }

func TestMySQLContract(t *testing.T) { runContract(t, true) }

func runContract(t *testing.T, integration bool) {
	t.Run("conditions", func(t *testing.T) {
		dao := newDAO(t, openDB(t, integration))
		seed(t, dao)
		w := gormx.NewWrapper[user]()
		cases := []struct {
			name string
			q    *gormx.Wrapper[user]
			want int64
		}{
			{"all", nil, 4},
			{"eq", w.Eq("Name", "alice"), 1},
			{"column tag", w.Eq("display_name", "bob"), 1},
			{"zero", w.Eq("Active", false), 1},
			{"ne", w.Ne("Name", "alice"), 3},
			{"gt", w.Gt("Age", 25), 1},
			{"ge", w.Ge("Age", 25), 3},
			{"lt", w.Lt("Age", 25), 1},
			{"le", w.Le("Age", 25), 3},
			{"between", w.Between("Age", 18, 25), 3},
			{"like", w.Like("Name", "a%"), 1},
			{"not like", w.NotLike("Name", "a%"), 3},
			{"in", w.In("Age", []int{18, 30}), 2},
			{"not in", w.NotIn("Age", []int{18, 30}), 2},
			{"empty in", w.In("Age", []int{}), 0},
			{"empty not in", w.NotIn("Age", nil), 4},
			{"null", w.IsNull("Note"), 3},
			{"not null", w.IsNotNull("Note"), 1},
			{"conditional", w.EqIf(false, "bad field", 1).LikeIf(false, "bad field", "%"), 4},
			{"parameter binding", w.Eq("Name", "' OR 1=1 --"), 0},
			{"nested or", w.Eq("Active", true).Or(func(q *gormx.Wrapper[user]) *gormx.Wrapper[user] {
				return q.Eq("Name", "alice").Eq("Name", "carol")
			}), 1},
			{"nested and", w.And(func(q *gormx.Wrapper[user]) *gormx.Wrapper[user] {
				return q.Ge("Age", 25).Eq("Active", true)
			}), 2},
			{"lambda", gormx.NewLambda[user]().Where(ageField.Ge(25), nameField.Ne("carol")), 2},
			{"lambda in", w.Where(ageField.In(18, 30)), 2},
			{"lambda not in", w.Where(ageField.NotIn(18, 30)), 2},
			{"lambda comparisons", w.Where(ageField.Gt(18), ageField.Le(30), ageField.Lt(30)), 2},
			{"lambda between", w.Where(ageField.Between(18, 25)), 3},
			{"lambda like", w.Where(gormx.Like(nameField, "a%")), 1},
			{"lambda null", w.Where(nameField.IsNotNull()), 4},
			{"lambda or", w.Where(gormx.Any(nameField.Eq("alice"), ageField.Eq(30))), 2},
			{"lambda conditional", w.WhereIf(false, ageField.Eq(99)), 4},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got, err := dao.Count(context.Background(), tc.q)
				if err != nil || got != tc.want {
					t.Fatalf("count=%d err=%v; want=%d", got, err, tc.want)
				}
			})
		}
	})

	t.Run("pagination", func(t *testing.T) {
		db := openDB(t, integration)
		dao := newDAO(t, db)
		seed(t, dao)
		var queries []string
		if err := db.Callback().Query().After("gorm:query").Register("test:queries", func(tx *gorm.DB) {
			queries = append(queries, tx.Statement.SQL.String())
		}); err != nil {
			t.Fatal(err)
		}
		w := gormx.NewWrapper[user]().Eq("Active", true).OrderByDesc("Age")
		page, err := dao.SelectPage(context.Background(), w, gormx.PageRequest{Current: 2, Size: 2})
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != 3 || page.Pages != 2 || page.Current != 2 || page.Size != 2 ||
			len(page.Records) != 1 || page.Records[0].Name != "alice" {
			t.Fatalf("unexpected page: %+v", page)
		}
		if len(queries) != 2 || !strings.Contains(strings.ToLower(queries[0]), "count(") ||
			strings.Contains(strings.ToUpper(queries[0]), "ORDER BY") ||
			strings.Contains(strings.ToUpper(queries[0]), "LIMIT") ||
			!strings.Contains(strings.ToUpper(queries[1]), "LIMIT") {
			t.Fatalf("expected clean count then limited select: %v", queries)
		}
		first, err := dao.SelectPage(context.Background(), w, gormx.PageRequest{Size: 2})
		if err != nil || first.Total != 3 || len(first.Records) != 2 || first.Records[0].Name != "bob" {
			t.Fatalf("reused wrapper: %+v, %v", first, err)
		}
		empty, err := dao.SelectPage(context.Background(), w, gormx.PageRequest{Current: 100, Size: 2})
		if err != nil || empty.Total != 3 || empty.Records == nil || len(empty.Records) != 0 {
			t.Fatalf("out of range: %+v, %v", empty, err)
		}
		empty, err = dao.SelectPage(context.Background(), gormx.NewWrapper[user]().In("ID", nil), gormx.PageRequest{})
		if err != nil || empty.Total != 0 || empty.Pages != 0 || empty.Current != 1 || empty.Size != 20 {
			t.Fatalf("empty/default page: %+v, %v", empty, err)
		}
		for _, request := range []gormx.PageRequest{
			{Current: -1}, {Size: -1}, {Size: 501}, {Current: int(^uint(0) >> 1), Size: 2},
		} {
			if _, err := dao.SelectPage(context.Background(), w, request); !errors.Is(err, gormx.ErrInvalidPage) {
				t.Fatalf("request %+v: %v", request, err)
			}
		}
	})

	t.Run("writes_and_soft_delete", func(t *testing.T) {
		db := openDB(t, integration)
		dao := newDAO(t, db)
		users := seed(t, dao)
		ctx := context.Background()
		changes := map[string]any{"Age": 0, "Active": false, "Name": ""}
		rows, err := dao.UpdateByID(ctx, users[0].ID, changes)
		if err != nil || rows != 1 {
			t.Fatalf("update: %d %v", rows, err)
		}
		got, err := dao.GetByID(ctx, users[0].ID)
		if err != nil || got.Age != 0 || got.Active || got.Name != "" {
			t.Fatalf("zero values: %+v, %v", got, err)
		}
		if changes["Name"] != "" || len(changes) != 3 {
			t.Fatal("caller update map mutated")
		}
		rows, err = dao.Delete(ctx, gormx.NewWrapper[user]().In("ID", []uint{users[0].ID, users[1].ID}))
		if err != nil || rows != 2 {
			t.Fatalf("delete: %d %v", rows, err)
		}
		if _, err := dao.GetByID(ctx, users[0].ID); !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("deleted row visible: %v", err)
		}
		page, err := dao.SelectPage(ctx, nil, gormx.PageRequest{})
		if err != nil || page.Total != 2 || len(page.Records) != 2 {
			t.Fatalf("soft delete filters: %+v %v", page, err)
		}
		var total int64
		if err := db.Unscoped().Model(&user{}).Count(&total).Error; err != nil || total != 4 {
			t.Fatalf("physical rows: %d %v", total, err)
		}
		rows, err = dao.DeleteByID(ctx, users[2].ID)
		if err != nil || rows != 1 {
			t.Fatalf("delete by ID: %d %v", rows, err)
		}
		list, err := dao.List(ctx, gormx.NewLambda[user]().OrderBy(ageField.Desc(), idField.Asc()))
		if err != nil || len(list) != 1 || list[0].Name != "dave" {
			t.Fatalf("list: %+v %v", list, err)
		}
	})

	t.Run("write_guards_and_validation", func(t *testing.T) {
		dao := newDAO(t, openDB(t, integration))
		seed(t, dao)
		ctx := context.Background()
		w := gormx.NewWrapper[user]()
		for _, q := range []*gormx.Wrapper[user]{
			nil, w, w.OrderByAsc("ID"), w.EqIf(false, "ID", 1), w.NotIn("ID", nil),
			w.Where(gormx.Any(idField.NotIn(), idField.Eq(1))),
		} {
			if _, err := dao.Update(ctx, q, map[string]any{"Age": 0}); !errors.Is(err, gormx.ErrUnsafeWrite) {
				t.Fatalf("unsafe update allowed: %v", err)
			}
			if _, err := dao.Delete(ctx, q); !errors.Is(err, gormx.ErrUnsafeWrite) {
				t.Fatalf("unsafe delete allowed: %v", err)
			}
		}
		bad := []struct {
			q   *gormx.Wrapper[user]
			err error
		}{
			{w.Eq("Name OR 1=1", 1), gormx.ErrInvalidField},
			{w.OrderByAsc("id; DROP TABLE users"), gormx.ErrInvalidField},
			{w.Eq("Ignored", "x"), gormx.ErrInvalidField},
			{w.In("ID", 1), gormx.ErrInvalidQuery},
			{w.Where(gormx.Any[user]()), gormx.ErrInvalidQuery},
			{w.Where(gormx.Predicate[user]{}), gormx.ErrInvalidQuery},
			{w.And(nil), gormx.ErrInvalidQuery},
			{w.Or(func(q *gormx.Wrapper[user]) *gormx.Wrapper[user] { return q.OrderByAsc("ID") }), gormx.ErrInvalidQuery},
			{w.Where(gormx.Field[user, int](nil).Eq(1)), gormx.ErrInvalidField},
			{w.Where(gormx.Field(func(*user) *int { n := 1; return &n }).Eq(1)), gormx.ErrInvalidField},
			{w.Where(gormx.Field(func(*user) *int { panic("invalid") }).Eq(1)), gormx.ErrInvalidField},
			{w.Where(gormx.Column[user, int]{}.Eq(1)), gormx.ErrInvalidField},
		}
		for _, tc := range bad {
			if _, err := dao.Count(ctx, tc.q); !errors.Is(err, tc.err) {
				t.Fatalf("invalid query: got %v want %v", err, tc.err)
			}
		}
		for _, changes := range []map[string]any{{"ID": 9}, {"bad": 1}, {"Name": "x", "display_name": "y"}} {
			if _, err := dao.Update(ctx, w.Eq("ID", 1), changes); !errors.Is(err, gormx.ErrInvalidField) {
				t.Fatalf("invalid update: %v", err)
			}
		}
		if _, err := dao.Update(ctx, w.Eq("ID", 1), nil); err == nil {
			t.Fatal("empty update accepted")
		}
		if rows, err := dao.Delete(ctx, w.In("ID", nil)); err != nil || rows != 0 {
			t.Fatalf("empty IN must be a safe no-op: %d %v", rows, err)
		}
		if _, err := dao.GetByID(ctx, "1 OR 1=1"); !errors.Is(err, gormx.ErrInvalidID) {
			t.Fatalf("invalid numeric ID accepted: %v", err)
		}
	})

	t.Run("transactions_context_and_isolation", func(t *testing.T) {
		db := openDB(t, integration)
		dao := newDAO(t, db)
		seed(t, dao)
		sentinel := errors.New("rollback")
		err := dao.Transaction(context.Background(), func(txDAO *gormx.DAO[user]) error {
			if err := txDAO.Create(context.Background(), &user{Name: "rolled-back"}); err != nil {
				return err
			}
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("transaction error: %v", err)
		}
		if n, err := dao.Count(context.Background(), nil); err != nil || n != 4 {
			t.Fatalf("rollback count: %d %v", n, err)
		}
		batch := []user{{Name: "batch-first"}, {Name: "alice"}}
		if err := dao.CreateBatch(context.Background(), batch, 1); err == nil {
			t.Fatal("expected unique constraint error")
		}
		if n, err := dao.Count(context.Background(), nil); err != nil || n != 4 {
			t.Fatalf("batch was not atomic: %d %v", n, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := dao.List(ctx, nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("context not propagated: %v", err)
		}
		// Only the connection/transaction is retained from a supplied GORM handle.
		clean := newDAO(t, db.Where("1 = 0").Limit(1).Order("id DESC"))
		if n, err := clean.Count(context.Background(), nil); err != nil || n != 4 {
			t.Fatalf("inherited query leakage: %d %v", n, err)
		}
		base := gormx.NewLambda[user]().Where(ageField.Ge(18))
		var wg sync.WaitGroup
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				n, err := dao.Count(context.Background(), base.Where(nameField.Eq("alice")))
				if err != nil || n != 1 {
					t.Errorf("concurrent count: %d %v", n, err)
				}
			}()
		}
		wg.Wait()
		if n, err := dao.Count(context.Background(), base); err != nil || n != 4 {
			t.Fatalf("wrapper was mutated: %d %v", n, err)
		}
	})

	t.Run("fill_plugin", func(t *testing.T) {
		db := openDB(t, integration)
		type actorKey struct{}
		actor := func(ctx context.Context) (any, error) {
			if value, ok := ctx.Value(actorKey{}).(string); ok {
				return value, nil
			}
			return nil, errors.New("actor required")
		}
		plugin := gormx.NewFillPlugin(
			gormx.FillRule{Field: "CreatedBy", OnCreate: true, Value: actor},
			gormx.FillRule{Field: "UpdatedBy", OnCreate: true, OnUpdate: true, Overwrite: true, Value: actor},
			gormx.FillRule{Field: "Unmapped", OnCreate: true, Value: actor},
		)
		if err := db.Use(plugin); err != nil {
			t.Fatal(err)
		}
		dao := newDAO(t, db)
		ctx := context.WithValue(context.Background(), actorKey{}, "operator")
		users := []user{{Name: "a"}, {Name: "b", CreatedBy: "imported"}}
		if err := dao.CreateBatch(ctx, users, 2); err != nil {
			t.Fatal(err)
		}
		if users[0].CreatedBy != "operator" || users[1].CreatedBy != "imported" || users[1].UpdatedBy != "operator" {
			t.Fatalf("batch fill: %+v", users)
		}
		changes := map[string]any{"Age": 0, "UpdatedBy": "forged"}
		if _, err := dao.UpdateByID(ctx, users[0].ID, changes); err != nil {
			t.Fatal(err)
		}
		got, err := dao.GetByID(ctx, users[0].ID)
		if err != nil || got.UpdatedBy != "operator" || changes["UpdatedBy"] != "forged" {
			t.Fatalf("update fill: %+v %v changes=%v", got, err, changes)
		}
		if err := dao.Create(context.Background(), &user{Name: "missing-actor"}); err == nil {
			t.Fatal("fill failure ignored")
		}
		if _, err := dao.UpdateByID(context.Background(), users[0].ID, map[string]any{"Name": "not-written"}); err == nil {
			t.Fatal("update fill failure ignored")
		}
		if n, err := dao.Count(ctx, nil); err != nil || n != 2 {
			t.Fatalf("failed insert persisted: %d %v", n, err)
		}
		got, err = dao.GetByID(ctx, users[0].ID)
		if err != nil || got.Name != "a" {
			t.Fatalf("failed update persisted: %+v %v", got, err)
		}
		if err := db.WithContext(ctx).Omit("CreatedBy").Create(&user{Name: "omitted"}).Error; err != nil {
			t.Fatal(err)
		}
		got, err = dao.First(ctx, gormx.NewWrapper[user]().Eq("Name", "omitted"))
		if err != nil || got.CreatedBy != "" {
			t.Fatalf("Omit ignored: %+v %v", got, err)
		}
	})
}

func TestConstructorAndInputs(t *testing.T) {
	db := openDB(t, false)
	if _, err := gormx.NewDAO[*user](db); !errors.Is(err, gormx.ErrInvalidModel) {
		t.Fatalf("pointer model: %v", err)
	}
	if _, err := gormx.NewDAO[int](db); !errors.Is(err, gormx.ErrInvalidModel) {
		t.Fatalf("scalar model: %v", err)
	}
	if _, err := gormx.NewDAO[user](nil); err == nil {
		t.Fatal("nil DB accepted")
	}
	dao := newDAO(t, db)
	if err := dao.Create(context.Background(), nil); err == nil {
		t.Fatal("nil entity accepted")
	}
	if err := dao.CreateBatch(context.Background(), nil, 0); err == nil {
		t.Fatal("zero batch size accepted")
	}
	if err := dao.CreateBatch(context.Background(), nil, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := dao.GetByID(context.Background(), nil); err == nil {
		t.Fatal("nil ID accepted")
	}
	if err := dao.Transaction(context.Background(), nil); err == nil {
		t.Fatal("nil transaction callback accepted")
	}
	if err := db.Use(gormx.NewFillPlugin(gormx.FillRule{Field: "CreatedBy"})); err == nil {
		t.Fatal("invalid fill rule accepted")
	}
}

type EmbeddedAudit struct {
	Owner string `gorm:"column:owner_name"`
}

type embeddedModel struct {
	ID             uint
	*EmbeddedAudit `gorm:"embedded;embeddedPrefix:audit_"`
}

func TestEmbeddedSelector(t *testing.T) {
	db := openDB(t, false)
	if err := db.AutoMigrate(&embeddedModel{}); err != nil {
		t.Fatal(err)
	}
	dao, err := gormx.NewDAO[embeddedModel](db)
	if err != nil {
		t.Fatal(err)
	}
	if err := dao.Create(context.Background(), &embeddedModel{EmbeddedAudit: &EmbeddedAudit{Owner: "alice"}}); err != nil {
		t.Fatal(err)
	}
	owner := gormx.Field(func(m *embeddedModel) *string { return &m.Owner })
	if n, err := dao.Count(context.Background(), gormx.NewLambda[embeddedModel]().Where(owner.Eq("alice"))); err != nil || n != 1 {
		t.Fatalf("embedded selector: %d %v", n, err)
	}
}

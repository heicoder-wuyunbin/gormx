package gormx_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/heicoder-wuyunbin/gormx"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

type eventRecord struct {
	ID     int64
	UserID uint
	Action string `gorm:"size:100"`
}

type permissionRecord struct {
	ID         uint
	Name       string `gorm:"size:100"`
	Owner      string `gorm:"size:100"`
	CreateOnly string `gorm:"size:100;<-:create"`
	ReadOnly   string `gorm:"size:100;->"`
}

func (p *permissionRecord) BeforeCreate(*gorm.DB) error {
	if p.Owner == "" {
		return errors.New("create hook did not receive filled owner")
	}
	return nil
}

func (p *permissionRecord) BeforeUpdate(tx *gorm.DB) error {
	if values, ok := tx.Statement.Dest.(map[string]any); ok {
		if _, exists := values["owner"]; !exists {
			return errors.New("update hook did not receive filled destination")
		}
	}
	return nil
}

func migrateExtra(t *testing.T, db *gorm.DB, models ...any) {
	t.Helper()
	t.Cleanup(func() {
		if err := db.Migrator().DropTable(models...); err != nil {
			t.Error(err)
		}
	})
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteExtendedContract(t *testing.T) { runExtendedContract(t, false) }
func TestMySQLExtendedContract(t *testing.T)  { runExtendedContract(t, true) }

func runExtendedContract(t *testing.T, integration bool) {
	t.Run("query_edges_and_copying", func(t *testing.T) {
		db := openDB(t, integration)
		dao := newDAO(t, db)
		seed(t, dao)
		ctx := context.Background()
		var empty *gormx.Wrapper[user]
		list, err := dao.List(ctx, empty.Eq("Name", "absent"))
		if err != nil || list == nil || len(list) != 0 {
			t.Fatalf("empty list contract: %v %v", list, err)
		}
		if _, err := dao.First(ctx, empty.Eq("Name", "absent")); !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("missing record: %v", err)
		}
		ids := []uint{1, 2}
		q := empty.In("ID", ids).OrderByAsc("ID")
		ids[0] = 999
		page, err := dao.SelectPage(ctx, q, gormx.PageRequest{Size: 500})
		if err != nil || page.Total != 2 || len(page.Records) != 2 || page.Records[0].ID != 1 {
			t.Fatalf("IN copy / page maximum: %+v %v", page, err)
		}
		if n, err := dao.Count(ctx, empty.In("ID", [2]uint{1, 2})); err != nil || n != 2 {
			t.Fatalf("array IN: %d %v", n, err)
		}
		note := gormx.Field(func(u *user) **string { return &u.Note })
		if n, err := dao.Count(ctx, empty.Where(note.IsNull())); err != nil || n != 3 {
			t.Fatalf("typed null: %d %v", n, err)
		}
		for _, invalid := range []*gormx.Wrapper[user]{
			empty.OrderBy(gormx.Order[user]{}),
			empty.OrderBy(gormx.Column[user, int]{}.Asc()),
			empty.Where(gormx.Column[user, int]{}.In(1)),
			empty.In("Ignored", []int{1}),
			empty.Where(gormx.Field(func(*user) *int { return nil }).Eq(1)),
			empty.Where(gormx.Field(func(u *user) *string { return &u.Ignored }).Eq("x")),
			empty.And(func(*gormx.Wrapper[user]) *gormx.Wrapper[user] { return nil }),
			empty.Where(gormx.All(gormx.Predicate[user]{})),
		} {
			if _, err := dao.SelectPage(ctx, invalid, gormx.PageRequest{}); err == nil {
				t.Fatal("invalid page query accepted")
			}
			if _, err := dao.First(ctx, invalid); err == nil {
				t.Fatal("invalid first query accepted")
			}
			if _, err := dao.List(ctx, invalid); err == nil {
				t.Fatal("invalid list query accepted")
			}
			if _, err := dao.Update(ctx, invalid, map[string]any{"Age": 1}); err == nil {
				t.Fatal("invalid update query accepted")
			}
			if _, err := dao.Delete(ctx, invalid); err == nil {
				t.Fatal("invalid delete query accepted")
			}
		}
		for _, id := range []any{nil, "bad-id"} {
			if _, err := dao.UpdateByID(ctx, id, map[string]any{"Age": 1}); !errors.Is(err, gormx.ErrInvalidID) {
				t.Fatalf("update ID: %v", err)
			}
			if _, err := dao.DeleteByID(ctx, id); !errors.Is(err, gormx.ErrInvalidID) {
				t.Fatalf("delete ID: %v", err)
			}
		}
	})

	t.Run("transactions_and_restore", func(t *testing.T) {
		db := openDB(t, integration)
		migrateExtra(t, db, &eventRecord{})
		dao := newDAO(t, db)
		users := seed(t, dao)
		ctx := context.Background()
		err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			usersDAO := newDAO(t, tx)
			events, err := gormx.NewDAO[eventRecord](tx)
			if err != nil {
				return err
			}
			if _, err := usersDAO.UpdateByID(ctx, users[0].ID, map[string]any{"Age": 42}); err != nil {
				return err
			}
			return events.Create(ctx, &eventRecord{UserID: users[0].ID, Action: "updated"})
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := dao.GetByID(ctx, users[0].ID)
		if err != nil || got.Age != 42 {
			t.Fatalf("commit: %+v %v", got, err)
		}
		events, err := gormx.NewDAO[eventRecord](db)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := events.GetByID(ctx, "1"); err != nil {
			t.Fatalf("signed primary key: %v", err)
		}
		if _, err := events.GetByID(ctx, "9223372036854775808"); !errors.Is(err, gormx.ErrInvalidID) {
			t.Fatalf("signed key overflow: %v", err)
		}
		sentinel := errors.New("nested rollback")
		if err := dao.Transaction(ctx, func(outer *gormx.DAO[user]) error {
			err := outer.Transaction(ctx, func(inner *gormx.DAO[user]) error {
				if err := inner.Create(ctx, &user{Name: "inner"}); err != nil {
					return err
				}
				return sentinel
			})
			if !errors.Is(err, sentinel) {
				return fmt.Errorf("unexpected nested error: %v", err)
			}
			return outer.Create(ctx, &user{Name: "outer"})
		}); err != nil {
			t.Fatal(err)
		}
		if n, err := dao.Count(ctx, gormx.NewWrapper[user]().Eq("Name", "inner")); err != nil || n != 0 {
			t.Fatalf("savepoint rollback: %d %v", n, err)
		}
		if n, err := dao.Count(ctx, nil); err != nil || n != 5 {
			t.Fatalf("outer commit: %d %v", n, err)
		}
		if _, err := dao.DeleteByID(ctx, users[0].ID); err != nil {
			t.Fatal(err)
		}
		if err := db.Unscoped().Model(&user{}).Where("id = ?", users[0].ID).Update("deleted_at", nil).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := dao.GetByID(ctx, users[0].ID); err != nil {
			t.Fatalf("restore: %v", err)
		}
		if err := db.Unscoped().Delete(&user{}, users[0].ID).Error; err != nil {
			t.Fatal(err)
		}
		if n, err := dao.Count(ctx, nil); err != nil || n != 4 {
			t.Fatalf("physical cleanup: %d %v", n, err)
		}
	})

	t.Run("fill_permissions_and_hooks", func(t *testing.T) {
		db := openDB(t, integration)
		migrateExtra(t, db, &permissionRecord{})
		var calls int
		resolve := func(context.Context) (any, error) {
			calls++
			return "system", nil
		}
		if err := db.Use(gormx.NewFillPlugin(
			gormx.FillRule{Field: "Owner", OnCreate: true, OnUpdate: true, Overwrite: true, Value: resolve},
			gormx.FillRule{Field: "CreateOnly", OnCreate: true, OnUpdate: true, Overwrite: true, Value: resolve},
			gormx.FillRule{Field: "ReadOnly", OnCreate: true, OnUpdate: true, Value: resolve},
			gormx.FillRule{Field: "ID", OnCreate: true, Value: resolve},
		)); err != nil {
			t.Fatal(err)
		}
		dao, err := gormx.NewDAO[permissionRecord](db)
		if err != nil {
			t.Fatal(err)
		}
		entities := []permissionRecord{{Name: "a"}, {Name: "b"}, {Name: "c"}}
		ctx := context.Background()
		if err := dao.CreateBatch(ctx, entities, 2); err != nil {
			t.Fatal(err)
		}
		if calls != 4 {
			t.Fatalf("expected two rules per batch statement, got %d", calls)
		}
		if _, err := dao.UpdateByID(ctx, entities[0].ID, map[string]any{"Name": "new"}); err != nil {
			t.Fatal(err)
		}
		if calls != 5 {
			t.Fatalf("update-only permissions ignored: %d", calls)
		}
		for _, column := range []string{"CreateOnly", "ReadOnly"} {
			if _, err := dao.UpdateByID(ctx, entities[0].ID, map[string]any{column: "x"}); !errors.Is(err, gormx.ErrInvalidField) {
				t.Fatalf("field permission %s: %v", column, err)
			}
		}
		explicit := permissionRecord{Name: "selected", Owner: "explicit"}
		if err := db.Select("Name", "Owner").Create(&explicit).Error; err != nil {
			t.Fatal(err)
		}
		got, err := dao.GetByID(ctx, explicit.ID)
		if err != nil || got.CreateOnly != "" || got.Owner != "system" || got.ReadOnly != "" {
			t.Fatalf("Select/permission handling: %+v %v", got, err)
		}
		before := calls
		if _, err := dao.DeleteByID(ctx, entities[0].ID); err != nil || calls != before {
			t.Fatalf("delete unexpectedly invoked update fill: %v", err)
		}
	})
}

func TestMySQLRepeatableRead(t *testing.T) {
	db := openDB(t, true)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(2)
	dao := newDAO(t, db)
	seed(t, dao)
	ctx := context.Background()
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		snapshot := newDAO(t, tx)
		n, err := snapshot.Count(ctx, nil)
		if err != nil || n != 4 {
			return fmt.Errorf("initial snapshot: %d %v", n, err)
		}
		// A second connection commits after the transaction's snapshot is established.
		if err := dao.Create(ctx, &user{Name: "concurrent"}); err != nil {
			return err
		}
		page, err := snapshot.SelectPage(ctx, nil, gormx.PageRequest{})
		if err != nil {
			return err
		}
		if page.Total != 4 || len(page.Records) != 4 {
			return fmt.Errorf("snapshot changed: %+v", page)
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := dao.Count(ctx, nil); err != nil || n != 5 {
		t.Fatalf("outside snapshot: %d %v", n, err)
	}
}

type namingRecord struct {
	ID          uint
	DisplayName string
}

func TestFieldCacheAcrossNamingStrategies(t *testing.T) {
	name := gormx.Field(func(m *namingRecord) *string { return &m.DisplayName })
	for _, replacement := range []string{"Public", "Private"} {
		t.Run(replacement, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
				NamingStrategy: schema.NamingStrategy{NameReplacer: strings.NewReplacer("Display", replacement)},
			})
			if err != nil {
				t.Fatal(err)
			}
			sqlDB, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			sqlDB.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = sqlDB.Close() })
			if err := db.AutoMigrate(&namingRecord{}); err != nil {
				t.Fatal(err)
			}
			dao, err := gormx.NewDAO[namingRecord](db)
			if err != nil {
				t.Fatal(err)
			}
			if err := dao.Create(context.Background(), &namingRecord{DisplayName: "alice"}); err != nil {
				t.Fatal(err)
			}
			if n, err := dao.Count(context.Background(), gormx.NewLambda[namingRecord]().Where(name.Eq("alice"))); err != nil || n != 1 {
				t.Fatalf("naming strategy cache contamination: %d %v", n, err)
			}
		})
	}
}

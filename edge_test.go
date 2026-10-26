package gormx_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/heicoder-wuyunbin/gormx"

	"gorm.io/gorm"
)

type composite struct {
	Tenant string `gorm:"primaryKey"`
	Key    string `gorm:"primaryKey"`
}

type stringKey struct {
	Key  string `gorm:"primaryKey"`
	Name string
}

type deletePermission struct{}

type guarded struct {
	ID uint
	gorm.DeletedAt
}

func (*guarded) BeforeDelete(tx *gorm.DB) error {
	if allowed, _ := tx.Statement.Context.Value(deletePermission{}).(bool); !allowed {
		return errors.New("delete denied")
	}
	return nil
}

func TestPrimaryKeysAndDeleteHooks(t *testing.T) {
	db := openDB(t, false)
	if err := db.AutoMigrate(&composite{}, &stringKey{}, &guarded{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	keys, err := gormx.NewDAO[composite](db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.GetByID(ctx, "x"); err == nil {
		t.Fatal("composite key accepted by ByID")
	}
	record := composite{Tenant: "tenant-a", Key: "x"}
	if err := keys.Create(ctx, &record); err != nil {
		t.Fatal(err)
	}
	if rows, err := keys.Delete(ctx, gormx.NewWrapper[composite]().Eq("Tenant", "tenant-a").Eq("Key", "x")); err != nil || rows != 1 {
		t.Fatalf("composite delete: %d %v", rows, err)
	}
	strings, err := gormx.NewDAO[stringKey](db)
	if err != nil {
		t.Fatal(err)
	}
	if err := strings.Create(ctx, &stringKey{Key: "custom-id", Name: "alice"}); err != nil {
		t.Fatal(err)
	}
	if got, err := strings.GetByID(ctx, "custom-id"); err != nil || got.Name != "alice" {
		t.Fatalf("string key: %+v %v", got, err)
	}
	users := newDAO(t, db)
	seed(t, users)
	if got, err := users.GetByID(ctx, "1"); err != nil || got.ID != 1 {
		t.Fatalf("numeric string key: %+v %v", got, err)
	}
	for _, id := range []any{"", "-1", "1x", "1.2", []int{1}, map[string]int{"ID": 1}, 1.0} {
		if _, err := users.GetByID(ctx, id); !errors.Is(err, gormx.ErrInvalidID) {
			t.Fatalf("id %v: %v", id, err)
		}
	}
	deletes, err := gormx.NewDAO[guarded](db)
	if err != nil {
		t.Fatal(err)
	}
	entity := guarded{}
	if err := deletes.Create(ctx, &entity); err != nil {
		t.Fatal(err)
	}
	if _, err := deletes.DeleteByID(ctx, entity.ID); err == nil {
		t.Fatal("BeforeDelete hook bypassed")
	}
	if n, err := deletes.Count(ctx, nil); err != nil || n != 1 {
		t.Fatalf("denied delete persisted: %d %v", n, err)
	}
	allowed := context.WithValue(ctx, deletePermission{}, true)
	if rows, err := deletes.DeleteByID(allowed, entity.ID); err != nil || rows != 1 {
		t.Fatalf("allowed delete: %d %v", rows, err)
	}
}

func TestPageQueryErrors(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			db := openDB(t, false)
			dao := newDAO(t, db)
			seed(t, dao)
			calls := 0
			sentinel := errors.New("query failed")
			if err := db.Callback().Query().Before("gorm:query").Register("test:fail", func(tx *gorm.DB) {
				calls++
				if calls == failAt {
					tx.AddError(sentinel)
				}
			}); err != nil {
				t.Fatal(err)
			}
			page, err := dao.SelectPage(context.Background(), nil, gormx.PageRequest{})
			if !errors.Is(err, sentinel) || page != nil || calls != failAt {
				t.Fatalf("page=%+v err=%v calls=%d", page, err, calls)
			}
		})
	}
}

func TestConcurrentUpdates(t *testing.T) {
	db := openDB(t, false)
	dao := newDAO(t, db.Session(&gorm.Session{SkipDefaultTransaction: true}))
	users := seed(t, dao)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := dao.UpdateByID(context.Background(), users[0].ID, map[string]any{"Age": 40}); err != nil {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	got, err := dao.GetByID(context.Background(), users[0].ID)
	if err != nil || got.Age != 40 {
		t.Fatalf("concurrent update: %+v %v", got, err)
	}
}

func TestFillMapAndEmbeddedFields(t *testing.T) {
	db := openDB(t, false)
	if err := db.AutoMigrate(&embeddedModel{}); err != nil {
		t.Fatal(err)
	}
	plugin := gormx.NewFillPlugin(
		gormx.FillRule{Field: "Owner", OnCreate: true, Value: func(context.Context) (any, error) {
			return "system", nil
		}},
		gormx.FillRule{Field: "CreatedBy", OnCreate: true, OnUpdate: true, Value: func(context.Context) (any, error) {
			return "system", nil
		}},
	)
	if err := db.Use(plugin); err != nil {
		t.Fatal(err)
	}
	dao, err := gormx.NewDAO[embeddedModel](db)
	if err != nil {
		t.Fatal(err)
	}
	entity := embeddedModel{}
	if err := dao.Create(context.Background(), &entity); err != nil || entity.Owner != "system" {
		t.Fatalf("embedded fill: %+v %v", entity, err)
	}
	data := map[string]any{"Name": "map", "CreatedBy": ""}
	if err := db.Model(&user{}).Create(data).Error; err != nil {
		t.Fatal(err)
	}
	users := newDAO(t, db)
	got, err := users.First(context.Background(), gormx.NewWrapper[user]().Eq("Name", "map"))
	if err != nil || got.CreatedBy != "" {
		t.Fatalf("explicit map zero overwritten: %+v %v", got, err)
	}
	if _, err := users.UpdateByID(context.Background(), got.ID, map[string]any{"Age": 1}); err != nil {
		t.Fatal(err)
	}
	got, err = users.GetByID(context.Background(), got.ID)
	if err != nil || got.CreatedBy != "system" {
		t.Fatalf("missing map key not filled: %+v %v", got, err)
	}
	skipped := user{Name: "skip"}
	if err := db.Session(&gorm.Session{SkipHooks: true}).Create(&skipped).Error; err != nil || skipped.CreatedBy != "" {
		t.Fatalf("SkipHooks ignored: %+v %v", skipped, err)
	}
}

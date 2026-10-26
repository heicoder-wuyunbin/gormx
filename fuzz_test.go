package gormx

import (
	"errors"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type fuzzModel struct {
	ID   uint
	Name string `gorm:"column:display_name"`
}

func FuzzPageRequest(f *testing.F) {
	for _, seed := range [][2]int{{0, 0}, {1, 20}, {-1, 20}, {1, 501}, {int(^uint(0) >> 1), 500}} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, current, size int) {
		request := PageRequest{Current: current, Size: size}
		normalized, offset, err := request.normalize()
		if err != nil {
			if !errors.Is(err, ErrInvalidPage) {
				t.Fatalf("unexpected error: %v", err)
			}
			return
		}
		if normalized.Current < 1 || normalized.Size < 1 || normalized.Size > MaxPageSize || offset < 0 {
			t.Fatalf("invalid accepted request: %+v, offset=%d", normalized, offset)
		}
		if offset/normalized.Size != normalized.Current-1 || offset%normalized.Size != 0 {
			t.Fatalf("offset overflow: %+v, offset=%d", normalized, offset)
		}
		again, secondOffset, err := normalized.normalize()
		if err != nil || again != normalized || secondOffset != offset {
			t.Fatal("normalization is not idempotent")
		}
	})
}

func FuzzWrapperBinding(f *testing.F) {
	f.Add("Name", "' OR 1=1 --")
	f.Add("display_name", "")
	f.Add("Name; DROP TABLE users", "value")
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{DryRun: true})
	if err != nil {
		f.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { _ = sqlDB.Close() })
	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(&fuzzModel{}); err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, field, value string) {
		if len(field) > 4096 || len(value) > 4096 {
			t.Skip()
		}
		spec, err := NewWrapper[fuzzModel]().Eq(field, value).compile(stmt.Schema)
		expectedField := stmt.Schema.LookUpField(field)
		if expectedField == nil || expectedField.DBName == "" {
			if !errors.Is(err, ErrInvalidField) {
				t.Fatalf("unknown field was accepted: %q", field)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		query := spec.apply(db.Model(&fuzzModel{}), false).Find(&[]fuzzModel{}).Statement
		if len(query.Vars) != 1 || query.Vars[0] != value {
			t.Fatalf("value not bound as one parameter: %q %v", query.SQL.String(), query.Vars)
		}
		control, err := NewWrapper[fuzzModel]().Eq(field, "control").compile(stmt.Schema)
		if err != nil {
			t.Fatal(err)
		}
		controlSQL := control.apply(db.Model(&fuzzModel{}), false).Find(&[]fuzzModel{}).Statement.SQL.String()
		if query.SQL.String() != controlSQL {
			t.Fatalf("value changed SQL structure: %q vs %q", query.SQL.String(), controlSQL)
		}
	})
}

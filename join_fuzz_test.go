package gormx

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func FuzzJoinBinding(f *testing.F) {
	f.Add("e", "name", "' OR 1=1 --")
	f.Add("e; DROP", "name", "value")
	f.Add("d", "name", "value")
	f.Add("e", "*", "value")
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{DryRun: true})
	if err != nil {
		f.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { _ = sqlDB.Close() })
	f.Fuzz(func(t *testing.T, alias, output, value string) {
		if len(alias) > 100 || len(output) > 100 || len(value) > 4096 {
			t.Skip()
		}
		e, d := TableOf[fuzzModel](alias), TableOf[fuzzModel]("d")
		query, err := NewJoinQuery(db, e)
		if !joinIdentifier.MatchString(alias) {
			if !errors.Is(err, ErrInvalidQuery) {
				t.Fatalf("invalid table alias accepted: %q", alias)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		query = query.LeftJoin(d, On(e.Col("ID"), d.Col("ID"))).
			Where(e.Col("Name").Eq(value)).Select(d.Col("Name").As(output))
		plan, err := query.compile()
		if strings.EqualFold(alias, "d") || !joinIdentifier.MatchString(output) {
			if !errors.Is(err, ErrInvalidQuery) {
				t.Fatalf("invalid alias accepted: %q %q", alias, output)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		stmt := query.base(context.Background(), plan).
			Clauses(clause.Select{Columns: plan.selects}).Find(&[]fuzzModel{}).Statement
		if len(stmt.Vars) != 1 || stmt.Vars[0] != value {
			t.Fatalf("ordinary value was not bound: %s %v", stmt.SQL.String(), stmt.Vars)
		}
		if plan.selects[0].Table != "d" || plan.selects[0].Name != "display_name" || plan.selects[0].Alias != output {
			t.Fatalf("column/tag/alias changed: %+v", plan.selects)
		}
	})
}

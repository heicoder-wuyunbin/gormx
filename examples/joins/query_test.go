package joins

import (
	"context"
	"testing"

	"github.com/heicoder-wuyunbin/gormx"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestEmployeeDepartmentPage(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&Employee{}, &Department{}); err != nil {
		t.Fatal(err)
	}
	dept := Department{Name: "engineering", TenantID: 1}
	if err := db.Create(&dept).Error; err != nil {
		t.Fatal(err)
	}
	employees := []Employee{
		{Name: "alice", TenantID: 1, DepartmentID: &dept.ID},
		{Name: "bob", TenantID: 1},
		{Name: "other tenant", TenantID: 2},
	}
	if err := db.Create(&employees).Error; err != nil {
		t.Fatal(err)
	}
	page, err := Page(context.Background(), db, 1, "", gormx.PageRequest{})
	if err != nil || page.Total != 2 || len(page.Records) != 2 ||
		page.Records[0].DepartmentName == nil || *page.Records[0].DepartmentName != "engineering" ||
		page.Records[1].DepartmentName != nil {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	filtered, err := Page(context.Background(), db, 1, "ali", gormx.PageRequest{})
	if err != nil || filtered.Total != 1 || filtered.Records[0].EmployeeName != "alice" {
		t.Fatalf("filtered page=%+v err=%v", filtered, err)
	}
	if _, err := Page(context.Background(), nil, 1, "", gormx.PageRequest{}); err == nil {
		t.Fatal("nil database accepted")
	}
}

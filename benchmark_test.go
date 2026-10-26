package gormx

import (
	"context"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type benchmarkRecord struct {
	ID   uint
	Name string
	Age  int
}

var benchmarkQuery *Wrapper[benchmarkRecord]

func BenchmarkWrapperBuild(b *testing.B) {
	age := Field(func(m *benchmarkRecord) *int { return &m.Age })
	for _, style := range []string{"String", "Lambda"} {
		b.Run(style, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if style == "String" {
					benchmarkQuery = NewWrapper[benchmarkRecord]().Ge("Age", 18).Le("Age", 40).OrderByDesc("Age")
				} else {
					benchmarkQuery = NewLambda[benchmarkRecord]().Where(age.Ge(18), age.Le(40)).OrderBy(age.Desc())
				}
			}
		})
	}
}

// This measures local in-memory SQLite overhead, not production database throughput.
func BenchmarkPagination(b *testing.B) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		b.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		b.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	b.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&benchmarkRecord{}); err != nil {
		b.Fatal(err)
	}
	records := make([]benchmarkRecord, 10000)
	for i := range records {
		records[i] = benchmarkRecord{Name: "benchmark", Age: i % 80}
	}
	if err := db.CreateInBatches(&records, 500).Error; err != nil {
		b.Fatal(err)
	}
	dao, err := NewDAO[benchmarkRecord](db)
	if err != nil {
		b.Fatal(err)
	}
	age := Field(func(m *benchmarkRecord) *int { return &m.Age })
	ctx := context.Background()
	for _, style := range []string{"GORM", "String", "Lambda"} {
		b.Run(style, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if style == "GORM" {
					base := db.WithContext(ctx).Model(&benchmarkRecord{}).Where("age >= ?", 18)
					var total int64
					if err := base.Session(&gorm.Session{}).Count(&total).Error; err != nil {
						b.Fatal(err)
					}
					var rows []benchmarkRecord
					if err := base.Session(&gorm.Session{}).Order("age DESC, id ASC").Limit(20).Offset(20).Find(&rows).Error; err != nil {
						b.Fatal(err)
					}
					if total != 7750 || len(rows) != 20 {
						b.Fatal("unexpected baseline page")
					}
				} else {
					var q *Wrapper[benchmarkRecord]
					if style == "Lambda" {
						q = NewLambda[benchmarkRecord]().Where(age.Ge(18)).OrderBy(age.Desc())
					} else {
						q = NewWrapper[benchmarkRecord]().Ge("Age", 18).OrderByDesc("Age")
					}
					page, err := dao.SelectPage(ctx, q, PageRequest{Current: 2, Size: 20})
					if err != nil || page.Total != 7750 || len(page.Records) != 20 {
						b.Fatalf("unexpected page: %+v %v", page, err)
					}
				}
			}
		})
	}
}

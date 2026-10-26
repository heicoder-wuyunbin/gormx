package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/heicoder-wuyunbin/gormx"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestControllerParity(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&User{}); err != nil {
		t.Fatal(err)
	}
	users := []User{
		{Name: "alice", Age: 18, Status: 0},
		{Name: "adam", Age: 25, Status: 1},
		{Name: "bob", Age: 30, Status: 1},
	}
	if err := db.Create(&users).Error; err != nil {
		t.Fatal(err)
	}
	dao, err := gormx.NewDAO[User](db)
	if err != nil {
		t.Fatal(err)
	}
	c := &controller{db: db, dao: dao}
	for _, query := range []string{
		"", "?name=a&size=1&current=2", "?status=0", "?minAge=25", "?name=%27%20OR%201%3D1--", "?current=100",
	} {
		t.Run(query, func(t *testing.T) {
			var baseline gormx.Page[User]
			for i, handler := range []http.HandlerFunc{c.before, c.after, c.lambda} {
				response := httptest.NewRecorder()
				handler(response, httptest.NewRequest(http.MethodGet, "/users"+query, nil))
				if response.Code != http.StatusOK {
					t.Fatalf("handler %d: %d %s", i, response.Code, response.Body.String())
				}
				var page gormx.Page[User]
				if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					baseline = page
				} else if !reflect.DeepEqual(baseline, page) {
					t.Fatalf("handler %d differs: %+v vs %+v", i, page, baseline)
				}
				if query == "?status=0" && (page.Total != 1 || page.Records[0].Name != "alice") {
					t.Fatal("zero-valued status was ignored")
				}
			}
		})
	}
	for _, query := range []string{"?size=501", "?size=0", "?current=-1", "?minAge=no", "?status=2"} {
		for _, handler := range []http.HandlerFunc{c.before, c.after, c.lambda} {
			response := httptest.NewRecorder()
			handler(response, httptest.NewRequest(http.MethodGet, "/users"+query, nil))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("%s returned %d", query, response.Code)
			}
		}
	}
}

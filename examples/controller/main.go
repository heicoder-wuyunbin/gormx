package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/heicoder-wuyunbin/gormx"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// User is the model used by the controller example. It has a numeric primary
// key and a soft-delete marker, so Delete issues an UPDATE instead of removing
// the row. JSON tags shape the API response; the DeletedAt field is hidden.
type User struct {
	ID        uint           `json:"id" gorm:"primaryKey"`
	Name      string         `json:"name" gorm:"size:100;index"`
	Age       int            `json:"age"`
	Status    int            `json:"status" gorm:"index"`
	CreatedAt time.Time      `json:"createdAt"`
	UpdatedAt time.Time      `json:"updatedAt"`
	DeletedAt gorm.DeletedAt `json:"-" gorm:"index"`
}

var (
	userName   = gormx.Field(func(u *User) *string { return &u.Name })
	userAge    = gormx.Field(func(u *User) *int { return &u.Age })
	userStatus = gormx.Field(func(u *User) *int { return &u.Status })
)

type controller struct {
	db  *gorm.DB
	dao *gormx.DAO[User]
}

type filters struct {
	Name   string
	Status *int
	MinAge *int
	Page   gormx.PageRequest
}

func parseFilters(r *http.Request) (filters, error) {
	query := r.URL.Query()
	f := filters{Name: query.Get("name"), Page: gormx.PageRequest{Current: 1, Size: 20}}
	for key, target := range map[string]*int{"current": &f.Page.Current, "size": &f.Page.Size} {
		if raw := query.Get(key); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 1 {
				return f, fmt.Errorf("%s must be a positive integer", key)
			}
			*target = value
		}
	}
	if f.Page.Size > gormx.MaxPageSize || f.Page.Current-1 > int(^uint(0)>>1)/f.Page.Size {
		return f, fmt.Errorf("page size or offset is too large")
	}
	for key, target := range map[string]**int{"status": &f.Status, "minAge": &f.MinAge} {
		if raw := query.Get(key); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 0 || (key == "status" && value > 1) {
				return f, fmt.Errorf("invalid %s", key)
			}
			*target = &value
		}
	}
	return f, nil
}

// Before: each endpoint repeats conditions, count, pagination and page assembly.
func (c *controller) before(w http.ResponseWriter, r *http.Request) {
	f, err := parseFilters(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	query := c.db.WithContext(r.Context()).Model(&User{})
	if f.Name != "" {
		query = query.Where("name LIKE ?", "%"+f.Name+"%")
	}
	if f.Status != nil {
		query = query.Where("status = ?", *f.Status)
	}
	if f.MinAge != nil {
		query = query.Where("age >= ?", *f.MinAge)
	}
	page := &gormx.Page[User]{Records: make([]User, 0), Current: f.Page.Current, Size: f.Page.Size}
	if err := query.Session(&gorm.Session{}).Count(&page.Total).Error; err != nil {
		writeResult(w, nil, err)
		return
	}
	err = query.Session(&gorm.Session{}).Order("age DESC, id ASC").
		Offset((f.Page.Current - 1) * f.Page.Size).Limit(f.Page.Size).Find(&page.Records).Error
	page.Pages = page.Total / int64(page.Size)
	if page.Total%int64(page.Size) != 0 {
		page.Pages++
	}
	writeResult(w, page, err)
}

// After: no SQL fragments and no endpoint-specific DAO method.
func (c *controller) after(w http.ResponseWriter, r *http.Request) {
	f, err := parseFilters(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	query := gormx.NewWrapper[User]().
		LikeIf(f.Name != "", "Name", "%"+f.Name+"%").
		OrderByDesc("Age")
	if f.Status != nil {
		query = query.Eq("Status", *f.Status)
	}
	if f.MinAge != nil {
		query = query.Ge("Age", *f.MinAge)
	}
	page, err := c.dao.SelectPage(r.Context(), query, f.Page)
	writeResult(w, page, err)
}

// Lambda style: renames and mismatched value types are caught by the compiler.
func (c *controller) lambda(w http.ResponseWriter, r *http.Request) {
	f, err := parseFilters(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	query := gormx.NewLambda[User]().
		WhereIf(f.Name != "", gormx.Like(userName, "%"+f.Name+"%")).
		OrderBy(userAge.Desc())
	if f.Status != nil {
		query = query.Where(userStatus.Eq(*f.Status))
	}
	if f.MinAge != nil {
		query = query.Where(userAge.Ge(*f.MinAge))
	}
	page, err := c.dao.SelectPage(r.Context(), query, f.Page)
	writeResult(w, page, err)
}

func writeResult(w http.ResponseWriter, result any, err error) {
	if err != nil {
		log.Printf("query failed: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		log.Printf("response failed: %v", err)
	}
}

func main() {
	dsn := os.Getenv("GORMX_DSN")
	if dsn == "" {
		log.Fatal("set GORMX_DSN to a MySQL DSN; credentials are never hard-coded")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		log.Fatal(err)
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(20)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(5 * time.Minute)
	// Opt-in only: production schema changes should use reviewed migrations.
	if os.Getenv("GORMX_AUTO_MIGRATE") == "1" {
		if err := db.AutoMigrate(&User{}); err != nil {
			log.Fatal(err)
		}
	}
	dao, err := gormx.NewDAO[User](db)
	if err != nil {
		log.Fatal(err)
	}
	c := &controller{db: db, dao: dao}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /before/users", c.before)
	mux.HandleFunc("GET /users", c.after)
	mux.HandleFunc("GET /lambda/users", c.lambda)
	server := &http.Server{
		Addr: "127.0.0.1:8080", Handler: mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("listening on http://%s", server.Addr)
	log.Fatal(server.ListenAndServe())
}

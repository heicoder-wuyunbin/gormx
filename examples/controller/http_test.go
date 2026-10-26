package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/heicoder-wuyunbin/gormx"

	mysqlconfig "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestControllerDatabaseErrors(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			if err != nil {
				t.Fatal(err)
			}
			sqlDB, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			sqlDB.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = sqlDB.Close() })
			if err := db.AutoMigrate(&User{}); err != nil {
				t.Fatal(err)
			}
			dao, err := gormx.NewDAO[User](db)
			if err != nil {
				t.Fatal(err)
			}
			c := &controller{db: db, dao: dao}
			calls := 0
			if err := db.Callback().Query().Before("gorm:query").Register("test:failure", func(tx *gorm.DB) {
				calls++
				if calls == failAt {
					tx.AddError(errors.New("private database diagnostic"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			for _, handler := range []http.HandlerFunc{c.before, c.after, c.lambda} {
				calls = 0
				response := httptest.NewRecorder()
				handler(response, httptest.NewRequest(http.MethodGet, "/users", nil))
				if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "private") || calls != failAt {
					t.Fatalf("incorrect error response: %d %q calls=%d", response.Code, response.Body.String(), calls)
				}
			}
		})
	}
	// Encoder failures should be handled without a panic or a second response.
	writeResult(httptest.NewRecorder(), make(chan int), nil)
}

func buildController(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "controller")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	output, err := exec.CommandContext(ctx, "go", "build", "-o", binary, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("build controller: %v\n%s", err, output)
	}
	return binary
}

func exampleEnv() []string {
	env := make([]string, 0)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GORMX_") {
			env = append(env, entry)
		}
	}
	return env
}

func TestControllerStartupValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("process checks are omitted in short mode")
	}
	binary := buildController(t)
	for _, tc := range []struct {
		name string
		dsn  string
		want string
	}{
		{"missing_dsn", "", "set GORMX_DSN"},
		{"invalid_dsn", "not-a-dsn", "invalid DSN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, binary)
			command.Env = append(exampleEnv(), "GORMX_DSN="+tc.dsn)
			output, err := command.CombinedOutput()
			if err == nil || !strings.Contains(string(output), tc.want) {
				t.Fatalf("startup validation: %v\n%s", err, output)
			}
		})
	}
}

// This opt-in test requires CREATE/DROP DATABASE privileges and a free port 8080.
// It creates a private schema; it never migrates the schema named in the input DSN.
func TestControllerMySQLProcess(t *testing.T) {
	dsn := os.Getenv("GORMX_HTTP_TEST_DSN")
	if dsn == "" {
		t.Skip("set GORMX_HTTP_TEST_DSN for the real MySQL HTTP process test")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:8080")
	if err != nil {
		t.Fatalf("HTTP process test requires an unused localhost:8080: %v", err)
	}
	_ = listener.Close()
	config, err := mysqlconfig.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.DBName = ""
	admin, err := gorm.Open(mysql.Open(config.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	adminSQL, err := admin.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminSQL.Close() })
	database := fmt.Sprintf("gx_http_%d", time.Now().UnixNano())
	if err := admin.Exec("CREATE DATABASE `" + database + "` CHARACTER SET utf8mb4").Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admin.Exec("DROP DATABASE `" + database + "`").Error; err != nil {
			t.Error(err)
		}
	})
	config.DBName = database
	config.ParseTime = true
	privateDSN := config.FormatDSN()
	db, err := gorm.Open(mysql.Open(privateDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	binary := buildController(t)
	client := &http.Client{Timeout: time.Second}
	stop := startController(t, binary, privateDSN, true, client)
	if !db.Migrator().HasTable(&User{}) {
		t.Fatal("opt-in AutoMigrate did not create the table")
	}
	users := []User{{Name: "alice", Age: 18, Status: 0}, {Name: "adam", Age: 25, Status: 1}, {Name: "bob", Age: 30, Status: 1}}
	if err := db.Create(&users).Error; err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"", "?name=a&size=1&current=2", "?status=0", "?minAge=25", "?current=100"} {
		var baseline gormx.Page[User]
		for i, path := range []string{"/before/users", "/users", "/lambda/users"} {
			response, err := client.Get("http://127.0.0.1:8080" + path + query)
			if err != nil {
				t.Fatal(err)
			}
			var page gormx.Page[User]
			err = json.NewDecoder(response.Body).Decode(&page)
			_ = response.Body.Close()
			if err != nil || response.StatusCode != http.StatusOK {
				t.Fatalf("HTTP result: %d %v", response.StatusCode, err)
			}
			if i == 0 {
				baseline = page
			} else if !reflect.DeepEqual(page, baseline) {
				t.Fatalf("live endpoint mismatch: %+v vs %+v", page, baseline)
			}
			if query == "?status=0" && (page.Total != 1 || len(page.Records) != 1 || page.Records[0].Name != "alice") {
				t.Fatal("live endpoint lost zero-valued status")
			}
		}
	}
	for _, tc := range []struct {
		method string
		path   string
		code   int
	}{
		{"GET", "/users?size=501", 400},
		{"GET", "/users?status=2", 400},
		{"POST", "/users", 405},
		{"GET", "/missing", 404},
	} {
		request, err := http.NewRequest(tc.method, "http://127.0.0.1:8080"+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != tc.code {
			t.Fatalf("%s %s: %d", tc.method, tc.path, response.StatusCode)
		}
	}
	stop()
	// Verify normal startup against the existing schema without AutoMigrate.
	stop = startController(t, binary, privateDSN, false, client)
	defer stop()
	var total int64
	if err := db.Model(&User{}).Count(&total).Error; err != nil || total != 3 {
		t.Fatalf("restart changed data: %d %v", total, err)
	}
}

func startController(t *testing.T, binary, dsn string, migrate bool, client *http.Client) func() {
	t.Helper()
	logFile, err := os.Create(filepath.Join(t.TempDir(), "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary)
	command.Env = append(exampleEnv(), "GORMX_DSN="+dsn)
	if migrate {
		command.Env = append(command.Env, "GORMX_AUTO_MIGRATE=1")
	}
	command.Stdout, command.Stderr = logFile, logFile
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(done)
	}()
	stop := func() {
		select {
		case <-done:
		default:
			_ = command.Process.Kill()
			<-done
		}
		_ = logFile.Close()
	}
	t.Cleanup(stop)
	timeout := time.NewTimer(15 * time.Second)
	defer timeout.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			logs, _ := os.ReadFile(logFile.Name())
			t.Fatalf("controller exited before readiness:\n%s", logs)
		case <-timeout.C:
			t.Fatal("controller did not become ready")
		case <-ticker.C:
			response, err := client.Get("http://127.0.0.1:8080/users")
			if err == nil {
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if response.StatusCode == http.StatusOK {
					return stop
				}
			}
		}
	}
}

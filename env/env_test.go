package env_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"github.com/heicoder-wuyunbin/gormx/env"
)

var envSuffixes = []string{
	"DSN", "HOST", "PORT", "USER", "PASSWORD", "NAME", "CHARSET", "LOC",
	"MAX_OPEN_CONNS", "MAX_IDLE_CONNS", "CONN_MAX_LIFETIME", "CONNECT_TIMEOUT",
}

// clearEnv blanks every key under prefix so ambient variables cannot leak in.
// Empty values count as unset, which is the documented contract.
func clearEnv(t *testing.T, prefix string) {
	t.Helper()
	for _, suffix := range envSuffixes {
		t.Setenv(prefix+suffix, "")
	}
}

func devDefaults() env.MySQLConfig {
	return env.MySQLConfig{
		Host:     "192.168.100.135",
		Port:     3306,
		User:     "root",
		Password: "root1234",
		Database: "demo",
	}
}

func TestLoadFallsBackToDefaultsAndNeutralValues(t *testing.T) {
	clearEnv(t, "DB_")

	cfg, err := env.Load("DB_", devDefaults())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Host != "192.168.100.135" || cfg.Port != 3306 || cfg.User != "root" ||
		cfg.Password != "root1234" || cfg.Database != "demo" {
		t.Fatalf("defaults lost: %+v", cfg)
	}
	if cfg.Charset != env.DefaultCharset {
		t.Fatalf("charset = %q", cfg.Charset)
	}
	if cfg.Loc != time.Local {
		t.Fatalf("loc = %v", cfg.Loc)
	}
	if cfg.MaxOpenConns != env.DefaultMaxOpenConns || cfg.MaxIdleConns != env.DefaultMaxIdleConns {
		t.Fatalf("pool = %d/%d", cfg.MaxOpenConns, cfg.MaxIdleConns)
	}
	if cfg.ConnMaxLifetime != env.DefaultConnMaxLifetime || cfg.ConnectTimeout != env.DefaultConnectTimeout {
		t.Fatalf("durations = %v/%v", cfg.ConnMaxLifetime, cfg.ConnectTimeout)
	}
}

func TestLoadNeutralValuesWhenDefaultsAreZero(t *testing.T) {
	clearEnv(t, "DB_")

	cfg, err := env.Load("DB_", env.MySQLConfig{Host: "127.0.0.1"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != env.DefaultPort || cfg.Charset != env.DefaultCharset || cfg.Loc != time.Local {
		t.Fatalf("neutral values missing: %+v", cfg)
	}
}

func TestLoadEnvOverridesDefaults(t *testing.T) {
	clearEnv(t, "DB_")
	t.Setenv("DB_HOST", "mysql-server")
	t.Setenv("DB_PORT", "3307")
	t.Setenv("DB_USER", "app")
	t.Setenv("DB_PASSWORD", " pass with spaces ")
	t.Setenv("DB_NAME", "appdb")
	t.Setenv("DB_CHARSET", "utf8mb4")
	t.Setenv("DB_LOC", "UTC")
	t.Setenv("DB_MAX_OPEN_CONNS", "30")
	t.Setenv("DB_MAX_IDLE_CONNS", "7")
	t.Setenv("DB_CONN_MAX_LIFETIME", "90s")
	t.Setenv("DB_CONNECT_TIMEOUT", "3s")

	cfg, err := env.Load("DB_", devDefaults())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Host != "mysql-server" || cfg.Port != 3307 || cfg.User != "app" ||
		cfg.Password != " pass with spaces " || cfg.Database != "appdb" {
		t.Fatalf("env not applied: %+v", cfg)
	}
	if cfg.Loc != time.UTC {
		t.Fatalf("loc = %v", cfg.Loc)
	}
	if cfg.MaxOpenConns != 30 || cfg.MaxIdleConns != 7 {
		t.Fatalf("pool = %d/%d", cfg.MaxOpenConns, cfg.MaxIdleConns)
	}
	if cfg.ConnMaxLifetime != 90*time.Second || cfg.ConnectTimeout != 3*time.Second {
		t.Fatalf("durations = %v/%v", cfg.ConnMaxLifetime, cfg.ConnectTimeout)
	}
}

func TestLoadAcceptsPrefixWithOrWithoutUnderscore(t *testing.T) {
	clearEnv(t, "APP_")

	t.Setenv("APP_HOST", "10.0.0.8")
	for _, prefix := range []string{"APP", "APP_"} {
		cfg, err := env.Load(prefix, devDefaults())
		if err != nil {
			t.Fatalf("Load(%q): %v", prefix, err)
		}
		if cfg.Host != "10.0.0.8" {
			t.Fatalf("Load(%q) host = %q", prefix, cfg.Host)
		}
	}
}

func TestLoadFullDSNTakesPrecedence(t *testing.T) {
	clearEnv(t, "DB_")
	t.Setenv("DB_PORT", "9999")
	dsn := "app:secret@tcp(127.0.0.1:3306)/demo?charset=utf8mb4&parseTime=True&loc=Local"
	t.Setenv("DB_DSN", dsn)

	cfg, err := env.Load("DB_", env.MySQLConfig{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DSN != dsn {
		t.Fatalf("DSN = %q", cfg.DSN)
	}
	if cfg.Port != 9999 {
		t.Fatalf("parts should still be recorded, port = %d", cfg.Port)
	}
	built, err := cfg.BuildDSN()
	if err != nil {
		t.Fatalf("BuildDSN: %v", err)
	}
	if built != dsn {
		t.Fatalf("BuildDSN = %q", built)
	}
	if cfg.Address() != "configured dsn" {
		t.Fatalf("Address = %q", cfg.Address())
	}
}

func TestLoadRejectsMalformedValues(t *testing.T) {
	tests := []struct {
		name   string
		key    string
		value  string
		expect string
	}{
		{name: "port not a number", key: "DB_PORT", value: "abc", expect: "DB_PORT"},
		{name: "port out of range", key: "DB_PORT", value: "70000", expect: "DB_PORT"},
		{name: "bad duration", key: "DB_CONN_MAX_LIFETIME", value: "5 minutes", expect: "DB_CONN_MAX_LIFETIME"},
		{name: "negative duration", key: "DB_CONNECT_TIMEOUT", value: "-1s", expect: "DB_CONNECT_TIMEOUT"},
		{name: "negative pool", key: "DB_MAX_OPEN_CONNS", value: "-1", expect: "DB_MAX_OPEN_CONNS"},
		{name: "unknown time zone", key: "DB_LOC", value: "Mars/Olympus", expect: "DB_LOC"},
		{name: "invalid dsn", key: "DB_DSN", value: "not-a-dsn", expect: "DB_DSN"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t, "DB_")
			t.Setenv(tc.key, tc.value)

			_, err := env.Load("DB_", devDefaults())
			if !errors.Is(err, env.ErrInvalidConfig) {
				t.Fatalf("err = %v, want ErrInvalidConfig", err)
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Fatalf("err = %q, want mention of %s", err, tc.expect)
			}
		})
	}
}

func TestLoadRequiresHost(t *testing.T) {
	clearEnv(t, "DB_")

	_, err := env.Load("DB_", env.MySQLConfig{})
	if !errors.Is(err, env.ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
}

func TestBuildDSNRoundTripsSpecialCharacters(t *testing.T) {
	cfg := env.MySQLConfig{
		Host:     "db.internal",
		Port:     3307,
		User:     "app",
		Password: `p@ss:w/rd?x#y`,
		Database: "demo",
	}
	dsn, err := cfg.BuildDSN()
	if err != nil {
		t.Fatalf("BuildDSN: %v", err)
	}
	parsed, err := mysqldriver.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("ParseDSN(%q): %v", dsn, err)
	}
	if parsed.User != cfg.User || parsed.Passwd != cfg.Password {
		t.Fatalf("credentials lost: %q / %q", parsed.User, parsed.Passwd)
	}
	if parsed.Addr != "db.internal:3307" {
		t.Fatalf("addr = %q", parsed.Addr)
	}
	if parsed.DBName != "demo" || !parsed.ParseTime {
		t.Fatalf("db/parseTime = %q/%v", parsed.DBName, parsed.ParseTime)
	}
	if parsed.Loc != time.Local {
		t.Fatalf("loc = %v", parsed.Loc)
	}
	if parsed.Params["charset"] != "utf8mb4" {
		t.Fatalf("charset = %q", parsed.Params["charset"])
	}
}

func TestAddress(t *testing.T) {
	if got := (env.MySQLConfig{Host: "192.168.100.135"}).Address(); got != "192.168.100.135:3306" {
		t.Fatalf("Address = %q", got)
	}
	if got := (env.MySQLConfig{Host: "::1", Port: 3307}).Address(); got != "[::1]:3307" {
		t.Fatalf("Address = %q", got)
	}
	if got := (env.MySQLConfig{}).Address(); got != "" {
		t.Fatalf("Address = %q", got)
	}
}

func TestOpenStopsOnCanceledContextWithoutLeakingPassword(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cfg := env.MySQLConfig{Host: "127.0.0.1", Password: "s3cret-value"}
	_, err := env.Open(ctx, cfg)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if strings.Contains(err.Error(), cfg.Password) {
		t.Fatalf("error leaks password: %q", err)
	}
}

func TestOpenRetriesWithinBudgetAndKeepsCallerConfig(t *testing.T) {
	cfg := env.MySQLConfig{
		Host:           "127.0.0.1",
		Port:           1,
		Password:       "s3cret-value",
		ConnectTimeout: 200 * time.Millisecond,
	}
	callerConfig := &gorm.Config{}

	start := time.Now()
	_, err := env.Open(context.Background(), cfg,
		env.WithGORMConfig(callerConfig),
		env.WithConnectRetry(300*time.Millisecond, 50*time.Millisecond),
	)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("retry took %v, want a bounded wait", elapsed)
	}
	if strings.Contains(err.Error(), cfg.Password) {
		t.Fatalf("error leaks password: %q", err)
	}
	if callerConfig.DisableAutomaticPing {
		t.Fatal("Open mutated the caller's gorm.Config")
	}
}

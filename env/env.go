// Package env loads MySQL connection settings from environment variables.
// Defaults belong to the application: the caller passes them to Load, so this
// package never ships credentials or environment-specific addresses.
package env

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
)

// ErrInvalidConfig reports a malformed or incomplete MySQL configuration.
var ErrInvalidConfig = errors.New("gormx/env: invalid mysql config")

// Neutral fallbacks used when neither the environment nor the caller's
// defaults provide a value. They describe the protocol and pool, not a host.
const (
	// DefaultPort is the MySQL port applied when neither configuration nor the
	// environment supplies one. Valid ports are 1-65535.
	DefaultPort = 3306
	// DefaultCharset is the connection charset applied when none is supplied.
	// It is passed to the driver as the "charset" parameter.
	DefaultCharset = "utf8mb4"
	// DefaultMaxOpenConns is the default database/sql maximum open connection
	// count. A value of 0 here means "use this default", not "unlimited".
	DefaultMaxOpenConns = 20
	// DefaultMaxIdleConns is the default database/sql idle connection count.
	DefaultMaxIdleConns = 5
	// DefaultConnMaxLifetime is how long a pooled connection may be reused
	// before being closed. Keep it below any server or proxy idle timeout.
	DefaultConnMaxLifetime = 5 * time.Minute
	// DefaultConnectTimeout bounds each initial ping attempt when no timeout is
	// configured, so startup cannot hang indefinitely.
	DefaultConnectTimeout = 10 * time.Second
)

// MySQLConfig describes one MySQL connection.
//
// DSN, when non-empty, overrides every other field and is passed to the driver
// unchanged; it must carry parseTime=True on its own, or time.Time scanning
// will fail. Remaining zero values fall back to the neutral constants above,
// while Host, User, Password and Database have no built-in default and must be
// supplied by defaults or the environment.
//
// Load validates the configuration and reports ErrInvalidConfig for missing or
// malformed values. BuildDSN and Open validate again, so a hand-built config is
// never used silently.
type MySQLConfig struct {
	// DSN is a complete driver DSN. When set, it takes precedence over all
	// other connection fields, including Host and Port, which are then ignored.
	DSN string
	// Host is the MySQL host name or IP address. It is required unless DSN is
	// set; Address never exposes credentials.
	Host string
	// Port is the TCP port. Zero means DefaultPort.
	Port int
	// User is the login user. It has no default.
	User string
	// Password is the login password, taken verbatim by Load so spaces are
	// preserved. It has no default.
	Password string
	// Database is the schema name (env variable suffix NAME). It has no default.
	Database string
	// Charset is the connection charset passed to the driver as a parameter.
	// Zero means DefaultCharset.
	Charset string
	// Loc is the location used to interpret DATETIME values. Zero means
	// time.Local.
	Loc *time.Location

	// MaxOpenConns is the maximum number of open connections. Zero means
	// DefaultMaxOpenConns; negative values are rejected.
	MaxOpenConns int
	// MaxIdleConns is the maximum number of idle connections. Zero means
	// DefaultMaxIdleConns; negative values are rejected.
	MaxIdleConns int
	// ConnMaxLifetime limits how long a connection may be reused. Zero means
	// DefaultConnMaxLifetime; negative values are rejected.
	ConnMaxLifetime time.Duration
	// ConnectTimeout is written to the driver DSN as the dial timeout and also
	// bounds each ping attempt performed by Open. Zero means
	// DefaultConnectTimeout; negative values are rejected.
	ConnectTimeout time.Duration
}

// Load reads prefix-prefixed variables such as DB_HOST and DB_PORT, falling
// back to defaults for every unset value. prefix may be given as "DB" or
// "DB_"; a variable that is empty, or blank apart from spaces, counts as
// unset. A password is taken verbatim, because spaces may be part of it.
// The full DSN variable (for example DB_DSN) takes precedence.
func Load(prefix string, defaults MySQLConfig) (MySQLConfig, error) {
	cfg := defaults

	if v := lookup(prefix, "DSN", true); v != "" {
		if _, err := mysqldriver.ParseDSN(v); err != nil {
			return MySQLConfig{}, fmt.Errorf("%w: %sDSN is not a valid DSN: %v", ErrInvalidConfig, normalizePrefix(prefix), err)
		}
		cfg.DSN = v
	}
	cfg.Host = stringFromEnv(prefix, "HOST", cfg.Host)
	cfg.User = stringFromEnv(prefix, "USER", cfg.User)
	cfg.Password = stringFromEnv(prefix, "PASSWORD", cfg.Password)
	cfg.Database = stringFromEnv(prefix, "NAME", cfg.Database)
	cfg.Charset = stringFromEnv(prefix, "CHARSET", cfg.Charset)

	var err error
	if cfg.Port, err = intFromEnv(prefix, "PORT", cfg.Port, 1, 65535); err != nil {
		return MySQLConfig{}, err
	}
	if cfg.MaxOpenConns, err = intFromEnv(prefix, "MAX_OPEN_CONNS", cfg.MaxOpenConns, 0, 0); err != nil {
		return MySQLConfig{}, err
	}
	if cfg.MaxIdleConns, err = intFromEnv(prefix, "MAX_IDLE_CONNS", cfg.MaxIdleConns, 0, 0); err != nil {
		return MySQLConfig{}, err
	}
	if cfg.ConnMaxLifetime, err = durationFromEnv(prefix, "CONN_MAX_LIFETIME", cfg.ConnMaxLifetime); err != nil {
		return MySQLConfig{}, err
	}
	if cfg.ConnectTimeout, err = durationFromEnv(prefix, "CONNECT_TIMEOUT", cfg.ConnectTimeout); err != nil {
		return MySQLConfig{}, err
	}
	if cfg.Loc, err = locationFromEnv(prefix, "LOC", cfg.Loc); err != nil {
		return MySQLConfig{}, err
	}

	cfg = cfg.withNeutralDefaults()
	if err := cfg.validate(); err != nil {
		return MySQLConfig{}, err
	}
	return cfg, nil
}

// BuildDSN renders the driver DSN. Password and database name travel through
// the driver's own formatting, so special characters survive a round trip.
func (c MySQLConfig) BuildDSN() (string, error) {
	if c.DSN != "" {
		return c.DSN, nil
	}
	c = c.withNeutralDefaults()
	if err := c.validate(); err != nil {
		return "", err
	}

	cfg := mysqldriver.NewConfig()
	cfg.User = c.User
	cfg.Passwd = c.Password
	cfg.Net = "tcp"
	cfg.Addr = c.Address()
	cfg.DBName = c.Database
	cfg.ParseTime = true
	cfg.Loc = c.Loc
	cfg.Timeout = c.ConnectTimeout
	cfg.Params = map[string]string{"charset": c.Charset}
	return cfg.FormatDSN(), nil
}

// Address returns host:port for logs and errors. It never contains credentials.
func (c MySQLConfig) Address() string {
	if c.Host == "" {
		if c.DSN != "" {
			return "configured dsn"
		}
		return ""
	}
	port := c.Port
	if port == 0 {
		port = DefaultPort
	}
	return net.JoinHostPort(c.Host, strconv.Itoa(port))
}

func (c MySQLConfig) withNeutralDefaults() MySQLConfig {
	if c.Port == 0 {
		c.Port = DefaultPort
	}
	if c.Charset == "" {
		c.Charset = DefaultCharset
	}
	if c.Loc == nil {
		c.Loc = time.Local
	}
	if c.MaxOpenConns == 0 {
		c.MaxOpenConns = DefaultMaxOpenConns
	}
	if c.MaxIdleConns == 0 {
		c.MaxIdleConns = DefaultMaxIdleConns
	}
	if c.ConnMaxLifetime == 0 {
		c.ConnMaxLifetime = DefaultConnMaxLifetime
	}
	if c.ConnectTimeout == 0 {
		c.ConnectTimeout = DefaultConnectTimeout
	}
	return c
}

func (c MySQLConfig) validate() error {
	if c.DSN != "" {
		return nil
	}
	if strings.TrimSpace(c.Host) == "" {
		return fmt.Errorf("%w: host is required, set it in defaults or the HOST variable", ErrInvalidConfig)
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("%w: port %d is outside 1-65535", ErrInvalidConfig, c.Port)
	}
	if c.MaxOpenConns < 0 || c.MaxIdleConns < 0 {
		return fmt.Errorf("%w: pool sizes must not be negative", ErrInvalidConfig)
	}
	if c.ConnMaxLifetime < 0 || c.ConnectTimeout < 0 {
		return fmt.Errorf("%w: durations must not be negative", ErrInvalidConfig)
	}
	return nil
}

func normalizePrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return ""
	}
	return strings.TrimSuffix(prefix, "_") + "_"
}

func lookup(prefix, suffix string, trim bool) string {
	v, ok := os.LookupEnv(normalizePrefix(prefix) + suffix)
	if !ok {
		return ""
	}
	if trim {
		v = strings.TrimSpace(v)
	}
	return v
}

func stringFromEnv(prefix, suffix, current string) string {
	if v := lookup(prefix, suffix, suffix != "PASSWORD"); v != "" {
		return v
	}
	return current
}

func intFromEnv(prefix, suffix string, current, min, max int) (int, error) {
	raw := lookup(prefix, suffix, true)
	if raw == "" {
		return current, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %s%s=%q is not an integer", ErrInvalidConfig, normalizePrefix(prefix), suffix, raw)
	}
	if n < min || (max > 0 && n > max) {
		return 0, fmt.Errorf("%w: %s%s=%q is out of range", ErrInvalidConfig, normalizePrefix(prefix), suffix, raw)
	}
	return n, nil
}

func durationFromEnv(prefix, suffix string, current time.Duration) (time.Duration, error) {
	raw := lookup(prefix, suffix, true)
	if raw == "" {
		return current, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %s%s=%q is not a duration such as 5m", ErrInvalidConfig, normalizePrefix(prefix), suffix, raw)
	}
	if d < 0 {
		return 0, fmt.Errorf("%w: %s%s=%q must not be negative", ErrInvalidConfig, normalizePrefix(prefix), suffix, raw)
	}
	return d, nil
}

func locationFromEnv(prefix, suffix string, current *time.Location) (*time.Location, error) {
	raw := lookup(prefix, suffix, true)
	if raw == "" {
		return current, nil
	}
	switch {
	case strings.EqualFold(raw, "Local"):
		return time.Local, nil
	case strings.EqualFold(raw, "UTC"):
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(raw)
	if err != nil {
		// A scratch container without tzdata cannot resolve IANA names; point
		// at the supported escape hatch instead of failing mute.
		return nil, fmt.Errorf("%w: %s%s=%q is not a time zone (use Local or UTC, or import time/tzdata)", ErrInvalidConfig, normalizePrefix(prefix), suffix, raw)
	}
	return loc, nil
}

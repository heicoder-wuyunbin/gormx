package env

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	drivermysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

type openOptions struct {
	gormConfig    *gorm.Config
	retryTotal    time.Duration
	retryInterval time.Duration
}

// Option customises Open.
type Option func(*openOptions)

// WithGORMConfig supplies the GORM config. Open copies it and forces
// DisableAutomaticPing so that ping retries and timeouts stay in one place.
func WithGORMConfig(cfg *gorm.Config) Option {
	return func(o *openOptions) {
		o.gormConfig = cfg
	}
}

// WithConnectRetry retries the initial ping until total elapses, waiting
// interval between attempts. total <= 0 disables retry, which is the default.
func WithConnectRetry(total, interval time.Duration) Option {
	return func(o *openOptions) {
		o.retryTotal = total
		o.retryInterval = interval
	}
}

// Open builds the DSN, opens GORM, configures the pool and pings the server.
// A failed ping closes the pool and returns an error without credentials.
func Open(ctx context.Context, cfg MySQLConfig, opts ...Option) (*gorm.DB, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("gormx/env: mysql %s: %w", cfg.Address(), err)
	}

	options := openOptions{retryInterval: time.Second}
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}

	dsn, err := cfg.BuildDSN()
	if err != nil {
		return nil, err
	}
	cfg = cfg.withNeutralDefaults()

	gormConfig := &gorm.Config{}
	if options.gormConfig != nil {
		copied := *options.gormConfig
		gormConfig = &copied
	}
	gormConfig.DisableAutomaticPing = true

	db, err := gorm.Open(drivermysql.Open(dsn), gormConfig)
	if err != nil {
		return nil, fmt.Errorf("gormx/env: open mysql %s: %w", cfg.Address(), err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("gormx/env: mysql %s: %w", cfg.Address(), err)
	}
	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)

	if err := ping(ctx, sqlDB, cfg.ConnectTimeout, options); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("gormx/env: ping mysql %s: %w", cfg.Address(), err)
	}
	return db, nil
}

func ping(ctx context.Context, db *sql.DB, timeout time.Duration, options openOptions) error {
	deadline := time.Now().Add(options.retryTotal)
	for {
		err := pingOnce(ctx, db, timeout)
		if err == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if options.retryTotal <= 0 || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(options.retryInterval):
		}
	}
}

func pingOnce(ctx context.Context, db *sql.DB, timeout time.Duration) error {
	if timeout <= 0 {
		return db.PingContext(ctx)
	}
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return db.PingContext(attemptCtx)
}

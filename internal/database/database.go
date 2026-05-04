package database

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/cockroachdb/errors"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"unknwon.dev/git-lfs-server/internal/logx"
)

// Config holds PostgreSQL connection and pool settings. The ConnMaxLifetime and
// ConnMaxIdleTime fields are in minutes to align with INI config parsing.
type Config struct {
	Host            string `ini:"HOST"`
	Port            int    `ini:"PORT"`
	Name            string `ini:"NAME"`
	User            string `ini:"USER"`
	Password        string `ini:"PASSWORD"`
	SSLMode         string `ini:"SSL_MODE"`
	MaxOpenConns    int    `ini:"MAX_OPEN_CONNS"`
	MaxIdleConns    int    `ini:"MAX_IDLE_CONNS"`
	ConnMaxLifetime int    `ini:"CONN_MAX_LIFETIME"`
	ConnMaxIdleTime int    `ini:"CONN_MAX_IDLE_TIME"`
}

// tables is the list of all models used for auto-migration and test cleanup.
// Child tables must come before parent tables so that forward-order deletes
// respect foreign key constraints.
//
// ⚠️ WARNING: This list is meant to be read-only.
var tables = []any{
	&Object{},
	&RepoObject{},
}

// DB wraps a GORM database connection with lifecycle management.
type DB struct {
	db *gorm.DB
}

// Ping verifies the database connection is alive.
func (d *DB) Ping(ctx context.Context) error {
	sqlDB, err := d.db.DB()
	if err != nil {
		return errors.Wrap(err, "get underlying *sql.DB")
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		return errors.Wrap(err, "ping database")
	}
	return nil
}

// New opens a PostgreSQL connection via GORM and configures the connection pool.
func New(ctx context.Context, logger *logx.Logger, cfg Config) (*DB, error) {
	dsn := fmt.Sprintf(
		"postgres://%s:%s@%s:%d/%s?sslmode=%s&application_name=lfsd",
		url.QueryEscape(cfg.User),
		url.QueryEscape(cfg.Password),
		cfg.Host,
		cfg.Port,
		url.QueryEscape(cfg.Name),
		cfg.SSLMode,
	)

	// NOTE: Create GORM logger before adding generic connection details as it doesn't need it.
	gormLogger := newGORMLogger(logger.Scoped("gorm"), 200*time.Millisecond)

	logger = logger.With("host", cfg.Host, "port", cfg.Port, "dbName", cfg.Name, "sslMode", cfg.SSLMode)
	logger.DebugContext(ctx, "Connecting to database")
	gormDB, err := gorm.Open(
		postgres.Open(dsn),
		&gorm.Config{
			Logger:                                   gormLogger,
			SkipDefaultTransaction:                   true,
			DisableForeignKeyConstraintWhenMigrating: true,
		},
	)
	if err != nil {
		return nil, errors.Wrap(err, "open database connection")
	}

	sqlDB, err := gormDB.DB()
	if err != nil {
		return nil, errors.Wrap(err, "get underlying *sql.DB")
	}

	// Configure connection pool. Only set values that are positive to avoid
	// unexpected behaviour from zero or negative inputs.
	if cfg.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns > 0 {
		maxIdle := cfg.MaxIdleConns
		if cfg.MaxOpenConns > 0 && maxIdle > cfg.MaxOpenConns {
			maxIdle = cfg.MaxOpenConns
		}
		sqlDB.SetMaxIdleConns(maxIdle)
	}
	if cfg.ConnMaxLifetime > 0 {
		sqlDB.SetConnMaxLifetime(time.Duration(cfg.ConnMaxLifetime) * time.Minute)
	}
	if cfg.ConnMaxIdleTime > 0 {
		sqlDB.SetConnMaxIdleTime(time.Duration(cfg.ConnMaxIdleTime) * time.Minute)
	}

	logger.DebugContext(ctx, "Auto-migrating database tables")
	if err = gormDB.AutoMigrate(tables...); err != nil {
		return nil, errors.Wrap(err, "auto-migrate database tables")
	}

	// AutoMigrate doesn't emit CHECK constraints. Add ours idempotently:
	// Postgres has no "ADD CONSTRAINT IF NOT EXISTS" form for CHECK, so we
	// catch duplicate_object via SQLSTATE.
	if err = gormDB.Exec(`
DO $$ BEGIN
    ALTER TABLE objects ADD CONSTRAINT check_verified_consistency CHECK (
        (verified_at IS NULL     AND size IS NULL     AND object_uri IS NULL)
        OR
        (verified_at IS NOT NULL AND size IS NOT NULL AND object_uri IS NOT NULL)
    );
EXCEPTION WHEN duplicate_object THEN NULL;
END $$`).Error; err != nil {
		return nil, errors.Wrap(err, "add check_verified_consistency constraint")
	}

	logger.InfoContext(ctx, "Database ready")
	return &DB{db: gormDB}, nil
}

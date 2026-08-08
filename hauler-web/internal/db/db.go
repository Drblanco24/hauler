// Package db is hauler-web's Postgres access layer.
//
// Queries are hand-written against pgx rather than generated. The query set is
// small and the interesting ones -- the delivery lookup that drives dedupe,
// the queue claim -- are worth reading as SQL in the same file as the Go that
// depends on their exact semantics.
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/Drblanco24/hauler-web/migrations"
)

// DB owns the connection pool.
type DB struct {
	pool *pgxpool.Pool
	dsn  string
}

// Open connects and verifies the connection. It does not run migrations;
// callers do that explicitly so a worker never silently reshapes the schema
// out from under a running web replica.
func Open(ctx context.Context, dsn string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing database url: %w", err)
	}
	// A reconcile holds a connection while hauler runs, which can be hours,
	// so the pool must not reap idle-looking connections aggressively.
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.MaxConnLifetime = time.Hour

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}
	return &DB{pool: pool, dsn: dsn}, nil
}

// Close releases the pool.
func (d *DB) Close() {
	if d.pool != nil {
		d.pool.Close()
	}
}

// Ping is the readiness probe's backing check.
func (d *DB) Ping(ctx context.Context) error { return d.pool.Ping(ctx) }

// Pool exposes the underlying pool for packages that need it (the job queue's
// SKIP LOCKED claim, LISTEN/NOTIFY).
func (d *DB) Pool() *pgxpool.Pool { return d.pool }

// Migrate applies all pending migrations.
//
// goose needs a database/sql handle, so this opens a second, short-lived
// connection through pgx's stdlib shim rather than borrowing from the pool --
// migrations run once at startup and should not hold a pool slot.
func (d *DB) Migrate(ctx context.Context) error {
	sqlDB, err := goose.OpenDBWithDriver("pgx", d.dsn)
	if err != nil {
		return fmt.Errorf("opening migration connection: %w", err)
	}
	defer sqlDB.Close()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	if err := goose.UpContext(ctx, sqlDB, migrations.Dir); err != nil {
		return fmt.Errorf("applying migrations: %w", err)
	}
	return nil
}

// MigrateDown rolls back one migration. Used by tests and by an operator
// recovering from a bad deploy.
func (d *DB) MigrateDown(ctx context.Context) error {
	sqlDB, err := goose.OpenDBWithDriver("pgx", d.dsn)
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.DownContext(ctx, sqlDB, migrations.Dir)
}

var _ = stdlib.GetDefaultDriver // keep the pgx stdlib driver registered for goose

// ErrNotFound is returned when a lookup matches nothing.
var ErrNotFound = errors.New("not found")

func noRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

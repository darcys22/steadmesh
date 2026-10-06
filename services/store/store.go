// Package store is the platform's durable operational store on PostgreSQL
// (design §13.1). Every query is scoped by organisation, and by memory store
// where records are involved. Uniqueness, deduplication and optimistic
// concurrency are enforced by database constraints; fenced writes check the
// caller's lease generation inside the same transaction (ADR-0005).
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrations embed.FS

var (
	ErrNotFound = errors.New("not found")
	// ErrFenced means the caller's lease generation is not the current one.
	ErrFenced   = errors.New("fenced")
	ErrConflict = errors.New("conflict")
	ErrInvalid  = errors.New("invalid")
	// ErrInboxFull means the recipient already has MaxPendingDeliveries (ADR-0006).
	ErrInboxFull = errors.New("recipient inbox full")
)

// Store wraps a pgx connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to the database at url and applies pending migrations.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.Migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Migrate applies the embedded goose migrations.
func (s *Store) Migrate(ctx context.Context) error {
	fsys, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	db := stdlib.OpenDBFromPool(s.pool)
	defer db.Close()
	p, err := goose.NewProvider(goose.DialectPostgres, db, fsys)
	if err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping checks that the database is reachable (readiness).
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// inTx runs fn in a transaction, committing when it returns nil.
func (s *Store) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, fn)
}

// Fence identifies a seat's write and the lease generation it claims.
type Fence struct {
	SeatID     string
	Generation int64
}

// checkFence locks the seat's lease row for share and rejects a stale
// generation. Lease acquisition updates the row, so it serialises with
// in-flight fenced writes.
func checkFence(ctx context.Context, tx pgx.Tx, f Fence) error {
	var gen int64
	err := tx.QueryRow(ctx, `SELECT generation FROM execution_leases WHERE seat_id = $1 FOR SHARE`, f.SeatID).Scan(&gen)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrFenced
	}
	if err != nil {
		return err
	}
	if gen != f.Generation {
		return ErrFenced
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

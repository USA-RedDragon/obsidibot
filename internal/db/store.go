// Package db is obsidibot's persistence layer: the connection pool, the
// sqlc-generated query layer, single-transaction plumbing, and the migration
// runner.
//
// The layering rule: this package stays mechanical. It exposes the pool, the
// generated queries and InTx, and nothing else. Domain workflows -- "settle a
// deposit", "apply a kill" -- belong to the packages that own those concepts,
// built on InTx.
//
// obsidibot OWNS this database. Nothing else reads or writes it, which is why
// it applies its own DDL on startup rather than waiting for another service to
// migrate on its behalf.
package db

import (
	"context"
	"fmt"
	"math"

	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store bundles the pgx pool with the generated query layer. It is the one
// handle other packages use to reach Postgres.
type Store struct {
	pool *pgxpool.Pool
	q    *gen.Queries
}

// Connect opens a pool for url and verifies it with a ping, so a bad URL fails
// at startup rather than at the first interaction. On ping failure the pool is
// closed before returning, so a failed Connect never leaks connections.
//
// maxConns sizes the pool EXPLICITLY rather than leaving it to pgx, whose
// default is max(4, runtime.NumCPU()). That default makes the same image behave
// differently on every node size -- a comfortable pool on a build machine and a
// four-connection pool on a 4-vCPU node -- and pool exhaustion does not present
// as an error, it presents as every caller blocking in Acquire until its
// deadline. It is a configured number here so that capacity is a deployment
// decision rather than a property of the hardware that happened to schedule us.
// It overrides any pool_max_conns carried in the URL: one place decides.
func Connect(ctx context.Context, url string, maxConns int) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if maxConns < 1 || maxConns > math.MaxInt32 {
		return nil, fmt.Errorf("database.maxConns %d must be between 1 and %d", maxConns, math.MaxInt32)
	}
	cfg.MaxConns = int32(maxConns) //nolint:gosec // range-checked immediately above

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return NewStore(pool), nil
}

// NewStore wraps an existing pool, for tests and callers that own the pool's
// lifecycle themselves.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, q: gen.New(pool)}
}

// Ping reports whether the database is reachable. It backs /readyz.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}

// Close closes the pool and blocks until its connections are returned.
func (s *Store) Close() {
	s.pool.Close()
}

// Pool exposes the underlying pool for callers that need connection-level
// control: the migration runner's advisory lock, and internal/leader's.
func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

// ConnConfig returns a copy of the settings the pool dials with, for the one
// caller that needs a connection OUTSIDE the pool: internal/leader, whose
// advisory lock must sit on a single session for as long as leadership lasts
// and must therefore never take that session out of the pool serving requests.
//
// A parsed config rather than the URL, because the URL may carry pgxpool's own
// pool_* settings, which pgx.ParseConfig would pass to the server as unknown
// parameters and the server would reject. A copy, so adjusting it cannot reach
// into the pool's live configuration.
func (s *Store) ConnConfig() *pgx.ConnConfig {
	return s.pool.Config().ConnConfig.Copy()
}

// MaxConns reports the pool's connection ceiling, which is what callers have
// to size their concurrency against: exceeding it does not fail, it queues.
func (s *Store) MaxConns() int {
	return int(s.pool.Config().MaxConns)
}

// Queries returns the generated query layer for one-shot reads and writes
// outside a transaction.
func (s *Store) Queries() *gen.Queries {
	return s.q
}

// InTx runs fn inside a single transaction: Begin, fn over the tx-bound
// queries, Commit -- with rollback deferred so an error or panic unwinds
// cleanly.
//
// Every write that must not half-happen goes through here. The banking path in
// particular depends on it: moving a balance and closing its ledger row are one
// fact, and a crash between them would be indistinguishable from theft.
func (s *Store) InTx(ctx context.Context, fn func(*gen.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	// Rollback after a successful commit is a harmless no-op error. The
	// cancel-free context lets an aborted request still roll back cleanly.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(s.q.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

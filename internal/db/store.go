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

	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
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
func Connect(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
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

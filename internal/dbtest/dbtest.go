// Package dbtest gives the integration tests a database to work against.
//
// It lives outside _test.go files so every package that needs one uses the same
// setup rather than a copy that drifts. Tests SKIP rather than fail when no
// database is configured: `go test ./...` on a laptop with nothing running
// should still be useful, and CI supplies TEST_DATABASE_URL so the coverage is
// not quietly optional there.
//
// # One database, one schema per package
//
// `go test ./...` runs packages CONCURRENTLY. An earlier version of this file
// gave every package the `public` schema and reset it between tests, which
// meant two packages dropped the schema out from under each other -- passing
// alone and failing together, which is the worst way to find out. Each test
// binary now gets a Postgres schema named after itself and never touches
// anyone else's.
package dbtest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// URLEnv names the environment variable holding the test database URL.
const URLEnv = "TEST_DATABASE_URL"

//nolint:gochecknoglobals // compiled once
var unsafeSchemaChars = regexp.MustCompile(`[^a-z0-9_]`)

// SchemaName derives this test binary's private schema from its own name, so
// two packages running at once cannot collide.
func SchemaName() string {
	base := filepath.Base(os.Args[0])
	base = strings.TrimSuffix(base, ".test")
	base = unsafeSchemaChars.ReplaceAllString(strings.ToLower(base), "_")
	if base == "" {
		base = "unknown"
	}
	return "test_" + base
}

// Pool returns a pool scoped to this package's own schema, skipping the test if
// no database is configured. The pool is closed when the test ends.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv(URLEnv)
	if url == "" {
		t.Skipf("%s is not set; skipping database integration test", URLEnv)
	}

	schema := SchemaName()

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse %s: %v", URLEnv, err)
	}
	// Every connection in the pool lands in this package's schema, so the code
	// under test can use unqualified names exactly as it does in production.
	cfg.ConnConfig.RuntimeParams["search_path"] = schema

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, cfg.ConnString())
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	pool.Config().ConnConfig.RuntimeParams["search_path"] = schema

	// Reopen from the mutated config: ConnString() does not carry RuntimeParams.
	pool.Close()
	pool, err = pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	if _, err := pool.Exec(ctx, "create schema if not exists "+schema); err != nil {
		pool.Close()
		t.Fatalf("create schema %s: %v", schema, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Reset empties this package's schema so each test starts from nothing.
func Reset(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	schema := SchemaName()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		fmt.Sprintf("drop schema if exists %s cascade; create schema %s", schema, schema)); err != nil {
		t.Fatalf("reset schema %s: %v", schema, err)
	}
}

package db_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/dbtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationsFS returns the real migration directory, found relative to this
// source file. Tests run against the SHIPPED schema rather than a fixture, so a
// migration that only works in production is a test failure here.
func migrationsFS(t *testing.T) fs.FS {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "schema", "migrations")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("migrations directory %s: %v", dir, err)
	}
	return os.DirFS(dir)
}

// migrated returns a store on a freshly migrated database.
func migrated(t *testing.T) (*pgxpool.Pool, *db.Store) {
	t.Helper()
	pool := dbtest.Pool(t)
	dbtest.Reset(t, pool)
	if err := db.Migrate(context.Background(), pool, migrationsFS(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool, db.NewStore(pool)
}

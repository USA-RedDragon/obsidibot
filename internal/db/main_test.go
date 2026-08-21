package db_test

import (
	"context"
	"testing"

	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/dbtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationsFS returns the real migration directory, found relative to this
// source file. Tests run against the SHIPPED schema rather than a fixture, so a
// migration that only works in production is a test failure here.
// migrated returns a store on a freshly migrated database.
func migrated(t *testing.T) (*pgxpool.Pool, *db.Store) {
	t.Helper()
	pool := dbtest.Pool(t)
	dbtest.Reset(t, pool)
	if err := db.Migrate(context.Background(), pool, dbtest.MigrationsFS(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool, db.NewStore(pool)
}

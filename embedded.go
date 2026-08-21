package main

import (
	"embed"
	"io/fs"
	"log/slog"
	"os"
)

// schemaFS carries the migrations into the binary so a deployment is one
// artifact: there is no image variant, init container, or mounted volume that
// can drift from the code that expects a particular schema.
//
//go:embed schema/migrations/*.sql
var schemaFS embed.FS

// migrations returns the migration files rooted at the directory that directly
// contains them, which is what db.Migrate expects.
//
// A failure here means the embed pattern stopped matching -- a build problem,
// not a runtime one -- so it is fatal rather than deferred to the first
// startup against a real database.
func migrations() fs.FS {
	sub, err := fs.Sub(schemaFS, "schema/migrations")
	if err != nil {
		slog.Error("embedded migrations are unreadable; this is a broken build", "error", err)
		os.Exit(1)
	}
	return sub
}

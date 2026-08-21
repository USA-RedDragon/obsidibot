package db

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationLockKey is the pg_advisory_lock key that serializes Migrate across
// replicas. Advisory locks are session-scoped and need no table, so multiple
// replicas starting at once serialize with no bootstrap race. The value is
// the ASCII bytes of "obsidibo" read as a big-endian int64, so the constant
// is recoverable from the string if another component ever needs to
// coordinate with it.
//
// It shares a keyspace with the background-job locks in internal/leader, which
// is why those are derived from their own eight-character names rather than
// from small ordinals: a job that picked 1 would eventually collide with
// something.
const migrationLockKey int64 = 0x6f62_7369_6469_626f // "obsidibo"

// Transaction-control keywords recognized at the start of a top-level
// statement in a migration file.
const (
	txBegin    = "begin"
	txCommit   = "commit"
	txRollback = "rollback"
)

// Migrate applies every not-yet-applied *.sql file in migrations, in filename
// order, recording each application in the schema_migrations ledger. It is
// safe to run from every replica on every startup: a session-scoped advisory
// lock serializes concurrent runs, and already-applied files are skipped
// silently.
//
// migrations must be rooted at the directory that directly contains the
// *.sql files. The schema embeds under "schema/migrations/...", so the
// caller passes the fs.Sub of that subtree:
//
//	//go:embed schema/migrations/*.sql
//	var schemaFS embed.FS
//
//	sub, err := fs.Sub(schemaFS, "schema/migrations")
//	...
//	err = db.Migrate(ctx, pool, sub)
//
// Each file runs together with its ledger insert in ONE transaction on ONE
// dedicated connection, so a file is either fully applied and recorded, or
// neither. Transaction control inside the file would break that guarantee,
// which is why applyMigration strips the outer begin/commit pair and refuses
// files with any other transaction control.
func Migrate(ctx context.Context, pool *pgxpool.Pool, migrations fs.FS) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "select pg_advisory_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		// Cancel-free context: an aborted migration must still unlock the
		// session before the connection returns to the pool. If the unlock
		// fails anyway, the session is almost certainly dead -- advisory
		// locks die with their session -- but close the connection to make
		// sure a healthy-but-still-locked one can never be pooled.
		unlockCtx := context.WithoutCancel(ctx)
		if _, uerr := conn.Exec(unlockCtx, "select pg_advisory_unlock($1)", migrationLockKey); uerr != nil {
			_ = conn.Conn().Close(unlockCtx)
			slog.WarnContext(unlockCtx, "failed to release migration lock; discarding connection", "error", uerr)
		}
	}()

	// Created only after the lock is held, so first-boot replicas cannot race
	// the CREATE either.
	if _, err := conn.Exec(ctx, `create table if not exists schema_migrations (
		filename   text        primary key,
		applied_at timestamptz not null default now()
	)`); err != nil {
		return fmt.Errorf("create schema_migrations ledger: %w", err)
	}

	names, err := migrationFilenames(migrations)
	if err != nil {
		return err
	}
	applied, err := appliedMigrations(ctx, conn)
	if err != nil {
		return err
	}
	for _, name := range names {
		if applied[name] {
			continue
		}
		if err := applyMigration(ctx, conn, migrations, name); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration runs one migration file and its ledger insert in a single
// transaction on the dedicated (lock-holding) connection.
func applyMigration(ctx context.Context, conn *pgxpool.Conn, migrations fs.FS, name string) error {
	raw, err := fs.ReadFile(migrations, name)
	if err != nil {
		return fmt.Errorf("read migration %s: %w", name, err)
	}
	stripped, err := stripTxControl(string(raw))
	if err != nil {
		return fmt.Errorf("migration %s: %w", name, err)
	}

	// Migration files may create pg_temp helpers (0001 does). The temp
	// namespace is session-scoped and this connection comes from a pool, so an
	// earlier file -- or an earlier Migrate on the same session -- may have
	// left those helpers behind, and re-creating them would fail. Give every
	// file a fresh temp namespace; outside the transaction so a rollback
	// cannot resurrect anything.
	if _, err := conn.Exec(ctx, "discard temp"); err != nil {
		return fmt.Errorf("discard temp before migration %s: %w", name, err)
	}

	start := time.Now()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction for migration %s: %w", name, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// A zero-argument Exec always uses the simple query protocol (pgx v5
	// conn.go: "Always use simple protocol when there are no arguments"),
	// and the simple protocol accepts multiple semicolon-separated statements
	// in one call -- all of which run inside the transaction begun above.
	if _, err := tx.Exec(ctx, stripped); err != nil {
		return fmt.Errorf("apply migration %s: %w", name, err)
	}
	if _, err := tx.Exec(ctx, "insert into schema_migrations (filename) values ($1)", name); err != nil {
		return fmt.Errorf("record migration %s: %w", name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration %s: %w", name, err)
	}
	slog.InfoContext(ctx, "applied migration", "filename", name, "duration", time.Since(start))
	return nil
}

// appliedMigrations reads the ledger into a set.
func appliedMigrations(ctx context.Context, conn *pgxpool.Conn) (map[string]bool, error) {
	rows, err := conn.Query(ctx, "select filename from schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("list applied migrations: %w", err)
	}
	defer rows.Close()
	applied := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan applied migration: %w", err)
		}
		applied[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	return applied, nil
}

// migrationFilenames lists the *.sql files at the root of migrations, sorted
// by filename -- which is the application order.
func migrationFilenames(migrations fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(migrations, ".")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names, nil
}

// txStmt is one top-level transaction-control statement in a migration file.
type txStmt struct {
	kind    string // txBegin, txCommit or txRollback
	start   int    // byte offset of the keyword
	end     int    // byte offset just past the terminating semicolon (or EOF)
	ordinal int    // 0-based position among all top-level statements
	plain   bool   // bare form, optionally WORK/TRANSACTION: strippable
}

// stripTxControl removes the outer transaction-control pair from a migration
// file: one leading `begin;` (the file's first statement) and one trailing
// `commit;` (its last), case-insensitively, tolerating whitespace and
// comments around both. Migration files carry their own begin/commit so they
// stay usable under plain psql, but the runner wraps each file in its own
// transaction together with the ledger insert; left in place, the file's
// inner COMMIT would end the runner's transaction early and decouple the file
// from its ledger record -- a crash between the two would re-apply the file
// forever.
//
// Any transaction control left after removing that outer pair -- an interior
// begin/commit, a rollback anywhere, or a non-bare form such as `begin
// isolation level ...` -- is an error: such a file cannot be applied
// atomically. Input without transaction control is returned unchanged, so
// the function is idempotent over its own output.
func stripTxControl(sql string) (string, error) {
	stmts, total := scanTxStmts(sql)
	if len(stmts) == 0 {
		return sql, nil
	}
	strip := make([]bool, len(stmts))
	for idx, s := range stmts {
		leading := s.kind == txBegin && s.ordinal == 0
		trailing := s.kind == txCommit && s.ordinal == total-1
		if s.plain && (leading || trailing) {
			strip[idx] = true
		}
	}
	for idx, s := range stmts {
		if strip[idx] {
			continue
		}
		line := 1 + strings.Count(sql[:s.start], "\n")
		return "", fmt.Errorf(
			"unstrippable transaction control: %q statement at line %d (statement %d of %d); only a leading begin and a trailing commit are stripped, and the runner supplies the transaction",
			s.kind, line, s.ordinal+1, total)
	}
	var b strings.Builder
	b.Grow(len(sql))
	prev := 0
	for idx, s := range stmts {
		if !strip[idx] {
			continue
		}
		b.WriteString(sql[prev:s.start])
		prev = s.end
	}
	b.WriteString(sql[prev:])
	return b.String(), nil
}

// scanTxStmts finds every top-level transaction-control statement in sql,
// skipping comments, quoted strings and dollar-quoted bodies (a plpgsql BEGIN
// inside $$...$$ is not transaction control), and counts top-level statements
// so the caller can tell leading and trailing from interior.
func scanTxStmts(sql string) (stmts []txStmt, total int) {
	inStmt := false
	i := 0
	for i < len(sql) {
		i = skipIgnorable(sql, i)
		if i >= len(sql) {
			break
		}
		switch c := sql[i]; {
		case c == ';':
			inStmt = false
			i++
		case c == '\'' || c == '"':
			if !inStmt {
				inStmt = true
				total++
			}
			i = skipQuoted(sql, i)
		case c == '$':
			if !inStmt {
				inStmt = true
				total++
			}
			if end, ok := skipDollarQuoted(sql, i); ok {
				i = end
			} else {
				i++
			}
		case isWordByte(c):
			word, end := readWord(sql, i)
			if !inStmt {
				inStmt = true
				total++
				kind := strings.ToLower(word)
				if kind == txBegin || kind == txCommit || kind == txRollback {
					stmt := finishTxStmt(sql, i, end, kind, total-1)
					stmts = append(stmts, stmt)
					i = stmt.end
					inStmt = false
					continue
				}
			}
			i = end
		default:
			if !inStmt {
				inStmt = true
				total++
			}
			i++
		}
	}
	return stmts, total
}

// finishTxStmt consumes the rest of a transaction-control statement whose
// keyword occupies [start,wordEnd). Bare BEGIN/COMMIT/ROLLBACK, optionally
// followed by the WORK or TRANSACTION noise word, is "plain"; anything more
// (isolation modes, COMMIT PREPARED, ROLLBACK TO SAVEPOINT) is not, and is
// therefore never strippable.
func finishTxStmt(sql string, start, wordEnd int, kind string, ordinal int) txStmt {
	i := skipIgnorable(sql, wordEnd)
	if i < len(sql) && isWordByte(sql[i]) {
		word, end := readWord(sql, i)
		if w := strings.ToLower(word); w == "work" || w == "transaction" {
			i = skipIgnorable(sql, end)
		}
	}
	switch {
	case i >= len(sql):
		return txStmt{kind: kind, start: start, end: len(sql), ordinal: ordinal, plain: true}
	case sql[i] == ';':
		return txStmt{kind: kind, start: start, end: i + 1, ordinal: ordinal, plain: true}
	default:
		return txStmt{kind: kind, start: start, end: scanToStmtEnd(sql, i), ordinal: ordinal, plain: false}
	}
}

// scanToStmtEnd advances to just past the next top-level ';' (or to EOF),
// honouring comments and quoting.
func scanToStmtEnd(sql string, i int) int {
	for i < len(sql) {
		i = skipIgnorable(sql, i)
		if i >= len(sql) {
			break
		}
		switch sql[i] {
		case ';':
			return i + 1
		case '\'', '"':
			i = skipQuoted(sql, i)
		case '$':
			if end, ok := skipDollarQuoted(sql, i); ok {
				i = end
			} else {
				i++
			}
		default:
			i++
		}
	}
	return len(sql)
}

// skipIgnorable advances past whitespace and comments.
func skipIgnorable(sql string, i int) int {
	for i < len(sql) {
		switch {
		case sql[i] == ' ' || sql[i] == '\t' || sql[i] == '\r' || sql[i] == '\n':
			i++
		case strings.HasPrefix(sql[i:], "--"):
			i = skipLineComment(sql, i)
		case strings.HasPrefix(sql[i:], "/*"):
			i = skipBlockComment(sql, i)
		default:
			return i
		}
	}
	return i
}

// skipLineComment advances past a -- comment, i pointing at the first '-'.
func skipLineComment(sql string, i int) int {
	if j := strings.IndexByte(sql[i:], '\n'); j >= 0 {
		return i + j + 1
	}
	return len(sql)
}

// skipBlockComment advances past a /* ... */ comment, which Postgres nests.
func skipBlockComment(sql string, i int) int {
	depth := 0
	for i < len(sql) {
		switch {
		case strings.HasPrefix(sql[i:], "/*"):
			depth++
			i += 2
		case strings.HasPrefix(sql[i:], "*/"):
			depth--
			i += 2
			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}
	return len(sql)
}

// skipQuoted advances past a '...' string or "..." identifier, i pointing at
// the opening quote; doubling the quote character escapes it. E'...'
// backslash escapes are not understood, so migrations must not use them.
func skipQuoted(sql string, i int) int {
	quote := sql[i]
	i++
	for i < len(sql) {
		if sql[i] == quote {
			if i+1 < len(sql) && sql[i+1] == quote {
				i += 2
				continue
			}
			return i + 1
		}
		i++
	}
	return len(sql)
}

// skipDollarQuoted advances past a $tag$ ... $tag$ literal if i points at
// one, reporting whether it did. Positional parameters such as $1 are not
// dollar quotes (a tag cannot start with a digit).
func skipDollarQuoted(sql string, i int) (int, bool) {
	j := i + 1
	if j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
		return 0, false
	}
	for j < len(sql) && isWordByte(sql[j]) {
		j++
	}
	if j >= len(sql) || sql[j] != '$' {
		return 0, false
	}
	tag := sql[i : j+1]
	if k := strings.Index(sql[j+1:], tag); k >= 0 {
		return j + 1 + k + len(tag), true
	}
	return len(sql), true
}

// readWord reads the identifier-like word starting at i.
func readWord(sql string, i int) (string, int) {
	j := i
	for j < len(sql) && isWordByte(sql[j]) {
		j++
	}
	return sql[i:j], j
}

func isWordByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

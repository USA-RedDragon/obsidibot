package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/dbtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const testAGID = "555-000-101"

// TestMigrateIsIdempotent is the property every replica depends on: they all
// call Migrate on every start, so a second run over an already-migrated
// database must do nothing rather than fail on an existing table.
func TestMigrateIsIdempotent(t *testing.T) {
	pool, _ := migrated(t)
	if err := db.Migrate(context.Background(), pool, migrationsFS(t)); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var applied int
	if err := pool.QueryRow(context.Background(), "select count(*) from schema_migrations").Scan(&applied); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if applied != 1 {
		t.Fatalf("ledger has %d rows after two migrations, want 1", applied)
	}
}

// TestMigrateIsSafeConcurrently covers the rolling-deploy case: several
// replicas starting at once. The advisory lock has to serialise them, and none
// may report a failure.
func TestMigrateIsSafeConcurrently(t *testing.T) {
	pool := dbtest.Pool(t)
	dbtest.Reset(t, pool)
	migrations := migrationsFS(t)

	const replicas = 4
	errs := make(chan error, replicas)
	for range replicas {
		go func() { errs <- db.Migrate(context.Background(), pool, migrations) }()
	}
	for range replicas {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent migrate: %v", err)
		}
	}
}

func seedPlayer(t *testing.T, q *gen.Queries) {
	t.Helper()
	if err := q.UpsertPlayerSeen(context.Background(), gen.UpsertPlayerSeenParams{
		AlderonID: testAGID, LastKnownName: "testplayer", Rating: 1200,
	}); err != nil {
		t.Fatalf("seed player: %v", err)
	}
	if err := q.EnsureBankAccount(context.Background(), testAGID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
}

// TestOneOperationInFlight is THE banking safety property, and it is checked
// against the database rather than the Go code because that is where it is
// enforced: two replicas reading the same marks balance and both acting on it
// is exactly what the partial unique index exists to stop.
func TestOneOperationInFlight(t *testing.T) {
	_, store := migrated(t)
	q := store.Queries()
	ctx := context.Background()
	seedPlayer(t, q)

	before := int64(3838)
	first, err := q.BeginOperation(ctx, gen.BeginOperationParams{
		AlderonID: testAGID, DiscordUserID: "1",
		Direction: gen.BankDirectionDeposit, Amount: 100, MarksBefore: &before,
	})
	if err != nil {
		t.Fatalf("first operation rejected: %v", err)
	}

	_, err = q.BeginOperation(ctx, gen.BeginOperationParams{
		AlderonID: testAGID, DiscordUserID: "1",
		Direction: gen.BankDirectionWithdraw, Amount: 50, MarksBefore: &before,
	})
	if err == nil {
		t.Fatal("a second concurrent operation was accepted")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "bank_ledger_one_inflight" {
		t.Fatalf("second operation failed for the wrong reason: %v", err)
	}

	// Resolving the first must free the slot: the guard is "one at a time",
	// not "one ever".
	if err := q.CompleteOperation(ctx, gen.CompleteOperationParams{ID: first.ID}); err != nil {
		t.Fatalf("complete first: %v", err)
	}
	if _, err := q.BeginOperation(ctx, gen.BeginOperationParams{
		AlderonID: testAGID, DiscordUserID: "1",
		Direction: gen.BankDirectionWithdraw, Amount: 50, MarksBefore: &before,
	}); err != nil {
		t.Fatalf("operation after the first resolved was rejected: %v", err)
	}
}

// TestDebitBankCannotOverdraw covers the race the application cannot see: a
// balance read a moment ago, spent by another path before this update lands.
// DebitBank must report zero rows rather than driving the balance negative.
func TestDebitBankCannotOverdraw(t *testing.T) {
	_, store := migrated(t)
	q := store.Queries()
	ctx := context.Background()
	seedPlayer(t, q)

	if err := q.CreditBank(ctx, gen.CreditBankParams{AlderonID: testAGID, Balance: 100}); err != nil {
		t.Fatalf("credit: %v", err)
	}
	rows, err := q.DebitBank(ctx, gen.DebitBankParams{AlderonID: testAGID, Balance: 101})
	if err != nil {
		t.Fatalf("overdraw debit errored instead of reporting no rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("overdraw moved %d rows, want 0", rows)
	}
	account, err := q.GetBankAccount(ctx, testAGID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if account.Balance != 100 {
		t.Fatalf("balance is %d after a refused overdraw, want 100", account.Balance)
	}
}

// TestLinkIsOneToOne proves both directions are enforced by constraints, not by
// a read-then-write the application could race.
func TestLinkIsOneToOne(t *testing.T) {
	_, store := migrated(t)
	q := store.Queries()
	ctx := context.Background()
	seedPlayer(t, q)

	if err := q.UpsertPlayerSeen(ctx, gen.UpsertPlayerSeenParams{
		AlderonID: "111-222-333", LastKnownName: "other", Rating: 1200,
	}); err != nil {
		t.Fatalf("seed second player: %v", err)
	}

	if err := q.CreateLink(ctx, gen.CreateLinkParams{DiscordUserID: "discord-a", AlderonID: testAGID}); err != nil {
		t.Fatalf("first link: %v", err)
	}
	if err := q.CreateLink(ctx, gen.CreateLinkParams{
		DiscordUserID: "discord-a", AlderonID: "111-222-333",
	}); err == nil {
		t.Error("one Discord user was allowed two identities")
	}
	if err := q.CreateLink(ctx, gen.CreateLinkParams{
		DiscordUserID: "discord-b", AlderonID: testAGID,
	}); err == nil {
		t.Error("one identity was allowed two Discord users")
	}
}

// TestUnlinkPreservesStatsAndBalance is the promise /link remove makes: the
// Discord account is a label, and removing it must not destroy the identity's
// record or its marks.
func TestUnlinkPreservesStatsAndBalance(t *testing.T) {
	_, store := migrated(t)
	q := store.Queries()
	ctx := context.Background()
	seedPlayer(t, q)

	if err := q.CreateLink(ctx, gen.CreateLinkParams{DiscordUserID: "discord-a", AlderonID: testAGID}); err != nil {
		t.Fatalf("link: %v", err)
	}
	if err := q.CreditKill(ctx, gen.CreditKillParams{AlderonID: testAGID, Rating: 1234}); err != nil {
		t.Fatalf("credit kill: %v", err)
	}
	if err := q.CreditBank(ctx, gen.CreditBankParams{AlderonID: testAGID, Balance: 500}); err != nil {
		t.Fatalf("credit bank: %v", err)
	}

	rows, err := q.DeleteLinkByDiscordID(ctx, "discord-a")
	if err != nil || rows != 1 {
		t.Fatalf("unlink: rows=%d err=%v", rows, err)
	}

	player, err := q.GetPlayer(ctx, testAGID)
	if err != nil {
		t.Fatalf("player gone after unlink: %v", err)
	}
	if player.Kills != 1 || player.Rating != 1234 {
		t.Errorf("stats changed on unlink: kills=%d rating=%v", player.Kills, player.Rating)
	}
	account, err := q.GetBankAccount(ctx, testAGID)
	if err != nil {
		t.Fatalf("bank account gone after unlink: %v", err)
	}
	if account.Balance != 500 {
		t.Errorf("balance is %d after unlink, want 500", account.Balance)
	}

	// And relinking restores the connection to the same record.
	if err := q.CreateLink(ctx, gen.CreateLinkParams{DiscordUserID: "discord-b", AlderonID: testAGID}); err != nil {
		t.Fatalf("relink: %v", err)
	}
	relinked, err := q.GetPlayerByDiscordID(ctx, "discord-b")
	if err != nil {
		t.Fatalf("lookup after relink: %v", err)
	}
	if relinked.Kills != 1 {
		t.Errorf("relinked player has %d kills, want 1", relinked.Kills)
	}
}

// TestKillEventDedupe covers the retry protection: the same payload delivered
// twice must be recorded once, and the second must be distinguishable from an
// error so the caller can count it rather than alarm on it.
func TestKillEventDedupe(t *testing.T) {
	_, store := migrated(t)
	q := store.Queries()
	ctx := context.Background()

	params := gen.InsertKillEventParams{
		DedupeKey:  []byte("deadbeefdeadbeefdeadbeefdeadbeef"),
		ServerGuid: "guid",
		Payload:    []byte(`{}`),
		VictimAgid: testAGID, VictimName: "testplayer",
		DamageType: "DT_ATTACK", Credited: false,
	}
	if _, err := q.InsertKillEvent(ctx, params); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err := q.InsertKillEvent(ctx, params)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("duplicate insert returned %v, want pgx.ErrNoRows so the caller can count it", err)
	}

	unrated, err := q.CountUnratedEvents(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if unrated != 1 {
		t.Fatalf("%d unrated events after a duplicate delivery, want 1", unrated)
	}
}

// TestInTxRollsBack proves the primitive the banking path is built on actually
// unwinds, rather than leaving a half-applied transfer behind.
func TestInTxRollsBack(t *testing.T) {
	_, store := migrated(t)
	ctx := context.Background()
	seedPlayer(t, store.Queries())

	sentinel := errors.New("deliberate")
	err := store.InTx(ctx, func(q *gen.Queries) error {
		if err := q.CreditBank(ctx, gen.CreditBankParams{AlderonID: testAGID, Balance: 999}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("InTx returned %v, want the callback's error", err)
	}
	account, err := store.Queries().GetBankAccount(ctx, testAGID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if account.Balance != 0 {
		t.Fatalf("balance is %d after a rolled-back transaction, want 0", account.Balance)
	}
}

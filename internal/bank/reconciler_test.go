package bank_test

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/USA-RedDragon/obsidibot/internal/bank"
	"github.com/USA-RedDragon/obsidibot/internal/config"
	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/dbtest"
	"github.com/USA-RedDragon/obsidibot/internal/metrics"
	"github.com/USA-RedDragon/obsidibot/internal/pot"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testAGID = "555-000-101"

// fakeRCON reports a marks balance and records every command, so a test can
// assert that recovery NEVER re-issues a mutating one.
type fakeRCON struct {
	mu       sync.Mutex
	commands []string
	marks    int64
	online   bool
}

func (f *fakeRCON) Execute(_ context.Context, command string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command)
	fields := strings.Fields(command)
	if !f.online {
		return fmt.Sprintf("(%s): No player with the username '%s'.", command, fields[1]), nil
	}
	if strings.EqualFold(fields[0], "PlayerInfo") {
		return fmt.Sprintf(
			"(%s): Name: kitty / AGID: %s / Dinosaur: Ceratosaurus / Role: None / Marks: %d / Growth: 1 /"+
				" Location: (X=1.0 Y=2.0 Z=3.0)", command, testAGID, f.marks), nil
	}
	return fmt.Sprintf("(%s): ok", command), nil
}

func (f *fakeRCON) mutations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, cmd := range f.commands {
		if strings.HasPrefix(cmd, "AddMarks") || strings.HasPrefix(cmd, "RemoveMarks") ||
			strings.HasPrefix(cmd, "SetMarks") {
			out = append(out, cmd)
		}
	}
	return out
}

func migrationsFS(t *testing.T) fs.FS {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	return os.DirFS(filepath.Join(filepath.Dir(thisFile), "..", "..", "schema", "migrations"))
}

type harness struct {
	pool  *pgxpool.Pool
	store *db.Store
	rcon  *fakeRCON
	rec   *bank.Reconciler
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	pool := dbtest.Pool(t)
	dbtest.Reset(t, pool)
	if err := db.Migrate(context.Background(), pool, migrationsFS(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := db.NewStore(pool)
	fake := &fakeRCON{online: true, marks: 1000}
	cfg := &config.Config{Bank: config.Bank{
		CooldownSeconds: 0, VerifyAttempts: 2, VerifyBackoffSeconds: 1,
	}}

	ctx := context.Background()
	q := store.Queries()
	if err := q.UpsertPlayerSeen(ctx, gen.UpsertPlayerSeenParams{
		AlderonID: testAGID, LastKnownName: "kitty", Rating: 1200,
	}); err != nil {
		t.Fatalf("seed player: %v", err)
	}
	if err := q.EnsureBankAccount(ctx, testAGID); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	return &harness{
		pool: pool, store: store, rcon: fake,
		rec: bank.NewReconciler(store, pot.NewClient(fake, nil), metrics.New(), cfg),
	}
}

// stranded creates a ledger row in the given state and ages it past the
// reconciler's threshold, standing in for a request that died mid-flight.
// A stranded row always describes the same transfer: 400 marks against a
// starting balance of 1000. Fixing both keeps each test's arithmetic obvious --
// a confirmed deposit must leave exactly 600, a confirmed withdraw exactly 1400.
const (
	strandedAmount int64 = 400
	strandedBefore int64 = 1000
)

func (h *harness) stranded(t *testing.T, direction gen.BankDirection, state gen.BankState) int64 {
	marksBefore := strandedBefore
	t.Helper()
	ctx := context.Background()
	row, err := h.store.Queries().BeginOperation(ctx, gen.BeginOperationParams{
		AlderonID: testAGID, DiscordUserID: "d1",
		Direction: direction, Amount: strandedAmount, MarksBefore: &marksBefore,
	})
	if err != nil {
		t.Fatalf("open row: %v", err)
	}
	if _, err := h.pool.Exec(ctx,
		"update bank_ledger set state=$2, created_at = now() - interval '5 minutes' where id=$1",
		row.ID, state); err != nil {
		t.Fatalf("age row: %v", err)
	}
	return row.ID
}

func (h *harness) state(t *testing.T, id int64) gen.BankLedger {
	t.Helper()
	row, err := h.store.Queries().GetOperation(context.Background(), id)
	if err != nil {
		t.Fatalf("read row: %v", err)
	}
	return row
}

func (h *harness) balance(t *testing.T) int64 {
	t.Helper()
	account, err := h.store.Queries().GetBankAccount(context.Background(), testAGID)
	if err != nil {
		t.Fatalf("read balance: %v", err)
	}
	return account.Balance
}

// TestPendingRowsAreClosedAsFailed is the one automatic resolution that is
// PROVABLY safe: in_flight is written before the command goes out, so a row
// still in pending means the game was never contacted and nothing moved.
func TestPendingRowsAreClosedAsFailed(t *testing.T) {
	h := newHarness(t)
	id := h.stranded(t, gen.BankDirectionDeposit, gen.BankStatePending)

	if err := h.rec.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	row := h.state(t, id)
	if row.State != gen.BankStateFailed {
		t.Fatalf("state = %s, want failed", row.State)
	}
	if h.balance(t) != 0 {
		t.Errorf("a balance moved for a command that was never sent: %d", h.balance(t))
	}
	if cmds := h.rcon.mutations(); len(cmds) != 0 {
		t.Fatalf("recovery issued mutating commands: %v", cmds)
	}
}

// TestConfirmedInFlightRowIsSettled: the marks are exactly where a successful
// command would have left them, so the database half is finished.
func TestConfirmedInFlightRowIsSettled(t *testing.T) {
	h := newHarness(t)
	// A deposit of 400 from 1000 should leave exactly 600.
	h.rcon.marks = 600
	id := h.stranded(t, gen.BankDirectionDeposit, gen.BankStateInFlight)

	if err := h.rec.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	row := h.state(t, id)
	if row.State != gen.BankStateApplied {
		t.Fatalf("state = %s, want applied", row.State)
	}
	if h.balance(t) != 400 {
		t.Fatalf("balance = %d, want the 400 that was confirmed", h.balance(t))
	}
	if cmds := h.rcon.mutations(); len(cmds) != 0 {
		t.Fatalf("recovery issued mutating commands: %v", cmds)
	}
}

// TestUnconfirmableRowIsParkedAndNeverRetried is the discipline that keeps
// AddMarks from minting currency. The balance does not match, so the outcome is
// unknown -- and unknown means a human looks, never a retry.
func TestUnconfirmableRowIsParkedAndNeverRetried(t *testing.T) {
	h := newHarness(t)
	// Nothing moved, so it looks like the command never landed -- but the
	// player is live and may simply have earned marks back, so this cannot be
	// concluded either way.
	h.rcon.marks = 1000
	id := h.stranded(t, gen.BankDirectionWithdraw, gen.BankStateInFlight)

	// Sweep until the attempt budget is used up.
	for range 3 {
		if err := h.rec.Sweep(context.Background()); err != nil {
			t.Fatalf("sweep: %v", err)
		}
	}

	row := h.state(t, id)
	if row.State != gen.BankStateNeedsReview {
		t.Fatalf("state = %s, want needs_review", row.State)
	}
	if row.Error == nil || *row.Error == "" {
		t.Error("the parked row does not say why")
	}
	if h.balance(t) != 0 {
		t.Errorf("a balance moved for an unconfirmed transfer: %d", h.balance(t))
	}
	if cmds := h.rcon.mutations(); len(cmds) != 0 {
		t.Fatalf("recovery issued mutating commands, which would mint currency: %v", cmds)
	}
}

// TestReconcilerIsStricterThanTheRequestPath. In the request path a deposit is
// confirmed by "marks went down by AT LEAST the amount", which is sound because
// no time has passed. Minutes later that is no longer evidence -- the player may
// simply have spent marks -- so the reconciler demands the exact figure.
func TestReconcilerIsStricterThanTheRequestPath(t *testing.T) {
	h := newHarness(t)
	// Consistent with the deposit having landed AND the player having spent
	// another 50. The inequality would accept this; the reconciler must not.
	h.rcon.marks = 550
	id := h.stranded(t, gen.BankDirectionDeposit, gen.BankStateInFlight)

	for range 3 {
		if err := h.rec.Sweep(context.Background()); err != nil {
			t.Fatalf("sweep: %v", err)
		}
	}

	if row := h.state(t, id); row.State != gen.BankStateNeedsReview {
		t.Fatalf("state = %s; a drifted balance was treated as confirmation", row.State)
	}
}

// TestOfflinePlayerIsWaitedForThenParked: nothing can be observed while they
// are logged out, and their marks are not going anywhere in the meantime.
func TestOfflinePlayerIsWaitedForThenParked(t *testing.T) {
	h := newHarness(t)
	h.rcon.online = false
	id := h.stranded(t, gen.BankDirectionDeposit, gen.BankStateInFlight)

	if err := h.rec.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if row := h.state(t, id); row.State != gen.BankStateInFlight {
		t.Fatalf("state = %s after one attempt; it should still be waiting", row.State)
	}

	for range 3 {
		if err := h.rec.Sweep(context.Background()); err != nil {
			t.Fatalf("sweep: %v", err)
		}
	}
	if row := h.state(t, id); row.State != gen.BankStateNeedsReview {
		t.Fatalf("state = %s, want needs_review once the budget ran out", row.State)
	}
}

// TestFreshRowsAreLeftAlone: the reconciler must not race a request that is
// simply still running.
func TestFreshRowsAreLeftAlone(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	before := int64(1000)
	row, err := h.store.Queries().BeginOperation(ctx, gen.BeginOperationParams{
		AlderonID: testAGID, DiscordUserID: "d1",
		Direction: gen.BankDirectionDeposit, Amount: 400, MarksBefore: &before,
	})
	if err != nil {
		t.Fatalf("open row: %v", err)
	}

	if err := h.rec.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := h.state(t, row.ID); got.State != gen.BankStatePending {
		t.Fatalf("a just-created row was resolved as %s", got.State)
	}
}

// TestWithdrawConfirmedByObservation covers the direction where an unrecovered
// row costs the player marks they were owed.
func TestWithdrawConfirmedByObservation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.store.Queries().CreditBank(ctx, gen.CreditBankParams{
		AlderonID: testAGID, Balance: 500,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A withdraw of 400 from 1000 leaves exactly 1400.
	h.rcon.marks = 1400
	id := h.stranded(t, gen.BankDirectionWithdraw, gen.BankStateInFlight)

	if err := h.rec.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if row := h.state(t, id); row.State != gen.BankStateApplied {
		t.Fatalf("state = %s, want applied", row.State)
	}
	if h.balance(t) != 100 {
		t.Fatalf("balance = %d, want 100 after the confirmed withdraw", h.balance(t))
	}
}

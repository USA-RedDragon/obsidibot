package bank_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SRS-Hosting/rcon"
	"github.com/USA-RedDragon/obsidibot/internal/bank"
	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/metrics"
)

// TestFirstOperationSurvivesTheCooldown covers the state every player passes
// through exactly once: no ledger history at all.
//
// Every other test in this package runs with the cooldown disabled, which short
// circuits before the ledger is read -- which is precisely why a query that
// cannot answer "this player has never banked" went unnoticed. An aggregate
// returns one row holding NULL over zero rows, the scan fails, and the failure
// is not pgx.ErrNoRows, so the "no history" branch never runs. The player is
// then refused forever, because being refused is also what stops them ever
// getting the ledger row that would let them through.
func TestFirstOperationSurvivesTheCooldown(t *testing.T) {
	h := newHarness(t)
	h.cfg.Bank.CooldownSeconds = 10
	h.rcon.marks = 1000

	result, err := h.vault.Deposit(context.Background(), "d1", testAGID, "", 400)
	if err != nil {
		t.Fatalf("a player's first ever deposit was rejected: %v", err)
	}
	if result.Moved != 400 {
		t.Errorf("moved = %d, want 400", result.Moved)
	}
	if h.balance(t) != 400 {
		t.Errorf("balance = %d, want 400", h.balance(t))
	}
}

// TestCooldownStillBitesAfterAnOperation is the other half of the test above:
// the fix must make the FIRST operation possible without making the cooldown
// itself unenforceable.
func TestCooldownStillBitesAfterAnOperation(t *testing.T) {
	h := newHarness(t)
	h.cfg.Bank.CooldownSeconds = 600
	h.rcon.marks = 1000
	ctx := context.Background()

	if _, err := h.vault.Deposit(ctx, "d1", testAGID, "", 100); err != nil {
		t.Fatalf("first deposit: %v", err)
	}
	if _, err := h.vault.Deposit(ctx, "d1", testAGID, "", 100); !errors.Is(err, bank.ErrTooSoon) {
		t.Fatalf("second deposit err = %v, want ErrTooSoon", err)
	}
	if h.balance(t) != 100 {
		t.Errorf("balance = %d, want the 100 from the one operation that was allowed", h.balance(t))
	}
	if got := h.counted(t, gen.BankDirectionDeposit, metrics.ResultUserError); got != 1 {
		t.Errorf("cooldown refusals counted = %v, want 1", got)
	}
}

// TestASlowTransferIsNotCreditedTwice is the interleaving that mints currency.
//
// The command SUCCEEDS, slowly. While its reply is still coming back the
// reconciler picks the row up -- which it is entitled to do, because a row that
// has been open longer than the threshold may well be abandoned -- observes the
// marks exactly where a successful transfer would have left them, and finishes
// the database half. The request path then wakes up and finishes the same half
// again. Nothing in the second write checks that the row is still in_flight, so
// the bank is credited twice for one deposit.
func TestASlowTransferIsNotCreditedTwice(t *testing.T) {
	h := newHarness(t)
	h.rcon.marks = 1000
	h.rcon.during["RemoveMarks"] = func() {
		// The marks have already moved in game at this point; only the reply
		// is outstanding. That is exactly what the reconciler sees.
		h.age(t)
		if err := h.rec.Sweep(context.Background()); err != nil {
			t.Errorf("sweep: %v", err)
		}
	}

	result, err := h.vault.Deposit(context.Background(), "d1", testAGID, "", 400)
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}

	if h.balance(t) != 400 {
		t.Fatalf("balance = %d, want 400: the deposit was credited more than once", h.balance(t))
	}
	if result.Moved != 400 {
		t.Errorf("moved = %d, want 400", result.Moved)
	}
	if result.Balance != 400 {
		t.Errorf("reported balance = %d, want 400", result.Balance)
	}
	row := h.onlyRow(t)
	if row.State != gen.BankStateApplied {
		t.Errorf("state = %s, want applied", row.State)
	}
	if cmds := h.rcon.mutations(); len(cmds) != 1 {
		t.Fatalf("the transfer produced %d mutating commands, want exactly 1: %v", len(cmds), cmds)
	}
}

// TestNoCommandGoesOutOnceTheRowIsClosed is the window between the ledger row
// being opened and being claimed.
//
// A row still in pending PROVES nothing was sent, which is what lets the
// reconciler close it as failed without observing anything. That proof holds
// only while nothing is sent: if the claim silently matches no row and the
// transfer carries on anyway, RemoveMarks takes the player's marks against a
// row that is already terminal and will never be looked at again. The marks are
// simply gone, and nothing is flagged.
func TestNoCommandGoesOutOnceTheRowIsClosed(t *testing.T) {
	h := newHarness(t)
	h.rcon.marks = 1000
	h.closeRowsAtInsert(t)

	_, err := h.vault.Deposit(context.Background(), "d1", testAGID, "", 400)
	if err == nil {
		t.Fatal("the transfer went ahead against a ledger row that was already closed")
	}
	if !errors.Is(err, bank.ErrRaced) {
		t.Errorf("err = %v, want ErrRaced", err)
	}
	if cmds := h.rcon.mutations(); len(cmds) != 0 {
		t.Fatalf("marks were moved against a terminal ledger row, so nothing will ever "+
			"credit them: %v", cmds)
	}
	if h.balance(t) != 0 {
		t.Errorf("balance = %d, want 0", h.balance(t))
	}
	if got := h.counted(t, gen.BankDirectionDeposit, metrics.ResultError); got != 1 {
		t.Errorf("aborted transfers counted = %v, want 1", got)
	}
}

// TestParkOutlivesTheRequestContext: the park is the fail-closed path, and the
// commonest reason to need it is the request's own context expiring. Running it
// on that context fails the write instantly and leaves the row in_flight with
// the marks possibly gone and nobody told.
func TestParkOutlivesTheRequestContext(t *testing.T) {
	h := newHarness(t)
	h.rcon.marks = 1000
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The command lands and its answer is lost, with the request context dying
	// in the same moment -- one dropped connection, both effects.
	h.rcon.fail["RemoveMarks"] = errors.New("connection reset by peer")
	h.rcon.during["RemoveMarks"] = cancel

	_, err := h.vault.Deposit(ctx, "d1", testAGID, "", 400)
	if !errors.Is(err, bank.ErrNeedsReview) {
		t.Fatalf("err = %v, want ErrNeedsReview", err)
	}

	row := h.onlyRow(t)
	if row.State != gen.BankStateNeedsReview {
		t.Fatalf("state = %s, want needs_review: an unconfirmed transfer was left open "+
			"with nothing flagging it", row.State)
	}
	if row.Error == nil || *row.Error == "" {
		t.Error("the parked row does not say why")
	}
	if h.balance(t) != 0 {
		t.Errorf("balance = %d, want 0: nothing was confirmed", h.balance(t))
	}
	if got := h.counted(t, gen.BankDirectionDeposit, metrics.ResultNeedsReview); got != 1 {
		t.Errorf("parked transfers counted = %v, want 1", got)
	}
}

// TestReconcilerDoesNotHoldRowLocksAcrossRCON.
//
// The observation is an RCON round trip that can take as long as
// rcon.timeoutSeconds, and a sweep resolves up to reconcileBatch rows. Making
// those calls inside the resolving transaction holds a write lock on every row
// in the batch -- and a pooled connection -- for minutes, and the request path
// that is trying to finish those very transfers blocks behind it.
func TestReconcilerDoesNotHoldRowLocksAcrossRCON(t *testing.T) {
	h := newHarness(t)
	// A deposit of 400 from 1000 leaves exactly 600, so the row is confirmable.
	h.rcon.marks = 600
	id := h.stranded(t, gen.BankDirectionDeposit, gen.BankStateInFlight)

	var probeErr error
	h.rcon.during["PlayerInfo"] = func() {
		// Another writer touching the same row while the observation is in
		// flight. It gets a deadline of its own so a held lock fails the test
		// rather than hanging it.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, probeErr = h.pool.Exec(ctx,
			"update bank_ledger set interaction_token = 'probe' where id = $1", id)
	}

	if err := h.rec.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if probeErr != nil {
		t.Fatalf("the reconciler held a write transaction across an RCON round trip: %v", probeErr)
	}
	if row := h.state(t, id); row.State != gen.BankStateApplied {
		t.Fatalf("state = %s, want applied", row.State)
	}
}

// TestOneUnwritableRowDoesNotDiscardAnother.
//
// The rows in a sweep are independent transfers belonging to different players.
// Resolving them in one transaction ties them together: whichever row cannot be
// written throws away every resolution that already succeeded -- and, worse,
// their verify_attempts increments, which are the ONLY route by which a row
// that can never be confirmed reaches needs_review. Rows that never get there
// stay in_flight forever, and the one-operation-in-flight index then refuses
// that player every future command.
func TestOneUnwritableRowDoesNotDiscardAnother(t *testing.T) {
	h := newHarness(t)
	h.seedPlayer(t, otherAGID)
	// A deposit of 400 from 1000 leaves exactly 600 for both players, so both
	// rows are confirmable and both want to write.
	h.rcon.marks = 600
	first := h.stranded(t, gen.BankDirectionDeposit, gen.BankStateInFlight)
	second := h.strandedFor(t, otherAGID, gen.BankDirectionDeposit, gen.BankStateInFlight)

	// Hold the second player's account row so the credit that would finish
	// their transfer cannot land.
	release := h.holdAccount(t, otherAGID)

	// A deadline, because the blocked write is meant to fail rather than wait.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := h.rec.Sweep(ctx); err != nil {
		t.Logf("sweep reported: %v", err)
	}
	release()

	if row := h.state(t, first); row.State != gen.BankStateApplied {
		t.Fatalf("first row = %s, want applied: another player's row took this one down with it",
			row.State)
	}
	if h.balanceOf(t, testAGID) != 400 {
		t.Errorf("first player's balance = %d, want 400", h.balanceOf(t, testAGID))
	}

	// The blocked row is untouched and still open, so the next sweep retries
	// it. Its attempt was charged outside the transaction, so the budget that
	// eventually parks it actually advances.
	row := h.state(t, second)
	if row.State != gen.BankStateInFlight {
		t.Errorf("second row = %s, want it left in_flight for the next sweep", row.State)
	}
	if row.VerifyAttempts != 1 {
		t.Errorf("second row's verify_attempts = %d, want 1: a row that only reaches "+
			"needs_review by exhausting the budget would never get there", row.VerifyAttempts)
	}
	if h.balanceOf(t, otherAGID) != 0 {
		t.Errorf("second player's balance = %d, want 0: nothing was written for that row",
			h.balanceOf(t, otherAGID))
	}
}

// TestAClaimedRowIsNotClosedAsAbandoned is the hazard created by reading the
// candidates outside the transaction that resolves them.
//
// "Still pending, so nothing was sent" is a reading of the row, and it stops
// being true the instant the stalled request claims it -- which is exactly what
// the request does immediately before sending the command. Closing the row on
// the strength of a stale reading records "nothing was moved" about marks that
// are about to move, and leaves the request holding a terminal row.
func TestAClaimedRowIsNotClosedAsAbandoned(t *testing.T) {
	h := newHarness(t)
	h.seedPlayer(t, otherAGID)
	// A deposit of 400 from 1000 leaves exactly 600, so the first row resolves
	// by observation -- which is what gives the second row a window.
	h.rcon.marks = 600
	first := h.stranded(t, gen.BankDirectionDeposit, gen.BankStateInFlight)
	second := h.strandedFor(t, otherAGID, gen.BankDirectionDeposit, gen.BankStatePending)

	h.rcon.during["PlayerInfo"] = func() {
		// The stalled request wakes up and claims its row, in the gap between
		// the reconciler reading it as pending and acting on that reading.
		claimed, err := h.store.Queries().MarkOperationInFlight(context.Background(), second)
		if err != nil {
			t.Errorf("claim the second row: %v", err)
		}
		if claimed != 1 {
			t.Errorf("claiming a pending row updated %d rows, want 1", claimed)
		}
	}

	if err := h.rec.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if row := h.state(t, first); row.State != gen.BankStateApplied {
		t.Errorf("first row = %s, want applied", row.State)
	}
	if row := h.state(t, second); row.State != gen.BankStateInFlight {
		t.Fatalf("a row that had just been claimed was closed as %s, on a reading that "+
			"was already stale when it was acted on", row.State)
	}
}

// TestALibraryRefusalClosesTheRowAsFailed covers rcon's fail-fast errors: a
// full command queue or an over-long command is refused by the library BEFORE
// any network activity, so nothing can have moved in game. Classifying those
// as unconfirmed used to park the row as needs_review -- firing the
// operator alert, whose entire value is that it only fires when marks may
// actually be wrong -- for a transfer that provably never left the process.
// With in-game commands sharing the RCON connection, ErrBusy becomes
// player-triggerable at will, so the misclassification would be routine.
// Both directions are covered, because they issue DIFFERENT verbs -- a deposit
// removes marks in game, a withdraw adds them -- and the classification must
// not be accidentally tied to one of them.
func TestALibraryRefusalClosesTheRowAsFailed(t *testing.T) {
	tests := map[string]struct {
		refusal   error
		direction gen.BankDirection
		verb      string
		// wantBalance is the bank balance afterwards: unchanged in both cases,
		// which for a withdraw means the 400 seeded to have something to take.
		wantBalance int64
	}{
		"busy on deposit":      {rcon.ErrBusy, gen.BankDirectionDeposit, "RemoveMarks", 0},
		"too long on withdraw": {rcon.ErrCommandTooLong, gen.BankDirectionWithdraw, "AddMarks", 400},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.rcon.reject[tc.verb] = tc.refusal
			ctx := context.Background()

			var err error
			if tc.direction == gen.BankDirectionDeposit {
				_, err = h.vault.Deposit(ctx, "d1", testAGID, "", 400)
			} else {
				// Something to withdraw, or the request is refused before a
				// command is ever attempted.
				if cerr := h.store.Queries().CreditBank(ctx, gen.CreditBankParams{
					AlderonID: testAGID, Balance: 400,
				}); cerr != nil {
					t.Fatalf("credit: %v", cerr)
				}
				_, err = h.vault.Withdraw(ctx, "d1", testAGID, "", 400)
			}
			if !errors.Is(err, tc.refusal) {
				t.Fatalf("err = %v, want the refusal itself so the caller can say 'try again'", err)
			}

			row := h.onlyRow(t)
			if row.State != gen.BankStateFailed {
				t.Fatalf("state = %s, want failed: a provably-unsent command was left for review", row.State)
			}
			if h.balance(t) != tc.wantBalance {
				t.Errorf("balance = %d, want %d: the refusal moved a balance", h.balance(t), tc.wantBalance)
			}
			if got := h.counted(t, tc.direction, metrics.ResultUserError); got != 1 {
				t.Errorf("refusals counted as user error = %v, want 1", got)
			}
		})
	}
}

// closeRowsAtInsert makes every ledger row arrive already closed. It stands in
// for the reconciler resolving a row in the window between BeginOperation
// committing and the request path claiming it -- a window opened by a stalled
// connection, and only reachable deterministically from a test by moving the
// close into the insert itself.
//
// The schema is dropped and rebuilt for every test, so the trigger goes with it.
func (h *harness) closeRowsAtInsert(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		"create function close_at_insert() returns trigger language plpgsql as " +
			"$$ begin new.state := 'failed'; return new; end $$",
		"create trigger close_at_insert before insert on bank_ledger " +
			"for each row execute function close_at_insert()",
	} {
		if _, err := h.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("install the closing trigger: %v", err)
		}
	}
}

// holdAccount opens a transaction holding a row lock on one player's bank
// account, so any attempt to move that balance blocks until the returned
// function releases it.
func (h *harness) holdAccount(t *testing.T, agid string) func() {
	t.Helper()
	ctx := context.Background()
	conn, err := h.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection: %v", err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		conn.Release()
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx,
		"select balance from bank_accounts where alderon_id = $1 for update", agid); err != nil {
		_ = tx.Rollback(ctx)
		conn.Release()
		t.Fatalf("lock the account row: %v", err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			_ = tx.Rollback(ctx)
			conn.Release()
		})
	}
	t.Cleanup(release)
	return release
}

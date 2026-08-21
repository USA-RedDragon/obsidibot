package bank

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/config"
	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/metrics"
	"github.com/USA-RedDragon/obsidibot/internal/pot"
	"github.com/jackc/pgx/v5"
)

// requestBudget is the longest the in-request path can still be working on a
// row it opened: internal/interactions gives a deferred command exactly this
// long before it cancels the context behind the reply.
//
// Duplicated as a constant rather than imported because interactions depends on
// this package, not the other way round. If that budget changes, this changes
// with it -- a reconciler that starts before the request has given up is a
// reconciler racing a live transfer.
const requestBudget = time.Minute

// reconcileInterval is how often to sweep.
const reconcileInterval = 30 * time.Second

// reconcileBatch bounds one sweep.
const reconcileBatch = 50

// staleAfter is how long an unresolved row must sit before the reconciler
// touches it.
//
// It is DERIVED, not chosen: it has to clear the whole in-request path, which
// is the deferred budget, plus the RCON exchange that can still be outstanding
// inside it (rcon.timeoutSeconds, which config permits up to 60), plus the
// detached write that records the outcome after the request context is gone.
// A hardcoded number smaller than that -- 30 seconds, say, against a 45 second
// RCON timeout -- means the reconciler routinely picks up transfers that are
// simply still running.
//
// The state guards on the terminal transitions are what make that overlap SAFE
// rather than a double credit; this is what makes it RARE. Both are wanted:
// the guards cannot tell the operator that something is wrong, and a reconciler
// that constantly races the request path hides the failures it exists to find.
func (r *Reconciler) staleAfter() time.Duration {
	return requestBudget + r.cfg.RCON.Timeout() + fallbackWriteTimeout
}

// Reconciler resolves ledger rows whose request died before finishing.
//
// # It never re-issues a command
//
// That is the whole discipline. AddMarks is not idempotent, so a retry mints
// currency; a row whose command MAY have landed can only be settled by looking
// at the world, and if looking cannot settle it, a human must.
//
// # What it can and cannot conclude
//
//   - pending: the row never reached in_flight, and in_flight is written BEFORE
//     the command goes out. So the command was definitively never sent, nothing
//     moved, and the row can be closed as failed. This is the only automatic
//     resolution that is provably safe.
//   - in_flight: the outcome is unknown. The balance is re-read, but the check
//     is STRICTER than the in-request one: it demands the exact expected value
//     rather than an inequality. Seconds or minutes have passed and the player
//     has been playing, so "their marks went down by at least the deposit"
//     is no longer evidence of anything -- they may simply have spent some.
//     An exact match is strong evidence; anything else is parked.
//
// Unlike the rating applier this runs on EVERY replica: rows are independent
// per player and the query takes them FOR UPDATE SKIP LOCKED, so replicas share
// the work rather than contending for it.
type Reconciler struct {
	store   *db.Store
	rcon    *pot.Client
	metrics *metrics.Metrics
	cfg     *config.Config
}

// NewReconciler builds the sweeper.
func NewReconciler(store *db.Store, rcon *pot.Client, m *metrics.Metrics, cfg *config.Config) *Reconciler {
	return &Reconciler{store: store, rcon: rcon, metrics: m, cfg: cfg}
}

// Run sweeps until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) error {
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()

	for {
		if err := r.sweep(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.ErrorContext(ctx, "bank reconciliation failed", "error", err)
		}
		r.publishReviewGauge(ctx)

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Sweep resolves what it can, once. Exported so tests can drive one pass
// deterministically rather than waiting on the ticker.
func (r *Reconciler) Sweep(ctx context.Context) error { return r.sweep(ctx) }

func (r *Reconciler) sweep(ctx context.Context) error {
	rows, err := r.candidates(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := r.resolve(ctx, row); err != nil {
			// ONE ROW MUST NOT TAKE THE OTHERS DOWN WITH IT. Each row is
			// resolved in its own transaction precisely so that a failure here
			// is contained: the rows are independent transfers belonging to
			// different players, and discarding a batch would also discard the
			// verify_attempts increments that are the only way a row ever
			// reaches needs_review.
			slog.ErrorContext(ctx, "could not resolve an unfinished transfer",
				"ledgerId", row.ID, "error", err)
		}
	}
	return nil
}

// candidates reads a batch of unresolved rows in a SHORT transaction of its
// own.
//
// The observation each row needs is an RCON round trip that can take up to
// rcon.timeoutSeconds, and there can be reconcileBatch of them. Doing that
// inside the transaction that resolves them would sit idle-in-transaction for
// minutes, holding a pooled connection and a FOR UPDATE lock on every row in
// the batch -- which the request path then blocks on, for the very transfers it
// is trying to finish.
//
// Correctness no longer rests on holding those locks: every terminal transition
// is guarded by the row's state, so a row resolved by someone else while we
// were reading the world updates 0 rows and is left alone.
func (r *Reconciler) candidates(ctx context.Context) ([]gen.BankLedger, error) {
	var rows []gen.BankLedger
	err := r.store.InTx(ctx, func(q *gen.Queries) error {
		var err error
		rows, err = q.UnresolvedOperations(ctx, gen.UnresolvedOperationsParams{
			OlderThanSeconds: int32(r.staleAfter().Seconds()),
			Limit:            reconcileBatch,
		})
		if err != nil {
			return fmt.Errorf("read unresolved operations: %w", err)
		}
		return nil
	})
	return rows, err
}

// resolve settles one row: the observation happens OUTSIDE any transaction, and
// only the conclusion drawn from it is written, in a transaction of its own.
func (r *Reconciler) resolve(ctx context.Context, row gen.BankLedger) error {
	q := r.store.Queries()

	if row.State == gen.BankStatePending {
		// PROVABLY nothing happened: in_flight is written before the command.
		reason := "abandoned before the game server was contacted; nothing was moved"
		closed, err := q.FailAbandonedOperation(ctx, gen.FailAbandonedOperationParams{
			ID: row.ID, Error: &reason,
		})
		if err != nil {
			return fmt.Errorf("close abandoned row %d: %w", row.ID, err)
		}
		if closed == 0 {
			// It stopped being pending between the read and now: the stalled
			// request woke up and claimed it, or another replica closed it.
			// Either way this row's command may be going out RIGHT NOW, and
			// the reading that made it safe to close is stale.
			return nil
		}
		slog.InfoContext(ctx, "closed an abandoned transfer that never reached the game",
			"ledgerId", row.ID, "direction", row.Direction)
		return nil
	}

	// The attempt is charged BEFORE the observation and OUTSIDE the transaction
	// that later writes the conclusion. A row whose only route to needs_review
	// is exhausting this budget would otherwise never get there: if the write
	// were rolled back with the rest of a failed batch, the count would reset
	// every sweep and the row would stay in_flight forever -- and the partial
	// unique index would then refuse that player every future command.
	//
	// The state guard doubles as the claim: no row back means it has already
	// been resolved, so no RCON round trip is spent on it.
	attempts, err := q.RecordVerifyAttempt(ctx, row.ID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("record verify attempt for %d: %w", row.ID, err)
	}

	player, err := r.rcon.PlayerInfo(ctx, row.AlderonID)
	switch {
	case errors.Is(err, pot.ErrPlayerNotOnline):
		// Nothing can be observed while they are logged out. Keep waiting
		// until the attempt budget runs out; their marks are not going
		// anywhere in the meantime.
		if int(attempts) >= r.cfg.Bank.VerifyAttempts {
			r.parkRow(ctx, row, nil, "the player did not come back online to confirm the transfer")
		}
		return nil
	case err != nil:
		if int(attempts) >= r.cfg.Bank.VerifyAttempts {
			r.parkRow(ctx, row, nil, "could not reach the game server to confirm: "+err.Error())
		}
		return nil
	}

	if row.MarksBefore == nil {
		r.parkRow(ctx, row, &player.Marks, "no marks reading was recorded before the transfer")
		return nil
	}

	// STRICTER than the in-request check on purpose: see the type comment.
	expected := *row.MarksBefore - row.Amount
	if row.Direction == gen.BankDirectionWithdraw {
		expected = *row.MarksBefore + row.Amount
	}
	if player.Marks != expected {
		if int(attempts) >= r.cfg.Bank.VerifyAttempts {
			r.parkRow(ctx, row, &player.Marks,
				fmt.Sprintf("expected %d marks after the transfer but observed %d", expected, player.Marks))
		}
		return nil
	}

	return r.applyConfirmed(ctx, row, player.Marks)
}

// errBankShort means a confirmed withdraw could not be covered by the bank
// balance. It unwinds the transaction so nothing is half-applied, and the row
// is parked outside it.
var errBankShort = errors.New("bank: the balance no longer covers a delivered withdraw")

// applyConfirmed finishes the database half of a transfer the observation just
// confirmed. The balance move and the row's closure land together, because a
// moved balance and an open row are indistinguishable from theft.
func (r *Reconciler) applyConfirmed(ctx context.Context, row gen.BankLedger, observed int64) error {
	// The full requested amount: the exact-balance check by the caller is what
	// proves it, and a partial move would not have matched.
	moved := row.Amount

	err := r.store.InTx(ctx, func(q *gen.Queries) error {
		// The state guard is taken FIRST, so the row is what claims the balance
		// move. If the request path finished this transfer while we were
		// reading the player's marks, this matches 0 rows and the transaction
		// unwinds instead of crediting a second time.
		closed, err := q.CompleteOperation(ctx, gen.CompleteOperationParams{
			ID: row.ID, Moved: &moved, MarksAfter: &observed,
		})
		if err != nil {
			return fmt.Errorf("close reconciled row %d: %w", row.ID, err)
		}
		if closed == 0 {
			return errAlreadyResolved
		}
		if row.Direction == gen.BankDirectionDeposit {
			if err := q.CreditBank(ctx, gen.CreditBankParams{
				AlderonID: row.AlderonID, Balance: row.Amount,
			}); err != nil {
				return fmt.Errorf("credit bank for %d: %w", row.ID, err)
			}
			return nil
		}
		rows, err := q.DebitBank(ctx, gen.DebitBankParams{
			AlderonID: row.AlderonID, Balance: row.Amount,
		})
		if err != nil {
			return fmt.Errorf("debit bank for %d: %w", row.ID, err)
		}
		if rows == 0 {
			return errBankShort
		}
		return nil
	})

	switch {
	case errors.Is(err, errAlreadyResolved):
		// Someone else settled it first. Nothing to do and nothing wrong.
		return nil
	case errors.Is(err, errBankShort):
		// The marks were handed out and the bank cannot cover them. That is
		// exactly the situation a human must see -- parked in its own statement
		// because the transaction above is already unwound.
		r.parkRow(ctx, row, &observed,
			"the marks were delivered in game but the bank balance no longer covers them")
		return nil
	case err != nil:
		return err
	}
	slog.InfoContext(ctx, "reconciled an unfinished transfer by observing the result",
		"ledgerId", row.ID, "direction", row.Direction)
	return nil
}

// parkRow flags a row for a human. It runs OUTSIDE any transaction on purpose:
// this is the fail-closed path, and a park issued inside a transaction that
// later aborts -- or after a statement that already failed, which Postgres
// answers by refusing everything until rollback -- is silently lost, leaving a
// row in_flight forever with nobody told.
func (r *Reconciler) parkRow(ctx context.Context, row gen.BankLedger, observed *int64, reason string) {
	parked, err := r.store.Queries().ParkForReview(ctx, gen.ParkForReviewParams{
		ID: row.ID, Error: &reason, MarksAfter: observed,
	})
	switch {
	case err != nil:
		slog.ErrorContext(ctx, "could not flag a ledger row for review", "ledgerId", row.ID, "error", err)
		return
	case parked == 0:
		// The request path resolved it while we were observing; it knew the
		// command's own answer, which is better evidence than ours.
		slog.WarnContext(ctx, "a ledger row was resolved elsewhere before it could be flagged for review",
			"ledgerId", row.ID)
		return
	}
	if r.metrics != nil {
		r.metrics.BankOperationsTotal.WithLabelValues(string(row.Direction), metrics.ResultNeedsReview).Inc()
	}
	slog.ErrorContext(ctx, "a marks transfer was flagged for review",
		"ledgerId", row.ID, "direction", row.Direction, "amount", row.Amount, "reason", reason)
}

// publishReviewGauge keeps the alert honest across restarts: the counter is
// re-read from the ledger rather than accumulated in memory.
func (r *Reconciler) publishReviewGauge(ctx context.Context) {
	if r.metrics == nil {
		return
	}
	count, err := r.store.Queries().CountNeedsReview(ctx)
	if err != nil {
		slog.DebugContext(ctx, "could not count rows needing review", "error", err)
		return
	}
	r.metrics.BankNeedsReview.Set(float64(count))
}

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
)

// staleAfter is how long an unresolved row must sit before the reconciler
// touches it. It has to clear the in-request path comfortably, or the
// reconciler would race a transfer that is simply still running.
const staleAfter = 30 * time.Second

// reconcileInterval is how often to sweep.
const reconcileInterval = 30 * time.Second

// reconcileBatch bounds one sweep.
const reconcileBatch = 50

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

// Sweep resolves what it can, once. Exported for tests and for a future
// operator command.
func (r *Reconciler) Sweep(ctx context.Context) error { return r.sweep(ctx) }

func (r *Reconciler) sweep(ctx context.Context) error {
	// The rows are read inside the transaction that resolves them, so
	// SKIP LOCKED actually excludes another replica rather than merely
	// reordering the work.
	return r.store.InTx(ctx, func(q *gen.Queries) error {
		rows, err := q.UnresolvedOperations(ctx, gen.UnresolvedOperationsParams{
			OlderThanSeconds: int32(staleAfter.Seconds()),
			Limit:            reconcileBatch,
		})
		if err != nil {
			return fmt.Errorf("read unresolved operations: %w", err)
		}
		for _, row := range rows {
			if err := r.resolve(ctx, q, row); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *Reconciler) resolve(ctx context.Context, q *gen.Queries, row gen.BankLedger) error {
	if row.State == gen.BankStatePending {
		// PROVABLY nothing happened: in_flight is written before the command.
		reason := "abandoned before the game server was contacted; nothing was moved"
		if err := q.FailOperation(ctx, gen.FailOperationParams{ID: row.ID, Error: &reason}); err != nil {
			return fmt.Errorf("close abandoned row %d: %w", row.ID, err)
		}
		slog.InfoContext(ctx, "closed an abandoned transfer that never reached the game",
			"ledgerId", row.ID, "direction", row.Direction)
		return nil
	}

	attempts, err := q.RecordVerifyAttempt(ctx, row.ID)
	if err != nil {
		return fmt.Errorf("record verify attempt for %d: %w", row.ID, err)
	}

	player, err := r.rcon.PlayerInfo(ctx, row.AlderonID)
	switch {
	case errors.Is(err, pot.ErrPlayerNotOnline):
		// Nothing can be observed while they are logged out. Keep waiting
		// until the attempt budget runs out; their marks are not going
		// anywhere in the meantime.
		if int(attempts) >= r.cfg.Bank.VerifyAttempts {
			r.parkRow(ctx, q, row, nil, "the player did not come back online to confirm the transfer")
		}
		return nil
	case err != nil:
		if int(attempts) >= r.cfg.Bank.VerifyAttempts {
			r.parkRow(ctx, q, row, nil, "could not reach the game server to confirm: "+err.Error())
		}
		return nil
	}

	if row.MarksBefore == nil {
		r.parkRow(ctx, q, row, &player.Marks, "no marks reading was recorded before the transfer")
		return nil
	}

	// STRICTER than the in-request check on purpose: see the type comment.
	expected := *row.MarksBefore - row.Amount
	if row.Direction == gen.BankDirectionWithdraw {
		expected = *row.MarksBefore + row.Amount
	}
	if player.Marks != expected {
		if int(attempts) >= r.cfg.Bank.VerifyAttempts {
			r.parkRow(ctx, q, row, &player.Marks,
				fmt.Sprintf("expected %d marks after the transfer but observed %d", expected, player.Marks))
		}
		return nil
	}

	// Confirmed. Finish the half that was left undone; the balance move and
	// the row's closure land together.
	if row.Direction == gen.BankDirectionDeposit {
		if err := q.CreditBank(ctx, gen.CreditBankParams{
			AlderonID: row.AlderonID, Balance: row.Amount,
		}); err != nil {
			return fmt.Errorf("credit bank for %d: %w", row.ID, err)
		}
	} else {
		rows, err := q.DebitBank(ctx, gen.DebitBankParams{
			AlderonID: row.AlderonID, Balance: row.Amount,
		})
		if err != nil {
			return fmt.Errorf("debit bank for %d: %w", row.ID, err)
		}
		if rows == 0 {
			// The marks were handed out and the bank cannot cover them. That is
			// exactly the situation a human must see.
			r.parkRow(ctx, q, row, &player.Marks,
				"the marks were delivered in game but the bank balance no longer covers them")
			return nil
		}
	}
	// The full requested amount: the exact-balance check above is what proves
	// it, and a partial move would not have matched.
	moved := row.Amount
	if err := q.CompleteOperation(ctx, gen.CompleteOperationParams{
		ID: row.ID, Moved: &moved, MarksAfter: &player.Marks,
	}); err != nil {
		return fmt.Errorf("close reconciled row %d: %w", row.ID, err)
	}
	slog.InfoContext(ctx, "reconciled an unfinished transfer by observing the result",
		"ledgerId", row.ID, "direction", row.Direction)
	return nil
}

func (r *Reconciler) parkRow(ctx context.Context, q *gen.Queries, row gen.BankLedger,
	observed *int64, reason string,
) {
	if err := q.ParkForReview(ctx, gen.ParkForReviewParams{
		ID: row.ID, Error: &reason, MarksAfter: observed,
	}); err != nil {
		slog.ErrorContext(ctx, "could not flag a ledger row for review", "ledgerId", row.ID, "error", err)
		return
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

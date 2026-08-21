// Package bank moves marks between a player's current dinosaur and a
// Discord-side balance.
//
// # The problem this package exists to manage
//
// RCON HAS NO TRANSACTIONS, and AddMarks is NOT IDEMPOTENT. There is no way to
// move marks and update a balance atomically, and no way to ask the game "did
// my last command land?". Every design decision below follows from that.
//
// The rules, in order of importance:
//
//  1. A mutating RCON command is issued AT MOST ONCE per ledger row. Recovery
//     observes the world; it never re-sends. A blind retry mints currency.
//  2. The row is moved to in_flight BEFORE the command goes out. A row still in
//     pending therefore proves the command was never sent, which is the one
//     situation that can be resolved automatically and safely.
//  3. RCON first, database second. The database half is ours and can be retried
//     safely under the ledger row's state; the RCON half cannot.
//  4. Anything that cannot be established lands in needs_review. That is a real
//     cost, paid deliberately: a wrong balance nobody notices is worse than a
//     row a human has to look at.
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
	"github.com/jackc/pgx/v5/pgconn"
)

// Errors a caller renders differently.
var (
	// ErrBusy means the player already has an operation in flight. It comes
	// from the partial unique index, so it holds across replicas.
	ErrBusy = errors.New("bank: an operation is already in progress")
	// ErrTooSoon means the per-user cooldown has not elapsed.
	ErrTooSoon = errors.New("bank: too soon since the last operation")
	// ErrNothingToMove means the clamped amount came out at zero.
	ErrNothingToMove = errors.New("bank: nothing to move")
	// ErrNeedsReview means the transfer's outcome could not be established.
	ErrNeedsReview = errors.New("bank: the transfer could not be confirmed and was flagged for review")
)

// Result describes a completed transfer.
type Result struct {
	// Moved is the amount actually transferred, which may be less than asked
	// for because the request was clamped to what was available.
	Moved int64
	// Clamped reports that the request was reduced.
	Clamped bool
	// Balance is the bank balance afterwards.
	Balance int64
	// InGame is the marks reading taken to confirm the transfer.
	InGame int64
}

// Bank performs transfers.
type Bank struct {
	store   *db.Store
	rcon    *pot.Client
	metrics *metrics.Metrics
	cfg     *config.Config
}

// New builds the bank.
func New(store *db.Store, rcon *pot.Client, m *metrics.Metrics, cfg *config.Config) *Bank {
	return &Bank{store: store, rcon: rcon, metrics: m, cfg: cfg}
}

// AmountAll asks for everything available. It is negative so it cannot collide
// with a real request, which validation already constrains to be positive.
const AmountAll int64 = -1

// Deposit moves marks from the player's current dinosaur into the bank.
func (b *Bank) Deposit(ctx context.Context, discordUserID, agid, token string, requested int64) (Result, error) {
	return b.transfer(ctx, gen.BankDirectionDeposit, discordUserID, agid, token, requested)
}

// Withdraw moves marks from the bank onto the player's current dinosaur.
func (b *Bank) Withdraw(ctx context.Context, discordUserID, agid, token string, requested int64) (Result, error) {
	return b.transfer(ctx, gen.BankDirectionWithdraw, discordUserID, agid, token, requested)
}

//nolint:gocyclo // the length is the state machine; splitting it would hide the ordering that makes it safe
func (b *Bank) transfer(ctx context.Context, direction gen.BankDirection,
	discordUserID, agid, token string, requested int64,
) (Result, error) {
	q := b.store.Queries()

	if err := b.checkCooldown(ctx, agid); err != nil {
		return Result{}, err
	}

	// The player must be in game: marks live on the character they are
	// controlling, so there is nothing to read or move otherwise. This read
	// also establishes marks_before, the evidence the whole verification rests
	// on.
	player, err := b.rcon.PlayerInfo(ctx, agid)
	if err != nil {
		return Result{}, err
	}

	if err := q.EnsureBankAccount(ctx, agid); err != nil {
		return Result{}, fmt.Errorf("ensure bank account: %w", err)
	}
	account, err := q.GetBankAccount(ctx, agid)
	if err != nil {
		return Result{}, fmt.Errorf("read bank account: %w", err)
	}

	// Clamp to what actually exists, so no command is ever issued that could
	// drive either side negative. This is also why it does not matter whether
	// RemoveMarks would permit a negative in-game balance: it is never asked to.
	available := account.Balance
	if direction == gen.BankDirectionDeposit {
		available = player.Marks
	}
	amount := requested
	if amount == AmountAll || amount > available {
		amount = available
	}
	if amount <= 0 {
		return Result{}, ErrNothingToMove
	}

	marksBefore := player.Marks
	row, err := q.BeginOperation(ctx, gen.BeginOperationParams{
		AlderonID:        agid,
		DiscordUserID:    discordUserID,
		Direction:        direction,
		Amount:           amount,
		MarksBefore:      &marksBefore,
		InteractionToken: nullable(token),
	})
	if err != nil {
		// The partial unique index is what serialises concurrent commands,
		// including ones that landed on different replicas.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.ConstraintName == "bank_ledger_one_inflight" {
			return Result{}, ErrBusy
		}
		return Result{}, fmt.Errorf("open ledger row: %w", err)
	}

	// EVERYTHING PAST HERE moves the row to in_flight first. A row left in
	// pending proves the command was never sent; a row in in_flight means the
	// outcome is unknown and only observation can settle it.
	if err := q.MarkOperationInFlight(ctx, row.ID); err != nil {
		// Still pending, so the reconciler can safely close it as failed.
		return Result{}, fmt.Errorf("mark operation in flight: %w", err)
	}

	// The command's own response is the confirmation. The game answers
	// AddMarks and RemoveMarks with what it did and the resulting balance
	// -- "Removed 100 Marks from X. They now have 3838 Marks." -- which is
	// stronger evidence than reading the balance back afterwards, because
	// there is no window in between for the player to earn or spend anything.
	outcome, err := b.issue(ctx, direction, agid, amount)
	if err != nil {
		if unmoved(err) {
			// REFUSED, not merely unanswered: the server told us it did
			// nothing, so this row can be closed cleanly and the player is
			// exactly where they started.
			if closeErr := q.FailOperation(ctx, gen.FailOperationParams{
				ID: row.ID, Error: strPtr(err.Error()),
			}); closeErr != nil {
				slog.ErrorContext(ctx, "could not close a failed ledger row",
					"ledgerId", row.ID, "error", closeErr)
			}
			b.count(direction, metrics.ResultUserError)
			return Result{}, err
		}
		// The command went out and the answer was lost or unreadable. It may
		// have landed. Only observation can settle that, and never a retry.
		b.park(ctx, row.ID, nil, err)
		return Result{}, ErrNeedsReview
	}

	// CREDIT WHAT ACTUALLY MOVED, not what was asked for. RemoveMarks clamps at
	// zero, so a player who spent marks between the balance read and the
	// command moves less than requested -- crediting the request would mint the
	// difference.
	moved := outcome.Moved
	if moved <= 0 {
		b.park(ctx, row.ID, &outcome.Balance, errors.New("the server reported that nothing moved"))
		return Result{}, ErrNeedsReview
	}

	balance, err := b.settle(ctx, row.ID, direction, agid, moved, outcome.Balance)
	if err != nil {
		// The marks HAVE moved and the balance has not. The reconciler retries
		// this half, which is safe because it is guarded by the row's state.
		slog.ErrorContext(ctx, "transfer confirmed but the balance could not be settled",
			"ledgerId", row.ID, "error", err)
		return Result{}, ErrNeedsReview
	}

	b.count(direction, metrics.ResultOK)
	return Result{
		Moved: moved,
		// Clamped covers both reasons the figure can differ from the request:
		// this bot capping it to the balance it read, and the game capping it
		// to what was actually there a moment later.
		Clamped: (requested != AmountAll && requested > moved) || moved < amount,
		Balance: balance,
		InGame:  outcome.Balance,
	}, nil
}

// unmoved reports whether an error proves nothing happened in game.
//
// The distinction is the whole basis of automatic recovery: these are the
// server ANSWERING with a refusal, so the row can be closed. Anything else --
// a timeout, a dropped connection, an unreadable reply -- means the command may
// have landed, and only observation can settle it.
func unmoved(err error) bool {
	return errors.Is(err, pot.ErrPlayerNotOnline) ||
		errors.Is(err, pot.ErrCommandRejected) ||
		errors.Is(err, pot.ErrInvalidIdentifier)
}

// issue sends the one mutating command this row will ever produce, and returns
// the server's own account of what it did.
func (b *Bank) issue(ctx context.Context, direction gen.BankDirection,
	agid string, amount int64,
) (pot.MarksResult, error) {
	if direction == gen.BankDirectionDeposit {
		return b.rcon.RemoveMarks(ctx, agid, amount)
	}
	return b.rcon.AddMarks(ctx, agid, amount)
}

// settle moves the balance and closes the row, in one transaction. A crash
// between the two would be indistinguishable from theft.
func (b *Bank) settle(ctx context.Context, ledgerID int64, direction gen.BankDirection,
	agid string, moved, marksAfter int64,
) (int64, error) {
	var balance int64
	err := b.store.InTx(ctx, func(q *gen.Queries) error {
		if direction == gen.BankDirectionDeposit {
			if err := q.CreditBank(ctx, gen.CreditBankParams{AlderonID: agid, Balance: moved}); err != nil {
				return fmt.Errorf("credit bank: %w", err)
			}
		} else {
			rows, err := q.DebitBank(ctx, gen.DebitBankParams{AlderonID: agid, Balance: moved})
			if err != nil {
				return fmt.Errorf("debit bank: %w", err)
			}
			if rows == 0 {
				// The balance moved under us despite the in-flight guard. Only
				// this package writes balances, so this should be impossible;
				// refusing rather than forcing it keeps the invariant honest.
				return fmt.Errorf("bank balance for %s no longer covers %d", agid, moved)
			}
		}
		if err := q.CompleteOperation(ctx, gen.CompleteOperationParams{
			ID: ledgerID, Moved: &moved, MarksAfter: &marksAfter,
		}); err != nil {
			return fmt.Errorf("close ledger row: %w", err)
		}
		account, err := q.GetBankAccount(ctx, agid)
		if err != nil {
			return fmt.Errorf("read balance: %w", err)
		}
		balance = account.Balance
		return nil
	})
	return balance, err
}

// park flags a row for a human. This is the only path by which marks can be
// wrong, and obsidibot_bank_needs_review is the alert for it.
func (b *Bank) park(ctx context.Context, ledgerID int64, observed *int64, cause error) {
	reason := "the transfer could not be confirmed"
	if cause != nil {
		reason = "could not read the player's marks to confirm: " + cause.Error()
	}
	if err := b.store.Queries().ParkForReview(ctx, gen.ParkForReviewParams{
		ID: ledgerID, Error: &reason, MarksAfter: observed,
	}); err != nil {
		slog.ErrorContext(ctx, "could not flag a ledger row for review",
			"ledgerId", ledgerID, "error", err)
	}
	slog.ErrorContext(ctx, "a marks transfer could not be confirmed and was flagged for review",
		"ledgerId", ledgerID, "reason", reason)
	// The gauge is NOT incremented here. It is published by the reconciler by
	// counting the ledger, so it survives a restart and cannot drift from what
	// is actually parked -- an in-memory counter would reset to zero on every
	// deploy and quietly clear the alert.
}

func (b *Bank) checkCooldown(ctx context.Context, agid string) error {
	if b.cfg.Bank.CooldownSeconds <= 0 {
		return nil
	}
	last, err := b.store.Queries().LastOperationAt(ctx, agid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("read last operation: %w", err)
	}
	if last.IsZero() {
		return nil
	}
	if time.Since(last) < b.cfg.Bank.Cooldown() {
		return ErrTooSoon
	}
	return nil
}

func (b *Bank) count(direction gen.BankDirection, result string) {
	if b.metrics != nil {
		b.metrics.BankOperationsTotal.WithLabelValues(string(direction), result).Inc()
	}
}

func nullable(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func strPtr(v string) *string { return &v }

// InGameMarks reads the marks on the player's current dinosaur.
//
// It returns pot.ErrPlayerNotOnline when they are logged out, which callers
// render as information rather than as a failure: not being in game is a normal
// state, not a fault.
func (b *Bank) InGameMarks(ctx context.Context, agid string) (int64, error) {
	player, err := b.rcon.PlayerInfo(ctx, agid)
	if err != nil {
		return 0, err
	}
	return player.Marks, nil
}

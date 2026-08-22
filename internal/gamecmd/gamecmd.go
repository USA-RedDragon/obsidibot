// Package gamecmd runs the in-game ! commands the PlayerCommand webhook
// delivers: !link, !deposit, !withdraw, !balance and !help, answered by RCON
// whisper.
//
// # Why a dispatcher rather than handling in the webhook request
//
// The ingest listener has ten-second read/write timeouts, while a banking
// command can take two or three RCON round trips of up to a minute each by
// configuration. So the route ACKs immediately and the work runs here on a
// tracked goroutine, the same shape internal/interactions uses for deferred
// slash commands: counted before it starts, panic-recovered, bounded by its
// own budget rather than the dead request's context.
//
// # The identity model
//
// The webhook's AlderonId IS the identity -- no link is required to bank in
// game, and balances merge when the player later links. The ledger snapshot
// carries the linked Discord id when one exists and "" otherwise; nothing
// reads it for logic.
//
// # Replies
//
// Every reply is a whisper: plain ASCII, at most 200 characters, because it is
// rendered on one in-game chat line and the game's font has no opinions worth
// discovering about anything fancier.
package gamecmd

import (
	"context"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/bank"
	"github.com/USA-RedDragon/obsidibot/internal/config"
	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/metrics"
	"github.com/USA-RedDragon/obsidibot/internal/pot"
)

// genericFailure is the whisper for a failure the player cannot act on. The
// reason is in the log; a database error is not something to render into game
// chat.
const genericFailure = "Something went wrong. This has been logged."

// maxWhisperLen bounds a reply. The game renders a whisper as one chat line,
// and anything past this is unreadable there anyway.
const maxWhisperLen = 200

// budgetSlack tops up the RCON share of a worker's budget. A banking command
// makes up to three RCON round trips plus the reply whisper, each bounded by
// rcon.Timeout(); the slack covers the database writes between them.
const budgetSlack = 15 * time.Second

// budgetRCONCalls is the most RCON exchanges one command makes: PlayerInfo,
// the mutating marks command, and the reply whisper, plus one spare for the
// library's own connection handling.
const budgetRCONCalls = 4

// Incoming is one PlayerCommand delivery, already validated by ingest: the
// AGID has passed pot.ValidIdentifier, because it is also the whisper target.
type Incoming struct {
	AGID       string
	PlayerName string
	Message    string
}

// Dispatcher parses, bounds and runs in-game commands.
type Dispatcher struct {
	store   *db.Store
	rcon    *pot.Client
	bank    *bank.Bank
	metrics *metrics.Metrics
	cfg     *config.Config

	// sem bounds in-flight commands at the RCON slot count, NOT above it. Every
	// command holds an RCON slot for most of its life, so a semaphore larger
	// than rcon.maxConcurrent would admit workers whose mutating bank command
	// is guaranteed rcon.ErrBusy under routine chat load -- failures this
	// dispatcher would be manufacturing itself. When full, the command is
	// dropped and counted "rejected"; the player can simply type it again.
	sem chan struct{}
	// workers counts the goroutines started by Dispatch. They outlive the
	// webhook request that started them -- that is the point -- so shutdown has
	// to wait on them before the database closes under a transfer.
	workers sync.WaitGroup
	budget  time.Duration
}

// New builds the dispatcher. vault is the same bank.Bank the slash commands
// use, so in-game transfers inherit the ledger's at-most-once guarantees.
func New(store *db.Store, rcon *pot.Client, vault *bank.Bank, m *metrics.Metrics, cfg *config.Config) *Dispatcher {
	return &Dispatcher{
		store:   store,
		rcon:    rcon,
		bank:    vault,
		metrics: m,
		cfg:     cfg,
		sem:     make(chan struct{}, cfg.RCON.MaxConcurrent),
		budget:  budgetRCONCalls*cfg.RCON.Timeout() + budgetSlack,
	}
}

// Budget is how long one dispatched command may run. Ingest derives its
// worker-wait grace from it, so shutdown never abandons a transfer this
// dispatcher would still have honoured.
func (d *Dispatcher) Budget() time.Duration {
	return d.budget
}

// Dispatch ACKs by returning immediately and runs the command in the
// background. It never blocks the webhook request beyond a channel send.
func (d *Dispatcher) Dispatch(ctx context.Context, in Incoming) {
	cmd := Parse(in.Message)

	select {
	case d.sem <- struct{}{}:
	default:
		// Full means every RCON slot is already spoken for. Queueing would only
		// delay the reply past the point of usefulness, so the honest move is to
		// drop, count it where an operator can see the rate, and let the player
		// retype.
		d.count(cmd.Name, metrics.ResultRejected)
		slog.WarnContext(ctx, "in-game command dropped: too many in flight", "command", cmd.Name)
		return
	}

	// WithoutCancel because the webhook request ends the moment ingest writes
	// its 200, and the work has only just started. The budget is this worker's
	// own deadline.
	workCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.budget)

	// Counted BEFORE the goroutine starts, so a shutdown beginning in the
	// window before it is scheduled still waits for it. See Wait.
	d.workers.Add(1)
	go func() {
		defer d.workers.Done()
		defer cancel()
		defer func() { <-d.sem }()
		// A panic in a bare goroutine takes the process down, not just the
		// command; one malformed message must not kill a replica that is
		// serving everything else correctly.
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			slog.ErrorContext(workCtx, "in-game command panicked",
				"command", cmd.Name, "panic", rec, "stack", string(debug.Stack()))
			d.count(cmd.Name, metrics.ResultError)
		}()
		d.run(workCtx, in, cmd)
	}()
}

// Wait blocks until every dispatched command has finished, or ctx expires.
// Separate from the ingest listener's own drain so the ordering is explicit at
// the call site: the listener stops accepting, the in-flight workers land, and
// only then may the database they write through be closed.
func (d *Dispatcher) Wait(ctx context.Context) error {
	done := make(chan struct{})
	// WaitGroup.Wait cannot be given a deadline, so it is watched from a
	// goroutine. On the timeout path that goroutine stays parked until the
	// workers eventually finish, which is fine: this is only called on the way
	// out.
	go func() {
		d.workers.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run executes one command and delivers its reply.
func (d *Dispatcher) run(ctx context.Context, in Incoming, cmd Command) {
	reply, result := d.execute(ctx, in, cmd)
	d.count(cmd.Name, result)
	if reply == "" {
		return
	}
	if err := d.rcon.Whisper(ctx, in.AGID, whisperable(reply)); err != nil {
		// The command itself may well have succeeded -- a deposit that moved
		// marks and then failed to whisper is not a lost deposit -- so this is
		// a delivery failure, logged as such.
		slog.ErrorContext(ctx, "could not whisper an in-game command reply",
			"command", cmd.Name, "error", err)
	}
}

func (d *Dispatcher) execute(ctx context.Context, in Incoming, cmd Command) (string, string) {
	// Every command records the player first: bank_accounts has a foreign key
	// to players, and an unlinked banker who has never appeared in a kill
	// event has no row yet. It also keeps last_seen_at fresh, which the decay
	// job reads.
	if err := d.store.Queries().UpsertPlayerSeen(ctx, gen.UpsertPlayerSeenParams{
		AlderonID:     in.AGID,
		LastKnownName: displayName(in),
		Rating:        float64(d.cfg.Rating.Initial),
	}); err != nil {
		slog.ErrorContext(ctx, "could not record player from an in-game command", "error", err)
		return genericFailure, metrics.ResultError
	}

	switch cmd.Name {
	case CommandLink:
		return d.link(ctx, in)
	case CommandDeposit:
		return d.move(ctx, in, cmd, gen.BankDirectionDeposit)
	case CommandWithdraw:
		return d.move(ctx, in, cmd, gen.BankDirectionWithdraw)
	case CommandBalance:
		return d.balance(ctx, in)
	case CommandHelp:
		return usage(cmd.Escape), metrics.ResultOK
	default:
		return usage(cmd.Escape), metrics.ResultUserError
	}
}

// usage echoes the escape char the player actually typed, because the right
// prefix is whatever their server is configured with, and they just proved
// what that is.
func usage(escape rune) string {
	e := string(escape)
	return "Commands: " + e + "link, " + e + "deposit [amount|all], " +
		e + "withdraw [amount|all], " + e + "balance, " + e + "help."
}

// displayName is the player row's name: the payload's name when it carries
// one, the AGID otherwise, so a never-seen player still gets a legible row.
func displayName(in Incoming) string {
	if name := strings.TrimSpace(in.PlayerName); name != "" {
		return name
	}
	return in.AGID
}

// whisperable makes a reply safe for one in-game chat line: ASCII only, dashes
// normalised, bounded. The shared error strings in internal/pot carry em
// dashes, which the game's chat font is not owed a chance to mangle.
func whisperable(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r < 128:
			return r
		case r == '—' || r == '–':
			return '-'
		default:
			return -1
		}
	}, s)
	if len(s) > maxWhisperLen {
		s = s[:maxWhisperLen]
	}
	return s
}

func (d *Dispatcher) count(command, result string) {
	if d.metrics != nil {
		d.metrics.GameCommandsTotal.WithLabelValues(command, result).Inc()
	}
}

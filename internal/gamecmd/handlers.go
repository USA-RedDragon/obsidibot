package gamecmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/bank"
	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/linkcode"
	"github.com/USA-RedDragon/obsidibot/internal/metrics"
	"github.com/USA-RedDragon/obsidibot/internal/numfmt"
	"github.com/USA-RedDragon/obsidibot/internal/pot"
	"github.com/jackc/pgx/v5"
)

// move is !deposit and !withdraw. It goes through the same bank.Bank as the
// slash commands, so in-game transfers inherit the ledger's at-most-once rule,
// the cooldown and the one-in-flight guard -- which, with no event id on the
// webhook, are also the only duplicate protection this path has.
func (d *Dispatcher) move(ctx context.Context, in Incoming, cmd Command,
	direction gen.BankDirection,
) (string, string) {
	amount, ok := ParseAmount(cmd.Args)
	if !ok {
		example := string(cmd.Escape) + cmd.Name
		return fmt.Sprintf("That amount is not a number. Use %s 500, %s all, or just %s for everything.",
			example, example, example), metrics.ResultUserError
	}

	// The ledger snapshot gets the linked Discord id when one exists and ""
	// otherwise. Empty-string is legal there and nothing reads it for logic;
	// the AGID is the identity either way.
	discordUserID := ""
	switch link, err := d.store.Queries().GetLinkByAlderonID(ctx, in.AGID); {
	case err == nil:
		discordUserID = link.DiscordUserID
	case !errors.Is(err, pgx.ErrNoRows):
		slog.ErrorContext(ctx, "could not look up link for an in-game transfer", "error", err)
		return genericFailure, metrics.ResultError
	}

	// Token "" because there is no Discord interaction to find the way back
	// to: the reply channel is the whisper below.
	var result bank.Result
	var err error
	if direction == gen.BankDirectionDeposit {
		result, err = d.bank.Deposit(ctx, discordUserID, in.AGID, "", amount)
	} else {
		result, err = d.bank.Withdraw(ctx, discordUserID, in.AGID, "", amount)
	}
	if err != nil {
		return d.explain(ctx, err, direction)
	}

	verb, preposition := "Deposited", "into your bank"
	if direction == gen.BankDirectionWithdraw {
		verb, preposition = "Withdrew", "onto your dinosaur"
	}
	reply := fmt.Sprintf("%s %s marks %s. Bank: %s. In game: %s.",
		verb, numfmt.Commas(result.Moved), preposition,
		numfmt.Commas(result.Balance), numfmt.Commas(result.InGame))
	if result.Clamped {
		reply += " That was everything available."
	}
	return reply, metrics.ResultOK
}

// explain turns a banking failure into a whisper the player can act on. The
// wording mirrors the Discord Banker's explain, so the two frontends describe
// the same failure the same way.
//
// The needs_review case is deliberately explicit rather than generic: the
// player's marks may be in an odd state, and telling them "something went
// wrong" while a moderator quietly fixes it is how a support problem becomes a
// trust problem.
func (d *Dispatcher) explain(ctx context.Context, err error, direction gen.BankDirection) (string, string) {
	switch {
	case errors.Is(err, bank.ErrBusy):
		return "You already have a transfer in progress. Give it a moment.", metrics.ResultUserError
	case errors.Is(err, bank.ErrTooSoon):
		return fmt.Sprintf("Too soon - wait %d seconds between transfers.",
			d.cfg.Bank.CooldownSeconds), metrics.ResultUserError
	case errors.Is(err, bank.ErrNothingToMove):
		if direction == gen.BankDirectionDeposit {
			return "Your dinosaur has no marks to deposit.", metrics.ResultUserError
		}
		return "You have no marks banked to withdraw.", metrics.ResultUserError
	case errors.Is(err, bank.ErrRaced):
		// Nothing was sent to the game, so this is the one failure that is
		// genuinely safe to retry, and saying so beats the generic failure.
		return "That did not go through - nothing was moved. Try again.", metrics.ResultUserError
	case errors.Is(err, bank.ErrNeedsReview):
		return "The game server did not confirm that transfer. Your balance is NOT changed and " +
			"a moderator will check it. Do not retry - check your balance first.", metrics.ResultNeedsReview
	case errors.Is(err, pot.ErrPlayerNotOnline), errors.Is(err, pot.ErrInvalidIdentifier):
		return pot.UserMessage(err), metrics.ResultUserError
	default:
		slog.ErrorContext(ctx, "in-game transfer failed", "error", err)
		return pot.UserMessage(err), metrics.ResultError
	}
}

// balance whispers the banked figure, and the in-game figure when it can be
// read. The bank half never depends on RCON: a player asking what they have
// banked deserves an answer even while the game server is not giving one.
func (d *Dispatcher) balance(ctx context.Context, in Incoming) (string, string) {
	q := d.store.Queries()
	if err := q.EnsureBankAccount(ctx, in.AGID); err != nil {
		slog.ErrorContext(ctx, "could not ensure bank account for an in-game balance", "error", err)
		return genericFailure, metrics.ResultError
	}
	account, err := q.GetBankAccount(ctx, in.AGID)
	if err != nil {
		slog.ErrorContext(ctx, "could not read balance for an in-game command", "error", err)
		return genericFailure, metrics.ResultError
	}

	reply := fmt.Sprintf("Bank: %s marks.", numfmt.Commas(account.Balance))
	// Best-effort: the in-game figure is a bonus, and its absence must not turn
	// a perfectly good bank answer into a failure.
	if marks, err := d.bank.InGameMarks(ctx, in.AGID); err == nil {
		reply = fmt.Sprintf("Bank: %s marks. On your current dinosaur: %s.",
			numfmt.Commas(account.Balance), numfmt.Commas(marks))
	} else if !errors.Is(err, pot.ErrPlayerNotOnline) {
		slog.WarnContext(ctx, "could not read in-game marks for an in-game balance", "error", err)
	}
	return reply, metrics.ResultOK
}

// link opens an UNCLAIMED challenge (discord_user_id null) and whispers the
// code: the person in game is the identity's authority, and the code is what
// they carry to /link confirm in Discord to claim it.
func (d *Dispatcher) link(ctx context.Context, in Incoming) (string, string) {
	q := d.store.Queries()

	switch _, err := q.GetLinkByAlderonID(ctx, in.AGID); {
	case err == nil:
		return "This character is already linked to a Discord account. " +
			"Use /link status or /link remove in Discord.", metrics.ResultUserError
	case !errors.Is(err, pgx.ErrNoRows):
		slog.ErrorContext(ctx, "could not look up link for !link", "error", err)
		return genericFailure, metrics.ResultError
	}

	// The reissue cooldown is keyed on the existing challenge row, whoever
	// opened it, so !link cannot be used to churn codes. PAST the cooldown the
	// upsert below replaces even a Discord-initiated challenge for this AGID
	// on purpose: whoever is in game holding the account is its authority.
	switch pending, err := q.GetChallengeByAlderonID(ctx, in.AGID); {
	case err == nil:
		if wait := time.Until(pending.CreatedAt.Add(d.cfg.Link.ReissueCooldown())); wait > 0 {
			return fmt.Sprintf("A code was already sent. Try again in %d seconds.",
				int(wait.Seconds())+1), metrics.ResultUserError
		}
	case !errors.Is(err, pgx.ErrNoRows):
		slog.ErrorContext(ctx, "could not look up challenge for !link", "error", err)
		return genericFailure, metrics.ResultError
	}

	code, err := linkcode.New()
	if err != nil {
		slog.ErrorContext(ctx, "could not generate link code for !link", "error", err)
		return genericFailure, metrics.ResultError
	}

	if err := q.UpsertChallenge(ctx, gen.UpsertChallengeParams{
		AlderonID: in.AGID,
		// nil = unclaimed: no Discord user owns this challenge yet, and
		// knowing the code IS the claim /link confirm accepts.
		DiscordUserID: nil,
		PlayerName:    displayName(in),
		// Only the hash is stored. The plaintext lives in the whisper below
		// and nowhere else, so database access cannot be turned into claiming
		// someone's identity.
		CodeHash:  linkcode.Hash(code),
		ExpiresAt: time.Now().Add(d.cfg.Link.CodeTTL()),
	}); err != nil {
		slog.ErrorContext(ctx, "could not store challenge for !link", "error", err)
		return genericFailure, metrics.ResultError
	}

	// "Do not share this code" is load-bearing: an unclaimed code is a bearer
	// token -- whoever enters it in Discord gets the link -- and streamers
	// broadcast their screens.
	return fmt.Sprintf("Your link code is %s. In Discord, run /link confirm with it. "+
		"Do not share this code. It expires in %d minutes.",
		code, int(d.cfg.Link.CodeTTL().Minutes())), metrics.ResultOK
}

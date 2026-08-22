// Package commands implements obsidibot's slash commands.
//
// Handlers here return a Reply and an error; they never write to the network.
// internal/interactions decides whether that reply is delivered immediately or
// as an edit to a deferred one, which keeps the three-second rule in one place
// instead of in every command.
package commands

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/config"
	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/interactions"
	"github.com/USA-RedDragon/obsidibot/internal/linkcode"
	"github.com/USA-RedDragon/obsidibot/internal/pot"
	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Linker implements /link.
type Linker struct {
	store *db.Store
	rcon  *pot.Client
	cfg   *config.Config
}

// NewLinker builds the /link handler.
func NewLinker(store *db.Store, rcon *pot.Client, cfg *config.Config) *Linker {
	return &Linker{store: store, rcon: rcon, cfg: cfg}
}

// Command returns the registration and routing for /link.
//
// Every subcommand is deferred: start and confirm both reach RCON or Postgres,
// and status and remove are grouped with them so one command never behaves two
// different ways. All of it is ephemeral -- a link code, and the fact of who
// plays as whom, belong to the caller.
func (l *Linker) Command() interactions.Command {
	return interactions.Command{
		Defer:     true,
		Ephemeral: true,
		Definition: &discordgo.ApplicationCommand{
			Name:        "link",
			Description: "Connect your Discord account to your in-game identity",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Name:        "start",
					Description: "Send a code to your character in game",
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Options: []*discordgo.ApplicationCommandOption{{
						Name:        "player",
						Description: "Your Alderon ID (like 555-000-101) or your in-game name",
						Type:        discordgo.ApplicationCommandOptionString,
						Required:    true,
					}},
				},
				{
					Name:        "confirm",
					Description: "Enter the code that was sent to you in game",
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Options: []*discordgo.ApplicationCommandOption{{
						Name:        "code",
						Description: "The code from your in-game message",
						Type:        discordgo.ApplicationCommandOptionString,
						Required:    true,
					}},
				},
				{
					Name:        "status",
					Description: "Show which in-game identity you are linked to",
					Type:        discordgo.ApplicationCommandOptionSubCommand,
				},
				{
					Name:        "remove",
					Description: "Disconnect your Discord account from your in-game identity",
					Type:        discordgo.ApplicationCommandOptionSubCommand,
				},
			},
		},
		Handler: l.handle,
	}
}

func (l *Linker) handle(ctx context.Context, ic interactions.Context) (interactions.Reply, error) {
	data := ic.Interaction.ApplicationCommandData()
	if len(data.Options) == 0 {
		return userError("Pick one of: start, confirm, status, remove."), nil
	}
	sub := data.Options[0]

	switch sub.Name {
	case "start":
		return l.start(ctx, ic.UserID, optionString(sub.Options, "player"))
	case "confirm":
		return l.confirm(ctx, ic.UserID, optionString(sub.Options, "code"))
	case "status":
		return l.status(ctx, ic.UserID)
	case "remove":
		return l.remove(ctx, ic.UserID)
	default:
		return userError("That is not something /link can do."), nil
	}
}

// start issues a challenge: it proves nothing by itself, it only sends a secret
// to whoever is holding that in-game account right now.
func (l *Linker) start(ctx context.Context, discordUserID, ident string) (interactions.Reply, error) {
	q := l.store.Queries()

	ident = strings.TrimSpace(ident)
	if !pot.ValidIdentifier(ident) {
		return userError("That does not look like an Alderon ID or a player name. " +
			"Use your Alderon ID, which looks like `555-000-101`."), nil
	}

	// Already linked? Say so rather than silently reissuing: someone running
	// /link start when they are already linked has usually forgotten.
	switch existing, err := q.GetLinkByDiscordID(ctx, discordUserID); {
	case err == nil:
		return userError(fmt.Sprintf(
			"Your Discord account is already linked to `%s`. Use `/link remove` first if you want to change it.",
			existing.AlderonID)), nil
	case !errors.Is(err, pgx.ErrNoRows):
		return interactions.Reply{}, fmt.Errorf("look up existing link: %w", err)
	}

	// Bound how often one user can cause a message to appear in the game. A
	// challenge is a whisper to another person's screen, so an uncapped
	// /link start is a spam button pointed at whoever they name.
	switch pending, err := q.GetChallengeByDiscordID(ctx, &discordUserID); {
	case err == nil:
		if wait := time.Until(pending.CreatedAt.Add(l.cfg.Link.ReissueCooldown())); wait > 0 {
			return userError(fmt.Sprintf("You just requested a code. Try again in %d seconds.",
				int(wait.Seconds())+1)), nil
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return interactions.Reply{}, fmt.Errorf("look up pending challenge: %w", err)
	}

	// The player must be online: the code is delivered into the game, and the
	// lookup is also what turns a typed name into a canonical Alderon ID.
	player, err := l.rcon.PlayerInfo(ctx, ident)
	if err != nil {
		return rconReply(err, "look up player for link"), nil
	}

	switch linked, err := q.GetLinkByAlderonID(ctx, player.AGID); {
	case err == nil:
		if linked.DiscordUserID == discordUserID {
			// Unreachable given the check above, but the constraint is the
			// authority and this keeps the two readings consistent.
			return userError("You are already linked to that identity."), nil
		}
		return userError(fmt.Sprintf(
			"`%s` is already linked to another Discord account. "+
				"If that is you, use `/link remove` from that account first.", player.AGID)), nil
	case !errors.Is(err, pgx.ErrNoRows):
		return interactions.Reply{}, fmt.Errorf("look up identity link: %w", err)
	}

	// Opportunistic tidy-up, so an abandoned challenge does not hold an
	// identity hostage until the sweeper runs.
	if _, err := q.DeleteExpiredChallenges(ctx); err != nil {
		slog.WarnContext(ctx, "could not clear expired link challenges", "error", err)
	}

	// One live challenge per identity, whoever started it: otherwise a user
	// can wipe someone else's pending challenge, or whisper them repeatedly by
	// naming their account over and over. This guard SURVIVES the re-keying of
	// the table on purpose -- with the upsert's conflict target now on
	// alderon_id, dropping it would turn the upsert itself into the stomp.
	switch live, err := q.GetLiveChallengeByAlderonID(ctx, player.AGID); {
	case err == nil && live.DiscordUserID == nil:
		// An in-game !link is pending for this identity. Whoever typed it is
		// holding the account, so point the caller at claiming that code
		// rather than replacing it.
		return userError("A code was already sent to that character in game. " +
			"Use `/link confirm` with it."), nil
	case err == nil && *live.DiscordUserID != discordUserID:
		return userError("Somebody is already trying to link that identity. Try again in a few minutes."), nil
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return interactions.Reply{}, fmt.Errorf("look up live challenge: %w", err)
	}

	code, err := linkcode.New()
	if err != nil {
		return interactions.Reply{}, fmt.Errorf("generate link code: %w", err)
	}

	// Delete-then-upsert in ONE transaction: the caller may hold a pending
	// challenge against a DIFFERENT identity, and with the conflict target on
	// alderon_id, the unique(discord_user_id) constraint would refuse the
	// upsert unless that older challenge is cleared first. Two statements
	// outside a transaction would leave a window with neither.
	err = l.store.InTx(ctx, func(q *gen.Queries) error {
		if _, err := q.DeleteChallengeByDiscordID(ctx, &discordUserID); err != nil {
			return fmt.Errorf("clear previous challenge: %w", err)
		}
		if err := q.UpsertChallenge(ctx, gen.UpsertChallengeParams{
			AlderonID:     player.AGID,
			DiscordUserID: &discordUserID,
			PlayerName:    player.Name,
			// Only the hash is stored. The plaintext lives in the message the
			// game delivers and nowhere else, so database access cannot be
			// turned into claiming someone's in-game identity.
			CodeHash:  linkcode.Hash(code),
			ExpiresAt: time.Now().Add(l.cfg.Link.CodeTTL()),
		}); err != nil {
			return fmt.Errorf("store link challenge: %w", err)
		}
		return nil
	})
	if err != nil {
		return interactions.Reply{}, err
	}

	if err := l.deliver(ctx, player.AGID, code); err != nil {
		// The challenge row is left in place: it expires on its own, and
		// deleting it here would let a delivery failure and a successful
		// delivery look the same to the next /link start.
		return rconReply(err, "deliver link code"), nil
	}

	return interactions.Reply{Content: fmt.Sprintf(
		"Sent a code in game to **%s** (`%s`). Check your in-game chat, then run `/link confirm` with it.\n"+
			"It expires in %d minutes.",
		player.Name, player.AGID, int(l.cfg.Link.CodeTTL().Minutes()))}, nil
}

// deliver sends the code into the game.
//
// The code NEVER goes to Discord: that it arrives only in game, on the client
// logged into that identity, is the entire proof of ownership.
func (l *Linker) deliver(ctx context.Context, agid, code string) error {
	return l.rcon.Whisper(ctx, agid,
		fmt.Sprintf("obsidibot link code: %s - enter it with /link confirm in Discord.", code))
}

// confirm checks the code and, if it matches, makes the link.
//
// A code can have arrived two ways: (A) the caller ran /link start, so the
// challenge row carries their Discord id; (B) somebody typed !link in game, so
// the row is unclaimed (discord_user_id null) and knowing the code IS the
// claim -- the whisper only ever appeared on the screen of whoever controls
// that identity.
func (l *Linker) confirm(ctx context.Context, discordUserID, code string) (interactions.Reply, error) {
	q := l.store.Queries()

	// Normalised the same way the code was generated, so a player who typed it
	// in lower case, or with the spaces a chat line might wrap in, still
	// succeeds.
	hash := linkcode.Hash(linkcode.Normalise(code))

	// Path A: the caller's own Discord-initiated challenge.
	var own *gen.LinkChallenge
	var ownExpired bool
	switch row, err := q.GetChallengeByDiscordID(ctx, &discordUserID); {
	case err == nil:
		if time.Now().After(row.ExpiresAt) {
			if derr := q.DeleteChallengeByAlderonID(ctx, row.AlderonID); derr != nil {
				slog.WarnContext(ctx, "could not delete expired challenge", "error", derr)
			}
			ownExpired = true
		} else {
			own = &row
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return interactions.Reply{}, fmt.Errorf("load link challenge: %w", err)
	}
	if own != nil && subtle.ConstantTimeCompare(hash, own.CodeHash) == 1 {
		return l.complete(ctx, discordUserID, *own)
	}

	// Path B: unclaimed in-game-initiated challenges -- and it runs BEFORE any
	// attempt is burned on path A. A caller holding a Discord-initiated
	// challenge on one identity who then typed !link on another would
	// otherwise burn attempts on the wrong row every time, while /link start
	// refuses them a reset because a challenge is pending: a deadlock.
	unclaimed, err := q.ListUnclaimedLiveChallenges(ctx)
	if err != nil {
		return interactions.Reply{}, fmt.Errorf("scan unclaimed challenges: %w", err)
	}
	// Every row is compared even after a hit: an early exit would let response
	// timing hint at where in the list a guessed code sits. And no attempts
	// are burned on unclaimed rows in any outcome -- a wrong guess must not
	// destroy other players' pending in-game links, and brute force is
	// impractical against 30^6 codes inside a five-minute TTL behind Discord's
	// rate limits.
	var matched *gen.LinkChallenge
	for i := range unclaimed {
		if subtle.ConstantTimeCompare(hash, unclaimed[i].CodeHash) == 1 && matched == nil {
			matched = &unclaimed[i]
		}
	}
	if matched != nil {
		return l.complete(ctx, discordUserID, *matched)
	}

	// Both paths missed. Only now does the caller's own challenge pay for the
	// failure.
	if own != nil {
		attempts, err := q.IncrementChallengeAttempts(ctx, own.AlderonID)
		if err != nil {
			return interactions.Reply{}, fmt.Errorf("record failed attempt: %w", err)
		}
		remaining := int32(l.cfg.Link.MaxAttempts) - attempts //nolint:gosec // bounded by config validation
		if remaining <= 0 {
			if err := q.DeleteChallengeByAlderonID(ctx, own.AlderonID); err != nil {
				slog.WarnContext(ctx, "could not burn exhausted challenge", "error", err)
			}
			return userError("Too many wrong codes. Run `/link start` to get a new one."), nil
		}
		return userError(fmt.Sprintf("That code is not right. %d attempt(s) left.", remaining)), nil
	}
	if ownExpired {
		return userError("That code has expired. Run `/link start` again, or type `!link` in game."), nil
	}
	return userError("That code does not match any link in progress. " +
		"Start one with `/link start`, or type `!link` in game."), nil
}

// complete finishes a link in one transaction: the player row, the link, the
// bank account and the spent challenge are one fact. A crash partway through
// must not leave a link with no account behind it.
func (l *Linker) complete(ctx context.Context, discordUserID string, challenge gen.LinkChallenge) (interactions.Reply, error) {
	err := l.store.InTx(ctx, func(q *gen.Queries) error {
		if err := q.UpsertPlayerSeen(ctx, gen.UpsertPlayerSeenParams{
			AlderonID:     challenge.AlderonID,
			LastKnownName: challenge.PlayerName,
			Rating:        float64(l.cfg.Rating.Initial),
		}); err != nil {
			return fmt.Errorf("record player: %w", err)
		}
		if err := q.CreateLink(ctx, gen.CreateLinkParams{
			DiscordUserID: discordUserID,
			AlderonID:     challenge.AlderonID,
		}); err != nil {
			return fmt.Errorf("create link: %w", err)
		}
		if err := q.EnsureBankAccount(ctx, challenge.AlderonID); err != nil {
			return fmt.Errorf("create bank account: %w", err)
		}
		if err := q.DeleteChallengeByAlderonID(ctx, challenge.AlderonID); err != nil {
			return fmt.Errorf("clear challenge: %w", err)
		}
		return nil
	})
	// Two racers can claim the same code, and an already-linked caller can
	// claim a fresh one; either way CreateLink's unique constraints refuse the
	// transaction. The loser deserves the truth, not the generic failure.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return userError("That code was just used, or you are already linked. Check `/link status`."), nil
	}
	if err != nil {
		return interactions.Reply{}, err
	}

	slog.InfoContext(ctx, "linked a discord account to an in-game identity",
		"alderonId", challenge.AlderonID)

	return interactions.Reply{Content: fmt.Sprintf(
		"Linked to **%s** (`%s`). Your stats and marks bank are now connected to this Discord account.",
		challenge.PlayerName, challenge.AlderonID)}, nil
}

func (l *Linker) status(ctx context.Context, discordUserID string) (interactions.Reply, error) {
	link, err := l.store.Queries().GetLinkByDiscordID(ctx, discordUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return interactions.Reply{Content: "You are not linked yet. Run `/link start` while you are in game."}, nil
	}
	if err != nil {
		return interactions.Reply{}, fmt.Errorf("look up link: %w", err)
	}

	name := link.AlderonID
	if player, err := l.store.Queries().GetPlayer(ctx, link.AlderonID); err == nil {
		name = player.LastKnownName
	}
	return interactions.Reply{Content: fmt.Sprintf("You are linked to **%s** (`%s`), since <t:%d:D>.",
		name, link.AlderonID, link.LinkedAt.Unix())}, nil
}

// remove breaks the link and nothing else. Stats and the bank balance hang off
// the Alderon ID, so they survive and reattach if the player links again.
func (l *Linker) remove(ctx context.Context, discordUserID string) (interactions.Reply, error) {
	rows, err := l.store.Queries().DeleteLinkByDiscordID(ctx, discordUserID)
	if err != nil {
		return interactions.Reply{}, fmt.Errorf("remove link: %w", err)
	}
	if rows == 0 {
		return userError("You were not linked to anything."), nil
	}
	return interactions.Reply{Content: "Unlinked. Your stats and banked marks are kept, " +
		"and come back if you link the same identity again."}, nil
}

// optionString reads a string option, returning "" when absent.
func optionString(options []*discordgo.ApplicationCommandInteractionDataOption, name string) string {
	for _, opt := range options {
		if opt.Name == name {
			return opt.StringValue()
		}
	}
	return ""
}

// userError marks a refusal the caller can act on, so it is counted as a user
// error rather than a system fault.
func userError(content string) interactions.Reply {
	return interactions.Reply{Content: content, UserError: true}
}

// rconReply turns an RCON failure into something worth showing the player,
// logging the real error. A player who is simply logged out is a user error,
// not an incident.
func rconReply(err error, what string) interactions.Reply {
	if errors.Is(err, pot.ErrPlayerNotOnline) || errors.Is(err, pot.ErrInvalidIdentifier) {
		return userError(pot.UserMessage(err))
	}
	slog.Error("rcon call failed", "operation", what, "error", err)
	return interactions.Reply{Content: pot.UserMessage(err)}
}

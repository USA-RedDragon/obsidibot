// Package commands implements obsidibot's slash commands.
//
// Handlers here return a Reply and an error; they never write to the network.
// internal/interactions decides whether that reply is delivered immediately or
// as an edit to a deferred one, which keeps the three-second rule in one place
// instead of in every command.
package commands

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	"github.com/USA-RedDragon/obsidibot/internal/pot"
	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5"
)

// codeAlphabet omits 0/O/1/I/L and every vowel-adjacent lookalike, because the
// player reads this off an in-game chat line and types it into Discord. A code
// that is unambiguous to read is worth more here than one extra bit of entropy.
const codeAlphabet = "23456789ABCDEFGHJKMNPQRSTVWXYZ"

// codeLength gives 30^6 ~ 7.3e8 codes. Against five attempts inside a
// five-minute window that is not guessable, and it stays short enough to retype
// without resentment.
const codeLength = 6

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
	switch pending, err := q.GetChallenge(ctx, discordUserID); {
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

	// One live challenge per identity, whoever started it: otherwise a user can
	// wipe someone else's pending challenge, or whisper them repeatedly by
	// naming their account over and over.
	switch live, err := q.GetLiveChallengeByAlderonID(ctx, player.AGID); {
	case err == nil && live.DiscordUserID != discordUserID:
		return userError("Somebody is already trying to link that identity. Try again in a few minutes."), nil
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return interactions.Reply{}, fmt.Errorf("look up live challenge: %w", err)
	}

	code, err := newCode()
	if err != nil {
		return interactions.Reply{}, fmt.Errorf("generate link code: %w", err)
	}
	sum := sha256.Sum256([]byte(code))

	if err := q.UpsertChallenge(ctx, gen.UpsertChallengeParams{
		DiscordUserID: discordUserID,
		AlderonID:     player.AGID,
		PlayerName:    player.Name,
		// Only the hash is stored. The plaintext lives in the message the game
		// delivers and nowhere else, so database access cannot be turned into
		// claiming someone's in-game identity.
		CodeHash:  sum[:],
		ExpiresAt: time.Now().Add(l.cfg.Link.CodeTTL()),
	}); err != nil {
		return interactions.Reply{}, fmt.Errorf("store link challenge: %w", err)
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
func (l *Linker) confirm(ctx context.Context, discordUserID, code string) (interactions.Reply, error) {
	q := l.store.Queries()

	challenge, err := q.GetChallenge(ctx, discordUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return userError("You have no link in progress. Start one with `/link start`."), nil
	}
	if err != nil {
		return interactions.Reply{}, fmt.Errorf("load link challenge: %w", err)
	}

	if time.Now().After(challenge.ExpiresAt) {
		if err := q.DeleteChallenge(ctx, discordUserID); err != nil {
			slog.WarnContext(ctx, "could not delete expired challenge", "error", err)
		}
		return userError("That code has expired. Run `/link start` again."), nil
	}

	// Normalised the same way the code was generated, so a player who typed it
	// in lower case, or with the spaces a chat line might wrap in, still
	// succeeds. Case folding here is not a weakening: the alphabet has no
	// lower-case members, so nothing collides.
	sum := sha256.Sum256([]byte(normaliseCode(code)))
	if subtle.ConstantTimeCompare(sum[:], challenge.CodeHash) != 1 {
		attempts, err := q.IncrementChallengeAttempts(ctx, discordUserID)
		if err != nil {
			return interactions.Reply{}, fmt.Errorf("record failed attempt: %w", err)
		}
		remaining := int32(l.cfg.Link.MaxAttempts) - attempts //nolint:gosec // bounded by config validation
		if remaining <= 0 {
			if err := q.DeleteChallenge(ctx, discordUserID); err != nil {
				slog.WarnContext(ctx, "could not burn exhausted challenge", "error", err)
			}
			return userError("Too many wrong codes. Run `/link start` to get a new one."), nil
		}
		return userError(fmt.Sprintf("That code is not right. %d attempt(s) left.", remaining)), nil
	}

	// One transaction: the player row, the link, the bank account and the
	// spent challenge are one fact. A crash partway through must not leave a
	// link with no account behind it.
	err = l.store.InTx(ctx, func(q *gen.Queries) error {
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
		if err := q.DeleteChallenge(ctx, discordUserID); err != nil {
			return fmt.Errorf("clear challenge: %w", err)
		}
		return nil
	})
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

// newCode returns a fresh link code.
//
// The modulo is unbiased because len(codeAlphabet) is 30 and the source is a
// uniform byte reduced by rejection rather than by truncation.
func newCode() (string, error) {
	alphabet := []byte(codeAlphabet)
	out := make([]byte, 0, codeLength)
	// 256 is not a multiple of 30, so bytes at or above the largest multiple
	// are discarded; taking them would make the first six letters likelier.
	limit := 256 - (256 % len(alphabet))
	buf := make([]byte, 1)
	for len(out) < codeLength {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("read random bytes: %w", err)
		}
		if int(buf[0]) >= limit {
			continue
		}
		out = append(out, alphabet[int(buf[0])%len(alphabet)])
	}
	return string(out), nil
}

// normaliseCode forgives the ways a code gets mangled between an in-game chat
// line and a Discord text box.
func normaliseCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
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

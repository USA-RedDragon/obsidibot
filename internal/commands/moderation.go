package commands

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/SRS-Hosting/rcon"
	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/discordfmt"
	"github.com/USA-RedDragon/obsidibot/internal/interactions"
	"github.com/USA-RedDragon/obsidibot/internal/metrics"
	"github.com/USA-RedDragon/obsidibot/internal/moderation"
	"github.com/USA-RedDragon/obsidibot/internal/pot"
	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// maxReasonLen bounds a reason at the Discord option level, which refuses an
// over-long one before it is ever typed into a command.
//
// Discord's default is 6000, which would blow the 1024-character embed field
// limit in the feeds and /modstats -- and push the RCON line past
// rcon.MaxCommandLen, where the ban becomes permanently unenforceable. The pot
// wrappers truncate as well; this is the half a person can see.
const maxReasonLen = 400

// recentShown is how much history /modstats prints. Enough to see a pattern,
// short enough to stay inside one embed.
const recentShown = 5

const colourModStats = 0x9B59B6

// modGateRefusal is what a non-moderator is told. It names both routes in,
// because "you cannot do that" without saying who can is a support ticket.
const modGateRefusal = "You need the moderator role, or the Manage Server permission, to use that. " +
	"A server admin can set the role with `/config mod-role`."

// The enforcement sentences shown in the /ban reply and in the ban feed. What
// the database recorded and what the GAME was told are different facts, and
// only saying the first is how a moderator ends up believing a player is
// blocked when they are not.
const (
	enforcedNow = "Kicked and banned in game."
	enforcedNot = "Recorded, but the game server did not confirm the ban. " +
		"obsidibot will keep trying automatically."
	enforcedUnlinked = "Recorded, but it cannot be enforced in game yet: " +
		"this Discord account is not linked to an in-game identity. " +
		"It will be enforced automatically as soon as they link."
	enforcedAdminTarget = "The game refuses to ban server admins. " +
		"Remove them from `ServerAdmins` in Game.ini and restart the server, then lift and re-issue this ban."
	enforcedTooLong = "The ban command was too long for the game server. " +
		"Lift this ban and re-issue it with a shorter reason."
	enforcedRaced = "The ban was lifted while it was being placed, so nothing is in force."
)

// agidRE recognises an Alderon ID typed into the player option.
//
// It is local rather than in internal/pot: pot.IsAGID was deleted as dead code
// in increment 1, and the shape matters HERE -- it is what decides whether a
// player: argument is used as-is or looked up in game.
//
//nolint:gochecknoglobals // compiled once and never reassigned
var agidRE = regexp.MustCompile(`^\d{3}-\d{3}-\d{3}$`)

// Moderation implements /warn, /ban, /unban and /modstats.
type Moderation struct {
	store   *db.Store
	rcon    *pot.Client
	feed    *moderation.Feed
	metrics *metrics.Metrics
}

// NewModeration builds the moderation commands. poster is the Discord session,
// used for the ban and warn feeds.
func NewModeration(store *db.Store, rconClient *pot.Client, poster moderation.Poster,
	m *metrics.Metrics,
) *Moderation {
	return &Moderation{store: store, rcon: rconClient, feed: moderation.NewFeed(store, poster), metrics: m}
}

// Commands returns all four moderation commands.
//
// Every one of them is deferred and ephemeral: the gate reads guild_config,
// warns and bans reach RCON, and none of it belongs in a public channel -- the
// feeds are where a moderation action is announced, deliberately, to a channel
// somebody chose.
func (mod *Moderation) Commands() []interactions.Command {
	return []interactions.Command{
		{
			Defer: true, Ephemeral: true,
			Definition: &discordgo.ApplicationCommand{
				Name:        "warn",
				Description: "Record a warning against a player",
				// Discord requires every required option before the optional
				// ones, which is why the reason comes first.
				Options: append([]*discordgo.ApplicationCommandOption{
					reasonOption("Why they are being warned (they are shown this in game)", true),
				}, targetOptions()...),
			},
			Handler: mod.warn,
		},
		{
			Defer: true, Ephemeral: true,
			Definition: &discordgo.ApplicationCommand{
				Name:        "ban",
				Description: "Ban a player from the game server",
				Options: append(append([]*discordgo.ApplicationCommandOption{
					reasonOption("Why they are being banned (they are shown this)", true),
				}, targetOptions()...), &discordgo.ApplicationCommandOption{
					Name:        "duration",
					Description: "How long, like 1d3h30m. Leave it out for a permanent ban",
					Type:        discordgo.ApplicationCommandOptionString,
					MaxLength:   32,
				}),
			},
			Handler: mod.ban,
		},
		{
			Defer: true, Ephemeral: true,
			Definition: &discordgo.ApplicationCommand{
				Name:        "unban",
				Description: "Lift a player's ban",
				Options: append(targetOptions(),
					reasonOption("Why the ban is being lifted", false)),
			},
			Handler: mod.unban,
		},
		{
			Defer: true, Ephemeral: true,
			Definition: &discordgo.ApplicationCommand{
				Name:        "modstats",
				Description: "Show a player's warnings and bans",
				Options:     targetOptions(),
			},
			Handler: mod.modstats,
		},
	}
}

// targetOptions are the two ways to name somebody. Exactly one is required, and
// Discord cannot express that, so the handler does.
func targetOptions() []*discordgo.ApplicationCommandOption {
	return []*discordgo.ApplicationCommandOption{
		{
			Name:        "user",
			Description: "The Discord account to act on",
			Type:        discordgo.ApplicationCommandOptionUser,
		},
		{
			Name:        "player",
			Description: "The Alderon ID (like 555-000-101) or in-game name to act on",
			Type:        discordgo.ApplicationCommandOptionString,
			MaxLength:   32,
		},
	}
}

func reasonOption(description string, required bool) *discordgo.ApplicationCommandOption {
	return &discordgo.ApplicationCommandOption{
		Name:        "reason",
		Description: description,
		Type:        discordgo.ApplicationCommandOptionString,
		Required:    required,
		MaxLength:   maxReasonLen,
	}
}

// requireMod decides whether the caller may moderate.
//
// Both inputs come from the Ed25519-signed interaction payload -- Discord
// resolves the permission bits and the member's roles itself and signs them --
// so this needs no API call and cannot be forged.
//
// The router's RequiresManageGuild cannot express any of this: the role lives
// in the database. /config keeps RequiresManageGuild for exactly that reason,
// so a mod-role holder cannot move the gate that lets them in.
func (mod *Moderation) requireMod(ctx context.Context, ic interactions.Context) (bool, error) {
	member := ic.Interaction.Member
	if member == nil || ic.GuildID == "" {
		// A DM has no guild, no roles and no permissions to read.
		return false, nil
	}
	// Manage Server is the bootstrap: somebody has to be able to moderate
	// before a mod role has been configured.
	if member.Permissions&discordgo.PermissionManageGuild != 0 {
		return true, nil
	}

	cfg, err := mod.store.Queries().GetGuildConfig(ctx, ic.GuildID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read guild config: %w", err)
	}
	if cfg.ModRoleID == nil || *cfg.ModRoleID == "" {
		return false, nil
	}
	return slices.Contains(member.Roles, *cfg.ModRoleID), nil
}

// modTarget is one person, known by however many identifiers could be resolved.
//
// BOTH are carried everywhere, because a player warned by Alderon ID before
// linking and by @user afterwards is one person with one record; every count
// and lookup matches on either.
type modTarget struct {
	agid      *string
	discordID *string
	name      *string
}

// label names the target for a reply.
func (t modTarget) label() string {
	switch {
	case t.name != nil && *t.name != "":
		return fmt.Sprintf("**%s**", discordfmt.EscapeMarkdown(*t.name))
	case t.agid != nil:
		return fmt.Sprintf("`%s`", discordfmt.EscapeMarkdown(*t.agid))
	case t.discordID != nil:
		return fmt.Sprintf("<@%s>", *t.discordID)
	default:
		return "that player"
	}
}

// resolveTarget turns the two options into identifiers. The second return is a
// refusal to show the caller; it is empty when the target resolved.
func (mod *Moderation) resolveTarget(ctx context.Context,
	opts []*discordgo.ApplicationCommandInteractionDataOption,
) (modTarget, string, error) {
	user := optionUser(opts, "user")
	player := strings.TrimSpace(optionString(opts, "player"))

	switch {
	case user != "" && player != "":
		return modTarget{}, "Name the player one way or the other: `user:` for a Discord account, " +
			"`player:` for an Alderon ID or in-game name.", nil
	case user == "" && player == "":
		return modTarget{}, "Say who: `user:` for a Discord account, or `player:` for an Alderon ID " +
			"(like `555-000-101`) or in-game name.", nil
	case user != "":
		return mod.resolveDiscordUser(ctx, user)
	default:
		return mod.resolvePlayer(ctx, player)
	}
}

func (mod *Moderation) resolveDiscordUser(ctx context.Context, discordID string) (modTarget, string, error) {
	target := modTarget{discordID: &discordID}

	link, err := mod.store.Queries().GetLinkByDiscordID(ctx, discordID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Not linked. That is not a refusal: the action is still recorded
		// against the Discord account, and the scheduler attaches the in-game
		// identity the moment they link.
		return target, "", nil
	case err != nil:
		return modTarget{}, "", fmt.Errorf("look up link for target: %w", err)
	}

	target.agid = &link.AlderonID
	mod.attachName(ctx, &target)
	return target, "", nil
}

func (mod *Moderation) resolvePlayer(ctx context.Context, player string) (modTarget, string, error) {
	q := mod.store.Queries()

	agid := player
	var name *string
	if !agidRE.MatchString(player) {
		// A NAME, not an identity. Names go stale and are reused, so it is
		// canonicalised through the game exactly as /link start does -- which
		// means the player has to be online for it to work.
		if !pot.ValidIdentifier(player) {
			return modTarget{}, "That does not look like an Alderon ID or a player name. " +
				"Alderon IDs look like `555-000-101`.", nil
		}
		found, err := mod.rcon.PlayerInfo(ctx, player)
		if errors.Is(err, pot.ErrPlayerNotOnline) {
			return modTarget{}, fmt.Sprintf(
				"**%s** is not in game right now, so I cannot tell which account that name belongs to. "+
					"Use their Alderon ID (like `555-000-101`) instead.", discordfmt.EscapeMarkdown(player)), nil
		}
		if err != nil {
			return modTarget{}, "", fmt.Errorf("look up player by name: %w", err)
		}
		agid, name = found.AGID, &found.Name
	}

	target := modTarget{agid: &agid, name: name}
	switch link, err := q.GetLinkByAlderonID(ctx, agid); {
	case err == nil:
		target.discordID = &link.DiscordUserID
	case !errors.Is(err, pgx.ErrNoRows):
		return modTarget{}, "", fmt.Errorf("look up link for identity: %w", err)
	}
	if target.name == nil {
		mod.attachName(ctx, &target)
	}
	return target, "", nil
}

// attachName records the display name obsidibot last saw, so the record and the
// feed still name the person after they rename or unlink.
func (mod *Moderation) attachName(ctx context.Context, target *modTarget) {
	if target.agid == nil {
		return
	}
	player, err := mod.store.Queries().GetPlayer(ctx, *target.agid)
	if err != nil {
		return
	}
	target.name = &player.LastKnownName
}

func (mod *Moderation) warn(ctx context.Context, ic interactions.Context) (interactions.Reply, error) {
	allowed, err := mod.requireMod(ctx, ic)
	if err != nil {
		return interactions.Reply{}, err
	}
	if !allowed {
		return userError(modGateRefusal), nil
	}

	opts := ic.Interaction.ApplicationCommandData().Options
	reason := strings.TrimSpace(optionString(opts, "reason"))
	if reason == "" {
		return userError("Give a reason. The player is shown it, and it stays on their record."), nil
	}
	target, refusal, err := mod.resolveTarget(ctx, opts)
	if err != nil {
		return interactions.Reply{}, err
	}
	if refusal != "" {
		return userError(refusal), nil
	}

	q := mod.store.Queries()
	warn, err := q.InsertWarn(ctx, gen.InsertWarnParams{
		AlderonID: target.agid, DiscordUserID: target.discordID, TargetName: target.name,
		Reason: reason, IssuedByDiscordID: ic.UserID,
	})
	if err != nil {
		return interactions.Reply{}, fmt.Errorf("record warning: %w", err)
	}
	count, err := q.CountWarns(ctx, gen.CountWarnsParams{
		AlderonID: target.agid, DiscordUserID: target.discordID,
	})
	if err != nil {
		return interactions.Reply{}, fmt.Errorf("count warnings: %w", err)
	}

	// Best-effort: a warning the player did not see is still a warning on the
	// record, and it is usually delivered in Discord as well.
	delivered := false
	if target.agid != nil {
		if err := mod.rcon.Whisper(ctx, *target.agid,
			"You have been warned by a moderator: "+reason); err != nil {
			slog.DebugContext(ctx, "could not deliver a warning in game", "error", err)
		} else {
			delivered = true
		}
	}

	mod.feed.Post(ctx, ic.GuildID, moderation.KindWarn, moderation.WarnEmbed(warn, count))
	mod.count(moderation.ActionWarn, metrics.ResultOK)

	note := " They were not in game, so they have not seen it yet."
	if delivered {
		note = " They were told in game."
	}
	return interactions.Reply{Content: fmt.Sprintf(
		"Warned %s. That is warning #%d for this player.%s", target.label(), count, note)}, nil
}

func (mod *Moderation) ban(ctx context.Context, ic interactions.Context) (interactions.Reply, error) {
	allowed, err := mod.requireMod(ctx, ic)
	if err != nil {
		return interactions.Reply{}, err
	}
	if !allowed {
		return userError(modGateRefusal), nil
	}

	opts := ic.Interaction.ApplicationCommandData().Options
	reason := strings.TrimSpace(optionString(opts, "reason"))
	if reason == "" {
		return userError("Give a reason. The player is shown it when they are removed."), nil
	}

	// An absent duration is a PERMANENT ban, which is why this is a pointer:
	// "no expiry" and "expires at the zero time" must not be the same value.
	var expiresAt *time.Time
	if raw := strings.TrimSpace(optionString(opts, "duration")); raw != "" {
		duration, err := moderation.ParseDuration(raw)
		if err != nil {
			// The parser's errors are written as sentences for this reply.
			return userError(err.Error()), nil
		}
		when := time.Now().Add(duration)
		expiresAt = &when
	}

	target, refusal, err := mod.resolveTarget(ctx, opts)
	if err != nil {
		return interactions.Reply{}, err
	}
	if refusal != "" {
		return userError(refusal), nil
	}

	q := mod.store.Queries()
	active, err := q.GetActiveBans(ctx, gen.GetActiveBansParams{
		AlderonID: target.agid, DiscordUserID: target.discordID,
	})
	if err != nil {
		return interactions.Reply{}, fmt.Errorf("look up active bans: %w", err)
	}
	if len(active) > 0 {
		mod.count(moderation.ActionBan, metrics.ResultDuplicate)
		return userError(mod.alreadyBanned(target, active[0])), nil
	}

	ban, err := q.InsertBan(ctx, gen.InsertBanParams{
		AlderonID: target.agid, DiscordUserID: target.discordID, TargetName: target.name,
		Reason: reason, IssuedByDiscordID: ic.UserID, ExpiresAt: expiresAt,
	})
	// The partial unique indexes are the real authority on "one active ban per
	// identity", and two moderators can reach them at once. The loser is told
	// the same thing the check above would have said.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		mod.count(moderation.ActionBan, metrics.ResultDuplicate)
		return userError(fmt.Sprintf("%s is already banned.", target.label())), nil
	}
	if err != nil {
		return interactions.Reply{}, fmt.Errorf("record ban: %w", err)
	}

	enforcement := mod.enforce(ctx, ban)

	count, err := q.CountBans(ctx, gen.CountBansParams{
		AlderonID: target.agid, DiscordUserID: target.discordID,
	})
	if err != nil {
		return interactions.Reply{}, fmt.Errorf("count bans: %w", err)
	}

	mod.feed.Post(ctx, ic.GuildID, moderation.KindBan, moderation.BanEmbed(ban, count, enforcement))

	return interactions.Reply{Content: fmt.Sprintf("Banned %s %s.\n%s\nThat is ban #%d for this player.",
		target.label(), banWindow(expiresAt), enforcement, count)}, nil
}

// enforce puts a freshly recorded ban into the game and reports what happened
// in the moderator's own terms.
func (mod *Moderation) enforce(ctx context.Context, ban gen.Ban) string {
	if ban.AlderonID == nil {
		// Nothing to enforce against yet. The row stays unenforced and the
		// scheduler's backfill picks it up when the player links.
		mod.count(moderation.ActionBan, metrics.ResultOK)
		return enforcedUnlinked
	}
	agid := *ban.AlderonID

	// The kick is what shows them the reason on screen; ErrKickFailed only
	// means they were offline, and a ban works on offline identities.
	if err := mod.rcon.Kick(ctx, agid, moderation.KickReason(ban)); err != nil &&
		!errors.Is(err, pot.ErrKickFailed) {
		slog.WarnContext(ctx, "could not kick a player while banning them", "banId", ban.ID, "error", err)
	}

	adminReason, userReason := moderation.BanReasons(ban)
	err := mod.rcon.Ban(ctx, agid, adminReason, userReason)
	switch {
	case err == nil, errors.Is(err, pot.ErrAlreadyBanned):
		marked, markErr := mod.store.Queries().MarkBanEnforced(ctx, ban.ID)
		if markErr != nil {
			slog.ErrorContext(ctx, "could not record a ban as enforced", "banId", ban.ID, "error", markErr)
			mod.count(moderation.ActionBan, metrics.ResultError)
			return enforcedNot
		}
		if marked == 0 {
			// Somebody lifted it in the moment between the insert and now.
			// Their decision is newer, so the game ban comes straight back off.
			if unbanErr := mod.rcon.Unban(ctx, agid); unbanErr != nil &&
				!errors.Is(unbanErr, pot.ErrNotBanned) {
				slog.ErrorContext(ctx, "could not undo a game ban placed on a lifted record",
					"banId", ban.ID, "error", unbanErr)
			}
			return enforcedRaced
		}
		mod.count(moderation.ActionBan, metrics.ResultOK)
		return enforcedNow

	case errors.Is(err, pot.ErrCannotBanAdmin):
		mod.markUnenforceable(ctx, ban, "the game refuses to ban a server admin "+
			"(remove them from ServerAdmins in Game.ini first)")
		return enforcedAdminTarget

	case errors.Is(err, rcon.ErrCommandTooLong):
		mod.markUnenforceable(ctx, ban, "the ban command was too long for the game server")
		return enforcedTooLong

	default:
		// Transient. The row stays unenforced and the scheduler retries it.
		mod.count(moderation.ActionBan, metrics.ResultError)
		slog.ErrorContext(ctx, "could not enforce a ban in game", "banId", ban.ID, "error", err)
		return enforcedNot
	}
}

func (mod *Moderation) markUnenforceable(ctx context.Context, ban gen.Ban, reason string) {
	if err := mod.store.Queries().MarkBanUnenforceable(ctx, gen.MarkBanUnenforceableParams{
		ID: ban.ID, UnenforceableReason: &reason,
	}); err != nil {
		slog.ErrorContext(ctx, "could not flag a ban as unenforceable", "banId", ban.ID, "error", err)
	}
	mod.count(moderation.ActionBan, metrics.ResultRejected)
}

func (mod *Moderation) alreadyBanned(target modTarget, ban gen.Ban) string {
	line := fmt.Sprintf("%s is already banned (%s). Use `/unban` first if you want to change it.",
		target.label(), moderation.FormatExpiry(ban.ExpiresAt))
	if ban.UnenforceableReason != nil && *ban.UnenforceableReason != "" {
		line += "\nThat ban is NOT in force in game: " + discordfmt.EscapeMarkdown(*ban.UnenforceableReason) + "."
	}
	return line
}

func (mod *Moderation) unban(ctx context.Context, ic interactions.Context) (interactions.Reply, error) {
	allowed, err := mod.requireMod(ctx, ic)
	if err != nil {
		return interactions.Reply{}, err
	}
	if !allowed {
		return userError(modGateRefusal), nil
	}

	opts := ic.Interaction.ApplicationCommandData().Options
	target, refusal, err := mod.resolveTarget(ctx, opts)
	if err != nil {
		return interactions.Reply{}, err
	}
	if refusal != "" {
		return userError(refusal), nil
	}

	q := mod.store.Queries()
	active, err := q.GetActiveBans(ctx, gen.GetActiveBansParams{
		AlderonID: target.agid, DiscordUserID: target.discordID,
	})
	if err != nil {
		return interactions.Reply{}, fmt.Errorf("look up active bans: %w", err)
	}
	if len(active) == 0 {
		return userError(fmt.Sprintf("%s has no ban on record.", target.label())), nil
	}

	liftReason := "lifted by a moderator"
	if reason := strings.TrimSpace(optionString(opts, "reason")); reason != "" {
		liftReason += " - " + reason
	}

	// There can legitimately be two rows: one recorded against the Discord
	// account before the player linked, one against the Alderon ID.
	var notes []string
	lifted := 0
	for _, ban := range active {
		note, ok, err := mod.liftOne(ctx, ban, liftReason)
		if err != nil {
			return interactions.Reply{}, err
		}
		if ok {
			lifted++
			mod.feed.Post(ctx, ic.GuildID, moderation.KindBan,
				moderation.UnbanEmbed(ban, ic.UserID, note))
		}
		notes = append(notes, note)
	}

	if lifted == 0 {
		mod.count(moderation.ActionUnban, metrics.ResultError)
		return interactions.Reply{Content: fmt.Sprintf(
			"Could not lift the ban on %s: %s\nThe ban is still in force; try again in a moment.",
			target.label(), strings.Join(notes, " "))}, nil
	}
	mod.count(moderation.ActionUnban, metrics.ResultOK)
	return interactions.Reply{Content: fmt.Sprintf("Unbanned %s. %s",
		target.label(), strings.Join(notes, " "))}, nil
}

// liftOne closes one ban row, reporting what happened and whether it worked.
//
// FAIL CLOSED: the row is marked lifted only after the game says the ban is
// gone. A record saying "not banned" over a game that still refuses the player
// is a ban nobody can see or remove.
func (mod *Moderation) liftOne(ctx context.Context, ban gen.Ban, liftReason string) (string, bool, error) {
	q := mod.store.Queries()

	if ban.EnforcedAt == nil {
		// Nothing in the game to undo -- unless the scheduler enforced it
		// between the read above and now, which is exactly what this guarded
		// update detects.
		switch rows, err := q.LiftUnenforcedBan(ctx,
			gen.LiftUnenforcedBanParams{ID: ban.ID, LiftReason: &liftReason}); {
		case err != nil:
			return "", false, fmt.Errorf("lift unenforced ban: %w", err)
		case rows > 0:
			return "It had not reached the game server yet, so nothing had to be undone.", true, nil
		}
		// Zero rows: it was enforced (or already lifted) in the meantime. Fall
		// through to the RCON path with fresh information.
		fresh, err := q.GetActiveBans(ctx, gen.GetActiveBansParams{
			AlderonID: ban.AlderonID, DiscordUserID: ban.DiscordUserID,
		})
		if err != nil {
			return "", false, fmt.Errorf("re-read ban: %w", err)
		}
		found := false
		for _, row := range fresh {
			if row.ID == ban.ID {
				ban, found = row, true
				break
			}
		}
		if !found {
			return "It had already been lifted.", true, nil
		}
	}

	if ban.AlderonID == nil {
		// Enforced without an identity is impossible, but a lift that dereferences
		// nil in front of a moderator would be worse than a redundant branch.
		rows, err := q.LiftBan(ctx, gen.LiftBanParams{ID: ban.ID, LiftReason: &liftReason})
		if err != nil {
			return "", false, fmt.Errorf("lift ban: %w", err)
		}
		return "Record closed.", rows > 0, nil
	}

	note := "They can rejoin now."
	switch err := mod.rcon.Unban(ctx, *ban.AlderonID); {
	case err == nil:
	case errors.Is(err, pot.ErrNotBanned):
		// Ambiguous by design: either the ban was already gone, or the target
		// is a listed server admin whose bans.txt row RCON cannot touch.
		note = "They were not banned in game; the record is closed."
	default:
		slog.ErrorContext(ctx, "could not lift a ban in game", "banId", ban.ID, "error", err)
		return "the game server did not confirm the unban.", false, nil
	}

	rows, err := q.LiftBan(ctx, gen.LiftBanParams{ID: ban.ID, LiftReason: &liftReason})
	if err != nil {
		return "", false, fmt.Errorf("lift ban: %w", err)
	}
	if rows == 0 {
		return "It had already been lifted.", true, nil
	}
	return note, true, nil
}

func (mod *Moderation) modstats(ctx context.Context, ic interactions.Context) (interactions.Reply, error) {
	allowed, err := mod.requireMod(ctx, ic)
	if err != nil {
		return interactions.Reply{}, err
	}
	if !allowed {
		return userError(modGateRefusal), nil
	}

	target, refusal, err := mod.resolveTarget(ctx, ic.Interaction.ApplicationCommandData().Options)
	if err != nil {
		return interactions.Reply{}, err
	}
	if refusal != "" {
		return userError(refusal), nil
	}

	q := mod.store.Queries()
	warnCount, err := q.CountWarns(ctx, gen.CountWarnsParams{
		AlderonID: target.agid, DiscordUserID: target.discordID,
	})
	if err != nil {
		return interactions.Reply{}, fmt.Errorf("count warnings: %w", err)
	}
	banCount, err := q.CountBans(ctx, gen.CountBansParams{
		AlderonID: target.agid, DiscordUserID: target.discordID,
	})
	if err != nil {
		return interactions.Reply{}, fmt.Errorf("count bans: %w", err)
	}
	active, err := q.GetActiveBans(ctx, gen.GetActiveBansParams{
		AlderonID: target.agid, DiscordUserID: target.discordID,
	})
	if err != nil {
		return interactions.Reply{}, fmt.Errorf("look up active bans: %w", err)
	}
	recentWarns, err := q.ListRecentWarns(ctx, gen.ListRecentWarnsParams{
		Limit: recentShown, AlderonID: target.agid, DiscordUserID: target.discordID,
	})
	if err != nil {
		return interactions.Reply{}, fmt.Errorf("read recent warnings: %w", err)
	}
	recentBans, err := q.ListRecentBans(ctx, gen.ListRecentBansParams{
		Limit: recentShown, AlderonID: target.agid, DiscordUserID: target.discordID,
	})
	if err != nil {
		return interactions.Reply{}, fmt.Errorf("read recent bans: %w", err)
	}

	embed := &discordgo.MessageEmbed{
		Title: "Moderation record",
		Color: colourModStats,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Player", Value: target.label()},
			{Name: "Warnings", Value: fmt.Sprintf("%d", warnCount), Inline: true},
			{Name: "Bans", Value: fmt.Sprintf("%d", banCount), Inline: true},
			{Name: "Currently banned", Value: activeBanSummary(active)},
		},
	}
	if len(recentWarns) > 0 {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name: "Recent warnings", Value: warnLines(recentWarns),
		})
	}
	if len(recentBans) > 0 {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name: "Recent bans", Value: banLines(recentBans),
		})
	}
	return interactions.Reply{Embeds: []*discordgo.MessageEmbed{embed}}, nil
}

// activeBanSummary says whether the player is banned AND whether the game knows
// about it, which are separate facts and only one of them keeps anybody out.
func activeBanSummary(active []gen.Ban) string {
	if len(active) == 0 {
		return "No."
	}
	lines := make([]string, 0, len(active))
	for _, ban := range active {
		state := "in force in game"
		switch {
		case ban.UnenforceableReason != nil && *ban.UnenforceableReason != "":
			state = "NOT in force: " + discordfmt.EscapeMarkdown(*ban.UnenforceableReason)
		case ban.EnforcedAt == nil:
			state = "not in force yet; waiting to be applied"
		}
		lines = append(lines, fmt.Sprintf("Yes, %s — %s (%s)",
			moderation.FormatExpiry(ban.ExpiresAt), discordfmt.EscapeMarkdown(ban.Reason), state))
	}
	return strings.Join(lines, "\n")
}

func warnLines(warns []gen.Warn) string {
	lines := make([]string, 0, len(warns))
	for _, warn := range warns {
		lines = append(lines, fmt.Sprintf("<t:%d:d> %s — by <@%s>",
			warn.CreatedAt.Unix(), discordfmt.EscapeMarkdown(warn.Reason), warn.IssuedByDiscordID))
	}
	return strings.Join(lines, "\n")
}

func banLines(bans []gen.Ban) string {
	lines := make([]string, 0, len(bans))
	for _, ban := range bans {
		line := fmt.Sprintf("<t:%d:d> %s — by <@%s>",
			ban.CreatedAt.Unix(), discordfmt.EscapeMarkdown(ban.Reason), ban.IssuedByDiscordID)
		if ban.LiftedAt != nil {
			line += " (lifted"
			if ban.LiftReason != nil && *ban.LiftReason != "" {
				line += ": " + discordfmt.EscapeMarkdown(*ban.LiftReason)
			}
			line += ")"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// banWindow phrases the duration for the reply.
func banWindow(expiresAt *time.Time) string {
	if expiresAt == nil {
		return "permanently"
	}
	return fmt.Sprintf("until <t:%d:f>", expiresAt.Unix())
}

func (mod *Moderation) count(kind, result string) {
	if mod.metrics == nil {
		return
	}
	mod.metrics.ModerationActionsTotal.WithLabelValues(kind, result).Inc()
}

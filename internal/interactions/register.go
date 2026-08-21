package interactions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// Register makes the bot's commands the guild's complete command set.
//
// It is a BULK OVERWRITE rather than a series of creates, so a command removed
// from this binary disappears from Discord instead of lingering as a
// registration that routes to nothing. That is also why it is safe to run on
// every start: the operation is idempotent by construction.
//
// Registration is guild-scoped. Guild commands apply immediately where global
// ones take about an hour to propagate, and obsidibot serves one guild, so
// there is nothing to gain from the slower path.
//
// Callers run this under a leader lock. Bulk overwrite is idempotent, so
// several replicas doing it at once would be correct but wasteful, and it
// counts against Discord's daily command-creation limits.
func Register(ctx context.Context, session *discordgo.Session, appID, guildID string, commands []*discordgo.ApplicationCommand) error {
	registered, err := session.ApplicationCommandBulkOverwrite(
		appID, guildID, commands, discordgo.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("register %d commands in guild %s: %w", len(commands), guildID, err)
	}

	names := make([]string, 0, len(registered))
	for _, cmd := range registered {
		names = append(names, cmd.Name)
	}
	slog.InfoContext(ctx, "registered slash commands", "guild", guildID, "commands", names)
	return nil
}

// VerifyToken confirms the bot token works and reports who it belongs to.
//
// A bad token must fail at startup. Left to be discovered later it shows up as
// every deferred reply failing to deliver -- the ACK still goes out, so users
// see a command that thinks forever rather than one that is broken.
func VerifyToken(ctx context.Context, session *discordgo.Session) (*discordgo.User, error) {
	me, err := session.User("@me", discordgo.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("verify discord token: %w", err)
	}
	return me, nil
}

// ErrNoGuilds means the bot has not been invited anywhere yet.
var ErrNoGuilds = errors.New("interactions: the bot is not in any guild; invite it first")

// DiscoverGuild resolves the single guild this bot serves.
//
// obsidibot is a one-guild bot, so the guild it is in IS the guild it serves
// and there is no setting for it. Being told which guild to use would only be
// meaningful if there were a choice to make.
//
// More than one guild is an ERROR rather than a guess, and there is no setting
// to break the tie: picking one arbitrarily would register commands into a
// server at random and post a kill feed there. Keep the application non-public
// so nobody else can invite it, and remove it from any guild it should not be
// in.
func DiscoverGuild(ctx context.Context, session *discordgo.Session) (*discordgo.UserGuild, error) {
	// Two is enough to detect ambiguity, and asking for more would page
	// through guilds nobody expects to exist.
	guilds, err := session.UserGuilds(2, "", "", false, discordgo.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("list the bot's guilds: %w", err)
	}

	switch len(guilds) {
	case 0:
		return nil, ErrNoGuilds
	case 1:
		return guilds[0], nil
	default:
		names := make([]string, 0, len(guilds))
		for _, guild := range guilds {
			names = append(names, fmt.Sprintf("%s (%s)", guild.Name, guild.ID))
		}
		return nil, fmt.Errorf(
			"the bot is in %d or more guilds (%s); obsidibot serves exactly one, "+
				"so remove it from the others and make the application non-public",
			len(guilds), strings.Join(names, ", "))
	}
}

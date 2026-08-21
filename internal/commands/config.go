package commands

import (
	"context"
	"errors"
	"fmt"

	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/interactions"
	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5"
)

// Config implements /config, the moderator-facing settings.
//
// These channels live in the DATABASE and never in obsidibot's config file: a
// moderator has to be able to move the kill feed without a redeploy, which was
// an explicit requirement.
type Config struct {
	store *db.Store
}

// NewConfig builds the /config handler.
func NewConfig(store *db.Store) *Config {
	return &Config{store: store}
}

// channelOption restricts the picker to channels a bot can post in, so an
// obviously wrong choice is impossible rather than merely reported later.
func channelOption(description string) *discordgo.ApplicationCommandOption {
	return &discordgo.ApplicationCommandOption{
		Name:         "channel",
		Description:  description,
		Type:         discordgo.ApplicationCommandOptionChannel,
		Required:     true,
		ChannelTypes: []discordgo.ChannelType{discordgo.ChannelTypeGuildText},
	}
}

// Command returns the registration and routing for /config.
func (c *Config) Command() interactions.Command {
	return interactions.Command{
		// Gated on the caller's Discord permissions, read from the signed
		// interaction payload. Discord resolves and signs them, so they are as
		// trustworthy as the request itself.
		RequiresManageGuild: true,
		Ephemeral:           true,
		Definition: &discordgo.ApplicationCommand{
			Name:        "config",
			Description: "Configure obsidibot for this server",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Name:        "kill-channel",
					Description: "Choose where the kill feed is posted",
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Options:     []*discordgo.ApplicationCommandOption{channelOption("Channel for the kill feed")},
				},
				{
					Name:        "leaderboard-channel",
					Description: "Choose where the leaderboard message lives",
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Options:     []*discordgo.ApplicationCommandOption{channelOption("Channel for the leaderboard")},
				},
				{
					Name:        "show",
					Description: "Show the current settings",
					Type:        discordgo.ApplicationCommandOptionSubCommand,
				},
			},
		},
		Handler: c.handle,
	}
}

func (c *Config) handle(ctx context.Context, ic interactions.Context) (interactions.Reply, error) {
	if ic.GuildID == "" {
		return userError("That command only works inside a server."), nil
	}

	data := ic.Interaction.ApplicationCommandData()
	if len(data.Options) == 0 {
		return userError("Pick one of: kill-channel, leaderboard-channel, show."), nil
	}
	sub := data.Options[0]
	channelID := optionChannel(sub.Options, "channel")

	q := c.store.Queries()
	switch sub.Name {
	case "kill-channel":
		if err := q.SetKillFeedChannel(ctx, gen.SetKillFeedChannelParams{
			GuildID: ic.GuildID, KillFeedChannelID: &channelID,
		}); err != nil {
			return interactions.Reply{}, fmt.Errorf("set kill feed channel: %w", err)
		}
		return interactions.Reply{Content: fmt.Sprintf("Kill feed will post in <#%s>.", channelID)}, nil

	case "leaderboard-channel":
		// This also clears the remembered message id, because the stored one
		// names a message in the channel nobody is reading any more.
		if err := q.SetLeaderboardChannel(ctx, gen.SetLeaderboardChannelParams{
			GuildID: ic.GuildID, LeaderboardChannelID: &channelID,
		}); err != nil {
			return interactions.Reply{}, fmt.Errorf("set leaderboard channel: %w", err)
		}
		return interactions.Reply{Content: fmt.Sprintf(
			"Leaderboard will live in <#%s>. A fresh message will be posted within a minute.", channelID)}, nil

	case "show":
		return c.show(ctx, ic.GuildID)

	default:
		return userError("That is not something /config can do."), nil
	}
}

func (c *Config) show(ctx context.Context, guildID string) (interactions.Reply, error) {
	cfg, err := c.store.Queries().GetGuildConfig(ctx, guildID)
	if errors.Is(err, pgx.ErrNoRows) {
		// No row is the normal state before anything has been configured, so
		// it is reported as "unset" rather than as a failure.
		return interactions.Reply{Content: "Nothing is configured yet.\n" +
			"Kill feed: *unset*\nLeaderboard: *unset*"}, nil
	}
	if err != nil {
		return interactions.Reply{}, fmt.Errorf("read guild config: %w", err)
	}
	return interactions.Reply{Content: fmt.Sprintf("Kill feed: %s\nLeaderboard: %s",
		channelMention(cfg.KillFeedChannelID), channelMention(cfg.LeaderboardChannelID))}, nil
}

func channelMention(id *string) string {
	if id == nil || *id == "" {
		return "*unset*"
	}
	return "<#" + *id + ">"
}

// optionChannel reads a channel option, returning "" when absent.
func optionChannel(options []*discordgo.ApplicationCommandInteractionDataOption, name string) string {
	for _, opt := range options {
		if opt.Name == name && opt.Type == discordgo.ApplicationCommandOptionChannel {
			return opt.Value.(string) //nolint:forcetypeassert // Discord sends a snowflake string for a channel option
		}
	}
	return ""
}

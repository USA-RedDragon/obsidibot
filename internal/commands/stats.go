package commands

import (
	"context"
	"errors"
	"fmt"

	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/discordfmt"
	"github.com/USA-RedDragon/obsidibot/internal/interactions"
	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5"
)

const colourStats = 0x3498DB

// Stats implements /stats.
type Stats struct {
	store *db.Store
}

// NewStats builds the /stats handler.
func NewStats(store *db.Store) *Stats {
	return &Stats{store: store}
}

// Command returns the registration and routing for /stats.
//
// It is PUBLIC and not deferred: it is a single indexed read, so it comfortably
// answers inside Discord's three-second budget, and a public reply is what
// makes the feature advertise itself.
func (s *Stats) Command() interactions.Command {
	return interactions.Command{
		Definition: &discordgo.ApplicationCommand{
			Name:        "stats",
			Description: "Show a player's rating and record",
			Options: []*discordgo.ApplicationCommandOption{{
				Name:        "user",
				Description: "Whose stats to show (default: yours)",
				Type:        discordgo.ApplicationCommandOptionUser,
				Required:    false,
			}},
		},
		Handler: s.handle,
	}
}

func (s *Stats) handle(ctx context.Context, ic interactions.Context) (interactions.Reply, error) {
	target := ic.UserID
	self := true
	if chosen := optionUser(ic.Interaction.ApplicationCommandData().Options, "user"); chosen != "" {
		target = chosen
		self = chosen == ic.UserID
	}

	player, err := s.store.Queries().GetPlayerByDiscordID(ctx, target)
	if errors.Is(err, pgx.ErrNoRows) {
		if self {
			return userError("You have not linked an in-game identity yet. " +
				"Run `/link start` while you are in game."), nil
		}
		return userError(fmt.Sprintf("<@%s> has not linked an in-game identity.", target)), nil
	}
	if err != nil {
		return interactions.Reply{}, fmt.Errorf("look up player stats: %w", err)
	}

	embed := &discordgo.MessageEmbed{
		Title: discordfmt.EscapeMarkdown(player.LastKnownName),
		Color: colourStats,
		Fields: []*discordgo.MessageEmbedField{
			// Rating, not a leaderboard position: the ordering lives on the
			// board, and a rank in a reply invites a comparison the caller did
			// not ask for.
			{Name: "Rating", Value: fmt.Sprintf("%.0f", player.Rating), Inline: true},
			{Name: "K/D", Value: discordfmt.KD(player.Kills, player.Deaths), Inline: true},
			{Name: "Kills", Value: fmt.Sprintf("%d", player.Kills), Inline: true},
			{Name: "Deaths", Value: fmt.Sprintf("%d", player.Deaths), Inline: true},
			// A Discord timestamp so it renders in the reader's own timezone
			// and stays correct as time passes.
			{Name: "Last seen", Value: fmt.Sprintf("<t:%d:R>", player.LastSeenAt.Unix()), Inline: true},
		},
		Footer: &discordgo.MessageEmbedFooter{Text: player.AlderonID},
	}

	// NOTHING about where the player is, or was, appears here. That is the
	// standing rule, and /stats is the most tempting place to break it.
	return interactions.Reply{Embeds: []*discordgo.MessageEmbed{embed}}, nil
}

// optionUser reads a user option, returning "" when absent.
func optionUser(options []*discordgo.ApplicationCommandInteractionDataOption, name string) string {
	for _, opt := range options {
		if opt.Name == name && opt.Type == discordgo.ApplicationCommandOptionUser {
			return opt.Value.(string) //nolint:forcetypeassert // Discord sends a snowflake string for a user option
		}
	}
	return ""
}

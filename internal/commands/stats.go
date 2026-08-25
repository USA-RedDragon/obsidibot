package commands

import (
	"context"
	"errors"
	"fmt"

	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
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
// It is not deferred: two indexed reads answer comfortably inside Discord's
// three-second budget, and deferring would add a visible flicker for nothing.
//
// It is EPHEMERAL, which reverses the original decision to make it public. The
// history names whoever killed you, and a public message naming people is a
// message that eventually pings somebody who did not ask to be pinged.
// Mentions are also rendered with AllowedMentions cleared by the router, so
// the two protections are independent.
func (s *Stats) Command() interactions.Command {
	return interactions.Command{
		Ephemeral: true,
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

// Component returns the routing for the history buttons. The router matches on
// the prefix, so one registration covers every page of every player.
func (s *Stats) Component() interactions.Component {
	return interactions.Component{Prefix: historyPrefix, Handler: s.history}
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

	// The rating above is a claim; this is the evidence for it. A number
	// nobody can take apart is a number players argue with, so every event
	// that moved it is listed -- kills AND deaths, because a player asking why
	// they dropped forty points is asking about the deaths.
	components, err := s.attachHistory(ctx, embed, player.AlderonID)
	if err != nil {
		return interactions.Reply{}, err
	}

	// NOTHING about where the player is, or was, appears here. That is the
	// standing rule, and /stats is the most tempting place to break it.
	return interactions.Reply{
		Embeds:     []*discordgo.MessageEmbed{embed},
		Components: components,
	}, nil
}

// attachHistory adds the recent-events field and returns the button that opens
// the full list.
//
// The anchor is taken here, once, and every page the button leads to is filtered
// by it -- see historyPage.
func (s *Stats) attachHistory(ctx context.Context, embed *discordgo.MessageEmbed,
	agid string,
) ([]discordgo.MessageComponent, error) {
	q := s.store.Queries()
	anchor, err := q.MaxEventID(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the newest event id: %w", err)
	}

	rows, err := q.PlayerHistory(ctx, gen.PlayerHistoryParams{
		Agid: &agid, MaxID: anchor, PageSize: summaryHistory, PageOffset: 0,
	})
	if err != nil {
		return nil, fmt.Errorf("read player history: %w", err)
	}
	if len(rows) == 0 {
		// No block at all rather than an empty one: a heading over nothing
		// reads as a fault.
		return nil, nil
	}

	total, err := q.CountPlayerHistory(ctx, gen.CountPlayerHistoryParams{Agid: &agid, MaxID: anchor})
	if err != nil {
		return nil, fmt.Errorf("count player history: %w", err)
	}

	embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
		Name:   "Recent",
		Value:  renderHistory(rows, agid),
		Inline: false,
	})
	return viewMoreButton(historyPage{agid: agid, anchor: anchor}, total), nil
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

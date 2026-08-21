// Package board maintains the persistent leaderboard message.
//
// One message, edited in place on a timer, rather than a new post each time: a
// channel whose only content is the current standings reads as a scoreboard,
// where a stream of replaced posts reads as spam.
//
// The message id is remembered in guild_config. If it is missing -- first run,
// a moderator moved the channel, somebody deleted the message -- a new one is
// posted and its id stored. That recovery path is not an edge case; somebody
// will delete it.
package board

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/config"
	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/discordfmt"
	"github.com/USA-RedDragon/obsidibot/internal/metrics"
	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5"
)

// Messenger is the Discord surface the board needs. *discordgo.Session
// satisfies it.
type Messenger interface {
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend,
		options ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessageEditComplex(edit *discordgo.MessageEdit,
		options ...discordgo.RequestOption) (*discordgo.Message, error)
}

const colourBoard = 0x2ECC71

// Board renders and refreshes the leaderboard.
type Board struct {
	store     *db.Store
	messenger Messenger
	metrics   *metrics.Metrics
	cfg       *config.Config
	guildID   string
}

// New builds the leaderboard worker. guildID is the guild discovered at
// startup, which is where the board's channel setting lives.
func New(store *db.Store, messenger Messenger, m *metrics.Metrics, cfg *config.Config, guildID string) *Board {
	return &Board{store: store, messenger: messenger, metrics: m, cfg: cfg, guildID: guildID}
}

// Run refreshes on a fixed tick until ctx is cancelled.
//
// A fixed tick rather than an edit per kill: the feed and the board share the
// channel rate limit, and editing the board on every kill during a fight would
// starve the feed of the budget it needs to stay live.
func (b *Board) Run(ctx context.Context) error {
	ticker := time.NewTicker(b.cfg.Leaderboard.Interval())
	defer ticker.Stop()

	// Refresh once immediately, so a restart does not leave a stale board up
	// for a whole interval.
	if err := b.refresh(ctx); err != nil && ctx.Err() == nil {
		slog.ErrorContext(ctx, "leaderboard refresh failed", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := b.refresh(ctx); err != nil {
				//nolint:nilerr // deliberate: cancellation is a clean stop
				if ctx.Err() != nil {
					return nil
				}
				// Never fatal: the board being briefly stale is not worth
				// giving up leadership over, and the last-success gauge makes
				// a persistent failure visible.
				slog.ErrorContext(ctx, "leaderboard refresh failed", "error", err)
			}
		}
	}
}

// refresh renders the current standings and puts them on screen.
func (b *Board) refresh(ctx context.Context) error {
	q := b.store.Queries()

	guild, err := q.GetGuildConfig(ctx, b.guildID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // nothing configured yet
	}
	if err != nil {
		return fmt.Errorf("read guild config: %w", err)
	}
	if guild.LeaderboardChannelID == nil || *guild.LeaderboardChannelID == "" {
		return nil
	}
	channelID := *guild.LeaderboardChannelID

	rows, err := q.TopPlayers(ctx, int32(b.cfg.Leaderboard.Size)) //nolint:gosec // bounded by config validation
	if err != nil {
		return fmt.Errorf("read leaderboard: %w", err)
	}
	embed := b.render(rows)

	if guild.LeaderboardMessageID != nil && *guild.LeaderboardMessageID != "" {
		_, err := b.messenger.ChannelMessageEditComplex(&discordgo.MessageEdit{
			Channel:         channelID,
			ID:              *guild.LeaderboardMessageID,
			Embeds:          &[]*discordgo.MessageEmbed{embed},
			AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
		}, discordgo.WithContext(ctx))
		if err == nil {
			b.markSuccess()
			return nil
		}
		// Somebody deleted the message, or it moved. Falling through to post a
		// fresh one is the whole recovery path; without it the board silently
		// stops updating forever.
		slog.InfoContext(ctx, "leaderboard message could not be edited, posting a new one", "error", err)
	}

	message, err := b.messenger.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Embeds:          []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	}, discordgo.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("post leaderboard: %w", err)
	}
	if err := q.SetLeaderboardMessage(ctx, gen.SetLeaderboardMessageParams{
		GuildID: b.guildID, LeaderboardMessageID: &message.ID,
	}); err != nil {
		return fmt.Errorf("remember leaderboard message: %w", err)
	}
	b.markSuccess()
	return nil
}

func (b *Board) markSuccess() {
	if b.metrics != nil {
		b.metrics.LeaderboardLastSuccess.Set(float64(time.Now().Unix()))
	}
}

// render builds the standings embed.
//
// The ordering is by rating, and THE RATING IS NOT SHOWN AS A SORT KEY people
// are asked to reason about -- each row shows kills, deaths and K/D, which is
// what players actually compare. No in-game position appears anywhere.
func (b *Board) render(rows []gen.TopPlayersRow) *discordgo.MessageEmbed {
	embed := &discordgo.MessageEmbed{
		Title:     "Obsidian Wilds — Top Players",
		Color:     colourBoard,
		Timestamp: time.Now().Format(time.RFC3339),
		Footer: &discordgo.MessageEmbedFooter{
			Text: "Ranked by rating. Beating stronger players is worth more; " +
				"farming weaker ones is worth almost nothing.",
		},
	}

	if len(rows) == 0 {
		embed.Description = "No kills recorded yet."
		return embed
	}

	var body strings.Builder
	for i, row := range rows {
		// A linked player renders as a mention so their Discord identity is
		// visible; allowed_mentions keeps it from pinging them every minute.
		// An unlinked one renders as their in-game name, which is what makes
		// this a ranking of the SERVER rather than of the subset who use the
		// bot.
		name := discordfmt.EscapeMarkdown(row.LastKnownName)
		if row.DiscordUserID != nil {
			name = "<@" + *row.DiscordUserID + ">"
		}

		fmt.Fprintf(&body, "`#%2d` **%.0f** · %s\n%s· %d kills · %d deaths · %s K/D\n",
			i+1, row.Rating, name, indent, row.Kills, row.Deaths, discordfmt.KD(row.Kills, row.Deaths))
	}
	embed.Description = body.String()
	return embed
}

// indent lines the second line of each entry up under the first.
const indent = "  "

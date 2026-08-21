package kills

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/config"
	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/discordfmt"
	"github.com/USA-RedDragon/obsidibot/internal/metrics"
	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5"
)

// Poster sends a message to a channel. *discordgo.Session satisfies it; the
// interface is here so the feed can be tested without Discord.
type Poster interface {
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend,
		options ...discordgo.RequestOption) (*discordgo.Message, error)
}

// feedBatch bounds one pass over the queue.
const feedBatch = 50

// errorBackoff is the wait after a failed pass, as opposed to idlePoll which is
// the wait after an EMPTY one. They are deliberately separate: an empty queue
// can be checked lazily, but a backlog that failed to post is work waiting to
// happen and should be retried promptly. discordgo already honours Retry-After
// on a 429 internally, so this does not need to be a rate-limit backoff -- it
// only stops a hard failure becoming a spin loop.
const errorBackoff = time.Second

// Embed colours, chosen so a PvP kill and a death by the world are
// distinguishable at a glance in a busy channel.
const (
	colourKill        = 0xC0392B // red
	colourEnvironment = 0x7F8C8D // grey
)

// Feed posts one message per kill, in order.
//
// The queue is LOSSLESS: an event is marked posted only after Discord accepts
// it, so a rate limit or an outage delays the feed rather than dropping it.
// The cost of that choice is an unbounded backlog during a long outage, which
// is why obsidibot_kill_feed_backlog exists and is worth alerting on.
type Feed struct {
	store   *db.Store
	poster  Poster
	metrics *metrics.Metrics
	cfg     *config.Config
	guildID string
}

// NewFeed builds the kill feed worker. guildID is the guild discovered at
// startup, which is where the feed's channel setting lives.
func NewFeed(store *db.Store, poster Poster, m *metrics.Metrics, cfg *config.Config, guildID string) *Feed {
	return &Feed{store: store, poster: poster, metrics: m, cfg: cfg, guildID: guildID}
}

// Run drains the feed until ctx is cancelled.
func (f *Feed) Run(ctx context.Context) error {
	for {
		posted, err := f.drain(ctx)
		if err != nil {
			//nolint:nilerr // deliberate: cancellation is a clean stop
			if ctx.Err() != nil {
				return nil
			}
			// A Discord failure must not end the job -- the backlog is
			// deliberately durable, so waiting and retrying is the correct
			// response and the gauge shows how far behind it is.
			slog.ErrorContext(ctx, "kill feed pass failed, will retry", "error", err)
			if !sleep(ctx, errorBackoff) {
				return nil
			}
			continue
		}

		if posted == 0 && !sleep(ctx, idlePoll) {
			return nil
		}
	}
}

func (f *Feed) drain(ctx context.Context) (int, error) {
	q := f.store.Queries()

	if f.metrics != nil {
		if backlog, err := q.CountUnpostedEvents(ctx); err == nil {
			f.metrics.KillFeedBacklog.Set(float64(backlog))
		}
	}

	events, err := q.NextUnpostedEvents(ctx, feedBatch)
	if err != nil {
		return 0, fmt.Errorf("read unposted events: %w", err)
	}
	if len(events) == 0 {
		return 0, nil
	}

	channelID, err := f.channel(ctx)
	if err != nil {
		return 0, err
	}
	if channelID == "" {
		// Nowhere to post. Mark them done rather than accumulating a backlog
		// that waits on a human: a moderator who sets the channel next week
		// wants the feed from then on, not a month of history dumped at once.
		for _, event := range events {
			if err := q.MarkEventPosted(ctx, event.ID); err != nil {
				return 0, fmt.Errorf("skip event %d: %w", event.ID, err)
			}
		}
		slog.DebugContext(ctx, "no kill feed channel configured; skipping events", "count", len(events))
		return len(events), nil
	}

	for _, event := range events {
		if _, err := f.poster.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
			Embeds: []*discordgo.MessageEmbed{f.embed(event)},
			// The feed names players constantly. Rendering is wanted;
			// notifying them every time they die is not.
			AllowedMentions: &discordgo.MessageAllowedMentions{
				Parse: []discordgo.AllowedMentionType{},
			},
		}, discordgo.WithContext(ctx)); err != nil {
			// Stop the batch and leave this event unposted, so the feed stays
			// in order and nothing is lost.
			return 0, fmt.Errorf("post kill %d: %w", event.ID, err)
		}

		// Marked only AFTER Discord accepted it. The other order would drop a
		// kill whenever a post failed.
		if err := q.MarkEventPosted(ctx, event.ID); err != nil {
			return 0, fmt.Errorf("mark event %d posted: %w", event.ID, err)
		}
	}
	return len(events), nil
}

// channel returns the configured kill feed channel, or "" if a moderator has
// not set one.
func (f *Feed) channel(ctx context.Context) (string, error) {
	cfg, err := f.store.Queries().GetGuildConfig(ctx, f.guildID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read guild config: %w", err)
	}
	if cfg.KillFeedChannelID == nil {
		return "", nil
	}
	return *cfg.KillFeedChannelID, nil
}

// embed renders one kill.
//
// It carries NO COORDINATES. VictimPOI is a named region rather than a
// position, and is included only when killfeed.showPOI is on, which it is not
// by default.
func (f *Feed) embed(event gen.KillEvent) *discordgo.MessageEmbed {
	victim := discordfmt.Describe(event.VictimName, event.VictimDino, event.VictimGrowth)

	embed := &discordgo.MessageEmbed{
		Timestamp: event.ReceivedAt.Format(time.RFC3339),
		Color:     colourEnvironment,
	}

	switch {
	case event.KillerAgid == nil:
		embed.Description = fmt.Sprintf("**%s** %s", victim, environmentPhrase(event.DamageType))
	case event.Credited:
		embed.Color = colourKill
		embed.Description = fmt.Sprintf("**%s** killed **%s**",
			discordfmt.Describe(derefOr(event.KillerName, *event.KillerAgid), event.KillerDino, event.KillerGrowth),
			victim)
	default:
		// A kill the rules did not credit -- an admin, or a self-kill. It still
		// happened, so the feed still shows it; it simply moved no rating.
		embed.Description = fmt.Sprintf("**%s** killed **%s** *(unranked)*",
			discordfmt.Describe(derefOr(event.KillerName, *event.KillerAgid), event.KillerDino, event.KillerGrowth),
			victim)
	}

	if f.cfg.KillFeed.ShowPOI && event.VictimPoi != nil && *event.VictimPoi != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name: "Location", Value: discordfmt.EscapeMarkdown(*event.VictimPoi), Inline: true,
		})
	}
	return embed
}

// environmentPhrase turns a damage type into something readable. An unknown one
// falls back to naming it, so a new damage type shows up as itself rather than
// as a wrong guess.
func environmentPhrase(damageType string) string {
	switch damageType {
	case "DT_THIRST":
		return "died of thirst"
	case "DT_HUNGER":
		return "starved"
	case "DT_OXYGEN":
		return "drowned"
	case "DT_BLEED":
		return "bled out"
	case "DT_BREAKLEGS":
		return "died from a fall"
	case "DT_TRAMPLE":
		return "was trampled"
	case "DT_SPIKES":
		return "died on spikes"
	case "DT_GENERIC", "DT_ATTACK", "DT_ARMORPIERCING":
		return "died"
	default:
		return "died (" + discordfmt.EscapeMarkdown(damageType) + ")"
	}
}

func derefOr(v *string, fallback string) string {
	if v == nil || *v == "" {
		return fallback
	}
	return *v
}

// sleep waits for d, reporting false if the context ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

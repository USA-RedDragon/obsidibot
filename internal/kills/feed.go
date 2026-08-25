package kills

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
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

// blockedBackoff is the wait after an error that RETRYING CANNOT FIX.
//
// A missing permission or a deleted channel needs a human. Retrying it on the
// transient schedule means roughly one doomed request per second, forever:
// pointless load on Discord's API, a log line per second burying everything
// else, and no closer to working. Waiting minutes costs nothing, because the
// queue is durable and drains the moment somebody fixes the channel.
const blockedBackoff = 5 * time.Minute

// Embed colours, chosen so a PvP kill and a death by the world are
// distinguishable at a glance in a busy channel.
const (
	colourKill        = 0xC0392B // red
	colourEnvironment = 0x7F8C8D // grey
	colourUnranked    = 0xE67E22 // orange: it happened, but it did not count
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
			// response and the gauge shows how far behind it is. But WHAT to
			// wait depends on whether retrying could ever help.
			backoff := errorBackoff
			if blocked, why := blockedFromPosting(err); blocked {
				backoff = blockedBackoff
				// Logged once per blockedBackoff rather than once per second,
				// and phrased so the fix is in the message: whoever reads this
				// needs to change a channel permission, not restart anything.
				slog.ErrorContext(ctx, "cannot post to the kill feed channel; "+
					"grant obsidibot View Channel, Send Messages and Embed Links there. "+
					"Kills are still being recorded and will post once it can",
					"reason", why, "retryIn", backoff, "error", err)
			} else {
				slog.ErrorContext(ctx, "kill feed pass failed, will retry", "error", err)
			}
			if !sleep(ctx, backoff) {
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

// embed renders one kill, carrying EVERY field the game reports.
//
// The game's own Discord webhook posts a raw field dump -- twenty labelled
// lines including two coordinate triples -- which is complete but unreadable in
// a busy channel. This keeps all of it and arranges it: who and whom in the
// title, the two combatants side by side, and the circumstances underneath.
func (f *Feed) embed(event gen.NextUnpostedEventsRow) *discordgo.MessageEmbed {
	embed := &discordgo.MessageEmbed{
		Timestamp: event.ReceivedAt.Format(time.RFC3339),
		Color:     colourEnvironment,
	}

	victimName := discordfmt.EscapeMarkdown(event.VictimName)
	killerName := ""
	if event.KillerAgid != nil {
		killerName = discordfmt.EscapeMarkdown(derefOr(event.KillerName, *event.KillerAgid))
	}

	// The world killed them if nobody did, or if the game named them as their
	// own killer -- which is how thirst, falls and impacts actually arrive.
	// Reading only the nil case rendered those as "X killed X".
	byTheWorld := event.KillerAgid == nil || *event.KillerAgid == event.VictimAgid

	switch {
	case byTheWorld:
		embed.Title = victimName + " " + environmentPhrase(event.DamageType)
	case creditsAKill(event.DamageType, event.KillerAgid, event.VictimAgid):
		embed.Color = colourKill
		embed.Title = killerName + " killed " + victimName
	default:
		// Killed by somebody, but not in a way the rules rate -- a trample,
		// say. It still happened and it still counts as a death, so the footer
		// may only disclaim the RATING.
		embed.Color = colourUnranked
		embed.Title = killerName + " killed " + victimName
		embed.Footer = &discordgo.MessageEmbedFooter{Text: "unranked - does not affect rating"}
	}

	// Suppressed when the world did it: the game names the victim as their own
	// killer for an environmental death, so this column would otherwise read
	// "Killer: Wrathbelly" directly under "Wrathbelly died of thirst".
	if !byTheWorld {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name: "Killer", Inline: true,
			Value: combatant(derefOr(event.KillerName, *event.KillerAgid), *event.KillerAgid,
				event.KillerCharacter, event.KillerDino, event.KillerGrowth,
				event.KillerRole, event.KillerIsAdmin,
				event.KillerRatingBefore, event.KillerRatingAfter),
		})
	}
	embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
		Name: "Victim", Inline: true,
		Value: combatant(event.VictimName, event.VictimAgid,
			event.VictimCharacter, event.VictimDino, event.VictimGrowth,
			event.VictimRole, event.VictimIsAdmin,
			event.VictimRatingBefore, event.VictimRatingAfter),
	})

	if details := circumstances(event, byTheWorld); details != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name: "Where & how", Inline: true, Value: details,
		})
	}
	return embed
}

// combatant renders one party: who they are, which character, what they were
// playing, how grown, what role, and their Alderon ID.
func combatant(name, agid string, character, species *string, growth *float64,
	role *string, isAdmin bool, ratingBefore, ratingAfter *float64,
) string {
	var b strings.Builder
	b.WriteString("**" + discordfmt.EscapeMarkdown(name) + "**")
	if isAdmin {
		b.WriteString(" - admin")
	}
	b.WriteString("\n`" + discordfmt.EscapeMarkdown(agid) + "`")

	if character != nil && *character != "" {
		b.WriteString("\n" + discordfmt.EscapeMarkdown(*character))
	}

	line := ""
	if species != nil && *species != "" {
		line = discordfmt.EscapeMarkdown(*species)
	}
	if growth != nil {
		if line != "" {
			line += " / "
		}
		line += fmt.Sprintf("%.0f%%", *growth*100)
	}
	if line != "" {
		b.WriteString("\n" + line)
	}
	if role != nil && *role != "" {
		b.WriteString("\n" + discordfmt.EscapeMarkdown(*role))
	}
	if figures := ratingMove(ratingBefore, ratingAfter); figures != "" {
		b.WriteString("\n" + figures)
	}
	return b.String()
}

// circumstances renders where and how: the point of interest, the damage type,
// how far apart the two were, the in-world clock, and both coordinates.
func circumstances(event gen.NextUnpostedEventsRow, byTheWorld bool) string {
	var lines []string
	if event.VictimPoi != nil && *event.VictimPoi != "" {
		lines = append(lines, discordfmt.EscapeMarkdown(*event.VictimPoi))
	}

	how := damageLabel(event.DamageType)
	// An environmental death carries a distance of 0 rather than the -1 the
	// ingest endpoint drops, so without this a thirst death reports "0.0 m"
	// as though somebody had been standing on top of them.
	if event.KillDistance != nil && !byTheWorld {
		// The game reports Unreal units, i.e. centimetres. Verified against a
		// real event by recomputing the distance from the two coordinates.
		how += fmt.Sprintf(" / %.1f m", *event.KillDistance/100)
	}
	lines = append(lines, how)

	if event.TimeOfDay != nil {
		lines = append(lines, inWorldClock(*event.TimeOfDay)+" in-world")
	}
	if coords := formatLocation(event.KillerLocation); coords != "" {
		lines = append(lines, "K `"+coords+"`")
	}
	if coords := formatLocation(event.VictimLocation); coords != "" {
		lines = append(lines, "V `"+coords+"`")
	}
	return strings.Join(lines, "\n")
}

// inWorldClock renders the game's day clock.
//
// TimeOfDay is HUNDREDTHS OF AN HOUR, not HHMM: 1489 means 14.89 hours, which
// is 14:53. Printing the low two digits as minutes produced impossible times --
// 14 of the first 46 live events had a minutes field of 60 or more, and 1779
// rendered as "17:79".
func inWorldClock(timeOfDay int32) string {
	hours := timeOfDay / 100
	minutes := (timeOfDay % 100) * 60 / 100
	return fmt.Sprintf("%02d:%02d", hours, minutes)
}

//nolint:gochecknoglobals // compiled once and never reassigned
var locationRE = regexp.MustCompile(
	`X=(-?[\d.]+)\s*,?\s*Y=(-?[\d.]+)\s*,?\s*Z=(-?[\d.]+)`)

// formatLocation turns the game's "(X=105140.83,Y=162629.63,Z=-410.86)" into
// something a person can read. Unparseable input renders as nothing rather than
// as a broken string.
func formatLocation(raw *string) string {
	if raw == nil || *raw == "" {
		return ""
	}
	m := locationRE.FindStringSubmatch(*raw)
	if m == nil {
		return ""
	}
	out := make([]string, 0, 3)
	for _, axis := range m[1:4] {
		v, err := strconv.ParseFloat(axis, 64)
		if err != nil {
			return ""
		}
		out = append(out, strconv.FormatInt(int64(v), 10))
	}
	return strings.Join(out, ", ")
}

// damageLabel renders a damage type as a noun for the details line.
func damageLabel(damageType string) string {
	if label, ok := damageLabels[damageType]; ok {
		return label
	}
	return discordfmt.EscapeMarkdown(damageType)
}

//nolint:gochecknoglobals // a lookup table, never reassigned
var damageLabels = map[string]string{
	"DT_ATTACK": "Attack", "DT_THIRST": "Thirst", "DT_HUNGER": "Hunger",
	"DT_OXYGEN": "Drowning", "DT_BLEED": "Bleeding", "DT_BREAKLEGS": "Fall",
	// DT_IMPACT is the other half of falling and arrives from the live server;
	// it was absent here, so it rendered as the raw "DT\_IMPACT".
	"DT_IMPACT":  "Fall",
	"DT_TRAMPLE": "Trampled", "DT_SPIKES": "Spikes", "DT_GENERIC": "Unknown causes",
	"DT_ARMORPIERCING": "Armour piercing",
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
	case "DT_BREAKLEGS", "DT_IMPACT":
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

// ratingMove renders what a kill did to one player's rating.
//
// Both figures are stored on the event when it is rated, because this is the
// only moment anything knows them -- recomputing later would mean replaying the
// whole order-dependent chain. Nothing is rendered when nothing moved: a "+0.0"
// reads as a result of zero rather than as "this kill was not rated".
func ratingMove(before, after *float64) string {
	if before == nil || after == nil {
		return ""
	}
	delta := *after - *before
	sign := "+"
	if delta < 0 {
		// A minus sign rather than a hyphen: the feed is read at a glance and
		// the two are hard to tell apart in Discord's font.
		sign = "\u2212"
		delta = -delta
	}
	return fmt.Sprintf("%.1f \u2192 %.1f (%s%.1f)", *before, *after, sign, delta)
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

// blockedFromPosting reports whether an error means the bot cannot post to the
// configured channel at all, as opposed to a failure worth retrying promptly.
//
// These are the states a human has to resolve: the channel denies the bot
// Send Messages or Embed Links, the bot cannot see it, or it has been deleted.
// Everything else -- rate limits, 5xx, a dropped connection -- is transient and
// gets the short backoff.
func blockedFromPosting(err error) (bool, string) {
	var rest *discordgo.RESTError
	if !errors.As(err, &rest) || rest.Message == nil {
		return false, ""
	}
	switch rest.Message.Code {
	case discordgo.ErrCodeMissingPermissions:
		return true, "missing Send Messages or Embed Links in the channel"
	case discordgo.ErrCodeMissingAccess:
		return true, "the bot cannot see the channel"
	case discordgo.ErrCodeUnknownChannel:
		return true, "the channel no longer exists; set a new one with /config kill-channel"
	default:
		return false, ""
	}
}

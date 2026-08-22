package moderation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/discordfmt"
	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5"
)

// Poster sends a message to a channel. *discordgo.Session satisfies it.
//
// It is structurally identical to kills.Poster and deliberately declared again
// rather than imported: one method is cheaper to repeat than a dependency from
// moderation to the kill feed, which have nothing else to do with each other.
type Poster interface {
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend,
		options ...discordgo.RequestOption) (*discordgo.Message, error)
}

// Feed colours. Warnings are advisory, bans are not, and the two lifting
// outcomes share the "it is over" green.
const (
	colourWarn    = 0xF1C40F // yellow
	colourBan     = 0xC0392B // red
	colourLifted  = 0x2ECC71 // green
	feedFieldWrap = 1024     // Discord's per-field limit
)

// Kind selects which feed channel a notice belongs in.
type Kind int

const (
	// KindWarn goes to the warn feed.
	KindWarn Kind = iota
	// KindBan goes to the ban feed -- bans, unbans and expiries alike, because
	// a lift is only readable next to the ban it lifted.
	KindBan
)

// Feed posts moderation notices to the configured channels.
//
// # This is NOT the kill feed's lossless queue, on purpose
//
// The warns/bans ROW is the durable record of a moderation action, and it is
// written before anything is posted. So a Discord outage may cost the channel a
// notice, and that is strictly better than the alternative: a queue whose
// backlog delays a ban, or an action that fails because a channel was deleted.
// Post is therefore best-effort and never returns an error to its caller; the
// permanent-versus-transient classification exists only to make the LOG line
// actionable.
type Feed struct {
	store  *db.Store
	poster Poster

	// A feed channel nobody has configured is normal, not a fault, so the skip
	// is silent -- but silent forever is undiagnosable ("why is the feed
	// empty?"), so the first skip of each kind says so once.
	unsetOnce [2]sync.Once
}

// NewFeed builds the notice poster. poster may be nil in tests that only care
// about the database side.
func NewFeed(store *db.Store, poster Poster) *Feed {
	return &Feed{store: store, poster: poster}
}

// Post delivers one notice, best-effort.
//
// guildID is passed per call rather than held on the struct because the two
// callers know it differently: a slash command reads it off the interaction,
// the scheduler holds the guild discovered at startup.
func (f *Feed) Post(ctx context.Context, guildID string, kind Kind, embed *discordgo.MessageEmbed) {
	if f.poster == nil || embed == nil {
		return
	}
	channelID, err := f.channel(ctx, guildID, kind)
	if err != nil {
		slog.WarnContext(ctx, "could not read the moderation feed channel", "error", err)
		return
	}
	if channelID == "" {
		if kind < 0 || int(kind) >= len(f.unsetOnce) {
			// Unreachable from the two constants; a background job must not
			// panic on an index if a third kind is ever added.
			return
		}
		f.unsetOnce[kind].Do(func() {
			slog.InfoContext(ctx, "no moderation feed channel is configured, so notices are not being posted; "+
				"set one with /config ban-channel or /config warn-channel", "feed", kind.String())
		})
		return
	}

	if _, err := f.poster.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{embed},
		// The notices name people by mention so the record is unambiguous.
		// Rendering the mention is wanted; pinging someone every time their
		// ban is discussed is not.
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	}, discordgo.WithContext(ctx)); err != nil {
		if blocked, why := blockedFromPosting(err); blocked {
			// Phrased so the fix is in the message: this needs a channel
			// permission changed, not a restart. The action itself already
			// happened and is recorded either way.
			slog.ErrorContext(ctx, "cannot post to the moderation feed channel; "+
				"grant obsidibot View Channel, Send Messages and Embed Links there",
				"feed", kind.String(), "reason", why, "error", err)
			return
		}
		slog.WarnContext(ctx, "could not post a moderation notice", "feed", kind.String(), "error", err)
	}
}

// String names the feed for a log line.
func (k Kind) String() string {
	if k == KindWarn {
		return "warn"
	}
	return "ban"
}

func (f *Feed) channel(ctx context.Context, guildID string, kind Kind) (string, error) {
	cfg, err := f.store.Queries().GetGuildConfig(ctx, guildID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read guild config: %w", err)
	}
	id := cfg.BanFeedChannelID
	if kind == KindWarn {
		id = cfg.WarnFeedChannelID
	}
	if id == nil {
		return "", nil
	}
	return *id, nil
}

// blockedFromPosting reports whether an error means the bot cannot post to the
// channel at all, as opposed to something that would work on a retry. Same
// distinction the kill feed draws, for the same reason: only one of the two is
// a human's to fix, and only one is worth an error-level line.
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
		return true, "the channel no longer exists; set a new one with /config ban-channel or /config warn-channel"
	default:
		return false, ""
	}
}

// WarnEmbed renders a warning. count is how many warnings this player now has,
// counted across both identities.
func WarnEmbed(warn gen.Warn, count int64) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Title:     "Warning",
		Color:     colourWarn,
		Timestamp: warn.CreatedAt.Format(discordTimeLayout),
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Player", Value: targetLabel(warn.AlderonID, warn.DiscordUserID, warn.TargetName)},
			{Name: "Reason", Value: reasonValue(warn.Reason)},
			{Name: "Moderator", Value: moderatorLabel(warn.IssuedByDiscordID), Inline: true},
		},
		Footer: &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("warning #%d for this player", count)},
	}
}

// BanEmbed renders a ban. enforcement says what the GAME was told, which is a
// different fact from what the database recorded and the one people actually
// want: a ban nobody could enforce still looks like a ban in a channel.
func BanEmbed(ban gen.Ban, count int64, enforcement string) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Title:     "Ban",
		Color:     colourBan,
		Timestamp: ban.CreatedAt.Format(discordTimeLayout),
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Player", Value: targetLabel(ban.AlderonID, ban.DiscordUserID, ban.TargetName)},
			{Name: "Reason", Value: reasonValue(ban.Reason)},
			{Name: "Expires", Value: FormatExpiry(ban.ExpiresAt), Inline: true},
			{Name: "Moderator", Value: moderatorLabel(ban.IssuedByDiscordID), Inline: true},
			{Name: "Enforcement", Value: reasonValue(enforcement)},
		},
		Footer: &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("ban #%d for this player", count)},
	}
}

// UnbanEmbed renders a ban lifted by a moderator.
func UnbanEmbed(ban gen.Ban, byDiscordID, note string) *discordgo.MessageEmbed {
	fields := []*discordgo.MessageEmbedField{
		{Name: "Player", Value: targetLabel(ban.AlderonID, ban.DiscordUserID, ban.TargetName)},
		{Name: "Original reason", Value: reasonValue(ban.Reason)},
		{Name: "Moderator", Value: moderatorLabel(byDiscordID), Inline: true},
	}
	if note != "" {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Note", Value: reasonValue(note)})
	}
	return &discordgo.MessageEmbed{
		Title:  "Ban lifted",
		Color:  colourLifted,
		Fields: fields,
	}
}

// ExpiredEmbed renders a ban the scheduler let run out. It is posted only after
// the row is closed, so a notice in the channel always means the player really
// can rejoin.
func ExpiredEmbed(ban gen.Ban, note string) *discordgo.MessageEmbed {
	fields := []*discordgo.MessageEmbedField{
		{Name: "Player", Value: targetLabel(ban.AlderonID, ban.DiscordUserID, ban.TargetName)},
		{Name: "Original reason", Value: reasonValue(ban.Reason)},
	}
	if note != "" {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Note", Value: reasonValue(note)})
	}
	return &discordgo.MessageEmbed{
		Title:  "Ban expired",
		Color:  colourLifted,
		Fields: fields,
	}
}

// discordTimeLayout is RFC3339, which is what discordgo expects in an embed
// timestamp.
const discordTimeLayout = "2006-01-02T15:04:05Z07:00"

// targetLabel names the person every way the record knows them: the display
// name captured when the action was taken, the Alderon ID, and the Discord
// account if one was linked at the time.
//
// EVERY piece of it is user-supplied at some point -- an in-game name is chosen
// by the player -- so all of it goes through EscapeMarkdown.
func targetLabel(agid, discordID, name *string) string {
	var parts []string
	if name != nil && *name != "" {
		parts = append(parts, "**"+discordfmt.EscapeMarkdown(*name)+"**")
	}
	if agid != nil && *agid != "" {
		parts = append(parts, "`"+discordfmt.EscapeMarkdown(*agid)+"`")
	}
	if discordID != nil && *discordID != "" {
		parts = append(parts, "<@"+*discordID+">")
	}
	if len(parts) == 0 {
		return "*unknown*"
	}
	return strings.Join(parts, " ")
}

func moderatorLabel(discordID string) string {
	if discordID == "" {
		return "*obsidibot*"
	}
	return "<@" + discordID + ">"
}

// reasonValue makes a free-text field safe and short enough for an embed. An
// embed field over 1024 characters is refused by Discord OUTRIGHT, which would
// turn a long reason into a missing feed post.
func reasonValue(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "*none given*"
	}
	text = discordfmt.EscapeMarkdown(text)
	if len(text) > feedFieldWrap {
		// Cut with room to spare and repair any rune the cut landed inside:
		// Discord counts characters where this counts bytes, and invalid UTF-8
		// would be refused as surely as an over-long field.
		text = strings.ToValidUTF8(text[:feedFieldWrap-8], "") + "…"
	}
	return text
}

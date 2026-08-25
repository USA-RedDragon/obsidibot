package commands

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/discordfmt"
	"github.com/USA-RedDragon/obsidibot/internal/interactions"
	"github.com/bwmarrin/discordgo"
)

// The history sizes. Five on the summary because the embed has to stay
// readable next to the figures it explains; ten per page once somebody has
// asked to see more, which is about as much as reads comfortably without
// scrolling on a phone.
const (
	summaryHistory = 5
	pageHistory    = 10
)

// historyPrefix routes a button press back to this file. It is the first
// segment of every custom ID this file writes, and it is registered with the
// interactions router by NewStats.
const historyPrefix = "hist"

// historyPage is the state a paginated view needs, and all of it travels in
// the button's custom ID.
//
// The replicas share nothing, so the press may land on a different one than
// drew the message: anything held in memory here would work in testing and
// break the moment a load balancer did its job.
//
// anchor is the highest event id that existed when the FIRST page was drawn,
// and every page filters on it. Without that, a kill landing while somebody
// pages through shifts every row down by one and they see the same event
// twice -- which looks exactly like the bug a player checking their rating is
// hoping to find.
type historyPage struct {
	agid   string
	anchor int64
	offset int32
}

func (p historyPage) customID() string {
	return fmt.Sprintf("%s:%s:%d:%d", historyPrefix, p.agid, p.anchor, p.offset)
}

// parseHistoryPage reads a custom ID back. Anything malformed is a refusal
// rather than a panic: the string arrives from Discord, and a button from an
// older version of the bot can still be sitting in somebody's scrollback.
func parseHistoryPage(customID string) (historyPage, bool) {
	parts := strings.Split(customID, ":")
	if len(parts) != 4 || parts[0] != historyPrefix {
		return historyPage{}, false
	}
	anchor, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || anchor < 0 {
		return historyPage{}, false
	}
	offset, err := strconv.ParseInt(parts[3], 10, 32)
	if err != nil || offset < 0 {
		return historyPage{}, false
	}
	if !agidRE.MatchString(parts[1]) {
		// The id is interpolated into a query as an identity; refusing anything
		// that is not shaped like one keeps a hand-crafted custom ID from
		// asking questions about somebody else.
		return historyPage{}, false
	}
	return historyPage{agid: parts[1], anchor: anchor, offset: int32(offset)}, true
}

// renderHistory turns a page of events into the lines a player reads.
//
// Each line answers the three questions somebody disputing their rating
// actually asks: when, against whom, and what did it do to my number.
func renderHistory(rows []gen.PlayerHistoryRow, agid string) string {
	if len(rows) == 0 {
		return "_No recorded kills or deaths yet._"
	}

	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		lines = append(lines, historyLine(row, agid))
	}
	return strings.Join(lines, "\n")
}

func historyLine(row gen.PlayerHistoryRow, agid string) string {
	// Relative time rather than a date: "3 days ago" is what a dispute is
	// actually about, and it renders in the reader's own timezone.
	when := fmt.Sprintf("<t:%d:R>", row.ReceivedAt.Unix())

	// The world killed them if nobody did, or if the game named them as their
	// own killer -- which is how thirst, falls and impacts arrive.
	byTheWorld := row.KillerAgid == nil || *row.KillerAgid == row.VictimAgid

	var what, move string
	switch {
	case byTheWorld:
		what = damagePhrase(row.DamageType)
		move = ratingMove(row.VictimRatingBefore, row.VictimRatingAfter)
	case *row.KillerAgid == agid:
		what = "killed **" + discordfmt.EscapeMarkdown(row.VictimName) + "**"
		move = ratingMove(row.KillerRatingBefore, row.KillerRatingAfter)
	default:
		name := *row.KillerAgid
		if row.KillerName != nil && *row.KillerName != "" {
			name = *row.KillerName
		}
		name = discordfmt.EscapeMarkdown(name)
		what = "killed by **" + name + "**"
		move = ratingMove(row.VictimRatingBefore, row.VictimRatingAfter)
	}

	if move == "" {
		// Said outright rather than left blank. "No rating change" is an
		// answer; an empty column reads as missing data.
		move = "no rating change"
	}
	return fmt.Sprintf("%s - %s - %s", when, what, move)
}

// ratingMove renders one side of an exchange as a signed delta and a result.
func ratingMove(before, after *float64) string {
	if before == nil || after == nil {
		return ""
	}
	delta := *after - *before
	sign := "+"
	if delta < 0 {
		// A minus sign rather than a hyphen: these are read at a glance and
		// the two are hard to tell apart at Discord's font size.
		sign = "−"
		delta = -delta
	}
	return fmt.Sprintf("%s%.1f → %.1f", sign, delta, *after)
}

// damagePhrase describes a death nobody is credited with.
func damagePhrase(damageType string) string {
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
	default:
		return "died"
	}
}

// historyControls builds the buttons under a page.
//
// Both are emitted even when one cannot move, disabled rather than absent, so
// the row does not jump around underneath the reader's cursor as they page.
func historyControls(page historyPage, total int64) []discordgo.MessageComponent {
	back := page
	back.offset = max(0, page.offset-pageHistory)
	forward := page
	forward.offset = page.offset + pageHistory

	return []discordgo.MessageComponent{discordgo.ActionsRow{
		Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    "Newer",
				Style:    discordgo.SecondaryButton,
				CustomID: back.customID(),
				Disabled: page.offset == 0,
			},
			discordgo.Button{
				Label:    "Older",
				Style:    discordgo.SecondaryButton,
				CustomID: forward.customID(),
				Disabled: int64(forward.offset) >= total,
			},
		},
	}}
}

// viewMoreButton is the control on the /stats summary.
func viewMoreButton(page historyPage, total int64) []discordgo.MessageComponent {
	if total <= summaryHistory {
		// Nothing further to show, so offering to show it is a dead end.
		return nil
	}
	return []discordgo.MessageComponent{discordgo.ActionsRow{
		Components: []discordgo.MessageComponent{discordgo.Button{
			Label:    fmt.Sprintf("View all %d", total),
			Style:    discordgo.PrimaryButton,
			CustomID: page.customID(),
		}},
	}}
}

// history answers a page button.
func (s *Stats) history(ctx context.Context, ic interactions.Context) (interactions.Reply, error) {
	page, ok := parseHistoryPage(ic.Interaction.MessageComponentData().CustomID)
	if !ok {
		return userError("That button is no longer valid. Run `/stats` again."), nil
	}

	q := s.store.Queries()
	total, err := q.CountPlayerHistory(ctx, gen.CountPlayerHistoryParams{
		Agid: &page.agid, MaxID: page.anchor,
	})
	if err != nil {
		return interactions.Reply{}, fmt.Errorf("count player history: %w", err)
	}
	// Clamped rather than trusted: the offset arrives from a string a client
	// sent us, and paging past the end would otherwise render an empty page.
	if int64(page.offset) >= total && total > 0 {
		last := (total - 1) / pageHistory * pageHistory
		// A history long enough to overflow an int32 offset is not reachable --
		// it would need two billion events -- but the clamp is the one place
		// that arithmetic meets untrusted input, so it is bounded rather than
		// asserted.
		if last > math.MaxInt32 {
			last = math.MaxInt32
		}
		//nolint:gosec // clamped to MaxInt32 immediately above
		page.offset = int32(last)
	}

	rows, err := q.PlayerHistory(ctx, gen.PlayerHistoryParams{
		Agid: &page.agid, MaxID: page.anchor,
		PageSize: pageHistory, PageOffset: page.offset,
	})
	if err != nil {
		return interactions.Reply{}, fmt.Errorf("read player history: %w", err)
	}

	name := page.agid
	if player, err := q.GetPlayer(ctx, page.agid); err == nil {
		name = player.LastKnownName
	}

	first := int64(page.offset) + 1
	last := min(int64(page.offset)+int64(len(rows)), total)
	embed := &discordgo.MessageEmbed{
		Title:       discordfmt.EscapeMarkdown(name),
		Color:       colourStats,
		Description: renderHistory(rows, page.agid),
		Footer: &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("%d-%d of %d - %s", first, last, total, page.agid),
		},
	}

	return interactions.Reply{
		Embeds:     []*discordgo.MessageEmbed{embed},
		Components: historyControls(page, total),
		Ephemeral:  true,
	}, nil
}

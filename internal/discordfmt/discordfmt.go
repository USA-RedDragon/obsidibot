// Package discordfmt renders the values obsidibot shows to players.
//
// It exists so the kill feed, the leaderboard and /stats cannot disagree about
// what a K/D of "12 kills, no deaths" reads as, and so the escaping of
// player-controlled names is written once rather than three times.
package discordfmt

import (
	"fmt"
	"strings"
)

// zeroWidthSpace is inserted after an @ to stop a name being read as a mention.
const zeroWidthSpace = "\u200b"

//nolint:gochecknoglobals // built once and never reassigned
var markdownEscaper = strings.NewReplacer(
	`\`, `\\`,
	"`", "\\`",
	"*", `\*`,
	"_", `\_`,
	"~", `\~`,
	">", `\>`,
	"|", `\|`,
	// An @ is not markdown, but @everyone in a name would otherwise render as
	// a mention attempt. allowed_mentions already stops the ping; this stops
	// the name looking like one.
	"@", "@"+zeroWidthSpace,
)

// EscapeMarkdown neutralises the characters Discord treats as formatting.
//
// Player names are ATTACKER-CONTROLLED: they come from the game, and a name
// containing backticks or underscores would otherwise break an embed's layout
// or let someone style themselves into looking like part of the bot's own text.
func EscapeMarkdown(s string) string {
	return markdownEscaper.Replace(s)
}

// KD renders a kill/death ratio.
//
// No deaths is shown as the kill count with a marker rather than as infinity or
// as a divide-by-one. "12 kills, never died" is a different claim from a K/D of
// 12, and on a board people argue about, flattening the two would be a small
// lie.
func KD(kills, deaths int32) string {
	switch {
	case kills == 0 && deaths == 0:
		return "—"
	case deaths == 0:
		return fmt.Sprintf("%d.00*", kills)
	default:
		return fmt.Sprintf("%.2f", float64(kills)/float64(deaths))
	}
}

// Describe renders "name (Species 50%)", omitting whatever is unknown.
func Describe(name string, species *string, growth *float64) string {
	out := EscapeMarkdown(name)
	var parts []string
	if species != nil && *species != "" {
		parts = append(parts, EscapeMarkdown(*species))
	}
	if growth != nil {
		parts = append(parts, fmt.Sprintf("%.0f%%", *growth*100))
	}
	if len(parts) > 0 {
		out += " (" + strings.Join(parts, " ") + ")"
	}
	return out
}

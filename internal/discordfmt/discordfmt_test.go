package discordfmt_test

import (
	"strings"
	"testing"

	"github.com/USA-RedDragon/obsidibot/internal/discordfmt"
)

// TestEscapeMarkdownNeutralisesPlayerNames. Names come from the game and are
// attacker-controlled: without escaping, a player can style themselves into
// looking like part of the bot's own text, or break an embed's layout.
func TestEscapeMarkdownNeutralisesPlayerNames(t *testing.T) {
	// Exact outputs rather than a "does not contain" check: escaping works by
	// PREFIXING a backslash, so `\> quote` still contains `> quote` as a
	// substring even though it renders as a literal. Only the exact form says
	// whether the escaping is right.
	hostile := []struct {
		name string
		want string
	}{
		{"**Admin**", `\*\*Admin\*\*`},
		{"`code`", "\\`code\\`"},
		{"__underline__", `\_\_underline\_\_`},
		{"~~strike~~", `\~\~strike\~\~`},
		{"> quote", `\> quote`},
		{"||spoiler||", `\|\|spoiler\|\|`},
		{`back\slash`, `back\\slash`},
	}
	for _, tc := range hostile {
		if got := discordfmt.EscapeMarkdown(tc.name); got != tc.want {
			t.Errorf("EscapeMarkdown(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}

	// An @everyone in a name must not read as a mention attempt.
	got := discordfmt.EscapeMarkdown("@everyone")
	if strings.Contains(got, "@everyone") {
		t.Errorf("EscapeMarkdown(%q) = %q, which still reads as a mention", "@everyone", got)
	}

	// An ordinary name survives unchanged.
	if got := discordfmt.EscapeMarkdown("kittykat95"); got != "kittykat95" {
		t.Errorf("an ordinary name was mangled: %q", got)
	}
}

// TestKD pins the one rendering decision people will argue about.
func TestKD(t *testing.T) {
	tests := []struct {
		kills, deaths int32
		want          string
	}{
		{0, 0, "—"},
		{12, 4, "3.00"},
		{1, 3, "0.33"},
		{0, 5, "0.00"},
		// Never died: shown as the kill count with a marker, NOT as infinity
		// and not as a divide-by-one, because "12 kills, never died" is a
		// different claim from a K/D of 12.
		{12, 0, "12.00*"},
		{1, 0, "1.00*"},
	}
	for _, tc := range tests {
		if got := discordfmt.KD(tc.kills, tc.deaths); got != tc.want {
			t.Errorf("KD(%d, %d) = %q, want %q", tc.kills, tc.deaths, got, tc.want)
		}
	}
}

func TestDescribe(t *testing.T) {
	species := "Ceratosaurus"
	growth := 0.5
	if got := discordfmt.Describe("kitty", &species, &growth); got != "kitty (Ceratosaurus 50%)" {
		t.Errorf("Describe = %q", got)
	}
	if got := discordfmt.Describe("kitty", nil, nil); got != "kitty" {
		t.Errorf("Describe with nothing known = %q", got)
	}
	if got := discordfmt.Describe("kitty", &species, nil); got != "kitty (Ceratosaurus)" {
		t.Errorf("Describe without growth = %q", got)
	}
	// The species field also comes from the game.
	hostile := "**Rex**"
	if got := discordfmt.Describe("kitty", &hostile, nil); strings.Contains(got, "**Rex**") {
		t.Errorf("Describe did not escape the species: %q", got)
	}
}

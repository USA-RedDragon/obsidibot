package gamecmd_test

import (
	"slices"
	"testing"

	"github.com/USA-RedDragon/obsidibot/internal/bank"
	"github.com/USA-RedDragon/obsidibot/internal/gamecmd"
)

// TestParse covers what the game actually delivers. The message ARRIVES WITH
// its prefix, and which prefix that is belongs to the server's
// PlayerCommandEscapeChars, which this bot cannot read -- so any leading
// non-alphanumeric rune is the escape char and the command is what follows.
func TestParse(t *testing.T) {
	tests := map[string]struct {
		message string
		name    string
		args    []string
		escape  rune
	}{
		"default prefix":      {"!balance", gamecmd.CommandBalance, nil, '!'},
		"reconfigured prefix": {".balance", gamecmd.CommandBalance, nil, '.'},
		"slash prefix":        {"/balance", gamecmd.CommandBalance, nil, '/'},
		"mixed case":          {"!BaLaNcE", gamecmd.CommandBalance, nil, '!'},
		"leading space":       {"  !balance  ", gamecmd.CommandBalance, nil, '!'},
		"with argument":       {"!deposit 500", gamecmd.CommandDeposit, []string{"500"}, '!'},
		"extra arguments":     {"!withdraw 500 now", gamecmd.CommandWithdraw, []string{"500", "now"}, '!'},
		"link":                {"!link", gamecmd.CommandLink, nil, '!'},
		"help":                {"!help", gamecmd.CommandHelp, nil, '!'},
		"unknown command":     {"!teleport me", gamecmd.CommandUnknown, []string{"me"}, '!'},
		"prefix only":         {"!", gamecmd.CommandUnknown, nil, '!'},
		"empty":               {"", gamecmd.CommandUnknown, nil, '!'},
		// No prefix at all should not be reachable through the webhook, but if
		// it ever is, it must not silently become a command.
		"no prefix": {"balance", gamecmd.CommandBalance, nil, '!'},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := gamecmd.Parse(tc.message)
			if got.Name != tc.name {
				t.Errorf("name = %q, want %q", got.Name, tc.name)
			}
			if !slices.Equal(got.Args, tc.args) {
				t.Errorf("args = %v, want %v", got.Args, tc.args)
			}
			if got.Escape != tc.escape {
				t.Errorf("escape = %q, want %q", got.Escape, tc.escape)
			}
		})
	}
}

// TestParseNeverLeaksChatIntoTheMetricLabel: the command label is a Prometheus
// dimension, and player-supplied text is unbounded. Everything unrecognised
// has to collapse to the one "unknown" cell.
func TestParseNeverLeaksChatIntoTheMetricLabel(t *testing.T) {
	for _, message := range []string{
		"!drop table players", "!" + string(make([]byte, 400)), "!💀", "!../../etc/passwd", "!ban someone",
	} {
		if got := gamecmd.Parse(message); got.Name != gamecmd.CommandUnknown {
			t.Errorf("Parse(%q).Name = %q, want unknown", message, got.Name)
		}
	}
}

// TestParseAmount. Omitting the amount is the common case and means
// everything, matching the slash commands; a copied "1,000" is not a mistake
// worth refusing; and anything unreadable is a refusal rather than a zero,
// which would otherwise read as "nothing to move".
func TestParseAmount(t *testing.T) {
	tests := map[string]struct {
		args   []string
		want   int64
		wantOK bool
	}{
		"absent":       {nil, bank.AmountAll, true},
		"all":          {[]string{"all"}, bank.AmountAll, true},
		"ALL":          {[]string{"ALL"}, bank.AmountAll, true},
		"plain":        {[]string{"500"}, 500, true},
		"with commas":  {[]string{"1,000"}, 1000, true},
		"zero":         {[]string{"0"}, 0, false},
		"negative":     {[]string{"-5"}, 0, false},
		"not a number": {[]string{"lots"}, 0, false},
		"float":        {[]string{"1.5"}, 0, false},
		"hex":          {[]string{"0x10"}, 0, false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := gamecmd.ParseAmount(tc.args)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Errorf("amount = %d, want %d", got, tc.want)
			}
		})
	}
}

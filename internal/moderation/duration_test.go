package moderation_test

import (
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/moderation"
)

func TestParseDurationAcceptsWhatModeratorsType(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"1m", time.Minute},
		{"30m", 30 * time.Minute},
		{"3h30m", 3*time.Hour + 30*time.Minute},
		{"1d", 24 * time.Hour},
		{"7d", 7 * 24 * time.Hour},
		{"1d3h43m", 24*time.Hour + 3*time.Hour + 43*time.Minute},
		// A moderator holding shift is not a syntax error.
		{"1D3H", 27 * time.Hour},
		{"  2d  ", 48 * time.Hour},
		// The upper bound itself is allowed; only past it is refused.
		{"1825d", moderation.MaxBanDuration},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := moderation.ParseDuration(tc.in)
			if err != nil {
				t.Fatalf("ParseDuration(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseDuration(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestParseDurationRefusals covers every way a ban duration can be wrong. The
// sub-minute cases matter most: the scheduler wakes once a minute, so a "30s"
// ban would be a promise nothing can keep.
func TestParseDurationRefusals(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"whitespace", "   "},
		{"sub-minute", "30s"},
		{"milliseconds", "1500ms"},
		{"microseconds", "5000us"},
		{"nanoseconds", "60000000000ns"},
		{"negative", "-1h"},
		{"negative days", "-1d"},
		{"zero", "0m"},
		{"over five years", "1826d"},
		{"absurd day count", "99999999999999d"},
		{"days not a prefix", "3h1d"},
		{"two day components", "1d2d"},
		{"fractional days", "1.5d"},
		{"nonsense", "soon"},
		{"bare number", "5"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := moderation.ParseDuration(tc.in)
			if err == nil {
				t.Fatalf("ParseDuration(%q) = %v, want a refusal", tc.in, got)
			}
			// The message is shown to the moderator verbatim, so it has to read
			// as a sentence rather than as an internal error.
			msg := err.Error()
			if msg == "" || !strings.HasSuffix(msg, ".") {
				t.Errorf("refusal for %q is not a sentence: %q", tc.in, msg)
			}
		})
	}
}

func TestFormatExpiryDistinguishesPermanent(t *testing.T) {
	if got := moderation.FormatExpiry(nil); got != "permanent" {
		t.Errorf("FormatExpiry(nil) = %q", got)
	}
	when := time.Unix(1700000000, 0)
	if got := moderation.FormatExpiry(&when); got != "<t:1700000000:R>" {
		t.Errorf("FormatExpiry = %q, want a Discord relative timestamp", got)
	}
}

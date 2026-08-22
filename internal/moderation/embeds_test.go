package moderation_test

import (
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/moderation"
)

func strPtr(s string) *string { return &s }

// TestEmbedsEscapeEverythingASupplierChose. An in-game name and a moderator's
// reason are both typed by a person, and both land in a channel where markdown
// renders. A player called "**everyone**" must not be able to reformat the feed.
func TestEmbedsEscapeEverythingASupplierChose(t *testing.T) {
	warn := gen.Warn{
		AlderonID:         strPtr("555-000-101"),
		TargetName:        strPtr("**loud**"),
		Reason:            "said `something` *rude*",
		IssuedByDiscordID: "discord-1",
		CreatedAt:         time.Unix(1700000000, 0),
	}
	embed := moderation.WarnEmbed(warn, 3)

	for _, field := range embed.Fields {
		if strings.Contains(field.Value, "**loud**") {
			t.Errorf("an unescaped player name reached the embed: %q", field.Value)
		}
		if strings.Contains(field.Value, "*rude*") {
			t.Errorf("an unescaped reason reached the embed: %q", field.Value)
		}
	}
	if embed.Footer == nil || embed.Footer.Text != "warning #3 for this player" {
		t.Errorf("footer = %+v, want the warning count", embed.Footer)
	}
}

// TestBanEmbedStatesEnforcement: whether the GAME is holding the ban is a
// different fact from whether obsidibot recorded it, and the feed has to say
// which -- a ban nobody could enforce otherwise looks exactly like one that is
// keeping the player out.
func TestBanEmbedStatesEnforcement(t *testing.T) {
	ban := gen.Ban{
		ID: 7, AlderonID: strPtr("555-000-101"), DiscordUserID: strPtr("discord-1"),
		Reason: "griefing", IssuedByDiscordID: "discord-2", CreatedAt: time.Unix(1700000000, 0),
	}
	embed := moderation.BanEmbed(ban, 1, "Kicked and banned in game.")

	fields := map[string]string{}
	for _, f := range embed.Fields {
		fields[f.Name] = f.Value
	}
	if fields["Enforcement"] != "Kicked and banned in game." {
		t.Errorf("Enforcement = %q", fields["Enforcement"])
	}
	if fields["Expires"] != "permanent" {
		t.Errorf("Expires = %q, want permanent for a ban with no expiry", fields["Expires"])
	}
	if !strings.Contains(fields["Player"], "555-000-101") ||
		!strings.Contains(fields["Player"], "<@discord-1>") {
		t.Errorf("Player = %q, want both identities", fields["Player"])
	}

	expires := time.Unix(1700003600, 0)
	ban.ExpiresAt = &expires
	if got := moderation.BanEmbed(ban, 1, "x").Fields[2].Value; got != "<t:1700003600:R>" {
		t.Errorf("Expires = %q, want a Discord timestamp", got)
	}
}

// TestReasonFitsAnEmbedField: Discord refuses a field over 1024 characters
// OUTRIGHT, which would turn a long reason into a missing feed post rather than
// a truncated one.
func TestReasonFitsAnEmbedField(t *testing.T) {
	ban := gen.Ban{ID: 1, AlderonID: strPtr("555-000-101"), Reason: strings.Repeat("x", 5000)}
	for _, field := range moderation.BanEmbed(ban, 1, "").Fields {
		if len(field.Value) > 1024 {
			t.Errorf("field %q is %d bytes, over Discord's limit", field.Name, len(field.Value))
		}
	}
}

func TestBanReasonsSplitAdminFromPlayer(t *testing.T) {
	ban := gen.Ban{ID: 12, Reason: "harassment", IssuedByDiscordID: "discord-9"}
	admin, user := moderation.BanReasons(ban)
	if user != "harassment" {
		t.Errorf("user reason = %q, want the moderator's words -- it is what the player is shown", user)
	}
	if !strings.Contains(admin, "12") || !strings.Contains(admin, "discord-9") {
		t.Errorf("admin reason = %q, want it to name the record and the issuer", admin)
	}
	// bans.txt is colon-separated; a colon in either reason corrupts the row.
	if strings.Contains(admin, ":") {
		t.Errorf("admin reason %q contains the bans.txt field separator", admin)
	}
}

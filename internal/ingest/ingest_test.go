package ingest_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/USA-RedDragon/obsidibot/internal/ingest"
)

// documentedPayload is the PlayerKilled example from Alderon's webhook
// documentation, verbatim. Decoding this is the contract.
// livePayload is a real PvP kill as the SERVER sends it, which differs from the
// documented shape: DinosaurVictimName rather than VictimCharacterName, plus
// KillDistance, VictimRole and KillerRole that the docs do not mention.
// Identifiers are anonymised.
const livePayload = `{
    "TimeOfDay": 1411,
    "DamageType": "DT_ATTACK",
    "VictimPOI": "Green Valley",
    "VictimName": "testplayer",
    "VictimAlderonId": "555-000-101",
    "DinosaurVictimName": "lll",
    "VictimDinosaurType": "Miragaia",
    "VictimRole": "Owner",
    "VictimIsAdmin": true,
    "VictimGrowth": 1,
    "VictimLocation": "(X=105140.83,Y=162629.63,Z=-410.86)",
    "KillerName": "otherplayer",
    "KillerAlderonId": "555-000-202",
    "KillerCharacterName": "Thel",
    "KillerDinosaurType": "Allosaurus",
    "KillerRole": "",
    "KillerIsAdmin": false,
    "KillerGrowth": 1,
    "KillerLocation": "(X=105127.26,Y=163060.36,Z=-470.88)",
    "KillDistance": 435.103296,
    "ServerGuid": "63a86971-0cb9-4569-a43a-4b05317f2d73"
}`

const documentedPayload = `{
    "ServerGuid": "63a86971-0cb9-4569-a43a-4b05317f2d73",
    "TimeOfDay": 1300,
    "DamageType": "DT_ATTACK",
    "VictimPOI": "Talons Point",
    "VictimName": "Test1",
    "VictimAlderonId": "048-236-424",
    "VictimCharacterName": "DiloIsCool",
    "VictimDinosaurType": "Dilophosaurus",
    "VictimRole": "CoolRole",
    "VictimIsAdmin": false,
    "VictimGrowth": 0.5,
    "VictimLocation": "(X=328866.125,Y=-130023.359375,Z=853.25)",
    "KillerName": "Test2",
    "KillerAlderonId": "123-430-121",
    "KillerCharacterName": "DiloIsCooler",
    "KillerDinosaurType": "Dilophosaurus",
    "KillerRole": "NotAsCoolRole",
    "KillerIsAdmin": false,
    "KillerGrowth": 0.5,
    "KillerLocation": "(X=328866.125,Y=-130023.359375,Z=853.25)"
}`

func TestDecodesTheDocumentedPayload(t *testing.T) {
	var event ingest.PlayerKilled
	if err := json.Unmarshal([]byte(documentedPayload), &event); err != nil {
		t.Fatalf("the documented payload did not decode: %v", err)
	}
	if event.ServerGUID != "63a86971-0cb9-4569-a43a-4b05317f2d73" {
		t.Errorf("ServerGUID = %q", event.ServerGUID)
	}
	if event.VictimID() != "048-236-424" || event.KillerID() != "123-430-121" {
		t.Errorf("ids: victim %q killer %q", event.VictimID(), event.KillerID())
	}
	if event.VictimDinosaurType != "Dilophosaurus" || event.VictimGrowth != 0.5 {
		t.Errorf("victim dino %q growth %v", event.VictimDinosaurType, event.VictimGrowth)
	}
	if event.VictimPOI != "Talons Point" {
		t.Errorf("VictimPOI = %q", event.VictimPOI)
	}
	if !event.Credited() {
		t.Error("a plain DT_ATTACK kill between two non-admins was not credited")
	}
}

// TestPayloadCarriesEveryReportedField. The kill feed reproduces the game's own
// webhook in full, so anything the server sends must survive decoding --
// including the coordinates, which an earlier revision deliberately dropped.
// That rule was reversed: the feed describes a fight that already happened, and
// a partial account of it was worth less than the privacy it bought.
//
// Live position -- where somebody IS now, via RCON PlayerInfo -- is a separate
// question and is still never published; see internal/pot's package doc.
func TestPayloadCarriesEveryReportedField(t *testing.T) {
	var event ingest.PlayerKilled
	if err := json.Unmarshal([]byte(livePayload), &event); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// KillerRole is deliberately absent from this list: the real payload this
	// fixture reproduces carries it as an empty string, so "populated" is not
	// the contract for it -- only that the field exists to receive one.
	for name, got := range map[string]any{
		"VictimCharacterName": event.VictimCharacterName,
		"VictimRole":          event.VictimRole,
		"VictimLocation":      event.VictimLocation,
		"KillerCharacterName": event.KillerCharacterName,
		"KillerLocation":      event.KillerLocation,
		"KillDistance":        event.KillDistance,
		"TimeOfDay":           event.TimeOfDay,
	} {
		if got == "" || got == 0 || got == 0.0 {
			t.Errorf("%s did not decode (got %#v)", name, got)
		}
	}
}

// TestVictimCharacterComesFromDinosaurVictimName pins the one place the live
// server disagrees with Alderon's documentation.
//
// The docs name the victim's character `VictimCharacterName`; the server
// actually sends `DinosaurVictimName`, while the KILLER's side really is
// `KillerCharacterName`. Binding the documented name silently yielded an empty
// string for every victim, which is exactly the kind of bug a schema mismatch
// hides: nothing errors, a field is just always blank.
func TestVictimCharacterComesFromDinosaurVictimName(t *testing.T) {
	var event ingest.PlayerKilled
	if err := json.Unmarshal([]byte(livePayload), &event); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if event.VictimCharacterName != "lll" {
		t.Errorf("VictimCharacterName = %q, want the DinosaurVictimName value", event.VictimCharacterName)
	}
	// And the documented spelling must NOT be what populates it.
	if !strings.Contains(livePayload, "DinosaurVictimName") ||
		strings.Contains(livePayload, "VictimCharacterName") {
		t.Fatal("this fixture no longer exercises the mismatch it was written for")
	}
}

// TestCreditRules is the whole scoring policy in one table. Each row is a
// decision that was made deliberately, so each gets a case.
func TestCreditRules(t *testing.T) {
	tests := []struct {
		name  string
		event ingest.PlayerKilled
		want  bool
		why   string
	}{
		{
			name:  "a normal PvP kill",
			event: ingest.PlayerKilled{DamageType: "DT_ATTACK", KillerAlderonID: "111-111-111", VictimAlderonID: "222-222-222"},
			want:  true,
		},
		{
			name:  "starved to death",
			event: ingest.PlayerKilled{DamageType: "DT_HUNGER", VictimAlderonID: "222-222-222"},
			want:  false,
			why:   "nobody killed them, so there is no counterparty to take the points",
		},
		{
			name:  "died of thirst",
			event: ingest.PlayerKilled{DamageType: "DT_THIRST", VictimAlderonID: "222-222-222"},
			want:  false,
		},
		{
			name:  "bled out",
			event: ingest.PlayerKilled{DamageType: "DT_BLEED", VictimAlderonID: "222-222-222"},
			want:  false,
		},
		{
			name:  "broke its legs",
			event: ingest.PlayerKilled{DamageType: "DT_BREAKLEGS", VictimAlderonID: "222-222-222"},
			want:  false,
		},
		{
			name: "a self kill",
			event: ingest.PlayerKilled{
				DamageType: "DT_ATTACK", KillerAlderonID: "111-111-111", VictimAlderonID: "111-111-111",
			},
			want: false,
			why:  "otherwise anyone can mint rated games against themselves",
		},
		{
			name: "an admin's kill",
			event: ingest.PlayerKilled{
				DamageType: "DT_ATTACK", KillerAlderonID: "111-111-111",
				VictimAlderonID: "222-222-222", KillerIsAdmin: true,
			},
			want: true,
			why: "the game's admin kill function names NO killer, so an event that " +
				"names one is an admin playing the game like anybody else",
		},
		{
			name: "an admin being killed by a player",
			event: ingest.PlayerKilled{
				DamageType: "DT_ATTACK", KillerAlderonID: "111-111-111",
				VictimAlderonID: "222-222-222", VictimIsAdmin: true,
			},
			want: true,
			why:  "the rule is about who is credited, not who may be beaten",
		},
		{
			name: "attack damage with no killer recorded",
			event: ingest.PlayerKilled{
				DamageType: "DT_ATTACK", KillerAlderonID: "", VictimAlderonID: "222-222-222",
			},
			want: false,
		},
		{
			name: "whitespace-only killer id",
			event: ingest.PlayerKilled{
				DamageType: "DT_ATTACK", KillerAlderonID: "   ", VictimAlderonID: "222-222-222",
			},
			want: false,
			why:  "a blank id is absence, not an identity",
		},
		{
			name: "self kill with padded ids",
			event: ingest.PlayerKilled{
				DamageType: "DT_ATTACK", KillerAlderonID: " 111-111-111", VictimAlderonID: "111-111-111 ",
			},
			want: false,
			why:  "whitespace must not be a way around the self-kill rule",
		},
		{
			name: "died of thirst, as the live server actually reports it",
			event: ingest.PlayerKilled{
				DamageType: "DT_THIRST", KillerAlderonID: "111-111-111", VictimAlderonID: "111-111-111",
				KillerIsAdmin: true,
			},
			want: false,
			why: "the game names the VICTIM as their own killer for an environmental " +
				"death, which is the shape this rule has to recognise",
		},
		{
			name: "fell to their death, as the live server actually reports it",
			event: ingest.PlayerKilled{
				DamageType: "DT_IMPACT", KillerAlderonID: "111-111-111", VictimAlderonID: "111-111-111",
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.event.Credited(); got != tc.want {
				t.Errorf("Credited() = %v, want %v (%s)", got, tc.want, tc.why)
			}
		})
	}
}

// TestDedupeKeyIsStableAndSpecific: stable so a redelivery is recognised,
// specific so two genuinely different kills are not collapsed.
func TestDedupeKeyIsStableAndSpecific(t *testing.T) {
	body := []byte(documentedPayload)
	first := ingest.DedupeKey(body)
	if string(first) != string(ingest.DedupeKey(body)) {
		t.Fatal("the same body produced two different keys; redeliveries would be double counted")
	}
	if len(first) != 32 {
		t.Fatalf("key is %d bytes, want a 32-byte SHA-256", len(first))
	}

	// Any difference at all, including the coordinates the decoder throws
	// away, keeps two events distinct.
	other := strings.Replace(documentedPayload, "328866.125", "328866.126", 1)
	if string(ingest.DedupeKey([]byte(other))) == string(first) {
		t.Fatal("two different bodies produced the same key")
	}
}

// TestCountsDeathRules is the second half of the scoring policy, and it is
// deliberately NOT the same question as Credited.
//
// Three things are decided about one event: whether the feed shows it (always),
// whether Elo moves (only if credited), and whether it counts against K/D
// (this). They do not coincide -- an environmental death counts but is not
// credited, and an admin's kill is neither.
func TestCountsDeathRules(t *testing.T) {
	// Every shape the live server has actually produced, plus the two the old
	// rules wrongly excluded. All of them are a player dying, which is the
	// whole of the rule -- but they are listed rather than collapsed into one
	// assertion so that a future exclusion has to be argued shape by shape.
	tests := []struct {
		name  string
		event ingest.PlayerKilled
		why   string
	}{
		{
			name:  "a normal PvP kill",
			event: ingest.PlayerKilled{DamageType: "DT_ATTACK", KillerAlderonID: "111-111-111", VictimAlderonID: "222-222-222"},
		},
		{
			name:  "starved to death",
			event: ingest.PlayerKilled{DamageType: "DT_HUNGER", VictimAlderonID: "222-222-222"},
			why:   "surviving is part of playing, even though no rating moves",
		},
		{
			name:  "no killer at all, which is how DT_GENERIC arrives",
			event: ingest.PlayerKilled{DamageType: "DT_GENERIC", VictimAlderonID: "222-222-222"},
		},
		{
			name: "died of thirst, named as their own killer",
			event: ingest.PlayerKilled{
				DamageType: "DT_THIRST", KillerAlderonID: "111-111-111", VictimAlderonID: "111-111-111",
			},
			why: "the live server reports environmental deaths this way, and the old " +
				"self-kill exclusion silently discarded every one of them",
		},
		{
			name: "killed by an admin",
			event: ingest.PlayerKilled{
				DamageType: "DT_ATTACK", KillerAlderonID: "111-111-111",
				VictimAlderonID: "222-222-222", KillerIsAdmin: true,
			},
			why: "the admin kill function names no killer, so this is an ordinary death",
		},
		{
			name: "killed by a player with non-attack damage",
			event: ingest.PlayerKilled{
				DamageType: "DT_TRAMPLE", KillerAlderonID: "111-111-111", VictimAlderonID: "222-222-222",
			},
			why: "a player did it, in play; it just does not move Elo",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.event.CountsDeath() {
				t.Errorf("CountsDeath() = false, want true (%s)", tc.why)
			}
		})
	}
}

// TestCreditedAndCountsDeathAreIndependent guards against the two rules being
// quietly collapsed back into one.
func TestCreditedAndCountsDeathAreIndependent(t *testing.T) {
	// The two questions are still genuinely separate, and this is the shape
	// that proves it: the world killed them, so it is a death against their
	// record with no counterparty to take the rating points. Inventing one
	// would drain a zero-sum system and deflate every rating over time.
	//
	// It used to be demonstrated with an admin's kill, which is no longer a
	// divergence at all -- both answers are now true there.
	thirst := ingest.PlayerKilled{
		DamageType: "DT_THIRST", KillerAlderonID: "111-111-111", VictimAlderonID: "111-111-111",
	}
	if thirst.Credited() {
		t.Error("an environmental death credited a kill; the world has no rating to take points from")
	}
	if !thirst.CountsDeath() {
		t.Error("an environmental death did not count as a death; surviving is part of playing")
	}
}

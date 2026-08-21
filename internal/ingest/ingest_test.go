package ingest_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/USA-RedDragon/obsidibot/internal/ingest"
)

// documentedPayload is the PlayerKilled example from Alderon's webhook
// documentation, verbatim. Decoding this is the contract.
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

// TestPayloadCarriesNoCoordinates is the structural half of "obsidibot never
// publishes where a player is". The payload HAS locations; this type must not,
// so nothing downstream can render one by accident.
func TestPayloadCarriesNoCoordinates(t *testing.T) {
	var event ingest.PlayerKilled
	if err := json.Unmarshal([]byte(documentedPayload), &event); err != nil {
		t.Fatalf("decode: %v", err)
	}
	blob, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, forbidden := range []string{"Location", "X=", "Y=", "328866"} {
		if strings.Contains(string(blob), forbidden) {
			t.Errorf("the decoded event carries %q; coordinates must be dropped at the boundary", forbidden)
		}
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
			want: false,
			why:  "admins moderate and test; neither should move the board",
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
			want:  true,
			why:   "surviving is part of playing, even though no rating moves",
		},
		{
			name:  "drowned",
			event: ingest.PlayerKilled{DamageType: "DT_OXYGEN", VictimAlderonID: "222-222-222"},
			want:  true,
		},
		{
			name: "a self kill",
			event: ingest.PlayerKilled{
				DamageType: "DT_ATTACK", KillerAlderonID: "111-111-111", VictimAlderonID: "111-111-111",
			},
			want: false,
			why:  "it says nothing about how they play, and it is shown in the feed anyway",
		},
		{
			name: "killed by an admin",
			event: ingest.PlayerKilled{
				DamageType: "DT_ATTACK", KillerAlderonID: "111-111-111",
				VictimAlderonID: "222-222-222", KillerIsAdmin: true,
			},
			want: false,
			why:  "an admin moderating a fight should not dent the record of whoever they stop",
		},
		{
			name: "killed by a player with non-attack damage",
			event: ingest.PlayerKilled{
				DamageType: "DT_TRAMPLE", KillerAlderonID: "111-111-111", VictimAlderonID: "222-222-222",
			},
			want: true,
			why:  "a player did it, in play; it just does not move Elo",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.event.CountsDeath(); got != tc.want {
				t.Errorf("CountsDeath() = %v, want %v (%s)", got, tc.want, tc.why)
			}
		})
	}
}

// TestCreditedAndCountsDeathAreIndependent guards against the two rules being
// quietly collapsed back into one.
func TestCreditedAndCountsDeathAreIndependent(t *testing.T) {
	environmental := ingest.PlayerKilled{DamageType: "DT_THIRST", VictimAlderonID: "222-222-222"}
	if environmental.Credited() || !environmental.CountsDeath() {
		t.Error("an environmental death should count a death without being credited")
	}

	adminKill := ingest.PlayerKilled{
		DamageType: "DT_ATTACK", KillerAlderonID: "111-111-111",
		VictimAlderonID: "222-222-222", KillerIsAdmin: true,
	}
	if adminKill.Credited() || adminKill.CountsDeath() {
		t.Error("an admin's kill should be neither credited nor counted")
	}
}

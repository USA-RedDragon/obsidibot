// Package ingest receives the game's PlayerKilled webhook.
//
// # What this package is allowed to do
//
// Insert one row. That is all. Elo is ORDER-DEPENDENT -- the same kills applied
// in a different order produce different ratings -- so ratings are computed by
// a single writer walking kill_events in id order. If this endpoint ever
// computed a rating inline, several replicas receiving kills concurrently would
// each compute a different answer and overwrite each other.
//
// # Authentication
//
// Path of Titans signs nothing and sends no configurable headers, so the only
// credential available is a secret in the URL. That is why this listener is on
// its own port: the secret is the second line of defence, and not being
// reachable from the internet is the first.
package ingest

import (
	"crypto/sha256"
	"strings"
)

// DamageTypeAttack is the only damage type that credits a kill. Everything else
// the game reports -- DT_OXYGEN, DT_BLEED, DT_THIRST, DT_HUNGER, DT_BREAKLEGS,
// DT_GENERIC, DT_TRAMPLE, DT_SPIKES, DT_ARMORPIERCING -- is the world killing
// somebody, and the world does not have a rating to take points from.
const DamageTypeAttack = "DT_ATTACK"

// PlayerKilled is the game's kill webhook.
//
// # Locations are deliberately absent
//
// The payload carries VictimLocation and KillerLocation. Neither is declared
// here, so decoding drops them: obsidibot never publishes where a player is,
// and a field that does not exist cannot be rendered into a feed by someone who
// did not know the rule. The RAW payload is still stored, so a future rule
// change can be replayed against history -- but nothing reads coordinates out
// of it.
//
// VictimPOI is a named region rather than a coordinate and is kept, gated
// behind killfeed.showPOI, which defaults off.
type PlayerKilled struct {
	ServerGUID string `json:"ServerGuid"`
	DamageType string `json:"DamageType"`

	VictimName          string  `json:"VictimName"`
	VictimAlderonID     string  `json:"VictimAlderonId"`
	VictimCharacterName string  `json:"VictimCharacterName"`
	VictimDinosaurType  string  `json:"VictimDinosaurType"`
	VictimGrowth        float64 `json:"VictimGrowth"`
	VictimIsAdmin       bool    `json:"VictimIsAdmin"`
	VictimPOI           string  `json:"VictimPOI"`

	KillerName          string  `json:"KillerName"`
	KillerAlderonID     string  `json:"KillerAlderonId"`
	KillerCharacterName string  `json:"KillerCharacterName"`
	KillerDinosaurType  string  `json:"KillerDinosaurType"`
	KillerGrowth        float64 `json:"KillerGrowth"`
	KillerIsAdmin       bool    `json:"KillerIsAdmin"`
}

// Credited reports whether this event gives its killer a rated kill.
//
// The rules, and why each one is here:
//
//   - DamageType must be DT_ATTACK. Starving to death is a death, but nobody
//     killed you.
//   - There must be a killer. An environmental death names none.
//   - The killer must not be the victim. Self-kills would otherwise let anyone
//     mint rated games against themselves.
//   - The killer must not be an admin. Admins moderate and test; neither should
//     move the board.
//
// Whether a DEATH is recorded is a separate question -- see CountsDeath.
func (p PlayerKilled) Credited() bool {
	killer := strings.TrimSpace(p.KillerAlderonID)
	switch {
	case p.DamageType != DamageTypeAttack:
		return false
	case killer == "":
		return false
	case killer == strings.TrimSpace(p.VictimAlderonID):
		return false
	case p.KillerIsAdmin:
		return false
	default:
		return true
	}
}

// CountsDeath reports whether this event counts against the victim's K/D.
//
// It is deliberately NOT the same question as Credited, because three things
// are being decided about one event and they do not coincide:
//
//   - The kill feed shows every event. It happened; people want to see it.
//   - Elo moves only on a credited kill.
//   - K/D counts a death unless the death was somebody else's doing in a way
//     the victim could not play around.
//
// So: dying of thirst counts, because surviving is part of playing, even though
// it moves no rating -- there is no counterparty to take the points, and
// inventing one would drain a zero-sum system and deflate every rating over
// time. Being killed by an ADMIN does not count: an admin moderating a fight
// should not dent the record of whoever they stop. A SELF-KILL does not count
// either, or a player could farm their own K/D in reverse to no purpose and
// the number would stop meaning anything.
func (p PlayerKilled) CountsDeath() bool {
	killer := strings.TrimSpace(p.KillerAlderonID)
	switch {
	case killer == "":
		// Nobody killed them: the world did, and that counts.
		return true
	case killer == strings.TrimSpace(p.VictimAlderonID):
		return false
	case p.KillerIsAdmin:
		return false
	default:
		return true
	}
}

// KillerID returns the killer's Alderon ID, or "" when nobody killed them.
func (p PlayerKilled) KillerID() string {
	return strings.TrimSpace(p.KillerAlderonID)
}

// VictimID returns the victim's Alderon ID.
func (p PlayerKilled) VictimID() string {
	return strings.TrimSpace(p.VictimAlderonID)
}

// DedupeKey identifies a delivery.
//
// Path of Titans sends no event id, so this is a hash of the payload bytes. It
// protects against a redelivery at the cost of collapsing two byte-identical
// kills into one -- same killer, same victim, same species, same growth, same
// time of day, same float coordinates. That is vanishingly unlikely and the
// ingest metric counts every drop, so the trade is observable rather than
// silent.
func DedupeKey(body []byte) []byte {
	sum := sha256.Sum256(body)
	return sum[:]
}

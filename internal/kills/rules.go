package kills

import "github.com/USA-RedDragon/obsidibot/internal/ingest"

// creditsAKill answers the credit question for a STORED event.
//
// # Why this is not a read of event.Credited
//
// The flag on the row records what the rules said when the row was INSERTED.
// That is the wrong authority for two reasons.
//
// The first is deployment. Replicas roll one at a time, so for a minute or two
// an old binary is still ingesting under the old rules while a new one is
// already rating. A replay that trusted the stored flag would take those rows
// at their word and bake a superseded rule permanently into an
// order-dependent Elo chain, where nothing would ever detect it.
//
// The second is that a rule change is only ever half-applied if history keeps
// its old answers. Deriving here means the current rule decides every event,
// every time it is rated, with no migration required to agree with the Go
// code -- an agreement that is otherwise re-litigated in SQL and can silently
// drift.
//
// The columns this needs are ones both workers already select, so deriving
// costs nothing. The stored flag survives as a record of what the bot believed
// at the time, which is worth having and worth not trusting.
func creditsAKill(damageType string, killerAGID *string, victimAGID string) bool {
	killer := ""
	if killerAGID != nil {
		killer = *killerAGID
	}
	// Delegated rather than restated: one rule, one implementation. A copy
	// here would be a second place to forget to change.
	return ingest.PlayerKilled{
		DamageType:      damageType,
		KillerAlderonID: killer,
		VictimAlderonID: victimAGID,
	}.Credited()
}

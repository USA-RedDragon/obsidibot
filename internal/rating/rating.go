// Package rating implements the Elo system the leaderboard is ordered by.
//
// Everything here is a pure function of its arguments. That is deliberate: the
// numbers are the thing players argue about, so they must be checkable in a
// table test rather than only observable by replaying a season of kills.
//
// # Why Elo rather than K/D
//
// A PlayerKilled event names BOTH parties. Every count-based metric throws the
// opponent away and asks "how many"; Elo asks "who did you beat", which is the
// only question the data can actually answer well.
//
// Two consequences fall out of that and both are load-bearing:
//
//   - Farming stops paying. At 1600, killing a 900-rated hatchling is worth
//     about 0.4 points; killing a 1600 is worth about 16. Grinding easy kills
//     asymptotes to nothing, which is exactly the exploit that defeats "K/D
//     with a minimum-kills floor".
//   - Collusion is bounded. Two players trading kills oscillate around their
//     starting ratings and net out near zero, because a win and a loss between
//     the same pair very nearly cancel. No ratio metric survives that attack.
//
// # What it does NOT fix
//
// Rating partly measures what you play. A Rex eating a Dilo is not skill, and
// Elo only corrects for it indirectly -- Dilo mains carry lower ratings, so
// eating one is worth less. Species and growth are deliberately not weighted:
// that would be a second, unfalsifiable model layered on this one.
package rating

import (
	"math"

	"github.com/USA-RedDragon/obsidibot/internal/config"
)

// scale is Elo's classic 400: a player 400 points above another is expected to
// win about ten times out of eleven.
const scale = 400.0

// Expected returns the probability that a player rated a beats one rated b.
func Expected(a, b float64) float64 {
	return 1 / (1 + math.Pow(10, (b-a)/scale))
}

// K returns the sensitivity of a rating with ratedGames behind it.
//
// It steps down rather than sliding, because the boundaries are meant to be
// explainable: a new player moves fast enough to find their level in an
// evening, and an established rating stops lurching over one lucky fight.
func K(ratedGames int32, cfg config.Rating) float64 {
	switch {
	case int(ratedGames) < cfg.ProvisionalGames:
		return float64(cfg.ProvisionalK)
	case int(ratedGames) < cfg.SettlingGames:
		return float64(cfg.SettlingK)
	default:
		return float64(cfg.StableK)
	}
}

// Outcome is the pair of new ratings after one kill.
type Outcome struct {
	Killer float64
	Victim float64
}

// Apply computes both ratings after killer beats victim.
//
// The two sides can carry different K factors, so the exchange is not exactly
// zero-sum -- a newcomer beating a veteran gains more than the veteran loses.
// That is standard Elo practice and it is the price of provisional ratings
// converging quickly; the drift is small and bounded by the K spread.
func Apply(killerRating, victimRating float64, killerGames, victimGames int32, cfg config.Rating) Outcome {
	expected := Expected(killerRating, victimRating)
	// The surprise in the result. Both sides move by this same fraction of
	// their own K, which is what makes the exchange zero-sum at equal K:
	// the killer scored 1 where they were expected to score `expected`, and
	// the victim scored 0 where they were expected to score `1 - expected`.
	//
	// Using `expected` for the victim instead of its complement inverts the
	// whole system -- a hatchling farmed by a 1600 would lose ~15.8 points
	// rather than ~0.16, and two players trading kills would inflate each
	// other indefinitely. TestCollusionIsBounded is what catches that.
	surprise := 1 - expected
	return Outcome{
		Killer: killerRating + K(killerGames, cfg)*surprise,
		Victim: victimRating - K(victimGames, cfg)*surprise,
	}
}

// Decay pulls an idle rating back toward the starting value.
//
// days is how many days of decay to APPLY, which the caller derives from how
// long the rating has been idle past the grace period. Passing the number of
// steps rather than a pair of timestamps is what makes the job resumable: a
// missed run simply applies more steps next time, and running twice in one day
// applies zero.
//
// Decay only ever moves a rating DOWNWARD toward the baseline. Pulling a
// below-average rating up would reward not playing, which is the opposite of
// what an inactivity rule is for.
func Decay(rating float64, days int, cfg config.Rating) float64 {
	initial := float64(cfg.Initial)
	if rating <= initial || days <= 0 || cfg.DecayPermillePerDay <= 0 {
		return rating
	}

	// Geometric rather than linear, so a rating approaches the baseline and
	// never overshoots below it however long someone stays away.
	retained := math.Pow(1-float64(cfg.DecayPermillePerDay)/1000, float64(days))
	decayed := initial + (rating-initial)*retained
	if decayed < initial {
		return initial
	}
	return decayed
}

package rating_test

import (
	"math"
	"testing"

	"github.com/USA-RedDragon/obsidibot/internal/config"
	"github.com/USA-RedDragon/obsidibot/internal/rating"
)

func cfg() config.Rating {
	return config.Rating{
		Initial: 1200, ProvisionalK: 40, SettlingK: 20, StableK: 16,
		ProvisionalGames: 20, SettlingGames: 50,
		DecayGraceDays: 30, DecayPermillePerDay: 5,
	}
}

func closeTo(got, want, tolerance float64) bool {
	return math.Abs(got-want) <= tolerance
}

func TestExpected(t *testing.T) {
	if got := rating.Expected(1200, 1200); !closeTo(got, 0.5, 1e-9) {
		t.Errorf("equal ratings expect %v, want 0.5", got)
	}
	// The defining property of the 400 scale.
	if got := rating.Expected(1600, 1200); !closeTo(got, 10.0/11.0, 1e-3) {
		t.Errorf("400 points ahead expects %v, want ~0.909", got)
	}
	if got := rating.Expected(1200, 1600); !closeTo(got, 1.0/11.0, 1e-3) {
		t.Errorf("400 points behind expects %v, want ~0.091", got)
	}
	// Expectations are complementary, always.
	for _, pair := range [][2]float64{{1200, 1200}, {1600, 900}, {800, 2400}} {
		sum := rating.Expected(pair[0], pair[1]) + rating.Expected(pair[1], pair[0])
		if !closeTo(sum, 1, 1e-9) {
			t.Errorf("Expected(%v,%v) + its complement = %v, want 1", pair[0], pair[1], sum)
		}
	}
}

func TestKStepsDownWithExperience(t *testing.T) {
	c := cfg()
	tests := []struct {
		games int32
		want  float64
	}{
		{0, 40}, {19, 40}, {20, 20}, {49, 20}, {50, 16}, {5000, 16},
	}
	for _, tc := range tests {
		if got := rating.K(tc.games, c); got != tc.want {
			t.Errorf("K(%d) = %v, want %v", tc.games, got, tc.want)
		}
	}
}

// TestFarmingStopsPaying is the property that made Elo the right choice. The
// exploit that beats every ratio metric -- grind easy kills, then stop -- has
// to asymptote to nothing here.
func TestFarmingStopsPaying(t *testing.T) {
	c := cfg()
	const established int32 = 100 // past SettlingGames, so K = 16

	farm := rating.Apply(1600, 900, established, established, c)
	gainFromHatchling := farm.Killer - 1600

	fair := rating.Apply(1600, 1600, established, established, c)
	gainFromPeer := fair.Killer - 1600

	if gainFromHatchling >= 1 {
		t.Errorf("killing a 900 from 1600 gained %.2f points; farming still pays", gainFromHatchling)
	}
	if gainFromPeer <= 7 {
		t.Errorf("killing a peer gained only %.2f points", gainFromPeer)
	}
	if gainFromPeer/gainFromHatchling < 10 {
		t.Errorf("a fair fight is only %.1fx a farm; the gradient is too flat",
			gainFromPeer/gainFromHatchling)
	}

	// And it keeps getting worse as the gap widens: no amount of hatchling
	// hunting climbs the board.
	total := 1600.0
	for range 200 {
		total = rating.Apply(total, 900, established, established, c).Killer
	}
	if total > 1900 {
		t.Errorf("200 hatchling kills took a rating from 1600 to %.0f; farming is still viable", total)
	}
}

// TestCollusionIsBounded covers the attack no ratio metric survives: two
// players trading kills back and forth. Elo is very nearly zero-sum between a
// pair, so it must net out near zero rather than lifting them both.
func TestCollusionIsBounded(t *testing.T) {
	c := cfg()
	a, b := 1200.0, 1200.0
	// Well past provisional, so both carry the same stable K.
	var games int32 = 100

	for range 500 {
		out := rating.Apply(a, b, games, games, c)
		a, b = out.Killer, out.Victim
		out = rating.Apply(b, a, games, games, c)
		b, a = out.Killer, out.Victim
	}

	if !closeTo(a, 1200, 5) || !closeTo(b, 1200, 5) {
		t.Errorf("after 500 traded kills each: a=%.1f b=%.1f, want both near 1200", a, b)
	}
	if a+b > 2410 {
		t.Errorf("collusion inflated the pair to %.0f total, want ~2400", a+b)
	}
}

// TestBeatingAStrongerPlayerPaysMore is the ordering property the whole system
// rests on.
func TestBeatingAStrongerPlayerPaysMore(t *testing.T) {
	c := cfg()
	var games int32 = 100
	previous := math.Inf(1)
	for _, victim := range []float64{2000, 1600, 1400, 1200, 1000, 800} {
		gain := rating.Apply(1200, victim, games, games, c).Killer - 1200
		if gain >= previous {
			t.Errorf("beating a %.0f gained %.2f, which is not less than beating a stronger player (%.2f)",
				victim, gain, previous)
		}
		if gain <= 0 {
			t.Errorf("beating a %.0f lost the killer rating (%.2f)", victim, gain)
		}
		previous = gain
	}
}

// TestVictimAlwaysLoses: a death must never raise a rating, whoever it was to.
func TestVictimAlwaysLoses(t *testing.T) {
	c := cfg()
	for _, killer := range []float64{400, 1200, 2400} {
		for _, victim := range []float64{400, 1200, 2400} {
			out := rating.Apply(killer, victim, 100, 100, c)
			if out.Victim >= victim {
				t.Errorf("victim rated %.0f killed by %.0f went to %.2f", victim, killer, out.Victim)
			}
			if out.Killer <= killer {
				t.Errorf("killer rated %.0f beating %.0f went to %.2f", killer, victim, out.Killer)
			}
		}
	}
}

// TestProvisionalRatingsMoveFaster is what stops a new player grinding up from
// the baseline for a week before the board says anything true about them.
func TestProvisionalRatingsMoveFaster(t *testing.T) {
	c := cfg()
	newcomer := rating.Apply(1200, 1200, 0, 100, c).Killer - 1200
	veteran := rating.Apply(1200, 1200, 100, 100, c).Killer - 1200
	if newcomer <= veteran {
		t.Errorf("a newcomer gained %.2f and a veteran %.2f; provisional K is not doing anything",
			newcomer, veteran)
	}
	if !closeTo(newcomer, 20, 0.01) { // K=40 * (1 - 0.5)
		t.Errorf("newcomer gain = %.2f, want 20", newcomer)
	}
	if !closeTo(veteran, 8, 0.01) { // K=16 * (1 - 0.5)
		t.Errorf("veteran gain = %.2f, want 8", veteran)
	}
}

func TestDecay(t *testing.T) {
	c := cfg()

	t.Run("never raises a below-average rating", func(t *testing.T) {
		// Decaying upward would REWARD not playing, which is the opposite of
		// what an inactivity rule exists to do.
		for _, r := range []float64{1199, 900, 400} {
			if got := rating.Decay(r, 365, c); got != r {
				t.Errorf("Decay(%v, 365 days) = %v; a low rating must not drift up", r, got)
			}
		}
		if got := rating.Decay(1200, 365, c); got != 1200 {
			t.Errorf("Decay at the baseline moved to %v", got)
		}
	})

	t.Run("pulls a high rating toward the baseline", func(t *testing.T) {
		got := rating.Decay(1800, 30, c)
		if got >= 1800 || got <= 1200 {
			t.Errorf("Decay(1800, 30) = %v, want between 1200 and 1800", got)
		}
		// 0.995^30 ~ 0.8604 of the 600-point gap.
		if !closeTo(got, 1200+600*math.Pow(0.995, 30), 0.01) {
			t.Errorf("Decay(1800, 30) = %v, not the documented geometric curve", got)
		}
	})

	t.Run("never overshoots below the baseline", func(t *testing.T) {
		// Geometric decay approaches the baseline; a linear one would sail past
		// it and turn a long absence into a punishment without a floor.
		for _, days := range []int{100, 1000, 100000} {
			if got := rating.Decay(2400, days, c); got < 1200 {
				t.Errorf("Decay(2400, %d days) = %v, below the baseline", days, got)
			}
		}
	})

	t.Run("is resumable", func(t *testing.T) {
		// A missed run applies more steps next time; the job must not depend on
		// having run every day.
		oneGo := rating.Decay(1800, 10, c)
		inSteps := 1800.0
		for range 10 {
			inSteps = rating.Decay(inSteps, 1, c)
		}
		if !closeTo(oneGo, inSteps, 1e-6) {
			t.Errorf("ten days at once = %v but one day ten times = %v", oneGo, inSteps)
		}
	})

	t.Run("zero or negative days is a no-op", func(t *testing.T) {
		for _, days := range []int{0, -1, -100} {
			if got := rating.Decay(1800, days, c); got != 1800 {
				t.Errorf("Decay(1800, %d) = %v, want 1800", days, got)
			}
		}
	})

	t.Run("disabled when the rate is zero", func(t *testing.T) {
		off := cfg()
		off.DecayPermillePerDay = 0
		if got := rating.Decay(1800, 1000, off); got != 1800 {
			t.Errorf("decay ran with the rate set to zero: %v", got)
		}
	})
}

// TestRatingsStayFinite guards the arithmetic against the extremes a long-lived
// server will eventually produce.
func TestRatingsStayFinite(t *testing.T) {
	c := cfg()
	high, low := 1200.0, 1200.0
	for range 10000 {
		out := rating.Apply(high, low, 100, 100, c)
		high, low = out.Killer, out.Victim
	}
	if math.IsNaN(high) || math.IsInf(high, 0) || math.IsNaN(low) || math.IsInf(low, 0) {
		t.Fatalf("ratings left the reals after 10000 one-sided kills: %v / %v", high, low)
	}
	// One player beating the same victim forever converges rather than running
	// away: the expected score approaches 1, so the gain approaches 0.
	if high > 2400 {
		t.Errorf("10000 kills against one opponent reached %.0f; the gradient is not flattening", high)
	}
}

// TestExchangeIsZeroSumAtEqualK pins the arithmetic that TestCollusionIsBounded
// only observes indirectly. Whatever the ratings, at equal K the killer's gain
// must be exactly the victim's loss.
//
// This is the regression test for a real bug: the victim's loss was computed
// from the KILLER's expected score rather than their own, which inverted the
// system -- a hatchling farmed by a 1600 lost ~15.8 points instead of ~0.16,
// and every pair of players trading kills inflated each other.
func TestExchangeIsZeroSumAtEqualK(t *testing.T) {
	c := cfg()
	var games int32 = 100 // past SettlingGames, so both carry StableK

	for _, pair := range [][2]float64{
		{1200, 1200}, {1600, 900}, {900, 1600}, {2400, 400}, {400, 2400}, {1503, 1497},
	} {
		killer, victim := pair[0], pair[1]
		out := rating.Apply(killer, victim, games, games, c)
		gain := out.Killer - killer
		loss := victim - out.Victim
		if !closeTo(gain, loss, 1e-9) {
			t.Errorf("killer %.0f beat victim %.0f: gained %.6f but victim lost %.6f",
				killer, victim, gain, loss)
		}
		if !closeTo(out.Killer+out.Victim, killer+victim, 1e-9) {
			t.Errorf("killer %.0f beat victim %.0f: total moved from %.6f to %.6f",
				killer, victim, killer+victim, out.Killer+out.Victim)
		}
	}
}

// TestFarmingBarelyHurtsTheVictim is the other half of the anti-farming
// property, and the half the inverted formula got backwards. Being repeatedly
// killed by someone far stronger must cost almost nothing -- otherwise a
// newcomer's rating is destroyed by the very players they cannot avoid.
func TestFarmingBarelyHurtsTheVictim(t *testing.T) {
	c := cfg()
	var games int32 = 100

	out := rating.Apply(1600, 900, games, games, c)
	loss := 900 - out.Victim
	if loss >= 1 {
		t.Errorf("a 900 killed by a 1600 lost %.2f points; being farmed should cost almost nothing", loss)
	}

	// Even sustained farming leaves the victim near where they started.
	victim := 900.0
	for range 100 {
		victim = rating.Apply(1600, victim, games, games, c).Victim
	}
	if victim < 850 {
		t.Errorf("100 deaths to a far stronger player took a 900 down to %.0f", victim)
	}
}

package kills_test

import (
	"context"
	"strings"
	"testing"
)

// TestEnvironmentalDeathsReadAsDeaths.
//
// The live server names the VICTIM as their own killer for thirst, falls and
// impacts. Keying the environmental wording on "no killer" therefore never
// fired, and the feed said "kittykat95 killed kittykat95 - unranked" over a
// player who had died of thirst.
func TestEnvironmentalDeathsReadAsDeaths(t *testing.T) {
	for damageType, phrase := range map[string]string{
		"DT_THIRST":    "died of thirst",
		"DT_IMPACT":    "died from a fall",
		"DT_BREAKLEGS": "died from a fall",
		"DT_HUNGER":    "starved",
	} {
		t.Run(damageType, func(t *testing.T) {
			h := newHarness(t)
			h.setKillChannel(t)
			// Named as their own killer, exactly as the game sends it.
			h.enqueue(t, bob, bob, damageType, false)
			h.runApplier(t)

			poster := &fakePoster{}
			h.runFeed(t, poster)

			got := poster.descriptions()
			if len(got) != 1 {
				t.Fatalf("%d messages, want 1", len(got))
			}
			if !strings.Contains(got[0], phrase) {
				t.Errorf("message does not say %q: %q", phrase, got[0])
			}
			if strings.Contains(got[0], "killed") {
				t.Errorf("the world's kill was rendered as a player kill: %q", got[0])
			}
			// The killer column is the victim again, which under a title
			// saying they died of thirst is nonsense.
			if strings.Contains(got[0], "Killer") {
				t.Errorf("an environmental death carried a Killer field: %q", got[0])
			}
			// kill_distance is 0 on these rows -- not the -1 the ingest
			// endpoint drops -- so it would otherwise report "0.0 m".
			if strings.Contains(got[0], "0.0 m") {
				t.Errorf("an environmental death reported a kill distance: %q", got[0])
			}
		})
	}
}

// TestFeedShowsWhatTheKillDidToBothRatings: the number on the leaderboard is
// only arguable if the events that produced it show their working.
func TestFeedShowsWhatTheKillDidToBothRatings(t *testing.T) {
	h := newHarness(t)
	h.setKillChannel(t)
	h.enqueue(t, alice, bob, "DT_ATTACK", false)
	h.runApplier(t)

	poster := &fakePoster{}
	h.runFeed(t, poster)

	got := poster.descriptions()
	if len(got) != 1 {
		t.Fatalf("%d messages, want 1", len(got))
	}
	// The killer gained and the victim lost, from the same starting rating.
	if !strings.Contains(got[0], "1200.0 →") {
		t.Errorf("no before-and-after rating in the message: %q", got[0])
	}
	if !strings.Contains(got[0], "(+") {
		t.Errorf("the killer's gain is not shown: %q", got[0])
	}
	if !strings.Contains(got[0], "(−") {
		t.Errorf("the victim's loss is not shown: %q", got[0])
	}
}

// TestUnratedEventsShowNoRatingFigures: a "+0.0" would read as a result of
// zero rather than as "this moved nothing".
func TestUnratedEventsShowNoRatingFigures(t *testing.T) {
	h := newHarness(t)
	h.setKillChannel(t)
	h.enqueue(t, "", bob, "DT_GENERIC", false)
	h.runApplier(t)

	poster := &fakePoster{}
	h.runFeed(t, poster)

	got := poster.descriptions()
	if len(got) != 1 {
		t.Fatalf("%d messages, want 1", len(got))
	}
	if strings.Contains(got[0], "→") {
		t.Errorf("an unrated event reported a rating change: %q", got[0])
	}
}

// TestPrunerKeepsTheEventAndDropsThePayload.
//
// The row is what /stats reads and what a replay walks; only the raw webhook
// bytes age out. Deleting the row used to cost both, silently.
func TestPrunerKeepsTheEventAndDropsThePayload(t *testing.T) {
	h := newHarness(t)
	h.setKillChannel(t)
	h.enqueue(t, alice, bob, "DT_ATTACK", false)
	h.runApplier(t)
	h.runFeed(t, &fakePoster{})

	ctx := context.Background()
	// Age it past the retention window.
	if _, err := h.pool.Exec(ctx,
		"update kill_events set received_at = now() - interval '90 days'"); err != nil {
		t.Fatalf("age event: %v", err)
	}

	//nolint:gosec // config validation bounds retentionDays to 1..3650
	rows, err := h.store.Queries().PruneProcessedEvents(ctx, int32(h.cfg.KillFeed.RetentionDays))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d rows pruned, want 1", rows)
	}

	var events, payloads int
	if err := h.pool.QueryRow(ctx,
		"select count(*), count(payload) from kill_events").Scan(&events, &payloads); err != nil {
		t.Fatalf("count: %v", err)
	}
	if events != 1 {
		t.Errorf("%d events survived pruning, want 1 -- the history must be kept", events)
	}
	if payloads != 0 {
		t.Errorf("%d payloads survived pruning, want 0", payloads)
	}

	// And the event is still replayable, because the credit rule reads columns
	// rather than the payload.
	h.requestReplay(t, "after pruning")
	h.runApplier(t)
	if killer := h.player(t, alice); killer.Kills != 1 {
		t.Errorf("%d kills after replaying a payload-less event, want 1", killer.Kills)
	}
}

// TestInWorldClockNeverRendersAnImpossibleTime.
//
// TimeOfDay is hundredths of an hour, not HHMM. Read the wrong way, 1779 came
// out as "17:79" -- and 14 of the first 46 live events had a minutes field of
// 60 or more.
func TestInWorldClockNeverRendersAnImpossibleTime(t *testing.T) {
	h := newHarness(t)
	h.setKillChannel(t)
	h.enqueue(t, alice, bob, "DT_ATTACK", false)
	if _, err := h.pool.Exec(context.Background(),
		"update kill_events set time_of_day = 1779"); err != nil {
		t.Fatalf("set clock: %v", err)
	}
	h.runApplier(t)

	poster := &fakePoster{}
	h.runFeed(t, poster)

	got := poster.descriptions()
	if len(got) != 1 {
		t.Fatalf("%d messages, want 1", len(got))
	}
	if strings.Contains(got[0], "17:79") {
		t.Errorf("an impossible in-world time was rendered: %q", got[0])
	}
	// 17.79 hours is 17:47.
	if !strings.Contains(got[0], "17:47") {
		t.Errorf("in-world time not rendered as 17:47: %q", got[0])
	}
}

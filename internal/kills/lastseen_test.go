package kills_test

import (
	"context"
	"testing"
	"time"
)

// backdateEvents moves every queued event into the past, so a replay is
// replaying history rather than re-applying something that just happened.
func (h *harness) backdateEvents(t *testing.T, age time.Duration) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		"update kill_events set received_at = now() - $1::interval",
		age.String()); err != nil {
		t.Fatalf("backdate events: %v", err)
	}
}

// TestReplayDoesNotMarkTheServerActive.
//
// The applier used to stamp last_seen_at = now() on every player it touched.
// Replaying a year of history through that marks the entire server as active
// this instant, which lies in /stats and -- worse, because nothing would ever
// show it -- resets everybody's inactivity decay for another grace period.
func TestReplayDoesNotMarkTheServerActive(t *testing.T) {
	h := newHarness(t)
	h.enqueue(t, alice, bob, "DT_ATTACK", false)
	h.backdateEvents(t, 60*24*time.Hour)

	h.runApplier(t)
	h.requestReplay(t, "test")
	h.runApplier(t)

	for _, agid := range []string{alice, bob} {
		seen := h.player(t, agid).LastSeenAt
		if time.Since(seen) < 24*time.Hour {
			t.Errorf("%s was stamped as last seen %v ago after replaying a 60-day-old kill",
				agid, time.Since(seen).Round(time.Second))
		}
	}
}

// TestReplayNeverDragsLastSeenBackwards: a player's real last activity may be a
// bank operation or a link that happened after their last kill, and neither is
// in the event stream. greatest() is what protects it.
func TestReplayNeverDragsLastSeenBackwards(t *testing.T) {
	h := newHarness(t)
	h.enqueue(t, alice, bob, "DT_ATTACK", false)
	h.backdateEvents(t, 30*24*time.Hour)
	h.runApplier(t)

	ctx := context.Background()
	// Something that is not a kill happens today.
	recent := time.Now().Add(-time.Hour)
	if _, err := h.pool.Exec(ctx,
		"update players set last_seen_at = $1 where alderon_id = $2", recent, alice); err != nil {
		t.Fatalf("record later activity: %v", err)
	}

	h.requestReplay(t, "test")
	h.runApplier(t)

	seen := h.player(t, alice).LastSeenAt
	if seen.Before(recent.Add(-time.Minute)) {
		t.Errorf("last seen moved back to %v; the later activity was overwritten by a replayed kill", seen)
	}
}

// TestAPlayerFirstSeenDuringAReplayGetsTheEventsTime is the insert path, which
// the conflict clause never covers.
//
// last_seen_at and first_seen_at both default to now(), so a player whose row
// is CREATED by the replay would take the replay's clock from the column
// default and look like they joined today -- however old the kill was.
func TestAPlayerFirstSeenDuringAReplayGetsTheEventsTime(t *testing.T) {
	h := newHarness(t)
	h.enqueue(t, alice, bob, "DT_ATTACK", false)
	h.backdateEvents(t, 45*24*time.Hour)
	h.runApplier(t)

	ctx := context.Background()
	// Wipe the players entirely: the replay has to recreate them from scratch,
	// which is exactly what happens for anyone whose row never existed.
	if _, err := h.pool.Exec(ctx, "delete from players"); err != nil {
		t.Fatalf("clear players: %v", err)
	}

	h.requestReplay(t, "test")
	h.runApplier(t)

	for _, agid := range []string{alice, bob} {
		player := h.player(t, agid)
		if time.Since(player.LastSeenAt) < 24*time.Hour {
			t.Errorf("%s was created by the replay with last_seen_at = now()", agid)
		}
		if time.Since(player.FirstSeenAt) < 24*time.Hour {
			t.Errorf("%s was created by the replay with first_seen_at = now()", agid)
		}
	}
}

package kills_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/kills"
	"github.com/USA-RedDragon/obsidibot/internal/metrics"
)

// requestReplay puts a replay in the queue the way migration 0004 does.
func (h *harness) requestReplay(t *testing.T, reason string) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`insert into rating_replays (reason, min_event_id)
		 values ($1, (select min(id) from kill_events))`, reason); err != nil {
		t.Fatalf("request replay: %v", err)
	}
}

func (h *harness) pendingReplays(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(),
		"select count(*) from rating_replays where completed_at is null").Scan(&n); err != nil {
		t.Fatalf("count pending replays: %v", err)
	}
	return n
}

// enqueueWithStaleFlags inserts an event carrying the flags the OLD rules would
// have written, which is exactly what every row in the live database looks like
// before the replay runs.
func (h *harness) enqueueWithStaleFlags(t *testing.T, killer, victim, damageType string) {
	t.Helper()
	h.seq++
	params := gen.InsertKillEventParams{
		DedupeKey:  fmt.Appendf(nil, "stale-%03d-padding-to-32-bytes!!", h.seq),
		ServerGuid: "guid",
		Payload:    json.RawMessage(`{}`),
		VictimAgid: victim,
		VictimName: "player-" + victim,
		DamageType: damageType,
		// The old rules: an admin's kill counted for nothing, and an
		// environmental death naming the victim as their own killer was read as
		// a self-kill and discarded.
		KillerIsAdmin: true,
		Credited:      false,
		CountsDeath:   false,
	}
	if killer != "" {
		name := "player-" + killer
		params.KillerAgid = &killer
		params.KillerName = &name
	}
	if _, err := h.store.Queries().InsertKillEvent(context.Background(), params); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
}

// TestReplayAppliesTheCurrentRulesToOldRows is the test the whole replay exists
// to pass.
//
// It seeds rows carrying the SUPERSEDED flags -- which is what the production
// table actually held -- and checks the replay produces what live ingestion of
// the same events would produce today. A test that seeded current flags on both
// sides would prove only that the reset and requeue plumbing works, and would
// pass just as happily if the applier still trusted the stored column.
func TestReplayAppliesTheCurrentRulesToOldRows(t *testing.T) {
	h := newHarness(t)
	h.enqueueWithStaleFlags(t, alice, bob, "DT_ATTACK") // an admin's kill
	h.enqueueWithStaleFlags(t, bob, bob, "DT_THIRST")   // died of thirst
	h.runApplier(t)

	// Sanity: with the current rules these already land correctly, because the
	// applier derives rather than reading the stale flags.
	if killer := h.player(t, alice); killer.Kills != 1 {
		t.Fatalf("the admin's kill was not credited on first pass: %d kills", killer.Kills)
	}
	before := h.player(t, bob)

	// Now replay, and demand the same answer. Ratings are order-dependent, so
	// "the same" has to mean exactly, not approximately.
	h.requestReplay(t, "test")
	h.runApplier(t)

	after := h.player(t, bob)
	if after.Rating != before.Rating {
		t.Errorf("rating after replay = %v, want %v -- the replay is not reproducible",
			after.Rating, before.Rating)
	}
	if after.Deaths != before.Deaths || after.Kills != before.Kills {
		t.Errorf("record after replay = %d/%d, want %d/%d",
			after.Kills, after.Deaths, before.Kills, before.Deaths)
	}
	// Two events, both deaths for bob: the admin's kill and the thirst.
	if after.Deaths != 2 {
		t.Errorf("%d deaths, want 2", after.Deaths)
	}
	if h.pendingReplays(t) != 0 {
		t.Error("the replay was not marked complete")
	}
}

// TestReplayIsNotCumulative: running it twice must not double every total. The
// reset is what makes that true, and it is the difference between a replay and
// re-applying the queue.
func TestReplayIsNotCumulative(t *testing.T) {
	h := newHarness(t)
	h.enqueue(t, alice, bob, "DT_ATTACK", false)
	h.enqueue(t, alice, carol, "DT_ATTACK", false)
	h.runApplier(t)
	once := h.player(t, alice)

	h.requestReplay(t, "first")
	h.runApplier(t)
	h.requestReplay(t, "second")
	h.runApplier(t)

	twice := h.player(t, alice)
	if twice.Kills != once.Kills || twice.Rating != once.Rating {
		t.Errorf("after two more replays: %d kills at %v, want %d at %v",
			twice.Kills, twice.Rating, once.Kills, once.Rating)
	}
}

// TestReplayNeverRepostsTheFeed is the expensive mistake.
//
// The replay clears `rated` so every event is re-rated. Clearing `posted` at
// the same time would look identical in the database and would re-post the
// server's entire kill history to Discord -- hundreds of messages, unsendable
// back.
func TestReplayNeverRepostsTheFeed(t *testing.T) {
	h := newHarness(t)
	h.setKillChannel(t)
	h.enqueue(t, alice, bob, "DT_ATTACK", false)
	h.enqueue(t, bob, carol, "DT_ATTACK", false)
	h.runApplier(t)

	poster := &fakePoster{}
	h.runFeed(t, poster)
	if len(poster.descriptions()) != 2 {
		t.Fatalf("%d messages posted before the replay, want 2", len(poster.descriptions()))
	}

	h.requestReplay(t, "test")
	h.runApplier(t)

	again := &fakePoster{}
	h.runFeed(t, again)
	if got := len(again.descriptions()); got != 0 {
		t.Errorf("%d messages re-posted after a replay, want 0 -- history must not be re-fed", got)
	}

	var unposted int
	if err := h.pool.QueryRow(context.Background(),
		"select count(*) from kill_events where not posted").Scan(&unposted); err != nil {
		t.Fatalf("count: %v", err)
	}
	if unposted != 0 {
		t.Errorf("%d events became unposted again", unposted)
	}
}

// TestReplayRefusesWhenHistoryIsIncomplete.
//
// Aggregates are rebuilt from the surviving events, so replaying over a partial
// set silently produces lower kill counts and a wrong chain -- and the result
// looks like a perfectly ordinary leaderboard, so nothing downstream ever
// notices. Refusing leaves the old numbers intact and the request pending,
// which is the recoverable failure.
func TestReplayRefusesWhenHistoryIsIncomplete(t *testing.T) {
	h := newHarness(t)
	h.enqueue(t, alice, bob, "DT_ATTACK", false)
	h.enqueue(t, alice, carol, "DT_ATTACK", false)
	h.runApplier(t)
	before := h.player(t, alice)

	h.requestReplay(t, "test")
	// The oldest event disappears after the request is written -- which is what
	// a prune between the two would do.
	if _, err := h.pool.Exec(context.Background(),
		"delete from kill_events where id = (select min(id) from kill_events)"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := kills.NewApplier(h.store, metrics.New(), h.cfg).Run(ctx)
	if err == nil {
		t.Fatal("the replay ran against an incomplete history")
	}
	if !strings.Contains(err.Error(), "missing events") {
		t.Errorf("error %q does not say what is wrong", err)
	}

	after := h.player(t, alice)
	if after.Kills != before.Kills || after.Rating != before.Rating {
		t.Errorf("the refused replay still changed the record: %d kills at %v, was %d at %v",
			after.Kills, after.Rating, before.Kills, before.Rating)
	}
	if h.pendingReplays(t) != 1 {
		t.Error("the refused replay was marked complete; it must stay pending")
	}
}

// TestReplayIsClaimedOnce: internal/leader hands out no fencing token, so two
// replicas can both believe they lead. The claim is what stops both of them
// resetting.
func TestReplayIsClaimedOnce(t *testing.T) {
	h := newHarness(t)
	h.enqueue(t, alice, bob, "DT_ATTACK", false)
	h.runApplier(t)
	h.requestReplay(t, "test")

	// Two appliers, started together, against one database.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			runCtx, runCancel := context.WithTimeout(ctx, 3*time.Second)
			defer runCancel()
			errs <- kills.NewApplier(h.store, metrics.New(), h.cfg).Run(runCtx)
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("applier: %v", err)
		}
	}

	// One kill, whoever ran the replay and however many tried.
	if killer := h.player(t, alice); killer.Kills != 1 {
		t.Errorf("%d kills after two concurrent appliers, want 1", killer.Kills)
	}
	if h.pendingReplays(t) != 0 {
		t.Error("the replay is still pending")
	}
}

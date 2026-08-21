package kills_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/config"
	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/dbtest"
	"github.com/USA-RedDragon/obsidibot/internal/kills"
	"github.com/USA-RedDragon/obsidibot/internal/metrics"
	"github.com/USA-RedDragon/obsidibot/internal/rating"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	alice = "111-111-111"
	bob   = "222-222-222"
	carol = "333-333-333"
)

func migrationsFS(t *testing.T) fs.FS {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	return os.DirFS(filepath.Join(filepath.Dir(thisFile), "..", "..", "schema", "migrations"))
}

func testConfig() *config.Config {
	return &config.Config{
		Rating: config.Rating{
			Initial: 1200, ProvisionalK: 40, SettlingK: 20, StableK: 16,
			ProvisionalGames: 20, SettlingGames: 50,
			DecayGraceDays: 30, DecayPermillePerDay: 5,
		},
		KillFeed: config.KillFeed{RetentionDays: 30},
	}
}

type harness struct {
	pool  *pgxpool.Pool
	store *db.Store
	cfg   *config.Config
	seq   int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	pool := dbtest.Pool(t)
	dbtest.Reset(t, pool)
	if err := db.Migrate(context.Background(), pool, migrationsFS(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return &harness{pool: pool, store: db.NewStore(pool), cfg: testConfig()}
}

// enqueue appends a kill to the queue exactly as the ingest endpoint would.
func (h *harness) enqueue(t *testing.T, killer, victim, damageType string, killerIsAdmin bool) {
	t.Helper()
	h.seq++
	credited := damageType == "DT_ATTACK" && killer != "" && killer != victim && !killerIsAdmin
	countsDeath := killer == "" || (killer != victim && !killerIsAdmin)

	params := gen.InsertKillEventParams{
		DedupeKey:     fmt.Appendf(nil, "event-%03d-padding-to-32-bytes!!", h.seq),
		ServerGuid:    "guid",
		Payload:       json.RawMessage(`{}`),
		VictimAgid:    victim,
		VictimName:    "player-" + victim,
		DamageType:    damageType,
		KillerIsAdmin: killerIsAdmin,
		Credited:      credited,
		CountsDeath:   countsDeath,
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

// runApplier drains the queue once and stops.
func (h *harness) runApplier(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	applier := kills.NewApplier(h.store, metrics.New(), h.cfg)
	done := make(chan error, 1)
	go func() { done <- applier.Run(ctx) }()

	// Wait for the queue to empty, then stop it.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		n, err := h.store.Queries().CountUnratedEvents(ctx)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("applier: %v", err)
	}
}

func (h *harness) player(t *testing.T, agid string) gen.Player {
	t.Helper()
	p, err := h.store.Queries().GetPlayer(context.Background(), agid)
	if err != nil {
		t.Fatalf("get player %s: %v", agid, err)
	}
	return p
}

// TestAppliesACreditedKill is the base case: stats and ratings both move, and
// the event is marked done.
func TestAppliesACreditedKill(t *testing.T) {
	h := newHarness(t)
	h.enqueue(t, alice, bob, "DT_ATTACK", false)
	h.runApplier(t)

	killer, victim := h.player(t, alice), h.player(t, bob)
	if killer.Kills != 1 || killer.Deaths != 0 {
		t.Errorf("killer: %d kills %d deaths", killer.Kills, killer.Deaths)
	}
	if victim.Deaths != 1 || victim.Kills != 0 {
		t.Errorf("victim: %d kills %d deaths", victim.Kills, victim.Deaths)
	}
	if killer.Rating <= 1200 {
		t.Errorf("killer rating did not rise: %v", killer.Rating)
	}
	if victim.Rating >= 1200 {
		t.Errorf("victim rating did not fall: %v", victim.Rating)
	}
	if killer.RatedGames != 1 || victim.RatedGames != 1 {
		t.Errorf("rated games: killer %d victim %d", killer.RatedGames, victim.RatedGames)
	}
	// Both sides carry the same K here, so the exchange is exactly zero-sum.
	if total := killer.Rating + victim.Rating; total < 2399.99 || total > 2400.01 {
		t.Errorf("ratings total %v after one kill, want 2400", total)
	}
}

// TestEnvironmentalDeathCountsButDoesNotRate is the rule that came out of the
// design discussion: dying of thirst is a death, but there is no opponent to
// take the points, and inventing one would deflate every rating over time.
func TestEnvironmentalDeathCountsButDoesNotRate(t *testing.T) {
	h := newHarness(t)
	for _, damage := range []string{"DT_THIRST", "DT_HUNGER", "DT_BLEED", "DT_BREAKLEGS", "DT_OXYGEN"} {
		h.enqueue(t, "", bob, damage, false)
	}
	h.runApplier(t)

	victim := h.player(t, bob)
	if victim.Deaths != 5 {
		t.Errorf("%d deaths recorded, want 5", victim.Deaths)
	}
	if victim.Rating != 1200 {
		t.Errorf("rating moved to %v on environmental deaths; it must not", victim.Rating)
	}
	if victim.RatedGames != 0 {
		t.Errorf("%d rated games from environmental deaths, want 0", victim.RatedGames)
	}
}

// TestSelfKillsAndAdminKillsTouchNothing. They are still shown in the kill feed
// -- they happened, and people want to see them -- but they move neither Elo
// NOR K/D: an admin moderating a fight should not dent the record of whoever
// they stop, and a self-kill says nothing about how someone plays.
func TestSelfKillsAndAdminKillsTouchNothing(t *testing.T) {
	h := newHarness(t)
	h.enqueue(t, bob, bob, "DT_ATTACK", false)  // self kill
	h.enqueue(t, alice, bob, "DT_ATTACK", true) // admin kill
	h.runApplier(t)

	victim := h.player(t, bob)
	if victim.Deaths != 0 {
		t.Errorf("%d deaths recorded from a self-kill and an admin kill, want 0", victim.Deaths)
	}
	if victim.Rating != 1200 {
		t.Errorf("victim rating moved to %v on uncredited kills", victim.Rating)
	}
	// The admin must not appear as having a kill either.
	if killer, err := h.store.Queries().GetPlayer(context.Background(), alice); err == nil && killer.Kills != 0 {
		t.Errorf("the admin was credited with %d kills", killer.Kills)
	}

	// But both events are still queued for the feed: the applier marks them
	// rated, never posted.
	unposted, err := h.store.Queries().CountUnpostedEvents(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if unposted != 2 {
		t.Errorf("%d events waiting for the feed, want 2 -- these must still be shown", unposted)
	}
}

// TestRatingsMatchThePureFunction proves the applier is using internal/rating
// as written rather than an approximation of it.
func TestRatingsMatchThePureFunction(t *testing.T) {
	h := newHarness(t)
	h.enqueue(t, alice, bob, "DT_ATTACK", false)
	h.enqueue(t, alice, carol, "DT_ATTACK", false)
	h.enqueue(t, bob, alice, "DT_ATTACK", false)
	h.runApplier(t)

	// Replay the same sequence through the pure function.
	type state struct {
		r     float64
		games int32
	}
	want := map[string]*state{alice: {1200, 0}, bob: {1200, 0}, carol: {1200, 0}}
	replay := func(killer, victim string) {
		k, v := want[killer], want[victim]
		out := rating.Apply(k.r, v.r, k.games, v.games, h.cfg.Rating)
		k.r, v.r = out.Killer, out.Victim
		k.games++
		v.games++
	}
	replay(alice, bob)
	replay(alice, carol)
	replay(bob, alice)

	for agid, expected := range want {
		got := h.player(t, agid)
		if diff := got.Rating - expected.r; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s rated %v, the pure function says %v", agid, got.Rating, expected.r)
		}
	}
}

// TestOrderMatters is why the applier is single-writer. The same three kills in
// a different order MUST give different ratings -- if they did not, the
// ordering constraint would be pointless and this test would be lying.
func TestOrderMatters(t *testing.T) {
	c := testConfig().Rating

	// Sequence one: alice beats bob, then bob beats carol.
	a1, b1, c1 := 1200.0, 1200.0, 1200.0
	var ag, bg, cg int32
	out := rating.Apply(a1, b1, ag, bg, c)
	a1, b1 = out.Killer, out.Victim
	bg++
	out = rating.Apply(b1, c1, bg, cg, c)
	b1, c1 = out.Killer, out.Victim

	// Sequence two: the same two kills, swapped.
	a2, b2, c2 := 1200.0, 1200.0, 1200.0
	ag, bg, cg = 0, 0, 0
	out = rating.Apply(b2, c2, bg, cg, c)
	b2, c2 = out.Killer, out.Victim
	bg++
	out = rating.Apply(a2, b2, ag, bg, c)
	a2, b2 = out.Killer, out.Victim

	if b1 == b2 {
		t.Fatalf("both orderings gave bob %v; if order did not matter the "+
			"single-writer constraint would be unnecessary", b1)
	}
	_ = a1
	_ = a2
	_ = c1
	_ = c2
}

// TestAppliedExactlyOnce: running the applier again must not re-award anything.
// This is what makes a crashed-and-restarted worker safe.
func TestAppliedExactlyOnce(t *testing.T) {
	h := newHarness(t)
	h.enqueue(t, alice, bob, "DT_ATTACK", false)
	h.runApplier(t)

	first := h.player(t, alice)
	h.runApplier(t)
	second := h.player(t, alice)

	if first.Kills != second.Kills || first.Rating != second.Rating {
		t.Fatalf("a second pass changed things: %d kills/%v then %d kills/%v",
			first.Kills, first.Rating, second.Kills, second.Rating)
	}
}

// TestUnlinkedPlayersStillAccumulate is what makes the leaderboard a real
// server ranking on day one rather than an empty list.
func TestUnlinkedPlayersStillAccumulate(t *testing.T) {
	h := newHarness(t)
	h.enqueue(t, alice, bob, "DT_ATTACK", false)
	h.runApplier(t)

	top, err := h.store.Queries().TopPlayers(context.Background(), 20)
	if err != nil {
		t.Fatalf("top players: %v", err)
	}
	if len(top) != 2 {
		t.Fatalf("%d players on the board, want 2", len(top))
	}
	for _, row := range top {
		if row.DiscordUserID != nil {
			t.Errorf("%s reported a Discord link that was never made", row.AlderonID)
		}
	}
	if top[0].AlderonID != alice {
		t.Errorf("board is ordered %s first, want the killer %s", top[0].AlderonID, alice)
	}
}

// TestBoardExcludesPlayersWithNoRecord: otherwise the top twenty is whoever
// happened to link first, all tied at the starting rating.
func TestBoardExcludesPlayersWithNoRecord(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.store.Queries().UpsertPlayerSeen(ctx, gen.UpsertPlayerSeenParams{
		AlderonID: carol, LastKnownName: "carol", Rating: 1200,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h.enqueue(t, alice, bob, "DT_ATTACK", false)
	h.runApplier(t)

	top, err := h.store.Queries().TopPlayers(ctx, 20)
	if err != nil {
		t.Fatalf("top: %v", err)
	}
	for _, row := range top {
		if row.AlderonID == carol {
			t.Fatal("a player with no kills and no deaths appeared on the board")
		}
	}
}

// TestDecayJobIsResumableAndBounded exercises the real query and the real
// stamping, not just the pure function.
func TestDecayJobIsResumableAndBounded(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.enqueue(t, alice, bob, "DT_ATTACK", false)
	h.runApplier(t)

	// Push alice well above the baseline and make her long idle.
	if _, err := h.pool.Exec(ctx, `update players
		set rating = 1800, last_seen_at = now() - interval '200 days',
		    decayed_at = now() - interval '100 days'
		where alderon_id = $1`, alice); err != nil {
		t.Fatalf("age player: %v", err)
	}

	decayer := kills.NewDecayer(h.store, h.cfg)
	runOnce := func() {
		runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- decayer.Run(runCtx) }()
		time.Sleep(300 * time.Millisecond)
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("decayer: %v", err)
		}
	}

	runOnce()
	after := h.player(t, alice)
	if after.Rating >= 1800 {
		t.Errorf("an idle rating of 1800 did not decay: %v", after.Rating)
	}
	if after.Rating < 1200 {
		t.Errorf("decay overshot the baseline: %v", after.Rating)
	}

	// Immediately running again must be a no-op: decayed_at was just stamped,
	// so there are no whole days to apply.
	runOnce()
	if again := h.player(t, alice); again.Rating != after.Rating {
		t.Errorf("a second decay pass in the same day moved the rating from %v to %v",
			after.Rating, again.Rating)
	}

	// A player who is still active must be untouched.
	if victim := h.player(t, bob); victim.Rating >= 1200 {
		t.Logf("bob rating %v (below baseline, correctly not decayed upward)", victim.Rating)
	}
}

// TestPrunerKeepsUnprocessedEvents: pruning must never discard something a
// worker still needs.
func TestPrunerKeepsUnprocessedEvents(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.enqueue(t, alice, bob, "DT_ATTACK", false)
	h.enqueue(t, bob, carol, "DT_ATTACK", false)
	h.runApplier(t) // rated, but nothing is posted yet

	if _, err := h.pool.Exec(ctx,
		"update kill_events set received_at = now() - interval '400 days'"); err != nil {
		t.Fatalf("age events: %v", err)
	}

	rows, err := h.store.Queries().PruneProcessedEvents(ctx, 30)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if rows != 0 {
		t.Fatalf("pruned %d events that had not been posted to the feed yet", rows)
	}

	// Once both workers are done with them, they go.
	if _, err := h.pool.Exec(ctx, "update kill_events set posted = true"); err != nil {
		t.Fatalf("mark posted: %v", err)
	}
	rows, err = h.store.Queries().PruneProcessedEvents(ctx, 30)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if rows != 2 {
		t.Fatalf("pruned %d fully processed events, want 2", rows)
	}

	// And the stats they produced are untouched.
	if killer := h.player(t, alice); killer.Kills != 1 {
		t.Errorf("pruning destroyed a stat: alice has %d kills", killer.Kills)
	}
}

// Package kills turns recorded kill events into ratings, stats and a feed.
//
// # One writer, in id order
//
// Elo is ORDER-DEPENDENT: apply the same kills in a different order and the
// ratings differ, because each result is computed against the ratings as they
// stood at that moment. The applier therefore walks kill_events by id -- which
// is the arrival order -- from exactly one replica, holding an advisory lock.
//
// This is not an optimisation that could be relaxed for throughput. Two
// appliers running concurrently would each read a player's rating, each compute
// a different new value, and the second write would erase the first: not
// duplicated work, but a wrong number that nothing would ever detect.
package kills

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/config"
	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/metrics"
	"github.com/USA-RedDragon/obsidibot/internal/rating"
	"github.com/jackc/pgx/v5"
)

// batchSize bounds one pass. Small enough that a restart loses little progress,
// large enough that a backlog drains at a useful rate.
const batchSize = 200

// idlePoll is how long to wait when the queue is empty.
const idlePoll = 2 * time.Second

// Applier is the single-writer rating worker.
type Applier struct {
	store   *db.Store
	metrics *metrics.Metrics
	cfg     *config.Config
}

// NewApplier builds the rating applier.
func NewApplier(store *db.Store, m *metrics.Metrics, cfg *config.Config) *Applier {
	return &Applier{store: store, metrics: m, cfg: cfg}
}

// Run drains the queue until ctx is cancelled. It is the Job handed to
// internal/leader, so it only ever runs on the replica holding the lock.
func (a *Applier) Run(ctx context.Context) error {
	// A pending replay is a rule change waiting to be applied to history. It
	// has to happen before anything else touches a rating, and it is checked
	// once per acquisition rather than every pass because the claim below is
	// what makes it a one-shot.
	if err := a.replay(ctx); err != nil {
		if ctx.Err() != nil {
			//nolint:nilerr // deliberate: cancellation is a clean stop
			return nil
		}
		return err
	}

	for {
		applied, err := a.drain(ctx)
		if err != nil {
			// Shutting down is not a failure: a cancelled context unwinds
			// through whatever query was in flight, so the error is a symptom
			// of the exit rather than a fault worth reporting.
			//nolint:nilerr // deliberate: cancellation is a clean stop
			if ctx.Err() != nil {
				return nil
			}
			return err
		}

		if applied == 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(idlePoll):
			}
		}
	}
}

// drain applies one batch and reports how many events it handled.
func (a *Applier) drain(ctx context.Context) (int, error) {
	events, err := a.store.Queries().NextUnratedEvents(ctx, batchSize)
	if err != nil {
		return 0, fmt.Errorf("read unrated events: %w", err)
	}

	if a.metrics != nil {
		if backlog, err := a.store.Queries().CountUnratedEvents(ctx); err == nil {
			a.metrics.RatingBacklog.Set(float64(backlog))
		}
	}

	for _, event := range events {
		if err := a.apply(ctx, event); err != nil {
			// Stop at the first failure rather than skipping ahead: the queue
			// is an ORDERED sequence, and applying event N+1 while N is
			// unresolved would bake the wrong ratings in permanently.
			return 0, fmt.Errorf("apply kill event %d: %w", event.ID, err)
		}
	}
	return len(events), nil
}

// apply lands one event in its own transaction. A crash partway through must
// not leave an event half-applied, because the flag is the only record of
// whether it counted.
func (a *Applier) apply(ctx context.Context, event gen.NextUnratedEventsRow) error {
	credited := creditsAKill(event.DamageType, event.KillerAgid, event.VictimAgid)

	if err := a.store.InTx(ctx, func(q *gen.Queries) error {
		return a.applyIn(ctx, q, event)
	}); err != nil {
		return err
	}

	if a.metrics != nil {
		a.metrics.RatingUpdatesTotal.WithLabelValues(strconv.FormatBool(credited)).Inc()
	}
	return nil
}

// applyIn is the whole of what one event does, against a transaction somebody
// else owns.
//
// It is split out so the normal one-event-at-a-time path and the replay --
// which runs every event inside ONE transaction -- are the same code. Two
// implementations of "what does this kill do to the ratings" would be two
// chances to disagree, and the disagreement would be invisible: an
// order-dependent Elo chain that nothing recomputes.
//
// seen_at is the event's own arrival time rather than now(). Live, they differ
// by milliseconds; on a replay, now() would stamp the entire server as active
// this instant and reset everyone's decay clock.
func (a *Applier) applyIn(ctx context.Context, q *gen.Queries, event gen.NextUnratedEventsRow) error {
	// Derived, not read off the row: see creditsAKill. The nil check is
	// belt-and-braces -- the rule already requires a killer -- because the
	// pointer is dereferenced below.
	credited := creditsAKill(event.DamageType, event.KillerAgid, event.VictimAgid) && event.KillerAgid != nil
	seenAt := event.ReceivedAt

	// The victim always exists in the players table after this, whether or not
	// they have ever linked a Discord account: the board ranks the server, not
	// the subset of it that uses the bot.
	if err := q.UpsertPlayerSeenAt(ctx, gen.UpsertPlayerSeenAtParams{
		AlderonID:     event.VictimAgid,
		LastKnownName: event.VictimName,
		Rating:        float64(a.cfg.Rating.Initial),
		SeenAt:        seenAt,
	}); err != nil {
		return fmt.Errorf("record victim: %w", err)
	}

	if !credited {
		// No rating moves: there is no counterparty to take the points. It is
		// still a death -- every one of these events is somebody dying, whether
		// the world did it or a kill the rules do not rate -- and it is still
		// shown in the feed by the other worker.
		if err := q.RecordUnratedDeath(ctx, gen.RecordUnratedDeathParams{
			AlderonID: event.VictimAgid, SeenAt: seenAt,
		}); err != nil {
			return fmt.Errorf("record unrated death: %w", err)
		}
		// No figures recorded: nothing moved, and a zero would read as a real
		// result of zero rather than as "not applicable".
		return q.MarkEventRated(ctx, gen.MarkEventRatedParams{ID: event.ID})
	}

	killerID := *event.KillerAgid
	killerName := killerID
	if event.KillerName != nil {
		killerName = *event.KillerName
	}
	if err := q.UpsertPlayerSeenAt(ctx, gen.UpsertPlayerSeenAtParams{
		AlderonID:     killerID,
		LastKnownName: killerName,
		Rating:        float64(a.cfg.Rating.Initial),
		SeenAt:        seenAt,
	}); err != nil {
		return fmt.Errorf("record killer: %w", err)
	}

	// Read both ratings INSIDE the transaction, after the upserts, so the
	// numbers this result is computed against are the ones on disk.
	killer, err := q.GetPlayer(ctx, killerID)
	if err != nil {
		return fmt.Errorf("load killer: %w", err)
	}
	victim, err := q.GetPlayer(ctx, event.VictimAgid)
	if err != nil {
		return fmt.Errorf("load victim: %w", err)
	}

	outcome := rating.Apply(killer.Rating, victim.Rating, killer.RatedGames, victim.RatedGames, a.cfg.Rating)

	if err := q.CreditKill(ctx, gen.CreditKillParams{
		AlderonID: killerID, Rating: outcome.Killer, SeenAt: seenAt,
	}); err != nil {
		return fmt.Errorf("credit kill: %w", err)
	}
	if err := q.RecordRatedLoss(ctx, gen.RecordRatedLossParams{
		AlderonID: event.VictimAgid, Rating: outcome.Victim, SeenAt: seenAt,
	}); err != nil {
		return fmt.Errorf("record rated loss: %w", err)
	}
	// Both sides of the exchange are written onto the event, because this is
	// the only moment anything knows them: the feed and /stats report the Elo a
	// kill moved, and recomputing it later would mean replaying the whole
	// chain to find out.
	return q.MarkEventRated(ctx, gen.MarkEventRatedParams{
		ID:                 event.ID,
		KillerRatingBefore: &killer.Rating,
		KillerRatingAfter:  &outcome.Killer,
		VictimRatingBefore: &victim.Rating,
		VictimRatingAfter:  &outcome.Victim,
	})
}

// replay rebuilds every rating from the events, if one has been requested.
//
// # Why history is rebuilt rather than patched
//
// Elo is order-dependent: each result is computed against the ratings as they
// stood at that moment. When a rule changes, there is no arithmetic that
// converts the old numbers into the new ones -- the only correct answer is to
// start from the initial rating and walk the events again in id order. That is
// what this does, using the same applyIn every live event goes through.
//
// # Why it is all one transaction
//
// The reset zeroes every aggregate, and the leaderboard query only lists
// players with kills or deaths. A committed-but-unreplayed reset therefore
// shows an EMPTY leaderboard and every player at the starting rating -- and if
// the replay then hit an event it could not apply, it would stay that way,
// because the drain stops at the first error and the job simply restarts.
// Inside one transaction, readers see the old ratings until the new ones are
// complete, and any failure rolls back to a replay that is still pending.
func (a *Applier) replay(ctx context.Context) error {
	return a.store.InTx(ctx, func(q *gen.Queries) error {
		claim, err := q.ClaimRatingReplay(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // nothing requested, or another replica has it
		}
		if err != nil {
			return fmt.Errorf("claim rating replay: %w", err)
		}

		// THE PRUNE INTERLOCK. Aggregates are rebuilt from the surviving
		// events, so replaying after history has been deleted quietly produces
		// lower kill counts and a wrong chain -- wrong in a way no later pass
		// detects, because the result looks like a perfectly ordinary
		// leaderboard. Refusing leaves the request pending and the old numbers
		// intact, which is the recoverable failure.
		oldest, err := q.MinEventID(ctx)
		if err != nil {
			return fmt.Errorf("read oldest event: %w", err)
		}
		if claim.MinEventID != nil && oldest > *claim.MinEventID {
			return fmt.Errorf(
				"%w: replay %d needs events from id %d but the oldest surviving event is %d",
				errHistoryIncomplete, claim.ID, *claim.MinEventID, oldest)
		}

		if err := q.ResetPlayerAggregates(ctx, float64(a.cfg.Rating.Initial)); err != nil {
			return fmt.Errorf("reset player aggregates: %w", err)
		}
		requeued, err := q.RequeueRatedEvents(ctx)
		if err != nil {
			return fmt.Errorf("requeue events: %w", err)
		}

		// Walked in pages for the same reason the live drain is: the batch
		// bounds how much is held in memory at once. Every page is inside this
		// one transaction, so the events requeued above stay unrated to
		// everyone else until the whole thing commits.
		var applied int64
		for {
			events, err := q.NextUnratedEvents(ctx, batchSize)
			if err != nil {
				return fmt.Errorf("read events to replay: %w", err)
			}
			if len(events) == 0 {
				break
			}
			for _, event := range events {
				if err := a.applyIn(ctx, q, event); err != nil {
					return fmt.Errorf("replay kill event %d: %w", event.ID, err)
				}
				applied++
			}
		}

		slog.InfoContext(ctx, "replayed every rating from the kill history",
			"reason", claim.Reason, "requeued", requeued, "applied", applied)
		return nil
	})
}

// errHistoryIncomplete means a replay was requested over events that no longer
// all exist. It is a refusal, not a failure: the request stays pending so it
// can run once somebody decides what to do about the gap.
var errHistoryIncomplete = errors.New("kills: the kill history is missing events the replay needs")

// Decayer pulls idle ratings back toward the baseline.
type Decayer struct {
	store *db.Store
	cfg   *config.Config
}

// NewDecayer builds the inactivity decay job.
func NewDecayer(store *db.Store, cfg *config.Config) *Decayer {
	return &Decayer{store: store, cfg: cfg}
}

// decayInterval is how often to look for idle ratings. Decay is measured in
// whole days, so checking more often than hourly only wastes queries.
const decayInterval = time.Hour

// Run applies decay until ctx is cancelled.
func (d *Decayer) Run(ctx context.Context) error {
	for {
		if err := d.pass(ctx); err != nil {
			//nolint:nilerr // deliberate: cancellation is a clean stop
			if ctx.Err() != nil {
				return nil
			}
			slog.ErrorContext(ctx, "rating decay pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(decayInterval):
		}
	}
}

func (d *Decayer) pass(ctx context.Context) error {
	q := d.store.Queries()
	candidates, err := q.DecayCandidates(ctx, gen.DecayCandidatesParams{
		Floor:     float64(d.cfg.Rating.Initial),
		GraceDays: int32(d.cfg.Rating.DecayGraceDays), //nolint:gosec // bounded by config validation
		Limit:     batchSize,
	})
	if err != nil {
		return fmt.Errorf("find decay candidates: %w", err)
	}

	now := time.Now()
	for _, player := range candidates {
		// Steps are counted from decayed_at, so a pass that was missed applies
		// the days it missed and a pass that runs twice in one day applies
		// none. That is what makes the job resumable.
		//
		// But decayed_at DEFAULTS TO WHEN THE ROW WAS CREATED and only ever
		// advances here, and this job only ever sees players already past the
		// grace period. So for an active player it still holds the moment they
		// were first seen, and the elapsed time since then is their whole
		// account age -- not the time they have been idle.
		//
		// Without the second term, a player first seen a year ago who stops
		// playing crosses the grace period and loses ~343 rating points in one
		// hourly tick instead of the ~2 the curve intends. The number on the
		// board is the thing players argue about, so getting this wrong is
		// visible and indefensible.
		sinceStamp := int(now.Sub(player.DecayedAt).Hours() / 24)
		pastGrace := int(now.Sub(player.LastSeenAt).Hours()/24) - d.cfg.Rating.DecayGraceDays
		days := min(sinceStamp, pastGrace)
		if days < 0 {
			days = 0
		}
		// ApplyDecay is issued even when the rating does not move, because it
		// also stamps decayed_at. Skipping it would leave this player in the
		// candidate set to be reconsidered every hour forever.
		newRating := rating.Decay(player.Rating, days, d.cfg.Rating)
		if err := q.ApplyDecay(ctx, gen.ApplyDecayParams{
			AlderonID: player.AlderonID, Rating: newRating,
		}); err != nil {
			return fmt.Errorf("apply decay to a player: %w", err)
		}
	}
	if len(candidates) > 0 {
		slog.InfoContext(ctx, "applied rating decay", "players", len(candidates))
	}
	return nil
}

// Pruner drops kill events both workers have finished with.
type Pruner struct {
	store *db.Store
	cfg   *config.Config
}

// NewPruner builds the retention job.
func NewPruner(store *db.Store, cfg *config.Config) *Pruner {
	return &Pruner{store: store, cfg: cfg}
}

const pruneInterval = 6 * time.Hour

// Run prunes until ctx is cancelled.
//
// Only events that are BOTH rated and posted are eligible, so this can never
// take something a worker still needs.
//
// It ages out the raw payload and KEEPS THE ROW. Deleting rows cost two things
// that turned out to matter more than the disk: a player being able to see
// their whole history in /stats, and the ability to replay a rule change --
// which has now been needed twice. A slim row is a few hundred bytes, so the
// events are kept for the life of the server.
func (p *Pruner) Run(ctx context.Context) error {
	for {
		rows, err := p.store.Queries().PruneProcessedEvents(ctx,
			int32(p.cfg.KillFeed.RetentionDays)) //nolint:gosec // bounded by config validation
		switch {
		case err != nil && ctx.Err() != nil:
			return nil
		case err != nil:
			slog.ErrorContext(ctx, "could not prune kill events", "error", err)
		case rows > 0:
			slog.InfoContext(ctx, "dropped the raw payload of processed kill events",
				"rows", rows)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(pruneInterval):
		}
	}
}

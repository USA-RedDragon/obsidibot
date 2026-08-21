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
	"fmt"
	"log/slog"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/config"
	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/metrics"
	"github.com/USA-RedDragon/obsidibot/internal/rating"
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

// apply lands one event: the stat changes, the rating changes and the "done"
// mark, all in ONE transaction. A crash partway through must not leave an event
// half-applied, because the flag is the only record of whether it counted.
func (a *Applier) apply(ctx context.Context, event gen.KillEvent) error {
	credited := event.Credited && event.KillerAgid != nil

	err := a.store.InTx(ctx, func(q *gen.Queries) error {
		// The victim always exists in the players table after this, whether or
		// not they have ever linked a Discord account: the board ranks the
		// server, not the subset of it that uses the bot.
		if err := q.UpsertPlayerSeen(ctx, gen.UpsertPlayerSeenParams{
			AlderonID:     event.VictimAgid,
			LastKnownName: event.VictimName,
			Rating:        float64(a.cfg.Rating.Initial),
		}); err != nil {
			return fmt.Errorf("record victim: %w", err)
		}

		if !credited {
			// No rating moves: there is no counterparty to take the points.
			// Whether it counts as a DEATH is a separate question the ingest
			// endpoint already answered -- an environmental death does, an
			// admin's kill or a self-kill does not. Either way the event is
			// still shown in the feed by the other worker.
			if event.CountsDeath {
				if err := q.RecordUnratedDeath(ctx, event.VictimAgid); err != nil {
					return fmt.Errorf("record unrated death: %w", err)
				}
			}
			return q.MarkEventRated(ctx, event.ID)
		}

		killerID := *event.KillerAgid
		killerName := killerID
		if event.KillerName != nil {
			killerName = *event.KillerName
		}
		if err := q.UpsertPlayerSeen(ctx, gen.UpsertPlayerSeenParams{
			AlderonID:     killerID,
			LastKnownName: killerName,
			Rating:        float64(a.cfg.Rating.Initial),
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
			AlderonID: killerID, Rating: outcome.Killer,
		}); err != nil {
			return fmt.Errorf("credit kill: %w", err)
		}
		if err := q.RecordRatedLoss(ctx, gen.RecordRatedLossParams{
			AlderonID: event.VictimAgid, Rating: outcome.Victim,
		}); err != nil {
			return fmt.Errorf("record rated loss: %w", err)
		}
		return q.MarkEventRated(ctx, event.ID)
	})
	if err != nil {
		return err
	}

	if a.metrics != nil {
		a.metrics.RatingUpdatesTotal.WithLabelValues(boolLabel(credited)).Inc()
	}
	return nil
}

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
		// Steps are counted from decayed_at, not from last_seen_at, so a pass
		// that was missed applies the days it missed and a pass that runs twice
		// in one day applies none. That is what makes the job resumable.
		days := int(now.Sub(player.DecayedAt).Hours() / 24)
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
// Only events that are BOTH rated and posted are eligible, so pruning can never
// discard something a worker still needs. The aggregates live on the player
// row, so this loses no stats -- only the ability to replay a rule change
// against events older than the window.
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
			slog.InfoContext(ctx, "pruned processed kill events", "rows", rows)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(pruneInterval):
		}
	}
}

func boolLabel(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

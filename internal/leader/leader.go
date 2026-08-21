// Package leader runs a job on exactly one replica at a time.
//
// obsidibot scales by adding identical replicas, but three of its background
// jobs cannot be run twice at once. The rating applier is the strict case: Elo
// is order-dependent, so two writers walking the kill queue would not merely
// duplicate work, they would compute DIFFERENT ratings and overwrite each
// other. The kill feed and the leaderboard are the mild case: two writers
// would double-post and fight over one message.
//
// The lock is a Postgres SESSION advisory lock, which is the right primitive
// precisely because it dies with its session. A replica that is partitioned,
// paused, or OOM-killed does not get to keep the lock: Postgres notices the
// connection is gone and the next replica to try takes over. A lease in a
// table would need a heartbeat and a clock, and would hand the lock to a
// second writer while the first was merely slow.
//
// What this does NOT provide is fencing. Between losing the connection and
// noticing, a former leader may still believe it leads. That is tolerable here
// because every job is idempotent per row -- each marks its own progress in the
// same transaction as its effect -- so the worst case is repeating one item,
// not corrupting the sequence.
package leader

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Key derives an advisory lock key from a job name.
//
// Keys are the big-endian reading of the name's first eight ASCII bytes,
// matching how the migration lock is built, so the whole keyspace is
// recoverable from strings. Small ordinals are deliberately avoided: an
// unrelated component picking "1" would collide, and an advisory-lock
// collision presents as a job that silently never runs.
//
// Names shorter than eight bytes are padded with spaces; longer ones are
// truncated, so job names must be distinct in their first eight characters.
func Key(name string) int64 {
	var buf [8]byte
	for i := range buf {
		if i < len(name) {
			buf[i] = name[i]
			continue
		}
		buf[i] = ' '
	}
	return int64(binary.BigEndian.Uint64(buf[:])) //nolint:gosec // a bit pattern, not a magnitude
}

// Job is the work to run while the lock is held. It should return only when
// ctx is cancelled, or on an error worth losing leadership over.
type Job func(ctx context.Context) error

// Runner acquires one job's lock and runs it.
type Runner struct {
	pool *pgxpool.Pool
	name string
	key  int64
	// retry is how long to wait before trying again, both after losing a
	// contest for the lock and after the job returns an error.
	retry time.Duration
	// onAcquire is called each time leadership is taken, for metrics.
	onAcquire func(job string)
}

// New returns a Runner for the named job. onAcquire may be nil.
func New(pool *pgxpool.Pool, name string, retry time.Duration, onAcquire func(job string)) *Runner {
	return &Runner{pool: pool, name: name, key: Key(name), retry: retry, onAcquire: onAcquire}
}

// Run blocks until ctx is cancelled, holding the lock and running the job
// whenever it can get it, and waiting quietly whenever it cannot.
//
// It returns nil on cancellation: a replica that never became leader has not
// failed, it simply was not needed.
func (r *Runner) Run(ctx context.Context, job Job) error {
	for {
		if err := r.attempt(ctx, job); err != nil {
			// Shutting down is not a failure, however the job reported it:
			// a cancelled context unwinds through whatever call was in flight,
			// so the error here is a symptom of the exit rather than a fault.
			//nolint:nilerr // deliberate: cancellation is a clean stop
			if ctx.Err() != nil {
				return nil
			}
			// A job that fails is logged and retried rather than taking the
			// process down: the other replicas are not better placed to run
			// it, so exiting would just move the outage.
			slog.ErrorContext(ctx, "background job failed, will retry",
				"job", r.name, "retry", r.retry, "error", err)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(r.retry):
		}
	}
}

// attempt takes the lock if it is free and runs the job while holding it.
// A lock held elsewhere is not an error; it returns nil and the caller waits.
func (r *Runner) attempt(ctx context.Context, job Job) error {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for %s lock: %w", r.name, err)
	}
	// The lock lives on this connection, so the connection must be held for
	// exactly as long as leadership and then discarded carefully below.
	defer conn.Release()

	var acquired bool
	if err := conn.QueryRow(ctx, "select pg_try_advisory_lock($1)", r.key).Scan(&acquired); err != nil {
		return fmt.Errorf("try %s lock: %w", r.name, err)
	}
	if !acquired {
		slog.DebugContext(ctx, "another replica holds this job", "job", r.name)
		return nil
	}

	defer func() {
		// Cancel-free: a job stopping because the process is shutting down
		// must still hand the lock back, so the replacement replica does not
		// wait out a TCP timeout to take over.
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, "select pg_advisory_unlock($1)", r.key); err != nil {
			// The session is almost certainly already dead -- advisory locks
			// die with their session -- but close the connection to make sure
			// a healthy-but-still-locked one can never go back to the pool and
			// deadlock this job against itself.
			_ = conn.Conn().Close(unlockCtx)
			slog.WarnContext(unlockCtx, "failed to release job lock; discarding connection",
				"job", r.name, "error", err)
		}
	}()

	slog.InfoContext(ctx, "took leadership of background job", "job", r.name)
	if r.onAcquire != nil {
		r.onAcquire(r.name)
	}

	err = job(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

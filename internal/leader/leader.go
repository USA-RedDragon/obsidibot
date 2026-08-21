// Package leader runs a job on exactly one replica at a time.
//
// obsidibot scales by adding identical replicas, but six of its background jobs
// cannot be run twice at once. The rating applier is the strict case: Elo
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
//
// # Why the lock never comes from the request pool
//
// A session lock has to sit on ONE connection for as long as leadership lasts,
// and leadership lasts until the process stops. Borrowing that connection from
// the pool that serves interactions would mean every leader-held job
// permanently removing one connection from that pool -- with six singletons and
// a pool sized from NumCPU, a small node ends up with nothing left for slash
// commands, kill webhooks or /readyz, which then block in Acquire until their
// deadlines. That was a real outage. A Runner therefore opens its OWN
// connection and closes it when it stops leading: leadership is a lifetime
// lease, so it is paid for with a connection nobody else is queueing behind.
package leader

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
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

// releaseTimeout bounds handing the lock back on the way out. It is short
// because the replacement replica is already contesting the lock and a
// shutdown that hangs here delays the whole process exit; if Postgres cannot
// answer inside it, closing the connection ends the session and the lock dies
// with it anyway.
const releaseTimeout = 5 * time.Second

// Job is the work to run while the lock is held. It should return only when
// ctx is cancelled, or on an error worth losing leadership over.
type Job func(ctx context.Context) error

// Connector opens the dedicated connection a Runner holds its lock on.
//
// It is a function rather than a connection because a Runner needs a FRESH
// session per attempt: the previous one was closed to release the lock, and a
// leader that loses its connection has to be able to come back.
type Connector func(ctx context.Context) (*pgx.Conn, error)

// DialConfig returns a Connector that opens connections from cfg -- in practice
// the configuration the request pool itself dials with, so leadership reaches
// the same database, with the same TLS, search_path and timeouts, over
// connections the pool never sees.
//
// It takes a parsed config rather than a DSN on purpose. A connection string
// may legitimately carry pgxpool's own settings (pool_max_conns and friends),
// which pgxpool strips but pgx.ParseConfig keeps as server parameters: dialling
// the raw DSN would then fail with `unrecognized configuration parameter`, and
// every background job would be stuck retrying forever on a deployment whose
// URL happens to be written that way.
func DialConfig(cfg *pgx.ConnConfig) Connector {
	return func(ctx context.Context) (*pgx.Conn, error) {
		// Copied per dial so no connection can be affected by another's config.
		conn, err := pgx.ConnectConfig(ctx, cfg.Copy())
		if err != nil {
			return nil, fmt.Errorf("connect for job lock: %w", err)
		}
		return conn, nil
	}
}

// Runner acquires one job's lock and runs it.
type Runner struct {
	// connect opens the connection the lock lives on. It is deliberately not a
	// pool: see the package comment.
	connect Connector
	name    string
	key     int64
	// retry is how long to wait before trying again, both after losing a
	// contest for the lock and after the job returns an error.
	retry time.Duration
	// onAcquire is called each time leadership is taken, for metrics.
	onAcquire func(job string)
}

// New returns a Runner for the named job. onAcquire may be nil.
func New(connect Connector, name string, retry time.Duration, onAcquire func(job string)) *Runner {
	return &Runner{connect: connect, name: name, key: Key(name), retry: retry, onAcquire: onAcquire}
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
	conn, err := r.connect(ctx)
	if err != nil {
		return fmt.Errorf("open connection for %s lock: %w", r.name, err)
	}
	// One connection per ATTEMPT, not one kept for the Runner's lifetime: a
	// follower that loses the contest below hands its session straight back, so
	// an idle replica costs Postgres one short-lived connection per retry and
	// only the actual leader holds one open.
	defer func() {
		// Cancel-free: the connection must be closed even when the process is
		// already shutting down, or the session -- and the lock on it --
		// outlives us until Postgres notices the socket is gone.
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
		defer cancel()
		if err := conn.Close(closeCtx); err != nil {
			slog.DebugContext(closeCtx, "closing job lock connection", "job", r.name, "error", err)
		}
	}()

	var acquired bool
	if err := conn.QueryRow(ctx, "select pg_try_advisory_lock($1)", r.key).Scan(&acquired); err != nil {
		return fmt.Errorf("try %s lock: %w", r.name, err)
	}
	if !acquired {
		slog.DebugContext(ctx, "another replica holds this job", "job", r.name)
		return nil
	}

	// Ordered before the close above by defer's LIFO: unlock, then close.
	defer func() {
		// Unlock EXPLICITLY even though ending the session drops the lock
		// anyway: pg_advisory_unlock returns only once the lock is really
		// gone, while a terminated backend is reaped asynchronously. Without
		// this, the replacement replica can contest the lock inside that
		// window, lose, and then wait out a whole retry interval for a job
		// that is already free.
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, "select pg_advisory_unlock($1)", r.key); err != nil {
			// The session is almost certainly already dead -- advisory locks
			// die with their session -- so this is a note rather than a fault,
			// and the close below finishes the release either way.
			slog.WarnContext(unlockCtx, "failed to release job lock; closing its connection",
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

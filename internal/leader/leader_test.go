package leader_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/dbtest"
	"github.com/USA-RedDragon/obsidibot/internal/leader"
)

// TestKeyIsDerivedFromTheName pins the encoding, because a changed key is
// invisible: two versions of the binary would each think they held the lock and
// both would run a job that must have one writer.
func TestKeyIsDerivedFromTheName(t *testing.T) {
	if got, want := leader.Key("obsidibo"), int64(0x6f62_7369_6469_626f); got != want {
		t.Errorf("Key(%q) = %#x, want %#x", "obsidibo", got, want)
	}
	// Short names pad rather than collapsing to zero.
	if leader.Key("feed") == 0 {
		t.Error("a short name produced the zero key")
	}
	// Distinct job names must not collide in their first eight bytes.
	names := []string{"ratings", "killfeed", "leaderbd", "bankrecn", "prune"}
	seen := make(map[int64]string, len(names))
	for _, name := range names {
		key := leader.Key(name)
		if other, dup := seen[key]; dup {
			t.Errorf("jobs %q and %q share lock key %#x", name, other, key)
		}
		seen[key] = name
	}
}

// TestOnlyOneRunnerHoldsTheJob is the property every replica depends on. Two
// runners on one key, started together: exactly one may be inside the job.
func TestOnlyOneRunnerHoldsTheJob(t *testing.T) {
	pool := dbtest.Pool(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var concurrent, peak, entered atomic.Int64
	job := func(ctx context.Context) error {
		entered.Add(1)
		now := concurrent.Add(1)
		for {
			high := peak.Load()
			if now <= high || peak.CompareAndSwap(high, now) {
				break
			}
		}
		defer concurrent.Add(-1)
		<-ctx.Done()
		return nil
	}

	const runners = 3
	done := make(chan error, runners)
	for range runners {
		r := leader.New(pool, "testjob1", 10*time.Millisecond, nil)
		go func() { done <- r.Run(ctx, job) }()
	}

	// Long enough that every loser has retried several times.
	time.Sleep(300 * time.Millisecond)
	if got := peak.Load(); got != 1 {
		t.Fatalf("%d runners were inside the job at once, want 1", got)
	}
	if entered.Load() != 1 {
		t.Fatalf("the job was entered %d times, want 1", entered.Load())
	}

	cancel()
	for range runners {
		if err := <-done; err != nil {
			t.Errorf("runner returned %v; losing a leadership contest is not an error", err)
		}
	}
}

// TestLeadershipPassesOn covers the failover the design promises: when the
// leader stops, a waiting replica picks the job up without human help.
func TestLeadershipPassesOn(t *testing.T) {
	pool := dbtest.Pool(t)

	leaderCtx, stopLeader := context.WithCancel(context.Background())
	followerCtx, stopFollower := context.WithCancel(context.Background())
	defer stopFollower()

	var leaderRan, followerRan atomic.Bool
	hold := func(flag *atomic.Bool) leader.Job {
		return func(ctx context.Context) error {
			flag.Store(true)
			<-ctx.Done()
			return nil
		}
	}

	first := leader.New(pool, "testjob2", 10*time.Millisecond, nil)
	firstDone := make(chan error, 1)
	go func() { firstDone <- first.Run(leaderCtx, hold(&leaderRan)) }()

	// Let the first take the lock before the second starts contesting it.
	time.Sleep(150 * time.Millisecond)
	if !leaderRan.Load() {
		t.Fatal("the first runner never took the job")
	}

	second := leader.New(pool, "testjob2", 10*time.Millisecond, nil)
	secondDone := make(chan error, 1)
	go func() { secondDone <- second.Run(followerCtx, hold(&followerRan)) }()

	time.Sleep(150 * time.Millisecond)
	if followerRan.Load() {
		t.Fatal("the second runner took a job the first still held")
	}

	stopLeader()
	if err := <-firstDone; err != nil {
		t.Fatalf("first runner: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for !followerRan.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !followerRan.Load() {
		t.Fatal("leadership never passed to the waiting runner")
	}

	stopFollower()
	if err := <-secondDone; err != nil {
		t.Fatalf("second runner: %v", err)
	}
}

// TestRunSurvivesAFailingJob: a job that errors must be retried, not fatal.
// The other replicas are no better placed to run it, so exiting would only
// move the outage.
func TestRunSurvivesAFailingJob(t *testing.T) {
	pool := dbtest.Pool(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var attempts atomic.Int64
	r := leader.New(pool, "testjob3", 10*time.Millisecond, nil)
	done := make(chan error, 1)
	go func() {
		done <- r.Run(ctx, func(context.Context) error {
			attempts.Add(1)
			return context.DeadlineExceeded
		})
	}()

	deadline := time.Now().Add(3 * time.Second)
	for attempts.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if attempts.Load() < 3 {
		t.Fatalf("a failing job was retried %d times, want at least 3", attempts.Load())
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v after cancellation, want nil", err)
	}
}

// TestReleasesLockOnShutdown: the replacement must not have to wait out a TCP
// timeout, so the lock is handed back explicitly on the way out.
func TestReleasesLockOnShutdown(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx, cancel := context.WithCancel(context.Background())

	started := make(chan struct{})
	r := leader.New(pool, "testjob4", 10*time.Millisecond, nil)
	done := make(chan error, 1)
	go func() {
		done <- r.Run(ctx, func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return nil
		})
	}()

	<-started
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The lock must be free the instant Run returns.
	var held bool
	if err := pool.QueryRow(context.Background(),
		"select pg_try_advisory_lock($1)", leader.Key("testjob4")).Scan(&held); err != nil {
		t.Fatalf("probe lock: %v", err)
	}
	if !held {
		t.Fatal("the lock was still held after the runner returned")
	}
	if _, err := pool.Exec(context.Background(),
		"select pg_advisory_unlock($1)", leader.Key("testjob4")); err != nil {
		t.Fatalf("release probe lock: %v", err)
	}
}

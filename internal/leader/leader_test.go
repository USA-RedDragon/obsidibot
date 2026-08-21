package leader_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/dbtest"
	"github.com/USA-RedDragon/obsidibot/internal/leader"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// connector opens the dedicated connections a Runner locks on, in the same
// schema as pool so a test job sees the same tables the code under test would.
// Every Runner in these tests gets its own session, exactly as in production.
func connector(t *testing.T, pool *pgxpool.Pool) leader.Connector {
	t.Helper()
	return leader.DialConfig(pool.Config().ConnConfig)
}

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
		r := leader.New(connector(t, pool), "testjob1", 10*time.Millisecond, nil)
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

	first := leader.New(connector(t, pool), "testjob2", 10*time.Millisecond, nil)
	firstDone := make(chan error, 1)
	go func() { firstDone <- first.Run(leaderCtx, hold(&leaderRan)) }()

	// Let the first take the lock before the second starts contesting it.
	time.Sleep(150 * time.Millisecond)
	if !leaderRan.Load() {
		t.Fatal("the first runner never took the job")
	}

	second := leader.New(connector(t, pool), "testjob2", 10*time.Millisecond, nil)
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
	r := leader.New(connector(t, pool), "testjob3", 10*time.Millisecond, nil)
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
	r := leader.New(connector(t, pool), "testjob4", 10*time.Millisecond, nil)
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

// TestLeadershipDoesNotBorrowFromTheRequestPool is the outage this package was
// changed to prevent, expressed as a test.
//
// A session advisory lock is held for the whole life of the process, so taking
// it on a POOLED connection removes that connection from the pool permanently.
// With six singletons and a pool sized from NumCPU, a small node ran out of
// connections and every slash command, kill webhook and /readyz blocked in
// Acquire until its deadline. Here: a pool of two, three jobs holding
// leadership, and foreground queries that must still be served immediately.
func TestLeadershipDoesNotBorrowFromTheRequestPool(t *testing.T) {
	url := os.Getenv(dbtest.URLEnv)
	if url == "" {
		t.Skipf("%s is not set; skipping database integration test", dbtest.URLEnv)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse %s: %v", dbtest.URLEnv, err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = dbtest.SchemaName()
	// Deliberately tiny: this is the 4-vCPU node that fell over, scaled down so
	// the failure is immediate rather than load-dependent.
	cfg.MaxConns = 2

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	names := []string{"tstpool1", "tstpool2", "tstpool3"}
	var leading sync.WaitGroup
	leading.Add(len(names))
	done := make(chan error, len(names))
	for _, name := range names {
		var once sync.Once
		r := leader.New(connector(t, pool), name, 50*time.Millisecond, nil)
		go func() {
			done <- r.Run(ctx, func(ctx context.Context) error {
				once.Do(leading.Done)
				<-ctx.Done()
				return nil
			})
		}()
	}

	held := make(chan struct{})
	go func() { leading.Wait(); close(held) }()
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("the jobs never took leadership")
	}

	// Not one pooled connection is checked out: leadership is held elsewhere.
	if got := pool.Stat().AcquiredConns(); got != 0 {
		t.Errorf("%d pooled connections are held by leadership, want 0", got)
	}

	// And the pool still answers foreground work, on the same short deadline a
	// slash command would have.
	for i := range int(cfg.MaxConns) {
		queryCtx, queryCancel := context.WithTimeout(ctx, 3*time.Second)
		var one int
		err := pool.QueryRow(queryCtx, "select 1").Scan(&one)
		queryCancel()
		if err != nil {
			t.Fatalf("foreground query %d was starved by leadership: %v", i, err)
		}
	}

	cancel()
	for range names {
		if err := <-done; err != nil {
			t.Errorf("runner returned %v", err)
		}
	}
}

// TestDialConfigToleratesPoolOnlySettings covers a connection string that is
// perfectly valid for the pool and not for a plain connection.
//
// pgxpool understands pool_max_conns and strips it; pgx.ParseConfig does not,
// and hands it to the server as a runtime parameter, which answers
// `unrecognized configuration parameter`. Dialling the raw DSN here would
// therefore leave every background job retrying forever on a deployment that
// happens to write its URL that way -- silently, because losing a leadership
// contest is not an error either. Dialling from the pool's parsed config is
// what makes the two agree.
func TestDialConfigToleratesPoolOnlySettings(t *testing.T) {
	url := os.Getenv(dbtest.URLEnv)
	if url == "" {
		t.Skipf("%s is not set; skipping database integration test", dbtest.URLEnv)
	}

	separator := "?"
	if strings.Contains(url, "?") {
		separator = "&"
	}
	cfg, err := pgxpool.ParseConfig(url + separator + "pool_max_conns=8")
	if err != nil {
		t.Fatalf("parse pooled url: %v", err)
	}

	conn, err := leader.DialConfig(cfg.ConnConfig)(context.Background())
	if err != nil {
		t.Fatalf("a pool-only setting in the URL broke the leadership connection: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	var one int
	if err := conn.QueryRow(context.Background(), "select 1").Scan(&one); err != nil {
		t.Fatalf("query on the leadership connection: %v", err)
	}
}

// TestTheDedicatedConnectionIsClosedOnRelease: the connection leadership sits
// on is not pooled, so nothing else will ever clean it up. A Runner that
// stopped leading and left its session open would leak one Postgres backend
// every time the job changed hands.
func TestTheDedicatedConnectionIsClosedOnRelease(t *testing.T) {
	pool := dbtest.Pool(t)

	// application_name is how this test tells its own sessions apart from every
	// other connection to a shared test database.
	const marker = "obsidibot-leader-release-test"
	base := pool.Config().ConnConfig
	connect := func(ctx context.Context) (*pgx.Conn, error) {
		cfg := base.Copy()
		cfg.RuntimeParams["application_name"] = marker
		return pgx.ConnectConfig(ctx, cfg)
	}

	backends := func() int {
		var n int
		if err := pool.QueryRow(context.Background(),
			"select count(*) from pg_stat_activity where application_name = $1", marker).Scan(&n); err != nil {
			t.Fatalf("count sessions: %v", err)
		}
		return n
	}

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	r := leader.New(connect, "testjob6", 10*time.Millisecond, nil)
	done := make(chan error, 1)
	go func() {
		done <- r.Run(ctx, func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return nil
		})
	}()

	<-started
	if got := backends(); got == 0 {
		t.Fatal("the leader holds no dedicated session; the lock is on someone else's connection")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Postgres reaps a terminated backend asynchronously, so poll rather than
	// assert instantly.
	deadline := time.Now().Add(5 * time.Second)
	for backends() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := backends(); got != 0 {
		t.Errorf("%d leader sessions outlived the runner", got)
	}
}

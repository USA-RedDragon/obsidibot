package metrics_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/metrics"
)

// TestNewRegistersEverything is the test that catches a field added to Metrics
// and then never wired to the registry, which is silent otherwise: the caller
// increments happily and the series never appears in a scrape.
func TestNewRegistersEverything(t *testing.T) {
	t.Parallel()

	m := metrics.New()

	// Touch every field, so every family has at least one series.
	m.InteractionsTotal.WithLabelValues("bank", metrics.ResultOK).Inc()
	m.InteractionDuration.WithLabelValues("bank").Observe(0.01)
	m.RCONCommandsTotal.WithLabelValues("whisper", metrics.ResultError).Inc()
	m.RCONDuration.Observe(0.02)
	m.KillEventsIngestedTotal.WithLabelValues(metrics.ResultDuplicate).Inc()
	m.KillFeedBacklog.Set(1)
	m.RatingBacklog.Set(2)
	m.RatingUpdatesTotal.WithLabelValues("true").Inc()
	m.BankOperationsTotal.WithLabelValues("withdraw", metrics.ResultNeedsReview).Inc()
	m.BankNeedsReview.Set(3)
	m.LeaderboardLastSuccess.Set(1700000000)
	m.LeaderTransitionsTotal.WithLabelValues("killfeed").Inc()
	m.DBErrorsTotal.Inc()

	body := string(m.Registry.Gather())
	for _, name := range []string{
		"obsidibot_interactions_total",
		"obsidibot_interaction_duration_seconds",
		"obsidibot_rcon_commands_total",
		"obsidibot_rcon_duration_seconds",
		"obsidibot_kill_events_ingested_total",
		"obsidibot_kill_feed_backlog",
		"obsidibot_rating_backlog",
		"obsidibot_rating_updates_total",
		"obsidibot_bank_operations_total",
		"obsidibot_bank_needs_review",
		"obsidibot_leaderboard_last_success_timestamp_seconds",
		"obsidibot_leader_transitions_total",
		"obsidibot_db_errors_total",
	} {
		if !strings.Contains(body, "# TYPE "+name+" ") {
			t.Errorf("%s is not in the scrape", name)
		}
	}

	validate(t, body)
}

// TestRuntimeSeries checks the replacement for NewGoCollector and
// NewProcessCollector: a small set, but the set an operator actually reads.
func TestRuntimeSeries(t *testing.T) {
	t.Parallel()

	body := string(metrics.New().Registry.Gather())
	for _, name := range []string{
		"go_goroutines",
		"go_gomaxprocs",
		"go_memstats_alloc_bytes",
		"go_memstats_alloc_bytes_total",
		"go_memstats_sys_bytes",
		"go_memstats_heap_objects",
		"go_gc_cycles_total",
		"go_gc_cpu_seconds_total",
		"go_info",
	} {
		if !strings.Contains(body, "# TYPE "+name+" ") {
			t.Errorf("runtime series %s is missing", name)
		}
	}

	values := seriesValues(t, body)
	if values["go_goroutines"] < 1 {
		t.Errorf("go_goroutines = %v, want at least 1", values["go_goroutines"])
	}
	if values["go_memstats_alloc_bytes"] <= 0 {
		t.Errorf("go_memstats_alloc_bytes = %v, want a positive number", values["go_memstats_alloc_bytes"])
	}

	var sawInfo bool
	for series, value := range values {
		if strings.HasPrefix(series, "go_info{version=\"go") && value == 1 {
			sawInfo = true
		}
	}
	if !sawInfo {
		t.Errorf("go_info is missing or malformed in:\n%s", body)
	}
}

// TestRuntimeSeriesAreLive checks the values are read at scrape time rather
// than captured once at New, which would make them worse than useless.
func TestRuntimeSeriesAreLive(t *testing.T) {
	t.Parallel()

	m := metrics.New()
	before := seriesValues(t, string(m.Registry.Gather()))["go_goroutines"]

	release := make(chan struct{})
	var running sync.WaitGroup
	for range 25 {
		running.Add(1)
		go func() {
			defer running.Done()
			<-release
		}()
	}

	// Give the goroutines a moment to actually exist.
	deadline := time.Now().Add(2 * time.Second)
	var after float64
	for time.Now().Before(deadline) {
		after = seriesValues(t, string(m.Registry.Gather()))["go_goroutines"]
		if after > before {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	running.Wait()

	if after <= before {
		t.Errorf("go_goroutines did not move: %v then %v", before, after)
	}
}

// TestConcurrentUpdates is the -race test. Many goroutines mint and increment
// children of the same families while another scrapes.
func TestConcurrentUpdates(t *testing.T) {
	t.Parallel()

	m := metrics.New()

	const (
		writers = 16
		each    = 500
		kinds   = 4
	)

	stop := make(chan struct{})
	var scraping sync.WaitGroup
	scraping.Add(1)
	go func() {
		defer scraping.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if len(m.Registry.Gather()) == 0 {
				t.Error("scrape came back empty")
				return
			}
		}
	}()

	var writing sync.WaitGroup
	for w := range writers {
		writing.Add(1)
		go func() {
			defer writing.Done()
			for i := range each {
				command := "cmd" + strconv.Itoa((w+i)%kinds)
				m.InteractionsTotal.WithLabelValues(command, metrics.ResultOK).Inc()
				m.InteractionDuration.WithLabelValues(command).Observe(float64(i%100) / 1000)
				m.RCONDuration.Observe(0.001)
				m.DBErrorsTotal.Inc()
				m.KillFeedBacklog.Set(float64(i))
			}
		}()
	}
	writing.Wait()
	close(stop)
	scraping.Wait()

	values := seriesValues(t, string(m.Registry.Gather()))

	if got := values["obsidibot_db_errors_total"]; got != writers*each {
		t.Errorf("obsidibot_db_errors_total = %v, want %v", got, writers*each)
	}
	if got := values[`obsidibot_rcon_duration_seconds_count`]; got != writers*each {
		t.Errorf("rcon duration count = %v, want %v", got, writers*each)
	}

	var interactions float64
	for series, value := range values {
		if strings.HasPrefix(series, "obsidibot_interactions_total{") {
			interactions += value
		}
	}
	if interactions != writers*each {
		t.Errorf("interactions summed to %v, want %v", interactions, writers*each)
	}
}

// TestCardinalityIsBounded is the load-bearing one.
//
// The package doc forbids labelling a metric with a player name; this asserts
// that a caller who ignores that produces a bounded registry rather than an
// unbounded leak. It is not a licence to ignore the rule: the resulting
// series are useless, they are just not fatal.
func TestCardinalityIsBounded(t *testing.T) {
	t.Parallel()

	m := metrics.New()

	// Exactly the thing the doc forbids: a per-player label.
	for i := range 10000 {
		m.InteractionsTotal.WithLabelValues("bank", "player-"+strconv.Itoa(i)).Inc()
	}

	var series int
	for _, line := range strings.Split(string(m.Registry.Gather()), "\n") {
		if strings.HasPrefix(line, "obsidibot_interactions_total{") {
			series++
		}
	}

	// The cap plus the single overflow series. The exact cap is an
	// implementation detail; that it is a constant, and nowhere near 10000, is
	// the property under test.
	if series > 300 {
		t.Errorf("10000 distinct label values produced %d series; the family is unbounded", series)
	}
	if series < 2 {
		t.Errorf("got %d series, want the family to still work below its cap", series)
	}

	body := string(m.Registry.Gather())
	if !strings.Contains(body, "__overflow__") {
		t.Error("past the cap there is no overflow series to make the problem visible")
	}

	// Nothing was lost: every increment is still counted somewhere.
	var total float64
	for series, value := range seriesValues(t, body) {
		if strings.HasPrefix(series, "obsidibot_interactions_total{") {
			total += value
		}
	}
	if total != 10000 {
		t.Errorf("increments summed to %v, want 10000; the cap dropped counts", total)
	}

	validate(t, body)
}

// TestServeScrape starts the real listener, scrapes it over HTTP the way
// Prometheus would, and checks both the headers and the body.
func TestServeScrape(t *testing.T) {
	t.Parallel()

	m := metrics.New()
	m.InteractionsTotal.WithLabelValues("bank", metrics.ResultOK).Inc()
	m.RCONDuration.Observe(0.03)

	port := freePort(t)
	ctx, cancel := context.WithCancel(t.Context())

	served := make(chan error, 1)
	go func() { served <- m.Serve(ctx, "127.0.0.1", port) }()

	url := fmt.Sprintf("http://127.0.0.1:%d/metrics", port)
	resp := getWhenUp(t, url)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scrape returned %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	validate(t, string(body))
	if !strings.Contains(string(body), `obsidibot_interactions_total{command="bank",result="ok"} 1`) {
		t.Errorf("the scrape is missing the interaction that was recorded:\n%s", body)
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve returned %v, want nil after a clean shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("Serve did not return after its context was cancelled")
	}
}

// TestServeOnlyMetrics pins the deliberate absence of health endpoints here.
func TestServeOnlyMetrics(t *testing.T) {
	t.Parallel()

	m := metrics.New()
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Registry)
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("/healthz returned %d, want 404: health lives on the interactions listener", rec.Code)
	}
}

// TestRegistriesAreIndependent is what lets every package's tests call New
// without fighting over a global.
func TestRegistriesAreIndependent(t *testing.T) {
	t.Parallel()

	first, second := metrics.New(), metrics.New()
	first.DBErrorsTotal.Add(5)

	if got := seriesValues(t, string(second.Registry.Gather()))["obsidibot_db_errors_total"]; got != 0 {
		t.Errorf("a second registry sees %v db errors, want 0", got)
	}
	if got := seriesValues(t, string(first.Registry.Gather()))["obsidibot_db_errors_total"]; got != 5 {
		t.Errorf("the first registry sees %v db errors, want 5", got)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T, want *net.TCPAddr", ln.Addr())
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("release the port: %v", err)
	}
	return addr.Port
}

func getWhenUp(t *testing.T, url string) *http.Response {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			return resp
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the metrics listener never came up: %v", errors.Join(lastErr))
	return nil
}

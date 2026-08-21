package ingest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/USA-RedDragon/obsidibot/internal/config"
	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/dbtest"
	"github.com/USA-RedDragon/obsidibot/internal/ingest"
	"github.com/USA-RedDragon/obsidibot/internal/metrics"
)

const (
	testSecret     = "0123456789abcdef0123456789abcdef"
	testServerGUID = "63a86971-0cb9-4569-a43a-4b05317f2d73"
)

func migrationsFS(t *testing.T) fs.FS {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	return os.DirFS(filepath.Join(filepath.Dir(thisFile), "..", "..", "schema", "migrations"))
}

// serve builds the ingest handler on a real database and returns a client for
// it. The route carries the secret, so this exercises the real path matching.
func serve(t *testing.T) (*httptest.Server, *db.Store) {
	t.Helper()
	pool := dbtest.Pool(t)
	dbtest.Reset(t, pool)
	if err := db.Migrate(context.Background(), pool, migrationsFS(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := db.NewStore(pool)

	cfg := &config.Config{
		Ingest: config.Ingest{Secret: testSecret},
		Rating: config.Rating{Initial: 1200},
	}

	ts := httptest.NewServer(ingest.New(store, metrics.New(), cfg, testServerGUID).Handler())
	t.Cleanup(ts.Close)
	return ts, store
}

func post(t *testing.T, ts *httptest.Server, secret, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		fmt.Sprintf("%s/webhooks/pot/%s/killed", ts.URL, secret), strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func countEvents(t *testing.T, store *db.Store) int64 {
	t.Helper()
	n, err := store.Queries().CountUnratedEvents(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// TestWrongSecretIsRefused is the authentication boundary. The game signs
// nothing, so this secret plus not being internet-reachable is the whole
// defence.
func TestWrongSecretIsRefused(t *testing.T) {
	ts, store := serve(t)

	for _, secret := range []string{
		"", "wrong", strings.ToUpper(testSecret),
		testSecret[:len(testSecret)-1],
		testSecret + "x",
	} {
		resp := post(t, ts, secret, documentedPayload)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("secret %q got status %d, want 404", secret, resp.StatusCode)
		}
	}
	if n := countEvents(t, store); n != 0 {
		t.Fatalf("%d events were recorded from unauthenticated requests", n)
	}
}

// TestCorrectSecretRecordsTheEvent is the positive control.
func TestCorrectSecretRecordsTheEvent(t *testing.T) {
	ts, store := serve(t)

	resp := post(t, ts, testSecret, documentedPayload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if n := countEvents(t, store); n != 1 {
		t.Fatalf("%d events recorded, want 1", n)
	}

	events, err := store.Queries().NextUnratedEvents(context.Background(), 10)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	event := events[0]
	if event.VictimAgid != "048-236-424" || *event.KillerAgid != "123-430-121" {
		t.Errorf("ids stored wrong: %q / %v", event.VictimAgid, event.KillerAgid)
	}
	if !event.Credited {
		t.Error("a plain PvP kill was stored as uncredited")
	}
	if event.Rated || event.Posted {
		t.Error("a freshly ingested event was already marked processed")
	}
	// The raw payload is kept so a rule change can be replayed, coordinates
	// and all -- the rule is that nothing RENDERS them.
	if !strings.Contains(string(event.Payload), "VictimLocation") {
		t.Error("the raw payload was not preserved")
	}
}

// TestEventsFromAnotherServerAreRefused: a second game server pointed here by
// mistake would otherwise merge its kills into this server's ratings.
func TestEventsFromAnotherServerAreRefused(t *testing.T) {
	ts, store := serve(t)

	other := strings.Replace(documentedPayload, testServerGUID, "11111111-2222-3333-4444-555555555555", 1)
	resp := post(t, ts, testSecret, other)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403", resp.StatusCode)
	}
	if n := countEvents(t, store); n != 0 {
		t.Fatalf("%d events recorded from another server", n)
	}
}

// TestRedeliveryIsCountedNotDoubled: the game may retry, and a retry must not
// award the kill twice. 200 so it stops retrying.
func TestRedeliveryIsCountedNotDoubled(t *testing.T) {
	ts, store := serve(t)

	for range 3 {
		resp := post(t, ts, testSecret, documentedPayload)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d, want 200 so the game stops retrying", resp.StatusCode)
		}
	}
	if n := countEvents(t, store); n != 1 {
		t.Fatalf("%d events after three identical deliveries, want 1", n)
	}
}

// TestMalformedBodiesAreRefused: a body without a victim has no subject, and
// accepting it would put an unusable row into the ordered rating queue.
func TestMalformedBodiesAreRefused(t *testing.T) {
	ts, store := serve(t)

	bodies := map[string]string{
		"not json":          "{{{",
		"empty object":      "{}",
		"no victim id":      `{"ServerGuid":"` + testServerGUID + `","DamageType":"DT_ATTACK","VictimName":"x"}`,
		"blank victim id":   `{"ServerGuid":"` + testServerGUID + `","DamageType":"DT_ATTACK","VictimAlderonId":"   "}`,
		"json but an array": `[]`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			resp := post(t, ts, testSecret, body)
			if resp.StatusCode == http.StatusOK {
				t.Errorf("status 200 for %s", name)
			}
		})
	}
	if n := countEvents(t, store); n != 0 {
		t.Fatalf("%d malformed events were recorded", n)
	}
}

// TestOversizedBodyIsRefused keeps an unauthenticated-ish endpoint from being a
// memory sink.
func TestOversizedBodyIsRefused(t *testing.T) {
	ts, store := serve(t)

	var payload map[string]any
	if err := json.Unmarshal([]byte(documentedPayload), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	payload["Padding"] = strings.Repeat("a", 128<<10)
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	resp := post(t, ts, testSecret, string(body))
	if resp.StatusCode == http.StatusOK {
		t.Fatal("an oversized body was accepted")
	}
	if n := countEvents(t, store); n != 0 {
		t.Fatalf("%d oversized events were recorded", n)
	}
}

// TestEnvironmentalDeathIsRecordedAsUncredited: it still has to arrive, because
// it is still a death.
func TestEnvironmentalDeathIsRecordedAsUncredited(t *testing.T) {
	ts, store := serve(t)

	body := `{"ServerGuid":"` + testServerGUID + `","DamageType":"DT_THIRST",` +
		`"VictimName":"Test1","VictimAlderonId":"048-236-424","VictimDinosaurType":"Dilophosaurus",` +
		`"VictimGrowth":0.5,"KillerAlderonId":"","KillerName":""}`
	if resp := post(t, ts, testSecret, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	events, err := store.Queries().NextUnratedEvents(context.Background(), 10)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("%d events, want 1", len(events))
	}
	if events[0].Credited {
		t.Error("an environmental death was credited")
	}
	if !events[0].CountsDeath {
		t.Error("an environmental death was not counted as a death; surviving is part of playing")
	}
	if events[0].KillerAgid != nil {
		t.Errorf("an environmental death stored a killer: %v", *events[0].KillerAgid)
	}
	if events[0].KillerGrowth != nil {
		t.Error("an environmental death stored a killer growth of 0 instead of NULL")
	}
}

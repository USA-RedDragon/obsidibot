package ingest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/config"
	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/dbtest"
	"github.com/USA-RedDragon/obsidibot/internal/gamecmd"
	"github.com/USA-RedDragon/obsidibot/internal/ingest"
	"github.com/USA-RedDragon/obsidibot/internal/metrics"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	testSecret     = "0123456789abcdef0123456789abcdef"
	testServerGUID = "63a86971-0cb9-4569-a43a-4b05317f2d73"
	// testAGID is the Alderon ID the recorded payloads in this package carry.
	testAGID = "048-236-424"
)

// serve builds the ingest handler on a real database and returns a client for
// it. The route carries the secret, so this exercises the real path matching.
func serve(t *testing.T) (*httptest.Server, *db.Store, *pgxpool.Pool) {
	t.Helper()
	return serveWith(t, &commandRecorder{}, metrics.New())
}

// serveWith is serve with the command dispatcher named, so the command route's
// tests can see what was handed to it.
func serveWith(t *testing.T, commands ingest.Commands, m *metrics.Metrics) (
	*httptest.Server, *db.Store, *pgxpool.Pool,
) {
	t.Helper()
	pool := dbtest.Pool(t)
	dbtest.Reset(t, pool)
	if err := db.Migrate(context.Background(), pool, dbtest.MigrationsFS(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := db.NewStore(pool)

	cfg := &config.Config{
		Ingest: config.Ingest{Secret: testSecret},
		Rating: config.Rating{Initial: 1200},
	}

	ts := httptest.NewServer(ingest.New(store, m, cfg, testServerGUID, commands).Handler())
	t.Cleanup(ts.Close)
	return ts, store, pool
}

// commandRecorder stands in for the dispatcher: it records what the route
// handed over instead of reaching a database, RCON and a bank, so these tests
// are about the ROUTE -- what it accepts, what it refuses, and what it passes
// on -- and nothing else.
type commandRecorder struct {
	mu       sync.Mutex
	received []gamecmd.Incoming
}

func (c *commandRecorder) Dispatch(_ context.Context, in gamecmd.Incoming) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.received = append(c.received, in)
}

func (c *commandRecorder) Wait(context.Context) error { return nil }

func (c *commandRecorder) Budget() time.Duration { return time.Second }

func (c *commandRecorder) all() []gamecmd.Incoming {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]gamecmd.Incoming(nil), c.received...)
}

// postCommand delivers a PlayerCommand webhook.
func postCommand(t *testing.T, ts *httptest.Server, secret, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		fmt.Sprintf("%s/webhooks/pot/%s/command", ts.URL, secret), strings.NewReader(body))
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

// commandBody is a real PlayerCommand delivery. The game reuses its chat
// payload for these, so the fields it carries and does not carry are the chat
// ones -- and the Message arrives WITH its prefix.
func commandBody(guid, message string) string {
	return fmt.Sprintf(`{"ServerGuid":%q,"ChannelId":0,"ChannelName":"Global",
		"PlayerName":"testplayer","AlderonId":%q,"Message":%q,
		"bServerAdmin":false,"FromWhisper":false}`, guid, testAGID, message)
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
	ts, store, _ := serve(t)

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
	ts, store, pool := serve(t)

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
	if event.VictimAgid != testAGID || *event.KillerAgid != "123-430-121" {
		t.Errorf("ids stored wrong: %q / %v", event.VictimAgid, event.KillerAgid)
	}
	if !event.Credited {
		t.Error("a plain PvP kill was stored as uncredited")
	}
	if event.Rated || event.Posted {
		t.Error("a freshly ingested event was already marked processed")
	}
	// The raw payload is still stored for replay, even though the worker
	// queries deliberately no longer select it.
	var payload []byte
	if err := pool.QueryRow(context.Background(),
		"select payload from kill_events where id = $1", event.ID).Scan(&payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if !strings.Contains(string(payload), "VictimLocation") {
		t.Error("the raw payload was not preserved")
	}
}

// TestEventsFromAnotherServerAreRefused: a second game server pointed here by
// mistake would otherwise merge its kills into this server's ratings.
func TestEventsFromAnotherServerAreRefused(t *testing.T) {
	ts, store, _ := serve(t)

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
	ts, store, _ := serve(t)

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
	ts, store, _ := serve(t)

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
	ts, store, _ := serve(t)

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
	ts, store, _ := serve(t)

	body := `{"ServerGuid":"` + testServerGUID + `","DamageType":"DT_THIRST",` +
		`"VictimName":"Test1","VictimAlderonId":"` + testAGID + `","VictimDinosaurType":"Dilophosaurus",` +
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

// TestCommandRouteAcceptsADelivery is the happy path: the fields the
// dispatcher needs arrive intact, INCLUDING the message's prefix, which the
// parser needs because the escape character is server configuration.
func TestCommandRouteAcceptsADelivery(t *testing.T) {
	rec := &commandRecorder{}
	ts, _, _ := serveWith(t, rec, metrics.New())

	resp := postCommand(t, ts, testSecret, commandBody(testServerGUID, "!deposit 1,000"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}

	got := rec.all()
	if len(got) != 1 {
		t.Fatalf("%d commands dispatched, want 1", len(got))
	}
	if got[0].AGID != testAGID {
		t.Errorf("agid = %q", got[0].AGID)
	}
	if got[0].PlayerName != "testplayer" {
		t.Errorf("player name = %q", got[0].PlayerName)
	}
	if got[0].Message != "!deposit 1,000" {
		t.Errorf("message = %q, want the prefix kept", got[0].Message)
	}
}

// TestCommandRouteRefusesTheWrongSecret: the secret in the path is this
// endpoint's only credential, on this route exactly as on the kill one.
func TestCommandRouteRefusesTheWrongSecret(t *testing.T) {
	rec := &commandRecorder{}
	ts, _, _ := serveWith(t, rec, metrics.New())

	resp := postCommand(t, ts, "not-the-secret", commandBody(testServerGUID, "!balance"))
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status %d, want 404", resp.StatusCode)
	}
	if len(rec.all()) != 0 {
		t.Error("a command with the wrong secret was dispatched")
	}
}

// TestCommandRouteRefusesAnotherServer: a second game server pointed here
// would otherwise whisper this server's players in reply to its own.
func TestCommandRouteRefusesAnotherServer(t *testing.T) {
	rec := &commandRecorder{}
	ts, _, _ := serveWith(t, rec, metrics.New())

	resp := postCommand(t, ts, testSecret,
		commandBody("11111111-2222-3333-4444-555555555555", "!balance"))
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status %d, want 403", resp.StatusCode)
	}
	if len(rec.all()) != 0 {
		t.Error("a command from another server was dispatched")
	}
}

// TestCommandRouteRefusesUnusableIdentifiers is the injection guard. The
// Alderon ID is not merely stored on this route: it becomes the target of an
// RCON whisper, so anything that could carry a second command with it is
// refused at the door rather than deeper in.
func TestCommandRouteRefusesUnusableIdentifiers(t *testing.T) {
	for name, agid := range map[string]string{
		"empty":     "",
		"blank":     "   ",
		"injection": testAGID + " 100",
		"newline":   testAGID + "\nBan " + testAGID,
	} {
		t.Run(name, func(t *testing.T) {
			rec := &commandRecorder{}
			ts, _, _ := serveWith(t, rec, metrics.New())

			body, err := json.Marshal(map[string]any{
				"ServerGuid": testServerGUID,
				"PlayerName": "testplayer",
				"AlderonId":  agid,
				"Message":    "!balance",
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			resp := postCommand(t, ts, testSecret, string(body))
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status %d, want 400", resp.StatusCode)
			}
			if len(rec.all()) != 0 {
				t.Errorf("a command with agid %q was dispatched", agid)
			}
		})
	}
}

// TestCommandRouteRefusesMalformedBodies.
func TestCommandRouteRefusesMalformedBodies(t *testing.T) {
	rec := &commandRecorder{}
	ts, _, _ := serveWith(t, rec, metrics.New())

	if resp := postCommand(t, ts, testSecret, "{not json"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
	if resp := postCommand(t, ts, testSecret,
		strings.Repeat("a", 70<<10)); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d, want 413", resp.StatusCode)
	}
	if len(rec.all()) != 0 {
		t.Error("a malformed command was dispatched")
	}
}

// TestDuplicateCommandsAreBothDispatched: this route deliberately does NOT
// dedupe. The payload carries no event id and no timestamp, so two identical
// deliveries are indistinguishable from one player typing the same thing
// twice -- which is a thing players do. Collapsing them would silently swallow
// a real request; the bank's cooldown and one-in-flight index are what make
// the duplicate safe.
func TestDuplicateCommandsAreBothDispatched(t *testing.T) {
	rec := &commandRecorder{}
	ts, _, _ := serveWith(t, rec, metrics.New())

	body := commandBody(testServerGUID, "!balance")
	for range 2 {
		if resp := postCommand(t, ts, testSecret, body); resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d, want 200", resp.StatusCode)
		}
	}
	if got := rec.all(); len(got) != 2 {
		t.Fatalf("%d commands dispatched, want 2", len(got))
	}
}

// TestRefusedDeliveriesAreCounted: a Game.ini pointed at the wrong secret, or
// at another server's bot, otherwise looks exactly like nobody typing
// anything. Accepted deliveries are deliberately NOT counted here -- the
// dispatcher counts those under the command the player actually typed.
func TestRefusedDeliveriesAreCounted(t *testing.T) {
	rec := &commandRecorder{}
	m := metrics.New()
	ts, _, _ := serveWith(t, rec, m)

	postCommand(t, ts, "wrong", commandBody(testServerGUID, "!balance"))
	postCommand(t, ts, testSecret, commandBody(testServerGUID, "!balance"))

	body := scrape(t, m)
	want := `obsidibot_game_commands_total{command="delivery",result="rejected"} 1`
	if !strings.Contains(body, want) {
		t.Errorf("scrape does not contain %q:\n%s", want, body)
	}
	if strings.Contains(body, `command="delivery",result="ok"`) {
		t.Error("an accepted delivery was counted at the route as well as at the dispatcher")
	}
}

// scrape renders the registry exactly as a Prometheus scrape would see it.
func scrape(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Registry.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

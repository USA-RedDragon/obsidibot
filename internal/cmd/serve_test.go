package cmd_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/cmd"
	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/dbtest"
)

// redirectTo sends every Discord REST call to the fake server instead, keeping
// the path so the fake can tell the calls apart.
func redirectTo(base string) http.RoundTripper {
	target, err := url.Parse(base)
	if err != nil {
		panic(err)
	}
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		clone.URL.Scheme = target.Scheme
		clone.URL.Host = target.Host
		return http.DefaultTransport.RoundTrip(clone)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fakeDiscord answers the handful of REST calls a gateway-less bot makes, and
// records what got posted so the test can see the pipeline's output.
type fakeDiscord struct {
	mu        sync.Mutex
	posted    []string
	registrd  int
	guildPath string
}

func (f *fakeDiscord) start(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/users/@me"):
			// token verification
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/commands"):
			f.registrd++
			if parts := strings.Split(r.URL.Path, "/guilds/"); len(parts) == 2 {
				f.guildPath = strings.TrimSuffix(parts[1], "/commands")
			}
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/messages"):
			f.posted = append(f.posted, string(body))
		}
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/users/@me/guilds"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "g1", "name": "Obsidian Wilds"}})
		case strings.HasSuffix(r.URL.Path, "/users/@me"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "999", "username": "obsidibot"})
		case r.Method == http.MethodPut:
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "1", "name": "link"}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "msg-1"})
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func (f *fakeDiscord) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.posted...)
}

func (f *fakeDiscord) registrations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registrd
}

// registeredGuild reports which guild the commands were registered into, read
// out of the request path.
func (f *fakeDiscord) registeredGuild() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.guildPath
}

func migrationsFS(t *testing.T) fs.FS {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	return os.DirFS(filepath.Join(filepath.Dir(thisFile), "..", "..", "schema", "migrations"))
}

// writeConfig produces a config file for one replica, on its own ports.
func writeConfig(t *testing.T, dir, dsn string, base, rconPort int) string {
	t.Helper()
	path := filepath.Join(dir, fmt.Sprintf("replica-%d.yaml", base))
	body := fmt.Sprintf(`logLevel: warn
interactions: {port: %d}
ingest: {port: %d, secret: "0123456789abcdef0123456789abcdef"}
metrics: {enabled: true, port: %d}
pprof: {enabled: false, port: %d}
database: {url: "%s"}
discord:
  token: faketoken
  applicationId: "1"
  publicKey: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
rcon: {host: 127.0.0.1, port: %d, password: hunter2, timeoutSeconds: 2}
leaderboard: {intervalSeconds: 5}
`, base, base+1, base+2, base+3, dsn, rconPort)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestTwoReplicasServeTogether is the horizontal-scalability requirement,
// exercised rather than asserted.
//
// It starts TWO complete instances against ONE database and checks the three
// things that make that safe:
//
//   - both answer HTTP, so a load balancer can send traffic to either;
//   - both discover the guild they serve rather than being told it;
//   - the single-writer jobs run on exactly one of them, so ratings are not
//     computed twice and the leaderboard is not fought over;
//   - a kill posted to either replica flows all the way through to a rating and
//     a feed message.
func TestTwoReplicasServeTogether(t *testing.T) {
	pool := dbtest.Pool(t)
	dbtest.Reset(t, pool)

	dsn := os.Getenv(dbtest.URLEnv)
	if dsn == "" {
		t.Skipf("%s is not set", dbtest.URLEnv)
	}
	// The replicas must land in this package's schema like everything else.
	dsn += "?search_path=" + dbtest.SchemaName()

	discord := &fakeDiscord{}
	ts := discord.start(t)

	// discordgo resolves its endpoints into package-level variables at init, so
	// they cannot be repointed afterwards. Redirecting the HTTP client is what
	// works, and it leaves no global state for other tests to trip over.
	client := &http.Client{Transport: redirectTo(ts.URL)}

	dir := t.TempDir()
	const baseA, baseB = 39100, 39200
	game := startFakeRCON(t)
	configs := []string{
		writeConfig(t, dir, dsn, baseA, game.port()),
		writeConfig(t, dir, dsn, baseB, game.port()),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for _, path := range configs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := cmd.Serve(ctx, path, migrationsFS(t), cmd.WithDiscordHTTPClient(client)); err != nil {
				t.Errorf("replica %s: %v", path, err)
			}
		}()
	}
	t.Cleanup(func() { cancel(); wg.Wait() })

	// Both replicas answer, on the interactions port -- the one Discord talks
	// to, and the only one carrying health endpoints.
	for _, port := range []int{baseA, baseB} {
		waitFor(t, fmt.Sprintf("http://127.0.0.1:%d/readyz", port))
	}

	// Exactly one replica registered the commands: bulk overwrite counts
	// against Discord's daily limits, so a second would be waste.
	if got := discord.registrations(); got != 1 {
		t.Errorf("%d replicas registered commands, want exactly 1", got)
	}

	// The config named neither the guild nor the server GUID. Both replicas got
	// here, so both discovered the guild from the bot's own membership and the
	// GUID from the game -- and the registration above went to the discovered
	// guild.
	if got := discord.registeredGuild(); got != "g1" {
		t.Errorf("commands were registered into guild %q, want the discovered g1", got)
	}
	var askedTheGame bool
	for _, cmd := range game.issued() {
		if strings.EqualFold(cmd, "ServerInfo") {
			askedTheGame = true
		}
	}
	if !askedTheGame {
		t.Error("no replica asked the game for its GUID; discovery did not run")
	}

	// Configure the feed and the board, then post a kill to ONE replica.
	store := db.NewStore(pool)
	channel := "channel-1"
	if err := store.Queries().SetKillFeedChannel(ctx, gen.SetKillFeedChannelParams{
		GuildID: "g1", KillFeedChannelID: &channel,
	}); err != nil {
		t.Fatalf("set kill channel: %v", err)
	}
	if err := store.Queries().SetLeaderboardChannel(ctx, gen.SetLeaderboardChannelParams{
		GuildID: "g1", LeaderboardChannelID: &channel,
	}); err != nil {
		t.Fatalf("set board channel: %v", err)
	}

	// The payload carries the GUID the fake game reports, so it is only
	// accepted if discovery actually took effect.
	postKill(t, fmt.Sprintf("http://127.0.0.1:%d/webhooks/pot/%s/killed",
		baseA+1, "0123456789abcdef0123456789abcdef"))

	// The kill must be rated and fed through, whichever replica holds the jobs.
	deadline := time.Now().Add(25 * time.Second)
	var rated bool
	for time.Now().Before(deadline) {
		killer, err := store.Queries().GetPlayer(ctx, "123-430-121")
		if err == nil && killer.Kills == 1 && killer.Rating > 1200 {
			rated = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !rated {
		t.Fatal("a kill posted to one replica was never rated by either")
	}

	// And it reached the channel.
	var fed bool
	for time.Now().Before(deadline) {
		for _, msg := range discord.messages() {
			if strings.Contains(msg, "Test2") {
				fed = true
			}
		}
		if fed {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !fed {
		t.Error("the kill never reached the feed channel")
	}

	// Exactly one leaderboard message exists, not one per replica.
	var boards int
	for _, msg := range discord.messages() {
		if strings.Contains(msg, "Top Players") {
			boards++
		}
	}
	if boards > 1 {
		t.Errorf("%d leaderboard messages were posted; both replicas ran the job", boards)
	}
}

func waitFor(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s never became ready", url)
}

const killPayload = `{"ServerGuid":"63a86971-0cb9-4569-a43a-4b05317f2d73","DamageType":"DT_ATTACK",` +
	`"VictimName":"Test1","VictimAlderonId":"048-236-424","VictimDinosaurType":"Dilophosaurus","VictimGrowth":0.5,` +
	`"KillerName":"Test2","KillerAlderonId":"123-430-121","KillerDinosaurType":"Dilophosaurus","KillerGrowth":0.5}`

func postKill(t *testing.T, url string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url,
		strings.NewReader(killPayload))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post kill: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ingest returned %d", resp.StatusCode)
	}
}

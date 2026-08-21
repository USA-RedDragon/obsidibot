package board_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/board"
	"github.com/USA-RedDragon/obsidibot/internal/config"
	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/dbtest"
	"github.com/USA-RedDragon/obsidibot/internal/metrics"
	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fakeMessenger records posts and edits, and can be told to fail an edit the
// way Discord does when the message has been deleted.
type fakeMessenger struct {
	mu        sync.Mutex
	posts     []*discordgo.MessageSend
	edits     []*discordgo.MessageEdit
	editFails bool
	nextID    int
}

func (f *fakeMessenger) ChannelMessageSendComplex(_ string, data *discordgo.MessageSend,
	_ ...discordgo.RequestOption,
) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.posts = append(f.posts, data)
	return &discordgo.Message{ID: fmt.Sprintf("msg-%d", f.nextID)}, nil
}

func (f *fakeMessenger) ChannelMessageEditComplex(edit *discordgo.MessageEdit,
	_ ...discordgo.RequestOption,
) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.editFails {
		return nil, errors.New("404: Unknown Message")
	}
	f.edits = append(f.edits, edit)
	return &discordgo.Message{ID: edit.ID}, nil
}

func (f *fakeMessenger) counts() (posts, edits int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts), len(f.edits)
}

func (f *fakeMessenger) lastRendered() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.edits) > 0 {
		e := f.edits[len(f.edits)-1]
		if e.Embeds != nil && len(*e.Embeds) > 0 {
			return (*e.Embeds)[0].Description
		}
	}
	if len(f.posts) > 0 {
		p := f.posts[len(f.posts)-1]
		if len(p.Embeds) > 0 {
			return p.Embeds[0].Description
		}
	}
	return ""
}

func migrationsFS(t *testing.T) fs.FS {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	return os.DirFS(filepath.Join(filepath.Dir(thisFile), "..", "..", "schema", "migrations"))
}

type harness struct {
	pool  *pgxpool.Pool
	store *db.Store
	cfg   *config.Config
	msg   *fakeMessenger
	board *board.Board
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	pool := dbtest.Pool(t)
	dbtest.Reset(t, pool)
	if err := db.Migrate(context.Background(), pool, migrationsFS(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := db.NewStore(pool)
	cfg := &config.Config{
		Leaderboard: config.Leaderboard{IntervalSeconds: 60, Size: 20},
		Rating:      config.Rating{Initial: 1200},
	}
	msg := &fakeMessenger{}
	return &harness{
		pool: pool, store: store, cfg: cfg, msg: msg,
		board: board.New(store, msg, metrics.New(), cfg, "g1"),
	}
}

// tick runs one refresh by starting Run (which refreshes immediately) and
// stopping it before the ticker fires again.
func (h *harness) tick(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.board.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("board: %v", err)
	}
}

func (h *harness) setChannel(t *testing.T, channelID string) {
	t.Helper()
	if err := h.store.Queries().SetLeaderboardChannel(context.Background(),
		gen.SetLeaderboardChannelParams{GuildID: "g1", LeaderboardChannelID: &channelID}); err != nil {
		t.Fatalf("set channel: %v", err)
	}
}

func (h *harness) seed(t *testing.T, agid, name string, rating float64, kills, deaths int32, discordID *string) {
	t.Helper()
	ctx := context.Background()
	q := h.store.Queries()
	if err := q.UpsertPlayerSeen(ctx, gen.UpsertPlayerSeenParams{
		AlderonID: agid, LastKnownName: name, Rating: rating,
	}); err != nil {
		t.Fatalf("seed player: %v", err)
	}
	if _, err := h.pool.Exec(ctx,
		"update players set rating=$2, kills=$3, deaths=$4 where alderon_id=$1",
		agid, rating, kills, deaths); err != nil {
		t.Fatalf("seed stats: %v", err)
	}
	if discordID != nil {
		if err := q.CreateLink(ctx, gen.CreateLinkParams{
			DiscordUserID: *discordID, AlderonID: agid,
		}); err != nil {
			t.Fatalf("seed link: %v", err)
		}
	}
}

// TestBoardPostsThenEditsInPlace is the whole point of a persistent message:
// a scoreboard, not a stream of replaced posts.
func TestBoardPostsThenEditsInPlace(t *testing.T) {
	h := newHarness(t)
	h.setChannel(t, "channel-1")
	h.seed(t, "111-111-111", "alice", 1400, 10, 2, nil)

	h.tick(t)
	posts, edits := h.msg.counts()
	if posts != 1 || edits != 0 {
		t.Fatalf("first refresh: %d posts %d edits, want 1 and 0", posts, edits)
	}

	h.tick(t)
	posts, edits = h.msg.counts()
	if posts != 1 || edits != 1 {
		t.Fatalf("second refresh: %d posts %d edits, want 1 and 1", posts, edits)
	}
}

// TestBoardRecoversFromADeletedMessage. Somebody will delete it. Without this
// the board silently stops updating forever.
func TestBoardRecoversFromADeletedMessage(t *testing.T) {
	h := newHarness(t)
	h.setChannel(t, "channel-1")
	h.seed(t, "111-111-111", "alice", 1400, 10, 2, nil)

	h.tick(t)
	h.msg.editFails = true
	h.tick(t)

	posts, _ := h.msg.counts()
	if posts != 2 {
		t.Fatalf("%d posts after the message was deleted, want a replacement", posts)
	}

	// And the new id was remembered, so the next tick edits rather than
	// posting a third.
	h.msg.editFails = false
	h.tick(t)
	posts, edits := h.msg.counts()
	if posts != 2 || edits != 1 {
		t.Fatalf("after recovery: %d posts %d edits, want 2 and 1", posts, edits)
	}
}

// TestChangingChannelRepostsRatherThanEditingTheOld: the stored id names a
// message in the channel nobody is reading any more.
func TestChangingChannelRepostsRatherThanEditingTheOld(t *testing.T) {
	h := newHarness(t)
	h.setChannel(t, "channel-1")
	h.seed(t, "111-111-111", "alice", 1400, 10, 2, nil)
	h.tick(t)

	h.setChannel(t, "channel-2")
	h.tick(t)

	posts, edits := h.msg.counts()
	if posts != 2 {
		t.Fatalf("%d posts after moving the channel, want 2", posts)
	}
	if edits != 0 {
		t.Fatalf("the board edited the message in the old channel %d times", edits)
	}
	h.msg.mu.Lock()
	defer h.msg.mu.Unlock()
	if len(h.msg.posts) != 2 {
		t.Fatal("expected a fresh post in the new channel")
	}
}

// TestBoardOrdersByRatingAndShowsWhatPlayersCompare.
func TestBoardOrdersByRatingAndShowsWhatPlayersCompare(t *testing.T) {
	h := newHarness(t)
	h.setChannel(t, "channel-1")
	h.seed(t, "111-111-111", "alice", 1500, 20, 5, nil)
	h.seed(t, "222-222-222", "bob", 1300, 30, 30, nil)
	h.seed(t, "333-333-333", "carol", 1700, 8, 1, nil)
	h.tick(t)

	rendered := h.msg.lastRendered()
	alicePos := strings.Index(rendered, "alice")
	bobPos := strings.Index(rendered, "bob")
	carolPos := strings.Index(rendered, "carol")
	if carolPos < 0 || alicePos < 0 || bobPos < 0 {
		t.Fatalf("not everyone rendered: %q", rendered)
	}
	if carolPos >= alicePos || alicePos >= bobPos {
		t.Errorf("board is not ordered by rating:\n%s", rendered)
	}

	// Each row carries what players actually compare.
	for _, want := range []string{"20 kills", "5 deaths", "4.00", "1700"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("board does not show %q:\n%s", want, rendered)
		}
	}
}

// TestBoardShowsUnlinkedPlayersByName is what makes it a ranking of the SERVER
// rather than of the subset who use the bot.
func TestBoardShowsUnlinkedPlayersByName(t *testing.T) {
	h := newHarness(t)
	h.setChannel(t, "channel-1")
	linked := "discord-42"
	h.seed(t, "111-111-111", "alice", 1500, 20, 5, &linked)
	h.seed(t, "222-222-222", "bob", 1300, 30, 30, nil)
	h.tick(t)

	rendered := h.msg.lastRendered()
	if !strings.Contains(rendered, "<@discord-42>") {
		t.Errorf("a linked player did not render as a mention:\n%s", rendered)
	}
	if !strings.Contains(rendered, "bob") {
		t.Errorf("an unlinked player is missing from the board:\n%s", rendered)
	}
}

// TestBoardNeverPings: up to twenty people named every minute.
func TestBoardNeverPings(t *testing.T) {
	h := newHarness(t)
	h.setChannel(t, "channel-1")
	linked := "discord-42"
	h.seed(t, "111-111-111", "alice", 1500, 20, 5, &linked)
	h.tick(t)
	h.tick(t)

	h.msg.mu.Lock()
	defer h.msg.mu.Unlock()
	for _, post := range h.msg.posts {
		if post.AllowedMentions == nil || len(post.AllowedMentions.Parse) != 0 {
			t.Error("a board post could ping twenty people")
		}
	}
	for _, edit := range h.msg.edits {
		if edit.AllowedMentions == nil || len(edit.AllowedMentions.Parse) != 0 {
			t.Error("a board edit could ping twenty people")
		}
	}
}

// TestBoardHandlesAnEmptyServer: a fresh install must render something rather
// than an empty embed Discord rejects.
func TestBoardHandlesAnEmptyServer(t *testing.T) {
	h := newHarness(t)
	h.setChannel(t, "channel-1")
	h.tick(t)

	if rendered := h.msg.lastRendered(); rendered == "" {
		t.Fatal("an empty leaderboard rendered an empty description")
	}
}

// TestBoardDoesNothingWithoutAChannel: no channel is a normal state before a
// moderator has run /config, not an error.
func TestBoardDoesNothingWithoutAChannel(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "111-111-111", "alice", 1400, 10, 2, nil)
	h.tick(t)

	if posts, edits := h.msg.counts(); posts != 0 || edits != 0 {
		t.Fatalf("the board wrote somewhere with no channel configured: %d posts %d edits", posts, edits)
	}
}

// TestBoardRespectsSize keeps the embed inside Discord's description limit.
func TestBoardRespectsSize(t *testing.T) {
	h := newHarness(t)
	h.cfg.Leaderboard.Size = 3
	h.setChannel(t, "channel-1")
	for i := range 10 {
		h.seed(t, fmt.Sprintf("%03d-000-000", i+1), fmt.Sprintf("player%d", i), float64(1200+i*10), 5, 1, nil)
	}
	h.tick(t)

	rendered := h.msg.lastRendered()
	if strings.Count(rendered, "`#") != 3 {
		t.Fatalf("board rendered %d rows, want 3:\n%s", strings.Count(rendered, "`#"), rendered)
	}
}

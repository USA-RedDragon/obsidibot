package kills_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/kills"
	"github.com/USA-RedDragon/obsidibot/internal/metrics"
	"github.com/bwmarrin/discordgo"
)

// fakePoster records what the feed would have sent to Discord.
type fakePoster struct {
	mu    sync.Mutex
	sends []*discordgo.MessageSend
	// failUntil makes the first N sends fail, so the lossless promise can be
	// tested against an outage.
	failUntil int
	attempts  int
	// err, when set, makes every send fail with it.
	err error
}

func (f *fakePoster) attemptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

func (f *fakePoster) ChannelMessageSendComplex(_ string, data *discordgo.MessageSend,
	_ ...discordgo.RequestOption,
) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.err != nil {
		return nil, f.err
	}
	if f.attempts <= f.failUntil {
		return nil, errors.New("discord is having a moment")
	}
	f.sends = append(f.sends, data)
	return &discordgo.Message{ID: "m1"}, nil
}

// rendered flattens each posted embed into one string, so a test can assert on
// what a reader would actually see regardless of which part carries it.
func (f *fakePoster) descriptions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.sends))
	for _, send := range f.sends {
		for _, e := range send.Embeds {
			var b strings.Builder
			b.WriteString(e.Title + "\n" + e.Description)
			for _, field := range e.Fields {
				b.WriteString("\n" + field.Name + ": " + field.Value)
			}
			if e.Footer != nil {
				b.WriteString("\n" + e.Footer.Text)
			}
			out = append(out, b.String())
		}
	}
	return out
}

// feedChannel is where the tests point the kill feed.
const feedChannel = "channel-1"

func (h *harness) setKillChannel(t *testing.T) {
	t.Helper()
	channelID := feedChannel
	if err := h.store.Queries().SetKillFeedChannel(context.Background(), gen.SetKillFeedChannelParams{
		GuildID: "g1", KillFeedChannelID: &channelID,
	}); err != nil {
		t.Fatalf("set kill channel: %v", err)
	}
}

// runFeed drains the feed once and stops.
func (h *harness) runFeed(t *testing.T, poster kills.Poster) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	feed := kills.NewFeed(h.store, poster, metrics.New(), h.cfg, "g1")
	done := make(chan error, 1)
	go func() { done <- feed.Run(ctx) }()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		n, err := h.store.Queries().CountUnpostedEvents(ctx)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("feed: %v", err)
	}
}

// TestFeedPostsInOrder: the feed is a narrative, so it has to read in the order
// things happened.
func TestFeedPostsInOrder(t *testing.T) {
	h := newHarness(t)
	h.setKillChannel(t)
	h.enqueue(t, alice, bob, "DT_ATTACK", false)
	h.enqueue(t, bob, carol, "DT_ATTACK", false)
	h.enqueue(t, "", alice, "DT_THIRST", false)
	h.runApplier(t)

	poster := &fakePoster{}
	h.runFeed(t, poster)

	got := poster.descriptions()
	if len(got) != 3 {
		t.Fatalf("%d messages posted, want 3", len(got))
	}
	if !strings.Contains(got[0], "player-"+alice) || !strings.Contains(got[0], "player-"+bob) {
		t.Errorf("first message: %q", got[0])
	}
	if !strings.Contains(got[2], "died of thirst") {
		t.Errorf("environmental death rendered as %q", got[2])
	}
}

// TestFeedIsLossless: a Discord outage must delay the feed, not drop it. This
// is the property that justifies the unbounded backlog and its gauge.
func TestFeedIsLossless(t *testing.T) {
	h := newHarness(t)
	h.setKillChannel(t)
	h.enqueue(t, alice, bob, "DT_ATTACK", false)
	h.enqueue(t, bob, carol, "DT_ATTACK", false)
	h.runApplier(t)

	// Every send fails for a while, then recovers.
	poster := &fakePoster{failUntil: 3}
	h.runFeed(t, poster)

	if len(poster.descriptions()) != 2 {
		t.Fatalf("%d messages survived the outage, want 2", len(poster.descriptions()))
	}
	n, err := h.store.Queries().CountUnpostedEvents(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d events still unposted after recovery", n)
	}
}

// TestFeedNeverPings: the feed names players constantly, and notifying them
// every time they die would make the channel unusable.
func TestFeedNeverPings(t *testing.T) {
	h := newHarness(t)
	h.setKillChannel(t)
	h.enqueue(t, alice, bob, "DT_ATTACK", false)
	h.runApplier(t)

	poster := &fakePoster{}
	h.runFeed(t, poster)

	poster.mu.Lock()
	defer poster.mu.Unlock()
	for _, send := range poster.sends {
		if send.AllowedMentions == nil || len(send.AllowedMentions.Parse) != 0 {
			t.Fatal("a feed message could ping the players it names")
		}
	}
}

// TestFeedSkipsWhenNoChannelIsSet. Holding a backlog until a moderator sets a
// channel would mean dumping a month of history the moment they do, which is
// worse than starting the feed from that point.
func TestFeedSkipsWhenNoChannelIsSet(t *testing.T) {
	h := newHarness(t)
	h.enqueue(t, alice, bob, "DT_ATTACK", false)
	h.runApplier(t)

	poster := &fakePoster{}
	h.runFeed(t, poster)

	if len(poster.descriptions()) != 0 {
		t.Fatal("a message was posted with no channel configured")
	}
	n, err := h.store.Queries().CountUnpostedEvents(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d events are queued waiting for a channel to be configured", n)
	}
}

// TestFeedEscapesHostileNames: names come from the game, so a player can name
// themselves markdown.
func TestFeedEscapesHostileNames(t *testing.T) {
	h := newHarness(t)
	h.setKillChannel(t)

	ctx := context.Background()
	killer := "111-111-111"
	hostile := "**@everyone** ||gotcha||"
	if _, err := h.store.Queries().InsertKillEvent(ctx, gen.InsertKillEventParams{
		DedupeKey:  []byte("hostile-name-key-padding-32bytes"),
		ServerGuid: "guid",
		Payload:    []byte(`{}`),
		VictimAgid: bob, VictimName: hostile,
		KillerAgid: &killer, KillerName: &hostile,
		DamageType: "DT_ATTACK", Credited: true, CountsDeath: true,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	h.runApplier(t)

	poster := &fakePoster{}
	h.runFeed(t, poster)

	for _, description := range poster.descriptions() {
		if strings.Contains(description, "@everyone") {
			t.Errorf("an unescaped @everyone reached the feed: %q", description)
		}
		if strings.Contains(description, "||gotcha||") {
			t.Errorf("unescaped spoiler markup reached the feed: %q", description)
		}
	}
}

// TestPermissionFailureIsNotHammered. A missing channel permission cannot be
// fixed by retrying, so the feed must back off for minutes rather than issuing
// roughly one doomed request per second forever — which is what it did in
// production: 100 identical 403s in two minutes, burning Discord API quota and
// burying every other log line.
//
// The queue is durable, so waiting costs nothing: the backlog drains the moment
// somebody grants the permission.
func TestPermissionFailureIsNotHammered(t *testing.T) {
	h := newHarness(t)
	h.setKillChannel(t)
	h.enqueue(t, alice, bob, "DT_ATTACK", false)
	h.runApplier(t)

	poster := &fakePoster{err: &discordgo.RESTError{
		Message: &discordgo.APIErrorMessage{
			Code: discordgo.ErrCodeMissingPermissions, Message: "Missing Permissions",
		},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	feed := kills.NewFeed(h.store, poster, metrics.New(), h.cfg, "g1")
	done := make(chan error, 1)
	go func() { done <- feed.Run(ctx) }()

	// Long enough that the one-second transient backoff would have produced
	// several attempts.
	time.Sleep(3 * time.Second)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("feed: %v", err)
	}

	if got := poster.attemptCount(); got > 1 {
		t.Errorf("a permanent permission failure was retried %d times in 3s; "+
			"it must back off instead of hammering Discord", got)
	}

	// And nothing was dropped: the event is still queued for when it is fixed.
	unposted, err := h.store.Queries().CountUnpostedEvents(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if unposted != 1 {
		t.Fatalf("%d events queued after a blocked post, want 1 — the feed must stay lossless", unposted)
	}
}

// TestTransientFailureIsRetriedPromptly is the other half: a rate limit or a
// 5xx must NOT get the long backoff, or the feed stalls for minutes over a
// hiccup.
func TestTransientFailureIsRetriedPromptly(t *testing.T) {
	h := newHarness(t)
	h.setKillChannel(t)
	h.enqueue(t, alice, bob, "DT_ATTACK", false)
	h.runApplier(t)

	poster := &fakePoster{err: errors.New("HTTP 502 Bad Gateway")}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	feed := kills.NewFeed(h.store, poster, metrics.New(), h.cfg, "g1")
	done := make(chan error, 1)
	go func() { done <- feed.Run(ctx) }()
	time.Sleep(2500 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("feed: %v", err)
	}

	if got := poster.attemptCount(); got < 2 {
		t.Errorf("a transient failure was retried only %d times in 2.5s; "+
			"it should retry on the short backoff", got)
	}
}

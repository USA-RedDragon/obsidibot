package interactions_test

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/interactions"
	"github.com/USA-RedDragon/obsidibot/internal/metrics"
	"github.com/bwmarrin/discordgo"
)

// signTimestamp is the timestamp every signature in these tests covers. The
// tests that care about it change the HEADER after signing, which is the
// forgery worth catching.
const signTimestamp = "1700000000"

// signed builds a request signed exactly the way Discord signs one: the
// signature covers timestamp || body.
func signed(t *testing.T, priv ed25519.PrivateKey, body []byte) *http.Request {
	t.Helper()
	timestamp := signTimestamp
	sig := ed25519.Sign(priv, append([]byte(timestamp), body...))
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	req.Header.Set("X-Signature-Ed25519", hex.EncodeToString(sig))
	req.Header.Set("X-Signature-Timestamp", timestamp)
	return req
}

// fakeEditor stands in for the Discord REST call that delivers a deferred
// reply, so the background half of the deferred path can be observed.
type fakeEditor struct {
	mu     sync.Mutex
	edits  []*discordgo.WebhookEdit
	err    error
	edited chan struct{}
}

func newFakeEditor() *fakeEditor {
	return &fakeEditor{edited: make(chan struct{}, 8)}
}

func (f *fakeEditor) InteractionResponseEdit(_ *discordgo.Interaction, e *discordgo.WebhookEdit,
	_ ...discordgo.RequestOption,
) (*discordgo.Message, error) {
	f.mu.Lock()
	f.edits = append(f.edits, e)
	f.mu.Unlock()
	f.edited <- struct{}{}
	return &discordgo.Message{}, f.err
}

func (f *fakeEditor) last() *discordgo.WebhookEdit {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.edits) == 0 {
		return nil
	}
	return f.edits[len(f.edits)-1]
}

func newRouter(t *testing.T, commands []interactions.Command) (*interactions.Router, ed25519.PrivateKey) {
	t.Helper()
	router, priv, _ := newRouterWithEditor(t, commands)
	return router, priv
}

func newRouterWithEditor(t *testing.T, commands []interactions.Command) (*interactions.Router, ed25519.PrivateKey, *fakeEditor) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	editor := newFakeEditor()
	router, err := interactions.NewRouter(hex.EncodeToString(pub), editor, metrics.New(), commands)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return router, priv, editor
}

func commandBody(t *testing.T, name string, member *discordgo.Member) []byte {
	t.Helper()
	payload := map[string]any{
		"type":     int(discordgo.InteractionApplicationCommand),
		"id":       "1",
		"token":    "tok",
		"guild_id": "g1",
		"data":     map[string]any{"id": "c1", "name": name, "type": 1},
	}
	if member != nil {
		payload["member"] = member
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

func echoCommand(name string, reply interactions.Reply) interactions.Command {
	return interactions.Command{
		Definition: &discordgo.ApplicationCommand{Name: name, Description: "test"},
		Handler: func(context.Context, interactions.Context) (interactions.Reply, error) {
			return reply, nil
		},
	}
}

// TestPingIsAnsweredWithPong is what Discord does when the endpoint URL is
// saved. If this breaks, the URL cannot be configured at all.
func TestPingIsAnsweredWithPong(t *testing.T) {
	router, priv := newRouter(t, nil)
	body := []byte(`{"type":1}`)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, signed(t, priv, body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	var resp discordgo.InteractionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Type != discordgo.InteractionResponsePong {
		t.Fatalf("type %d, want PONG (%d)", resp.Type, discordgo.InteractionResponsePong)
	}
}

// TestUnsignedRequestsAreRefused is THE security test for this package. The
// signature is the only thing separating Discord from anyone who finds the URL,
// and every one of these forgeries must get 401 without reaching a handler.
func TestUnsignedRequestsAreRefused(t *testing.T) {
	var handlerRan bool
	router, priv := newRouter(t, []interactions.Command{{
		Definition: &discordgo.ApplicationCommand{Name: "ping", Description: "t"},
		Handler: func(context.Context, interactions.Context) (interactions.Reply, error) {
			handlerRan = true
			return interactions.Reply{Content: "pong"}, nil
		},
	}})

	body := commandBody(t, "ping", nil)
	otherPub, otherPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	_ = otherPub

	tests := []struct {
		name    string
		mutate  func(*http.Request)
		newBody []byte
	}{
		{"no signature headers at all", func(r *http.Request) {
			r.Header.Del("X-Signature-Ed25519")
			r.Header.Del("X-Signature-Timestamp")
		}, body},
		{"missing signature", func(r *http.Request) { r.Header.Del("X-Signature-Ed25519") }, body},
		{"missing timestamp", func(r *http.Request) { r.Header.Del("X-Signature-Timestamp") }, body},
		{"signature is not hex", func(r *http.Request) { r.Header.Set("X-Signature-Ed25519", "zzzz") }, body},
		{"signature truncated", func(r *http.Request) {
			r.Header.Set("X-Signature-Ed25519", r.Header.Get("X-Signature-Ed25519")[:64])
		}, body},
		{"timestamp swapped after signing", func(r *http.Request) {
			r.Header.Set("X-Signature-Timestamp", "1700000001")
		}, body},
		{"body swapped after signing", func(r *http.Request) {
			r.Body = http.NoBody
			r.Body = httptest.NewRequest(http.MethodPost, "/",
				strings.NewReader(`{"type":2,"data":{"name":"ping"}}`)).Body
		}, body},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handlerRan = false
			req := signed(t, priv, tc.newBody)
			tc.mutate(req)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status %d, want 401", rec.Code)
			}
			if handlerRan {
				t.Error("a forged request reached the handler")
			}
		})
	}

	t.Run("signed with the wrong key", func(t *testing.T) {
		handlerRan = false
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, signed(t, otherPriv, body))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status %d, want 401", rec.Code)
		}
		if handlerRan {
			t.Error("a request signed with another key reached the handler")
		}
	})
}

// TestValidSignatureReachesTheHandler is the positive control: without it the
// test above would pass on a router that refused everything.
func TestValidSignatureReachesTheHandler(t *testing.T) {
	router, priv := newRouter(t, []interactions.Command{
		echoCommand("ping", interactions.Reply{Content: "pong"}),
	})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, signed(t, priv, commandBody(t, "ping", nil)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	var resp discordgo.InteractionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Type != discordgo.InteractionResponseChannelMessageWithSource {
		t.Fatalf("type %d", resp.Type)
	}
	if resp.Data.Content != "pong" {
		t.Fatalf("content %q", resp.Data.Content)
	}
}

// TestRepliesNeverPing: the leaderboard mentions up to twenty people every
// minute. Rendering a mention is wanted; notifying twenty people is not.
func TestRepliesNeverPing(t *testing.T) {
	router, priv := newRouter(t, []interactions.Command{
		echoCommand("stats", interactions.Reply{Content: "<@12345> is rated 1500"}),
	})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, signed(t, priv, commandBody(t, "stats", nil)))

	var resp discordgo.InteractionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data.AllowedMentions == nil {
		t.Fatal("no allowed_mentions set; a reply naming users would ping them")
	}
	if len(resp.Data.AllowedMentions.Parse) != 0 {
		t.Fatalf("allowed_mentions.parse = %v, want empty", resp.Data.AllowedMentions.Parse)
	}
}

// TestManageGuildGate covers the admin commands. The permission bits come from
// the signed payload, so this is the whole check.
func TestManageGuildGate(t *testing.T) {
	var handlerRan bool
	router, priv := newRouter(t, []interactions.Command{{
		Definition:          &discordgo.ApplicationCommand{Name: "config", Description: "t"},
		RequiresManageGuild: true,
		Handler: func(context.Context, interactions.Context) (interactions.Reply, error) {
			handlerRan = true
			return interactions.Reply{Content: "set"}, nil
		},
	}})

	tests := []struct {
		name        string
		member      *discordgo.Member
		wantHandler bool
	}{
		{"no member (a DM)", nil, false},
		{"no permissions", &discordgo.Member{
			User: &discordgo.User{ID: "u1"}, Permissions: 0,
		}, false},
		{"send messages only", &discordgo.Member{
			User: &discordgo.User{ID: "u1"}, Permissions: discordgo.PermissionSendMessages,
		}, false},
		{"manage server", &discordgo.Member{
			User: &discordgo.User{ID: "u1"}, Permissions: discordgo.PermissionManageGuild,
		}, true},
		{"administrator carries manage server", &discordgo.Member{
			User:        &discordgo.User{ID: "u1"},
			Permissions: discordgo.PermissionManageGuild | discordgo.PermissionAdministrator,
		}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handlerRan = false
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, signed(t, priv, commandBody(t, "config", tc.member)))

			if rec.Code != http.StatusOK {
				t.Fatalf("status %d", rec.Code)
			}
			if handlerRan != tc.wantHandler {
				t.Fatalf("handler ran = %v, want %v", handlerRan, tc.wantHandler)
			}
			if !tc.wantHandler {
				var resp discordgo.InteractionResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if !strings.Contains(resp.Data.Content, "Manage Server") {
					t.Errorf("refusal did not explain why: %q", resp.Data.Content)
				}
				// A refusal must not be visible to the whole channel.
				if resp.Data.Flags&discordgo.MessageFlagsEphemeral == 0 {
					t.Error("the refusal was posted publicly")
				}
			}
		})
	}
}

// TestUnknownCommandStillAnswers: a stale registration must not leave the
// caller's interaction hanging.
func TestUnknownCommandStillAnswers(t *testing.T) {
	router, priv := newRouter(t, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, signed(t, priv, commandBody(t, "ghost", nil)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	var resp discordgo.InteractionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data == nil || resp.Data.Content == "" {
		t.Fatal("an unknown command produced an empty reply")
	}
}

// TestDeferredCommandAcksImmediately is the three-second rule. The ACK must go
// out on the original response without waiting for the handler, or Discord
// abandons the interaction and the user sees a failure for work that ran.
func TestDeferredCommandAcksImmediately(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	router, priv := newRouter(t, []interactions.Command{{
		Definition: &discordgo.ApplicationCommand{Name: "deposit", Description: "t"},
		Defer:      true,
		Ephemeral:  true,
		Handler: func(context.Context, interactions.Context) (interactions.Reply, error) {
			<-release
			return interactions.Reply{Content: "done"}, nil
		},
	}})
	defer once.Do(func() { close(release) })

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, signed(t, priv, commandBody(t, "deposit", nil)))
		done <- rec
	}()

	select {
	case rec := <-done:
		var resp discordgo.InteractionResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Type != discordgo.InteractionResponseDeferredChannelMessageWithSource {
			t.Fatalf("type %d, want a deferred ACK (%d)",
				resp.Type, discordgo.InteractionResponseDeferredChannelMessageWithSource)
		}
		if resp.Data.Flags&discordgo.MessageFlagsEphemeral == 0 {
			t.Error("an ephemeral command's ACK was not marked ephemeral, so the reply would be public")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the ACK waited on the handler; Discord would have abandoned the interaction")
	}
}

// TestDuplicateCommandNamesAreRefused: two handlers under one name means one of
// them silently never runs, which is a startup failure rather than a mystery.
func TestDuplicateCommandNamesAreRefused(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	_, err = interactions.NewRouter(hex.EncodeToString(pub), nil, metrics.New(), []interactions.Command{
		echoCommand("dup", interactions.Reply{}),
		echoCommand("dup", interactions.Reply{}),
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate command names were accepted: %v", err)
	}
}

// TestBadPublicKeyIsAStartupFailure: an unusable key must stop the process, not
// turn every interaction into a 401 nobody can explain.
func TestBadPublicKeyIsAStartupFailure(t *testing.T) {
	for _, key := range []string{"", "not-hex", "abcd", strings.Repeat("ab", 31)} {
		if _, err := interactions.NewRouter(key, nil, metrics.New(), nil); err == nil {
			t.Errorf("NewRouter accepted public key %q", key)
		}
	}
}

// TestOversizedBodyIsRefused: the body is read in full by the verifier, so the
// cap has to come first.
func TestOversizedBodyIsRefused(t *testing.T) {
	router, priv := newRouter(t, nil)
	huge := []byte(`{"type":1,"pad":"` + strings.Repeat("a", 2<<20) + `"}`)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, signed(t, priv, huge))
	if rec.Code == http.StatusOK {
		t.Fatal("a body over the cap was accepted")
	}
}

// TestNonCommandInteractionsAreRejected: components and autocomplete are not
// used, and answering them with a PONG would be wrong.
func TestNonCommandInteractionsAreRejected(t *testing.T) {
	router, priv := newRouter(t, nil)
	for _, typ := range []discordgo.InteractionType{
		discordgo.InteractionMessageComponent,
		discordgo.InteractionApplicationCommandAutocomplete,
		discordgo.InteractionModalSubmit,
	} {
		body := []byte(`{"type":` + strconv.Itoa(int(typ)) + `}`)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, signed(t, priv, body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("interaction type %d got status %d, want 400", typ, rec.Code)
		}
	}
}

// TestDeferredReplyIsDelivered covers the second half of the deferred path:
// the ACK proves Discord will wait, this proves the answer actually arrives.
func TestDeferredReplyIsDelivered(t *testing.T) {
	router, priv, editor := newRouterWithEditor(t, []interactions.Command{{
		Definition: &discordgo.ApplicationCommand{Name: "withdraw", Description: "t"},
		Defer:      true,
		Handler: func(context.Context, interactions.Context) (interactions.Reply, error) {
			return interactions.Reply{Content: "withdrew 100 marks"}, nil
		},
	}})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, signed(t, priv, commandBody(t, "withdraw", nil)))

	select {
	case <-editor.edited:
	case <-time.After(3 * time.Second):
		t.Fatal("the deferred reply was never delivered; the user would see 'thinking' forever")
	}
	edit := editor.last()
	if edit == nil || edit.Content == nil || *edit.Content != "withdrew 100 marks" {
		t.Fatalf("edit = %+v, want the handler's content", edit)
	}
}

// TestDeferredHandlerErrorStillAnswers: a failing handler must still replace
// the "thinking" state, or the user is left with a command that never ends.
func TestDeferredHandlerErrorStillAnswers(t *testing.T) {
	router, priv, editor := newRouterWithEditor(t, []interactions.Command{{
		Definition: &discordgo.ApplicationCommand{Name: "deposit", Description: "t"},
		Defer:      true,
		Handler: func(context.Context, interactions.Context) (interactions.Reply, error) {
			return interactions.Reply{}, errors.New("rcon exploded")
		},
	}})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, signed(t, priv, commandBody(t, "deposit", nil)))

	select {
	case <-editor.edited:
	case <-time.After(3 * time.Second):
		t.Fatal("a failing deferred handler left the interaction hanging")
	}
	edit := editor.last()
	if edit == nil || edit.Content == nil || *edit.Content == "" {
		t.Fatal("the failure produced an empty reply")
	}
	// Nothing about the internals may reach the user.
	if strings.Contains(*edit.Content, "rcon exploded") {
		t.Errorf("the internal error text was shown to the user: %q", *edit.Content)
	}
}

// TestDeferredPanicDoesNotKillTheProcess. A panic in a bare goroutine is
// unrecoverable and takes down the whole replica -- one bad command would stop
// the bot answering every other command, ingesting kills, and running the
// ledger. It has to be contained.
func TestDeferredPanicDoesNotKillTheProcess(t *testing.T) {
	router, priv, editor := newRouterWithEditor(t, []interactions.Command{
		{
			Definition: &discordgo.ApplicationCommand{Name: "boom", Description: "t"},
			Defer:      true,
			Handler: func(context.Context, interactions.Context) (interactions.Reply, error) {
				var p *discordgo.User
				_ = p.ID // deliberate nil dereference
				return interactions.Reply{}, nil
			},
		},
		echoCommand("ping", interactions.Reply{Content: "pong"}),
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, signed(t, priv, commandBody(t, "boom", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("the ACK did not go out: status %d", rec.Code)
	}

	// Give the goroutine time to panic and be recovered.
	time.Sleep(200 * time.Millisecond)
	_ = editor

	// The process is still here, and the router still works.
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, signed(t, priv, commandBody(t, "ping", nil)))
	var resp discordgo.InteractionResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data.Content != "pong" {
		t.Fatalf("the router stopped working after a handler panicked: %q", resp.Data.Content)
	}
}

// TestEmptyReplyStillClearsThinking: an edit with no content leaves the user
// staring at "thinking" indefinitely.
func TestEmptyReplyStillClearsThinking(t *testing.T) {
	router, priv, editor := newRouterWithEditor(t, []interactions.Command{{
		Definition: &discordgo.ApplicationCommand{Name: "quiet", Description: "t"},
		Defer:      true,
		Handler: func(context.Context, interactions.Context) (interactions.Reply, error) {
			return interactions.Reply{}, nil
		},
	}})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, signed(t, priv, commandBody(t, "quiet", nil)))

	select {
	case <-editor.edited:
	case <-time.After(3 * time.Second):
		t.Fatal("no edit was delivered")
	}
	edit := editor.last()
	if edit == nil || edit.Content == nil || *edit.Content == "" {
		t.Fatal("an empty reply left the interaction thinking forever")
	}
}

// serveMux returns the listener's real routing table.
func serveMux(t *testing.T, router *interactions.Router, ready func(context.Context) error) http.Handler {
	t.Helper()
	return router.Handler(ready)
}

// TestHealthEndpoints covers the probe surface. These live on the interactions
// listener specifically -- it is the one that always exists and the one Discord
// talks to -- so they must work without any of the interaction machinery.
func TestHealthEndpoints(t *testing.T) {
	router, _ := newRouter(t, nil)

	t.Run("healthz claims only that the process is up", func(t *testing.T) {
		// It must NOT consult the readiness function: a liveness probe that
		// fails on a dependency gets the container restarted for an outage
		// restarting cannot fix.
		var consulted bool
		handler := serveMux(t, router, func(context.Context) error {
			consulted = true
			return errors.New("database is down")
		})

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

		if rec.Code != http.StatusOK {
			t.Errorf("status %d, want 200 even with the database down", rec.Code)
		}
		if consulted {
			t.Error("healthz consulted the readiness check; a dependency outage would restart the container")
		}
	})

	t.Run("readyz reports the database", func(t *testing.T) {
		handler := serveMux(t, router, func(context.Context) error { return nil })
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("status %d, want 200", rec.Code)
		}
	})

	t.Run("readyz fails with the reason", func(t *testing.T) {
		handler := serveMux(t, router, func(context.Context) error {
			return errors.New("database: connection refused")
		})
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status %d, want 503", rec.Code)
		}
		// The body carries the reason. That is only safe because the route in
		// front of this listener is an EXACT match on "/", so these paths are
		// not reachable from outside; see the handler's comment.
		if !strings.Contains(rec.Body.String(), "connection refused") {
			t.Errorf("body %q does not say what is wrong", rec.Body.String())
		}
	})

	t.Run("health paths do not disturb the interaction route", func(t *testing.T) {
		// "POST /{$}" matches the root exactly, so the health paths cannot be
		// swallowed by it and unknown paths still 404 rather than reaching the
		// signature verifier.
		handler := serveMux(t, router, func(context.Context) error { return nil })
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nonsense", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status %d for an unknown path, want 404", rec.Code)
		}
	})
}

// guildLister serves the /users/@me/guilds response discovery reads.
func guildLister(t *testing.T, guilds []map[string]any, status int) *discordgo.Session {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(guilds)
	}))
	t.Cleanup(ts.Close)

	session, err := discordgo.New("Bot token")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	target, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	session.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		clone.URL.Scheme, clone.URL.Host = target.Scheme, target.Host
		return http.DefaultTransport.RoundTrip(clone)
	})}
	return session
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestDiscoverGuild is what lets the guild go unconfigured entirely: on a
// single-guild bot, the guild it is in IS the guild it serves.
func TestDiscoverGuild(t *testing.T) {
	t.Run("one guild is the answer", func(t *testing.T) {
		session := guildLister(t, []map[string]any{
			{"id": "111", "name": "Obsidian Wilds"},
		}, http.StatusOK)

		guild, err := interactions.DiscoverGuild(context.Background(), session)
		if err != nil {
			t.Fatalf("DiscoverGuild: %v", err)
		}
		if guild.ID != "111" {
			t.Errorf("ID = %q", guild.ID)
		}
	})

	t.Run("no guilds says to invite it", func(t *testing.T) {
		session := guildLister(t, []map[string]any{}, http.StatusOK)
		_, err := interactions.DiscoverGuild(context.Background(), session)
		if !errors.Is(err, interactions.ErrNoGuilds) {
			t.Fatalf("err = %v, want ErrNoGuilds", err)
		}
	})

	t.Run("several guilds refuses to guess", func(t *testing.T) {
		// There is no setting to break the tie, and picking one arbitrarily
		// would register commands into a server at random and post a kill feed
		// there. So this fails loudly, names them, and says what to do.
		session := guildLister(t, []map[string]any{
			{"id": "111", "name": "Obsidian Wilds"},
			{"id": "222", "name": "Somewhere Else"},
		}, http.StatusOK)

		_, err := interactions.DiscoverGuild(context.Background(), session)
		if err == nil {
			t.Fatal("DiscoverGuild picked one of two guilds")
		}
		for _, want := range []string{"Obsidian Wilds", "Somewhere Else", "non-public"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	})
}

// TestDeferredPanicStillAnswers: a panicking command must still land an edit.
//
// The ACK is already out by the time a handler runs, so a recover that only
// logs leaves the caller watching "thinking" until Discord gives up -- and
// leaves nothing on a dashboard to say it happened. The rule this package holds
// to everywhere else is that an edit ALWAYS lands.
func TestDeferredPanicStillAnswers(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	editor := newFakeEditor()
	m := metrics.New()
	router, err := interactions.NewRouter(hex.EncodeToString(pub), editor, m, []interactions.Command{{
		Definition: &discordgo.ApplicationCommand{Name: "boom", Description: "t"},
		Defer:      true,
		Handler: func(context.Context, interactions.Context) (interactions.Reply, error) {
			var p *discordgo.User
			_ = p.ID // deliberate nil dereference
			return interactions.Reply{}, nil
		},
	}})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, signed(t, priv, commandBody(t, "boom", nil)))

	select {
	case <-editor.edited:
	case <-time.After(3 * time.Second):
		t.Fatal("a panicking command never edited its reply; the caller is still thinking")
	}
	edit := editor.last()
	if edit == nil || edit.Content == nil || *edit.Content == "" {
		t.Fatal("the edit carried no content")
	}
	if !strings.Contains(*edit.Content, "went wrong") {
		t.Errorf("edit content %q is not the standard failure message", *edit.Content)
	}

	// And it is counted as a failure, so a command that panics on every call is
	// visible as one rather than as a gap in the graph.
	if got := counterValue(t, m, "boom", metrics.ResultError); got != 1 {
		t.Errorf("interactions_total{command=boom,result=error} = %v, want 1", got)
	}
}

// counterValue reads one obsidibot_interactions_total sample off the private
// registry, which is the only place these collectors live.
//
// The registry exposes the Prometheus TEXT format rather than protobuf structs,
// so this parses the one line it wants: the sample name with its label pair, in
// the canonical order the exposition writer emits.
func counterValue(t *testing.T, m *metrics.Metrics, command, result string) float64 {
	t.Helper()
	want := fmt.Sprintf(`obsidibot_interactions_total{command="%s",result="%s"} `, command, result)
	for _, line := range strings.Split(string(m.Registry.Gather()), "\n") {
		value, found := strings.CutPrefix(line, want)
		if !found {
			continue
		}
		got, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			t.Fatalf("unreadable sample %q: %v", line, err)
		}
		return got
	}
	return 0
}

// TestShutdownWaitsForDeferredCommands is the rolling-deploy bug.
//
// http.Server.Shutdown drains ACTIVE requests, and a deferred command has none:
// its ACK went out immediately and the connection went idle, so Shutdown
// returned while the work -- a banking transfer, in the case that hurt -- was
// still running, and the caller then closed the database under it. Serve must
// not return until the deferred goroutines it started have finished.
func TestShutdownWaitsForDeferredCommands(t *testing.T) {
	release := make(chan struct{})
	var finished atomic.Bool
	router, priv, editor := newRouterWithEditor(t, []interactions.Command{{
		Definition: &discordgo.ApplicationCommand{Name: "slow", Description: "t"},
		Defer:      true,
		Handler: func(context.Context, interactions.Context) (interactions.Reply, error) {
			<-release
			finished.Store(true)
			return interactions.Reply{Content: "done"}, nil
		},
	}})

	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan error, 1)
	go func() {
		served <- router.Serve(ctx, "127.0.0.1", port, func(context.Context) error { return nil })
	}()
	waitForHealth(t, fmt.Sprintf("http://127.0.0.1:%d/healthz", port))

	body := commandBody(t, "slow", nil)
	sig := ed25519.Sign(priv, append([]byte(signTimestamp), body...))
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/", port), strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-Signature-Ed25519", hex.EncodeToString(sig))
	req.Header.Set("X-Signature-Timestamp", signTimestamp)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post interaction: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the ACK did not go out: status %d", resp.StatusCode)
	}

	// SIGTERM lands while the command is still working.
	cancel()
	select {
	case err := <-served:
		t.Fatalf("Serve returned (%v) while a deferred command was still running", err)
	case <-time.After(500 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve never returned after the deferred command finished")
	}

	if !finished.Load() {
		t.Error("the deferred command did not finish")
	}
	// The reply had to be delivered BEFORE Serve returned: everything the
	// caller tears down afterwards -- the database above all -- is gone by then.
	if edit := editor.last(); edit == nil || edit.Content == nil || *edit.Content != "done" {
		t.Error("the deferred reply was not delivered before shutdown completed")
	}
}

// freePort picks a port nothing is listening on, so these tests can run beside
// anything else on the machine.
func freePort(t *testing.T) int {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		t.Fatalf("listener reported a %T, want a TCP address", ln.Addr())
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return addr.Port
}

func waitForHealth(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
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
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never came up", url)
}

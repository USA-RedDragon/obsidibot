package interactions_test

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/USA-RedDragon/obsidibot/internal/interactions"
	"github.com/USA-RedDragon/obsidibot/internal/metrics"
	"github.com/bwmarrin/discordgo"
)

// componentBody builds a button press exactly as Discord delivers one.
func componentBody(t *testing.T, customID string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"type":     int(discordgo.InteractionMessageComponent),
		"id":       "1",
		"token":    "tok",
		"guild_id": "g1",
		"data":     map[string]any{"custom_id": customID, "component_type": 2},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

func newComponentRouter(t *testing.T, components ...interactions.Component) (*interactions.Router, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	router, err := interactions.NewRouter(hex.EncodeToString(pub), newFakeEditor(), metrics.New(), nil, components...)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return router, priv
}

// TestComponentIsRoutedByPrefix: one registration owns a whole family of custom
// IDs, because the state a paginated view needs travels in the ID itself.
func TestComponentIsRoutedByPrefix(t *testing.T) {
	var got string
	router, priv := newComponentRouter(t, interactions.Component{
		Prefix: "hist",
		Handler: func(_ context.Context, ic interactions.Context) (interactions.Reply, error) {
			got = ic.Interaction.MessageComponentData().CustomID
			return interactions.Reply{Content: "page"}, nil
		},
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, signed(t, priv, componentBody(t, "hist:555-000-101:48:10")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if got != "hist:555-000-101:48:10" {
		t.Errorf("handler saw custom ID %q", got)
	}

	// The response must REPLACE the message the button is on. Anything else
	// turns paging into a thread of near-identical messages.
	var response discordgo.InteractionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Type != discordgo.InteractionResponseUpdateMessage {
		t.Errorf("response type %d, want an in-place update", response.Type)
	}
}

// TestUnroutedComponentIsAnswered: usually a button from an older version of
// the bot, still sitting in somebody's scrollback. Saying so beats letting the
// click hang until Discord times it out.
func TestUnroutedComponentIsAnswered(t *testing.T) {
	router, priv := newComponentRouter(t)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, signed(t, priv, componentBody(t, "gone:1")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 -- an unrouted press must still be answered", rec.Code)
	}
	if !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("response is not JSON: %q", rec.Body.String())
	}
}

// TestComponentPressesAreStillSigned: a button press is an interaction like any
// other, and the signature check must not have been skipped on the new path.
func TestComponentPressesAreStillSigned(t *testing.T) {
	router, _ := newComponentRouter(t, interactions.Component{
		Prefix: "hist",
		Handler: func(context.Context, interactions.Context) (interactions.Reply, error) {
			t.Error("an unsigned component press reached the handler")
			return interactions.Reply{}, nil
		},
	})

	_, otherPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, signed(t, otherPriv, componentBody(t, "hist:1")))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", rec.Code)
	}
}

// TestComponentPrefixesAreValidatedAtStartup: a prefix containing the delimiter
// could never be matched, so it would ship as a silently dead button.
func TestComponentPrefixesAreValidatedAtStartup(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	noop := func(context.Context, interactions.Context) (interactions.Reply, error) {
		return interactions.Reply{}, nil
	}

	for _, prefix := range []string{"", "has:colon"} {
		if _, err := interactions.NewRouter(hex.EncodeToString(pub), nil, metrics.New(), nil,
			interactions.Component{Prefix: prefix, Handler: noop}); err == nil {
			t.Errorf("prefix %q was accepted", prefix)
		}
	}

	if _, err := interactions.NewRouter(hex.EncodeToString(pub), nil, metrics.New(), nil,
		interactions.Component{Prefix: "dup", Handler: noop},
		interactions.Component{Prefix: "dup", Handler: noop}); err == nil {
		t.Error("a duplicate component prefix was accepted; one handler would silently never run")
	}
}

// TestComponentFailureDoesNotLeakDetail: the same rule as commands -- a handler
// error becomes a generic ephemeral reply, not a database message.
func TestComponentFailureDoesNotLeakDetail(t *testing.T) {
	router, priv := newComponentRouter(t, interactions.Component{
		Prefix: "hist",
		Handler: func(context.Context, interactions.Context) (interactions.Reply, error) {
			return interactions.Reply{}, errTest
		},
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, signed(t, priv, componentBody(t, "hist:1")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, errTest.Error()) {
		t.Errorf("the underlying error reached the caller: %q", body)
	}
}

var errTest = errors.New("a database exploded")

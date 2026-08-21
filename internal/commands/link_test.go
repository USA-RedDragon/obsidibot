package commands_test

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/interactions"
	"github.com/bwmarrin/discordgo"
)

const (
	testAGID    = "746-132-258"
	testName    = "kittykat95"
	discordUser = "discord-1"
)

// linkInvoke builds the interaction a /link subcommand arrives in.
func linkInvoke(sub string, options map[string]string) interactions.Context {
	opts := make([]*discordgo.ApplicationCommandInteractionDataOption, 0, len(options))
	for name, value := range options {
		opts = append(opts, &discordgo.ApplicationCommandInteractionDataOption{
			Name: name, Type: discordgo.ApplicationCommandOptionString, Value: value,
		})
	}
	return interactions.Context{
		UserID:  discordUser,
		GuildID: "g1",
		Interaction: &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
			Type: discordgo.InteractionApplicationCommand,
			Data: discordgo.ApplicationCommandInteractionData{
				Name: "link",
				Options: []*discordgo.ApplicationCommandInteractionDataOption{{
					Name: sub, Type: discordgo.ApplicationCommandOptionSubCommand, Options: opts,
				}},
			},
		}},
	}
}

// TestLinkStartSendsCodeOnlyIntoTheGame is the security property of the whole
// scheme: the code proves control of the in-game account precisely because it
// travels through the game and never appears in Discord. If it leaked into the
// reply, anyone could link anyone.
func TestLinkStartSendsCodeOnlyIntoTheGame(t *testing.T) {
	h := newLinkHarness(t)

	reply, err := h.linker.Command().Handler(context.Background(),
		linkInvoke("start", map[string]string{"player": testAGID}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	code := h.rcon.lastCode(t)
	if code == "" {
		t.Fatal("no code was sent into the game")
	}
	if strings.Contains(reply.Content, code) {
		t.Fatalf("the link code appeared in the Discord reply: %q", reply.Content)
	}

	// And it is not recoverable from the database either.
	challenge, err := h.store.Queries().GetChallenge(context.Background(), discordUser)
	if err != nil {
		t.Fatalf("challenge not stored: %v", err)
	}
	if strings.Contains(string(challenge.CodeHash), code) {
		t.Fatal("the plaintext code was stored in the database")
	}
	sum := sha256.Sum256([]byte(code))
	if string(challenge.CodeHash) != string(sum[:]) {
		t.Fatal("the stored hash is not the SHA-256 of the delivered code")
	}
}

// TestLinkHappyPath walks start -> confirm and checks the whole fact landed:
// link, player row and bank account.
func TestLinkHappyPath(t *testing.T) {
	h := newLinkHarness(t)
	ctx := context.Background()
	handler := h.linker.Command().Handler

	if _, err := handler(ctx, linkInvoke("start", map[string]string{"player": testAGID})); err != nil {
		t.Fatalf("start: %v", err)
	}
	code := h.rcon.lastCode(t)

	reply, err := handler(ctx, linkInvoke("confirm", map[string]string{"code": code}))
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if reply.UserError {
		t.Fatalf("a correct code was refused: %q", reply.Content)
	}

	q := h.store.Queries()
	link, err := q.GetLinkByDiscordID(ctx, discordUser)
	if err != nil {
		t.Fatalf("link not created: %v", err)
	}
	if link.AlderonID != testAGID {
		t.Errorf("linked to %q, want %q", link.AlderonID, testAGID)
	}
	if _, err := q.GetPlayer(ctx, testAGID); err != nil {
		t.Errorf("player row not created: %v", err)
	}
	if _, err := q.GetBankAccount(ctx, testAGID); err != nil {
		t.Errorf("bank account not created: %v", err)
	}
	// The challenge is spent, so a replayed code cannot link again.
	if _, err := q.GetChallenge(ctx, discordUser); err == nil {
		t.Error("the challenge survived a successful confirm")
	}
}

// TestConfirmForgivesFormatting: the player reads the code off a chat line and
// retypes it. Case and stray whitespace must not fail the link.
func TestConfirmForgivesFormatting(t *testing.T) {
	for _, mangle := range []func(string) string{
		strings.ToLower,
		func(s string) string { return "  " + s + " " },
		func(s string) string { return strings.ToLower("  " + s + "\t") },
	} {
		h := newLinkHarness(t)
		ctx := context.Background()
		handler := h.linker.Command().Handler

		if _, err := handler(ctx, linkInvoke("start", map[string]string{"player": testAGID})); err != nil {
			t.Fatalf("start: %v", err)
		}
		reply, err := handler(ctx, linkInvoke("confirm", map[string]string{"code": mangle(h.rcon.lastCode(t))}))
		if err != nil {
			t.Fatalf("confirm: %v", err)
		}
		if reply.UserError {
			t.Errorf("a mangled but correct code was refused: %q", reply.Content)
		}
	}
}

// TestWrongCodesAreBurnedAfterMaxAttempts stops a code being brute-forced
// inside its lifetime.
func TestWrongCodesAreBurnedAfterMaxAttempts(t *testing.T) {
	h := newLinkHarness(t)
	ctx := context.Background()
	handler := h.linker.Command().Handler

	if _, err := handler(ctx, linkInvoke("start", map[string]string{"player": testAGID})); err != nil {
		t.Fatalf("start: %v", err)
	}

	for i := range h.cfg.Link.MaxAttempts {
		reply, err := handler(ctx, linkInvoke("confirm", map[string]string{"code": "WRONG1"}))
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if !reply.UserError {
			t.Fatalf("a wrong code was accepted on attempt %d", i)
		}
	}

	// The challenge is gone, so the real code no longer works either.
	if _, err := h.store.Queries().GetChallenge(ctx, discordUser); err == nil {
		t.Fatal("the challenge survived the attempt limit")
	}
	reply, err := handler(ctx, linkInvoke("confirm", map[string]string{"code": h.rcon.lastCode(t)}))
	if err != nil {
		t.Fatalf("confirm after burn: %v", err)
	}
	if !reply.UserError || !strings.Contains(reply.Content, "no link in progress") {
		t.Fatalf("a burned challenge still accepted its code: %q", reply.Content)
	}
}

// TestExpiredCodeIsRefused: the TTL has to be enforced on use, not only by the
// sweeper, or a code stays good until something happens to clean it up.
func TestExpiredCodeIsRefused(t *testing.T) {
	h := newLinkHarness(t)
	ctx := context.Background()
	handler := h.linker.Command().Handler

	if _, err := handler(ctx, linkInvoke("start", map[string]string{"player": testAGID})); err != nil {
		t.Fatalf("start: %v", err)
	}
	code := h.rcon.lastCode(t)

	// Age the challenge past its TTL.
	if _, err := h.pool.Exec(ctx,
		"update link_challenges set expires_at = now() - interval '1 minute'"); err != nil {
		t.Fatalf("age challenge: %v", err)
	}

	reply, err := handler(ctx, linkInvoke("confirm", map[string]string{"code": code}))
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if !reply.UserError || !strings.Contains(reply.Content, "expired") {
		t.Fatalf("an expired code was accepted: %q", reply.Content)
	}
}

// TestLinkRequiresBeingInGame: the whole proof is that the code reaches someone
// holding the account, so an offline player cannot start.
func TestLinkRequiresBeingInGame(t *testing.T) {
	h := newLinkHarness(t)
	h.rcon.online = false

	reply, err := h.linker.Command().Handler(context.Background(),
		linkInvoke("start", map[string]string{"player": testAGID}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !reply.UserError || !strings.Contains(reply.Content, "logged into the server") {
		t.Fatalf("an offline player was allowed to start a link: %q", reply.Content)
	}
	if _, err := h.store.Queries().GetChallenge(context.Background(), discordUser); err == nil {
		t.Error("a challenge was stored for an offline player")
	}
}

// TestCannotHijackALinkedIdentity: the identity is already someone else's, and
// no amount of /link start may take it.
func TestCannotHijackALinkedIdentity(t *testing.T) {
	h := newLinkHarness(t)
	ctx := context.Background()
	q := h.store.Queries()

	if err := q.UpsertPlayerSeen(ctx, gen.UpsertPlayerSeenParams{
		AlderonID: testAGID, LastKnownName: testName, Rating: 1200,
	}); err != nil {
		t.Fatalf("seed player: %v", err)
	}
	if err := q.CreateLink(ctx, gen.CreateLinkParams{
		DiscordUserID: "someone-else", AlderonID: testAGID,
	}); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	reply, err := h.linker.Command().Handler(ctx, linkInvoke("start", map[string]string{"player": testAGID}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !reply.UserError || !strings.Contains(reply.Content, "already linked to another Discord account") {
		t.Fatalf("a linked identity was offered up for hijack: %q", reply.Content)
	}
	if h.rcon.lastCode(t) != "" {
		t.Error("a code was whispered to an identity the caller does not own")
	}
}

// TestReissueCooldownStopsWhisperSpam. /link start makes a message appear on
// someone else's screen, so an uncapped command is a spam button pointed at
// whoever the caller names.
func TestReissueCooldownStopsWhisperSpam(t *testing.T) {
	h := newLinkHarness(t)
	ctx := context.Background()
	handler := h.linker.Command().Handler

	if _, err := handler(ctx, linkInvoke("start", map[string]string{"player": testAGID})); err != nil {
		t.Fatalf("first start: %v", err)
	}
	sent := h.rcon.messageCount()

	reply, err := handler(ctx, linkInvoke("start", map[string]string{"player": testAGID}))
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	if !reply.UserError {
		t.Fatalf("a second immediate /link start was allowed: %q", reply.Content)
	}
	if h.rcon.messageCount() != sent {
		t.Fatal("the cooldown did not stop a second in-game message")
	}
}

// TestCannotStompAnotherUsersPendingChallenge: without this, naming someone
// else's identity wipes their in-progress link and whispers them a code.
func TestCannotStompAnotherUsersPendingChallenge(t *testing.T) {
	h := newLinkHarness(t)
	ctx := context.Background()

	sum := sha256.Sum256([]byte("ABC123"))
	if err := h.store.Queries().UpsertChallenge(ctx, gen.UpsertChallengeParams{
		DiscordUserID: "someone-else",
		AlderonID:     testAGID,
		PlayerName:    testName,
		CodeHash:      sum[:],
		ExpiresAt:     time.Now().Add(5 * time.Minute),
	}); err != nil {
		t.Fatalf("seed challenge: %v", err)
	}

	reply, err := h.linker.Command().Handler(ctx, linkInvoke("start", map[string]string{"player": testAGID}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !reply.UserError {
		t.Fatalf("another user's pending challenge was stomped: %q", reply.Content)
	}

	kept, err := h.store.Queries().GetChallenge(ctx, "someone-else")
	if err != nil {
		t.Fatalf("the other user's challenge was destroyed: %v", err)
	}
	if string(kept.CodeHash) != string(sum[:]) {
		t.Fatal("the other user's code was replaced")
	}
}

// TestLinkStartRefusesInjection: the player option is user input that lands on
// an RCON command line.
func TestLinkStartRefusesInjection(t *testing.T) {
	h := newLinkHarness(t)
	reply, err := h.linker.Command().Handler(context.Background(),
		linkInvoke("start", map[string]string{"player": "746-132-258 100"}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !reply.UserError {
		t.Fatalf("an injection-shaped identifier was accepted: %q", reply.Content)
	}
	if h.rcon.commandCount() != 0 {
		t.Error("a command reached the game server")
	}
}

// TestStatusAndRemove covers the two read/undo subcommands, including that
// removing preserves what the player earned.
func TestStatusAndRemove(t *testing.T) {
	h := newLinkHarness(t)
	ctx := context.Background()
	handler := h.linker.Command().Handler

	reply, err := handler(ctx, linkInvoke("status", nil))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(reply.Content, "not linked") {
		t.Errorf("status before linking: %q", reply.Content)
	}

	if _, err := handler(ctx, linkInvoke("start", map[string]string{"player": testAGID})); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := handler(ctx, linkInvoke("confirm", map[string]string{"code": h.rcon.lastCode(t)})); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	reply, err = handler(ctx, linkInvoke("status", nil))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(reply.Content, testAGID) || !strings.Contains(reply.Content, testName) {
		t.Errorf("status after linking: %q", reply.Content)
	}

	if err := h.store.Queries().CreditBank(ctx, gen.CreditBankParams{
		AlderonID: testAGID, Balance: 750,
	}); err != nil {
		t.Fatalf("credit: %v", err)
	}

	reply, err = handler(ctx, linkInvoke("remove", nil))
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if reply.UserError {
		t.Errorf("remove refused: %q", reply.Content)
	}
	account, err := h.store.Queries().GetBankAccount(ctx, testAGID)
	if err != nil || account.Balance != 750 {
		t.Errorf("balance did not survive unlink: %d, %v", account.Balance, err)
	}

	reply, err = handler(ctx, linkInvoke("remove", nil))
	if err != nil {
		t.Fatalf("second remove: %v", err)
	}
	if !reply.UserError {
		t.Error("removing a link that does not exist reported success")
	}
}

// TestCodesAreUnpredictable is a smoke test on the generator: repeated codes,
// or a biased alphabet, would make the five-attempt limit meaningless.
func TestCodesAreUnpredictable(t *testing.T) {
	h := newLinkHarness(t)
	ctx := context.Background()

	seen := make(map[string]bool)
	for range 50 {
		if _, err := h.pool.Exec(ctx, "delete from link_challenges"); err != nil {
			t.Fatalf("clear: %v", err)
		}
		if _, err := h.linker.Command().Handler(ctx,
			linkInvoke("start", map[string]string{"player": testAGID})); err != nil {
			t.Fatalf("start: %v", err)
		}
		code := h.rcon.lastCode(t)
		if seen[code] {
			t.Fatalf("code %q was generated twice in 50 draws", code)
		}
		seen[code] = true
		if len(code) != 6 {
			t.Fatalf("code %q is %d characters, want 6", code, len(code))
		}
		if strings.ContainsAny(code, "01OIL") {
			t.Errorf("code %q contains a character that is ambiguous to read off a chat line", code)
		}
	}
}

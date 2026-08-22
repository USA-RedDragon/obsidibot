package commands_test

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/interactions"
	"github.com/USA-RedDragon/obsidibot/internal/linkcode"
	"github.com/bwmarrin/discordgo"
)

const (
	testAGID    = "555-000-101"
	testName    = "testplayer"
	discordUser = "discord-1"
)

// strPtr exists because the challenge queries take *string for the nullable
// discord_user_id and Go cannot take the address of a constant.
func strPtr(s string) *string { return &s }

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
	challenge, err := h.store.Queries().GetChallengeByDiscordID(context.Background(), strPtr(discordUser))
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
	if _, err := q.GetChallengeByDiscordID(ctx, strPtr(discordUser)); err == nil {
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
	if _, err := h.store.Queries().GetChallengeByDiscordID(ctx, strPtr(discordUser)); err == nil {
		t.Fatal("the challenge survived the attempt limit")
	}
	reply, err := handler(ctx, linkInvoke("confirm", map[string]string{"code": h.rcon.lastCode(t)}))
	if err != nil {
		t.Fatalf("confirm after burn: %v", err)
	}
	if !reply.UserError || !strings.Contains(reply.Content, "link in progress") {
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
	if _, err := h.store.Queries().GetChallengeByDiscordID(context.Background(), strPtr(discordUser)); err == nil {
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
		DiscordUserID: strPtr("someone-else"),
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

	kept, err := h.store.Queries().GetChallengeByDiscordID(ctx, strPtr("someone-else"))
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
		linkInvoke("start", map[string]string{"player": "555-000-101 100"}))
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

// seedUnclaimed plants an in-game-initiated challenge: no Discord user owns
// it, so the code itself is the claim.
// It returns the code it planted, which is the only way to reach that
// challenge -- exactly as in game.
func seedUnclaimed(t *testing.T, h *linkHarness) string {
	t.Helper()
	const code = "ABC234"
	if err := h.store.Queries().UpsertChallenge(context.Background(), gen.UpsertChallengeParams{
		AlderonID:  testAGID,
		PlayerName: testName,
		CodeHash:   linkcode.Hash(code),
		ExpiresAt:  time.Now().Add(5 * time.Minute),
	}); err != nil {
		t.Fatalf("seed unclaimed challenge: %v", err)
	}
	return code
}

// TestConfirmClaimsAnInGameChallenge is the new half of the link scheme: the
// challenge was started by typing !link in game, so no Discord user owns it,
// and whoever brings the code to /link confirm becomes the link.
func TestConfirmClaimsAnInGameChallenge(t *testing.T) {
	h := newLinkHarness(t)
	ctx := context.Background()
	code := seedUnclaimed(t, h)

	// Typed the way a player types: mangled case, stray space.
	reply, err := h.linker.Command().Handler(ctx,
		linkInvoke("confirm", map[string]string{"code": " " + strings.ToLower(code)}))
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if reply.UserError {
		t.Fatalf("a correct in-game code was refused: %q", reply.Content)
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
	if _, err := q.GetChallengeByAlderonID(ctx, testAGID); err == nil {
		t.Error("the challenge survived being claimed")
	}
}

// TestWrongCodeDoesNotBurnUnclaimedChallenges: an unclaimed challenge belongs
// to whoever is in game, and a stranger's wrong guess in Discord must not
// destroy it. Brute force is impractical against 30^6 codes inside the TTL, so
// forgiving guesses here costs nothing.
func TestWrongCodeDoesNotBurnUnclaimedChallenges(t *testing.T) {
	h := newLinkHarness(t)
	ctx := context.Background()
	seedUnclaimed(t, h)

	for range h.cfg.Link.MaxAttempts + 1 {
		reply, err := h.linker.Command().Handler(ctx,
			linkInvoke("confirm", map[string]string{"code": "WRONG2"}))
		if err != nil {
			t.Fatalf("confirm: %v", err)
		}
		if !reply.UserError {
			t.Fatalf("a wrong code was accepted: %q", reply.Content)
		}
	}

	row, err := h.store.Queries().GetChallengeByAlderonID(ctx, testAGID)
	if err != nil {
		t.Fatalf("the unclaimed challenge was destroyed by wrong guesses: %v", err)
	}
	if row.Attempts != 0 {
		t.Errorf("attempts = %d, want 0: unclaimed challenges must not be burned", row.Attempts)
	}
}

// TestStartReplacesOwnChallengeOnAnotherIdentity is the unique-constraint
// trap: with the table keyed by identity, a caller who holds a pending
// challenge on a DIFFERENT identity would violate unique(discord_user_id)
// unless start clears their old row in the same transaction.
func TestStartReplacesOwnChallengeOnAnotherIdentity(t *testing.T) {
	h := newLinkHarness(t)
	ctx := context.Background()
	q := h.store.Queries()

	if err := q.UpsertChallenge(ctx, gen.UpsertChallengeParams{
		AlderonID:     "555-000-999",
		DiscordUserID: strPtr(discordUser),
		PlayerName:    "oldname",
		CodeHash:      linkcode.Hash("OLD234"),
		ExpiresAt:     time.Now().Add(5 * time.Minute),
	}); err != nil {
		t.Fatalf("seed old challenge: %v", err)
	}
	// Age it past the reissue cooldown, which keys off created_at.
	if _, err := h.pool.Exec(ctx,
		"update link_challenges set created_at = now() - interval '1 hour'"); err != nil {
		t.Fatalf("age challenge: %v", err)
	}

	reply, err := h.linker.Command().Handler(ctx, linkInvoke("start", map[string]string{"player": testAGID}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if reply.UserError {
		t.Fatalf("a fresh start against a new identity was refused: %q", reply.Content)
	}

	if _, err := q.GetChallengeByAlderonID(ctx, "555-000-999"); err == nil {
		t.Error("the old challenge survived; the same user now holds two")
	}
	fresh, err := q.GetChallengeByAlderonID(ctx, testAGID)
	if err != nil {
		t.Fatalf("no challenge on the new identity: %v", err)
	}
	if fresh.DiscordUserID == nil || *fresh.DiscordUserID != discordUser {
		t.Errorf("new challenge owner = %v, want %q", fresh.DiscordUserID, discordUser)
	}
}

// TestStartYieldsToAPendingInGameChallenge: someone already typed !link on
// that character, and they are the identity's authority. /link start must
// point at the existing code, not stomp it with a fresh one.
func TestStartYieldsToAPendingInGameChallenge(t *testing.T) {
	h := newLinkHarness(t)
	ctx := context.Background()
	seedUnclaimed(t, h)

	reply, err := h.linker.Command().Handler(ctx, linkInvoke("start", map[string]string{"player": testAGID}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !reply.UserError || !strings.Contains(reply.Content, "/link confirm") {
		t.Fatalf("start did not defer to the in-game challenge: %q", reply.Content)
	}
	row, err := h.store.Queries().GetChallengeByAlderonID(ctx, testAGID)
	if err != nil {
		t.Fatalf("the in-game challenge was destroyed: %v", err)
	}
	if row.DiscordUserID != nil {
		t.Error("the in-game challenge was claimed by /link start")
	}
}

// TestInGameCodeWinsOverAStalePendingChallenge is the deadlock the path
// ordering exists to break: the caller started a Discord link on one identity,
// then typed !link on another. Their in-game code can never match their own
// pending row, so checking the unclaimed pool only AFTER burning an attempt
// would spend all their attempts on the wrong row -- while /link start refuses
// a reset because a challenge is pending.
func TestInGameCodeWinsOverAStalePendingChallenge(t *testing.T) {
	h := newLinkHarness(t)
	ctx := context.Background()
	q := h.store.Queries()

	if err := q.UpsertChallenge(ctx, gen.UpsertChallengeParams{
		AlderonID:     "555-000-999",
		DiscordUserID: strPtr(discordUser),
		PlayerName:    "oldname",
		CodeHash:      linkcode.Hash("OLD234"),
		ExpiresAt:     time.Now().Add(5 * time.Minute),
	}); err != nil {
		t.Fatalf("seed stale challenge: %v", err)
	}
	code := seedUnclaimed(t, h)

	reply, err := h.linker.Command().Handler(ctx,
		linkInvoke("confirm", map[string]string{"code": code}))
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if reply.UserError {
		t.Fatalf("the in-game code was refused: %q", reply.Content)
	}

	link, err := q.GetLinkByDiscordID(ctx, discordUser)
	if err != nil {
		t.Fatalf("link not created: %v", err)
	}
	if link.AlderonID != testAGID {
		t.Errorf("linked to %q, want the in-game identity %q", link.AlderonID, testAGID)
	}
	stale, err := q.GetChallengeByAlderonID(ctx, "555-000-999")
	if err != nil {
		t.Fatalf("read stale challenge: %v", err)
	}
	if stale.Attempts != 0 {
		t.Errorf("attempts = %d on the stale row, want 0: the match was found before any burn", stale.Attempts)
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

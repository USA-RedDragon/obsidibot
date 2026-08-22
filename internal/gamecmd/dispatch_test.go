package gamecmd_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/bank"
	"github.com/USA-RedDragon/obsidibot/internal/config"
	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/dbtest"
	"github.com/USA-RedDragon/obsidibot/internal/gamecmd"
	"github.com/USA-RedDragon/obsidibot/internal/metrics"
	"github.com/USA-RedDragon/obsidibot/internal/pot"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	testAGID = "555-000-101"
	testName = "testplayer"
)

// fakeRCON answers with the game's real response strings and applies the marks
// commands the way the server does -- clamping a removal at zero and reporting
// what it actually took -- so the bank's verification logic is exercised
// rather than stubbed.
type fakeRCON struct {
	mu       sync.Mutex
	commands []string
	whispers []string
	marks    int64
	online   bool
	// block, when set, holds every command until it is closed. It is how a
	// test fills the dispatcher's semaphore without racing.
	block chan struct{}
}

func newFakeRCON() *fakeRCON {
	return &fakeRCON{marks: 3838, online: true}
}

func (f *fakeRCON) Execute(_ context.Context, command string) (string, error) {
	f.mu.Lock()
	f.commands = append(f.commands, command)
	block := f.block
	f.mu.Unlock()

	if block != nil {
		<-block
	}

	fields := strings.Fields(command)
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.online {
		return fmt.Sprintf("(%s): No player with the username '%s'.", command, fields[1]), nil
	}

	switch strings.ToLower(fields[0]) {
	case "playerinfo":
		return fmt.Sprintf(
			"(%s): Name: %s / AGID: %s / Dinosaur: Ceratosaurus / Role: None / Marks: %d / Growth: 1 /"+
				" Location: (X=1.0 Y=2.0 Z=3.0)", command, testName, fields[1], f.marks), nil
	case "addmarks", "removemarks":
		amount, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return fmt.Sprintf("(%s): Incorrect Syntax, type /help.", command), nil
		}
		if strings.EqualFold(fields[0], "addmarks") {
			f.marks += amount
			return fmt.Sprintf("(%s): Added %d Marks to %s. They now have %d Marks.",
				command, amount, fields[1], f.marks), nil
		}
		if amount > f.marks {
			amount = f.marks
		}
		f.marks -= amount
		return fmt.Sprintf("(%s): Removed %d Marks from %s. They now have %d Marks.",
			command, amount, fields[1], f.marks), nil
	case "whisper":
		f.whispers = append(f.whispers, strings.Join(fields[2:], " "))
		return fmt.Sprintf("(%s): ok", command), nil
	}
	return fmt.Sprintf("(%s): That command does not exist.", command), nil
}

func (f *fakeRCON) said() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.whispers...)
}

// lastWhisper is what the player would have seen. Tests assert on this rather
// than on a return value because the whisper IS the reply: nothing else ever
// reaches the player.
func (f *fakeRCON) lastWhisper(t *testing.T) string {
	t.Helper()
	said := f.said()
	if len(said) == 0 {
		t.Fatal("nothing was whispered to the player")
	}
	return said[len(said)-1]
}

func (f *fakeRCON) issued() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

type harness struct {
	pool    *pgxpool.Pool
	store   *db.Store
	rcon    *fakeRCON
	cfg     *config.Config
	metrics *metrics.Metrics
	disp    *gamecmd.Dispatcher
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	pool := dbtest.Pool(t)
	dbtest.Reset(t, pool)
	if err := db.Migrate(context.Background(), pool, dbtest.MigrationsFS(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := db.NewStore(pool)
	fake := newFakeRCON()
	cfg := &config.Config{
		Rating: config.Rating{Initial: 1200},
		RCON:   config.RCON{MaxConcurrent: 4, TimeoutSeconds: 5},
		Bank:   config.Bank{CooldownSeconds: 0, VerifyAttempts: 2},
		Link:   config.Link{CodeTTLSeconds: 300, MaxAttempts: 5, ReissueCooldownSeconds: 30},
	}
	m := metrics.New()
	game := pot.NewClient(fake, nil)

	return &harness{
		pool: pool, store: store, rcon: fake, cfg: cfg, metrics: m,
		disp: gamecmd.New(store, game, bank.New(store, game, m, cfg), m, cfg),
	}
}

// send delivers one in-game command and waits for the worker to finish, so a
// test asserts on a settled world rather than on a race.
func (h *harness) send(t *testing.T, message string) {
	t.Helper()
	h.disp.Dispatch(context.Background(), gamecmd.Incoming{
		AGID: testAGID, PlayerName: testName, Message: message,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := h.disp.Wait(ctx); err != nil {
		t.Fatalf("waiting for the command to finish: %v", err)
	}
}

func (h *harness) ledger(t *testing.T) []gen.BankLedger {
	t.Helper()
	rows, err := h.pool.Query(context.Background(),
		"select id, alderon_id, discord_user_id, direction, amount, state from bank_ledger order by id")
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	defer rows.Close()
	var out []gen.BankLedger
	for rows.Next() {
		var row gen.BankLedger
		if err := rows.Scan(&row.ID, &row.AlderonID, &row.DiscordUserID,
			&row.Direction, &row.Amount, &row.State); err != nil {
			t.Fatalf("scan ledger: %v", err)
		}
		out = append(out, row)
	}
	return out
}

func (h *harness) counted(t *testing.T, command, result string) float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	h.metrics.Registry.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	want := fmt.Sprintf("obsidibot_game_commands_total{command=%q,result=%q} ", command, result)
	for line := range strings.SplitSeq(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, want) {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimPrefix(line, want), 64)
		if err != nil {
			t.Fatalf("unreadable counter line %q: %v", line, err)
		}
		return value
	}
	return 0
}

// TestDepositFromANeverSeenPlayer is the foreign-key path. bank_accounts
// references players, and a player who banks in game without ever having been
// linked or killed has no players row at all -- so every command records the
// player before doing anything else. Without that, the very first in-game
// deposit on a fresh server fails on a constraint the player can do nothing
// about.
func TestDepositFromANeverSeenPlayer(t *testing.T) {
	h := newHarness(t)
	h.send(t, "!deposit 500")

	player, err := h.store.Queries().GetPlayer(context.Background(), testAGID)
	if err != nil {
		t.Fatalf("player row was not created: %v", err)
	}
	if player.LastKnownName != testName {
		t.Errorf("name = %q, want %q", player.LastKnownName, testName)
	}

	rows := h.ledger(t)
	if len(rows) != 1 {
		t.Fatalf("%d ledger rows, want 1", len(rows))
	}
	if rows[0].State != gen.BankStateApplied {
		t.Errorf("state = %s, want applied", rows[0].State)
	}
	// Unlinked: the AGID is the identity, and the Discord column is an empty
	// snapshot rather than a lie about who did it.
	if rows[0].DiscordUserID != "" {
		t.Errorf("discord_user_id = %q, want empty for an unlinked in-game banker", rows[0].DiscordUserID)
	}

	account, err := h.store.Queries().GetBankAccount(context.Background(), testAGID)
	if err != nil {
		t.Fatalf("bank account: %v", err)
	}
	if account.Balance != 500 {
		t.Errorf("balance = %d, want 500", account.Balance)
	}
	if said := h.rcon.lastWhisper(t); !strings.Contains(said, "500") {
		t.Errorf("whisper did not report the transfer: %q", said)
	}
	if got := h.counted(t, gamecmd.CommandDeposit, metrics.ResultOK); got != 1 {
		t.Errorf("deposits counted = %v, want 1", got)
	}
}

// TestDepositFromALinkedPlayerSnapshotsTheDiscordID: the column is only ever a
// record of who asked, but recording "" for someone who IS linked would make
// the ledger harder to read back than it needs to be.
func TestDepositFromALinkedPlayerSnapshotsTheDiscordID(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	q := h.store.Queries()
	if err := q.UpsertPlayerSeen(ctx, gen.UpsertPlayerSeenParams{
		AlderonID: testAGID, LastKnownName: testName, Rating: 1200,
	}); err != nil {
		t.Fatalf("seed player: %v", err)
	}
	if err := q.CreateLink(ctx, gen.CreateLinkParams{
		DiscordUserID: "discord-1", AlderonID: testAGID,
	}); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	h.send(t, "!deposit 100")

	rows := h.ledger(t)
	if len(rows) != 1 {
		t.Fatalf("%d ledger rows, want 1", len(rows))
	}
	if rows[0].DiscordUserID != "discord-1" {
		t.Errorf("discord_user_id = %q, want discord-1", rows[0].DiscordUserID)
	}
}

// TestDuplicateDeliveryMovesMarksOnce is the whole duplicate story for this
// route. The webhook carries no event id, so the route cannot dedupe and
// deliberately does not try; what makes a redelivery safe is the bank's
// cooldown, and the player is told plainly rather than being silently ignored.
func TestDuplicateDeliveryMovesMarksOnce(t *testing.T) {
	h := newHarness(t)
	h.cfg.Bank.CooldownSeconds = 600

	h.send(t, "!deposit 100")
	h.send(t, "!deposit 100")

	rows := h.ledger(t)
	if len(rows) != 1 {
		t.Fatalf("%d ledger rows after a duplicate delivery, want 1", len(rows))
	}
	account, err := h.store.Queries().GetBankAccount(context.Background(), testAGID)
	if err != nil {
		t.Fatalf("bank account: %v", err)
	}
	if account.Balance != 100 {
		t.Fatalf("balance = %d, want 100: the duplicate was applied twice", account.Balance)
	}
	if said := h.rcon.lastWhisper(t); !strings.Contains(strings.ToLower(said), "too soon") {
		t.Errorf("the duplicate was not explained to the player: %q", said)
	}
}

// TestWithdrawIsClampedAndExplained: asking for more than is banked moves what
// there is, and says so.
func TestWithdrawIsClampedAndExplained(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	q := h.store.Queries()
	if err := q.UpsertPlayerSeen(ctx, gen.UpsertPlayerSeenParams{
		AlderonID: testAGID, LastKnownName: testName, Rating: 1200,
	}); err != nil {
		t.Fatalf("seed player: %v", err)
	}
	if err := q.EnsureBankAccount(ctx, testAGID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if err := q.CreditBank(ctx, gen.CreditBankParams{AlderonID: testAGID, Balance: 250}); err != nil {
		t.Fatalf("credit: %v", err)
	}

	h.send(t, "!withdraw 1,000")

	account, err := q.GetBankAccount(ctx, testAGID)
	if err != nil {
		t.Fatalf("bank account: %v", err)
	}
	if account.Balance != 0 {
		t.Errorf("balance = %d, want 0", account.Balance)
	}
	said := h.rcon.lastWhisper(t)
	if !strings.Contains(said, "250") || !strings.Contains(said, "everything available") {
		t.Errorf("the clamp was not explained: %q", said)
	}
}

// TestBalanceAnswersWithoutTheGame: the banked figure is ours and does not
// depend on RCON. A player asking what they have banked deserves an answer
// even while the game server is not giving one.
func TestBalanceAnswersWithoutTheGame(t *testing.T) {
	h := newHarness(t)
	h.send(t, "!deposit 300")
	h.rcon.mu.Lock()
	h.rcon.online = false
	h.rcon.mu.Unlock()

	h.send(t, "!balance")

	said := h.rcon.lastWhisper(t)
	if !strings.Contains(said, "300") {
		t.Errorf("balance whisper = %q, want the banked figure", said)
	}
}

// TestLinkOpensAnUnclaimedChallenge. The row belongs to no Discord user: the
// person in game is the identity's authority, and the code they carry to
// /link confirm is what claims it.
func TestLinkOpensAnUnclaimedChallenge(t *testing.T) {
	h := newHarness(t)
	h.send(t, "!link")

	challenge, err := h.store.Queries().GetChallengeByAlderonID(context.Background(), testAGID)
	if err != nil {
		t.Fatalf("challenge was not stored: %v", err)
	}
	if challenge.DiscordUserID != nil {
		t.Errorf("discord_user_id = %v, want null: an in-game link is unclaimed", *challenge.DiscordUserID)
	}

	said := h.rcon.lastWhisper(t)
	if !strings.Contains(said, "/link confirm") {
		t.Errorf("the whisper does not say what to do with the code: %q", said)
	}
	// An unclaimed code is a bearer token -- whoever types it in Discord gets
	// the link -- and people broadcast their screens.
	if !strings.Contains(strings.ToLower(said), "do not share") {
		t.Errorf("the whisper does not warn against sharing the code: %q", said)
	}
	// The code itself must never be recoverable from the database.
	if strings.Contains(string(challenge.CodeHash), codeIn(t, said)) {
		t.Error("the plaintext code was stored")
	}
}

// codeIn digs the six-character code out of a whisper.
func codeIn(t *testing.T, whisper string) string {
	t.Helper()
	for _, field := range strings.Fields(whisper) {
		trimmed := strings.Trim(field, ".,")
		if len(trimmed) != 6 {
			continue
		}
		if strings.ToUpper(trimmed) == trimmed && !strings.ContainsAny(trimmed, "01OIL") {
			return trimmed
		}
	}
	t.Fatalf("no code found in %q", whisper)
	return ""
}

// TestLinkReissueIsBounded: !link past the cooldown replaces the challenge on
// purpose, but inside it the player is told to wait rather than being handed a
// fresh code on demand.
func TestLinkReissueIsBounded(t *testing.T) {
	h := newHarness(t)
	h.send(t, "!link")
	first := codeIn(t, h.rcon.lastWhisper(t))

	h.send(t, "!link")
	said := h.rcon.lastWhisper(t)
	if !strings.Contains(strings.ToLower(said), "already sent") {
		t.Errorf("a second !link inside the cooldown was answered with %q", said)
	}

	challenge, err := h.store.Queries().GetChallengeByAlderonID(context.Background(), testAGID)
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	if string(challenge.CodeHash) == "" || first == "" {
		t.Fatal("no challenge to compare")
	}
}

// TestLinkOnAnAlreadyLinkedCharacter says so rather than opening a challenge
// that could never be claimed.
func TestLinkOnAnAlreadyLinkedCharacter(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	q := h.store.Queries()
	if err := q.UpsertPlayerSeen(ctx, gen.UpsertPlayerSeenParams{
		AlderonID: testAGID, LastKnownName: testName, Rating: 1200,
	}); err != nil {
		t.Fatalf("seed player: %v", err)
	}
	if err := q.CreateLink(ctx, gen.CreateLinkParams{
		DiscordUserID: "discord-1", AlderonID: testAGID,
	}); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	h.send(t, "!link")

	if said := h.rcon.lastWhisper(t); !strings.Contains(strings.ToLower(said), "already linked") {
		t.Errorf("whisper = %q, want it to say the character is already linked", said)
	}
	if _, err := q.GetChallengeByAlderonID(ctx, testAGID); err == nil {
		t.Error("a challenge was opened for an already-linked character")
	}
}

// TestUnknownCommandEchoesTheServersPrefix: the usage text has to name the
// prefix the player's server actually uses, and they just proved what that is
// by typing it.
func TestUnknownCommandEchoesTheServersPrefix(t *testing.T) {
	h := newHarness(t)
	h.send(t, ".teleport home")

	said := h.rcon.lastWhisper(t)
	if !strings.Contains(said, ".balance") {
		t.Errorf("usage = %q, want it to echo the '.' prefix the player used", said)
	}
	if got := h.counted(t, gamecmd.CommandUnknown, metrics.ResultUserError); got != 1 {
		t.Errorf("unknown commands counted = %v, want 1", got)
	}
}

// TestTooManyInFlightAreDropped. The semaphore matches the RCON slot count, so
// admitting more workers than that would only manufacture ErrBusy failures out
// of commands that were going to work. A drop is honest, counted, and the
// player can simply type it again.
func TestTooManyInFlightAreDropped(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	h.rcon.mu.Lock()
	h.rcon.block = release
	h.rcon.mu.Unlock()

	// Fill every slot. Each command parks inside RCON until release is closed.
	for range h.cfg.RCON.MaxConcurrent {
		h.disp.Dispatch(context.Background(), gamecmd.Incoming{
			AGID: testAGID, PlayerName: testName, Message: "!balance",
		})
	}
	// Wait for them to actually be in flight rather than merely dispatched.
	deadline := time.Now().Add(10 * time.Second)
	for len(h.rcon.issued()) < h.cfg.RCON.MaxConcurrent && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	h.disp.Dispatch(context.Background(), gamecmd.Incoming{
		AGID: testAGID, PlayerName: testName, Message: "!balance",
	})
	if got := h.counted(t, gamecmd.CommandBalance, metrics.ResultRejected); got != 1 {
		t.Errorf("dropped commands counted = %v, want 1", got)
	}

	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.disp.Wait(ctx); err != nil {
		t.Fatalf("waiting for the in-flight commands: %v", err)
	}
}

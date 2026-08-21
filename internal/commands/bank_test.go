package commands_test

import (
	"context"
	"strings"
	"testing"

	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/interactions"
	"github.com/bwmarrin/discordgo"
)

// bankInvoke builds the interaction a banking command arrives in.
func bankInvoke(name string, amount *int64) interactions.Context {
	var opts []*discordgo.ApplicationCommandInteractionDataOption
	if amount != nil {
		opts = append(opts, &discordgo.ApplicationCommandInteractionDataOption{
			Name: "amount", Type: discordgo.ApplicationCommandOptionInteger, Value: float64(*amount),
		})
	}
	return interactions.Context{
		UserID:  discordUser,
		GuildID: "g1",
		Interaction: &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
			Type:  discordgo.InteractionApplicationCommand,
			Token: "interaction-token",
			Data: discordgo.ApplicationCommandInteractionData{
				Name: name, Options: opts,
			},
		}},
	}
}

func amountOf(v int64) *int64 { return &v }

// handlerFor finds a banking command by name.
func (h *linkHarness) handlerFor(t *testing.T, name string) interactions.Handler {
	t.Helper()
	for _, cmd := range h.banker.Commands() {
		if cmd.Definition.Name == name {
			return cmd.Handler
		}
	}
	t.Fatalf("no command named %q", name)
	return nil
}

func (h *linkHarness) mustLink(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	q := h.store.Queries()
	if err := q.UpsertPlayerSeen(ctx, gen.UpsertPlayerSeenParams{
		AlderonID: testAGID, LastKnownName: testName, Rating: 1200,
	}); err != nil {
		t.Fatalf("seed player: %v", err)
	}
	if err := q.CreateLink(ctx, gen.CreateLinkParams{
		DiscordUserID: discordUser, AlderonID: testAGID,
	}); err != nil {
		t.Fatalf("seed link: %v", err)
	}
	if err := q.EnsureBankAccount(ctx, testAGID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
}

func (h *linkHarness) bankBalance(t *testing.T) int64 {
	t.Helper()
	account, err := h.store.Queries().GetBankAccount(context.Background(), testAGID)
	if err != nil {
		t.Fatalf("read balance: %v", err)
	}
	return account.Balance
}

func (h *linkHarness) ledgerStates(t *testing.T) []gen.BankState {
	t.Helper()
	rows, err := h.pool.Query(context.Background(), "select state from bank_ledger order by id")
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	defer rows.Close()
	var out []gen.BankState
	for rows.Next() {
		var state gen.BankState
		if err := rows.Scan(&state); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, state)
	}
	return out
}

// TestDepositMovesMarksAndBalanceTogether is the base case.
func TestDepositMovesMarksAndBalanceTogether(t *testing.T) {
	h := newLinkHarness(t)
	h.mustLink(t)
	h.rcon.marks = 3838

	reply, err := h.handlerFor(t, "deposit")(context.Background(), bankInvoke("deposit", amountOf(1000)))
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if reply.UserError {
		t.Fatalf("deposit refused: %q", reply.Content)
	}
	if got := h.rcon.balance(); got != 2838 {
		t.Errorf("in-game marks = %d, want 2838", got)
	}
	if got := h.bankBalance(t); got != 1000 {
		t.Errorf("bank balance = %d, want 1000", got)
	}
	if states := h.ledgerStates(t); len(states) != 1 || states[0] != gen.BankStateApplied {
		t.Errorf("ledger states = %v, want one applied", states)
	}
}

// TestWithdrawReturnsMarks is the reverse.
func TestWithdrawReturnsMarks(t *testing.T) {
	h := newLinkHarness(t)
	h.mustLink(t)
	h.rcon.marks = 100
	if err := h.store.Queries().CreditBank(context.Background(), gen.CreditBankParams{
		AlderonID: testAGID, Balance: 500,
	}); err != nil {
		t.Fatalf("seed bank: %v", err)
	}

	if _, err := h.handlerFor(t, "withdraw")(context.Background(),
		bankInvoke("withdraw", amountOf(300))); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if got := h.rcon.balance(); got != 400 {
		t.Errorf("in-game marks = %d, want 400", got)
	}
	if got := h.bankBalance(t); got != 200 {
		t.Errorf("bank balance = %d, want 200", got)
	}
}

// TestOmittedAmountMeansAll is the documented default.
func TestOmittedAmountMeansAll(t *testing.T) {
	h := newLinkHarness(t)
	h.mustLink(t)
	h.rcon.marks = 777

	if _, err := h.handlerFor(t, "deposit")(context.Background(), bankInvoke("deposit", nil)); err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if got := h.rcon.balance(); got != 0 {
		t.Errorf("in-game marks = %d after depositing everything, want 0", got)
	}
	if got := h.bankBalance(t); got != 777 {
		t.Errorf("bank balance = %d, want 777", got)
	}
}

// TestAmountIsClampedNotRefused: asking for more than you have takes what you
// have, which is what "deposit 99999" obviously means.
func TestAmountIsClampedNotRefused(t *testing.T) {
	h := newLinkHarness(t)
	h.mustLink(t)
	h.rcon.marks = 250

	reply, err := h.handlerFor(t, "deposit")(context.Background(),
		bankInvoke("deposit", amountOf(999999)))
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if reply.UserError {
		t.Fatalf("an over-large deposit was refused instead of clamped: %q", reply.Content)
	}
	if got := h.bankBalance(t); got != 250 {
		t.Errorf("bank balance = %d, want the 250 that existed", got)
	}
	if !strings.Contains(reply.Content, "everything available") {
		t.Errorf("the clamp was not explained: %q", reply.Content)
	}
	// And the command sent to the game asked for what was there, never more.
	for _, cmd := range h.rcon.issued() {
		if strings.Contains(cmd, "999999") {
			t.Errorf("a command asked for more marks than the player had: %q", cmd)
		}
	}
}

// TestBankingRequiresBeingInGame: marks live on the character being played.
func TestBankingRequiresBeingInGame(t *testing.T) {
	h := newLinkHarness(t)
	h.mustLink(t)
	h.rcon.online = false

	for _, name := range []string{"deposit", "withdraw"} {
		reply, err := h.handlerFor(t, name)(context.Background(), bankInvoke(name, amountOf(10)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reply.UserError || !strings.Contains(reply.Content, "logged into the server") {
			t.Errorf("%s while offline: %q", name, reply.Content)
		}
	}
	if states := h.ledgerStates(t); len(states) != 0 {
		t.Errorf("ledger rows were opened for an offline player: %v", states)
	}
}

// TestBankingRequiresALink.
func TestBankingRequiresALink(t *testing.T) {
	h := newLinkHarness(t)
	reply, err := h.handlerFor(t, "deposit")(context.Background(), bankInvoke("deposit", amountOf(10)))
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if !reply.UserError || !strings.Contains(reply.Content, "/link start") {
		t.Errorf("an unlinked user was not pointed at /link: %q", reply.Content)
	}
}

// TestWithdrawCannotOverdraw: the clamp plus the balance check mean a player
// can never take out more than they put in.
func TestWithdrawCannotOverdraw(t *testing.T) {
	h := newLinkHarness(t)
	h.mustLink(t)
	h.rcon.marks = 0

	reply, err := h.handlerFor(t, "withdraw")(context.Background(),
		bankInvoke("withdraw", amountOf(1000)))
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if !reply.UserError {
		t.Fatalf("withdrawing from an empty bank succeeded: %q", reply.Content)
	}
	if got := h.rcon.balance(); got != 0 {
		t.Errorf("marks were created out of nothing: %d", got)
	}
	if got := h.bankBalance(t); got != 0 {
		t.Errorf("bank balance went to %d", got)
	}
}

// TestRejectedCommandDoesNotMoveTheBalance is the regression test for the bug
// the live server revealed: Path of Titans reports an unknown command in the
// RESPONSE BODY with a successful RCON exchange around it. Treating that as
// success on a withdraw would debit the bank and pay out nothing.
func TestRejectedCommandDoesNotMoveTheBalance(t *testing.T) {
	h := newLinkHarness(t)
	h.mustLink(t)
	h.rcon.marks = 100
	if err := h.store.Queries().CreditBank(context.Background(), gen.CreditBankParams{
		AlderonID: testAGID, Balance: 500,
	}); err != nil {
		t.Fatalf("seed bank: %v", err)
	}
	// The game no longer has AddMarks -- e.g. Alderon renamed it.
	h.rcon.rejectVerb = "AddMarks"

	reply, err := h.handlerFor(t, "withdraw")(context.Background(),
		bankInvoke("withdraw", amountOf(300)))
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if !reply.UserError && !strings.Contains(reply.Content, "not") {
		t.Logf("reply: %q", reply.Content)
	}
	if got := h.bankBalance(t); got != 500 {
		t.Fatalf("the bank was debited %d marks for a command the game refused", 500-got)
	}
	if got := h.rcon.balance(); got != 100 {
		t.Fatalf("in-game marks changed to %d despite a refused command", got)
	}
	// Nothing moved, so the row closes cleanly rather than needing a human.
	states := h.ledgerStates(t)
	if len(states) != 1 || states[0] != gen.BankStateFailed {
		t.Fatalf("ledger states = %v, want one failed", states)
	}
}

// TestUnconfirmedTransferIsParkedNotGuessed. If the marks did not visibly move,
// the bot must NOT credit the balance and must NOT retry the command -- it
// flags the row and says so. This is the irreducible cost of a non-
// transactional RCON, and it has to be exercised.
func TestUnconfirmedTransferIsParkedNotGuessed(t *testing.T) {
	h := newLinkHarness(t)
	h.mustLink(t)
	h.rcon.marks = 1000
	// The command is accepted and silently does nothing, which is exactly what
	// a lost write looks like from here.
	h.rcon.swallowVerb = "RemoveMarks"

	reply, err := h.handlerFor(t, "deposit")(context.Background(),
		bankInvoke("deposit", amountOf(400)))
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if !strings.Contains(reply.Content, "not") || !strings.Contains(reply.Content, "moderator") {
		t.Errorf("the player was not told the transfer needs review: %q", reply.Content)
	}
	if got := h.bankBalance(t); got != 0 {
		t.Fatalf("the bank was credited %d for an unconfirmed transfer", got)
	}
	states := h.ledgerStates(t)
	if len(states) != 1 || states[0] != gen.BankStateNeedsReview {
		t.Fatalf("ledger states = %v, want one needs_review", states)
	}
	// And exactly ONE mutating command was ever sent.
	var mutations int
	for _, cmd := range h.rcon.issued() {
		if strings.HasPrefix(cmd, "RemoveMarks") || strings.HasPrefix(cmd, "AddMarks") {
			mutations++
		}
	}
	if mutations != 1 {
		t.Fatalf("%d mutating commands were sent for one transfer; retrying mints currency", mutations)
	}
}

// TestOneTransferAtATime: the partial unique index is the guard, and it holds
// across replicas because it is a database constraint.
func TestOneTransferAtATime(t *testing.T) {
	h := newLinkHarness(t)
	h.mustLink(t)
	h.rcon.marks = 1000
	ctx := context.Background()

	// Leave a row in flight, as a crashed request would.
	before := int64(1000)
	if _, err := h.store.Queries().BeginOperation(ctx, gen.BeginOperationParams{
		AlderonID: testAGID, DiscordUserID: discordUser,
		Direction: gen.BankDirectionDeposit, Amount: 100, MarksBefore: &before,
	}); err != nil {
		t.Fatalf("seed in-flight row: %v", err)
	}

	reply, err := h.handlerFor(t, "deposit")(ctx, bankInvoke("deposit", amountOf(50)))
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if !reply.UserError || !strings.Contains(reply.Content, "in progress") {
		t.Fatalf("a second concurrent transfer was allowed: %q", reply.Content)
	}
	if got := h.rcon.balance(); got != 1000 {
		t.Errorf("the second transfer touched the game anyway: %d", got)
	}
}

// TestBalanceWorksOfflineToo: not being in game is normal, not an error.
func TestBalanceWorksOffline(t *testing.T) {
	h := newLinkHarness(t)
	h.mustLink(t)
	if err := h.store.Queries().CreditBank(context.Background(), gen.CreditBankParams{
		AlderonID: testAGID, Balance: 12345,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h.rcon.online = false

	reply, err := h.handlerFor(t, "balance")(context.Background(), bankInvoke("balance", nil))
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if reply.UserError {
		t.Fatalf("balance refused for an offline player: %q", reply.Content)
	}
	// Thousands grouped, because five- and six-digit balances are unreadable
	// otherwise.
	if !strings.Contains(reply.Content, "12,345") {
		t.Errorf("balance did not render readably: %q", reply.Content)
	}
	if !strings.Contains(reply.Content, "Log in") {
		t.Errorf("balance did not explain the missing in-game figure: %q", reply.Content)
	}
}

// TestDepositCreditsWhatActuallyMovedNotWhatWasAsked is the mint-prevention
// case the live server revealed.
//
// The bot clamps a deposit to the balance it just read, but the player is
// still playing: they can spend marks between that read and the command. The
// game clamps the removal at zero and REPORTS WHAT IT ACTUALLY TOOK. Crediting
// the requested figure instead of the reported one would create the difference
// out of nothing, every time.
func TestDepositCreditsWhatActuallyMovedNotWhatWasAsked(t *testing.T) {
	h := newLinkHarness(t)
	h.mustLink(t)
	h.rcon.marks = 1000

	// The bot reads 1000 and asks for 1000; the player spends 700 in the
	// meantime, so only 300 is actually there to take.
	h.rcon.beforeMutation = func(f *fakeRCON) { f.marks = 300 }

	reply, err := h.handlerFor(t, "deposit")(context.Background(), bankInvoke("deposit", nil))
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if reply.UserError {
		t.Fatalf("deposit refused: %q", reply.Content)
	}

	if got := h.bankBalance(t); got != 300 {
		t.Fatalf("bank credited %d, want the 300 that actually moved; the difference would be minted", got)
	}
	if got := h.rcon.balance(); got != 0 {
		t.Errorf("in-game marks = %d, want 0", got)
	}

	// The ledger records both figures: what was asked for, and what moved.
	var amount, moved int64
	if err := h.pool.QueryRow(context.Background(),
		"select amount, moved from bank_ledger order by id desc limit 1").Scan(&amount, &moved); err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if amount != 1000 || moved != 300 {
		t.Errorf("ledger says amount=%d moved=%d, want 1000 and 300", amount, moved)
	}
}

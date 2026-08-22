package commands_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/bwmarrin/discordgo"
)

const (
	targetUser = "discord-target"
	modRoleID  = "role-mod"
)

// banState is the part of a ban row these tests reason about. It is read with
// raw SQL because nothing in production needs a "read one ban by id" query and
// adding one for the tests would be a query nobody calls.
type banState struct {
	agid          *string
	expiresAt     *time.Time
	enforcedAt    *time.Time
	liftedAt      *time.Time
	liftReason    *string
	unenforceable *string
}

func (h *modHarness) banState(t *testing.T, id int64) banState {
	t.Helper()
	var state banState
	if err := h.pool.QueryRow(context.Background(),
		`select alderon_id, expires_at, enforced_at, lifted_at, lift_reason, unenforceable_reason
		   from bans where id = $1`, id).
		Scan(&state.agid, &state.expiresAt, &state.enforcedAt, &state.liftedAt,
			&state.liftReason, &state.unenforceable); err != nil {
		t.Fatalf("read ban %d: %v", id, err)
	}
	return state
}

func (h *modHarness) latestBanID(t *testing.T) int64 {
	t.Helper()
	var id int64
	if err := h.pool.QueryRow(context.Background(),
		`select id from bans order by id desc limit 1`).Scan(&id); err != nil {
		t.Fatalf("read latest ban: %v", err)
	}
	return id
}

// expire rewinds a ban's expiry into the past, which is how these tests reach
// the expiry sweep without waiting a minute for it.
func (h *modHarness) expire(t *testing.T, id int64) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`update bans set expires_at = now() - interval '1 minute' where id = $1`, id); err != nil {
		t.Fatalf("rewind expiry: %v", err)
	}
}

func (h *modHarness) sweep(t *testing.T) {
	t.Helper()
	if err := h.sched.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
}

// TestModRoleGateMatrix is the whole authorisation story for moderation. The
// router's RequiresManageGuild cannot express a database-configured role, so
// this gate is the only thing standing between an ordinary member and /ban.
func TestModRoleGateMatrix(t *testing.T) {
	h := newModHarness(t)
	h.link(t, testAGID, testName, targetUser)

	role := modRoleID
	if err := h.store.Queries().SetModRole(context.Background(), gen.SetModRoleParams{
		GuildID: testGuild, ModRoleID: &role,
	}); err != nil {
		t.Fatalf("set mod role: %v", err)
	}

	tests := []struct {
		name    string
		member  *discordgo.Member
		allowed bool
	}{
		{"manage server alone", manageGuildMember(), true},
		{"holds the configured role", roleMember(modRoleID), true},
		{"holds some other role", roleMember("role-colour"), false},
		{"no roles and no permissions", roleMember(), false},
		{"a DM, so no member at all", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reply := h.runAs(t, "modstats", tc.member, userOpt(targetUser))
			refused := reply.UserError && strings.Contains(reply.Content, "moderator role")
			if tc.allowed && refused {
				t.Fatalf("a moderator was refused: %q", reply.Content)
			}
			if !tc.allowed && !refused {
				t.Fatalf("a non-moderator was allowed: %q / embeds %d", reply.Content, len(reply.Embeds))
			}
		})
	}
}

// TestModRoleGateIgnoresAnUnconfiguredRole: before /config mod-role is run,
// Manage Server is the ONLY way in. That is the bootstrap, and a gate that
// defaulted to open would hand /ban to everyone on day one.
func TestModRoleGateWithoutAConfiguredRole(t *testing.T) {
	h := newModHarness(t)
	h.link(t, testAGID, testName, targetUser)

	if reply := h.runAs(t, "modstats", roleMember("role-anything"), userOpt(targetUser)); !reply.UserError {
		t.Fatalf("a role holder was allowed in with no mod role configured: %q", reply.Content)
	}
	if reply := h.runAs(t, "modstats", manageGuildMember(), userOpt(targetUser)); reply.UserError {
		t.Fatalf("Manage Server was refused: %q", reply.Content)
	}
}

// TestTargetingRequiresExactlyOneIdentity. Naming somebody twice is ambiguous
// and naming them not at all is a moderation action against nobody; both are
// refusals rather than guesses.
func TestTargetingRequiresExactlyOneIdentity(t *testing.T) {
	h := newModHarness(t)
	h.link(t, testAGID, testName, targetUser)

	both := h.run(t, "warn", stringOpt("reason", "x"), userOpt(targetUser), stringOpt("player", testAGID))
	if !both.UserError {
		t.Errorf("naming a target twice was accepted: %q", both.Content)
	}
	neither := h.run(t, "warn", stringOpt("reason", "x"))
	if !neither.UserError {
		t.Errorf("naming no target was accepted: %q", neither.Content)
	}

	// An in-game NAME needs the game to canonicalise it, so an offline one is a
	// refusal that says what to type instead.
	h.rcon.online = false
	offline := h.run(t, "warn", stringOpt("reason", "x"), stringOpt("player", "someoneelse"))
	if !offline.UserError || !strings.Contains(offline.Content, "555-000-101") {
		t.Errorf("an offline name did not point at the Alderon ID: %q", offline.Content)
	}
}

// TestWarnsMatchOneIdentityAcrossLinking is the identity rule: a player warned
// by Alderon ID before they linked and by @user afterwards is ONE person with
// ONE record. Getting this wrong resets everybody's history the day they link.
func TestWarnsMatchOneIdentityAcrossLinking(t *testing.T) {
	h := newModHarness(t)
	h.feedChannels(t)

	// Warned before any link exists, by identity.
	first := h.run(t, "warn", stringOpt("reason", "spawn camping"), stringOpt("player", testAGID))
	if !strings.Contains(first.Content, "#1") {
		t.Fatalf("first warning = %q", first.Content)
	}

	h.link(t, testAGID, testName, targetUser)

	second := h.run(t, "warn", stringOpt("reason", "again"), userOpt(targetUser))
	if !strings.Contains(second.Content, "#2") {
		t.Fatalf("second warning = %q, want it counted against the same player", second.Content)
	}

	// And the player was told, in game, both times.
	if h.rcon.messageCount() != 2 {
		t.Errorf("in-game warnings delivered = %d, want 2", h.rcon.messageCount())
	}
	if titles := h.poster.titles(); len(titles) != 2 || titles[0] != "Warning" {
		t.Errorf("warn feed posts = %q", titles)
	}
}

// TestBanLifecycle walks a ban from the command that placed it to the sweep
// that lifted it, which is the feature end to end.
func TestBanLifecycle(t *testing.T) {
	h := newModHarness(t)
	h.feedChannels(t)
	h.link(t, testAGID, testName, targetUser)

	reply := h.run(t, "ban", stringOpt("reason", "griefing"), userOpt(targetUser), stringOpt("duration", "1h"))
	if reply.UserError {
		t.Fatalf("/ban refused: %q", reply.Content)
	}
	if !strings.Contains(reply.Content, "Kicked and banned in game") {
		t.Errorf("the reply does not state enforcement: %q", reply.Content)
	}

	if kicked := h.rcon.kickedPlayers(); len(kicked) != 1 || kicked[0] != testAGID {
		t.Errorf("kicked = %q, want the target once", kicked)
	}
	if !h.rcon.isBanned(testAGID) {
		t.Fatal("the game is not holding the ban")
	}

	id := h.latestBanID(t)
	if state := h.banState(t, id); state.enforcedAt == nil {
		t.Fatal("enforced_at was not recorded")
	}

	// Rewind the expiry and let the scheduler do its job.
	h.expire(t, id)
	h.sweep(t)

	if h.rcon.isBanned(testAGID) {
		t.Error("the game is still holding an expired ban")
	}
	if unbanned := h.rcon.unbannedPlayers(); len(unbanned) != 1 {
		t.Errorf("unbanned = %q, want exactly one lift", unbanned)
	}
	state := h.banState(t, id)
	if state.liftedAt == nil {
		t.Fatal("the ban was not closed")
	}
	if state.liftReason == nil || *state.liftReason != "expired" {
		t.Errorf("lift_reason = %v, want expired", state.liftReason)
	}
	if titles := h.poster.titles(); len(titles) != 2 || titles[1] != "Ban expired" {
		t.Errorf("feed posts = %q, want the ban and its expiry", titles)
	}

	// A second pass must do nothing at all: the row is closed, and re-issuing
	// an Unban would be noise against the game on every tick forever.
	h.sweep(t)
	if len(h.rcon.unbannedPlayers()) != 1 {
		t.Errorf("the sweep was not idempotent: %q", h.rcon.unbannedPlayers())
	}
	if h.poster.count() != 2 {
		t.Errorf("the expiry was announced twice")
	}
}

// TestExpiryFailsClosed. A ban row is marked lifted only AFTER the game says
// the ban is gone. The other order leaves a player banned with no record that
// they are -- a ban nobody can see and nobody can remove.
func TestExpiryFailsClosed(t *testing.T) {
	h := newModHarness(t)
	h.link(t, testAGID, testName, targetUser)

	h.run(t, "ban", stringOpt("reason", "griefing"), userOpt(targetUser), stringOpt("duration", "1h"))
	id := h.latestBanID(t)
	h.expire(t, id)

	h.rcon.failVerb, h.rcon.failWith = "Unban", errors.New("connection reset")
	h.sweep(t)

	if state := h.banState(t, id); state.liftedAt != nil {
		t.Fatal("the ban was closed even though the game never confirmed the unban")
	}
	if !h.rcon.isBanned(testAGID) {
		t.Fatal("the game stopped holding the ban")
	}

	// The next pass, with the server answering again, clears it.
	h.rcon.failVerb, h.rcon.failWith = "", nil
	h.sweep(t)

	if state := h.banState(t, id); state.liftedAt == nil {
		t.Fatal("the ban was not lifted on the retry")
	}
	if h.rcon.isBanned(testAGID) {
		t.Error("the game is still holding the ban")
	}
}

// TestBackfillThenEnforce: a ban on somebody who has not linked cannot be
// enforced, and must not silently look enforced. It is recorded, the reply says
// so, and the scheduler picks it up the moment they link.
func TestBackfillThenEnforce(t *testing.T) {
	h := newModHarness(t)

	reply := h.run(t, "ban", stringOpt("reason", "griefing"), userOpt(targetUser))
	if reply.UserError {
		t.Fatalf("/ban refused: %q", reply.Content)
	}
	if !strings.Contains(reply.Content, "not linked") {
		t.Errorf("the reply does not explain why nothing was enforced: %q", reply.Content)
	}
	if h.rcon.commandCount() != 0 {
		t.Fatalf("RCON was called for a target with no in-game identity: %q", h.rcon.issued())
	}

	id := h.latestBanID(t)
	if state := h.banState(t, id); state.agid != nil || state.enforcedAt != nil {
		t.Fatalf("the ban looks enforced already: %+v", state)
	}

	h.link(t, testAGID, testName, targetUser)
	h.sweep(t)

	state := h.banState(t, id)
	if state.agid == nil || *state.agid != testAGID {
		t.Fatalf("the identity was not backfilled: %+v", state)
	}
	if state.enforcedAt == nil {
		t.Fatal("the backfilled ban was not enforced")
	}
	if !h.rcon.isBanned(testAGID) {
		t.Error("the game is not holding the backfilled ban")
	}
}

// TestBackfillClosesSupersededRecords: when the player already has an active
// in-game ban, the Discord-only row can never be enforced. Leaving it open
// would hold the unenforced gauge red forever and, worse, spring a re-ban on
// the player the moment the other ban expires and backfill finally attaches.
func TestBackfillClosesSupersededRecords(t *testing.T) {
	h := newModHarness(t)
	h.link(t, testAGID, testName, targetUser)
	ctx := context.Background()
	q := h.store.Queries()

	agid, discordID := testAGID, targetUser
	byIdentity, err := q.InsertBan(ctx, gen.InsertBanParams{
		AlderonID: &agid, Reason: "in game", IssuedByDiscordID: "discord-1",
	})
	if err != nil {
		t.Fatalf("seed identity ban: %v", err)
	}
	byDiscord, err := q.InsertBan(ctx, gen.InsertBanParams{
		DiscordUserID: &discordID, Reason: "on discord", IssuedByDiscordID: "discord-1",
	})
	if err != nil {
		t.Fatalf("seed discord ban: %v", err)
	}

	h.sweep(t)

	blocked := h.banState(t, byDiscord.ID)
	if blocked.liftedAt == nil {
		t.Fatal("the superseded record was left open")
	}
	if blocked.liftReason == nil || *blocked.liftReason != "superseded by active in-game ban" {
		t.Errorf("lift_reason = %v", blocked.liftReason)
	}
	if blocked.agid != nil {
		t.Error("the identity was attached anyway, which the unique index would have refused")
	}
	if kept := h.banState(t, byIdentity.ID); kept.liftedAt != nil {
		t.Error("the active in-game ban was closed as well")
	}
}

// TestBanRefusesADuplicate. One active ban per identity is enforced by the
// database; the command checks first so the moderator gets a sentence instead
// of a constraint violation, and maps the race loser to the same sentence.
func TestBanRefusesADuplicate(t *testing.T) {
	h := newModHarness(t)
	h.link(t, testAGID, testName, targetUser)

	if reply := h.run(t, "ban", stringOpt("reason", "one"), userOpt(targetUser)); reply.UserError {
		t.Fatalf("first ban refused: %q", reply.Content)
	}
	second := h.run(t, "ban", stringOpt("reason", "two"), stringOpt("player", testAGID))
	if !second.UserError || !strings.Contains(second.Content, "already banned") {
		t.Fatalf("second ban = %q, want a refusal", second.Content)
	}
}

// TestBanOfAServerAdminIsNotRetriedForever. "Cannot ban an admin." is
// PERMANENT -- the check reads Game.ini's ServerAdmins list, so no RCON call
// can change it -- and the remediation belongs in the reply, not in a retry
// loop that runs every minute for ever.
func TestBanOfAServerAdminIsNotRetriedForever(t *testing.T) {
	h := newModHarness(t)
	h.link(t, testAGID, testName, targetUser)
	h.rcon.makeServerAdmin(testAGID)

	reply := h.run(t, "ban", stringOpt("reason", "griefing"), userOpt(targetUser))
	if !strings.Contains(reply.Content, "ServerAdmins") {
		t.Fatalf("the reply does not name the remediation: %q", reply.Content)
	}

	id := h.latestBanID(t)
	state := h.banState(t, id)
	if state.unenforceable == nil {
		t.Fatal("the ban was not flagged unenforceable")
	}
	if state.enforcedAt != nil {
		t.Fatal("a refused ban was recorded as enforced")
	}

	before := h.rcon.commandCount()
	h.sweep(t)
	h.sweep(t)
	if h.rcon.commandCount() != before {
		t.Errorf("the scheduler retried a permanent refusal: %q", h.rcon.issued())
	}
}

// TestUnbanPaths covers the three shapes a lift can take, plus the one that
// must not lift anything.
func TestUnbanPaths(t *testing.T) {
	t.Run("never reached the game", func(t *testing.T) {
		h := newModHarness(t)
		h.run(t, "ban", stringOpt("reason", "griefing"), userOpt(targetUser))
		id := h.latestBanID(t)

		reply := h.run(t, "unban", userOpt(targetUser))
		if reply.UserError {
			t.Fatalf("/unban refused: %q", reply.Content)
		}
		if state := h.banState(t, id); state.liftedAt == nil {
			t.Fatal("the unenforced ban was not closed")
		}
		if h.rcon.commandCount() != 0 {
			t.Errorf("RCON was called for a ban the game never had: %q", h.rcon.issued())
		}
	})

	t.Run("enforced in game", func(t *testing.T) {
		h := newModHarness(t)
		h.link(t, testAGID, testName, targetUser)
		h.run(t, "ban", stringOpt("reason", "griefing"), userOpt(targetUser))
		id := h.latestBanID(t)

		reply := h.run(t, "unban", userOpt(targetUser), stringOpt("reason", "appealed"))
		if reply.UserError {
			t.Fatalf("/unban refused: %q", reply.Content)
		}
		if h.rcon.isBanned(testAGID) {
			t.Fatal("the game is still holding the ban")
		}
		state := h.banState(t, id)
		if state.liftedAt == nil {
			t.Fatal("the ban was not closed")
		}
		if state.liftReason == nil || !strings.Contains(*state.liftReason, "appealed") {
			t.Errorf("lift_reason = %v, want the moderator's reason", state.liftReason)
		}
	})

	t.Run("the game had no ban to lift", func(t *testing.T) {
		h := newModHarness(t)
		h.link(t, testAGID, testName, targetUser)
		h.run(t, "ban", stringOpt("reason", "griefing"), userOpt(targetUser))
		id := h.latestBanID(t)

		// Somebody cleared bans.txt by hand: the record says banned, the game
		// disagrees. The record is what gets closed.
		h.rcon.mu.Lock()
		h.rcon.banned = nil
		h.rcon.mu.Unlock()

		reply := h.run(t, "unban", userOpt(targetUser))
		if !strings.Contains(reply.Content, "not banned in game") {
			t.Errorf("reply = %q, want it to say the game had no ban", reply.Content)
		}
		if state := h.banState(t, id); state.liftedAt == nil {
			t.Fatal("the record was left open")
		}
	})

	t.Run("the game did not answer", func(t *testing.T) {
		h := newModHarness(t)
		h.link(t, testAGID, testName, targetUser)
		h.run(t, "ban", stringOpt("reason", "griefing"), userOpt(targetUser))
		id := h.latestBanID(t)

		h.rcon.failVerb, h.rcon.failWith = "Unban", errors.New("connection reset")
		reply := h.run(t, "unban", userOpt(targetUser))
		if state := h.banState(t, id); state.liftedAt != nil {
			t.Fatal("the ban was closed without the game confirming it")
		}
		if !strings.Contains(reply.Content, "still in force") {
			t.Errorf("reply = %q, want it to say the ban stands", reply.Content)
		}
	})

	t.Run("nothing on record", func(t *testing.T) {
		h := newModHarness(t)
		h.link(t, testAGID, testName, targetUser)
		if reply := h.run(t, "unban", userOpt(targetUser)); !reply.UserError {
			t.Errorf("unbanning an unbanned player was not a refusal: %q", reply.Content)
		}
	})
}

// TestEnforcementLosesToAConcurrentUnban is the C1 race. A moderator's /unban
// can close an unenforced row while the scheduler is mid-RCON; without the
// guarded update the scheduler would then bind a game ban to a CLOSED record,
// which the audit -- which walks active bans -- could never see again.
func TestEnforcementLosesToAConcurrentUnban(t *testing.T) {
	h := newModHarness(t)
	h.link(t, testAGID, testName, targetUser)
	ctx := context.Background()

	agid := testAGID
	ban, err := h.store.Queries().InsertBan(ctx, gen.InsertBanParams{
		AlderonID: &agid, Reason: "griefing", IssuedByDiscordID: moderatorUser,
	})
	if err != nil {
		t.Fatalf("seed ban: %v", err)
	}

	// The lift lands while the Ban command is in flight, which is exactly the
	// window MarkBanEnforced's `where lifted_at is null` guard exists for.
	lifted := "lifted by a moderator"
	h.rcon.onBan = func(string) {
		if _, err := h.store.Queries().LiftBan(ctx, gen.LiftBanParams{
			ID: ban.ID, LiftReason: &lifted,
		}); err != nil {
			t.Errorf("lift mid-enforce: %v", err)
		}
	}

	h.sweep(t)

	if h.rcon.isBanned(testAGID) {
		t.Fatal("the game is holding a ban bound to a lifted record")
	}
	if unbanned := h.rcon.unbannedPlayers(); len(unbanned) != 1 || unbanned[0] != testAGID {
		t.Fatalf("no compensating unban was issued: %q", unbanned)
	}
	if state := h.banState(t, ban.ID); state.enforcedAt != nil {
		t.Error("a lifted ban was recorded as enforced")
	}
}

// TestAuditRepairsALostGameBan. The game can lose bans.txt -- wiped, restored
// from a backup, hand-edited -- and nothing else would ever notice. Re-issuing
// the ban is both the question and the repair.
func TestAuditRepairsALostGameBan(t *testing.T) {
	h := newModHarness(t)
	h.link(t, testAGID, testName, targetUser)

	h.run(t, "ban", stringOpt("reason", "griefing"), userOpt(targetUser))
	if !h.rcon.isBanned(testAGID) {
		t.Fatal("the ban was not enforced to begin with")
	}

	// bans.txt goes missing.
	h.rcon.mu.Lock()
	h.rcon.banned = nil
	h.rcon.mu.Unlock()

	h.sweep(t)

	if !h.rcon.isBanned(testAGID) {
		t.Fatal("the audit did not restore the lost ban")
	}
}

// TestModstatsShowsTheRecord. /modstats is what a moderator reads before
// deciding, so it has to show BOTH whether a ban exists and whether the game is
// actually holding it -- they are different facts.
func TestModstatsShowsTheRecord(t *testing.T) {
	h := newModHarness(t)
	h.link(t, testAGID, testName, targetUser)

	h.run(t, "warn", stringOpt("reason", "first warning"), userOpt(targetUser))
	h.run(t, "ban", stringOpt("reason", "griefing"), userOpt(targetUser), stringOpt("duration", "2d"))

	reply := h.run(t, "modstats", stringOpt("player", testAGID))
	if len(reply.Embeds) != 1 {
		t.Fatalf("embeds = %d, want one", len(reply.Embeds))
	}
	fields := map[string]string{}
	for _, f := range reply.Embeds[0].Fields {
		fields[f.Name] = f.Value
	}
	if fields["Warnings"] != "1" || fields["Bans"] != "1" {
		t.Errorf("counts = %q / %q", fields["Warnings"], fields["Bans"])
	}
	if !strings.Contains(fields["Currently banned"], "in force in game") {
		t.Errorf("Currently banned = %q", fields["Currently banned"])
	}
	if !strings.Contains(fields["Recent warnings"], "first warning") {
		t.Errorf("Recent warnings = %q", fields["Recent warnings"])
	}
}

// TestModerationCommandDefinitions guards two things Discord itself enforces at
// registration time and which no unit test would otherwise reach: required
// options come first, and a reason cannot be long enough to blow an embed field
// or the RCON command length.
func TestModerationCommandDefinitions(t *testing.T) {
	h := newModHarness(t)
	for _, cmd := range h.mod.Commands() {
		if !cmd.Defer || !cmd.Ephemeral {
			t.Errorf("/%s is not deferred and ephemeral", cmd.Definition.Name)
		}
		seenOptional := false
		for _, opt := range cmd.Definition.Options {
			if opt.Required && seenOptional {
				t.Errorf("/%s: required option %q comes after an optional one, which Discord refuses",
					cmd.Definition.Name, opt.Name)
			}
			if !opt.Required {
				seenOptional = true
			}
			if opt.Name == "reason" && (opt.MaxLength == 0 || opt.MaxLength > 400) {
				t.Errorf("/%s: reason MaxLength = %d, want a bound Discord enforces",
					cmd.Definition.Name, opt.MaxLength)
			}
		}
	}
}

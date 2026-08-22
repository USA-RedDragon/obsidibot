package pot_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SRS-Hosting/rcon"
	"github.com/USA-RedDragon/obsidibot/internal/pot"
)

// These reproduce real server responses: the FORMAT was captured from a live
// Path of Titans server, and only the player name and Alderon ID are replaced
// with an obviously fictional pair. Real players' identities do not belong in a
// source tree. If Alderon changes the format, this is where it shows up.
const (
	liveSinglePlayer = `(PlayerInfo 555-000-101): Name: testplayer / AGID: 555-000-101 / Dinosaur: Ceratosaurus / Role: Owner / Marks: 3838 / Growth: 1 / Location: (X=104037.330 Y=169175.200 Z=-596.830)`
	liveNotOnline    = `(PlayerInfo 000-000-000): No player with the username '000-000-000'.`

	// The marks commands answer with what they did and the resulting balance.
	liveAddMarks    = `(AddMarks 555-000-101 100): Added 100 Marks to 555-000-101. They now have 3938 Marks.`
	liveRemoveMarks = `(RemoveMarks 555-000-101 100): Removed 100 Marks from 555-000-101. They now have 3838 Marks.`
	// Asking to remove more than a player holds removes what they have, and
	// says so. This is the response that makes crediting the REQUESTED amount
	// a way to create marks out of nothing.
	liveRemoveClamped = `(RemoveMarks 555-000-101 999999): Removed 3838 Marks from 555-000-101. They now have 0 Marks.`
)

func TestParsePlayerInfoLiveResponse(t *testing.T) {
	player, err := pot.ParsePlayerInfo(liveSinglePlayer)
	if err != nil {
		t.Fatalf("live response did not parse: %v", err)
	}
	if player.Name != "testplayer" {
		t.Errorf("Name = %q", player.Name)
	}
	if player.AGID != "555-000-101" {
		t.Errorf("AGID = %q", player.AGID)
	}
	if player.Species != "Ceratosaurus" {
		t.Errorf("Species = %q", player.Species)
	}
	if player.Role != "Owner" {
		t.Errorf("Role = %q", player.Role)
	}
	if player.Marks != 3838 {
		t.Errorf("Marks = %d, want 3838", player.Marks)
	}
	if player.Growth != 1 {
		t.Errorf("Growth = %v, want 1", player.Growth)
	}
}

// TestPlayerCarriesNoPosition is the structural half of "obsidibot never
// publishes where a player is". The parser must match the coordinate -- a line
// without one is truncated and must not parse -- and must not keep it. A field
// that does not exist cannot leak.
func TestPlayerCarriesNoPosition(t *testing.T) {
	typ := reflect.TypeOf(pot.Player{})
	for i := range typ.NumField() {
		switch name := typ.Field(i).Name; name {
		case "X", "Y", "Z", "Location", "Position", "POI", "Coordinates":
			t.Errorf("pot.Player has a %s field; positions must not leave this package", name)
		}
	}

	// A record missing its Location is a truncated line, not a positionless
	// player, and treating it as valid would hand a caller a partial record.
	truncated := strings.Split(liveSinglePlayer, " / Location:")[0]
	if _, err := pot.ParsePlayerInfo(truncated); !errors.Is(err, pot.ErrUnparseable) {
		t.Errorf("a truncated record parsed or failed wrongly: %v", err)
	}
}

func TestParsePlayerInfoNotOnline(t *testing.T) {
	if _, err := pot.ParsePlayerInfo(liveNotOnline); !errors.Is(err, pot.ErrPlayerNotOnline) {
		t.Fatalf("offline response gave %v, want ErrPlayerNotOnline", err)
	}
	// The same string is what the game returns for a name that never existed,
	// so an unknown player and a logged-out one are indistinguishable here --
	// and both are correctly "not available right now".
	if _, err := pot.ParsePlayerInfo(`(PlayerInfo Nobody): No player with the username 'Nobody'.`); !errors.Is(err, pot.ErrPlayerNotOnline) {
		t.Fatalf("unknown name gave %v, want ErrPlayerNotOnline", err)
	}
}

// TestParsePlayerInfoRefusesGarbage: an unrecognised response must be an error
// rather than a zero Player. A marks balance silently read as 0 would let a
// deposit clamp to nothing, or worse.
func TestParsePlayerInfoRefusesGarbage(t *testing.T) {
	for _, raw := range []string{"", "\n\n", "some unrelated output"} {
		if _, err := pot.ParsePlayerInfo(raw); !errors.Is(err, pot.ErrUnparseable) {
			t.Errorf("ParsePlayerInfo(%q) gave %v, want ErrUnparseable", raw, err)
		}
	}
	// A line that announces itself as a record but does not parse is a format
	// change, and must be reported rather than skipped.
	broken := "Name: x / AGID: 111-222-333 / Dinosaur: y / Role: z / Marks: not-a-number / Growth: 1 / Location: (X=0 Y=0 Z=0)"
	if _, err := pot.ParsePlayerInfo(broken); !errors.Is(err, pot.ErrUnparseable) {
		t.Errorf("a malformed record gave %v, want ErrUnparseable", err)
	}
}

// fakeExec records the command it was given and replays a canned response.
type fakeExec struct {
	commands []string
	response string
	err      error
}

func (f *fakeExec) Execute(_ context.Context, command string) (string, error) {
	f.commands = append(f.commands, command)
	return f.response, f.err
}

// TestIdentifierInjectionIsRefused is the security property. The identifier
// comes from a Discord slash-command argument and lands on a whitespace-
// separated command line, so anything that could shift the arguments after it
// must be refused OUTRIGHT rather than escaped.
func TestIdentifierInjectionIsRefused(t *testing.T) {
	hostile := []string{
		"555-000-101 100",       // an extra argument
		"victim 999999",         // shifting a marks amount
		"a b",                   // any whitespace at all
		"a\nBan someone",        // a newline
		"a\tb",                  // a tab
		"",                      // empty
		strings.Repeat("a", 33), // over length
		`a"b`,                   // quoting
		"a;b",                   // a separator from another shell's habits
		"555-000-101 Ban 111-222-333",
	}
	for _, ident := range hostile {
		t.Run(strings.ReplaceAll(ident, "\n", "\\n"), func(t *testing.T) {
			if pot.ValidIdentifier(ident) {
				t.Fatalf("ValidIdentifier(%q) accepted a hostile identifier", ident)
			}
			exec := &fakeExec{response: liveSinglePlayer}
			client := pot.NewClient(exec, nil)
			ctx := context.Background()

			if _, err := client.PlayerInfo(ctx, ident); !errors.Is(err, pot.ErrInvalidIdentifier) {
				t.Errorf("PlayerInfo accepted it: %v", err)
			}
			if _, err := client.AddMarks(ctx, ident, 1); !errors.Is(err, pot.ErrInvalidIdentifier) {
				t.Errorf("AddMarks accepted it: %v", err)
			}
			if _, err := client.RemoveMarks(ctx, ident, 1); !errors.Is(err, pot.ErrInvalidIdentifier) {
				t.Errorf("RemoveMarks accepted it: %v", err)
			}
			if err := client.Whisper(ctx, ident, "hello"); !errors.Is(err, pot.ErrInvalidIdentifier) {
				t.Errorf("Whisper accepted it: %v", err)
			}
			if len(exec.commands) != 0 {
				t.Errorf("a command reached the server anyway: %q", exec.commands)
			}
		})
	}
}

func TestIdentifierAcceptsRealValues(t *testing.T) {
	for _, ident := range []string{"555-000-101", "testplayer", "Player.One", "a_b-c.1"} {
		if !pot.ValidIdentifier(ident) {
			t.Errorf("ValidIdentifier(%q) = false, want true", ident)
		}
	}
}

func TestCommandsAreWellFormed(t *testing.T) {
	exec := &fakeExec{response: liveAddMarks}
	client := pot.NewClient(exec, nil)
	ctx := context.Background()

	if _, err := client.AddMarks(ctx, "555-000-101", 100); err != nil {
		t.Fatalf("AddMarks: %v", err)
	}
	exec.response = liveRemoveMarks
	if _, err := client.RemoveMarks(ctx, "555-000-101", 250); err != nil {
		t.Fatalf("RemoveMarks: %v", err)
	}
	exec.response = "ok"
	if err := client.Whisper(ctx, "555-000-101", "your code is ABC123"); err != nil {
		t.Fatalf("Whisper: %v", err)
	}
	want := []string{
		"AddMarks 555-000-101 100",
		"RemoveMarks 555-000-101 250",
		"Whisper 555-000-101 your code is ABC123",
	}
	for i, w := range want {
		if exec.commands[i] != w {
			t.Errorf("command %d = %q, want %q", i, exec.commands[i], w)
		}
	}
}

// TestMarksAmountMustBePositive: a negative amount would invert the command and
// turn a deposit into a payout, which no range check upstream would catch.
func TestMarksAmountMustBePositive(t *testing.T) {
	exec := &fakeExec{response: "ok"}
	client := pot.NewClient(exec, nil)
	for _, amount := range []int64{0, -1, -1000} {
		if _, err := client.AddMarks(context.Background(), "555-000-101", amount); err == nil {
			t.Errorf("AddMarks accepted %d", amount)
		}
		if _, err := client.RemoveMarks(context.Background(), "555-000-101", amount); err == nil {
			t.Errorf("RemoveMarks accepted %d", amount)
		}
	}
	if len(exec.commands) != 0 {
		t.Errorf("a command reached the server anyway: %q", exec.commands)
	}
}

// TestMutatingCommandsDetectAnOfflinePlayer: the server reports an unknown
// player in the RESPONSE BODY rather than as an error, so a command that did
// nothing would otherwise look like success -- and for a withdraw that means
// debiting a bank for marks nobody received.
func TestMutatingCommandsDetectAnOfflinePlayer(t *testing.T) {
	exec := &fakeExec{response: `(AddMarks 000-000-000): No player with the username '000-000-000'.`}
	client := pot.NewClient(exec, nil)
	if _, err := client.AddMarks(context.Background(), "000-000-000", 100); !errors.Is(err, pot.ErrPlayerNotOnline) {
		t.Fatalf("AddMarks on an offline player gave %v, want ErrPlayerNotOnline", err)
	}
	if err := client.Whisper(context.Background(), "000-000-000", "hi"); !errors.Is(err, pot.ErrPlayerNotOnline) {
		t.Fatalf("Whisper to an offline player gave %v, want ErrPlayerNotOnline", err)
	}
}

// TestMessageSanitisation: a newline would end the command line early, so it is
// replaced rather than escaped.
func TestMessageSanitisation(t *testing.T) {
	exec := &fakeExec{response: "ok"}
	client := pot.NewClient(exec, nil)
	if err := client.Whisper(context.Background(), "555-000-101", "line one\nAnnounce pwned"); err != nil {
		t.Fatalf("Whisper: %v", err)
	}
	got := exec.commands[0]
	if strings.Contains(got, "\n") {
		t.Errorf("a newline survived into the command: %q", got)
	}
	if got != "Whisper 555-000-101 line one Announce pwned" {
		t.Errorf("command = %q", got)
	}
	if err := client.Whisper(context.Background(), "555-000-101", "   "); err == nil {
		t.Error("an all-whitespace message was accepted")
	}
}

// TestObserverNeverSeesArguments: the verb becomes a Prometheus label, so it
// must be the command WORD ALONE. Passing the whole command line would mint one
// time series per player and never free them.
func TestObserverNeverSeesArguments(t *testing.T) {
	var verbs []string
	exec := &fakeExec{response: liveSinglePlayer}
	client := pot.NewClient(exec, func(verb string, _ error, _ time.Duration) {
		verbs = append(verbs, verb)
	})
	ctx := context.Background()

	if _, err := client.PlayerInfo(ctx, "555-000-101"); err != nil {
		t.Fatalf("PlayerInfo: %v", err)
	}
	exec.response = liveAddMarks
	if _, err := client.AddMarks(ctx, "555-000-101", 5); err != nil {
		t.Fatalf("AddMarks: %v", err)
	}
	exec.response = "ok"
	if err := client.Whisper(ctx, "testplayer", "hello"); err != nil {
		t.Fatalf("Whisper: %v", err)
	}

	want := []string{"PlayerInfo", "AddMarks", "Whisper"}
	if len(verbs) != len(want) {
		t.Fatalf("observed %d commands, want %d", len(verbs), len(want))
	}
	for i, w := range want {
		if verbs[i] != w {
			t.Errorf("verb %d = %q, want %q", i, verbs[i], w)
		}
		if strings.ContainsAny(verbs[i], " -") {
			t.Errorf("verb %d = %q carries an argument; this becomes a metric label", i, verbs[i])
		}
	}
}

// TestUserMessageDistinguishesWhoseProblemItIs. "You are not in game" is the
// player's to fix; the rest are not, and must not leak an address or a command.
func TestUserMessageDistinguishesWhoseProblemItIs(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantSubstr string
	}{
		{"offline", pot.ErrPlayerNotOnline, "logged into the server"},
		{"bad identifier", pot.ErrInvalidIdentifier, "Alderon ID"},
		{"busy", rcon.ErrBusy, "busy"},
		{"timeout", &rcon.TimeoutError{}, "did not answer in time"},
		{"truncated", rcon.ErrTruncated, "cut short"},
		{"auth", rcon.ErrAuthFailed, "cannot reach the game server"},
		{"not rcon", rcon.ErrNotRCON, "cannot reach the game server"},
		{"unparseable", pot.ErrUnparseable, "did not understand"},
		{"unknown", errors.New("dial tcp 10.0.0.1:7779: connection refused"), "Something went wrong"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := pot.UserMessage(tc.err)
			if !strings.Contains(got, tc.wantSubstr) {
				t.Errorf("UserMessage(%v) = %q, want it to contain %q", tc.err, got, tc.wantSubstr)
			}
		})
	}
	if pot.UserMessage(nil) != "" {
		t.Error("UserMessage(nil) should be empty")
	}
	// Nothing operational may reach a player.
	leaky := errors.New("dial tcp 10.0.0.1:7779: connection refused")
	if strings.Contains(pot.UserMessage(leaky), "10.0.0.1") {
		t.Error("UserMessage leaked an address to the player")
	}
}

// TestCommandRejectionIsNotSuccess is the guard against the game changing under
// us. Path of Titans reports an unknown or malformed command in the RESPONSE
// BODY with a perfectly successful RCON exchange around it, so a caller that
// only checked the transport error would read these as success. On a withdraw
// that means debiting a player's bank and paying out nothing -- and on the day
// Alderon renames AddMarks, doing it to everyone at once.
//
// Both reproduce real server responses, with the identifiers anonymised.
func TestCommandRejectionIsNotSuccess(t *testing.T) {
	rejections := map[string]string{
		"unknown command":  `(NotARealCommand foo): That command does not exist.`,
		"incorrect syntax": `(AddMarks 000-000-000): Incorrect Syntax, type /help.`,
	}

	for name, response := range rejections {
		t.Run(name, func(t *testing.T) {
			exec := &fakeExec{response: response}
			client := pot.NewClient(exec, nil)
			ctx := context.Background()

			if _, err := client.AddMarks(ctx, "555-000-101", 100); !errors.Is(err, pot.ErrCommandRejected) {
				t.Errorf("AddMarks treated a rejection as %v, want ErrCommandRejected", err)
			}
			if _, err := client.RemoveMarks(ctx, "555-000-101", 100); !errors.Is(err, pot.ErrCommandRejected) {
				t.Errorf("RemoveMarks treated a rejection as %v, want ErrCommandRejected", err)
			}
			if err := client.Whisper(ctx, "555-000-101", "hi"); !errors.Is(err, pot.ErrCommandRejected) {
				t.Errorf("Whisper treated a rejection as %v, want ErrCommandRejected", err)
			}
			if _, err := client.PlayerInfo(ctx, "555-000-101"); !errors.Is(err, pot.ErrCommandRejected) {
				t.Errorf("PlayerInfo treated a rejection as %v, want ErrCommandRejected", err)
			}
		})
	}
}

// TestRejectionErrorNamesTheCommand: when this fires in production it is
// because the game changed, so the log line has to say which command died.
func TestRejectionErrorNamesTheCommand(t *testing.T) {
	err := pot.CheckRejected("AddMarks", `(AddMarks x 1): That command does not exist.`)
	if err == nil {
		t.Fatal("CheckRejected accepted a rejection")
	}
	if !strings.Contains(err.Error(), "AddMarks") {
		t.Errorf("error %q does not name the command", err)
	}
	if pot.CheckRejected("AddMarks", "(AddMarks 555-000-101 100): ok") != nil {
		t.Error("CheckRejected rejected a normal response")
	}
}

// TestParseMarksResultReadsTheLiveEcho. The game answers AddMarks and
// RemoveMarks with what it did and the resulting balance, which is stronger
// evidence than reading the balance back afterwards: there is no window in
// between for the player to earn or spend anything. All three strings are
// reproduced from real server responses, with the identifiers anonymised.
func TestParseMarksResultReadsTheLiveEcho(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		wantMoved   int64
		wantBalance int64
	}{
		{"add", liveAddMarks, 100, 3938},
		{"remove", liveRemoveMarks, 100, 3838},
		{"remove clamped at zero", liveRemoveClamped, 3838, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pot.ParseMarksResult(tc.raw)
			if err != nil {
				t.Fatalf("did not parse: %v", err)
			}
			if got.Moved != tc.wantMoved {
				t.Errorf("Moved = %d, want %d", got.Moved, tc.wantMoved)
			}
			if got.Balance != tc.wantBalance {
				t.Errorf("Balance = %d, want %d", got.Balance, tc.wantBalance)
			}
		})
	}
}

// TestRemoveMarksReportsTheClampedAmount is the property that stops marks being
// created. The server caps a removal at what the player holds; crediting the
// REQUESTED amount instead of the reported one would mint the difference.
func TestRemoveMarksReportsTheClampedAmount(t *testing.T) {
	exec := &fakeExec{response: liveRemoveClamped}
	client := pot.NewClient(exec, nil)

	got, err := client.RemoveMarks(context.Background(), "555-000-101", 999999)
	if err != nil {
		t.Fatalf("RemoveMarks: %v", err)
	}
	if got.Moved != 3838 {
		t.Fatalf("Moved = %d; crediting the requested 999999 would create marks", got.Moved)
	}
	if got.Balance != 0 {
		t.Errorf("Balance = %d, want 0", got.Balance)
	}
}

// TestUnreadableMarksResponseIsAnError, never a zero. A transfer that silently
// reports "nothing moved" is the worst way for this to fail: the caller would
// close the row as a no-op while the marks had actually moved.
func TestUnreadableMarksResponseIsAnError(t *testing.T) {
	for _, raw := range []string{"", "ok", "(AddMarks x 1): something else entirely"} {
		if _, err := pot.ParseMarksResult(raw); !errors.Is(err, pot.ErrUnparseable) {
			t.Errorf("ParseMarksResult(%q) gave %v, want ErrUnparseable", raw, err)
		}
	}
	// And the two failures the server reports in the body stay distinguishable.
	if _, err := pot.ParseMarksResult(
		`(AddMarks 000-000-000 1): No player with the username '000-000-000'.`,
	); !errors.Is(err, pot.ErrPlayerNotOnline) {
		t.Errorf("an offline player was not recognised: %v", err)
	}
	if _, err := pot.ParseMarksResult(
		`(AddMarks x 1): That command does not exist.`,
	); !errors.Is(err, pot.ErrCommandRejected) {
		t.Errorf("a rejected command was not recognised: %v", err)
	}
}

// liveServerInfo is the ServerInfo response captured from Obsidian Wilds. Note
// the GUID is labelled UUID here, and ServerGuid in the webhook payload, and
// -ServerGUID= on the command line: three names for one value.
const liveServerInfo = `(ServerInfo): Server Name: Obsidian Wilds / UUID: 09466acf-241a-41db-b94d-7026b4246892 / TimeOfDay: 1224 / Weather: ClearSky`

// TestServerInfoYieldsTheGUID is what lets the server's GUID go unconfigured:
// the value is read from the very server the RCON connection points at, which
// is by definition the right one.
func TestServerInfoYieldsTheGUID(t *testing.T) {
	exec := &fakeExec{response: liveServerInfo}
	got, err := pot.NewClient(exec, nil).ServerInfo(context.Background())
	if err != nil {
		t.Fatalf("ServerInfo: %v", err)
	}
	if got.GUID != "09466acf-241a-41db-b94d-7026b4246892" {
		t.Errorf("GUID = %q", got.GUID)
	}
	if got.Name != "Obsidian Wilds" {
		t.Errorf("Name = %q", got.Name)
	}
	if exec.commands[0] != "ServerInfo" {
		t.Errorf("command = %q", exec.commands[0])
	}
}

// TestServerInfoRefusesGarbage. There is no configured GUID to fall back on, and
// an empty one rejects every kill the game sends while looking exactly like a
// server nobody is playing on -- so an unreadable response must be a startup
// error, never an empty string.
func TestServerInfoRefusesGarbage(t *testing.T) {
	for _, raw := range []string{
		"",
		"(ServerInfo): Server Name: X / TimeOfDay: 1",
		`(ServerInfo): That command does not exist.`,
		"(ServerInfo): Server Name: X / UUID: not-a-uuid / TimeOfDay: 1",
	} {
		if _, err := pot.NewClient(&fakeExec{response: raw}, nil).ServerInfo(context.Background()); err == nil {
			t.Errorf("ServerInfo(%q) returned no error", raw)
		}
	}
}

// The moderation responses, captured VERBATIM from a live Path of Titans server
// during design (probes against nonexistent Alderon IDs, plus one consenting
// admin who was temporarily de-adminned; bans.txt was restored afterwards).
// Only the identifiers are anonymised. If Alderon changes any of these, the
// classification below is where it shows up.
const (
	liveKicked     = `(Kick 555-000-101 "banned - griefing"): Requested action against player ID.`
	liveKickFailed = `(Kick 000-000-000 "banned - griefing"): Failed to kick '000-000-000'.`
	liveBanned     = `(Ban 555-000-101 0 "obsidibot ban #1 by discord-1" "griefing"): ` +
		`Banned '555-000-101' forever, Admin reason = obsidibot ban #1 by discord-1`
	liveAlreadyBanned  = `(Ban 555-000-101 0 "a" "b"): Player '555-000-101' is already banned.`
	liveCannotBanAdmin = `(Ban 555-000-101 0 "a" "b"): Cannot ban an admin.`
	liveUnbanned       = `(Unban 555-000-101): Unbanned player with Id '555-000-101'.`
	liveUnknownBan     = `(Unban 000-000-000): Unknown ban string '000-000-000'. Perhaps you meant to use AGID?`
)

// TestModerationResponsesAreClassified is the whole contract with the game
// server: each of these is a DIFFERENT decision -- carry on, treat as enforced,
// never retry, or report nothing to lift -- and reading one as another is how a
// banned player keeps playing or an innocent one gets kicked.
func TestModerationResponsesAreClassified(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name    string
		raw     string
		call    func(*pot.Client) error
		wantErr error
	}{
		{"kick of an online player", liveKicked,
			func(c *pot.Client) error { return c.Kick(ctx, "555-000-101", "griefing") }, nil},
		{"kick of an offline player", liveKickFailed,
			func(c *pot.Client) error { return c.Kick(ctx, "000-000-000", "griefing") }, pot.ErrKickFailed},
		{"ban placed", liveBanned,
			func(c *pot.Client) error { return c.Ban(ctx, "555-000-101", "admin", "user") }, nil},
		{"ban repeated", liveAlreadyBanned,
			func(c *pot.Client) error { return c.Ban(ctx, "555-000-101", "admin", "user") }, pot.ErrAlreadyBanned},
		{"ban of a server admin", liveCannotBanAdmin,
			func(c *pot.Client) error { return c.Ban(ctx, "555-000-101", "admin", "user") }, pot.ErrCannotBanAdmin},
		{"unban", liveUnbanned,
			func(c *pot.Client) error { return c.Unban(ctx, "555-000-101") }, nil},
		{"unban of a never-banned id", liveUnknownBan,
			func(c *pot.Client) error { return c.Unban(ctx, "000-000-000") }, pot.ErrNotBanned},
		{"the command was renamed", `(Ban 555-000-101 0 "a" "b"): That command does not exist.`,
			func(c *pot.Client) error { return c.Ban(ctx, "555-000-101", "admin", "user") }, pot.ErrCommandRejected},
		{"an answer nothing recognises", `(Ban 555-000-101 0 "a" "b"): what`,
			func(c *pot.Client) error { return c.Ban(ctx, "555-000-101", "admin", "user") }, pot.ErrUnparseable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(pot.NewClient(&fakeExec{response: tc.raw}, nil))
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("got %v, want success", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("got %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestBanIsAlwaysPermanent. The game's own timed ban was verified to write a
// corrupt bans.txt row with an EMPTY id field, which binds nobody and which
// Unban can never match -- so the time argument is hardcoded to 0 and expiry is
// obsidibot's to own. This test is the guard on that.
func TestBanIsAlwaysPermanent(t *testing.T) {
	exec := &fakeExec{response: liveBanned}
	if err := pot.NewClient(exec, nil).Ban(context.Background(),
		"555-000-101", "obsidibot ban #1 by discord-1", "griefing"); err != nil {
		t.Fatalf("Ban: %v", err)
	}
	want := `Ban 555-000-101 0 "obsidibot ban #1 by discord-1" "griefing"`
	if exec.commands[0] != want {
		t.Errorf("command = %q, want %q", exec.commands[0], want)
	}
}

// TestReasonQuotingSurvivesHostileText. Multi-word reasons must stay whole --
// unquoted words split positionally into admin/user reason with the extras
// silently dropped -- and a quote inside a reason would close the quoting early
// and shift every argument after it.
func TestReasonQuotingSurvivesHostileText(t *testing.T) {
	exec := &fakeExec{response: liveBanned}
	client := pot.NewClient(exec, nil)
	ctx := context.Background()

	if err := client.Ban(ctx, "555-000-101", `admin" reason`, `user" said "stop`); err != nil {
		t.Fatalf("Ban: %v", err)
	}
	got := exec.commands[0]
	if strings.Count(got, `"`) != 4 {
		t.Errorf("command has %d quotes, want exactly the four that delimit two reasons: %q",
			strings.Count(got, `"`), got)
	}
	if got != `Ban 555-000-101 0 "admin reason" "user said stop"` {
		t.Errorf("command = %q", got)
	}

	exec.response = liveKicked
	if err := client.Kick(ctx, "555-000-101", "line one\nAnnounce pwned"); err != nil {
		t.Fatalf("Kick: %v", err)
	}
	if strings.Contains(exec.commands[1], "\n") {
		t.Errorf("a newline survived into the command: %q", exec.commands[1])
	}

	// A reason that sanitises away to nothing still has to say something: the
	// player is shown it as they are removed.
	exec.response = liveKicked
	if err := client.Kick(ctx, "555-000-101", `"""`); err != nil {
		t.Fatalf("Kick: %v", err)
	}
	if !strings.Contains(exec.commands[2], "no reason given") {
		t.Errorf("an emptied reason left the player nothing: %q", exec.commands[2])
	}
}

// TestOverLongReasonsAreTruncatedToFit. rcon.ErrCommandTooLong is returned
// BEFORE any network activity, and the same command is the same length on every
// retry -- so a ban that does not fit is permanently unenforceable. The
// wrappers cut the reasons rather than let that happen.
func TestOverLongReasonsAreTruncatedToFit(t *testing.T) {
	exec := &fakeExec{response: liveBanned}
	client := pot.NewClient(exec, nil)
	ctx := context.Background()

	long := strings.Repeat("x", 900)
	if err := client.Ban(ctx, "555-000-101", long, strings.Repeat("y", 900)); err != nil {
		t.Fatalf("Ban: %v", err)
	}
	if len(exec.commands[0]) > rcon.MaxCommandLen {
		t.Errorf("command is %d bytes, over rcon.MaxCommandLen", len(exec.commands[0]))
	}
	// Both reasons survive in part: a short one must not be starved by a long
	// one, and neither may be dropped entirely.
	if !strings.Contains(exec.commands[0], "xxx") || !strings.Contains(exec.commands[0], "yyy") {
		t.Errorf("a reason was dropped entirely: %q", exec.commands[0])
	}

	exec.response = liveKicked
	if err := client.Kick(ctx, "555-000-101", long); err != nil {
		t.Fatalf("Kick: %v", err)
	}
	if len(exec.commands[1]) > rcon.MaxCommandLen {
		t.Errorf("kick command is %d bytes, over rcon.MaxCommandLen", len(exec.commands[1]))
	}
}

// TestModerationRefusesUnsafeIdentifiers. Enforcement always targets the
// Alderon ID, never a name, and an identifier carrying a space would shift
// every argument after it -- turning a reason into another command's arguments.
func TestModerationRefusesUnsafeIdentifiers(t *testing.T) {
	for _, ident := range []string{"555-000-101 0", "a b", "", `a"b`, "a\nBan 111-222-333"} {
		exec := &fakeExec{response: liveBanned}
		client := pot.NewClient(exec, nil)
		ctx := context.Background()

		if err := client.Kick(ctx, ident, "reason"); !errors.Is(err, pot.ErrInvalidIdentifier) {
			t.Errorf("Kick(%q) = %v", ident, err)
		}
		if err := client.Ban(ctx, ident, "a", "b"); !errors.Is(err, pot.ErrInvalidIdentifier) {
			t.Errorf("Ban(%q) = %v", ident, err)
		}
		if err := client.Unban(ctx, ident); !errors.Is(err, pot.ErrInvalidIdentifier) {
			t.Errorf("Unban(%q) = %v", ident, err)
		}
		if len(exec.commands) != 0 {
			t.Errorf("a command reached the server anyway: %q", exec.commands)
		}
	}
}

// TestModerationObserverSeesOnlyTheVerb: these command lines carry an Alderon
// ID and a moderator's free text, and the observer's first argument becomes a
// Prometheus label.
func TestModerationObserverSeesOnlyTheVerb(t *testing.T) {
	var verbs []string
	exec := &fakeExec{response: liveKicked}
	client := pot.NewClient(exec, func(verb string, _ error, _ time.Duration) {
		verbs = append(verbs, verb)
	})
	ctx := context.Background()

	if err := client.Kick(ctx, "555-000-101", "griefing"); err != nil {
		t.Fatalf("Kick: %v", err)
	}
	exec.response = liveBanned
	if err := client.Ban(ctx, "555-000-101", "admin", "user"); err != nil {
		t.Fatalf("Ban: %v", err)
	}
	exec.response = liveUnbanned
	if err := client.Unban(ctx, "555-000-101"); err != nil {
		t.Fatalf("Unban: %v", err)
	}

	want := []string{"Kick", "Ban", "Unban"}
	if !reflect.DeepEqual(verbs, want) {
		t.Fatalf("verbs = %q, want %q", verbs, want)
	}
	for _, verb := range verbs {
		if strings.ContainsAny(verb, ` "-`) {
			t.Errorf("verb %q carries an argument; this becomes a metric label", verb)
		}
	}
}

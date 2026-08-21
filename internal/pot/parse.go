// Package pot turns Path of Titans RCON output into values obsidibot can act
// on, and turns obsidibot's intentions into RCON commands.
//
// # Positions stop here
//
// PlayerInfo reports where a player is standing. That coordinate is NOT
// captured into any type this package exports. It is matched by the pattern —
// the line does not parse without it — and then dropped on the floor. This is
// deliberate and it is the enforcement, not a convention: a field that does not
// exist cannot be rendered into a kill feed, a /stats embed, or a log line by
// someone who did not know the rule.
package pot

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ErrPlayerNotOnline means the server has no such player connected right now.
//
// It is a distinct error because it is the ONE failure in this package that is
// the player's to fix rather than the operator's: banking and linking both
// require the player to be in game, and telling them "internal error" when they
// simply need to log in would be a support ticket every time.
var ErrPlayerNotOnline = errors.New("pot: no such player is connected")

// ErrUnparseable means the server answered with something this package does not
// recognise. The game's output format is Alderon's to change, and a silent
// mis-parse of a marks balance would be far worse than a refusal.
var ErrUnparseable = errors.New("pot: could not parse the server's response")

// ErrCommandRejected means the server refused the command itself -- it does not
// exist, or its arguments were wrong.
//
// This is the error that catches the game changing under us. Everything in this
// package is built on command names Alderon can rename in any patch, and the
// server announces that by answering "That command does not exist." with a
// perfectly successful RCON exchange around it.
var ErrCommandRejected = errors.New("pot: the server rejected the command")

// CheckRejected reports ErrCommandRejected if raw is the server refusing the
// command. Every command this package sends passes through it.
func CheckRejected(verb, raw string) error {
	if rejected(raw) {
		return fmt.Errorf("%w: %s: %s", ErrCommandRejected, verb, strings.TrimSpace(firstLine(raw)))
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// Player is one PlayerInfo record.
//
// Marks is the balance on the player's CURRENT CHARACTER. Path of Titans keeps
// marks per character, not per account, which is why banking operates on
// "whichever dinosaur you are logged in as" — there is no account-wide balance
// for it to operate on instead.
type Player struct {
	Name    string
	AGID    string
	Species string
	Role    string
	Marks   int64
	Growth  float64
}

// The response to PlayerInfo, one record per line, prefixed with the command
// that produced it:
//
//	(PlayerInfo 555-000-101): Name: testplayer / AGID: 555-000-101 / Dinosaur: Ceratosaurus / Role: Owner / Marks: 3838 / Growth: 1 / Location: (X=104037.330 Y=169175.200 Z=-596.830)
//
// The pattern searches within a line rather than anchoring to it, because of
// that prefix. Location is matched so a truncated line fails to parse, and is
// then deliberately discarded; see the package comment.
//
//nolint:gochecknoglobals // compiled once and never reassigned
var (
	playerRE = regexp.MustCompile(
		`Name:\s*(?P<name>.*?)\s*/\s*` +
			`AGID:\s*(?P<agid>[\d\-]+)\s*/\s*` +
			`Dinosaur:\s*(?P<species>.*?)\s*/\s*` +
			`Role:\s*(?P<role>.*?)\s*/\s*` +
			`Marks:\s*(?P<marks>-?[\d.]+)\s*/\s*` +
			`Growth:\s*(?P<growth>-?[\d.]+)\s*/\s*` +
			`Location:\s*\(\s*X=-?[\d.]+\s+Y=-?[\d.]+\s+Z=-?[\d.]+\s*\)`)
)

// The server's refusals, all matched as plain substrings rather than patterns.
//
// These are literals, and a regexp alternation over literals defeats Go's
// literal-prefix fast path: the two guards below ran on EVERY RCON reply and
// cost 2714ns against 19ns for strings.Contains -- 141x, for no expressiveness.
const (
	//   (PlayerInfo 000-000-000): No player with the username '000-000-000'.
	markerNotOnline = "No player with the username"

	// The server reports a command it could not run IN THE RESPONSE BODY, not
	// as a protocol error:
	//   (AddMarks 000-000-000): Incorrect Syntax, type /help.
	//   (NotARealCommand foo): That command does not exist.
	//
	// These MUST be detected. A caller that treated them as success would, for
	// a withdraw, debit a player's bank and pay out nothing -- and the day
	// Alderon renames AddMarks, it would do that to everyone at once.
	markerNoSuchCommand = "That command does not exist"
	markerBadSyntax     = "Incorrect Syntax"
)

// notOnline reports whether a response says the player is not connected.
func notOnline(raw string) bool { return strings.Contains(raw, markerNotOnline) }

// rejected reports whether the server refused the command itself.
func rejected(raw string) bool {
	return strings.Contains(raw, markerNoSuchCommand) || strings.Contains(raw, markerBadSyntax)
}

// MarksResult is what AddMarks or RemoveMarks reported.
//
// The game answers these commands with the outcome, not just an
// acknowledgement:
//
//	(AddMarks 555-000-101 100): Added 100 Marks to 555-000-101. They now have 3938 Marks.
//	(RemoveMarks 555-000-101 100): Removed 100 Marks from 555-000-101. They now have 3838 Marks.
//
// That makes the echo AUTHORITATIVE, and better than reading the balance back
// afterwards: it is the server stating what it did, with no window in between
// for the player to earn or spend anything.
//
// Moved is what ACTUALLY moved, which is not always what was asked for. The
// game clamps a removal at zero and says so -- asking to remove 999999 from a
// player holding 3838 answers "Removed 3838 Marks ... They now have 0 Marks."
// Crediting the requested amount instead of this one would create marks out of
// nothing.
type MarksResult struct {
	Moved   int64
	Balance int64
}

//nolint:gochecknoglobals // compiled once and never reassigned
var marksRE = regexp.MustCompile(
	`(?:Added|Removed)\s+(?P<moved>\d+)\s+Marks\s+(?:to|from)\s+\S+\.\s*` +
		`They now have\s+(?P<balance>-?\d+)\s+Marks`)

// ParseMarksResult reads the response to AddMarks or RemoveMarks.
//
// A response it does not recognise is an ERROR, never a zero: a marks transfer
// that silently reports "nothing moved" is the worst possible way for this to
// fail, and the caller is expected to park the operation for review instead.
func ParseMarksResult(raw string) (MarksResult, error) {
	if err := CheckRejected("marks", raw); err != nil {
		return MarksResult{}, err
	}
	if notOnline(raw) {
		return MarksResult{}, ErrPlayerNotOnline
	}

	match := marksRE.FindStringSubmatch(raw)
	if match == nil {
		return MarksResult{}, fmt.Errorf("%w: no marks result in %q", ErrUnparseable, firstLine(raw))
	}
	moved, err := strconv.ParseInt(match[marksRE.SubexpIndex("moved")], 10, 64)
	if err != nil {
		return MarksResult{}, fmt.Errorf("%w: unreadable moved amount", ErrUnparseable)
	}
	balance, err := strconv.ParseInt(match[marksRE.SubexpIndex("balance")], 10, 64)
	if err != nil {
		return MarksResult{}, fmt.Errorf("%w: unreadable balance", ErrUnparseable)
	}
	return MarksResult{Moved: moved, Balance: balance}, nil
}

// agidMarker identifies a line that was meant to be a player record. A line
// carrying it that does not parse is a format change worth reporting rather
// than skipping: silently ignoring it would read as "nobody is online", and for
// banking that is the difference between a refusal and a wrong balance.
const agidMarker = "AGID:"

// ParsePlayerInfo reads the response to PlayerInfo for a single player.
func ParsePlayerInfo(raw string) (Player, error) {
	if err := CheckRejected("PlayerInfo", raw); err != nil {
		return Player{}, err
	}
	if notOnline(raw) {
		return Player{}, ErrPlayerNotOnline
	}

	for line := range strings.SplitSeq(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		player, ok := parsePlayer(line)
		if ok {
			return player, nil
		}
		if strings.Contains(line, agidMarker) {
			return Player{}, fmt.Errorf("%w: a line looked like a player record but did not parse", ErrUnparseable)
		}
	}
	// No record and no refusal: an empty or unrecognised response. Refusing is
	// the only safe reading -- treating it as "not online" would let a broken
	// parser look like a quiet server.
	return Player{}, fmt.Errorf("%w: no player record in the response", ErrUnparseable)
}

// parsePlayer extracts one record, reporting whether the line held one.
func parsePlayer(line string) (Player, bool) {
	match := playerRE.FindStringSubmatch(line)
	if match == nil {
		return Player{}, false
	}

	field := func(name string) string {
		return match[playerRE.SubexpIndex(name)]
	}

	// The pattern already constrained these to numeric shapes, so a failure
	// here means something out of range. Treat it as a non-match rather than
	// returning a zeroed balance: a marks reading of 0 that should have been
	// 3838 is the worst possible way to fail.
	marks, err := strconv.ParseFloat(field("marks"), 64)
	if err != nil {
		return Player{}, false
	}
	growth, err := strconv.ParseFloat(field("growth"), 64)
	if err != nil {
		return Player{}, false
	}

	return Player{
		Name:    field("name"),
		AGID:    field("agid"),
		Species: field("species"),
		Role:    field("role"),
		// Marks is reported as a bare integer in every response seen, but the
		// pattern accepts a decimal because the growth field next to it is one.
		// Truncating rather than rounding keeps a balance from being read as
		// more than the player actually holds.
		Marks:  int64(marks),
		Growth: growth,
	}, true
}

// ServerInfo is what the ServerInfo command reports.
//
// The response, with the identifiers anonymised:
//
//	(ServerInfo): Server Name: Obsidian Wilds / UUID: 09466acf-241a-41db-b94d-7026b4246892 / TimeOfDay: 1224 / Weather: ClearSky
//
// Note the field is labelled UUID, not GUID -- it is the same value the server
// is launched with as -ServerGUID= and that every webhook carries as
// "ServerGuid", under a third name.
type ServerInfo struct {
	Name string
	GUID string
}

//nolint:gochecknoglobals // compiled once and never reassigned
var serverInfoRE = regexp.MustCompile(
	`Server Name:\s*(?P<name>.*?)\s*/\s*UUID:\s*(?P<uuid>[0-9a-fA-F-]{36})`)

// ParseServerInfo reads the response to ServerInfo.
func ParseServerInfo(raw string) (ServerInfo, error) {
	if err := CheckRejected("ServerInfo", raw); err != nil {
		return ServerInfo{}, err
	}
	match := serverInfoRE.FindStringSubmatch(raw)
	if match == nil {
		return ServerInfo{}, fmt.Errorf("%w: no server info in %q", ErrUnparseable, firstLine(raw))
	}
	return ServerInfo{
		Name: match[serverInfoRE.SubexpIndex("name")],
		GUID: match[serverInfoRE.SubexpIndex("uuid")],
	}, nil
}

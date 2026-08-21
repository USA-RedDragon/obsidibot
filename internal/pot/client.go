package pot

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/SRS-Hosting/rcon"
)

// Executor runs one RCON command. The concrete implementation is *rcon.Client;
// the interface is here so every caller above this package can be tested
// without a socket, and so the game's REST API -- which has an atomic marks
// endpoint and is simply not enabled on this server yet -- can be swapped in
// behind it later without touching anything above.
type Executor interface {
	Execute(ctx context.Context, command string) (string, error)
}

// Observer is told about each command for metrics. verb is the command word
// only; its ARGUMENTS ARE NEVER PASSED, because they carry player names and
// Alderon IDs and this is a Prometheus label.
type Observer func(verb string, err error, elapsed time.Duration)

// Client issues the small set of commands obsidibot needs.
type Client struct {
	exec   Executor
	notify Observer
}

// NewClient wraps an executor. notify may be nil.
func NewClient(exec Executor, notify Observer) *Client {
	return &Client{exec: exec, notify: notify}
}

// identifierRE bounds what may be interpolated into a command as a player.
//
// THIS IS AN INJECTION GUARD, not a formatting nicety. The identifier reaches
// here from a Discord slash-command argument, and RCON commands are
// whitespace-separated: "Whisper <Username> [Message]" takes everything after
// the first token as the message, so an identifier containing a space shifts
// every argument after it. Restricting to the characters an Alderon ID or a
// username can actually contain removes the whole class rather than trying to
// escape it.
//
//nolint:gochecknoglobals // compiled once and never reassigned
var identifierRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)

// agidRE matches an Alderon ID: three groups of three digits.
//
//nolint:gochecknoglobals // compiled once and never reassigned
var agidRE = regexp.MustCompile(`^\d{3}-\d{3}-\d{3}$`)

// ErrInvalidIdentifier means a player identifier was not something that could
// safely be put on a command line.
var ErrInvalidIdentifier = errors.New("pot: invalid player identifier")

// ValidIdentifier reports whether ident is safe to interpolate into a command.
func ValidIdentifier(ident string) bool {
	return identifierRE.MatchString(ident)
}

// IsAGID reports whether ident has the shape of an Alderon ID.
func IsAGID(ident string) bool {
	return agidRE.MatchString(ident)
}

// maxMessageLen bounds a message body. rcon.MaxCommandLen caps the whole
// command; this leaves comfortable room for the verb and the recipient.
const maxMessageLen = 700

// PlayerInfo returns the current state of one connected player.
//
// It returns ErrPlayerNotOnline when the server has no such player, which is
// the gate every banking and linking operation stands behind: marks live on the
// character the player is currently controlling, so there is nothing to read or
// move while they are logged out.
func (c *Client) PlayerInfo(ctx context.Context, ident string) (Player, error) {
	if !ValidIdentifier(ident) {
		return Player{}, fmt.Errorf("%w: %q", ErrInvalidIdentifier, ident)
	}
	raw, err := c.run(ctx, "PlayerInfo", ident)
	if err != nil {
		return Player{}, err
	}
	return ParsePlayerInfo(raw)
}

// PlayerInfoAll returns every connected player.
func (c *Client) PlayerInfoAll(ctx context.Context) ([]Player, int, int, error) {
	raw, err := c.run(ctx, "PlayerInfoAll")
	if err != nil {
		return nil, NoReportedTotal, 0, err
	}
	players, total, failures := ParsePlayerInfoAll(raw)
	return players, total, failures, nil
}

// AddMarks gives marks to a player and reports what the server said it did.
//
// THIS COMMAND IS NOT IDEMPOTENT. Running it twice gives twice the marks.
// Callers must issue it at most once per ledger row and recover by OBSERVING
// the player's balance, never by retrying. See internal/bank.
//
// The returned MarksResult is the server's own account of the outcome, which is
// the strongest confirmation available -- stronger than reading the balance back
// afterwards, because there is no window in between.
func (c *Client) AddMarks(ctx context.Context, ident string, amount int64) (MarksResult, error) {
	return c.marks(ctx, "AddMarks", ident, amount)
}

// RemoveMarks takes marks from a player, reporting what actually moved.
//
// The server CLAMPS at zero: asking to remove more than a player holds removes
// what they have and says so. MarksResult.Moved is therefore the figure to
// credit, never the amount requested.
func (c *Client) RemoveMarks(ctx context.Context, ident string, amount int64) (MarksResult, error) {
	return c.marks(ctx, "RemoveMarks", ident, amount)
}

func (c *Client) marks(ctx context.Context, verb, ident string, amount int64) (MarksResult, error) {
	if !ValidIdentifier(ident) {
		return MarksResult{}, fmt.Errorf("%w: %q", ErrInvalidIdentifier, ident)
	}
	if amount <= 0 {
		// A zero would be a pointless round trip; a negative would invert the
		// command's meaning and silently turn a deposit into a payout.
		return MarksResult{}, fmt.Errorf("pot: %s amount must be positive, got %d", verb, amount)
	}
	raw, err := c.run(ctx, verb, ident, strconv.FormatInt(amount, 10))
	if err != nil {
		return MarksResult{}, err
	}
	// ParseMarksResult also catches the two failures the server reports in the
	// response BODY rather than as protocol errors -- an unknown player, and a
	// command it does not have. Treating either as success on a withdraw would
	// debit a bank and pay out nothing.
	return ParseMarksResult(raw)
}

// Whisper sends a private in-game message to one player.
//
// This is how a link code reaches the person who must prove they control the
// account: it goes to the game client logged into that identity and nowhere
// else, which is the whole proof.
func (c *Client) Whisper(ctx context.Context, ident, message string) error {
	return c.message(ctx, "Whisper", ident, message)
}

// SystemMessage sends a system notification to one player. It is the fallback
// for Whisper on clients where a whisper is easy to miss.
func (c *Client) SystemMessage(ctx context.Context, ident, message string) error {
	return c.message(ctx, "SystemMessage", ident, message)
}

func (c *Client) message(ctx context.Context, verb, ident, message string) error {
	if !ValidIdentifier(ident) {
		return fmt.Errorf("%w: %q", ErrInvalidIdentifier, ident)
	}
	message = sanitiseMessage(message)
	if message == "" {
		return errors.New("pot: message must not be empty")
	}
	raw, err := c.run(ctx, verb, ident, message)
	if err != nil {
		return err
	}
	if err := CheckRejected(verb, raw); err != nil {
		return err
	}
	if notOnlineRE.MatchString(raw) {
		return ErrPlayerNotOnline
	}
	return nil
}

// sanitiseMessage makes a message safe to put at the end of a command line. An
// RCON command is a single line, so anything that could end it early is
// replaced rather than escaped.
func sanitiseMessage(message string) string {
	message = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\x00' {
			return ' '
		}
		return r
	}, message)
	message = strings.TrimSpace(message)
	if len(message) > maxMessageLen {
		message = message[:maxMessageLen]
	}
	return message
}

// run issues one command and reports it to the observer.
func (c *Client) run(ctx context.Context, verb string, args ...string) (string, error) {
	command := verb
	if len(args) > 0 {
		command += " " + strings.Join(args, " ")
	}

	start := time.Now()
	raw, err := c.exec.Execute(ctx, command)
	if c.notify != nil {
		c.notify(verb, err, time.Since(start))
	}
	if err != nil {
		// The command text is deliberately not wrapped into the error: it
		// carries the player identifier, and this error is rendered into logs.
		return "", fmt.Errorf("rcon %s: %w", verb, err)
	}
	return raw, nil
}

// UserMessage turns an error from this package into something worth showing a
// player in Discord.
//
// The distinctions matter to whoever reads them: "you are not in game" is the
// player's to fix, "the server is not answering" is the operator's, and
// conflating them turns every logged-out player into a support ticket. Anything
// unrecognised deliberately says nothing specific, because the underlying text
// can contain an address or a command line.
func UserMessage(err error) string {
	var timeout *rcon.TimeoutError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrPlayerNotOnline):
		return "You need to be logged into the server for that."
	case errors.Is(err, ErrInvalidIdentifier):
		return "That does not look like an Alderon ID or a player name."
	case errors.Is(err, rcon.ErrBusy):
		return "The server is busy right now — try again in a moment."
	case errors.As(err, &timeout), errors.Is(err, context.DeadlineExceeded):
		return "The game server did not answer in time — try again in a moment."
	case errors.Is(err, rcon.ErrTruncated):
		return "The game server's reply was cut short — try again in a moment."
	case errors.Is(err, rcon.ErrAuthFailed), errors.Is(err, rcon.ErrNotRCON):
		// Both are deployment faults the player can do nothing about, and
		// naming them would tell an attacker which one they had found.
		return "The bot cannot reach the game server right now. This has been logged."
	case errors.Is(err, ErrUnparseable), errors.Is(err, ErrCommandRejected):
		return "The game server replied with something the bot did not understand. This has been logged."
	default:
		return "Something went wrong talking to the game server. This has been logged."
	}
}

// ServerInfo asks the game which server this is.
//
// It exists so the server's GUID never has to be configured: the value is
// discoverable from the very server the RCON connection already points at,
// which is by definition the right one. A hand-copied GUID is a thing that can
// be wrong, and a wrong one silently rejects every kill the game sends.
func (c *Client) ServerInfo(ctx context.Context) (ServerInfo, error) {
	raw, err := c.run(ctx, "ServerInfo")
	if err != nil {
		return ServerInfo{}, err
	}
	return ParseServerInfo(raw)
}

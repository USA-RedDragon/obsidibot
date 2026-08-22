package gamecmd

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/USA-RedDragon/obsidibot/internal/bank"
)

// The closed set of command names. Everything the metric's "command" label can
// carry is one of these -- never raw chat text, which is unbounded and
// player-supplied.
const (
	CommandLink     = "link"
	CommandDeposit  = "deposit"
	CommandWithdraw = "withdraw"
	CommandBalance  = "balance"
	CommandHelp     = "help"
	CommandUnknown  = "unknown"
)

// defaultEscape is the game's default PlayerCommandEscapeChars. It is only a
// fallback for rendering usage text when a message somehow arrives without a
// prefix; parsing never depends on it.
const defaultEscape = '!'

// Command is one parsed in-game command.
type Command struct {
	// Name is drawn from the closed set above; anything unrecognised is
	// CommandUnknown, so raw chat never reaches a metric label.
	Name string
	// Args is everything after the command word.
	Args []string
	// Escape is the prefix rune the player actually typed, kept so usage text
	// can echo their server's convention back at them.
	Escape rune
}

// Parse reads one PlayerCommand message.
//
// The escape char is ANY single leading non-letter/digit rune, not '!':
// PlayerCommandEscapeChars is server-side configuration this bot cannot see,
// and the webhook delivers the message prefix included. Hardcoding '!' would
// silently break every command the day the server is reconfigured.
func Parse(message string) Command {
	message = strings.TrimSpace(message)

	escape := rune(defaultEscape)
	if r, size := utf8.DecodeRuneInString(message); r != utf8.RuneError &&
		!unicode.IsLetter(r) && !unicode.IsDigit(r) {
		escape = r
		message = message[size:]
	}

	fields := strings.Fields(message)
	if len(fields) == 0 {
		return Command{Name: CommandUnknown, Escape: escape}
	}

	name := strings.ToLower(fields[0])
	switch name {
	case CommandLink, CommandDeposit, CommandWithdraw, CommandBalance, CommandHelp:
	default:
		name = CommandUnknown
	}
	return Command{Name: name, Args: fields[1:], Escape: escape}
}

// ParseAmount reads the optional amount argument of !deposit and !withdraw.
//
// Absent and "all" both mean everything available, matching the slash
// commands, where omitting the amount is the common case. Commas are forgiven
// because a player copying "1,000" out of a balance whisper has done nothing
// wrong. Anything non-positive or non-numeric is a refusal (ok=false), never a
// zero that would read as "nothing to move".
func ParseAmount(args []string) (int64, bool) {
	if len(args) == 0 {
		return bank.AmountAll, true
	}
	raw := strings.ReplaceAll(args[0], ",", "")
	if strings.EqualFold(raw, "all") {
		return bank.AmountAll, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

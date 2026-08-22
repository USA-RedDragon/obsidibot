// Package moderation holds the parts of warning and banning that are not a
// Discord command: the duration format moderators type, the feed embeds, and
// the scheduler that enforces and expires bans in the game.
//
// # Expiry lives here, not in the game
//
// Path of Titans has a timed ban and it is BROKEN -- verified live: it reports
// success and writes a bans.txt row with an empty id field, which binds nobody
// and which Unban can never match by id. obsidibot therefore issues only
// permanent game bans and owns expiry itself. The bans row is the durable
// record; the game is a thing that record is asserted against, repeatedly if
// need be.
package moderation

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MaxBanDuration bounds what a moderator can type. Anything longer is meant to
// be a permanent ban, and a five-figure hour count is far more likely to be a
// typo than an intention.
const MaxBanDuration = 5 * 365 * 24 * time.Hour

// MinBanDuration is the shortest ban the scheduler can honour: it wakes once a
// minute, so a thirty-second ban would be a promise it cannot keep.
const MinBanDuration = time.Minute

// maxDays bounds the day prefix before it is multiplied, so a wild figure is
// refused rather than overflowing into a small or negative duration.
const maxDays = 5 * 366

// dayPrefixRE matches the day component, which Go's own parser has no unit for.
// Days are a PREFIX only: "3h1d" is a typo, not a duration, and accepting it
// would silently produce something the moderator did not type.
//
//nolint:gochecknoglobals // compiled once and never reassigned
var dayPrefixRE = regexp.MustCompile(`^(\d+)d`)

// durationError is a refusal phrased for the person who typed the duration.
//
// It is a string type rather than errors.New values because these messages are
// shown VERBATIM in the ephemeral reply: they are whole capitalised sentences,
// which is the opposite of what an internal error string should look like.
type durationError string

func (e durationError) Error() string { return string(e) }

const (
	errEmptyDuration = durationError("Give a duration like `1d3h30m`, or leave it out for a permanent ban.")
	errUnreadable    = durationError("I could not read that duration. Use something like `1d`, `3h30m` or `1d3h43m`.")
	errSubSecond     = durationError("Ban durations are measured in minutes at the smallest. Try something like `30m`.")
	errTooShort      = durationError("A ban has to last at least a minute — the expiry check runs once a minute.")
	errTooLong       = durationError("That is longer than five years. Leave the duration out for a permanent ban.")
	errNotPositive   = durationError("A ban duration has to be positive. Leave it out for a permanent ban.")
)

// ParseDuration reads the extended Go duration format moderators type:
// an optional whole-day prefix followed by whatever time.ParseDuration
// understands, so "1d", "3h30m" and "1d3h43m" all work.
//
// It is deliberately stricter than time.ParseDuration in both directions: no
// sub-second units (the scheduler's tick cannot honour them, and "1500ms" is
// nearly always a mistyped "15m"), and no fractional days.
func ParseDuration(s string) (time.Duration, error) {
	// Lower-cased so a moderator holding shift ("1D3H") is not refused for it.
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, errEmptyDuration
	}
	// Checked on the whole input rather than on the parsed result: by the time
	// Go has parsed it, "1500ms" and "1.5s" are indistinguishable from a
	// duration typed in seconds, and both should be refused.
	if strings.Contains(s, "ms") || strings.Contains(s, "us") ||
		strings.Contains(s, "µs") || strings.Contains(s, "ns") {
		return 0, errSubSecond
	}

	total := time.Duration(0)
	rest := s
	if m := dayPrefixRE.FindStringSubmatch(s); m != nil {
		days, err := strconv.Atoi(m[1])
		if err != nil || days > maxDays {
			return 0, errTooLong
		}
		total = time.Duration(days) * 24 * time.Hour
		rest = s[len(m[0]):]
	}
	if strings.Contains(rest, "d") {
		// Either a second day component or a day unit after an hour one. Both
		// are typos, and guessing at what was meant is worse than refusing.
		return 0, errUnreadable
	}

	if rest != "" {
		parsed, err := time.ParseDuration(rest)
		if err != nil {
			return 0, errUnreadable
		}
		total += parsed
	}

	switch {
	case total <= 0:
		return 0, errNotPositive
	case total < MinBanDuration:
		return 0, errTooShort
	case total > MaxBanDuration:
		return 0, errTooLong
	}
	return total, nil
}

// FormatExpiry renders a ban's end as a Discord timestamp, so every reader sees
// it in their own timezone and it stays correct as time passes.
func FormatExpiry(expiresAt *time.Time) string {
	if expiresAt == nil {
		return "permanent"
	}
	return fmt.Sprintf("<t:%d:R>", expiresAt.Unix())
}

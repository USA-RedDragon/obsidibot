// Package linkcode generates and verifies the one-time codes that prove
// control of an in-game identity.
//
// The code format is a PROTOCOL between two frontends -- the Discord /link
// command and the in-game !link command -- which is why it lives here rather
// than inside either of them: a shared format defined in one frontend would
// make the other import a pile of UI machinery for six lines of crypto, and a
// format defined twice would drift.
//
// The security model: the plaintext code exists ONLY in the whisper the game
// delivers to the player. The database stores its SHA-256, so database access
// cannot be turned into claiming someone's identity; the whisper arriving on
// the client logged into that identity IS the proof of control.
package linkcode

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"strings"
)

// alphabet omits 0/O/1/I/L and every lookalike, because the player reads this
// off an in-game chat line and types it into Discord. A code that is
// unambiguous to read is worth more here than one extra bit of entropy.
const alphabet = "23456789ABCDEFGHJKMNPQRSTVWXYZ"

// length gives 30^6 ~ 7.3e8 codes. Against a handful of attempts inside a
// five-minute window that is not guessable, and it stays short enough to
// retype without resentment.
const length = 6

// New returns a fresh code.
//
// The modulo is unbiased: 256 is not a multiple of 30, so bytes at or above
// the largest multiple are rejected rather than wrapped -- wrapping would make
// the first few letters of the alphabet likelier.
func New() (string, error) {
	letters := []byte(alphabet)
	out := make([]byte, 0, length)
	limit := 256 - (256 % len(letters))
	buf := make([]byte, 1)
	for len(out) < length {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("read random bytes: %w", err)
		}
		if int(buf[0]) >= limit {
			continue
		}
		out = append(out, letters[int(buf[0])%len(letters)])
	}
	return string(out), nil
}

// Normalise forgives the ways a code gets mangled between an in-game chat
// line and a Discord text box. Case folding is not a weakening: the alphabet
// has no lower-case members, so nothing collides.
func Normalise(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

// Hash is the stored form of a code.
func Hash(code string) []byte {
	sum := sha256.Sum256([]byte(code))
	return sum[:]
}

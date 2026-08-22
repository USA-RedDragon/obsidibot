// Package numfmt renders numbers for players.
//
// It exists so the Discord commands and the in-game whisper replies cannot
// drift in how they print a marks balance: one frontend showing "12,800" while
// the other shows "12800" reads as two different numbers to a player comparing
// them.
package numfmt

import "strconv"

// Commas groups thousands, because marks balances run to five and six digits
// and "1247300" is not a number anybody reads correctly at a glance.
func Commas(v int64) string {
	digits := strconv.FormatInt(v, 10)
	sign := ""
	if v < 0 {
		sign, digits = "-", digits[1:]
	}
	if len(digits) <= 3 {
		return sign + digits
	}
	out := make([]byte, 0, len(digits)+(len(digits)-1)/3)
	for i, d := range []byte(digits) {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, d)
	}
	return sign + string(out)
}

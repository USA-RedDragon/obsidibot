package numfmt_test

import (
	"testing"

	"github.com/USA-RedDragon/obsidibot/internal/numfmt"
)

func TestCommas(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{7, "7"},
		{999, "999"},
		{1000, "1,000"},
		{3838, "3,838"},
		{999999, "999,999"},
		{1000000, "1,000,000"},
		{1247300, "1,247,300"},
		{-1, "-1"},
		{-999, "-999"},
		{-1000, "-1,000"},
		{-1234567, "-1,234,567"},
	}
	for _, tc := range tests {
		if got := numfmt.Commas(tc.in); got != tc.want {
			t.Errorf("Commas(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

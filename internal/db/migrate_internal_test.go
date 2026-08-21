package db

import (
	"strings"
	"testing"
)

// TestStripTxControlStrips covers the shapes the migrations have (a leading
// begin under a comment header, a trailing commit before trailing
// whitespace/comments) and the ones they may grow (dollar-quoted function
// bodies), plus the idempotence guarantee: stripping already-stripped input must be a no-op, because a
// crash-restart may strip the same file twice.
func TestStripTxControlStrips(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "leading begin under a comment header",
			in:   "-- header comment\n-- more header\nbegin;\ncreate table x (id int);\ncommit;\n",
			want: "-- header comment\n-- more header\n\ncreate table x (id int);\n\n",
		},
		{
			name: "trailing commit followed by newline and comment",
			in:   "begin;\ncreate table x (id int);\ncommit;\n-- applied\n",
			want: "\ncreate table x (id int);\n\n-- applied\n",
		},
		{
			name: "case-insensitive with noise words and no trailing newline",
			in:   "BeGiN WoRk;\ncreate table x (id int);\nCOMMIT TRANSACTION;",
			want: "\ncreate table x (id int);\n",
		},
		{
			name: "already stripped input is untouched",
			in:   "create table x (id int);\ninsert into x values (1);\n",
			want: "create table x (id int);\ninsert into x values (1);\n",
		},
		{
			name: "plpgsql begin inside dollar quotes is not transaction control",
			in:   "begin;\ncreate function f() returns void language plpgsql as $body$ begin null; end $body$;\ncommit;\n",
			want: "\ncreate function f() returns void language plpgsql as $body$ begin null; end $body$;\n\n",
		},
		{
			name: "quoted literals mentioning commit are data",
			in:   "begin;\ninsert into x values ('commit;');\ncommit;\n",
			want: "\ninsert into x values ('commit;');\n\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := stripTxControl(tt.in)
			if err != nil {
				t.Fatalf("stripTxControl() error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("stripTxControl() = %q, want %q", got, tt.want)
			}
			again, err := stripTxControl(got)
			if err != nil {
				t.Fatalf("stripTxControl() on own output error: %v", err)
			}
			if again != got {
				t.Fatalf("stripTxControl() not idempotent: %q -> %q", got, again)
			}
		})
	}
}

// TestStripTxControlRejects pins the refusal cases: any transaction control
// beyond the outer pair would either end the runner's transaction early
// (decoupling the file from its ledger insert) or roll part of the file back,
// so such files must never be applied.
func TestStripTxControlRejects(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{
			name: "interior begin",
			in:   "begin;\ncreate table x (id int);\nbegin;\ncreate table y (id int);\ncommit;\n",
		},
		{
			name: "interior commit before more statements",
			in:   "begin;\ncreate table x (id int);\ncommit;\ncreate table y (id int);\ncommit;\n",
		},
		{
			name: "rollback anywhere",
			in:   "create table x (id int);\nrollback;\n",
		},
		{
			name: "begin with modes is not a bare wrapper",
			in:   "begin isolation level serializable;\ncreate table x (id int);\ncommit;\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := stripTxControl(tt.in)
			if err == nil {
				t.Fatalf("stripTxControl() accepted %q as %q, want error", tt.in, out)
			}
			if !strings.Contains(err.Error(), "transaction control") {
				t.Fatalf("stripTxControl() error %q does not name transaction control", err)
			}
		})
	}
}

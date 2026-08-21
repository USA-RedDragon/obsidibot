package cmd

import (
	"strings"
	"testing"

	"github.com/USA-RedDragon/obsidibot/internal/config"
)

// TestNewLoggerNeverReturnsNil is about a panic, not about formatting.
//
// slog.SetDefault dereferences what it is given, so a switch with no default
// arm turns an unexpected log level into a nil logger and a panic before the
// process has logged anything at all -- including the configuration error that
// would have explained it.
func TestNewLoggerNeverReturnsNil(t *testing.T) {
	levels := []config.LogLevel{
		config.LogLevelDebug,
		config.LogLevelInfo,
		config.LogLevelWarn,
		config.LogLevelError,
		// The ones a switch without a default arm falls through: unset, wrong
		// case, and simply not a level.
		"",
		"INFO",
		"silly",
	}
	for _, level := range levels {
		if newLogger(level) == nil {
			t.Errorf("newLogger(%q) returned nil; slog.SetDefault would panic on it", string(level))
		}
	}
}

// TestCheckPoolCapacity pins the refusal that keeps a starved pool from
// presenting as a mysteriously slow deployment.
func TestCheckPoolCapacity(t *testing.T) {
	const jobs = 6

	if err := checkPoolCapacity(jobs+poolHeadroom, jobs); err != nil {
		t.Errorf("a pool with the required headroom was refused: %v", err)
	}
	if err := checkPoolCapacity(jobs+poolHeadroom+10, jobs); err != nil {
		t.Errorf("a generous pool was refused: %v", err)
	}

	// pgx's own default on a 4-vCPU node, which is where this went wrong.
	err := checkPoolCapacity(4, jobs)
	if err == nil {
		t.Fatal("a four-connection pool was accepted for six background jobs")
	}
	// The message has to name the setting and the number to use, because the
	// operator reading it cannot see either from the symptom.
	for _, want := range []string{"maxConns", "at least"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	// One short is still short: the headroom is what serves the requests.
	if err := checkPoolCapacity(jobs+poolHeadroom-1, jobs); err == nil {
		t.Error("a pool one connection short of the requirement was accepted")
	}
}

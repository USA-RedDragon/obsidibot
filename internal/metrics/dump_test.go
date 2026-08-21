package metrics_test

import (
	"os"
	"strconv"
	"testing"

	"github.com/USA-RedDragon/obsidibot/internal/metrics"
)

// TestDumpScrape writes a fully populated scrape to the path in
// DUMP_SCRAPE_TO, and is skipped otherwise.
//
// The tests in this package assert the format against expected text, which
// checks this package against its own idea of correct. This is the hook for
// checking it against someone else's: dump a scrape and feed it to promtool
// ("promtool check metrics < scrape.txt") or to any other parser. Keep it —
// the day this output stops being accepted, that is how you will find out.
func TestDumpScrape(t *testing.T) {
	path := os.Getenv("DUMP_SCRAPE_TO")
	if path == "" {
		t.Skip("set DUMP_SCRAPE_TO to write a scrape for an external validator")
	}

	m := metrics.New()
	m.InteractionsTotal.WithLabelValues("bank", metrics.ResultOK).Inc()
	m.InteractionsTotal.WithLabelValues("link", metrics.ResultUserError).Add(3)
	m.InteractionDuration.WithLabelValues("bank").Observe(0.02)
	m.InteractionDuration.WithLabelValues("bank").Observe(3)
	m.InteractionDuration.WithLabelValues("link").Observe(0.001)
	m.RCONCommandsTotal.WithLabelValues("whisper", metrics.ResultError).Inc()
	m.RCONDuration.Observe(0.4)
	m.KillEventsIngestedTotal.WithLabelValues(metrics.ResultDuplicate).Inc()
	m.KillFeedBacklog.Set(12)
	m.RatingBacklog.Set(0)
	m.RatingUpdatesTotal.WithLabelValues("true").Inc()
	m.BankOperationsTotal.WithLabelValues("withdraw", metrics.ResultNeedsReview).Inc()
	m.BankNeedsReview.Set(1)
	m.LeaderboardLastSuccess.Set(1700000000)
	m.LeaderTransitionsTotal.WithLabelValues("killfeed").Inc()
	m.DBErrorsTotal.Inc()
	// A nasty label value, to prove escaping survives a real parser.
	m.LeaderTransitionsTotal.WithLabelValues("odd\\job \"one\"\ntwo").Inc()

	// And a family pushed past its cardinality cap, so the overflow series is
	// in the dump too: it has to be something a parser accepts, not just
	// something this package is willing to print.
	for i := range 5000 {
		m.RCONCommandsTotal.WithLabelValues("whisper", "player-"+strconv.Itoa(i)).Inc()
	}

	if err := os.WriteFile(path, m.Registry.Gather(), 0o600); err != nil {
		t.Fatalf("write dump: %v", err)
	}
}

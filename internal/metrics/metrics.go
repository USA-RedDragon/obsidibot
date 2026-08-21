// Package metrics defines every Prometheus collector obsidibot exports.
//
// All collectors hang off a private [prometheus.Registry] owned by the
// [Metrics] value — never the package-global default registry — so tests can
// build as many independent instances as they like and nothing registers
// behind the caller's back.
//
// Label-cardinality rule: labels must come from small, closed sets — command
// name, direction, result. NEVER label a metric with a Discord user id, an
// Alderon ID, a player name, or a channel id. This bot's whole subject matter
// is individual players, so the temptation is constant and the consequence is
// permanent: every distinct label value mints a new time series that lives for
// the rest of the process, and a busy evening of player churn is unbounded.
package metrics

import (
	"context"
	"net/http"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/server"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Results for the "result" label. They are a closed set on purpose.
const (
	// ResultOK is a request or operation that completed as intended.
	ResultOK = "ok"
	// ResultUserError is a refusal the caller caused and can fix: not linked,
	// not in game, too soon, not enough marks.
	ResultUserError = "user_error"
	// ResultError is a failure the caller cannot fix: RCON down, database
	// unreachable, Discord rejecting a write.
	ResultError = "error"
	// ResultDuplicate is an inbound event already recorded.
	ResultDuplicate = "duplicate"
	// ResultRejected is an inbound request that failed authentication or did
	// not belong to the configured server.
	ResultRejected = "rejected"
	// ResultNeedsReview is a banking operation whose outcome could not be
	// confirmed and was parked for a human.
	ResultNeedsReview = "needs_review"
)

// shutdownGrace bounds how long an in-flight metrics scrape may take to finish
// once shutdown has started.
const shutdownGrace = 5 * time.Second

// Metrics holds obsidibot's collectors, all registered on Registry.
type Metrics struct {
	// Registry is the private registry every field below is registered on,
	// alongside the standard Go runtime and process collectors.
	Registry *prometheus.Registry

	// InteractionsTotal counts handled Discord interactions by command and
	// result. "command" is the slash command name, a closed set this binary
	// defines; it is never the caller.
	InteractionsTotal *prometheus.CounterVec
	// InteractionDuration observes how long a command took to produce its
	// final response, deferred work included.
	InteractionDuration *prometheus.HistogramVec

	// RCONCommandsTotal counts RCON commands by verb and result. The verb is
	// the command word only — never its arguments, which carry player names.
	RCONCommandsTotal *prometheus.CounterVec
	// RCONDuration observes one RCON exchange in seconds.
	RCONDuration prometheus.Histogram

	// KillEventsIngestedTotal counts inbound webhook deliveries by result.
	KillEventsIngestedTotal *prometheus.CounterVec
	// KillFeedBacklog gauges kill events accepted but not yet posted. The feed
	// is lossless and drains at Discord's rate limit, so this is the signal
	// that it has fallen behind. Alert on sustained growth.
	KillFeedBacklog prometheus.Gauge
	// RatingBacklog gauges kill events accepted but not yet rated.
	RatingBacklog prometheus.Gauge
	// RatingUpdatesTotal counts kill events the rating applier has processed,
	// by whether the kill was credited.
	RatingUpdatesTotal *prometheus.CounterVec

	// BankOperationsTotal counts banking operations by direction and result.
	BankOperationsTotal *prometheus.CounterVec
	// BankNeedsReview gauges ledger rows parked for a human because an RCON
	// transfer could not be confirmed. ALERT ON NONZERO: this is the only path
	// by which marks can be wrong, and nothing else will surface it.
	BankNeedsReview prometheus.Gauge

	// LeaderboardLastSuccess is the unix time of the last leaderboard refresh
	// that landed. Alert on staleness.
	LeaderboardLastSuccess prometheus.Gauge

	// LeaderTransitionsTotal counts background jobs acquiring their advisory
	// lock, by job. A job flapping here means replicas are fighting over it.
	LeaderTransitionsTotal *prometheus.CounterVec

	// DBErrorsTotal counts database errors observed anywhere in the process.
	DBErrorsTotal prometheus.Counter
}

// New builds a Metrics whose collectors are registered on a fresh private
// registry together with the Go runtime and process collectors.
func New() *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		InteractionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "obsidibot_interactions_total",
			Help: "Handled Discord interactions by command and result.",
		}, []string{"command", "result"}),
		InteractionDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "obsidibot_interaction_duration_seconds",
			Help:    "Time to produce a command's final response, deferred work included.",
			Buckets: prometheus.ExponentialBuckets(0.005, 2, 12), // 5ms .. ~10s
		}, []string{"command"}),
		RCONCommandsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "obsidibot_rcon_commands_total",
			Help: "RCON commands by verb and result. The verb never carries its arguments.",
		}, []string{"command", "result"}),
		RCONDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "obsidibot_rcon_duration_seconds",
			Help:    "Latency of one RCON exchange in seconds.",
			Buckets: prometheus.ExponentialBuckets(0.005, 2, 12),
		}),
		KillEventsIngestedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "obsidibot_kill_events_ingested_total",
			Help: "Inbound PlayerKilled webhook deliveries by result: ok, duplicate, rejected, error.",
		}, []string{"result"}),
		KillFeedBacklog: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "obsidibot_kill_feed_backlog",
			Help: "Kill events accepted but not yet posted to Discord. The feed is lossless, so this grows rather than dropping.",
		}),
		RatingBacklog: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "obsidibot_rating_backlog",
			Help: "Kill events accepted but not yet applied to ratings.",
		}),
		RatingUpdatesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "obsidibot_rating_updates_total",
			Help: "Kill events processed by the rating applier, by whether the kill was credited.",
		}, []string{"credited"}),
		BankOperationsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "obsidibot_bank_operations_total",
			Help: "Banking operations by direction and result.",
		}, []string{"direction", "result"}),
		BankNeedsReview: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "obsidibot_bank_needs_review",
			Help: "Ledger rows parked for review because a transfer could not be confirmed. Alert on nonzero.",
		}),
		LeaderboardLastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "obsidibot_leaderboard_last_success_timestamp_seconds",
			Help: "Unix time of the last leaderboard refresh that landed. Alert on staleness.",
		}),
		LeaderTransitionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "obsidibot_leader_transitions_total",
			Help: "Times a background job acquired its advisory lock, by job.",
		}, []string{"job"}),
		DBErrorsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "obsidibot_db_errors_total",
			Help: "Database errors observed by obsidibot.",
		}),
	}

	m.Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.InteractionsTotal,
		m.InteractionDuration,
		m.RCONCommandsTotal,
		m.RCONDuration,
		m.KillEventsIngestedTotal,
		m.KillFeedBacklog,
		m.RatingBacklog,
		m.RatingUpdatesTotal,
		m.BankOperationsTotal,
		m.BankNeedsReview,
		m.LeaderboardLastSuccess,
		m.LeaderTransitionsTotal,
		m.DBErrorsTotal,
	)
	return m
}

// Serve runs the metrics endpoint until ctx is cancelled.
//
// It serves ONLY /metrics. The health endpoints live on the interactions
// listener instead, for two reasons: this listener can be switched off, and
// probes that vanish with a setting named "metrics.enabled" are a trap; and
// liveness that matters is the liveness of the port actually serving Discord,
// not of some other socket in the same process.
func (m *Metrics) Serve(ctx context.Context, bind string, port int) error {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: server.DefaultReadHeaderTimeout,
	}
	return server.Serve(ctx, "metrics", srv, bind, port, shutdownGrace)
}

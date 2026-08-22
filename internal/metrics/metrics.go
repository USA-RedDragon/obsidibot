// Package metrics defines every Prometheus collector obsidibot exports.
//
// All collectors hang off a private [Registry] owned by the [Metrics] value —
// never a package-global default registry — so tests can build as many
// independent instances as they like and nothing registers behind the caller's
// back.
//
// Label-cardinality rule: labels must come from small, closed sets — command
// name, direction, result. NEVER label a metric with a Discord user id, an
// Alderon ID, a player name, or a channel id. This bot's whole subject matter
// is individual players, so the temptation is constant and the consequence is
// permanent: every distinct label value mints a new time series that lives for
// the rest of the process, and a busy evening of player churn is unbounded.
// The rule is now backstopped in code — see maxSeriesPerFamily — but the
// backstop only bounds the damage; it does not make a player-labelled metric
// useful.
//
// The collectors, the registry and the text exposition format are implemented
// here rather than taken from prometheus/client_golang. That library is
// excellent and this is not a criticism of it: it pulls in protobuf, whose use
// of reflect.Value.MethodByName defeats the linker's dead-method elimination
// across the entire binary, and it cost this one 5.3 MB — 28% of the shipped
// artifact — for a handful of series and one text endpoint. What is here is
// the subset obsidibot uses.
package metrics

import (
	"context"
	"net/http"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/server"
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

// latencyBuckets returns the bucket layout both latency histograms use: 5ms
// doubling twelve times, so roughly 5ms to 10s.
//
// A function rather than a package variable so no caller can mutate the slice
// out from under a live histogram.
func latencyBuckets() []float64 { return ExponentialBuckets(0.005, 2, 12) }

// Metrics holds obsidibot's collectors, all registered on Registry.
type Metrics struct {
	// Registry is the private registry every field below is registered on,
	// alongside a small set of Go runtime series.
	Registry *Registry

	// InteractionsTotal counts handled Discord interactions by command and
	// result. "command" is the slash command name, a closed set this binary
	// defines; it is never the caller.
	InteractionsTotal *CounterVec
	// InteractionDuration observes how long a command took to produce its
	// final response, deferred work included.
	InteractionDuration *HistogramVec

	// RCONCommandsTotal counts RCON commands by verb and result. The verb is
	// the command word only — never its arguments, which carry player names.
	RCONCommandsTotal *CounterVec
	// RCONDuration observes one RCON exchange in seconds.
	RCONDuration *Histogram

	// KillEventsIngestedTotal counts inbound webhook deliveries by result.
	KillEventsIngestedTotal *CounterVec
	// KillFeedBacklog gauges kill events accepted but not yet posted. The feed
	// is lossless and drains at Discord's rate limit, so this is the signal
	// that it has fallen behind. Alert on sustained growth.
	KillFeedBacklog *Gauge
	// RatingBacklog gauges kill events accepted but not yet rated.
	RatingBacklog *Gauge
	// RatingUpdatesTotal counts kill events the rating applier has processed,
	// by whether the kill was credited.
	RatingUpdatesTotal *CounterVec

	// BankOperationsTotal counts banking operations by direction and result.
	BankOperationsTotal *CounterVec
	// BankNeedsReview gauges ledger rows parked for a human because an RCON
	// transfer could not be confirmed. ALERT ON NONZERO: this is the only path
	// by which marks can be wrong, and nothing else will surface it.
	BankNeedsReview *Gauge

	// LeaderboardLastSuccess is the unix time of the last leaderboard refresh
	// that landed. Alert on staleness.
	LeaderboardLastSuccess *Gauge

	// LeaderTransitionsTotal counts background jobs acquiring their advisory
	// lock, by job. A job flapping here means replicas are fighting over it.
	LeaderTransitionsTotal *CounterVec

	// DBErrorsTotal counts database errors observed anywhere in the process.
	DBErrorsTotal *Counter

	// GameCommandsTotal counts in-game ! commands by command and result. The
	// command label comes from the dispatcher's closed set, never raw chat.
	GameCommandsTotal *CounterVec

	// ModerationActionsTotal counts moderation actions by kind and result.
	ModerationActionsTotal *CounterVec
	// ModerationUnenforcedBans gauges active bans the game is not yet
	// enforcing (target unlinked, RCON failing, scheduler behind). ALERT ON
	// SUSTAINED NONZERO: a banned player who can still join is invisible
	// otherwise.
	ModerationUnenforcedBans *Gauge
}

// New builds a Metrics whose collectors are registered on a fresh private
// registry together with a small set of Go runtime series.
func New() *Metrics {
	r := NewRegistry()
	r.register(newGoRuntime())
	r.register(newProcSelf())

	return &Metrics{
		Registry: r,
		InteractionsTotal: r.NewCounterVec(
			"obsidibot_interactions_total",
			"Handled Discord interactions by command and result.",
			[]string{"command", "result"}),
		InteractionDuration: r.NewHistogramVec(
			"obsidibot_interaction_duration_seconds",
			"Time to produce a command's final response, deferred work included.",
			[]string{"command"}, latencyBuckets()),
		RCONCommandsTotal: r.NewCounterVec(
			"obsidibot_rcon_commands_total",
			"RCON commands by verb and result. The verb never carries its arguments.",
			[]string{"command", "result"}),
		RCONDuration: r.NewHistogram(
			"obsidibot_rcon_duration_seconds",
			"Latency of one RCON exchange in seconds.",
			latencyBuckets()),
		KillEventsIngestedTotal: r.NewCounterVec(
			"obsidibot_kill_events_ingested_total",
			"Inbound PlayerKilled webhook deliveries by result: ok, duplicate, rejected, error.",
			[]string{"result"}),
		KillFeedBacklog: r.NewGauge(
			"obsidibot_kill_feed_backlog",
			"Kill events accepted but not yet posted to Discord. The feed is lossless, so this grows rather than dropping."),
		RatingBacklog: r.NewGauge(
			"obsidibot_rating_backlog",
			"Kill events accepted but not yet applied to ratings."),
		RatingUpdatesTotal: r.NewCounterVec(
			"obsidibot_rating_updates_total",
			"Kill events processed by the rating applier, by whether the kill was credited.",
			[]string{"credited"}),
		BankOperationsTotal: r.NewCounterVec(
			"obsidibot_bank_operations_total",
			"Banking operations by direction and result.",
			[]string{"direction", "result"}),
		BankNeedsReview: r.NewGauge(
			"obsidibot_bank_needs_review",
			"Ledger rows parked for review because a transfer could not be confirmed. Alert on nonzero."),
		LeaderboardLastSuccess: r.NewGauge(
			"obsidibot_leaderboard_last_success_timestamp_seconds",
			"Unix time of the last leaderboard refresh that landed. Alert on staleness."),
		LeaderTransitionsTotal: r.NewCounterVec(
			"obsidibot_leader_transitions_total",
			"Times a background job acquired its advisory lock, by job.",
			[]string{"job"}),
		DBErrorsTotal: r.NewCounter(
			"obsidibot_db_errors_total",
			"Database errors observed by obsidibot."),
		GameCommandsTotal: r.NewCounterVec(
			"obsidibot_game_commands_total",
			"In-game commands dispatched from the PlayerCommand webhook, by command and result.",
			[]string{"command", "result"}),
		ModerationActionsTotal: r.NewCounterVec(
			"obsidibot_moderation_actions_total",
			"Moderation actions by kind (warn, ban, unban, expire) and result.",
			[]string{"kind", "result"}),
		ModerationUnenforcedBans: r.NewGauge(
			"obsidibot_moderation_unenforced_bans",
			"Active bans not yet enforced in game. Alert on sustained nonzero."),
	}
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
	mux.Handle("GET /metrics", m.Registry)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: server.DefaultReadHeaderTimeout,
	}
	return server.Serve(ctx, "metrics", srv, bind, port, shutdownGrace)
}

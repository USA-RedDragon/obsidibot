package ingest

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/config"
	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/gamecmd"
	"github.com/USA-RedDragon/obsidibot/internal/metrics"
	"github.com/USA-RedDragon/obsidibot/internal/pot"
	"github.com/USA-RedDragon/obsidibot/internal/server"
	"github.com/jackc/pgx/v5"
)

// maxRequestBody caps a webhook delivery. The real ones are around a kilobyte.
const maxRequestBody = 64 << 10

// shutdownGrace lets an in-flight delivery finish. It is short because both
// handlers answer quickly: the kill route only inserts a row, and the command
// route only dispatches. The command WORKERS outlive their requests and get a
// separate, longer wait in Serve.
const shutdownGrace = 5 * time.Second

// workerWaitSlack tops up the dispatcher's own budget when Serve waits for its
// workers, covering the gap between a worker's deadline firing and it actually
// unwinding.
const workerWaitSlack = 10 * time.Second

// Commands runs in-game commands. The concrete implementation is
// *gamecmd.Dispatcher; the interface is here so the route can be tested with a
// recorder instead of a database, RCON and a bank.
type Commands interface {
	// Dispatch ACKs by returning immediately; the work runs on a tracked
	// goroutine of the dispatcher's own.
	Dispatch(ctx context.Context, in gamecmd.Incoming)
	// Wait blocks until every dispatched command has finished, or ctx expires.
	Wait(ctx context.Context) error
	// Budget is how long one dispatched command may run, from which Serve
	// derives how long to wait for the workers at shutdown.
	Budget() time.Duration
}

// Server receives kill and command webhooks.
type Server struct {
	store    *db.Store
	metrics  *metrics.Metrics
	commands Commands
	secret   string
	// serverGUID is checked against every delivery. Without it, a second game
	// server pointed at this URL -- by mistake or otherwise -- would silently
	// merge its kills into this server's ratings. It is discovered from the
	// game at startup rather than configured, so it cannot disagree with the
	// server RCON is pointed at.
	serverGUID string
	initial    float64
}

// New builds the ingest server. serverGUID is the GUID discovered at startup;
// commands is where PlayerCommand deliveries are dispatched.
func New(store *db.Store, m *metrics.Metrics, cfg *config.Config, serverGUID string, commands Commands) *Server {
	return &Server{
		store:      store,
		metrics:    m,
		commands:   commands,
		secret:     cfg.Ingest.Secret,
		serverGUID: serverGUID,
		initial:    float64(cfg.Rating.Initial),
	}
}

// Handler returns the routes this server answers. Serve wraps it in a
// listener; tests exercise it directly, so the route patterns under test are
// the ones that run.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// The secret is a path segment because the game gives us nowhere else to
	// put a credential: no signature, no custom headers, no body field we
	// control. config.Ingest.validate refuses a secret containing /, ?, # or %
	// so it cannot change which route a delivery reaches.
	mux.HandleFunc("POST /webhooks/pot/{secret}/killed", s.handleKilled)
	mux.HandleFunc("POST /webhooks/pot/{secret}/command", s.handleCommand)
	return mux
}

// Serve runs the ingest listener until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, bind string, port int) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: server.DefaultReadHeaderTimeout,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	}
	err := server.Serve(ctx, "ingest", srv, bind, port, shutdownGrace)

	// Shutdown has drained the ACTIVE requests, which does NOT include a
	// dispatched command: its request finished when the 200 was written and
	// its work is on a goroutine no http.Server knows about. Waiting here is
	// what makes this function's return mean "nothing of mine is still
	// running", and the caller relies on that before closing the database.
	//
	// The grace is DERIVED from the dispatcher's budget rather than reusing
	// shutdownGrace: a bank transfer can legitimately run for minutes at the
	// maximum RCON timeout, and cutting Serve loose at five seconds would
	// close the pool under the settle and park writes -- the exact hazard the
	// interactions listener's own long grace exists to prevent.
	waitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.commands.Budget()+workerWaitSlack)
	defer cancel()
	if waitErr := s.commands.Wait(waitCtx); waitErr != nil {
		// The serve error, if any, says why we are shutting down at all, so it
		// wins; this one is still worth saying out loud because it means work
		// was abandoned mid-flight and may need reconciling.
		slog.ErrorContext(waitCtx, "in-game commands were still running at shutdown", "error", waitErr)
		if err == nil {
			err = fmt.Errorf("ingest: waiting for in-game commands: %w", waitErr)
		}
	}
	return err
}

func (s *Server) handleKilled(w http.ResponseWriter, r *http.Request) {
	// Constant time, so the secret cannot be recovered a byte at a time by
	// measuring how long the refusal takes.
	if subtle.ConstantTimeCompare([]byte(r.PathValue("secret")), []byte(s.secret)) != 1 {
		s.count(metrics.ResultRejected)
		// No explanation: whoever reaches here with the wrong secret is not
		// somebody to help.
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		s.count(metrics.ResultError)
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "could not read body", http.StatusBadRequest)
		return
	}

	var event PlayerKilled
	if err := json.Unmarshal(body, &event); err != nil {
		s.count(metrics.ResultError)
		slog.WarnContext(r.Context(), "malformed kill webhook", "error", err)
		http.Error(w, "malformed body", http.StatusBadRequest)
		return
	}

	if event.ServerGUID != s.serverGUID {
		s.count(metrics.ResultRejected)
		slog.WarnContext(r.Context(), "kill webhook for a different server was refused",
			"got", event.ServerGUID, "want", s.serverGUID)
		http.Error(w, "unknown server", http.StatusForbidden)
		return
	}

	if event.VictimID() == "" {
		// Every kill has a victim. One without is a format change, and
		// accepting it would put a row with no subject into the rating queue.
		s.count(metrics.ResultError)
		slog.WarnContext(r.Context(), "kill webhook had no victim Alderon ID")
		http.Error(w, "missing victim", http.StatusBadRequest)
		return
	}

	switch err := s.record(r.Context(), body, event); {
	case err == nil:
		s.count(metrics.ResultOK)
	case errors.Is(err, errDuplicate):
		// A redelivery is expected behaviour, not a fault: 200 so the game
		// stops retrying, and a counter so the rate is visible.
		s.count(metrics.ResultDuplicate)
	default:
		s.count(metrics.ResultError)
		s.metrics.DBErrorsTotal.Inc()
		slog.ErrorContext(r.Context(), "could not record kill event", "error", err)
		// 500 so the game may retry: dedupe makes that safe, and losing a kill
		// silently is worse than recording one twice would have been.
		http.Error(w, "could not record event", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// handleCommand takes a PlayerCommand delivery and hands it to the dispatcher.
//
// The checks mirror handleKilled's, for the same reasons, with ONE addition:
// the Alderon ID is validated at the door. Everywhere else in this package an
// identifier is only ever stored; here it goes onto an RCON command line as
// the whisper target, so it is checked before anything downstream can be
// tempted to interpolate it.
//
// The reply is a 200 and nothing else. The work is dispatched, not done: see
// the package comment on why this route is an RPC rather than a queue, and
// internal/gamecmd on why it cannot run inside this request's ten-second
// window.
func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.PathValue("secret")), []byte(s.secret)) != 1 {
		s.refuseDelivery(metrics.ResultRejected)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		s.refuseDelivery(metrics.ResultError)
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "could not read body", http.StatusBadRequest)
		return
	}

	var event PlayerCommand
	if err := json.Unmarshal(body, &event); err != nil {
		s.refuseDelivery(metrics.ResultError)
		slog.WarnContext(r.Context(), "malformed command webhook", "error", err)
		http.Error(w, "malformed body", http.StatusBadRequest)
		return
	}

	if event.ServerGUID != s.serverGUID {
		s.refuseDelivery(metrics.ResultRejected)
		slog.WarnContext(r.Context(), "command webhook for a different server was refused",
			"got", event.ServerGUID, "want", s.serverGUID)
		http.Error(w, "unknown server", http.StatusForbidden)
		return
	}

	// A missing or injection-shaped identifier is refused HERE rather than
	// downstream: it is the whisper target, and the dispatcher would have
	// nowhere to send a refusal anyway.
	if !pot.ValidIdentifier(event.PlayerID()) {
		s.refuseDelivery(metrics.ResultRejected)
		slog.WarnContext(r.Context(), "command webhook had no usable Alderon ID")
		http.Error(w, "missing player", http.StatusBadRequest)
		return
	}

	s.commands.Dispatch(r.Context(), gamecmd.Incoming{
		AGID:       event.PlayerID(),
		PlayerName: event.PlayerName,
		Message:    event.Message,
	})
	w.WriteHeader(http.StatusOK)
}

var errDuplicate = errors.New("ingest: event already recorded")

// record inserts the event. It does NOT rate it: see the package comment.
func (s *Server) record(ctx context.Context, body []byte, event PlayerKilled) error {
	_, err := s.store.Queries().InsertKillEvent(ctx, gen.InsertKillEventParams{
		DedupeKey:  DedupeKey(body),
		ServerGuid: event.ServerGUID,
		Payload:    body,

		VictimAgid:      event.VictimID(),
		VictimName:      event.VictimName,
		VictimDino:      nullable(event.VictimDinosaurType),
		VictimGrowth:    &event.VictimGrowth,
		VictimPoi:       nullable(event.VictimPOI),
		VictimCharacter: nullable(event.VictimCharacterName),
		VictimRole:      nullable(event.VictimRole),
		VictimLocation:  nullable(event.VictimLocation),
		VictimIsAdmin:   event.VictimIsAdmin,

		KillerAgid:      nullable(event.KillerID()),
		KillerName:      nullable(event.KillerName),
		KillerDino:      nullable(event.KillerDinosaurType),
		KillerGrowth:    nullableFloat(event.KillerID(), event.KillerGrowth),
		KillerIsAdmin:   event.KillerIsAdmin,
		KillerCharacter: nullable(event.KillerCharacterName),
		KillerRole:      nullable(event.KillerRole),
		KillerLocation:  nullable(event.KillerLocation),

		// The game reports -1 for "not applicable" when there was no killer,
		// which must not reach a display as "-1 m".
		KillDistance: positive(event.KillDistance),
		TimeOfDay:    nullableInt(event.TimeOfDay),

		DamageType: event.DamageType,
		// Both decided here because both are pure functions of the payload, so
		// the applier and the feed do not each re-derive them. The raw payload
		// is kept, so a rule change can be replayed.
		Credited:    event.Credited(),
		CountsDeath: event.CountsDeath(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// The insert is ON CONFLICT DO NOTHING, so no row back means the
		// dedupe key was already present.
		return errDuplicate
	}
	if err != nil {
		return fmt.Errorf("insert kill event: %w", err)
	}
	return nil
}

func (s *Server) count(result string) {
	if s.metrics != nil {
		s.metrics.KillEventsIngestedTotal.WithLabelValues(result).Inc()
	}
}

// commandDelivery is the "command" label for a PlayerCommand delivery that was
// refused before it ever became a command. It is a member of the same closed
// set as the real command names and cannot collide with one: no chat line
// parses to it, because the parser only ever yields its own listed names or
// "unknown".
const commandDelivery = "delivery"

// refuseDelivery counts a delivery this route turned away.
//
// ONLY refusals are counted here. An accepted delivery is counted by the
// dispatcher under the command the player actually typed, so counting it twice
// would make the total meaningless. What this leaves is precisely the
// diagnostic worth having: a Game.ini pointed at the wrong secret, or at the
// wrong server's bot, otherwise looks exactly like nobody typing anything.
func (s *Server) refuseDelivery(result string) {
	if s.metrics != nil {
		s.metrics.GameCommandsTotal.WithLabelValues(commandDelivery, result).Inc()
	}
}

// nullable turns an empty string into a NULL, so "no killer" is absent rather
// than an empty string masquerading as an identity.
func nullable(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// nullableFloat keeps a killer's growth NULL when there is no killer, rather
// than storing the zero the payload carries for an environmental death.
func nullableFloat(killerID string, v float64) *float64 {
	if killerID == "" || v < 0 {
		return nil
	}
	return &v
}

// positive drops the game's -1 "not applicable" sentinel.
func positive(v float64) *float64 {
	if v < 0 {
		return nil
	}
	return &v
}

// nullableInt drops a zero/negative in-world clock rather than rendering it.
func nullableInt(v int) *int32 {
	if v <= 0 {
		return nil
	}
	out := int32(v) //nolint:gosec // the game's clock is 0..2359
	return &out
}

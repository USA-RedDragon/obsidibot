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
	"github.com/USA-RedDragon/obsidibot/internal/metrics"
	"github.com/USA-RedDragon/obsidibot/internal/server"
	"github.com/jackc/pgx/v5"
)

// maxRequestBody caps a webhook delivery. The real ones are around a kilobyte.
const maxRequestBody = 64 << 10

// shutdownGrace lets an in-flight delivery finish. It is short because the
// handler only inserts a row.
const shutdownGrace = 5 * time.Second

// Server receives kill webhooks.
type Server struct {
	store   *db.Store
	metrics *metrics.Metrics
	secret  string
	// serverGUID is checked against every delivery. Without it, a second game
	// server pointed at this URL -- by mistake or otherwise -- would silently
	// merge its kills into this server's ratings. It is discovered from the
	// game at startup rather than configured, so it cannot disagree with the
	// server RCON is pointed at.
	serverGUID string
	initial    float64
}

// New builds the ingest server. serverGUID is the GUID discovered at startup.
func New(store *db.Store, m *metrics.Metrics, cfg *config.Config, serverGUID string) *Server {
	return &Server{
		store:      store,
		metrics:    m,
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
	return server.Serve(ctx, "ingest", srv, bind, port, shutdownGrace)
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

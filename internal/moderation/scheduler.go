package moderation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/SRS-Hosting/rcon"
	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/metrics"
	"github.com/USA-RedDragon/obsidibot/internal/pot"
)

// The action labels on obsidibot_moderation_actions_total. Exported so the
// slash commands and this scheduler cannot drift into two spellings of the
// same action.
const (
	// ActionWarn is a warning being recorded.
	ActionWarn = "warn"
	// ActionBan is a ban being placed or enforced.
	ActionBan = "ban"
	// ActionUnban is a ban lifted by a person.
	ActionUnban = "unban"
	// ActionExpire is a ban lifted because its time ran out.
	ActionExpire = "expire"
	// ActionRepair is the audit finding the game had LOST a ban and putting it
	// back. It shares the actions counter rather than having an instrument of
	// its own: it is rare by nature, and a series nobody ever graphs is worth
	// less than one label value on a series people already watch.
	ActionRepair = "repair"
)

const (
	// sweepInterval is how often the scheduler wakes. It bounds how late a ban
	// can be enforced and how long an expired one keeps the player out, and it
	// is why MinBanDuration is a minute.
	sweepInterval = time.Minute

	// sweepBatch bounds each pass, so one busy minute cannot turn into a
	// thousand serial RCON round trips.
	sweepBatch = 50

	// auditInterval is how often the ban list is re-asserted against the game.
	auditInterval = time.Hour

	// auditBudget time-boxes the audit pass.
	//
	// It is LOAD-BEARING, not tidiness: every call in it can take the full RCON
	// timeout (configurable up to 60s) when the server is flapping, and this
	// pass runs in the same goroutine as the lift pass. An unbounded serial
	// walk would keep expired bans enforced for hours to finish an audit that
	// has no ordering requirement and can simply resume next hour.
	auditBudget = 30 * time.Second
)

// The lift_reason and unenforceable_reason values this job writes. They are
// read by people in /modstats, so they are sentences rather than codes.
const (
	reasonExpired = "expired"

	// reasonExpiredNoLiftableBan is DISTINCT from a plain expiry on purpose.
	// The game answering "Unknown ban string" at lift time means either the ban
	// was already gone, or the target is now a listed server admin -- whose
	// bans.txt row RCON can neither place nor lift. The record says which
	// happened; probing with a re-Ban to find out would kick an online innocent.
	reasonExpiredNoLiftableBan = "expired; game reported no liftable ban"

	// reasonSuperseded closes a Discord-only ban row whose player turns out to
	// have an active in-game ban already. Leaving it open would hold the
	// unenforced gauge red forever and, worse, spring a surprise re-ban the
	// moment the other ban expires and backfill finally attaches the AGID.
	reasonSuperseded = "superseded by active in-game ban"

	reasonCannotBanAdmin = "the game refuses to ban a server admin " +
		"(remove them from ServerAdmins in Game.ini first)"
	reasonCommandTooLong = "the ban command was too long for the game server; shorten the reason"
)

// Scheduler owns everything about a ban that happens after the moderator has
// walked away: enforcing it in the game, repairing it if the game loses it, and
// lifting it when it expires.
//
// # Fail-closed, in one direction only
//
// A row is marked lifted only AFTER the game said the ban is gone. The other
// order would leave a player banned with no record that they are -- a ban
// nobody can see and nobody can remove. So every uncertain lift stays active
// and is retried, and the only thing that costs is a player waiting longer.
type Scheduler struct {
	store   *db.Store
	rcon    *pot.Client
	feed    *Feed
	metrics *metrics.Metrics
	guildID string

	// lastAudit is deliberately in memory: a restart simply audits up to an
	// hour early, which is harmless, and the alternative is a settings table
	// row that has to be kept honest across replicas for no benefit. Only the
	// Run loop touches it, and only one replica runs this job.
	lastAudit time.Time
}

// NewScheduler builds the ban enforcement and expiry job. guildID is the guild
// discovered at startup, which is where the feed channels are configured.
func NewScheduler(store *db.Store, rconClient *pot.Client, poster Poster,
	m *metrics.Metrics, guildID string,
) *Scheduler {
	return &Scheduler{
		store:   store,
		rcon:    rconClient,
		feed:    NewFeed(store, poster),
		metrics: m,
		guildID: guildID,
	}
}

// Run sweeps until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()

	for {
		if err := s.Sweep(ctx); err != nil {
			//nolint:nilerr // deliberate: cancellation is a clean stop
			if ctx.Err() != nil {
				return nil
			}
			slog.ErrorContext(ctx, "moderation sweep failed", "error", err)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Sweep runs one pass of every job, in order: attach identities that linking
// has since revealed, enforce what the game does not know about yet, lift what
// has expired, re-assert what the game may have forgotten, and publish the
// gauge.
//
// It is exported so tests can drive one deterministic pass instead of waiting
// on a ticker, exactly as bank.Reconciler does. Every pass runs even if an
// earlier one failed -- an enforcement outage must not stop expiries -- so the
// failures are joined rather than returned at the first one.
func (s *Scheduler) Sweep(ctx context.Context) error {
	return errors.Join(
		s.backfill(ctx),
		s.enforce(ctx),
		s.lift(ctx),
		s.audit(ctx),
		s.publishGauge(ctx),
	)
}

// backfill attaches Alderon IDs to bans recorded against a then-unlinked
// Discord account, so a ban placed before the player linked becomes enforceable
// the moment they do.
func (s *Scheduler) backfill(ctx context.Context) error {
	q := s.store.Queries()

	rows, err := q.BackfillBanAgids(ctx)
	if err != nil {
		return fmt.Errorf("backfill ban identities: %w", err)
	}
	if rows > 0 {
		slog.InfoContext(ctx, "attached in-game identities to bans placed before linking", "bans", rows)
	}

	// The rows the backfill's uniqueness guard skipped: the player already has
	// an active AGID ban, so this discord-only row can never be enforced. It is
	// closed explicitly rather than left in limbo; the history still counts it,
	// because every count matches on both identities.
	blocked, err := q.ListBackfillBlocked(ctx)
	if err != nil {
		return fmt.Errorf("list bans blocked from backfill: %w", err)
	}
	for _, id := range blocked {
		reason := reasonSuperseded
		closed, err := q.LiftBan(ctx, gen.LiftBanParams{ID: id, LiftReason: &reason})
		if err != nil {
			slog.ErrorContext(ctx, "could not close a superseded ban record", "banId", id, "error", err)
			continue
		}
		if closed > 0 {
			slog.InfoContext(ctx, "closed a ban record superseded by an active in-game ban", "banId", id)
		}
	}
	return nil
}

// enforce tells the game about bans it does not know yet.
func (s *Scheduler) enforce(ctx context.Context) error {
	// The query excludes rows already flagged unenforceable and rows whose ban
	// has expired while it went unenforced -- enforcing one of those would kick
	// and ban an innocent player seconds before the lift pass below unbanned
	// them.
	bans, err := s.store.Queries().NextUnenforcedBans(ctx, sweepBatch)
	if err != nil {
		return fmt.Errorf("read unenforced bans: %w", err)
	}
	for _, ban := range bans {
		s.enforceOne(ctx, ban)
	}
	return nil
}

func (s *Scheduler) enforceOne(ctx context.Context, ban gen.Ban) {
	if ban.AlderonID == nil {
		// NextUnenforcedBans requires one, so this is unreachable; the check is
		// here because the alternative is a nil dereference inside a job.
		return
	}
	agid := *ban.AlderonID

	// Kick first. Banning an online player was verified to kick them by itself,
	// so this is mechanically redundant -- but the kick is the one delivery
	// verified to SHOW the player the reason, and it costs one cheap call on a
	// rare operation.
	if err := s.rcon.Kick(ctx, agid, KickReason(ban)); err != nil && !errors.Is(err, pot.ErrKickFailed) {
		// ErrKickFailed just means they were offline. Anything else -- a
		// timeout above all -- says nothing about whether the ban can be
		// placed, and Ban works on offline and never-seen identities, so
		// enforcement carries on either way.
		slog.WarnContext(ctx, "could not kick a player while enforcing their ban",
			"banId", ban.ID, "error", err)
	}

	adminReason, userReason := BanReasons(ban)
	switch err := s.rcon.Ban(ctx, agid, adminReason, userReason); {
	case err == nil, errors.Is(err, pot.ErrAlreadyBanned):
		// "Already banned" is the desired state, reached by another route.
		s.markEnforced(ctx, ban, agid)
	case errors.Is(err, pot.ErrCannotBanAdmin):
		s.markUnenforceable(ctx, ban, reasonCannotBanAdmin)
	case errors.Is(err, rcon.ErrCommandTooLong):
		// PERMANENT for this row: the same command is the same length every
		// time, so retrying it every minute is pure noise.
		s.markUnenforceable(ctx, ban, reasonCommandTooLong)
	default:
		// Transient. Logged and left for the next tick; one broken row must not
		// starve the rest of the batch.
		s.count(ActionBan, metrics.ResultError)
		slog.ErrorContext(ctx, "could not enforce a ban in game, will retry", "banId", ban.ID, "error", err)
	}
}

// markEnforced records that the game now holds the ban.
func (s *Scheduler) markEnforced(ctx context.Context, ban gen.Ban, agid string) {
	marked, err := s.store.Queries().MarkBanEnforced(ctx, ban.ID)
	if err != nil {
		slog.ErrorContext(ctx, "could not record a ban as enforced", "banId", ban.ID, "error", err)
		return
	}
	if marked == 0 {
		// A moderator's /unban lifted the row while this was mid-RCON. The game
		// now holds a ban bound to a CLOSED record, which the audit -- which
		// walks active bans -- would never look at again. Undo it: Unban is
		// idempotent, and the moderator's decision is the newer one.
		slog.WarnContext(ctx, "a ban was lifted while it was being enforced; undoing the game ban",
			"banId", ban.ID)
		if err := s.rcon.Unban(ctx, agid); err != nil && !errors.Is(err, pot.ErrNotBanned) {
			slog.ErrorContext(ctx, "could not undo a game ban placed on an already-lifted record",
				"banId", ban.ID, "error", err)
		}
		return
	}
	s.count(ActionBan, metrics.ResultOK)
	slog.InfoContext(ctx, "enforced a ban in game", "banId", ban.ID)
}

// markUnenforceable flags a ban the game refuses in a way retrying cannot fix.
func (s *Scheduler) markUnenforceable(ctx context.Context, ban gen.Ban, reason string) {
	if err := s.store.Queries().MarkBanUnenforceable(ctx, gen.MarkBanUnenforceableParams{
		ID: ban.ID, UnenforceableReason: &reason,
	}); err != nil {
		slog.ErrorContext(ctx, "could not flag a ban as unenforceable", "banId", ban.ID, "error", err)
		return
	}
	s.count(ActionBan, metrics.ResultRejected)
	// Logged once, because the row is now excluded from the enforce pass. A
	// permanent refusal reported every minute is the kill-feed-403 mistake.
	slog.WarnContext(ctx, "a ban cannot be enforced in game and will not be retried",
		"banId", ban.ID, "reason", reason)
}

// lift releases bans whose time has run out.
func (s *Scheduler) lift(ctx context.Context) error {
	bans, err := s.store.Queries().NextExpiredBans(ctx, sweepBatch)
	if err != nil {
		return fmt.Errorf("read expired bans: %w", err)
	}
	for _, ban := range bans {
		s.liftOne(ctx, ban)
	}
	return nil
}

func (s *Scheduler) liftOne(ctx context.Context, ban gen.Ban) {
	reason, note := reasonExpired, ""

	// An unenforced ban has nothing in the game to undo, so it is closed
	// directly. Anything the game was told about has to be untold FIRST.
	if ban.EnforcedAt != nil && ban.AlderonID != nil {
		switch err := s.rcon.Unban(ctx, *ban.AlderonID); {
		case err == nil:
		case errors.Is(err, pot.ErrNotBanned):
			// The game has no ban to lift. Either it is already gone, or the
			// target is now a listed server admin, whose row RCON can neither
			// place nor lift. The distinct reason, this counter and this log
			// line ARE the surfacing of that edge -- remediation is manual
			// bans.txt surgery, documented in the README.
			reason = reasonExpiredNoLiftableBan
			note = "the game reported no ban to lift; if this player is a server admin, " +
				"their bans.txt row has to be removed by hand"
			s.count(ActionExpire, metrics.ResultNeedsReview)
			slog.WarnContext(ctx, "an expiring ban had nothing to lift in game",
				"banId", ban.ID, "reason", reason)
		default:
			// FAIL CLOSED: leave the row active and try again next tick. The
			// player waits a minute longer; the alternative is a record saying
			// they are free while the game still refuses them.
			s.count(ActionExpire, metrics.ResultError)
			slog.ErrorContext(ctx, "could not lift an expired ban in game, will retry",
				"banId", ban.ID, "error", err)
			return
		}
	}

	closed, err := s.store.Queries().LiftBan(ctx, gen.LiftBanParams{ID: ban.ID, LiftReason: &reason})
	if err != nil {
		slog.ErrorContext(ctx, "could not close an expired ban", "banId", ban.ID, "error", err)
		return
	}
	if closed == 0 {
		// A moderator's /unban got there first. Their reason is the better
		// record of why, so nothing is overwritten and nothing is announced.
		return
	}
	s.count(ActionExpire, metrics.ResultOK)
	slog.InfoContext(ctx, "lifted an expired ban", "banId", ban.ID, "reason", reason)

	// Only now, and best-effort: a Discord outage may cost the channel this
	// notice, but it must never cost the player their unban.
	s.feed.Post(ctx, s.guildID, KindBan, ExpiredEmbed(ban, note))
}

// audit re-asserts every active ban against the game, hourly.
//
// The game can LOSE a ban -- bans.txt wiped, restored from a backup, or
// hand-edited -- and nothing else would ever notice. Re-issuing the ban is both
// the question and the repair: the verified "already banned" answer means
// healthy, and an unexpected success means the ban was gone and now is not.
func (s *Scheduler) audit(ctx context.Context) error {
	if time.Since(s.lastAudit) < auditInterval {
		return nil
	}
	// Stamped BEFORE the work: a pass abandoned to a flapping game server
	// resumes next hour rather than being retried on every tick.
	s.lastAudit = time.Now()

	bans, err := s.store.Queries().NextAuditBans(ctx, sweepBatch)
	if err != nil {
		return fmt.Errorf("read bans to audit: %w", err)
	}

	deadline := time.Now().Add(auditBudget)
	for _, ban := range bans {
		if time.Now().After(deadline) {
			slog.InfoContext(ctx, "ban audit ran out of time; resuming next hour", "banId", ban.ID)
			return nil
		}
		if !s.auditOne(ctx, ban) {
			return nil
		}
	}
	return nil
}

// auditOne checks one ban, reporting whether the pass should continue.
func (s *Scheduler) auditOne(ctx context.Context, ban gen.Ban) bool {
	if ban.AlderonID == nil {
		return true
	}
	adminReason, userReason := BanReasons(ban)

	switch err := s.rcon.Ban(ctx, *ban.AlderonID, adminReason, userReason); {
	case errors.Is(err, pot.ErrAlreadyBanned):
		// Healthy: the game still holds it.
		return true

	case err == nil:
		// The game had LOST the ban, and this call just put it back.
		s.count(ActionRepair, metrics.ResultOK)
		slog.WarnContext(ctx, "the game had lost a ban and the audit restored it",
			"banId", ban.ID, "reason", ban.Reason)
		// enforced_at is bumped so the timestamp means "last known to be in
		// place" rather than "first placed"; zero rows means it was lifted
		// meanwhile, so the ban just re-asserted has to come straight back off.
		s.markEnforced(ctx, ban, *ban.AlderonID)
		return true

	case errors.Is(err, pot.ErrCannotBanAdmin):
		// The target has become a server admin since the ban was placed.
		s.markUnenforceable(ctx, ban, reasonCannotBanAdmin)
		return true

	default:
		// Transport-level: the server is unreachable or flapping. Every
		// remaining row would pay the same timeout, so the pass stops here and
		// resumes next hour rather than blocking the lift pass behind it.
		slog.ErrorContext(ctx, "ban audit stopped early; the game server is not answering",
			"banId", ban.ID, "error", err)
		return false
	}
}

// publishGauge re-reads the backlog rather than accumulating it in memory, so
// the alert stays honest across restarts and leadership changes.
func (s *Scheduler) publishGauge(ctx context.Context) error {
	if s.metrics == nil {
		return nil
	}
	count, err := s.store.Queries().CountUnenforcedActiveBans(ctx)
	if err != nil {
		return fmt.Errorf("count unenforced bans: %w", err)
	}
	s.metrics.ModerationUnenforcedBans.Set(float64(count))
	return nil
}

func (s *Scheduler) count(kind, result string) {
	if s.metrics == nil {
		return
	}
	s.metrics.ModerationActionsTotal.WithLabelValues(kind, result).Inc()
}

// BanReasons splits a ban record into the two reasons the game keeps.
//
// The USER reason is what the banned player is shown, so it is the moderator's
// own words. The ADMIN reason is the other half of the bans.txt row and is
// bookkeeping: which record this ban came from, and who issued it. Neither
// carries a colon, because that is the field separator in bans.txt.
func BanReasons(ban gen.Ban) (string, string) {
	return fmt.Sprintf("obsidibot ban #%d by %s", ban.ID, ban.IssuedByDiscordID), ban.Reason
}

// KickReason is what the player sees on screen as they are removed.
func KickReason(ban gen.Ban) string {
	return "banned - " + ban.Reason
}

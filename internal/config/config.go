// Package config holds obsidibot's configuration and the validation that keeps
// a bad deployment from starting.
//
// Field tags are camelCase, matching the RCON block this project inherited from
// rcon-web. Ports and interval-style values are plain ints rather than sized
// types or time.Duration: configulator assigns YAML numbers through reflection
// without a range check, so a narrower field would silently wrap 70000 to 4464
// and -1 to 65535 where an int lets Validate reject both, and it parses integer
// fields with strconv, so a "5s" default would not load.
package config

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// LogLevel selects the verbosity of the structured logger.
type LogLevel string

const (
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

const maxPort = 65535

// MaxTimeoutSeconds bounds rcon.timeoutSeconds. A command must fail fast enough
// that a Discord interaction can still be answered inside its own deadline.
const MaxTimeoutSeconds = 60

// MaxConcurrentLimit bounds rcon.maxConcurrent. Anything near this is already
// far past what a Source server tolerates; the ceiling exists to catch a typo,
// not to describe a workable setting.
const MaxConcurrentLimit = 64

// MinDatabaseMaxConns and MaxDatabaseMaxConns bound database.maxConns.
//
// The floor is not a performance number: a pool this small cannot overlap the
// background jobs' queries with a slash command and a readiness ping, and the
// symptom of that is not an error but every caller waiting in Acquire. The
// ceiling exists to catch a typo, because each replica opens up to this many
// and a deployment of several can take a Postgres server's max_connections
// with it.
const (
	MinDatabaseMaxConns = 4
	MaxDatabaseMaxConns = 500
)

// MinIngestSecretLen bounds ingest.secret. Path of Titans signs nothing, so this
// string is the entire cryptographic defence of the ingest endpoint: it must be
// long enough that guessing it is not a strategy.
const MinIngestSecretLen = 32

// Config is the root configuration.
type Config struct {
	LogLevel     LogLevel     `name:"logLevel" default:"info" description:"log verbosity: debug, info, warn, or error"`
	Interactions Interactions `name:"interactions" description:"public listener Discord posts interactions to"`
	Ingest       Ingest       `name:"ingest" description:"cluster-internal listener the game server posts webhooks to"`
	Metrics      Metrics      `name:"metrics" description:"Prometheus metrics listener"`
	PProf        PProf        `name:"pprof" description:"pprof debug listener"`
	Database     Database     `name:"database" description:"PostgreSQL connection"`
	Discord      Discord      `name:"discord" description:"Discord application credentials"`
	RCON         RCON         `name:"rcon" description:"Source RCON server settings"`
	Rating       Rating       `name:"rating" description:"Elo rating constants"`
	Bank         Bank         `name:"bank" description:"marks banking behaviour"`
	Leaderboard  Leaderboard  `name:"leaderboard" description:"persistent leaderboard message"`
	KillFeed     KillFeed     `name:"killfeed" description:"kill feed behaviour"`
	Link         Link         `name:"link" description:"/link challenge behaviour"`
}

// Interactions configures the public listener Discord delivers interactions to.
type Interactions struct {
	Bind string `name:"bind" default:"" description:"address to listen on; empty listens on all interfaces over both IPv4 and IPv6"`
	Port int    `name:"port" default:"8080" description:"port the Discord interactions endpoint listens on"`
}

func (i Interactions) validate() []error {
	var errs []error
	if !validPort(i.Port) {
		errs = append(errs, fmt.Errorf("interactions.port %d must be between 1 and %d", i.Port, maxPort))
	}
	return errs
}

// Ingest configures the listener the game server posts webhooks to.
//
// It is a SEPARATE LISTENER from interactions rather than another path on the
// same server, and that is the whole point: Path of Titans signs nothing, so a
// forged kill event is stopped by the secret and by reachability. Splitting the
// ports lets an ingress publish the interactions port alone, so an attacker has
// to already be inside the cluster before the secret is even the question.
type Ingest struct {
	Bind string `name:"bind" default:"" description:"address to listen on; empty listens on all interfaces over both IPv4 and IPv6"`
	Port int    `name:"port" default:"8081" description:"port the game webhook endpoint listens on; must NOT be published to the internet"`
	// Secret is carried in the URL path because the game offers no other place
	// to put a credential: it sends no signature and no configurable headers.
	Secret string `name:"secret" description:"shared secret embedded in the webhook path (required); generate with openssl rand -hex 32"`
}

func (i Ingest) validate() []error {
	var errs []error
	if !validPort(i.Port) {
		errs = append(errs, fmt.Errorf("ingest.port %d must be between 1 and %d", i.Port, maxPort))
	}
	switch {
	case i.Secret == "":
		errs = append(errs, errors.New("ingest.secret must not be empty"))
	case len(i.Secret) < MinIngestSecretLen:
		errs = append(errs, fmt.Errorf("ingest.secret is %d bytes; must be at least %d", len(i.Secret), MinIngestSecretLen))
	case strings.ContainsAny(i.Secret, "/?#%"):
		// The secret becomes a path segment. A separator or an escape in it
		// would route somewhere other than where the operator believes.
		errs = append(errs, errors.New("ingest.secret must not contain /, ?, # or %; it is used as a URL path segment"))
	}
	return errs
}

// Metrics configures the Prometheus metrics listener.
type Metrics struct {
	Enabled bool `name:"enabled" default:"true" description:"serve Prometheus metrics and health endpoints"`
	Port    int  `name:"port" default:"9090" description:"TCP port for the metrics listener"`
}

func (m Metrics) validate() []error {
	var errs []error
	if m.Enabled && !validPort(m.Port) {
		errs = append(errs, fmt.Errorf("metrics.port %d must be between 1 and %d", m.Port, maxPort))
	}
	return errs
}

// PProf configures the pprof debug listener.
type PProf struct {
	Enabled bool `name:"enabled" default:"false" description:"serve pprof profiling endpoints"`
	Port    int  `name:"port" default:"6060" description:"TCP port for the pprof listener"`
}

func (p PProf) validate() []error {
	var errs []error
	if p.Enabled && !validPort(p.Port) {
		errs = append(errs, fmt.Errorf("pprof.port %d must be between 1 and %d", p.Port, maxPort))
	}
	return errs
}

// Database configures the PostgreSQL connection. obsidibot owns this database
// outright, so unlike the services that read a shared control plane it applies
// its own DDL.
type Database struct {
	URL            string `name:"url" description:"connection URL, e.g. postgres://user:pass@host:5432/obsidibot; psql:// and postgresql:// are accepted too"`
	MigrateOnStart bool   `name:"migrateOnStart" default:"true" description:"apply pending schema migrations on startup"`
	// MaxConns is set explicitly because pgx's own default is max(4, NumCPU):
	// the same image would then run a comfortable pool on a large node and a
	// four-connection pool on a small one, and a pool that is too small does
	// not fail, it makes every caller wait in Acquire until its deadline. The
	// default here leaves room for the background jobs' queries and the
	// interactions, ingest and readiness traffic at the same time.
	MaxConns int `name:"maxConns" default:"16" description:"maximum PostgreSQL connections this replica's pool may open; must leave room for the background jobs and request traffic at once"`
}

// DSN is the connection string to hand pgx. pgx accepts postgres:// and
// postgresql:// natively, so both pass through untouched; psql:// is a common
// human habit pgx rejects, and is normalised rather than refused. Anything
// else passes through for pgx to judge.
func (d Database) DSN() string {
	if rest, ok := strings.CutPrefix(d.URL, "psql://"); ok {
		return "postgres://" + rest
	}
	return d.URL
}

func (d Database) validate() []error {
	var errs []error
	if d.URL == "" {
		errs = append(errs, errors.New("database.url must not be empty"))
	}
	// The ceiling is a typo-catcher rather than a workable setting: every
	// replica opens up to this many, so a four-digit value here is a way to
	// exhaust the server's max_connections from one deployment.
	if d.MaxConns < MinDatabaseMaxConns || d.MaxConns > MaxDatabaseMaxConns {
		errs = append(errs, fmt.Errorf("database.maxConns %d must be between %d and %d",
			d.MaxConns, MinDatabaseMaxConns, MaxDatabaseMaxConns))
	}
	return errs
}

// Discord configures the application this bot runs as.
//
// There is no gateway and no sharding: Discord posts interactions to
// Interactions.Addr over HTTPS, which is what lets every replica be identical
// and stateless. PublicKey is what makes that safe, and it is REQUIRED — an
// unverified interactions endpoint accepts commands from anyone who finds it.
type Discord struct {
	Token         string `name:"token" description:"bot token (required), used for the REST calls that post the feed and the board"`
	ApplicationID string `name:"applicationId" description:"Discord application ID (required), used to register commands and edit deferred replies"`
	PublicKey     string `name:"publicKey" description:"Ed25519 public key of the application as hex (required); every interaction is verified against it"`
}

// ed25519PublicKeyHexLen is an Ed25519 public key, 32 bytes, hex encoded.
const ed25519PublicKeyHexLen = 64

func (d Discord) validate() []error {
	var errs []error
	if d.Token == "" {
		errs = append(errs, errors.New("discord.token must not be empty"))
	}
	if d.ApplicationID == "" {
		errs = append(errs, errors.New("discord.applicationId must not be empty"))
	}
	switch {
	case d.PublicKey == "":
		errs = append(errs, errors.New("discord.publicKey must not be empty"))
	case len(d.PublicKey) != ed25519PublicKeyHexLen:
		errs = append(errs, fmt.Errorf("discord.publicKey is %d characters; an Ed25519 public key is %d hex characters",
			len(d.PublicKey), ed25519PublicKeyHexLen))
	case !isHex(d.PublicKey):
		errs = append(errs, errors.New("discord.publicKey must be hex"))
	}
	return errs
}

// RCON configures the upstream Source RCON server. This is the only lever the
// bot has on the game: it reads marks and player presence, moves marks, and
// delivers link codes in-game.
type RCON struct {
	Host           string `name:"host" default:"127.0.0.1" description:"hostname or IP of the Source RCON server"`
	Port           int    `name:"port" default:"7779" description:"TCP port of the Source RCON server"`
	Password       string `name:"password" description:"RCON password (required)"`
	TimeoutSeconds int    `name:"timeoutSeconds" default:"10" description:"deadline in seconds covering a whole RCON exchange: connect, authenticate, command, response"`
	// Path of Titans handles RCON on its game thread and caps or bans clients
	// that pile on connections, so the useful value here is small.
	MaxConcurrent int `name:"maxConcurrent" default:"4" description:"maximum RCON commands in flight at once; further callers fail fast rather than queue"`
}

// Addr returns the RCON server address in host:port form.
func (r RCON) Addr() string {
	return net.JoinHostPort(r.Host, strconv.Itoa(r.Port))
}

// Timeout returns the per-command deadline.
func (r RCON) Timeout() time.Duration {
	return time.Duration(r.TimeoutSeconds) * time.Second
}

func (r RCON) validate() []error {
	var errs []error
	if r.Host == "" {
		errs = append(errs, errors.New("rcon.host must not be empty"))
	}
	if !validPort(r.Port) {
		errs = append(errs, fmt.Errorf("rcon.port %d must be between 1 and %d", r.Port, maxPort))
	}
	if r.Password == "" {
		errs = append(errs, errors.New("rcon.password must not be empty"))
	}
	if r.TimeoutSeconds < 1 || r.TimeoutSeconds > MaxTimeoutSeconds {
		errs = append(errs, fmt.Errorf("rcon.timeoutSeconds %d must be between 1 and %d", r.TimeoutSeconds, MaxTimeoutSeconds))
	}
	if r.MaxConcurrent < 1 || r.MaxConcurrent > MaxConcurrentLimit {
		errs = append(errs, fmt.Errorf("rcon.maxConcurrent %d must be between 1 and %d", r.MaxConcurrent, MaxConcurrentLimit))
	}
	return errs
}

// Rating holds the Elo constants.
//
// K decays with experience so a new player finds their real level in a handful
// of fights while an established rating stays stable. Decay pulls an idle
// rating back toward Initial, and applies ONLY ABOVE it: decaying a
// below-average rating upward would reward not playing.
type Rating struct {
	Initial          int `name:"initial" default:"1200" description:"rating every player starts at"`
	ProvisionalK     int `name:"provisionalK" default:"40" description:"K factor while a player has fewer than provisionalGames rated kills"`
	SettlingK        int `name:"settlingK" default:"20" description:"K factor between provisionalGames and settlingGames"`
	StableK          int `name:"stableK" default:"16" description:"K factor once a player passes settlingGames"`
	ProvisionalGames int `name:"provisionalGames" default:"20" description:"rated games before K drops from provisionalK to settlingK"`
	SettlingGames    int `name:"settlingGames" default:"50" description:"rated games before K drops from settlingK to stableK"`
	DecayGraceDays   int `name:"decayGraceDays" default:"30" description:"days a player may be idle before decay begins"`
	// Expressed as a permille of the gap to Initial so an integer setting can
	// still express a slow drift; 5 is half a percent of the gap per day.
	DecayPermillePerDay int `name:"decayPermillePerDay" default:"5" description:"thousandths of the gap to initial that an idle rating decays per day past the grace period"`
}

func (r Rating) validate() []error {
	var errs []error
	if r.Initial < 1 || r.Initial > 10000 {
		errs = append(errs, fmt.Errorf("rating.initial %d must be between 1 and 10000", r.Initial))
	}
	for _, k := range []struct {
		name  string
		value int
	}{
		{"provisionalK", r.ProvisionalK},
		{"settlingK", r.SettlingK},
		{"stableK", r.StableK},
	} {
		if k.value < 1 || k.value > 100 {
			errs = append(errs, fmt.Errorf("rating.%s %d must be between 1 and 100", k.name, k.value))
		}
	}
	// The three K factors describe a player settling down, so they have to
	// actually descend; a settlingK above provisionalK would make an
	// established rating swing harder than a brand new one.
	if r.ProvisionalK < r.SettlingK || r.SettlingK < r.StableK {
		errs = append(errs, fmt.Errorf("rating K factors must descend: provisionalK %d >= settlingK %d >= stableK %d",
			r.ProvisionalK, r.SettlingK, r.StableK))
	}
	if r.ProvisionalGames < 1 {
		errs = append(errs, fmt.Errorf("rating.provisionalGames %d must be at least 1", r.ProvisionalGames))
	}
	if r.SettlingGames <= r.ProvisionalGames {
		errs = append(errs, fmt.Errorf("rating.settlingGames %d must be greater than provisionalGames %d",
			r.SettlingGames, r.ProvisionalGames))
	}
	if r.DecayGraceDays < 1 {
		errs = append(errs, fmt.Errorf("rating.decayGraceDays %d must be at least 1", r.DecayGraceDays))
	}
	if r.DecayPermillePerDay < 0 || r.DecayPermillePerDay > 1000 {
		errs = append(errs, fmt.Errorf("rating.decayPermillePerDay %d must be between 0 and 1000", r.DecayPermillePerDay))
	}
	return errs
}

// Bank configures marks banking.
type Bank struct {
	CooldownSeconds int `name:"cooldownSeconds" default:"10" description:"seconds a player must wait between banking operations"`
	// VerifyAttempts governs how hard the reconciler OBSERVES an unverified
	// transfer before parking it for review. It never causes a command to be
	// re-sent. The wait between attempts is the reconciler's own tick, not a
	// separate setting -- an earlier verifyBackoffSeconds knob was wired to
	// nothing and only made the budget look like attempts x backoff.
	VerifyAttempts int `name:"verifyAttempts" default:"5" description:"times to re-read a player's marks trying to confirm an unverified transfer before parking it for review"`
}

// Cooldown returns the wait between banking operations.
func (b Bank) Cooldown() time.Duration {
	return time.Duration(b.CooldownSeconds) * time.Second
}

func (b Bank) validate() []error {
	var errs []error
	if b.CooldownSeconds < 0 || b.CooldownSeconds > 3600 {
		errs = append(errs, fmt.Errorf("bank.cooldownSeconds %d must be between 0 and 3600", b.CooldownSeconds))
	}
	if b.VerifyAttempts < 1 || b.VerifyAttempts > 100 {
		errs = append(errs, fmt.Errorf("bank.verifyAttempts %d must be between 1 and 100", b.VerifyAttempts))
	}
	return errs
}

// Leaderboard configures the persistent top-N message.
type Leaderboard struct {
	IntervalSeconds int `name:"intervalSeconds" default:"60" description:"seconds between leaderboard message refreshes"`
	Size            int `name:"size" default:"20" description:"players listed on the leaderboard"`
}

// Interval returns the time between leaderboard refreshes.
func (l Leaderboard) Interval() time.Duration {
	return time.Duration(l.IntervalSeconds) * time.Second
}

func (l Leaderboard) validate() []error {
	var errs []error
	if l.IntervalSeconds < 5 || l.IntervalSeconds > 3600 {
		errs = append(errs, fmt.Errorf("leaderboard.intervalSeconds %d must be between 5 and 3600", l.IntervalSeconds))
	}
	// A Discord embed description has a 4096-character ceiling and each row
	// costs on the order of 80; 50 keeps the rendered message well inside it.
	if l.Size < 1 || l.Size > 50 {
		errs = append(errs, fmt.Errorf("leaderboard.size %d must be between 1 and 50", l.Size))
	}
	return errs
}

// KillFeed configures the kill feed.
type KillFeed struct {
	// The kill feed renders EVERY field the game reports, including the point
	// of interest and both parties' coordinates. There is deliberately no flag:
	// the feed exists to describe a fight that already happened, and a partial
	// account of it was worth less than the privacy it bought. Live position --
	// where somebody IS right now, via RCON PlayerInfo -- is a different
	// question and is still never published; see internal/pot's package doc.
	// Kill events are kept only until they have been rated and posted; the
	// aggregates live on the player row and are unaffected by pruning.
	RetentionDays int `name:"retentionDays" default:"30" description:"days to keep processed kill events before pruning"`
}

func (k KillFeed) validate() []error {
	var errs []error
	if k.RetentionDays < 1 || k.RetentionDays > 3650 {
		errs = append(errs, fmt.Errorf("killfeed.retentionDays %d must be between 1 and 3650", k.RetentionDays))
	}
	return errs
}

// Link configures the /link challenge.
//
// The code is delivered by Whisper and nothing else. The alternatives -- the
// game also has SystemMessage and DirectMessage -- were tried against the live
// server and Whisper is the one this deployment uses; making it a setting would
// only invite a deployment where the code goes somewhere nobody reads, which is
// indistinguishable from a broken bot.
type Link struct {
	CodeTTLSeconds int `name:"codeTTLSeconds" default:"300" description:"seconds a link code stays valid"`
	MaxAttempts    int `name:"maxAttempts" default:"5" description:"wrong codes accepted before a challenge is burned"`
	// ReissueCooldownSeconds bounds how often one Discord user can cause a
	// message to be sent into the game. Without it, /link start is a button
	// that spams another player's chat.
	ReissueCooldownSeconds int `name:"reissueCooldownSeconds" default:"30" description:"seconds before a user may request another link code"`
}

// CodeTTL returns how long a link code stays valid.
func (l Link) CodeTTL() time.Duration {
	return time.Duration(l.CodeTTLSeconds) * time.Second
}

// ReissueCooldown returns the wait before another code may be requested.
func (l Link) ReissueCooldown() time.Duration {
	return time.Duration(l.ReissueCooldownSeconds) * time.Second
}

func (l Link) validate() []error {
	var errs []error
	if l.CodeTTLSeconds < 30 || l.CodeTTLSeconds > 3600 {
		errs = append(errs, fmt.Errorf("link.codeTTLSeconds %d must be between 30 and 3600", l.CodeTTLSeconds))
	}
	if l.MaxAttempts < 1 || l.MaxAttempts > 100 {
		errs = append(errs, fmt.Errorf("link.maxAttempts %d must be between 1 and 100", l.MaxAttempts))
	}
	if l.ReissueCooldownSeconds < 0 || l.ReissueCooldownSeconds > 3600 {
		errs = append(errs, fmt.Errorf("link.reissueCooldownSeconds %d must be between 0 and 3600", l.ReissueCooldownSeconds))
	}
	return errs
}

// Validate reports every problem with the configuration at once, so a bad
// deployment does not have to be fixed one restart at a time.
func (c Config) Validate() error {
	var errs []error

	switch c.LogLevel {
	case LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError:
	default:
		errs = append(errs, fmt.Errorf("logLevel %q must be one of debug, info, warn, error", c.LogLevel))
	}

	errs = append(errs, c.Interactions.validate()...)
	errs = append(errs, c.Ingest.validate()...)
	errs = append(errs, c.Metrics.validate()...)
	errs = append(errs, c.PProf.validate()...)
	errs = append(errs, c.Database.validate()...)
	errs = append(errs, c.Discord.validate()...)
	errs = append(errs, c.RCON.validate()...)
	errs = append(errs, c.Rating.validate()...)
	errs = append(errs, c.Bank.validate()...)
	errs = append(errs, c.Leaderboard.validate()...)
	errs = append(errs, c.KillFeed.validate()...)
	errs = append(errs, c.Link.validate()...)
	errs = append(errs, c.portCollisions()...)

	if len(errs) > 0 {
		return fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return nil
}

// portCollisions reports listeners configured onto the same TCP port.
//
// Interactions and ingest are always enabled and must never share a port: they
// have deliberately different exposure, and collapsing them onto one listener
// would publish the unsigned webhook endpoint to the internet.
func (c Config) portCollisions() []error {
	listeners := []struct {
		name    string
		port    int
		enabled bool
	}{
		{"interactions", c.Interactions.Port, true},
		{"ingest", c.Ingest.Port, true},
		{"metrics", c.Metrics.Port, c.Metrics.Enabled},
		{"pprof", c.PProf.Port, c.PProf.Enabled},
	}

	var errs []error
	for i := range listeners {
		for j := i + 1; j < len(listeners); j++ {
			a, b := listeners[i], listeners[j]
			if a.enabled && b.enabled && a.port == b.port {
				errs = append(errs, fmt.Errorf("%s.port %d conflicts with %s.port", b.name, b.port, a.name))
			}
		}
	}
	return errs
}

func validPort(port int) bool {
	return port >= 1 && port <= maxPort
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

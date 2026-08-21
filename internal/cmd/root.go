// Package cmd wires configuration loading to the server.
package cmd

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/SRS-Hosting/rcon"
	"github.com/USA-RedDragon/configulator"
	"github.com/USA-RedDragon/obsidibot/internal/bank"
	"github.com/USA-RedDragon/obsidibot/internal/board"
	"github.com/USA-RedDragon/obsidibot/internal/commands"
	"github.com/USA-RedDragon/obsidibot/internal/config"
	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/ingest"
	"github.com/USA-RedDragon/obsidibot/internal/interactions"
	"github.com/USA-RedDragon/obsidibot/internal/kills"
	"github.com/USA-RedDragon/obsidibot/internal/leader"
	"github.com/USA-RedDragon/obsidibot/internal/metrics"
	"github.com/USA-RedDragon/obsidibot/internal/pot"
	obsidipprof "github.com/USA-RedDragon/obsidibot/internal/pprof"
	"github.com/bwmarrin/discordgo"
	"github.com/lmittmann/tint"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

// New returns the root command. migrations carries the embedded schema, passed
// in rather than embedded here because a //go:embed pattern cannot reach above
// its own package directory.
func New(version string, commit string, migrations fs.FS) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "obsidibot",
		Short: "Obsidian Wilds Discord Bot",
		Long: `obsidibot is the Discord bot for the Obsidian Wilds Path of Titans server.

It links Discord accounts to in-game identities, tracks kills into a rating and
a live leaderboard, and banks marks on players' behalf over RCON.

Every replica is identical and stateless: Discord delivers interactions over
HTTP rather than a gateway, and the background jobs that must have a single
writer coordinate through Postgres advisory locks.`,
		Version: fmt.Sprintf("%s (%s)", version, commit),
		Annotations: map[string]string{
			"version": version,
			"commit":  commit,
		},
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd, migrations)
		},
	}

	return cmd
}

func run(cmd *cobra.Command, migrations fs.FS) error {
	ctx := cmd.Context()

	// configulator skips a config file it cannot find, which is right for the
	// default path but wrong for one the operator named explicitly: a typo'd
	// --config would otherwise start silently on defaults, looking like it worked.
	if flag := cmd.Flags().Lookup(configulator.ConfigFileKey); flag != nil && flag.Changed {
		if _, err := os.Stat(flag.Value.String()); err != nil {
			return fmt.Errorf("config file %s: %w", flag.Value.String(), err)
		}
	}

	c, err := configulator.FromContext[config.Config](ctx)
	if err != nil {
		return fmt.Errorf("failed to get config from context")
	}

	cfg, err := c.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	var logger *slog.Logger
	switch cfg.LogLevel {
	case config.LogLevelDebug:
		logger = slog.New(tint.NewTextHandler(os.Stdout, &tint.Options{Level: slog.LevelDebug}))
	case config.LogLevelInfo:
		logger = slog.New(tint.NewTextHandler(os.Stdout, &tint.Options{Level: slog.LevelInfo}))
	case config.LogLevelWarn:
		logger = slog.New(tint.NewTextHandler(os.Stderr, &tint.Options{Level: slog.LevelWarn}))
	case config.LogLevelError:
		logger = slog.New(tint.NewTextHandler(os.Stderr, &tint.Options{Level: slog.LevelError}))
	}
	slog.SetDefault(logger)

	slog.Info("obsidibot", "version", cmd.Annotations["version"], "commit", cmd.Annotations["commit"])

	if err := cfg.Validate(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	return serve(ctx, cfg, migrations)
}

// Option adjusts how Serve builds its dependencies.
type Option func(*options)

type options struct {
	discordClient *http.Client
}

// WithDiscordHTTPClient replaces the HTTP client the Discord REST calls use.
//
// This is a TESTING SEAM, and a deliberate one: discordgo resolves its
// endpoints into package-level variables at init, so they cannot be repointed
// afterwards, and the alternative -- a "discord API base URL" setting -- would
// put a knob in production configuration that exists only for tests.
func WithDiscordHTTPClient(client *http.Client) Option {
	return func(o *options) { o.discordClient = client }
}

// Serve loads a config file and runs every component until ctx is cancelled.
//
// It exists alongside the cobra path so the whole wiring can be exercised in a
// test -- several instances at once, against one database -- without going
// through the process-global flag state cobra owns.
func Serve(ctx context.Context, configPath string, migrations fs.FS, opts ...Option) error {
	c := configulator.New[config.Config]().
		WithEnvironmentVariables(&configulator.EnvironmentVariableOptions{Separator: "_"}).
		WithFile(&configulator.FileOptions{Paths: []string{configPath}})

	cfg, err := c.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	return serve(ctx, cfg, migrations, opts...)
}

// serve starts every long-running component and blocks until one of them
// stops. The group's context is cancelled when the first returns, so a failure
// anywhere takes the whole process down rather than leaving it half-serving:
// a replica that answers interactions but cannot reach Postgres is worse than
// one that is visibly gone.
func serve(ctx context.Context, cfg *config.Config, migrations fs.FS, opts ...Option) error {
	var settings options
	for _, opt := range opts {
		opt(&settings)
	}

	// A private registry rather than the default one, so nothing can register
	// a metric into this endpoint from a package we did not wire up here.
	m := metrics.New()

	store, err := db.Connect(ctx, cfg.Database.DSN())
	if err != nil {
		return err
	}
	defer store.Close()

	// Migrations run before anything serves. Every replica calls this on every
	// start; an advisory lock serialises them and already-applied files are
	// skipped, so a rolling deploy is safe.
	if cfg.Database.MigrateOnStart {
		if err := db.Migrate(ctx, store.Pool(), migrations); err != nil {
			return err
		}
	}

	// The Discord session is REST-only: no gateway is opened, which is what
	// lets every replica be identical and stateless. Verifying the token here
	// means a bad one fails at startup rather than showing up later as every
	// deferred reply failing to deliver.
	session, err := discordgo.New("Bot " + cfg.Discord.Token)
	if err != nil {
		return fmt.Errorf("create discord session: %w", err)
	}
	if settings.discordClient != nil {
		session.Client = settings.discordClient
	}
	me, err := interactions.VerifyToken(ctx, session)
	if err != nil {
		return err
	}
	slog.InfoContext(ctx, "discord token verified", "bot", me.Username, "botUserId", me.ID)

	rconClient := rcon.New(cfg.RCON.Addr(), cfg.RCON.Password,
		rcon.WithTimeout(cfg.RCON.Timeout()),
		rcon.WithMaxConcurrent(cfg.RCON.MaxConcurrent))
	game := pot.NewClient(rconClient, func(verb string, err error, elapsed time.Duration) {
		result := metrics.ResultOK
		if err != nil {
			result = metrics.ResultError
		}
		// The VERB only. Its arguments carry player names and Alderon IDs, and
		// this is a metric label.
		m.RCONCommandsTotal.WithLabelValues(verb, result).Inc()
		m.RCONDuration.Observe(elapsed.Seconds())
	})

	// Neither of these is configurable: both are read from the systems that
	// own them, so they cannot disagree with reality.
	self, err := discoverIdentity(ctx, session, game)
	if err != nil {
		return err
	}

	vault := bank.New(store, game, m, cfg)
	router, err := buildRouter(store, game, vault, session, m, cfg)
	if err != nil {
		return err
	}

	group, ctx := errgroup.WithContext(ctx)

	// Listeners. Interactions and ingest are always on; their deliberately
	// different exposure is the reason they are separate ports.
	// Readiness is the database: without it this replica cannot answer a
	// single command, however healthy the process looks.
	ready := func(ctx context.Context) error {
		if err := store.Ping(ctx); err != nil {
			return fmt.Errorf("database: %w", err)
		}
		return nil
	}

	group.Go(func() error {
		return router.Serve(ctx, cfg.Interactions.Bind, cfg.Interactions.Port, ready)
	})
	group.Go(func() error {
		return ingest.New(store, m, cfg, self.serverGUID).Serve(ctx, cfg.Ingest.Bind, cfg.Ingest.Port)
	})
	if cfg.Metrics.Enabled {
		group.Go(func() error {
			return m.Serve(ctx, "", cfg.Metrics.Port)
		})
	}
	if cfg.PProf.Enabled {
		group.Go(func() error {
			return obsidipprof.Serve(ctx, "", cfg.PProf.Port)
		})
	}

	// Single-writer background jobs. Each holds its own advisory lock, so N
	// replicas can run and exactly one does each job. The rating applier is the
	// strict case -- Elo is order-dependent -- and the rest would merely
	// duplicate work or fight over one message.
	singletons := map[string]leader.Job{
		"cmdreg":   registerCommands(session, router, cfg.Discord.ApplicationID, self.guildID),
		"ratings":  kills.NewApplier(store, m, cfg).Run,
		"killfeed": kills.NewFeed(store, session, m, cfg, self.guildID).Run,
		"leaderbd": board.New(store, session, m, cfg, self.guildID).Run,
		"decay":    kills.NewDecayer(store, cfg).Run,
		"prune":    kills.NewPruner(store, cfg).Run,
	}
	for name, job := range singletons {
		runner := leader.New(store.Pool(), name, leaderRetry, func(job string) {
			m.LeaderTransitionsTotal.WithLabelValues(job).Inc()
		})
		group.Go(func() error { return runner.Run(ctx, job) })
	}

	// The bank reconciler is the exception: its rows are independent per
	// player and it takes them FOR UPDATE SKIP LOCKED, so every replica shares
	// the work rather than contending for a lock.
	group.Go(func() error {
		return bank.NewReconciler(store, game, m, cfg).Run(ctx)
	})

	return group.Wait()
}

// identity is the pair of things obsidibot has to know about the world it is
// running in, and neither is configurable.
type identity struct {
	guildID    string
	serverGUID string
}

// discoverIdentity asks the systems that own these values what they are.
//
// They are discovered rather than configured because both are identifiers that
// fail SILENTLY when copied wrong, in ways that look like nothing rather than
// like an error: a wrong guild registers commands into a server nobody is
// watching, and a wrong server GUID rejects every kill the game sends, which is
// indistinguishable from a server nobody is playing on. The guild the bot is in
// and the server RCON is pointed at are the right answers by construction.
//
// Both failures are fatal rather than deferred. Serving Discord without knowing
// the guild would post nowhere, and accepting webhooks without the GUID would
// mean rejecting real kills and losing them.
func discoverIdentity(ctx context.Context, session *discordgo.Session, game *pot.Client) (identity, error) {
	guild, err := interactions.DiscoverGuild(ctx, session)
	if err != nil {
		return identity{}, fmt.Errorf("could not work out which guild to serve: %w", err)
	}
	slog.InfoContext(ctx, "discovered the guild to serve", "guild", guild.Name, "guildId", guild.ID)

	info, err := game.ServerInfo(ctx)
	if err != nil {
		return identity{}, fmt.Errorf("could not read the game server's GUID over RCON: %w", err)
	}
	slog.InfoContext(ctx, "discovered the game server", "server", info.Name, "serverGuid", info.GUID)

	return identity{guildID: guild.ID, serverGUID: info.GUID}, nil
}

// leaderRetry is how long a replica waits before contesting a job's lock again.
// Short enough that failover is quick, long enough that idle replicas are not
// hammering Postgres.
const leaderRetry = 5 * time.Second

// buildRouter assembles every slash command.
func buildRouter(store *db.Store, game *pot.Client, vault *bank.Bank,
	session *discordgo.Session, m *metrics.Metrics, cfg *config.Config,
) (*interactions.Router, error) {
	banking := commands.NewBanker(store, vault, cfg).Commands()
	commandSet := make([]interactions.Command, 0, 3+len(banking))
	commandSet = append(commandSet,
		commands.NewLinker(store, game, cfg).Command(),
		commands.NewStats(store).Command(),
		commands.NewConfig(store).Command(),
	)
	commandSet = append(commandSet, banking...)

	return interactions.NewRouter(cfg.Discord.PublicKey, session, m, commandSet)
}

// registerCommands makes this binary's commands the guild's complete set, then
// idles. It runs under a leader lock because bulk overwrite counts against
// Discord's daily limits, and one replica doing it is enough.
func registerCommands(session *discordgo.Session, router *interactions.Router,
	appID, guildID string,
) leader.Job {
	return func(ctx context.Context) error {
		if err := interactions.Register(ctx, session, appID, guildID, router.Commands()); err != nil {
			return err
		}
		<-ctx.Done()
		return nil
	}
}

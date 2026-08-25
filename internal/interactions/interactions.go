// Package interactions receives Discord slash commands over HTTP.
//
// # Why HTTP and not a gateway
//
// A gateway bot holds a stateful WebSocket and must coordinate shard
// assignment between replicas. An HTTP interactions endpoint holds nothing:
// Discord posts a signed request, any replica answers it, and scaling is
// `replicas: N` with no further thought. That is the entire reason obsidibot
// is horizontally scalable, and it is why the Ed25519 verification below is
// not optional -- it is the only thing distinguishing Discord from anyone else
// who finds the URL.
//
// # The three-second rule
//
// Discord abandons an interaction whose first response takes longer than three
// seconds. Every command that touches RCON will sometimes exceed that, so those
// commands ACK immediately with a "thinking" response and finish the work
// afterwards, editing the reply in place. The ACK goes out on the original HTTP
// response; the edit is a separate REST call authenticated by the interaction
// token, which is why the replica that received the command can always be the
// one that finishes it.
package interactions

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/USA-RedDragon/obsidibot/internal/metrics"
	"github.com/USA-RedDragon/obsidibot/internal/server"
	"github.com/bwmarrin/discordgo"
)

// maxRequestBody caps an interaction payload. Discord's own are far smaller;
// this exists so an unauthenticated caller cannot stream megabytes into the
// verifier before the signature check gets a chance to reject them.
const maxRequestBody = 1 << 20

// deferredBudget bounds the work behind a deferred reply. Discord invalidates
// an interaction token after 15 minutes, so anything past that could not be
// delivered anyway; this is deliberately far shorter, because a command that
// has not finished in a minute is not going to.
const deferredBudget = time.Minute

// shutdownGrace lets in-flight interactions finish before the listener closes.
// It clears deferredBudget deliberately: cutting a banking command off midway
// is exactly the situation the ledger exists to avoid, so shutdown waits.
//
// It bounds TWO waits in sequence, and the second is the one that matters.
// http.Server's Shutdown only drains ACTIVE requests, and a deferred command's
// request ended the moment its ACK was written -- so the goroutine still doing
// the work is invisible to it, and Shutdown returns straight away. Serve
// therefore waits on the router's own tracking afterwards, before the caller
// closes the database underneath commands that are mid-transfer.
//
// Each wait gets the whole grace rather than sharing one deadline, so the
// theoretical worst case is twice this. That is deliberate rather than
// overlooked: the first wait is over in milliseconds whenever the deferred path
// is what is holding things up, and the supervisor's own kill timer is the real
// ceiling either way.
const shutdownGrace = deferredBudget + 10*time.Second

// genericFailure is what a caller sees when a command could not be completed.
// It says nothing about why: the reason is in the log, and a stack trace or a
// database error is not something to render into a Discord channel.
const genericFailure = "Something went wrong. This has been logged."

// Context is what a command handler is given: the interaction itself, plus the
// pieces every handler needs and none of them should reach for globally.
type Context struct {
	// Interaction is the raw payload, for handlers that need the guild id or
	// the resolved options.
	Interaction *discordgo.InteractionCreate
	// UserID is the invoking Discord user, resolved from Member (in a guild) or
	// User (in a DM), so no handler has to remember which is populated.
	UserID string
	// GuildID is empty for a DM.
	GuildID string
}

// Reply is what a handler returns. A handler never writes to the network
// itself: it describes a reply, and the router delivers it either as the
// immediate response or as an edit to a deferred one.
type Reply struct {
	Content string
	Embeds  []*discordgo.MessageEmbed
	// Ephemeral hides the reply from everyone but the caller. It is only
	// honoured on an IMMEDIATE reply: the deferred ACK has already declared
	// visibility by the time a handler finishes, which is why Command.Ephemeral
	// rather than this field decides it for deferred commands.
	Ephemeral bool
	// UserError marks a refusal the caller caused and can fix, and selects the
	// metric label so "you are not linked" does not read as a system fault on
	// a dashboard.
	UserError bool
	// Components are the buttons and selects attached to the reply. Their
	// custom IDs are routed back here by prefix -- see Component.
	Components []discordgo.MessageComponent
}

// Handler runs one command.
type Handler func(ctx context.Context, ic Context) (Reply, error)

// Editor delivers the finished reply for a deferred command.
//
// *discordgo.Session satisfies it. The interface exists so the deferred path
// -- the one part of this package that reaches the network from a background
// goroutine -- can be exercised without one.
type Editor interface {
	InteractionResponseEdit(interaction *discordgo.Interaction, newresp *discordgo.WebhookEdit,
		options ...discordgo.RequestOption) (*discordgo.Message, error)
}

// Command is one registered slash command.
type Command struct {
	// Definition is what gets registered with Discord.
	Definition *discordgo.ApplicationCommand
	// Handler runs it.
	Handler Handler
	// Defer makes the router ACK immediately and edit the reply when Handler
	// returns. Required for anything that talks to RCON or waits on a lock.
	Defer bool
	// Ephemeral hides the reply from everyone but the caller.
	Ephemeral bool
	// RequiresManageGuild gates the command on the caller's Discord
	// permissions, read from the interaction payload. Discord signs the whole
	// payload, so the permission bits in it are as trustworthy as the request
	// itself -- there is no need to ask the API who the caller is.
	RequiresManageGuild bool
}

// Component handles a press on a button or a select.
//
// # Why the state lives in the custom ID
//
// Every replica is identical and shares nothing, so the press may well arrive
// at a different one than drew the message. Anything the handler needs -- which
// player, which page -- therefore has to travel in the custom ID, which Discord
// hands back verbatim. Holding it in memory would work in testing and fail in
// production the moment a load balancer did its job.
//
// Prefix is matched against the custom ID up to its first colon, so one handler
// owns a family of IDs like "stats:555-000-101:48:10".
type Component struct {
	Prefix  string
	Handler Handler
}

// Router verifies, routes and answers interactions.
type Router struct {
	publicKey  ed25519.PublicKey
	editor     Editor
	metrics    *metrics.Metrics
	commands   map[string]Command
	components map[string]Component
	// deferred counts the background goroutines started by deferAndRun. They
	// outlive the request that started them -- that is the whole point of a
	// deferred reply -- so something has to know they exist, or shutdown will
	// pull the database out from under a transfer that has already moved marks.
	deferred sync.WaitGroup
}

// NewRouter builds a Router. publicKeyHex is the application's Ed25519 public
// key; an invalid one is a startup failure rather than a per-request one.
// components are variadic so the many callers that register none -- every test
// of command routing, for one -- are untouched by their existence.
func NewRouter(publicKeyHex string, editor Editor, m *metrics.Metrics,
	commands []Command, components ...Component,
) (*Router, error) {
	key, err := hex.DecodeString(publicKeyHex)
	if err != nil {
		return nil, fmt.Errorf("decode discord.publicKey: %w", err)
	}
	if len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("discord.publicKey is %d bytes, want %d", len(key), ed25519.PublicKeySize)
	}

	byName := make(map[string]Command, len(commands))
	for _, cmd := range commands {
		if _, dup := byName[cmd.Definition.Name]; dup {
			// Two handlers for one name means one of them silently never runs.
			return nil, fmt.Errorf("duplicate command %q", cmd.Definition.Name)
		}
		byName[cmd.Definition.Name] = cmd
	}

	byPrefix := make(map[string]Component, len(components))
	for _, component := range components {
		if component.Prefix == "" || strings.Contains(component.Prefix, ":") {
			// The colon is the delimiter, so a prefix containing one could
			// never be matched and would fail as a silently dead button.
			return nil, fmt.Errorf("component prefix %q must be non-empty and contain no colon", component.Prefix)
		}
		if _, dup := byPrefix[component.Prefix]; dup {
			return nil, fmt.Errorf("duplicate component prefix %q", component.Prefix)
		}
		byPrefix[component.Prefix] = component
	}

	return &Router{
		publicKey: key, editor: editor, metrics: m,
		commands: byName, components: byPrefix,
	}, nil
}

// Commands returns the definitions to register, sorted by name.
//
// The sort is not required by Discord -- the registration is a bulk overwrite --
// but ranging a map gave a different order every boot, which made the
// "registered slash commands" log line reshuffle for no reason and any diff of
// two boots meaningless.
func (r *Router) Commands() []*discordgo.ApplicationCommand {
	defs := make([]*discordgo.ApplicationCommand, 0, len(r.commands))
	for _, cmd := range r.commands {
		defs = append(defs, cmd.Definition)
	}
	slices.SortFunc(defs, func(a, b *discordgo.ApplicationCommand) int {
		return strings.Compare(a.Name, b.Name)
	})
	return defs
}

// Serve runs the interactions listener until ctx is cancelled.
//
// ready backs /readyz and should report whether this replica can actually do
// its job -- in practice, whether the database is reachable. Its error is
// returned to the caller, so it should say what is wrong without being
// alarming about it.
//
// The health endpoints live HERE rather than beside the metrics because this
// listener is the one that always exists and the one Discord actually talks to.
// Probing a socket that a "metrics.enabled" setting can switch off, to learn
// whether a different socket is serving, is two kinds of wrong at once.
func (r *Router) Serve(ctx context.Context, bind string, port int, ready func(context.Context) error) error {
	srv := &http.Server{
		Handler:           r.Handler(ready),
		ReadHeaderTimeout: server.DefaultReadHeaderTimeout,
		ReadTimeout:       10 * time.Second,
		// The ACK is written promptly even for deferred commands, so this only
		// has to cover an immediate handler.
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
		ErrorLog:     slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	}
	err := server.Serve(ctx, "interactions", srv, bind, port, shutdownGrace)

	// Shutdown has drained the ACTIVE requests, which does NOT include a
	// deferred command: its request finished when the ACK was written and its
	// work is on a goroutine no http.Server knows about. Waiting here is what
	// makes this function's return mean "nothing of mine is still running", and
	// the caller relies on that before closing the database.
	waitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	if waitErr := r.Wait(waitCtx); waitErr != nil {
		// The serve error, if there is one, says why we are shutting down at
		// all, so it wins; this one is still worth saying out loud because it
		// means work was abandoned mid-flight and may need reconciling.
		slog.ErrorContext(waitCtx, "deferred commands were still running at shutdown", "error", waitErr)
		if err == nil {
			err = waitErr
		}
	}
	return err
}

// Wait blocks until every deferred command this router started has finished, or
// ctx expires.
//
// It is separate from Serve so the ordering is explicit at the call site: the
// listener stops accepting, the open requests drain, the deferred work lands,
// and only then may the database it writes through be closed.
func (r *Router) Wait(ctx context.Context) error {
	done := make(chan struct{})
	// WaitGroup.Wait cannot be given a deadline, so it is watched from a
	// goroutine. On the timeout path that goroutine stays parked until the
	// commands eventually finish, which is fine: this is only ever called on
	// the way out, and the alternative is blocking shutdown indefinitely.
	go func() {
		r.deferred.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("interactions: waiting for deferred commands: %w", ctx.Err())
	}
}

// Handler returns the routes this listener answers. Serve wraps it in a
// listener; tests exercise it directly, so the route patterns under test are
// the ones that run.
func (r *Router) Handler(ready func(context.Context) error) http.Handler {
	mux := http.NewServeMux()
	// "{$}" matches the root path exactly, so anything else falls through to a
	// 404 rather than being fed to the verifier.
	mux.Handle("POST /{$}", r)

	// Liveness: the process is up and accepting. Nothing more is claimed, and
	// nothing is looked up -- a liveness probe that can fail on a dependency
	// gets the container restarted for an outage it cannot fix.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Readiness, with the reason in the body.
	//
	// The error is the underlying one, so a database outage answers with
	// something like "database: failed to connect to `user=obsidibot
	// database=obsidibot`: 10.0.0.5:5432 ... connection refused". That is
	// deliberate and it is safe HERE because of how the route in front of this
	// listener is written: an EXACT match on "/", so only Discord's POST
	// traverses the ingress and these two paths are simply not exposed. The
	// kubelet reaches them on the pod IP, which never goes through it.
	//
	// If that route is ever widened to a prefix, this body starts leaking
	// internal hostnames to the internet -- drop it and keep the status code.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, req *http.Request) {
		if err := ready(req.Context()); err != nil {
			slog.WarnContext(req.Context(), "readiness check failed", "error", err)
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	return mux
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// Cap the body BEFORE verification, which reads all of it.
	req.Body = http.MaxBytesReader(w, req.Body, maxRequestBody)

	// This is the authentication boundary. Everything past it is Discord;
	// everything that fails it gets 401 and no explanation, because the only
	// callers who reach here and fail are ones who should not have found it.
	if !discordgo.VerifyInteraction(req, r.publicKey) {
		http.Error(w, "invalid request signature", http.StatusUnauthorized)
		return
	}

	var interaction discordgo.InteractionCreate
	if err := json.NewDecoder(req.Body).Decode(&interaction); err != nil {
		slog.WarnContext(req.Context(), "malformed interaction payload", "error", err)
		http.Error(w, "malformed request body", http.StatusBadRequest)
		return
	}

	switch interaction.Type {
	case discordgo.InteractionPing:
		// Discord sends this when the endpoint URL is saved and periodically
		// afterwards. Answering it is what proves the key is right.
		r.writeJSON(req.Context(), w, &discordgo.InteractionResponse{Type: discordgo.InteractionResponsePong})
	case discordgo.InteractionApplicationCommand:
		r.handleCommand(w, req, &interaction)
	case discordgo.InteractionMessageComponent:
		r.handleComponent(w, req, &interaction)
	case discordgo.InteractionApplicationCommandAutocomplete,
		discordgo.InteractionModalSubmit:
		// Neither is used. Replying with a PONG would tell Discord we handled
		// something we did not, so this is a 400 -- honest, and not retried.
		http.Error(w, "unsupported interaction type", http.StatusBadRequest)
	default:
		http.Error(w, "unsupported interaction type", http.StatusBadRequest)
	}
}

// handleComponent answers a button press.
//
// It never defers. A component press carries the same three-second budget as a
// command, and the only thing behind these is a single indexed query -- so
// deferring would add a round trip and a visible flicker to buy nothing.
//
// The reply REPLACES the message the button is on, which is what makes paging
// feel like paging rather than like a thread of near-identical messages.
func (r *Router) handleComponent(w http.ResponseWriter, req *http.Request, ic *discordgo.InteractionCreate) {
	customID := ic.MessageComponentData().CustomID
	prefix, _, _ := strings.Cut(customID, ":")

	component, known := r.components[prefix]
	if !known {
		// Usually a button from a previous version of the bot, still sitting in
		// somebody's scrollback. Saying so beats letting their click hang.
		slog.WarnContext(req.Context(), "press on an unrouted component", "prefix", prefix)
		r.writeJSON(req.Context(), w, immediate(Reply{
			Content:   "That button is no longer available. Run the command again.",
			Ephemeral: true,
		}))
		return
	}

	ictx := Context{Interaction: ic, UserID: userID(ic), GuildID: ic.GuildID}
	start := time.Now()
	reply, err := component.Handler(req.Context(), ictx)
	if err != nil {
		slog.ErrorContext(req.Context(), "component failed", "prefix", prefix, "error", err)
		reply = Reply{Content: genericFailure, Ephemeral: true}
	}
	r.observe(prefix, err == nil && !reply.UserError, time.Since(start))
	r.writeJSON(req.Context(), w, update(reply))
}

func (r *Router) handleCommand(w http.ResponseWriter, req *http.Request, ic *discordgo.InteractionCreate) {
	data := ic.ApplicationCommandData()
	cmd, known := r.commands[data.Name]
	if !known {
		// A command Discord knows about and this binary does not means a stale
		// registration -- worth a log line, and worth telling the caller
		// something rather than timing out their interaction.
		slog.WarnContext(req.Context(), "interaction for an unregistered command", "command", data.Name)
		r.writeJSON(req.Context(), w, immediate(Reply{
			Content:   "That command is not available on this bot version.",
			Ephemeral: true,
		}))
		return
	}

	ictx := Context{Interaction: ic, UserID: userID(ic), GuildID: ic.GuildID}

	if cmd.RequiresManageGuild && !hasManageGuild(ic) {
		r.observe(data.Name, true, 0)
		r.writeJSON(req.Context(), w, immediate(Reply{
			Content:   "You need the Manage Server permission to use that.",
			Ephemeral: true,
		}))
		return
	}

	if cmd.Defer {
		r.deferAndRun(w, req, cmd, ictx, data.Name)
		return
	}

	start := time.Now()
	reply, err := cmd.Handler(req.Context(), ictx)
	if err != nil {
		slog.ErrorContext(req.Context(), "command failed", "command", data.Name, "error", err)
		reply = Reply{Content: reply.Content, Ephemeral: true}
		if reply.Content == "" {
			reply.Content = genericFailure
		}
	}
	r.observe(data.Name, err == nil && !reply.UserError, time.Since(start))
	if cmd.Ephemeral {
		reply.Ephemeral = true
	}
	r.writeJSON(req.Context(), w, immediate(reply))
}

// deferAndRun ACKs immediately and finishes the work in the background.
func (r *Router) deferAndRun(w http.ResponseWriter, req *http.Request, cmd Command, ictx Context, name string) {
	flags := discordgo.MessageFlags(0)
	if cmd.Ephemeral {
		flags = discordgo.MessageFlagsEphemeral
	}
	r.writeJSON(req.Context(), w, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: flags},
	})

	// WithoutCancel because the request context dies the moment the ACK is
	// written, and the work has only just started. The interaction token in
	// ictx is what authorises the edit later, so this goroutine does not need
	// the request -- only its own deadline.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), deferredBudget)

	// Counted BEFORE the goroutine starts, so a shutdown that begins in the
	// window between the ACK and the goroutine being scheduled still waits for
	// it. See Wait.
	r.deferred.Add(1)
	go func() {
		defer r.deferred.Done()
		defer cancel()
		// Declared before the recover below so the panic path can report how
		// long the command ran, exactly as the success path does.
		start := time.Now()
		// A panic in a bare goroutine takes the PROCESS down, not just the
		// request -- one bad command would kill a replica that is serving
		// everything else correctly. The caller is told something went wrong
		// and the stack goes to the log.
		//
		// The recovered value is named rec rather than r: r is the Router, and
		// shadowing it here is how this path lost the ability to answer at all.
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			slog.ErrorContext(ctx, "deferred command panicked",
				"command", name, "panic", rec, "stack", string(debug.Stack()))
			// A panic is a failure of the command like any other, and this
			// package's rule is that an edit ALWAYS lands: the ACK has already
			// been written, so returning here leaves the caller watching
			// "thinking" until Discord gives up, with nothing on a dashboard to
			// say it happened.
			r.observe(name, false, time.Since(start))
			if editErr := r.edit(ctx, ictx.Interaction.Interaction, Reply{Content: genericFailure}); editErr != nil {
				slog.ErrorContext(ctx, "could not deliver the reply for a panicked command",
					"command", name, "error", editErr)
			}
		}()

		reply, err := cmd.Handler(ctx, ictx)
		if err != nil {
			slog.ErrorContext(ctx, "deferred command failed", "command", name, "error", err)
			if reply.Content == "" {
				reply.Content = genericFailure
			}
		}
		r.observe(name, err == nil && !reply.UserError, time.Since(start))

		if editErr := r.edit(ctx, ictx.Interaction.Interaction, reply); editErr != nil {
			// The work itself may well have succeeded -- a deposit that moved
			// marks and then failed to update the message is not a lost
			// deposit -- so this is a delivery failure, logged as such.
			slog.ErrorContext(ctx, "could not deliver deferred reply",
				"command", name, "error", editErr)
		}
	}()
}

// edit replaces a deferred reply with the finished one.
func (r *Router) edit(ctx context.Context, interaction *discordgo.Interaction, reply Reply) error {
	edit := &discordgo.WebhookEdit{}
	if reply.Content != "" {
		edit.Content = &reply.Content
	}
	if len(reply.Embeds) > 0 {
		edit.Embeds = &reply.Embeds
	}
	if len(reply.Components) > 0 {
		// Carried here as well as on the immediate path, so a command that
		// grows a Defer later does not silently lose its buttons.
		edit.Components = &reply.Components
	}
	if edit.Content == nil && edit.Embeds == nil {
		// An empty edit leaves the user staring at "thinking" forever.
		content := "Done."
		edit.Content = &content
	}
	if r.editor == nil {
		// Only reachable from a misconfigured construction; failing loudly here
		// beats a nil dereference inside a goroutine.
		return errors.New("interactions: no editor configured for deferred replies")
	}
	if _, err := r.editor.InteractionResponseEdit(interaction, edit, discordgo.WithContext(ctx)); err != nil {
		return fmt.Errorf("edit interaction response: %w", err)
	}
	return nil
}

func (r *Router) observe(command string, ok bool, elapsed time.Duration) {
	if r.metrics == nil {
		return
	}
	result := metrics.ResultError
	if ok {
		result = metrics.ResultOK
	}
	r.metrics.InteractionsTotal.WithLabelValues(command, result).Inc()
	if elapsed > 0 {
		r.metrics.InteractionDuration.WithLabelValues(command).Observe(elapsed.Seconds())
	}
}

func (r *Router) writeJSON(ctx context.Context, w http.ResponseWriter, resp *discordgo.InteractionResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.DebugContext(ctx, "write interaction response", "error", err)
	}
}

func immediate(reply Reply) *discordgo.InteractionResponse {
	return &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: responseData(reply),
	}
}

// update replaces the message a component is attached to, rather than posting
// a new one.
func update(reply Reply) *discordgo.InteractionResponse {
	return &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: responseData(reply),
	}
}

func responseData(reply Reply) *discordgo.InteractionResponseData {
	data := &discordgo.InteractionResponseData{
		Content:    reply.Content,
		Embeds:     reply.Embeds,
		Components: reply.Components,
		// Mentions are rendered but never ping. The leaderboard names up to
		// twenty people every minute; pinging them would be unusable -- and
		// /stats names whoever killed you, which must not buzz their phone.
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	}
	if reply.Ephemeral {
		data.Flags = discordgo.MessageFlagsEphemeral
	}
	return data
}

// userID resolves the caller from whichever field Discord populated: Member in
// a guild, User in a DM.
func userID(ic *discordgo.InteractionCreate) string {
	if ic.Member != nil && ic.Member.User != nil {
		return ic.Member.User.ID
	}
	if ic.User != nil {
		return ic.User.ID
	}
	return ""
}

// hasManageGuild reads the caller's permissions from the signed payload.
//
// Discord resolves these itself and signs them along with everything else, so
// they are exactly as trustworthy as the request -- there is no benefit to
// asking the API again, and doing so would put a network round trip inside the
// three-second budget.
func hasManageGuild(ic *discordgo.InteractionCreate) bool {
	if ic.Member == nil {
		// A DM has no guild and therefore no Manage Server.
		return false
	}
	return ic.Member.Permissions&discordgo.PermissionManageGuild != 0
}

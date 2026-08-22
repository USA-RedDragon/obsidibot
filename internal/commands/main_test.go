package commands_test

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/USA-RedDragon/obsidibot/internal/bank"
	"github.com/USA-RedDragon/obsidibot/internal/commands"
	"github.com/USA-RedDragon/obsidibot/internal/config"
	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/db/gen"
	"github.com/USA-RedDragon/obsidibot/internal/dbtest"
	"github.com/USA-RedDragon/obsidibot/internal/interactions"
	"github.com/USA-RedDragon/obsidibot/internal/metrics"
	"github.com/USA-RedDragon/obsidibot/internal/moderation"
	"github.com/USA-RedDragon/obsidibot/internal/pot"
	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fakeRCON stands in for the game server. It answers with real response
// strings, with player identifiers anonymised, so the parsing under test is the
// parsing that runs in production.
type fakeRCON struct {
	mu       sync.Mutex
	commands []string
	messages []string

	// online controls whether the player is present.
	online bool
	// marks is the balance the fake reports, and which AddMarks/RemoveMarks
	// move, so a test can watch a transfer actually happen.
	marks int64
	// failVerb makes one command fail, for testing the recovery paths.
	failVerb string
	failWith error
	// rejectVerb makes one command come back with the game's "That command
	// does not exist." rather than an error.
	rejectVerb string
	// swallowVerb accepts a mutating command but does NOT apply it, which is
	// how a silently-lost RCON write is simulated.
	swallowVerb string
	// kicked, banned and unbanned record moderation enforcement. banned is the
	// game's ban list: Ban appends to it, a second Ban on the same identity
	// answers "already banned", and Unban removes it -- which is what makes
	// the fake able to tell an enforced ban from a lifted one.
	kicked   []string
	banned   []string
	unbanned []string
	// serverAdmins are identities the game refuses to ban, the way Game.ini's
	// ServerAdmins list does. Verified live: this is PERMANENT for as long as
	// they are listed.
	serverAdmins map[string]bool
	// onBan runs while a Ban command is "in flight", which is how a test opens
	// the window a concurrent /unban lands in.
	onBan func(ident string)
	// beforeMutation runs just before a marks command is applied, so a test can
	// simulate the player spending marks between the bot's balance read and its
	// command -- the race that makes the server-side clamp matter.
	beforeMutation func(*fakeRCON)
}

func newFakeRCON() *fakeRCON {
	return &fakeRCON{online: true, marks: 3838, serverAdmins: map[string]bool{}}
}

func (f *fakeRCON) Execute(_ context.Context, command string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command)

	fields := strings.Fields(command)
	verb := fields[0]

	if f.failVerb != "" && strings.EqualFold(verb, f.failVerb) {
		return "", f.failWith
	}
	if f.rejectVerb != "" && strings.EqualFold(verb, f.rejectVerb) {
		return fmt.Sprintf("(%s): That command does not exist.", command), nil
	}

	switch strings.ToLower(verb) {
	case "playerinfo":
		if !f.online {
			return fmt.Sprintf("(%s): No player with the username '%s'.", command, fields[1]), nil
		}
		return fmt.Sprintf(
			"(%s): Name: %s / AGID: %s / Dinosaur: Ceratosaurus / Role: Owner / Marks: %d / Growth: 1 /"+
				" Location: (X=104037.330 Y=169175.200 Z=-596.830)",
			command, testName, testAGID, f.marks), nil

	case "addmarks", "removemarks":
		if !f.online {
			return fmt.Sprintf("(%s): No player with the username '%s'.", command, fields[1]), nil
		}
		var amount int64
		if _, err := fmt.Sscanf(fields[2], "%d", &amount); err != nil {
			return fmt.Sprintf("(%s): Incorrect Syntax, type /help.", command), nil
		}
		if f.beforeMutation != nil {
			hook := f.beforeMutation
			f.beforeMutation = nil
			hook(f)
		}
		if f.swallowVerb != "" && strings.EqualFold(verb, f.swallowVerb) {
			// Accepted, and deliberately not applied: a silently lost write.
			// The echo is omitted too, which is what an unreadable answer
			// looks like from the caller's side.
			return fmt.Sprintf("(%s): ok", command), nil
		}
		if strings.EqualFold(verb, "addmarks") {
			f.marks += amount
			return fmt.Sprintf("(%s): Added %d Marks to %s. They now have %d Marks.",
				command, amount, fields[1], f.marks), nil
		}
		// The real server CLAMPS a removal at zero and reports what it
		// actually took, which is the behaviour the bank depends on.
		moved := amount
		if moved > f.marks {
			moved = f.marks
		}
		f.marks -= moved
		return fmt.Sprintf("(%s): Removed %d Marks from %s. They now have %d Marks.",
			command, moved, fields[1], f.marks), nil

	case "whisper", "systemmessage":
		if !f.online {
			return fmt.Sprintf("(%s): No player with the username '%s'.", command, fields[1]), nil
		}
		f.messages = append(f.messages, strings.Join(fields[2:], " "))
		return fmt.Sprintf("(%s): ok", command), nil

	// The moderation verbs answer with the strings captured VERBATIM from a
	// live server during design, so the classification exercised here is the
	// classification that runs in production.
	case "kick":
		if !f.online {
			return fmt.Sprintf("(%s): Failed to kick '%s'.", command, fields[1]), nil
		}
		f.kicked = append(f.kicked, fields[1])
		return fmt.Sprintf("(%s): Requested action against player ID.", command), nil

	case "ban":
		ident := fields[1]
		if f.onBan != nil {
			hook := f.onBan
			f.onBan = nil
			hook(ident)
		}
		if f.serverAdmins[ident] {
			// Verified live: the refusal reads Game.ini's ServerAdmins list, so
			// no RCON call can ever make this target bannable.
			return fmt.Sprintf("(%s): Cannot ban an admin.", command), nil
		}
		if slices.Contains(f.banned, ident) {
			return fmt.Sprintf("(%s): Player '%s' is already banned.", command, ident), nil
		}
		f.banned = append(f.banned, ident)
		return fmt.Sprintf("(%s): Banned '%s' forever, Admin reason = %s", command, ident, fields[3]), nil

	case "unban":
		ident := fields[1]
		at := slices.Index(f.banned, ident)
		if at < 0 {
			return fmt.Sprintf(
				"(%s): Unknown ban string '%s'. Perhaps you meant to use AGID?", command, ident), nil
		}
		f.banned = slices.Delete(f.banned, at, at+1)
		f.unbanned = append(f.unbanned, ident)
		return fmt.Sprintf("(%s): Unbanned player with Id '%s'.", command, ident), nil
	}
	return fmt.Sprintf("(%s): That command does not exist.", command), nil
}

//nolint:gochecknoglobals // compiled once for the tests
var codeRE = regexp.MustCompile(`obsidibot link code: ([A-Z0-9]{6})`)

// lastCode digs the delivered code out of the in-game messages, which is the
// only place it is ever supposed to appear.
func (f *fakeRCON) lastCode(t *testing.T) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.messages) - 1; i >= 0; i-- {
		if m := codeRE.FindStringSubmatch(f.messages[i]); m != nil {
			return m[1]
		}
	}
	return ""
}

func (f *fakeRCON) messageCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.messages)
}

func (f *fakeRCON) commandCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.commands)
}

func (f *fakeRCON) issued() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

func (f *fakeRCON) balance() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.marks
}

// testConfig is the shipped defaults, not a special test set: a command that
// only works under bespoke configuration is not tested.
func testConfig() *config.Config {
	return &config.Config{
		Rating: config.Rating{
			Initial: 1200, ProvisionalK: 40, SettlingK: 20, StableK: 16,
			ProvisionalGames: 20, SettlingGames: 50,
			DecayGraceDays: 30, DecayPermillePerDay: 5,
		},
		Bank: config.Bank{CooldownSeconds: 0, VerifyAttempts: 5},
		Link: config.Link{
			CodeTTLSeconds: 300, MaxAttempts: 5, ReissueCooldownSeconds: 30,
		},
		Leaderboard: config.Leaderboard{IntervalSeconds: 60, Size: 20},
		KillFeed:    config.KillFeed{RetentionDays: 30},
	}
}

type linkHarness struct {
	pool   *pgxpool.Pool
	store  *db.Store
	rcon   *fakeRCON
	cfg    *config.Config
	linker *commands.Linker
	banker *commands.Banker
}

func newLinkHarness(t *testing.T) *linkHarness {
	t.Helper()
	pool := dbtest.Pool(t)
	dbtest.Reset(t, pool)
	if err := db.Migrate(context.Background(), pool, dbtest.MigrationsFS(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := db.NewStore(pool)
	fake := newFakeRCON()
	cfg := testConfig()
	rcon := pot.NewClient(fake, nil)
	return &linkHarness{
		pool:   pool,
		store:  store,
		rcon:   fake,
		cfg:    cfg,
		linker: commands.NewLinker(store, rcon, cfg),
		banker: commands.NewBanker(store, bank.New(store, rcon, metrics.New(), cfg), cfg),
	}
}

// isBanned reports whether the fake game server is holding a ban.
//
//nolint:unparam // named at the call site rather than implied by the helper
func (f *fakeRCON) isBanned(ident string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.banned, ident)
}

func (f *fakeRCON) kickedPlayers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.kicked...)
}

func (f *fakeRCON) unbannedPlayers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.unbanned...)
}

// makeServerAdmin puts an identity in the fake's ServerAdmins list, which the
// game refuses to ban -- permanently, verified live.
func (f *fakeRCON) makeServerAdmin(ident string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.serverAdmins[ident] = true
}

// fakePoster stands in for the Discord session the feeds post through.
type fakePoster struct {
	mu     sync.Mutex
	posts  []fakePost
	err    error
	blocks bool
}

type fakePost struct {
	channelID string
	embed     *discordgo.MessageEmbed
}

func (p *fakePoster) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend,
	_ ...discordgo.RequestOption,
) (*discordgo.Message, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.blocks {
		return nil, &discordgo.RESTError{
			Message: &discordgo.APIErrorMessage{Code: discordgo.ErrCodeMissingPermissions},
		}
	}
	if p.err != nil {
		return nil, p.err
	}
	var embed *discordgo.MessageEmbed
	if len(data.Embeds) > 0 {
		embed = data.Embeds[0]
	}
	p.posts = append(p.posts, fakePost{channelID: channelID, embed: embed})
	return &discordgo.Message{}, nil
}

func (p *fakePoster) titles() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.posts))
	for _, post := range p.posts {
		if post.embed != nil {
			out = append(out, post.embed.Title)
		}
	}
	return out
}

func (p *fakePoster) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.posts)
}

// testGuild is the guild every command in these tests arrives from.
const testGuild = "g1"

// modHarness wires the moderation commands and the scheduler onto ONE fake game
// server and one database, because the interesting behaviour is the handover
// between them: a command records and enforces, the scheduler enforces what it
// could not and lifts what expired.
type modHarness struct {
	pool   *pgxpool.Pool
	store  *db.Store
	rcon   *fakeRCON
	poster *fakePoster
	mod    *commands.Moderation
	sched  *moderation.Scheduler
}

func newModHarness(t *testing.T) *modHarness {
	t.Helper()
	pool := dbtest.Pool(t)
	dbtest.Reset(t, pool)
	if err := db.Migrate(context.Background(), pool, dbtest.MigrationsFS(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := db.NewStore(pool)
	fake := newFakeRCON()
	game := pot.NewClient(fake, nil)
	poster := &fakePoster{}
	m := metrics.New()

	return &modHarness{
		pool:   pool,
		store:  store,
		rcon:   fake,
		poster: poster,
		mod:    commands.NewModeration(store, game, poster, m),
		sched:  moderation.NewScheduler(store, game, poster, m, testGuild),
	}
}

// command finds one of the four moderation commands by name.
func (h *modHarness) command(t *testing.T, name string) interactions.Command {
	t.Helper()
	for _, cmd := range h.mod.Commands() {
		if cmd.Definition.Name == name {
			return cmd
		}
	}
	t.Fatalf("no command named %q", name)
	return interactions.Command{}
}

// run invokes a moderation command as a caller holding Manage Server.
func (h *modHarness) run(t *testing.T, name string,
	opts ...*discordgo.ApplicationCommandInteractionDataOption,
) interactions.Reply {
	t.Helper()
	return h.runAs(t, name, manageGuildMember(), opts...)
}

func (h *modHarness) runAs(t *testing.T, name string, member *discordgo.Member,
	opts ...*discordgo.ApplicationCommandInteractionDataOption,
) interactions.Reply {
	t.Helper()
	reply, err := h.command(t, name).Handler(context.Background(), modInvoke(name, member, opts...))
	if err != nil {
		t.Fatalf("/%s: %v", name, err)
	}
	return reply
}

// feedChannels configures both feed channels, so a test that cares about the
// notices gets them and one that does not is not slowed by Discord at all.
func (h *modHarness) feedChannels(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	ban, warn := "ban-channel", "warn-channel"
	if err := h.store.Queries().SetBanFeedChannel(ctx, gen.SetBanFeedChannelParams{
		GuildID: testGuild, BanFeedChannelID: &ban,
	}); err != nil {
		t.Fatalf("set ban channel: %v", err)
	}
	if err := h.store.Queries().SetWarnFeedChannel(ctx, gen.SetWarnFeedChannelParams{
		GuildID: testGuild, WarnFeedChannelID: &warn,
	}); err != nil {
		t.Fatalf("set warn channel: %v", err)
	}
}

// link records a player and links them to a Discord account, the state most
// moderation actions are taken against.
//
//nolint:unparam // named at the call site rather than implied by the helper
func (h *modHarness) link(t *testing.T, agid, name, discordID string) {
	t.Helper()
	ctx := context.Background()
	q := h.store.Queries()
	if err := q.UpsertPlayerSeen(ctx, gen.UpsertPlayerSeenParams{
		AlderonID: agid, LastKnownName: name, Rating: 1200,
	}); err != nil {
		t.Fatalf("seed player: %v", err)
	}
	if err := q.CreateLink(ctx, gen.CreateLinkParams{DiscordUserID: discordID, AlderonID: agid}); err != nil {
		t.Fatalf("seed link: %v", err)
	}
}

// modInvoke builds the interaction a moderation command arrives in, carrying
// the Member the role gate reads. Discord signs the member's roles and
// permission bits into the payload, so this is the real input shape.
func modInvoke(name string, member *discordgo.Member,
	opts ...*discordgo.ApplicationCommandInteractionDataOption,
) interactions.Context {
	guildID := testGuild
	if member == nil {
		// No member means a DM: no guild, no roles, no permissions.
		guildID = ""
	}
	return interactions.Context{
		UserID:  moderatorUser,
		GuildID: guildID,
		Interaction: &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
			Type:    discordgo.InteractionApplicationCommand,
			GuildID: guildID,
			Member:  member,
			Data: discordgo.ApplicationCommandInteractionData{
				Name:    name,
				Options: opts,
			},
		}},
	}
}

const moderatorUser = "discord-mod"

func manageGuildMember() *discordgo.Member {
	return &discordgo.Member{
		User:        &discordgo.User{ID: moderatorUser},
		Permissions: discordgo.PermissionManageGuild,
	}
}

// roleMember is a caller with no permissions at all, holding whichever roles
// are given -- the other half of the gate.
func roleMember(roles ...string) *discordgo.Member {
	return &discordgo.Member{User: &discordgo.User{ID: moderatorUser}, Roles: roles}
}

//nolint:unparam // named at the call site rather than implied by the helper
func userOpt(id string) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{
		Name: "user", Type: discordgo.ApplicationCommandOptionUser, Value: id,
	}
}

func stringOpt(name, value string) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{
		Name: name, Type: discordgo.ApplicationCommandOptionString, Value: value,
	}
}

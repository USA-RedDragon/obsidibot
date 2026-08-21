package commands_test

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/USA-RedDragon/obsidibot/internal/bank"
	"github.com/USA-RedDragon/obsidibot/internal/commands"
	"github.com/USA-RedDragon/obsidibot/internal/config"
	"github.com/USA-RedDragon/obsidibot/internal/db"
	"github.com/USA-RedDragon/obsidibot/internal/dbtest"
	"github.com/USA-RedDragon/obsidibot/internal/metrics"
	"github.com/USA-RedDragon/obsidibot/internal/pot"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fakeRCON stands in for the game server. It answers with the real response
// response strings, with player identifiers anonymised, so the parsing under
// test is the parsing that runs in production.
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
	// beforeMutation runs just before a marks command is applied, so a test can
	// simulate the player spending marks between the bot's balance read and its
	// command -- the race that makes the server-side clamp matter.
	beforeMutation func(*fakeRCON)
}

func newFakeRCON() *fakeRCON {
	return &fakeRCON{online: true, marks: 3838}
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
		Bank: config.Bank{CooldownSeconds: 0, VerifyAttempts: 5, VerifyBackoffSeconds: 1},
		Link: config.Link{
			CodeTTLSeconds: 300, MaxAttempts: 5, ReissueCooldownSeconds: 30,
		},
		Leaderboard: config.Leaderboard{IntervalSeconds: 60, Size: 20},
		KillFeed:    config.KillFeed{RetentionDays: 30},
	}
}

func migrationsFS(t *testing.T) fs.FS {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	return os.DirFS(filepath.Join(filepath.Dir(thisFile), "..", "..", "schema", "migrations"))
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
	if err := db.Migrate(context.Background(), pool, migrationsFS(t)); err != nil {
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

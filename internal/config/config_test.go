package config

import (
	"strings"
	"testing"
)

const (
	testSecret    = "0123456789abcdef0123456789abcdef"
	testPublicKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func valid() Config {
	return Config{
		LogLevel:     LogLevelInfo,
		Interactions: Interactions{Port: 8080},
		Ingest:       Ingest{Port: 8081, Secret: testSecret},
		Metrics:      Metrics{Enabled: true, Port: 9090},
		PProf:        PProf{Enabled: false, Port: 6060},
		Database:     Database{URL: "postgres://user:pass@host:5432/obsidibot"},
		Discord: Discord{
			Token:         "token",
			ApplicationID: "12345",
			PublicKey:     testPublicKey,
		},
		RCON:        RCON{Host: "127.0.0.1", Port: 7779, Password: "hunter2", TimeoutSeconds: 10, MaxConcurrent: 4},
		Rating:      Rating{Initial: 1200, ProvisionalK: 40, SettlingK: 20, StableK: 16, ProvisionalGames: 20, SettlingGames: 50, DecayGraceDays: 30, DecayPermillePerDay: 5},
		Bank:        Bank{CooldownSeconds: 10, VerifyAttempts: 5, VerifyBackoffSeconds: 5},
		Leaderboard: Leaderboard{IntervalSeconds: 60, Size: 20},
		KillFeed:    KillFeed{RetentionDays: 30},
		Link:        Link{CodeTTLSeconds: 300, MaxAttempts: 5, ReissueCooldownSeconds: 30},
	}
}

func TestValidateAcceptsValid(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

// TestValidateRangeChecks covers the values a YAML file can carry that a narrower
// field type would silently wrap: 70000 truncating to 4464 and -1 to 65535 were
// both real, and neither is visible to a != 0 check.
func TestValidateRangeChecks(t *testing.T) {
	tests := []struct {
		name  string
		mutef func(*Config)
		want  string
	}{
		{"bad log level", func(c *Config) { c.LogLevel = "silly" }, "logLevel"},
		{"interactions port wrapped high", func(c *Config) { c.Interactions.Port = 70000 }, "interactions.port"},
		{"interactions port wrapped low", func(c *Config) { c.Interactions.Port = -1 }, "interactions.port"},
		{"ingest port zero", func(c *Config) { c.Ingest.Port = 0 }, "ingest.port"},
		{"empty ingest secret", func(c *Config) { c.Ingest.Secret = "" }, "ingest.secret"},
		{"short ingest secret", func(c *Config) { c.Ingest.Secret = "tooshort" }, "ingest.secret"},
		{"metrics port when enabled", func(c *Config) { c.Metrics.Port = 0 }, "metrics.port"},
		{"pprof port when enabled", func(c *Config) { c.PProf.Enabled = true; c.PProf.Port = 99999 }, "pprof.port"},
		{"empty database url", func(c *Config) { c.Database.URL = "" }, "database.url"},
		{"empty discord token", func(c *Config) { c.Discord.Token = "" }, "discord.token"},
		{"empty application id", func(c *Config) { c.Discord.ApplicationID = "" }, "discord.applicationId"},
		{"short public key", func(c *Config) { c.Discord.PublicKey = "abcd" }, "discord.publicKey"},
		{"empty rcon host", func(c *Config) { c.RCON.Host = "" }, "rcon.host"},
		{"rcon port wrapped", func(c *Config) { c.RCON.Port = 70000 }, "rcon.port"},
		{"empty rcon password", func(c *Config) { c.RCON.Password = "" }, "rcon.password"},
		{"rcon timeout too high", func(c *Config) { c.RCON.TimeoutSeconds = MaxTimeoutSeconds + 1 }, "rcon.timeoutSeconds"},
		{"rcon concurrency too high", func(c *Config) { c.RCON.MaxConcurrent = MaxConcurrentLimit + 1 }, "rcon.maxConcurrent"},
		{"settling games below provisional", func(c *Config) { c.Rating.SettlingGames = 5 }, "rating.settlingGames"},
		{"bank verify attempts zero", func(c *Config) { c.Bank.VerifyAttempts = 0 }, "bank.verifyAttempts"},
		{"leaderboard size too large", func(c *Config) { c.Leaderboard.Size = 500 }, "leaderboard.size"},
		{"retention zero", func(c *Config) { c.KillFeed.RetentionDays = 0 }, "killfeed.retentionDays"},
		{"link ttl too short", func(c *Config) { c.Link.CodeTTLSeconds = 1 }, "link.codeTTLSeconds"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.mutef(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestPublicKeyMustBeHex guards the case a length check alone misses: 64
// characters of the right shape but not decodable, which would fail at the
// first interaction rather than at startup.
func TestPublicKeyMustBeHex(t *testing.T) {
	cfg := valid()
	cfg.Discord.PublicKey = strings.Repeat("z", ed25519PublicKeyHexLen)
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "hex") {
		t.Fatalf("non-hex public key of the right length was accepted: %v", err)
	}
}

// TestIngestSecretRejectsPathCharacters covers the case that makes the secret a
// path segment dangerous: a secret containing / would silently change which
// route the game's webhook reaches.
func TestIngestSecretRejectsPathCharacters(t *testing.T) {
	for _, bad := range []string{"/", "?", "#", "%"} {
		cfg := valid()
		cfg.Ingest.Secret = testSecret + bad
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "ingest.secret") {
			t.Errorf("secret containing %q was accepted: %v", bad, err)
		}
	}
}

// TestKFactorsMustDescend covers a mistake range checks cannot see: three
// individually legal K factors in an order that makes an established rating
// swing harder than a brand new one.
func TestKFactorsMustDescend(t *testing.T) {
	cfg := valid()
	cfg.Rating.ProvisionalK, cfg.Rating.StableK = cfg.Rating.StableK, cfg.Rating.ProvisionalK
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "descend") {
		t.Fatalf("ascending K factors were accepted: %v", err)
	}
}

// TestPortCollisions is the check that keeps the unsigned webhook endpoint off
// the port an ingress publishes. Every enabled pair must be distinct.
func TestPortCollisions(t *testing.T) {
	tests := []struct {
		name  string
		mutef func(*Config)
		want  string
	}{
		{"ingest onto interactions", func(c *Config) { c.Ingest.Port = c.Interactions.Port }, "ingest.port"},
		{"metrics onto interactions", func(c *Config) { c.Metrics.Port = c.Interactions.Port }, "metrics.port"},
		{"metrics onto ingest", func(c *Config) { c.Metrics.Port = c.Ingest.Port }, "metrics.port"},
		{"pprof onto metrics", func(c *Config) { c.PProf.Enabled = true; c.PProf.Port = c.Metrics.Port }, "pprof.port"},
		{"pprof onto interactions", func(c *Config) { c.PProf.Enabled = true; c.PProf.Port = c.Interactions.Port }, "pprof.port"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.mutef(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestDisabledListenersDoNotCollide: a disabled listener is not bound, so its
// port is free to duplicate. Refusing to start over it would be a false alarm.
func TestDisabledListenersDoNotCollide(t *testing.T) {
	cfg := valid()
	cfg.PProf.Enabled = false
	cfg.PProf.Port = cfg.Metrics.Port
	cfg.Metrics.Enabled = false
	cfg.Metrics.Port = cfg.Interactions.Port
	if err := cfg.Validate(); err != nil {
		t.Fatalf("disabled listeners sharing ports were rejected: %v", err)
	}
}

// TestValidateReportsEveryProblem is the documented behaviour: a bad deployment
// should not have to be fixed one restart at a time.
func TestValidateReportsEveryProblem(t *testing.T) {
	cfg := Config{} // every field wrong at once
	err := cfg.Validate()
	if err == nil {
		t.Fatal("empty config was accepted")
	}
	for _, want := range []string{
		"logLevel", "interactions.port", "ingest.port", "ingest.secret",
		"database.url", "discord.token", "discord.publicKey", "rcon.host",
		"rcon.password", "rating.initial", "leaderboard.size",
		"killfeed.retentionDays", "link.codeTTLSeconds",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestDSNNormalisesPsqlScheme(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"psql://user@host/db", "postgres://user@host/db"},
		{"postgres://user@host/db", "postgres://user@host/db"},
		{"postgresql://user@host/db", "postgresql://user@host/db"},
		{"host=localhost dbname=obsidibot", "host=localhost dbname=obsidibot"},
	}
	for _, tc := range tests {
		if got := (Database{URL: tc.in}).DSN(); got != tc.want {
			t.Errorf("DSN(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAddrs(t *testing.T) {
	cfg := valid()
	if got := cfg.RCON.Addr(); got != "127.0.0.1:7779" {
		t.Errorf("RCON.Addr() = %q", got)
	}
	// An empty bind must produce ":port" so the listener covers every
	// interface rather than a host literally named "".
	if got := cfg.Interactions.Addr(); got != ":8080" {
		t.Errorf("Interactions.Addr() = %q", got)
	}
	if got := cfg.Ingest.Addr(); got != ":8081" {
		t.Errorf("Ingest.Addr() = %q", got)
	}
}

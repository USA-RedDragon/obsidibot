package config

import (
	"strings"
	"testing"
)

func valid() Config {
	return Config{
		LogLevel: LogLevelInfo,
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

// TestValidateReportsEveryProblem is the documented behaviour: a bad deployment
// should not have to be fixed one restart at a time.
func TestValidateReportsEveryProblem(t *testing.T) {
	cfg := Config{} // every field wrong at once
	err := cfg.Validate()
	if err == nil {
		t.Fatal("empty config was accepted")
	}
	for _, want := range []string{"logLevel", "http.port", "rcon.host", "rcon.port", "rcon.password", "rcon.timeoutSeconds"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

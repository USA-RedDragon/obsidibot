package config

import (
	"errors"
	"fmt"
)

// LogLevel selects the verbosity of the structured logger.
type LogLevel string

const (
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

// Config is the root configuration.
type Config struct {
	LogLevel LogLevel `name:"logLevel" default:"info" description:"log verbosity: debug, info, warn, or error"`
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

	if len(errs) > 0 {
		return fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return nil
}

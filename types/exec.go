package types

import (
	"errors"
	"fmt"
	"strings"
)

// ExecConfig describes one command invocation inside a running sandbox. It is
// independent of the guest-agent wire format so core and CLI do not depend on
// protocol frames.
type ExecConfig struct {
	// Args contains the executable followed by its arguments. KumaBox never
	// inserts a shell between this list and the guest process.
	Args []string
	// Env contains caller-provided environment overrides in KEY=VALUE form.
	Env []string
	// Interactive connects the caller's input stream to the guest process.
	Interactive bool
}

// Validate rejects malformed commands before a guest-agent connection opens.
func (c ExecConfig) Validate() error {
	if len(c.Args) == 0 || c.Args[0] == "" {
		return errors.New("COMMAND must not be empty")
	}
	for _, argument := range c.Args {
		if strings.IndexByte(argument, 0) >= 0 {
			return errors.New("command arguments must not contain NUL bytes")
		}
	}
	for _, pair := range c.Env {
		key, _, ok := strings.Cut(pair, "=")
		if !ok || key == "" || strings.IndexByte(pair, 0) >= 0 || strings.Contains(key, "=") {
			return fmt.Errorf("environment %q must be KEY=VALUE", pair)
		}
	}
	return nil
}

// Environment converts validated KEY=VALUE entries into the map used by the
// agent protocol. Repeated keys use the last CLI value.
func (c ExecConfig) Environment() map[string]string {
	if len(c.Env) == 0 {
		return nil
	}
	environment := make(map[string]string, len(c.Env))
	for _, pair := range c.Env {
		key, value, ok := strings.Cut(pair, "=")
		if ok && key != "" {
			environment[key] = value
		}
	}
	return environment
}

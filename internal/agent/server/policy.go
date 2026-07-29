package server

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kumabox/kumabox/internal/agent/protocol"
)

const (
	defaultExecTimeout = 5 * time.Minute
	defaultMaxOutput   = 16 << 20
	maxExecTimeout     = 24 * time.Hour
	maxMaxOutput       = 1 << 30
)

type execPolicy struct {
	timeout   time.Duration
	maxOutput int64
	deniedEnv map[string]struct{}
}

func defaultPolicy() execPolicy {
	return execPolicy{timeout: defaultExecTimeout, maxOutput: defaultMaxOutput, deniedEnv: map[string]struct{}{
		"LD_PRELOAD": {}, "LD_LIBRARY_PATH": {},
	}}
}

func policyFromEnvironment() execPolicy {
	policy := defaultPolicy()
	if value := os.Getenv("KUMABOX_AGENT_EXEC_TIMEOUT"); value != "" {
		if duration, err := time.ParseDuration(value); err == nil && duration > 0 && duration <= maxExecTimeout {
			policy.timeout = duration
		}
	}
	if value := os.Getenv("KUMABOX_AGENT_MAX_OUTPUT_BYTES"); value != "" {
		if limit, err := strconv.ParseInt(value, 10, 64); err == nil && limit > 0 && limit <= maxMaxOutput {
			policy.maxOutput = limit
		}
	}
	if value := os.Getenv("KUMABOX_AGENT_DENY_ENV"); value != "" {
		policy.deniedEnv = make(map[string]struct{})
		for _, key := range strings.Split(value, ",") {
			key = strings.TrimSpace(key)
			if key != "" {
				policy.deniedEnv[key] = struct{}{}
			}
		}
	}
	return policy
}

func validateUser(user string) error {
	if user == "" || user == "root" {
		return nil
	}
	return fmt.Errorf("%w: only root is supported", protocol.ErrorUserUnsupported)
}

func validateEnvironment(values map[string]string, denied map[string]struct{}) error {
	for key := range values {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(values[key], '\x00') {
			return fmt.Errorf("%w: invalid environment key", protocol.ErrorEnvDenied)
		}
		if _, blocked := denied[key]; blocked {
			return fmt.Errorf("%w: environment %q is denied", protocol.ErrorEnvDenied, key)
		}
	}
	return nil
}

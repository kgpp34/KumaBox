package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/agent/protocol"
)

func TestPolicyRejectsUnsupportedUserAndSensitiveEnvironment(t *testing.T) {
	if err := validateUser("nobody"); err == nil || !strings.Contains(err.Error(), string(protocol.ErrorUserUnsupported)) {
		t.Fatalf("user error = %v", err)
	}
	if err := validateUser("root"); err != nil {
		t.Fatalf("root user rejected: %v", err)
	}
	if err := validateEnvironment(map[string]string{"LD_PRELOAD": "evil.so"}, defaultPolicy().deniedEnv); err == nil || !strings.Contains(err.Error(), string(protocol.ErrorEnvDenied)) {
		t.Fatalf("environment error = %v", err)
	}
}

func TestPolicyLimitsLegacyExecDurationAndOutput(t *testing.T) {
	original := agentPolicy
	agentPolicy = execPolicy{timeout: 20 * time.Millisecond, maxOutput: 8, deniedEnv: map[string]struct{}{}}
	defer func() { agentPolicy = original }()

	conn := &memoryConn{reader: strings.NewReader(`{"type":"exec","args":["sh","-c","sleep 1; printf 1234567890"]}` + "\n")}
	handleConn(conn)
	var response execResponse
	if err := json.Unmarshal(conn.writer.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.OK || response.ExitCode != 124 || !strings.Contains(response.Error, "EXEC_TIMEOUT") {
		t.Fatalf("response = %+v", response)
	}
}

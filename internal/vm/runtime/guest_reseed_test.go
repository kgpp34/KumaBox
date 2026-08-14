package runtime

import (
	"strings"
	"testing"

	agentclient "github.com/kumabox/kumabox/internal/agent/client"
)

func TestRequireAgentCapability(t *testing.T) {
	tests := []struct {
		name    string
		pong    *agentclient.PingPongResponse
		wantErr string
	}{
		{
			name: "supported",
			pong: &agentclient.PingPongResponse{Capabilities: []string{"reseed"}},
		},
		{
			name:    "missing",
			pong:    &agentclient.PingPongResponse{Version: "0.3.2", Capabilities: []string{"identity"}},
			wantErr: "0.3.2 does not advertise \"reseed\"",
		},
		{
			name:    "empty response",
			wantErr: "response is empty",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := requireAgentCapability(test.pong, agentclient.CapabilityReseed)
			if test.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

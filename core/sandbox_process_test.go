package core

import (
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/kumabox/kumabox/agent"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

func TestSpawnResolvesExactGenerationAndForwardsCommandOptions(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	_, err := service.Create(t.Context(), CreateSandboxRequest{ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage}})
	if err != nil {
		t.Fatal(err)
	}
	catalog := service.dependencies.catalog.(*fakeCatalog)
	catalog.record.State, catalog.record.Generation = types.SandboxStateRunning, 4
	runtimeAdapter := testRuntime(t, service)
	runtimeAdapter.observation = vmm.Observation{State: vmm.ProcessRunning, Process: vmm.Process{PID: 42, Generation: 3}}
	host, guest := net.Pipe()
	runtimeAdapter.vsock = host
	t.Cleanup(func() { _ = guest.Close() })
	done := make(chan agent.Message, 1)
	id := strings.Repeat("a", 32)
	go func() {
		request, _ := agent.NewDecoder(guest).Decode()
		done <- request
		_ = agent.NewEncoder(guest).Encode(agent.Message{Type: agent.MessageDone, Process: &types.ProcessInfo{ID: id, PID: 123, Running: true}})
	}()
	*steps = nil
	info, err := service.Spawn(t.Context(), "box", types.Command{Args: []string{"sleep", "30"}, Cwd: "/work", User: "worker"})
	if err != nil || info.ID != id {
		t.Fatalf("spawn = %+v, %v", info, err)
	}
	request := <-done
	if request.Type != agent.MessageSpawn || request.Cwd != "/work" || request.User != "worker" {
		t.Fatalf("spawn frame = %+v", request)
	}
	if got := strings.Join(*steps, ","); got != "resolve,resolve,locate,vsock" {
		t.Fatalf("spawn held wrong runtime path: %s", got)
	}
}

func TestProcessRequestsValidateBeforeAccessingRuntime(t *testing.T) {
	var service *SandboxService
	for _, test := range []struct {
		name, id string
		signal   types.ProcessSignal
	}{
		{name: "raw PID", id: "123", signal: types.ProcessSignalKILL},
		{name: "unsupported signal", id: strings.Repeat("a", 32), signal: "STOP"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := service.SignalProcess(t.Context(), "box", test.id, test.signal)
			var classified *errdefs.Error
			if !errors.As(err, &classified) || classified.Code != errdefs.CodeInvalidArgument {
				t.Fatalf("validation = %v", err)
			}
		})
	}
}

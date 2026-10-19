package core

import (
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/kumabox/kumabox/agent"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

func TestGuestFileRequestsValidateBeforeAccessingRuntime(t *testing.T) {
	var service *SandboxService
	mode := uint32(0o1000)
	for _, run := range []func() error{
		func() error { return service.ReadFile(t.Context(), "box", "relative", io.Discard) },
		func() error { return service.RemoveFile(t.Context(), "box", "/", true) },
		func() error { return service.RenameFile(t.Context(), "box", "/tmp/file", "/") },
		func() error {
			return service.WriteFile(t.Context(), "box", "/file", types.FileWriteOptions{Mode: &mode}, nil)
		},
	} {
		var classified *errdefs.Error
		if err := run(); !errors.As(err, &classified) || classified.Code != errdefs.CodeInvalidArgument {
			t.Fatalf("validation = %v", err)
		}
	}
}

func TestForwardPortValidationAndRuntimeFailureCloseLocalConnection(t *testing.T) {
	var service *SandboxService
	if err := service.ForwardPort(t.Context(), "box", 0, nil); err == nil {
		t.Fatal("zero port accepted")
	}
	local, peer := net.Pipe()
	defer func() { _ = peer.Close() }()
	if err := service.ForwardPort(t.Context(), "box", 80, local); err == nil {
		t.Fatal("unconfigured runtime accepted")
	}
	if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("local stream was not closed after runtime failure: %v", err)
	}
}

func TestGuestFileResolvesLiveGenerationAndMapsRemoteError(t *testing.T) {
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
	request := make(chan agent.Message, 1)
	go func() {
		message, _ := agent.NewDecoder(guest).Decode()
		request <- message
		_ = agent.NewEncoder(guest).Encode(agent.Message{Type: agent.MessageError, Code: "NOT_FOUND", Message: "missing file"})
	}()
	*steps = nil
	err = service.ReadFile(t.Context(), "box", "/missing", io.Discard)
	var classified *errdefs.Error
	if !errors.As(err, &classified) || classified.Code != errdefs.CodeNotFound {
		t.Fatalf("remote classification = %v", err)
	}
	message := <-request
	if message.Type != agent.MessageFileRead || message.Path != "/missing" || strings.Join(*steps, ",") != "resolve,resolve,locate,vsock" {
		t.Fatalf("wrong guest routing: %+v, %v", message, *steps)
	}
}

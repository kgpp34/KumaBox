package core

import (
	"bytes"
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/kumabox/kumabox/agent"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

func TestSandboxLifecycleRoutesToPersistedVMM(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	firecracker := &fakeRuntime{typ: types.VMMFirecracker, steps: steps, observation: vmm.Observation{State: vmm.ProcessAbsent}}
	runtimes, err := vmm.NewRegistry(testRuntime(t, service), firecracker)
	if err != nil {
		t.Fatal(err)
	}
	service.dependencies.runtimes = runtimes
	record, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo",
		Config:         types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
		VMM:            types.VMMFirecracker,
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.VMM != types.VMMFirecracker {
		t.Fatalf("VMM = %q, want %q", record.VMM, types.VMMFirecracker)
	}
	*steps = nil
	if _, err := service.Start(t.Context(), "box"); err != nil {
		t.Fatal(err)
	}
	if firecracker.plan.SandboxID != fixedID {
		t.Fatalf("Firecracker did not receive launch plan: %+v", firecracker.plan)
	}
	if got := strings.Join(*steps, ","); !strings.Contains(got, "status:launching firecracker,launch") {
		t.Fatalf("start was not routed through Firecracker: %v", *steps)
	}
}

func TestStartCommitsRunningOnlyAfterLaunchReadiness(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); err != nil {
		t.Fatal(err)
	}
	*steps = nil
	record, err := service.Start(t.Context(), "box")
	if err != nil {
		t.Fatal(err)
	}
	if record.State != types.SandboxStateRunning || record.Generation != 4 {
		t.Fatalf("running record = %+v", record)
	}
	runtimeAdapter := testRuntime(t, service)
	if runtimeAdapter.plan.Generation != 3 || len(runtimeAdapter.plan.Disks) != 2 || runtimeAdapter.plan.Disks[0].Serial != "kumabox-layer0" || runtimeAdapter.plan.Disks[1].Serial != vmm.COWSerial {
		t.Fatalf("launch plan = %+v", runtimeAdapter.plan)
	}
	want := []string{
		"status:resolving sandbox", "resolve", "status:waiting for sandbox operation lock", "resolve",
		"status:checking existing runtime", "observe", "cleanup", "status:checking host runtime", "preflight",
		"status:verifying image and sandbox disk", "verify", "check", "status:committing starting state", "starting",
		"status:launching cloud-hypervisor", "launch", "status:committing running state", "running", "report",
	}
	if !reflect.DeepEqual(*steps, want) {
		t.Fatalf("steps = %v, want %v", *steps, want)
	}
}

func TestStartRecoversNetworkBeforeLaunchingInItsNamespace(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo",
		Config: types.SandboxConfig{
			Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory,
			Storage: types.DefaultSandboxStorage, NICs: 1,
		},
	}); err != nil {
		t.Fatal(err)
	}
	*steps = nil
	record, err := service.Start(t.Context(), "box")
	if err != nil {
		t.Fatal(err)
	}
	if record.State != types.SandboxStateRunning {
		t.Fatalf("started record = %+v", record)
	}
	plan := testRuntime(t, service).plan
	if plan.Network.Namespace != "/var/run/netns/kumabox-test" || len(plan.Network.Interfaces) != 1 {
		t.Fatalf("launch network = %+v", plan.Network)
	}
	if got := *steps; !reflect.DeepEqual(got, []string{
		"status:resolving sandbox", "resolve", "status:waiting for sandbox operation lock", "resolve",
		"status:checking existing runtime", "observe", "cleanup",
		"status:checking host runtime", "preflight",
		"status:verifying image and sandbox disk", "verify", "check",
		"status:committing starting state", "starting",
		"status:recovering sandbox network", "network-recover",
		"status:launching cloud-hypervisor", "launch",
		"status:committing running state", "running", "report",
	}) {
		t.Fatalf("Start steps = %v", got)
	}
}

func TestStartNetworkRecoveryFailureRetainsErrorAndQuiesces(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo",
		Config: types.SandboxConfig{
			Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory,
			Storage: types.DefaultSandboxStorage, NICs: 1,
		},
	}); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("network recovery failed")
	testNetwork(t, service).recoverErr = failure
	*steps = nil
	if _, err := service.Start(t.Context(), "box"); !errors.Is(err, failure) {
		t.Fatalf("Start error = %v", err)
	}
	record := service.dependencies.catalog.(*fakeCatalog).record
	if record.State != types.SandboxStateError || record.Failure == nil || record.Failure.Phase != "recover network" {
		t.Fatalf("failed start record = %+v", record)
	}
	if got := strings.Join(*steps, ","); !strings.Contains(got,
		"starting,status:recovering sandbox network,network-recover,cleanup,status:quiescing sandbox network,network-quiesce,start-error") {
		t.Fatalf("recovery compensation steps = %v", *steps)
	}
}

func TestStartRejectsRecoveredNetworkIdentityDrift(t *testing.T) {
	service, _ := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo",
		Config: types.SandboxConfig{
			Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory,
			Storage: types.DefaultSandboxStorage, NICs: 1,
		},
	}); err != nil {
		t.Fatal(err)
	}
	networkAdapter := testNetwork(t, service)
	networkAdapter.recovered = append([]types.NetworkInterface(nil), service.dependencies.catalog.(*fakeCatalog).record.Network.Interfaces...)
	networkAdapter.recovered[0].MAC = "02:00:00:00:00:fe"
	if _, err := service.Start(t.Context(), "box"); err == nil {
		t.Fatal("Start accepted a recovered network with changed guest identity")
	} else if code, ok := errdefs.CodeOf(err); !ok || code != errdefs.CodeArtifactCorrupt {
		t.Fatalf("Start error = %v, want %s", err, errdefs.CodeArtifactCorrupt)
	}
}

func TestStartRecoversRunningProcessFromStartingState(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); err != nil {
		t.Fatal(err)
	}
	catalog := service.dependencies.catalog.(*fakeCatalog)
	catalog.record.State, catalog.record.Generation = types.SandboxStateStarting, 3
	runtimeAdapter := testRuntime(t, service)
	runtimeAdapter.observation = vmm.Observation{State: vmm.ProcessRunning, Process: vmm.Process{PID: 42}}
	*steps = nil
	record, err := service.Start(t.Context(), "box")
	if err != nil {
		t.Fatal(err)
	}
	if record.State != types.SandboxStateRunning || record.Generation != 4 {
		t.Fatalf("recovered record = %+v", record)
	}
	if strings.Contains(strings.Join(*steps, ","), "launch") || strings.Contains(strings.Join(*steps, ","), "preflight") {
		t.Fatalf("recovery relaunched VMM: %v", *steps)
	}
}

func TestStartFailureAbortsProcessAndRetainsError(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("VMM exited")
	testRuntime(t, service).launchErr = failure
	*steps = nil
	if _, err := service.Start(t.Context(), "box"); !errors.Is(err, failure) {
		t.Fatalf("Start error = %v", err)
	} else {
		var classified *errdefs.Error
		if !errors.As(err, &classified) || !classified.Committed {
			t.Fatalf("Start did not report retained state: %v", err)
		}
	}
	catalog := service.dependencies.catalog.(*fakeCatalog)
	if catalog.record.State != types.SandboxStateError || catalog.record.Failure == nil || catalog.record.Failure.Phase != "launch VMM" {
		t.Fatalf("failed start record = %+v", catalog.record)
	}
	joined := strings.Join(*steps, ",")
	if !strings.Contains(joined, "launch,abort,start-error") {
		t.Fatalf("process was not aborted before Error commit: %v", *steps)
	}
}

func TestStartRetryDoesNotLeaveStartingAfterPreflightFailure(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); err != nil {
		t.Fatal(err)
	}
	catalog := service.dependencies.catalog.(*fakeCatalog)
	catalog.record.State, catalog.record.Generation = types.SandboxStateStarting, 3
	failure := errors.New("KVM unavailable")
	testRuntime(t, service).preflightErr = failure
	*steps = nil
	if _, err := service.Start(t.Context(), "box"); !errors.Is(err, failure) {
		t.Fatalf("Start error = %v", err)
	}
	if catalog.record.State != types.SandboxStateError || catalog.record.Failure == nil || catalog.record.Failure.Phase != "host preflight" {
		t.Fatalf("failed recovery record = %+v", catalog.record)
	}
	if got := strings.Join(*steps, ","); !strings.Contains(got, "observe,cleanup,status:checking host runtime,preflight,cleanup,start-error") {
		t.Fatalf("recovery steps = %v", *steps)
	}
}

func TestStopRecordsIntentBeforeTerminatingRunningVMM(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); err != nil {
		t.Fatal(err)
	}
	catalog := service.dependencies.catalog.(*fakeCatalog)
	catalog.record.State, catalog.record.Generation = types.SandboxStateRunning, 4
	testRuntime(t, service).observation = vmm.Observation{State: vmm.ProcessRunning, Process: vmm.Process{PID: 42}}
	*steps = nil
	record, err := service.Stop(t.Context(), "box")
	if err != nil {
		t.Fatal(err)
	}
	if record.State != types.SandboxStateStopped || record.Generation != 6 {
		t.Fatalf("stopped record = %+v", record)
	}
	want := []string{
		"status:resolving sandbox", "resolve", "status:waiting for sandbox operation lock", "resolve",
		"status:checking existing runtime", "locate", "status:committing stopping state", "stopping",
		"status:stopping cloud-hypervisor", "stop", "status:cleaning runtime state", "cleanup",
		"status:committing stopped state", "stopped", "report",
	}
	if !reflect.DeepEqual(*steps, want) {
		t.Fatalf("steps = %v, want %v", *steps, want)
	}
}

func TestStopQuiescesNetworkAfterRuntimeCleanup(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo",
		Config: types.SandboxConfig{
			Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory,
			Storage: types.DefaultSandboxStorage, NICs: 1,
		},
	}); err != nil {
		t.Fatal(err)
	}
	catalog := service.dependencies.catalog.(*fakeCatalog)
	catalog.record.State, catalog.record.Generation = types.SandboxStateRunning, 4
	runtimeAdapter := testRuntime(t, service)
	runtimeAdapter.observation = vmm.Observation{State: vmm.ProcessRunning, Process: vmm.Process{PID: 42}}
	*steps = nil
	if _, err := service.Stop(t.Context(), "box"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*steps, ","); !strings.Contains(got,
		"stop,status:cleaning runtime state,cleanup,status:quiescing sandbox network,network-quiesce,status:committing stopped state,stopped") {
		t.Fatalf("network stop ordering = %v", *steps)
	}
}

func TestStopRetriesNetworkQuiesceFromStoppingState(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo",
		Config: types.SandboxConfig{
			Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory,
			Storage: types.DefaultSandboxStorage, NICs: 1,
		},
	}); err != nil {
		t.Fatal(err)
	}
	catalog := service.dependencies.catalog.(*fakeCatalog)
	catalog.record.State, catalog.record.Generation = types.SandboxStateRunning, 4
	runtimeAdapter := testRuntime(t, service)
	runtimeAdapter.observation = vmm.Observation{State: vmm.ProcessRunning, Process: vmm.Process{PID: 42}}
	networkAdapter := testNetwork(t, service)
	failure := errors.New("link state failed")
	networkAdapter.quiesceErr = failure
	*steps = nil
	if _, err := service.Stop(t.Context(), "box"); !errors.Is(err, failure) {
		t.Fatalf("Stop error = %v", err)
	}
	if catalog.record.State != types.SandboxStateStopping || catalog.record.Generation != 5 {
		t.Fatalf("retained record = %+v", catalog.record)
	}

	networkAdapter.quiesceErr = nil
	runtimeAdapter.observation = vmm.Observation{State: vmm.ProcessAbsent}
	*steps = nil
	record, err := service.Stop(t.Context(), "box")
	if err != nil {
		t.Fatal(err)
	}
	if record.State != types.SandboxStateStopped || record.Generation != 6 {
		t.Fatalf("retried stop record = %+v", record)
	}
	if got := strings.Join(*steps, ","); strings.Contains(got, ",stop,") || !strings.Contains(got,
		"cleanup,status:quiescing sandbox network,network-quiesce,status:committing stopped state,stopped") {
		t.Fatalf("retried stop steps = %v", *steps)
	}
}

func TestStopResumesStoppingAndRecoversStarting(t *testing.T) {
	for _, test := range []struct {
		name       string
		state      types.SandboxState
		generation uint64
		want       uint64
	}{
		{name: "stopping", state: types.SandboxStateStopping, generation: 5, want: 6},
		{name: "starting", state: types.SandboxStateStarting, generation: 3, want: 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, steps := newTestSandboxService(t, nil)
			if _, err := service.Create(t.Context(), CreateSandboxRequest{
				ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
			}); err != nil {
				t.Fatal(err)
			}
			catalog := service.dependencies.catalog.(*fakeCatalog)
			catalog.record.State, catalog.record.Generation = test.state, test.generation
			testRuntime(t, service).observation = vmm.Observation{State: vmm.ProcessStarting, Process: vmm.Process{PID: 42}}
			*steps = nil
			record, err := service.Stop(t.Context(), "box")
			if err != nil {
				t.Fatal(err)
			}
			if record.State != types.SandboxStateStopped || record.Generation != test.want {
				t.Fatalf("stopped record = %+v", record)
			}
			if got := strings.Join(*steps, ","); strings.Contains(got, ",stopping,") || !strings.Contains(got, "locate,status:stopping cloud-hypervisor,stop,status:cleaning runtime state,cleanup") {
				t.Fatalf("recovery steps = %v", *steps)
			}
		})
	}
}

func TestStopConvergesAbsentRunningWithoutSignalling(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); err != nil {
		t.Fatal(err)
	}
	catalog := service.dependencies.catalog.(*fakeCatalog)
	catalog.record.State, catalog.record.Generation = types.SandboxStateRunning, 4
	*steps = nil
	record, err := service.Stop(t.Context(), "box")
	if err != nil {
		t.Fatal(err)
	}
	if record.State != types.SandboxStateStopped || record.Generation != 5 {
		t.Fatalf("stopped record = %+v", record)
	}
	if got := strings.Join(*steps, ","); strings.Contains(got, ",stop,") || strings.Contains(got, ",stopping,") {
		t.Fatalf("absent VMM was signalled or marked Stopping: %v", *steps)
	}
}

func TestStopFailureRetainsRetryableStoppingState(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); err != nil {
		t.Fatal(err)
	}
	catalog := service.dependencies.catalog.(*fakeCatalog)
	catalog.record.State, catalog.record.Generation = types.SandboxStateRunning, 4
	failure := errors.New("signal failed")
	runtimeAdapter := testRuntime(t, service)
	runtimeAdapter.observation = vmm.Observation{State: vmm.ProcessRunning, Process: vmm.Process{PID: 42}}
	runtimeAdapter.stopErr = failure
	*steps = nil
	if _, err := service.Stop(t.Context(), "box"); !errors.Is(err, failure) {
		t.Fatalf("Stop error = %v", err)
	}
	if catalog.record.State != types.SandboxStateStopping || catalog.record.Generation != 5 {
		t.Fatalf("retained record = %+v", catalog.record)
	}
	if strings.Contains(strings.Join(*steps, ","), "cleanup") {
		t.Fatalf("runtime was cleaned before process absence: %v", *steps)
	}
}

func TestStopCreatedIsIdempotentAndPreservesCreated(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	created, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	})
	if err != nil {
		t.Fatal(err)
	}
	*steps = nil
	record, err := service.Stop(t.Context(), "box")
	if err != nil {
		t.Fatal(err)
	}
	if record.State != types.SandboxStateCreated || record.Generation != created.Generation {
		t.Fatalf("idempotent stop changed created record = %+v", record)
	}
	if got := strings.Join(*steps, ","); !strings.Contains(got, "status:cleaning stale runtime state,cleanup,report") {
		t.Fatalf("idempotent steps = %v", *steps)
	}
}

func TestConsoleOpensExactRunningGenerationWithoutHoldingOperationLock(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); err != nil {
		t.Fatal(err)
	}
	catalog := service.dependencies.catalog.(*fakeCatalog)
	catalog.record.State, catalog.record.Generation = types.SandboxStateRunning, 4
	runtimeAdapter := testRuntime(t, service)
	runtimeAdapter.observation = vmm.Observation{State: vmm.ProcessRunning, Process: vmm.Process{PID: 42, Generation: 3}}
	*steps = nil

	connection, err := service.Console(t.Context(), "box")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*steps, ","); got != "resolve,resolve,locate,console" {
		t.Fatalf("console steps = %q", got)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if !runtimeAdapter.console.(*fakeConsole).closed {
		t.Fatal("caller did not own the returned console")
	}
}

func TestConsoleRejectsNonRunningSandboxBeforeRuntimeAccess(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); err != nil {
		t.Fatal(err)
	}
	*steps = nil
	if _, err := service.Console(t.Context(), "box"); err == nil {
		t.Fatal("Console succeeded for Created sandbox")
	} else if code, ok := errdefs.CodeOf(err); !ok || code != errdefs.CodeStateConflict {
		t.Fatalf("Console error = %v", err)
	}
	if got := strings.Join(*steps, ","); got != "resolve,resolve" {
		t.Fatalf("non-running console touched runtime: %q", got)
	}
}

func TestLogsRoutesPersistedBackendForInactiveSandbox(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); err != nil {
		t.Fatal(err)
	}
	runtimeAdapter := testRuntime(t, service)
	runtimeAdapter.logs = "boot output\n"
	*steps = nil
	var output bytes.Buffer
	options := SandboxLogOptions{Tail: 12, Follow: true}
	if err := service.Logs(t.Context(), "box", options, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != runtimeAdapter.logs || runtimeAdapter.logOptions != (vmm.LogOptions{Tail: 12, Follow: true}) {
		t.Fatalf("log output/options = %q, %+v", output.String(), runtimeAdapter.logOptions)
	}
	if got := strings.Join(*steps, ","); got != "resolve,logs" {
		t.Fatalf("logs steps = %q", got)
	}
}

func TestLogsRejectsNegativeTailBeforeResolvingSandbox(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	*steps = nil
	if err := service.Logs(t.Context(), "box", SandboxLogOptions{Tail: -1}, io.Discard); err == nil {
		t.Fatal("Logs accepted a negative tail")
	} else if code, ok := errdefs.CodeOf(err); !ok || code != errdefs.CodeInvalidArgument {
		t.Fatalf("Logs error = %v", err)
	}
	if len(*steps) != 0 {
		t.Fatalf("invalid logs request touched adapters: %v", *steps)
	}
}

func TestExecUsesExactRunningGenerationAndStreamsResult(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo",
		Config:         types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); err != nil {
		t.Fatal(err)
	}
	catalog := service.dependencies.catalog.(*fakeCatalog)
	catalog.record.State, catalog.record.Generation = types.SandboxStateRunning, 4
	runtimeAdapter := testRuntime(t, service)
	runtimeAdapter.observation = vmm.Observation{State: vmm.ProcessRunning, Process: vmm.Process{PID: 42, Generation: 3}}
	host, guest := net.Pipe()
	runtimeAdapter.vsock = host
	t.Cleanup(func() { _ = guest.Close() })
	go func() {
		decoder := agent.NewDecoder(guest)
		encoder := agent.NewEncoder(guest)
		request, err := decoder.Decode()
		if err != nil || request.Type != agent.MessageExec {
			return
		}
		_, _ = decoder.Decode()
		_ = encoder.Encode(agent.Message{Type: agent.MessageStarted, PID: 100})
		_ = encoder.Encode(agent.Message{Type: agent.MessageStdout, Data: []byte("out")})
		_ = encoder.Encode(agent.Message{Type: agent.MessageStderr, Data: []byte("err")})
		_ = encoder.Encode(agent.Message{Type: agent.MessageExit, ExitCode: 17})
	}()
	*steps = nil
	var stdout, stderr bytes.Buffer
	code, err := service.Exec(t.Context(), "box", types.Command{Args: []string{"demo"}}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if code != 17 || stdout.String() != "out" || stderr.String() != "err" {
		t.Fatalf("result: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if got := strings.Join(*steps, ","); got != "resolve,resolve,locate,vsock" {
		t.Fatalf("exec steps = %q", got)
	}
}

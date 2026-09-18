package cloudhypervisor

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

func TestQueryStateRequiresUnixSocketAndDecodesRunning(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "kumabox-ch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	socket := filepath.Join(directory, "api.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var shutdown atomic.Bool
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/vm.info":
			_, _ = writer.Write([]byte(`{"state":"Running","config":{"console":{"mode":"Pty","file":"/dev/pts/7"}}}`))
		case request.Method == http.MethodPut && request.URL.Path == "/api/v1/vm.shutdown":
			shutdown.Store(true)
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	driver := &Driver{}
	state, err := driver.queryState(t.Context(), socket)
	if err != nil {
		t.Fatal(err)
	}
	if state != "Running" {
		t.Fatalf("state = %q", state)
	}
	info, err := driver.queryInfo(t.Context(), socket)
	if err != nil {
		t.Fatal(err)
	}
	if info.Config.Console.Mode != "Pty" || info.Config.Console.File != "/dev/pts/7" {
		t.Fatalf("console info = %+v", info.Config.Console)
	}
	if err := driver.requestShutdown(t.Context(), socket); err != nil {
		t.Fatal(err)
	}
	if !shutdown.Load() {
		t.Fatal("vm.shutdown request was not received")
	}

	regular := filepath.Join(t.TempDir(), "not-a-socket")
	if err := os.WriteFile(regular, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := driver.queryState(t.Context(), regular); err == nil {
		t.Fatal("accepted a regular file as the VMM API socket")
	} else if code, ok := errdefs.CodeOf(err); !ok || code != errdefs.CodeArtifactCorrupt {
		t.Fatalf("regular socket error = %v", err)
	}
}

func TestOpenConsolePTYRejectsUnmanagedOrRegularPaths(t *testing.T) {
	regular := filepath.Join(t.TempDir(), "7")
	if err := os.WriteFile(regular, []byte("not a PTY"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{regular, "/tmp/7", "/dev/pts/not-a-number", "dev/pts/7"} {
		if _, err := openConsolePTY(path); err == nil {
			t.Fatalf("accepted invalid console path %q", path)
		} else if code, ok := errdefs.CodeOf(err); !ok || code != errdefs.CodeArtifactCorrupt {
			t.Fatalf("console path %q error = %v", path, err)
		}
	}
}

func TestBuildArgsPreservesDiskOrderAndAccessMode(t *testing.T) {
	plan := vmm.LaunchPlan{
		SandboxID: "123e4567-e89b-42d3-a456-426614174000", Generation: 3,
		CPUs: 2, Memory: 1 << 30, BootProfile: types.BootProfileOverlayV1,
		Kernel: "/boot/vmlinuz", Initrd: "/boot/initrd.img", Cmdline: "boot=kumabox-overlay",
		Disks: []vmm.Disk{
			{Path: "/layers/0.erofs", Serial: "kumabox-layer0", ReadOnly: true},
			{Path: "/layers/1.erofs", Serial: "kumabox-layer1", ReadOnly: true},
			{Path: "/sandbox/cow.raw", Serial: vmm.COWSerial},
		},
	}
	args := buildArgs(plan, "/run/api.sock", "/run/vsock.uds")
	diskIndex := slices.Index(args, "--disk")
	if diskIndex < 0 || diskIndex+3 >= len(args) {
		t.Fatalf("disk arguments missing: %v", args)
	}
	disks := args[diskIndex+1 : diskIndex+4]
	if !strings.Contains(disks[0], "serial=kumabox-layer0") || !strings.Contains(disks[0], "readonly=on") ||
		!strings.Contains(disks[1], "serial=kumabox-layer1") || !strings.Contains(disks[1], "readonly=on") ||
		!strings.Contains(disks[2], "serial=kumabox-cow") || !strings.Contains(disks[2], "direct=on") || !strings.Contains(disks[2], "sparse=on") {
		t.Fatalf("disk arguments = %v", disks)
	}
	if slices.Index(args, "--kernel") < diskIndex+4 || slices.Index(args, "--initramfs") < 0 || slices.Index(args, "--vsock") < 0 {
		t.Fatalf("boot arguments = %v", args)
	}
}

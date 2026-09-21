package cloudhypervisor

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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

func TestBuildArgsMatchesDirectBootContract(t *testing.T) {
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
	want := []string{
		"--api-socket", "/run/api.sock",
		"--cpus", fmt.Sprintf("boot=2,max=%d", max(runtime.NumCPU(), 2)),
		"--memory", "size=1073741824",
		"--disk",
		"path=/layers/0.erofs,image_type=raw,num_queues=2,queue_size=512,serial=kumabox-layer0,readonly=on",
		"path=/layers/1.erofs,image_type=raw,num_queues=2,queue_size=512,serial=kumabox-layer1,readonly=on",
		"path=/sandbox/cow.raw,image_type=raw,num_queues=2,queue_size=512,serial=kumabox-cow,direct=on,sparse=on",
		"--kernel", "/boot/vmlinuz",
		"--initramfs", "/boot/initrd.img",
		"--cmdline", "boot=kumabox-overlay",
		"--rng", "src=/dev/urandom",
		"--watchdog",
		"--balloon", "size=268435456,deflate_on_oom=on,free_page_reporting=on",
		"--vsock", "cid=3,socket=/run/vsock.uds",
		"--serial", "off",
		"--console", "pty",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("buildArgs() =\n%q\nwant\n%q", args, want)
	}
}

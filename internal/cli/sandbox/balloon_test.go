package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/errdefs"
	"github.com/kumabox/kumabox/internal/rpc"
	"github.com/kumabox/kumabox/internal/storage"
	"github.com/kumabox/kumabox/internal/types"
)

func TestBalloonCommandSubmitsTargetAndFormatsAcknowledgement(t *testing.T) {
	cfg := config.Default()
	cfg.Client.Socket = filepath.Join(shortTempDir(t), "node.sock")
	requests := make(chan rpc.Target[types.BalloonTarget], 2)
	mux := http.NewServeMux()
	rpc.Register(mux, "sandbox.balloon", func(_ context.Context, _ *rpc.Session, request rpc.Target[types.BalloonTarget]) (any, error) {
		requests <- request
		size, err := request.Options.Resolve(1 << 30)
		return types.BalloonStatus{Target: size, Default: 256 << 20}, err
	})
	listener, err := net.Listen("unix", cfg.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	server := &http.Server{Handler: mux, Protocols: protocols, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	for _, test := range []struct {
		name   string
		args   []string
		asJSON bool
	}{
		{name: "percent JSON", args: []string{"box", "25%", "--json"}, asJSON: true},
		{name: "deflate text", args: []string{"box", "0"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := NewBalloonCommand(func() config.Config { return cfg })
			command.SetArgs(test.args)
			var output bytes.Buffer
			command.SetOut(&output)
			if err := command.ExecuteContext(t.Context()); err != nil {
				t.Fatal(err)
			}
			request := <-requests
			if request.Reference != "box" {
				t.Fatalf("request = %+v", request)
			}
			if test.asJSON {
				var status types.BalloonStatus
				if err := json.Unmarshal(output.Bytes(), &status); err != nil {
					t.Fatal(err)
				}
				if request.Options.Percent == nil || *request.Options.Percent != 25 || status.Target != 256<<20 ||
					!strings.Contains(output.String(), "\n  \"target\":") {
					t.Fatalf("JSON acknowledgement = %s, request = %+v", output.String(), request)
				}
				return
			}
			if request.Options.Percent != nil || request.Options.Size != 0 || !strings.Contains(output.String(), "0 bytes") {
				t.Fatalf("deflate acknowledgement = %s, request = %+v", output.String(), request)
			}
		})
	}
}

func TestBalloonRejectsInvalidTargetBeforeOpeningService(t *testing.T) {
	for _, args := range [][]string{{}, {"box"}, {"box", "-1%"}, {"box", "100%"}, {"box", "garbage"}} {
		command := NewBalloonCommand(func() config.Config {
			t.Fatal("invalid target opened the service")
			return config.Config{}
		})
		command.SetArgs(args)
		command.SilenceUsage = true
		command.SilenceErrors = true
		if err := command.ExecuteContext(t.Context()); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestBalloonCommandReachesDaemonAndRejectsStoppedSandbox(t *testing.T) {
	base := shortTempDir(t)
	roots := storage.Roots{Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log")}
	seedImage(t, roots)
	installFakeMKFS(t, base)
	executeCreate(t, roots, "box")
	command := NewBalloonCommand(func() config.Config { return sandboxTestConfig(t, roots) })
	command.SetArgs([]string{"box", "25%", "--json"})
	command.SilenceUsage = true
	command.SilenceErrors = true
	err := command.ExecuteContext(t.Context())
	if code, _ := errdefs.CodeOf(err); code != errdefs.CodeStateConflict {
		t.Fatalf("balloon command error = %v, code = %s", err, code)
	}
}

func TestInspectProjectsBalloonReclamationAndOmitsDisabledDevice(t *testing.T) {
	actual := int64(128 << 20)
	status := types.SandboxStatus{Balloon: &types.BalloonStatus{Target: 256 << 20, Actual: &actual, Default: 256 << 20}}
	var output bytes.Buffer
	if err := writeStatusDetailJSON(&output, status, types.AttachedDevices{}); err != nil {
		t.Fatal(err)
	}
	var decoded statusOutput
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Balloon == nil || decoded.Balloon.Target != status.Balloon.Target || decoded.Balloon.Actual == nil || *decoded.Balloon.Actual != actual {
		t.Fatalf("inspect output = %s", output.String())
	}
	output.Reset()
	status.Balloon = nil
	if err := writeStatusDetailJSON(&output, status, types.AttachedDevices{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), `"balloon"`) {
		t.Fatalf("disabled device was published: %s", output.String())
	}
}

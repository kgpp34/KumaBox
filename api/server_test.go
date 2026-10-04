package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

const testToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type fakeSandboxes struct {
	create  func(CreateSandboxInput) (types.Sandbox, error)
	exec    func(types.Command, io.Writer, io.Writer) (int, error)
	record  types.Sandbox
	stops   int
	removes int
	input   []byte
}

func (f *fakeSandboxes) Create(_ context.Context, req CreateSandboxInput) (types.Sandbox, error) {
	return f.create(req)
}

func (f *fakeSandboxes) Run(_ context.Context, req CreateSandboxInput) (types.Sandbox, error) {
	return f.create(req)
}
func (*fakeSandboxes) List(context.Context, bool) ([]types.Sandbox, error) { return nil, nil }
func (f *fakeSandboxes) Inspect(context.Context, string) (types.Sandbox, error) {
	return f.record, nil
}

func (*fakeSandboxes) Start(context.Context, string) (types.Sandbox, error) {
	return types.Sandbox{}, nil
}

func (f *fakeSandboxes) Stop(context.Context, string) (types.Sandbox, error) {
	f.stops++
	return f.record, nil
}

func (f *fakeSandboxes) Remove(context.Context, string) (types.Sandbox, error) {
	f.removes++
	return f.record, nil
}

func (f *fakeSandboxes) Exec(_ context.Context, _ string, cmd types.Command, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if stdin != nil {
		f.input, _ = io.ReadAll(stdin)
	}
	return f.exec(cmd, stdout, stderr)
}

func TestNativeFileTransferPreservesBinaryContentAndGuestPath(t *testing.T) {
	guestPath := "/tmp/safe'; touch /tmp/unsafe"
	var commands []types.Command
	sandboxes := &fakeSandboxes{
		record: types.Sandbox{State: types.SandboxStateRunning},
		exec: func(cmd types.Command, stdout, _ io.Writer) (int, error) {
			commands = append(commands, cmd)
			if len(commands) == 3 {
				_, _ = stdout.Write([]byte{0, 1, 255})
			}
			return 0, nil
		},
	}
	handler := testHandler(t, sandboxes)
	urlPath := "/v1/sandboxes/test/files?path=" + url.QueryEscape(guestPath)
	write := httptest.NewRequest("POST", urlPath, strings.NewReader(string([]byte{0, 1, 255})))
	write.Header.Set("Authorization", "Bearer "+testToken)
	write.Header.Set("Content-Type", "application/octet-stream")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, write)
	if response.Code != http.StatusOK || string(sandboxes.input) != string([]byte{0, 1, 255}) {
		t.Fatalf("write = %d %s, input = %v", response.Code, response.Body.String(), sandboxes.input)
	}
	read := httptest.NewRequest("GET", urlPath, nil)
	read.Header.Set("Authorization", "Bearer "+testToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, read)
	if response.Code != http.StatusOK || string(response.Body.Bytes()) != string([]byte{0, 1, 255}) {
		t.Fatalf("read = %d, content = %v", response.Code, response.Body.Bytes())
	}
	for _, command := range commands {
		if command.Args[len(command.Args)-1] != guestPath {
			t.Fatalf("path was not isolated as final argument: %+v", command.Args)
		}
	}
}

type fakeSnapshots struct{}

func (*fakeSnapshots) Save(context.Context, SaveSnapshotInput) (types.Snapshot, error) {
	return types.Snapshot{}, nil
}

func (*fakeSnapshots) Hibernate(context.Context, SaveSnapshotInput) (types.Snapshot, error) {
	return types.Snapshot{}, nil
}
func (*fakeSnapshots) List(context.Context) ([]types.Snapshot, error) { return nil, nil }
func (*fakeSnapshots) Inspect(context.Context, string) (types.Snapshot, error) {
	return types.Snapshot{}, nil
}

func (*fakeSnapshots) Remove(context.Context, string) (types.Snapshot, error) {
	return types.Snapshot{}, nil
}

func (*fakeSnapshots) Clone(context.Context, string, string) (types.Sandbox, error) {
	return types.Sandbox{}, nil
}

func (*fakeSnapshots) Restore(context.Context, string, string) (types.Sandbox, error) {
	return types.Sandbox{}, nil
}

func testHandler(t *testing.T, sandboxes *fakeSandboxes) http.Handler {
	t.Helper()
	handler, err := NewHandler(Services{Sandboxes: sandboxes, Snapshots: &fakeSnapshots{}}, testToken)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func apiRequest(method, path, body, token string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func TestHandlerRequiresAuthentication(t *testing.T) {
	handler := testHandler(t, &fakeSandboxes{})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, apiRequest("GET", "/v1/sandboxes", "", ""))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, apiRequest("GET", "/v1/sandboxes", "", testToken))
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "[]" {
		t.Fatalf("authenticated list = %d %s", response.Code, response.Body.String())
	}
}

func TestCreatePreservesExplicitZeroAndDefaults(t *testing.T) {
	var received CreateSandboxInput
	handler := testHandler(t, &fakeSandboxes{create: func(req CreateSandboxInput) (types.Sandbox, error) {
		received = req
		return types.Sandbox{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Config: req.Config}, nil
	}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, apiRequest("POST", "/v1/sandboxes", `{"image":"ubuntu","name":"sdk-test","nics":0,"start":false}`, testToken))
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if received.Config.NICs != 0 || received.Config.CPUs != types.DefaultSandboxCPUs || received.Config.Memory != types.DefaultSandboxMemory {
		t.Fatalf("unexpected config: %+v", received.Config)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, apiRequest("POST", "/v1/sandboxes", `{"image":"ubuntu","unknown":true}`, testToken))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d", response.Code)
	}
}

func TestExecFramesRemainValidWithConcurrentWriters(t *testing.T) {
	handler := testHandler(t, &fakeSandboxes{exec: func(cmd types.Command, stdout, stderr io.Writer) (int, error) {
		if len(cmd.Args) != 2 || cmd.Args[1] != "ok" {
			t.Fatalf("command = %+v", cmd)
		}
		var group sync.WaitGroup
		for _, writer := range []io.Writer{stdout, stderr} {
			group.Add(1)
			go func(writer io.Writer) {
				defer group.Done()
				for range 20 {
					_, _ = writer.Write([]byte("data"))
				}
			}(writer)
		}
		group.Wait()
		return 7, nil
	}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, apiRequest("POST", "/v1/sandboxes/test/exec", `{"args":["echo","ok"]}`, testToken))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	decoder := json.NewDecoder(response.Body)
	count := 0
	for {
		var event ExecEvent
		if err := decoder.Decode(&event); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decode frame %d: %v", count, err)
		}
		count++
		if count == 41 && (event.ExitCode == nil || *event.ExitCode != 7) {
			t.Fatalf("terminal frame = %+v", event)
		}
	}
	if count != 41 {
		t.Fatalf("frames = %d, want 41", count)
	}
}

func TestDeleteStopsRunningSandboxBeforeRemoval(t *testing.T) {
	service := &fakeSandboxes{record: types.Sandbox{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", State: types.SandboxStateRunning}}
	handler := testHandler(t, service)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, apiRequest("DELETE", "/v1/sandboxes/test", "", testToken))
	if response.Code != http.StatusOK || service.stops != 1 || service.removes != 1 {
		t.Fatalf("delete = %d, stops = %d, removes = %d", response.Code, service.stops, service.removes)
	}
}

func TestCreateFailureReturnsRetainedSandboxID(t *testing.T) {
	const id = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	handler := testHandler(t, &fakeSandboxes{create: func(req CreateSandboxInput) (types.Sandbox, error) {
		return types.Sandbox{ID: id}, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, errors.New("guest did not boot"))
	}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, apiRequest("POST", "/v1/sandboxes", `{"image":"ubuntu","name":"failed"}`, testToken))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var failure ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Error.ResourceID != id {
		t.Fatalf("resource ID = %q, want %q", failure.Error.ResourceID, id)
	}
}

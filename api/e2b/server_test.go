package e2b

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kumabox/kumabox/api"
	"github.com/kumabox/kumabox/types"
)

const (
	testKey = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testID  = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
)

type sandboxService struct {
	created  api.CreateSandboxInput
	command  types.Command
	commands []types.Command
	stdin    []byte
	state    types.SandboxState
	removed  atomic.Bool
}

func TestPauseRestoreAndTimeoutSurviveAdapterRestart(t *testing.T) {
	dir := t.TempDir()
	sandboxes := &sandboxService{}
	snapshots := &snapshotService{owner: sandboxes}
	handler, err := NewHandler(sandboxes, snapshots, testKey, dir)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/v2/sandboxes", strings.NewReader(`{"templateID":"ubuntu","timeout":60}`))
	request.Header.Set("X-API-Key", testKey)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest("POST", "/sandboxes/"+testID+"/pause", nil)
	request.Header.Set("X-API-Key", testKey)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || sandboxes.state != types.SandboxStateStopped {
		t.Fatalf("pause = %d, state = %s: %s", response.Code, sandboxes.state, response.Body.String())
	}
	// A new handler must recover the pause snapshot and lease from API-owned state.
	handler, err = NewHandler(sandboxes, snapshots, testKey, dir)
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest("GET", "/sandboxes/"+testID, nil)
	request.Header.Set("X-API-Key", testKey)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"state":"paused"`) {
		t.Fatalf("inspect pause = %d: %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest("POST", "/v2/sandboxes/"+testID+"/connect", nil)
	request.Header.Set("X-API-Key", testKey)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || sandboxes.state != types.SandboxStateRunning {
		t.Fatalf("restore = %d, state = %s: %s", response.Code, sandboxes.state, response.Body.String())
	}
	request = httptest.NewRequest("POST", "/sandboxes/"+testID+"/timeout", strings.NewReader(`{"timeout":120}`))
	request.Header.Set("X-API-Key", testKey)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("timeout = %d: %s", response.Code, response.Body.String())
	}
	store, err := openLeaseStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	policy, exists, err := store.get(testID)
	if err != nil || !exists || policy.PausedSnapshot != "" || time.Until(policy.ExpiresAt) < 110*time.Second {
		t.Fatalf("restored lease = %+v, exists = %t, err = %v", policy, exists, err)
	}
}

func TestStoppedSandboxIsNotReportedAsPausedOrColdStarted(t *testing.T) {
	dir := t.TempDir()
	sandboxes := &sandboxService{}
	handler, err := NewHandler(sandboxes, &snapshotService{}, testKey, dir)
	if err != nil {
		t.Fatal(err)
	}
	create := httptest.NewRequest("POST", "/v2/sandboxes", strings.NewReader(`{"templateID":"ubuntu"}`))
	create.Header.Set("X-API-Key", testKey)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, create)
	if response.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", response.Code, response.Body.String())
	}
	sandboxes.state = types.SandboxStateStopped
	for _, operation := range []struct{ method, path string }{
		{"GET", "/sandboxes/" + testID},
		{"POST", "/v2/sandboxes/" + testID + "/connect"},
	} {
		request := httptest.NewRequest(operation.method, operation.path, nil)
		request.Header.Set("X-API-Key", testKey)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusConflict {
			t.Fatalf("%s %s = %d: %s", operation.method, operation.path, response.Code, response.Body.String())
		}
	}
}

func TestCommittedHibernateErrorRetainsPauseSnapshot(t *testing.T) {
	dir := t.TempDir()
	sandboxes := &sandboxService{}
	snapshots := &snapshotService{owner: sandboxes, hibernateError: errors.New("report failed after stop")}
	handler, err := NewHandler(sandboxes, snapshots, testKey, dir)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/v2/sandboxes", strings.NewReader(`{"templateID":"ubuntu"}`))
	request.Header.Set("X-API-Key", testKey)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest("POST", "/sandboxes/"+testID+"/pause", nil)
	request.Header.Set("X-API-Key", testKey)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code == http.StatusNoContent {
		t.Fatal("committed hibernate error was hidden")
	}
	store, err := openLeaseStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	policy, exists, err := store.get(testID)
	if err != nil || !exists || policy.PausedSnapshot == "" {
		t.Fatalf("committed pause lease = %+v, exists = %t, err = %v", policy, exists, err)
	}
}

func TestExpiredSandboxIsRemoved(t *testing.T) {
	dir := t.TempDir()
	sandboxes := &sandboxService{}
	handler, err := NewHandler(sandboxes, &snapshotService{}, testKey, dir)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/v2/sandboxes", strings.NewReader(`{"templateID":"ubuntu","timeout":1}`))
	request.Header.Set("X-API-Key", testKey)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", response.Code, response.Body.String())
	}
	deadline := time.Now().Add(3 * time.Second)
	for !sandboxes.removed.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !sandboxes.removed.Load() {
		t.Fatal("sandbox was not removed after timeout")
	}
}

func TestExpiredAutoPauseRetainsSandboxAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	sandboxes := &sandboxService{}
	snapshots := &snapshotService{owner: sandboxes}
	handler, err := NewHandler(sandboxes, snapshots, testKey, dir)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/v2/sandboxes", strings.NewReader(`{"templateID":"ubuntu","timeout":1,"autoPause":true}`))
	request.Header.Set("X-API-Key", testKey)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", response.Code, response.Body.String())
	}
	store, err := openLeaseStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		policy, exists, err := store.get(testID)
		if err != nil {
			t.Fatal(err)
		}
		if exists && policy.PausedSnapshot != "" {
			if sandboxes.removed.Load() || !policy.ExpiresAt.IsZero() {
				t.Fatalf("auto pause lease = %+v, removed = %t", policy, sandboxes.removed.Load())
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("sandbox was not auto-paused after timeout")
}

func TestFileTransportUsesGuestPathArgumentAndBinaryStdin(t *testing.T) {
	dir := t.TempDir()
	sandboxes := &sandboxService{}
	handler, err := NewHandler(sandboxes, &snapshotService{}, testKey, dir)
	if err != nil {
		t.Fatal(err)
	}
	create := httptest.NewRequest("POST", "/v2/sandboxes", strings.NewReader(`{"templateID":"ubuntu"}`))
	create.Header.Set("X-API-Key", testKey)
	created := httptest.NewRecorder()
	handler.ServeHTTP(created, create)
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", created.Code, created.Body.String())
	}
	var credentials struct {
		EnvdAccessToken string `json:"envdAccessToken"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &credentials); err != nil {
		t.Fatal(err)
	}
	path := "/tmp/a'; touch /tmp/unwanted"
	request := httptest.NewRequest("POST", "/files?path="+url.QueryEscape(path), bytes.NewReader([]byte{0, 1, 2, 255}))
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("E2b-Sandbox-Id", testID)
	request.Header.Set("X-Access-Token", credentials.EnvdAccessToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !bytes.Equal(sandboxes.stdin, []byte{0, 1, 2, 255}) {
		t.Fatalf("write = %d %s, stdin = %v", response.Code, response.Body.String(), sandboxes.stdin)
	}
	if got := sandboxes.command.Args[len(sandboxes.command.Args)-1]; got != path {
		t.Fatalf("guest path argument = %q", got)
	}
	request = httptest.NewRequest("GET", "/files?path="+url.QueryEscape(path), nil)
	request.Header.Set("E2b-Sandbox-Id", testID)
	request.Header.Set("X-Access-Token", credentials.EnvdAccessToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "hello" {
		t.Fatalf("read = %d, body = %q", response.Code, response.Body.String())
	}
}

type snapshotService struct {
	cloned         string
	name           string
	owner          *sandboxService
	hibernateError error
}

func (*snapshotService) Save(_ context.Context, req api.SaveSnapshotInput) (types.Snapshot, error) {
	return types.Snapshot{ID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", Name: req.Name, SandboxID: testID}, nil
}

func (s *snapshotService) Hibernate(_ context.Context, req api.SaveSnapshotInput) (types.Snapshot, error) {
	if s.owner != nil {
		s.owner.state = types.SandboxStateStopped
	}
	return types.Snapshot{ID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", SandboxID: testID, Description: req.Description}, s.hibernateError
}

func (s *snapshotService) Restore(_ context.Context, _, _ string) (types.Sandbox, error) {
	if s.owner != nil {
		s.owner.state = types.SandboxStateRunning
	}
	return types.Sandbox{ID: testID, State: types.SandboxStateRunning}, nil
}
func (*snapshotService) List(context.Context) ([]types.Snapshot, error) { return nil, nil }
func (*snapshotService) Remove(context.Context, string) (types.Snapshot, error) {
	return types.Snapshot{}, nil
}

func (s *snapshotService) Clone(_ context.Context, ref, name string) (types.Sandbox, error) {
	s.cloned, s.name = ref, name
	return types.Sandbox{ID: testID, State: types.SandboxStateRunning}, nil
}

func (s *sandboxService) Run(_ context.Context, req api.CreateSandboxInput) (types.Sandbox, error) {
	s.created = req
	s.state = types.SandboxStateRunning
	return types.Sandbox{ID: testID, Config: req.Config, State: types.SandboxStateRunning}, nil
}

func (s *sandboxService) Inspect(context.Context, string) (types.Sandbox, error) {
	state := s.state
	if state == "" {
		state = types.SandboxStateRunning
	}
	return types.Sandbox{ID: testID, State: state}, nil
}

func (*sandboxService) Start(context.Context, string) (types.Sandbox, error) {
	return types.Sandbox{}, nil
}

func (s *sandboxService) Stop(context.Context, string) (types.Sandbox, error) {
	s.state = types.SandboxStateStopped
	return types.Sandbox{ID: testID, State: s.state}, nil
}

func (s *sandboxService) Remove(context.Context, string) (types.Sandbox, error) {
	s.removed.Store(true)
	return types.Sandbox{ID: testID}, nil
}

func (s *sandboxService) Exec(_ context.Context, _ string, command types.Command, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	s.command = command
	s.commands = append(s.commands, command)
	if stdin != nil {
		s.stdin, _ = io.ReadAll(stdin)
	}
	_, _ = stdout.Write([]byte("hello"))
	_, _ = stderr.Write([]byte("warning"))
	return 0, nil
}

func TestCreateAndForegroundCommand(t *testing.T) {
	dir := t.TempDir()
	service := &sandboxService{}
	handler, err := NewHandler(service, &snapshotService{}, testKey, dir)
	if err != nil {
		t.Fatal(err)
	}
	create := httptest.NewRequest("POST", "/v2/sandboxes", bytes.NewBufferString(`{"templateID":"ubuntu"}`))
	create.Header.Set("X-API-Key", testKey)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, create)
	if response.Code != http.StatusCreated || service.created.ImageReference != "ubuntu" {
		t.Fatalf("create = %d %s", response.Code, response.Body.String())
	}
	var sandbox struct {
		SandboxID       string `json:"sandboxID"`
		EnvdAccessToken string `json:"envdAccessToken"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &sandbox); err != nil {
		t.Fatal(err)
	}
	if sandbox.SandboxID != testID || sandbox.EnvdAccessToken == "" {
		t.Fatalf("sandbox = %+v", sandbox)
	}
	payload := []byte(`{"process":{"cmd":"/bin/bash","args":["-l","-c","echo hello"]}}`)
	var input bytes.Buffer
	var header [5]byte
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	input.Write(header[:])
	input.Write(payload)
	start := httptest.NewRequest("POST", "/process.Process/Start", &input)
	start.Header.Set("Content-Type", "application/connect+json")
	start.Header.Set("E2b-Sandbox-Id", testID)
	start.Header.Set("X-Access-Token", sandbox.EnvdAccessToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, start)
	if response.Code != http.StatusOK {
		t.Fatalf("process = %d %s", response.Code, response.Body.String())
	}
	if service.command.Args[0] != "/bin/bash" || service.command.Args[3] != "echo hello" {
		t.Fatalf("command = %+v", service.command)
	}
	reader := bytes.NewReader(response.Body.Bytes())
	frames := 0
	for reader.Len() > 0 {
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			t.Fatal(err)
		}
		length := binary.BigEndian.Uint32(header[1:])
		message := make([]byte, length)
		if _, err := io.ReadFull(reader, message); err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(message, &decoded); err != nil {
			t.Fatal(err)
		}
		if frames < 4 {
			event, ok := decoded["event"].(map[string]any)
			if !ok {
				t.Fatalf("frame %d missing process event: %s", frames, message)
			}
			switch frames {
			case 0:
				if event["start"] == nil {
					t.Fatalf("missing start event: %s", message)
				}
			case 1, 2:
				if event["data"] == nil {
					t.Fatalf("missing data event: %s", message)
				}
			case 3:
				if event["end"] == nil {
					t.Fatalf("missing end event: %s", message)
				}
			}
		}
		if frames == 4 && header[0] != 2 {
			t.Fatalf("missing Connect end-stream frame: %x", header[0])
		}
		frames++
	}
	if frames != 5 {
		t.Fatalf("frames = %d, want 5", frames)
	}
}

func TestUnsupportedCreateOptionsAreRejected(t *testing.T) {
	dir := t.TempDir()
	handler, err := NewHandler(&sandboxService{}, &snapshotService{}, testKey, dir)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/v2/sandboxes", bytes.NewBufferString(`{"templateID":"ubuntu","metadata":{"x":"y"}}`))
	request.Header.Set("X-API-Key", testKey)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestSnapshotCaptureAndClone(t *testing.T) {
	dir := t.TempDir()
	snapshots := &snapshotService{}
	handler, err := NewHandler(&sandboxService{}, snapshots, testKey, dir)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/sandboxes/"+testID+"/snapshots", bytes.NewBufferString(`{"name":"warm"}`))
	request.Header.Set("X-API-Key", testKey)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("snapshot = %d %s", response.Code, response.Body.String())
	}
	var info struct {
		SnapshotID string `json:"snapshotID"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest("POST", "/v2/sandboxes", bytes.NewBufferString(`{"templateID":"`+info.SnapshotID+`"}`))
	request.Header.Set("X-API-Key", testKey)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || snapshots.cloned != info.SnapshotID || snapshots.name == "" {
		t.Fatalf("clone = %d %s, snapshot = %+v", response.Code, response.Body.String(), snapshots)
	}
}

// This optional contract test runs the published E2B JS SDK, not a hand-built
// request fixture. Set KUMABOX_E2B_NODE_MODULE to an installed e2b package path.
func TestPublishedE2BSDK(t *testing.T) {
	dir := t.TempDir()
	module := os.Getenv("KUMABOX_E2B_NODE_MODULE")
	if module == "" {
		t.Skip("published E2B SDK is not installed")
	}
	handler, err := NewHandler(&sandboxService{}, &snapshotService{}, testKey, dir)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	command := exec.Command("node", "testdata/e2b-js.cjs")
	command.Env = append(os.Environ(),
		"E2B_API_URL="+server.URL, "E2B_SANDBOX_URL="+server.URL,
		"E2B_API_KEY="+testKey, "KUMABOX_E2B_NODE_MODULE="+module,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("E2B SDK: %v\n%s", err, output)
	}
}

func TestPublishedE2BPythonSDK(t *testing.T) {
	dir := t.TempDir()
	modulePath := os.Getenv("KUMABOX_E2B_PYTHON_PATH")
	if modulePath == "" {
		t.Skip("published E2B Python SDK is not installed")
	}
	handler, err := NewHandler(&sandboxService{}, &snapshotService{}, testKey, dir)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	command := exec.Command("python3", "testdata/e2b_python.py")
	command.Env = append(os.Environ(),
		"E2B_API_URL="+server.URL, "E2B_SANDBOX_URL="+server.URL,
		"E2B_API_KEY="+testKey, "PYTHONPATH="+modulePath,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("E2B Python SDK: %v\n%s", err, output)
	}
}

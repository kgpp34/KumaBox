package e2b

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"

	"github.com/kumabox/kumabox/api"
	"github.com/kumabox/kumabox/types"
)

const (
	testKey = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testID  = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
)

type sandboxService struct {
	created api.CreateSandboxInput
	command types.Command
}

type snapshotService struct {
	cloned string
	name   string
}

func (*snapshotService) Save(_ context.Context, req api.SaveSnapshotInput) (types.Snapshot, error) {
	return types.Snapshot{ID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", Name: req.Name, SandboxID: testID}, nil
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
	return types.Sandbox{ID: testID, Config: req.Config, State: types.SandboxStateRunning}, nil
}

func (*sandboxService) Inspect(context.Context, string) (types.Sandbox, error) {
	return types.Sandbox{ID: testID, State: types.SandboxStateRunning}, nil
}

func (*sandboxService) Start(context.Context, string) (types.Sandbox, error) {
	return types.Sandbox{}, nil
}

func (*sandboxService) Stop(context.Context, string) (types.Sandbox, error) {
	return types.Sandbox{}, nil
}

func (*sandboxService) Remove(context.Context, string) (types.Sandbox, error) {
	return types.Sandbox{}, nil
}

func (s *sandboxService) Exec(_ context.Context, _ string, command types.Command, _ io.Reader, stdout, stderr io.Writer) (int, error) {
	s.command = command
	_, _ = stdout.Write([]byte("hello"))
	_, _ = stderr.Write([]byte("warning"))
	return 0, nil
}

func TestCreateAndForegroundCommand(t *testing.T) {
	service := &sandboxService{}
	handler, err := NewHandler(service, &snapshotService{}, testKey)
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
	handler, err := NewHandler(&sandboxService{}, &snapshotService{}, testKey)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/v2/sandboxes", bytes.NewBufferString(`{"templateID":"ubuntu","timeout":300}`))
	request.Header.Set("X-API-Key", testKey)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestSnapshotCaptureAndClone(t *testing.T) {
	snapshots := &snapshotService{}
	handler, err := NewHandler(&sandboxService{}, snapshots, testKey)
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
	module := os.Getenv("KUMABOX_E2B_NODE_MODULE")
	if module == "" {
		t.Skip("published E2B SDK is not installed")
	}
	handler, err := NewHandler(&sandboxService{}, &snapshotService{}, testKey)
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
	modulePath := os.Getenv("KUMABOX_E2B_PYTHON_PATH")
	if modulePath == "" {
		t.Skip("published E2B Python SDK is not installed")
	}
	handler, err := NewHandler(&sandboxService{}, &snapshotService{}, testKey)
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

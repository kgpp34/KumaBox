package e2b

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/types"
)

func controlRequest(t *testing.T, handler http.Handler, method, path, body string, status int) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("X-API-Key", testKey)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != status {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, response.Code, status, response.Body.String())
	}
	return response
}

func TestDefaultEnvironmentAndCommandOverrides(t *testing.T) {
	dir := t.TempDir()
	service := &sandboxService{}
	adapter, err := NewHandler(service, &snapshotService{}, testKey, dir)
	if err != nil {
		t.Fatal(err)
	}
	response := controlRequest(t, adapter, "POST", "/v2/sandboxes",
		`{"templateID":"ubuntu","envVars":{"DEFAULT":"base","OVERRIDE":"base"},"metadata":{"project":"demo"}}`, http.StatusCreated)
	var sandbox struct {
		AccessToken string `json:"envdAccessToken"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &sandbox); err != nil {
		t.Fatal(err)
	}
	// Reopen the adapter to prove command defaults do not depend on memory.
	adapter, err = NewHandler(service, &snapshotService{}, testKey, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, envs := range []map[string]string{{"OVERRIDE": "command"}, nil} {
		payload, err := json.Marshal(map[string]any{"process": map[string]any{"cmd": "/bin/true", "envs": envs}})
		if err != nil {
			t.Fatal(err)
		}
		var framed bytes.Buffer
		var header [5]byte
		binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
		framed.Write(header[:])
		framed.Write(payload)
		request := httptest.NewRequest("POST", "/process.Process/Start", &framed)
		request.Header.Set("Content-Type", "application/connect+json")
		request.Header.Set("E2b-Sandbox-Id", testID)
		request.Header.Set("X-Access-Token", sandbox.AccessToken)
		response = httptest.NewRecorder()
		adapter.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("start = %d: %s", response.Code, response.Body.String())
		}
		want := "base"
		if envs != nil {
			want = "command"
		}
		if service.command.Env["OVERRIDE"] != want || service.command.Env["DEFAULT"] != "base" ||
			service.command.Env["E2B_SANDBOX_ID"] != testID || service.command.Env["E2B_TEMPLATE_ID"] != "ubuntu" {
			t.Fatalf("incorrect command defaults: %+v", service.command.Env)
		}
	}
	info := controlRequest(t, adapter, "GET", "/sandboxes/"+testID, "", http.StatusOK)
	if strings.Contains(info.Body.String(), "envVars") || strings.Contains(info.Body.String(), "DEFAULT") {
		t.Fatalf("environment leaked through inspect: %s", info.Body.String())
	}
}

func TestInvalidEnvironmentRejectedBeforeCreatingSandbox(t *testing.T) {
	for _, body := range []string{
		`{"templateID":"ubuntu","envVars":{"BAD=KEY":"value"}}`,
		`{"templateID":"ubuntu","envVars":{"GOOD":"bad\u0000value"}}`,
	} {
		service := &sandboxService{}
		adapter, err := NewHandler(service, &snapshotService{}, testKey, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		controlRequest(t, adapter, "POST", "/v2/sandboxes", body, http.StatusBadRequest)
		if service.created.ImageReference != "" {
			t.Fatal("invalid environment allocated a sandbox")
		}
	}
}

func TestSandboxListFiltersAndStablePagination(t *testing.T) {
	dir := t.TempDir()
	store, err := openLeaseStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Add(-time.Minute)
	ids := []types.SandboxID{
		"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002",
		"00000000-0000-4000-8000-000000000003", "00000000-0000-4000-8000-000000000004",
	}
	service := &sandboxService{records: make([]types.Sandbox, 0, len(ids))}
	for index, id := range ids {
		state := types.SandboxStateRunning
		policy := lease{TemplateID: "ubuntu", Metadata: map[string]string{"team +&%": "infra /+&%", "empty": ""}, EnvVars: map[string]string{"SECRET": "do-not-return"}}
		if index == 1 {
			state = types.SandboxStateStopped
			policy.PausedSnapshot = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
		}
		// Identical timestamps exercise the UUID tie-breaker.
		service.records = append(service.records, types.Sandbox{ID: id, State: state, CreatedAt: created})
		if index < 3 {
			if err := store.put(id, policy); err != nil {
				t.Fatal(err)
			}
		}
	}
	adapter, err := NewHandler(service, &snapshotService{}, testKey, dir)
	if err != nil {
		t.Fatal(err)
	}
	list := func(query string) ([]map[string]any, string) {
		t.Helper()
		response := controlRequest(t, adapter, "GET", "/v2/sandboxes?"+query, "", http.StatusOK)
		if strings.Contains(response.Body.String(), "do-not-return") || strings.Contains(response.Body.String(), "envdAccessToken") {
			t.Fatalf("private data leaked through listing: %s", response.Body.String())
		}
		var records []map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &records); err != nil {
			t.Fatal(err)
		}
		return records, response.Header().Get("X-Next-Token")
	}
	records, token := list("limit=1")
	if len(records) != 1 || records[0]["sandboxID"] != ids[2].String() || token == "" {
		t.Fatalf("first page: %+v, token = %q", records, token)
	}
	// Removing the first item must not cause an offset-based skip of page two.
	service.records = append(service.records[:2], service.records[3:]...)
	records, token = list("limit=1&nextToken=" + url.QueryEscape(token))
	if len(records) != 1 || records[0]["sandboxID"] != ids[1].String() || token == "" {
		t.Fatalf("second page: %+v, token = %q", records, token)
	}
	records, token = list("limit=1&nextToken=" + url.QueryEscape(token))
	if len(records) != 1 || records[0]["sandboxID"] != ids[0].String() || token != "" {
		t.Fatalf("last page: %+v, token = %q", records, token)
	}
	inner := url.Values{url.PathEscape("team +&%"): {url.PathEscape("infra /+&%")}}.Encode()
	for _, test := range []struct {
		name  string
		query string
		count int
	}{
		{"metadata", url.Values{"metadata": {inner}}.Encode(), 2},
		{"paused", "state=paused", 1},
		{"running", "state=running", 1},
		{"states", "state=running,paused", 2},
		{"template", "template=missing", 0},
		{"missing empty value", "metadata=missing%3D", 0},
		{"after", url.Values{"startedAfter": {created.Add(time.Second).Format(time.RFC3339Nano)}}.Encode(), 0},
		{"at boundary", url.Values{"startedAfter": {created.Format(time.RFC3339Nano)}}.Encode(), 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			records, _ := list(test.query)
			if len(records) != test.count {
				t.Fatalf("got %d records, want %d", len(records), test.count)
			}
		})
	}
	records, _ = list("order=asc")
	if len(records) != 2 || records[0]["sandboxID"] != ids[0].String() {
		t.Fatalf("ascending list = %+v", records)
	}
	for _, query := range []string{"limit=0", "limit=101", "order=other", "state=stopped", "nextToken=invalid", "startedAfter=no"} {
		controlRequest(t, adapter, "GET", "/v2/sandboxes?"+query, "", http.StatusBadRequest)
	}
}

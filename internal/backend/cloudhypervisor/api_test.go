package cloudhypervisor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestStateTransitionUsesVMInfoAndIsIdempotent(t *testing.T) {
	state := "Running"
	pauseCalls := 0
	client := apiTestClient(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/v1/vm.info":
			return apiResponse(http.StatusOK, fmt.Sprintf(`{"state":%q}`, state)), nil
		case "/api/v1/vm.pause":
			pauseCalls++
			state = "Paused"
			return apiResponse(http.StatusNoContent, ""), nil
		default:
			return apiResponse(http.StatusNotFound, "not found"), nil
		}
	})
	if err := stateTransitionWithClient(context.Background(), client, "vm.pause", "Paused"); err != nil {
		t.Fatal(err)
	}
	if err := stateTransitionWithClient(context.Background(), client, "vm.pause", "Paused"); err != nil {
		t.Fatal(err)
	}
	if pauseCalls != 1 {
		t.Fatalf("vm.pause calls = %d, want 1", pauseCalls)
	}
}

func TestStateTransitionReportsBackendAPIError(t *testing.T) {
	client := apiTestClient(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/api/v1/vm.info" {
			return apiResponse(http.StatusOK, `{"state":"Running"}`), nil
		}
		return apiResponse(http.StatusInternalServerError, "pause denied\n"), nil
	})
	err := stateTransitionWithClient(context.Background(), client, "vm.pause", "Paused")
	if err == nil || err.Error() != "BACKEND_API_ERROR: vm.pause returned 500 Internal Server Error: pause denied" {
		t.Fatalf("PauseVM() error = %v", err)
	}
}

func apiTestClient(fn roundTripFunc) *http.Client {
	return &http.Client{Transport: fn}
}

func apiResponse(code int, body string) *http.Response {
	return &http.Response{
		StatusCode: code,
		Status:     fmt.Sprintf("%d %s", code, http.StatusText(code)),
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

package firecracker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAPIClientBoundsAndReportsResponses(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "kb-fc-api-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "api.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.ContentLength != 0 {
			t.Error("GET carried a request body")
			http.Error(w, "GET body forbidden", 400)
			return
		}
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`{"state":"Running"}`))
		case "/error":
			http.Error(w, `{"fault_message":"bad config"}`, http.StatusBadRequest)
		case "/large":
			_, _ = w.Write([]byte(strings.Repeat("x", (1<<20)+1)))
		case "/actions":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	var info struct{ State string }
	if err := apiRequest(t.Context(), socket, "GET", "/", nil, &info); err != nil || info.State != "Running" {
		t.Fatalf("info: %+v %v", info, err)
	}
	for _, path := range []string{"/error", "/large"} {
		if err := apiRequest(t.Context(), socket, "GET", path, nil, nil); err == nil {
			t.Fatal("accepted invalid response", path)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := apiRequest(ctx, socket, "GET", "/", nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request: %v", err)
	}
	if err := apiRequest(t.Context(), socket, "PUT", "/actions", map[string]string{"action_type": "SendCtrlAltDel"}, nil); err != nil {
		t.Fatal(err)
	}
}

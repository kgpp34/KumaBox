package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// machineConfig is Firecracker's wire model, not a persisted sandbox contract.
type machineConfig struct {
	VCPUs     uint32 `json:"vcpu_count"`
	MemoryMiB int64  `json:"mem_size_mib"`
}

type bootSource struct {
	Kernel string `json:"kernel_image_path"`
	Initrd string `json:"initrd_path"`
	Args   string `json:"boot_args"`
}

type drive struct {
	ID       string `json:"drive_id"`
	Path     string `json:"path_on_host"`
	Root     bool   `json:"is_root_device"`
	ReadOnly bool   `json:"is_read_only"`
	Cache    string `json:"cache_type"`
	Engine   string `json:"io_engine"`
}

type networkInterface struct {
	ID  string `json:"iface_id"`
	TAP string `json:"host_dev_name"`
	MAC string `json:"guest_mac"`
}

type balloon struct {
	AmountMiB    int64 `json:"amount_mib"`
	DeflateOnOOM bool  `json:"deflate_on_oom"`
}

type vsock struct {
	CID  uint32 `json:"guest_cid"`
	Path string `json:"uds_path"`
}

// vmConfig is consumed once by --config-file, avoiding a control round trip
// for every device. The monitor still exposes its API for readiness and stop.
type vmConfig struct {
	Machine machineConfig      `json:"machine-config"`
	Boot    bootSource         `json:"boot-source"`
	Drives  []drive            `json:"drives"`
	NICs    []networkInterface `json:"network-interfaces"`
	Vsock   vsock              `json:"vsock"`
	Balloon *balloon           `json:"balloon,omitempty"`
	Entropy struct{}           `json:"entropy"`
}

// apiRequest bounds both response size and duration over a private Unix socket.
func apiRequest(ctx context.Context, socket, method, path string, body, output any) error {
	return apiRequestWithTimeout(ctx, socket, method, path, body, output, 500*time.Millisecond)
}

// apiRequestWithTimeout gives memory transfers their own deadline without
// retrying a mutating request whose result may already have taken effect.
func apiRequestWithTimeout(ctx context.Context, socket, method, path string, body, output any, timeout time.Duration) error {
	info, err := os.Lstat(socket)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return errors.New("firecracker API path is not a Unix socket")
	}
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: timeout}
	request, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close() //nolint:errcheck // bounded read/decode result is authoritative
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("firecracker API response exceeds limit")
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("firecracker %s %s: HTTP %d: %s", method, path, response.StatusCode, strings.TrimSpace(string(data)))
	}
	if output != nil {
		return json.Unmarshal(data, output)
	}
	return nil
}

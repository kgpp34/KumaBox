package cloudhypervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/kumabox/kumabox/internal/vmstore"
)

const (
	apiBaseURL            = "http://localhost/api/v1/"
	apiErrorBodySize      = 64 << 10
	nativeSnapshotTimeout = 10 * time.Minute
)

// APIError preserves the backend status and response body for diagnostics.
type APIError struct {
	Operation  string
	StatusCode int
	Status     string
	Message    string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("BACKEND_API_ERROR: %s returned %s", e.Operation, e.Status)
	}
	return fmt.Sprintf("BACKEND_API_ERROR: %s returned %s: %s", e.Operation, e.Status, e.Message)
}

type vmInfo struct {
	State      string                     `json:"state"`
	DeviceTree map[string]json.RawMessage `json:"device_tree"`
}

func socketHTTPClient(socketPath string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
	}
}

func doAPIOnce(ctx context.Context, socketPath string, timeout time.Duration, method, endpoint string, body []byte, successCodes ...int) ([]byte, error) {
	client := socketHTTPClient(socketPath, timeout)
	defer client.CloseIdleConnections()
	return doAPIOnceWithClient(ctx, client, method, endpoint, body, successCodes...)
}

func doAPIOnceWithClient(ctx context.Context, client *http.Client, method, endpoint string, body []byte, successCodes ...int) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, apiBaseURL+endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create %s request: %w", endpoint, err)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("BACKEND_API_UNAVAILABLE: %s: %w", endpoint, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, apiErrorBodySize+1))
	if err != nil {
		return nil, fmt.Errorf("read %s response: %w", endpoint, err)
	}
	if len(responseBody) > apiErrorBodySize {
		return nil, fmt.Errorf("BACKEND_API_ERROR: %s response exceeds %d bytes", endpoint, apiErrorBodySize)
	}
	for _, code := range successCodes {
		if resp.StatusCode == code {
			return responseBody, nil
		}
	}
	return nil, &APIError{
		Operation:  endpoint,
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Message:    strings.TrimSpace(string(responseBody)),
	}
}

func queryVMInfo(ctx context.Context, socketPath string, timeout time.Duration) (*vmInfo, error) {
	client := socketHTTPClient(socketPath, timeout)
	defer client.CloseIdleConnections()
	return queryVMInfoWithClient(ctx, client)
}

func queryVMInfoWithClient(ctx context.Context, client *http.Client) (*vmInfo, error) {
	raw, err := doAPIOnceWithClient(ctx, client, http.MethodGet, "vm.info", nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var info vmInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("decode vm.info response: %w", err)
	}
	if info.State == "" {
		return nil, errors.New("vm.info response has no state")
	}
	return &info, nil
}

func stateTransition(ctx context.Context, rec *vmstore.VMRecord, endpoint, target string) error {
	if rec == nil {
		return errors.New("VM record is nil")
	}
	apiSocket, timeout, err := backendAPIConfig(rec)
	if err != nil {
		return err
	}
	client := socketHTTPClient(apiSocket, timeout)
	defer client.CloseIdleConnections()
	return stateTransitionWithClient(ctx, client, endpoint, target)
}

func stateTransitionWithClient(ctx context.Context, client *http.Client, endpoint, target string) error {
	info, err := queryVMInfoWithClient(ctx, client)
	if err != nil {
		return err
	}
	if strings.EqualFold(info.State, target) {
		return nil
	}
	_, err = doAPIOnceWithClient(ctx, client, http.MethodPut, endpoint, nil, http.StatusNoContent)
	if err == nil || alreadyInState(err, target) {
		return nil
	}
	return err
}

func alreadyInState(err error, state string) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		return false
	}
	want := fmt.Sprintf("InvalidStateTransition(%s, %s)", state, state)
	return strings.Contains(apiErr.Message, want)
}

func backendAPIConfig(rec *vmstore.VMRecord) (string, time.Duration, error) {
	cfg, err := readRenderedConfig(rec.Config)
	if err != nil {
		return "", 0, fmt.Errorf("read backend config: %w", err)
	}
	apiSocket := rec.APISocket
	if apiSocket == "" {
		apiSocket = cfg.APISocket
	}
	if apiSocket == "" {
		return "", 0, errors.New("BACKEND_API_UNAVAILABLE: VM has no API socket")
	}
	timeout := time.Duration(cfg.APITimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return apiSocket, timeout, nil
}

func putJSONOnce(ctx context.Context, socketPath string, timeout time.Duration, endpoint string, payload any, successCodes ...int) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s request: %w", endpoint, err)
	}
	_, err = doAPIOnce(ctx, socketPath, timeout, http.MethodPut, endpoint, body, successCodes...)
	return err
}

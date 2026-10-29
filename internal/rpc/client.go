package rpc

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

	"github.com/kumabox/kumabox/internal/errdefs"
)

// Client owns reusable HTTP/2 connections to one local node socket.
type Client struct {
	http      *http.Client
	transport *http.Transport
	socket    string
}

// NewClient never opens local runtime state and never falls back to direct use.
func NewClient(socket string) *Client {
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	transport := &http.Transport{
		Protocols: protocols, Proxy: nil, IdleConnTimeout: 90 * time.Second,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
		},
	}
	return &Client{http: &http.Client{Transport: transport}, transport: transport, socket: socket}
}

// Streams separates binary I/O from metadata and optional progress callbacks.
// Call owns Input when it implements io.ReadCloser and closes it on return.
type Streams struct {
	Input  io.Reader
	Output io.Writer
	Error  io.Writer
	Event  func(Frame) error
}

// Call executes one operation. A disconnected mutation is never retried.
func (c *Client) Call(ctx context.Context, method string, params, result any, streams Streams) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if input, ok := streams.Input.(io.ReadCloser); ok {
		defer func() { _ = input.Close() }()
	}
	deadline, _ := ctx.Deadline()
	header, err := EncodeRequest(params, deadline)
	if err != nil {
		return err
	}
	var body io.Reader = bytes.NewReader(header)
	if streams.Input != nil {
		body = io.MultiReader(body, streams.Input)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://kumabox"+Prefix+method, body)
	if err != nil {
		return err
	}
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, fmt.Errorf("contact KumaBox daemon at %s: %w; start kumabox daemon", c.socket, err))
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("node request %s: HTTP %d: %s", method, response.StatusCode, strings.TrimSpace(string(data)))
	}
	scanner := Scanner(response.Body)
	for {
		frame, err := ReadFrame(scanner)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("node request %s ended without a result; inspect state before retrying: %w", method, err)
		}
		switch frame.Type {
		case "stdout", "stderr":
			output := streams.Output
			if frame.Type == "stderr" {
				output = streams.Error
			}
			if output != nil {
				if _, err := output.Write(frame.Bytes); err != nil {
					return err
				}
			}
		case "result":
			if result != nil && len(frame.Data) > 0 {
				if err := json.Unmarshal(frame.Data, result); err != nil {
					return err
				}
			}
			if frame.Failure != nil {
				return frame.Failure
			}
			return nil
		default:
			if streams.Event != nil {
				if err := streams.Event(frame); err != nil {
					return err
				}
			}
		}
	}
}

// Close releases idle connections after an invocation. It never closes the store.
func (c *Client) Close() error { c.transport.CloseIdleConnections(); return nil }

// IsUnavailable distinguishes a missing node from operation validation failures.
func IsUnavailable(err error) bool {
	var classified *errdefs.Error
	return errors.As(err, &classified) && classified.Code == errdefs.CodeArtifactUnavailable
}

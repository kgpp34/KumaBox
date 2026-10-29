package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/client"
	"github.com/kumabox/kumabox/internal/errdefs"
	"github.com/kumabox/kumabox/internal/types"
)

const maxForwardConnections = 64

// NewForwardCommand exposes a guest loopback service on a host loopback port.
// Each accepted TCP connection uses its own agent session; protocol bytes such
// as HTTP upgrades and WebSocket frames are forwarded without interpretation.
func NewForwardCommand(configuration configProvider) *cobra.Command {
	var address string
	var asJSON bool
	command := &cobra.Command{
		Use:   "forward SANDBOX LOCAL_PORT:GUEST_PORT",
		Short: "forward a host loopback TCP port to a guest service over vsock",
		Long: "Forward TCP traffic to 127.0.0.1 inside a running sandbox, including sandboxes without NICs.\n" +
			"Runs until interrupted. LOCAL_PORT may be 0 to allocate a free port.\n" +
			"At most 64 connections are relayed concurrently. Each new connection resolves the same sandbox ID.",
		Args: cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			localPort, guestPort, err := parseForwardPorts(args[1])
			if err != nil {
				return invalidFlag("ports", err)
			}
			ip := net.ParseIP(address)
			if ip == nil || !ip.IsLoopback() {
				return invalidFlag("address", errors.New("must be a loopback IP address"))
			}
			service, err := client.OpenSandbox(command.Context(), configuration(), nil)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, service.Close()) }()
			record, err := service.Inspect(command.Context(), args[0])
			if err != nil {
				return err
			}
			if record.State != types.SandboxStateRunning {
				return errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, errors.New("port forwarding requires a running sandbox"))
			}
			var listenConfig net.ListenConfig
			listener, err := listenConfig.Listen(command.Context(), "tcp", net.JoinHostPort(address, strconv.Itoa(int(localPort))))
			if err != nil {
				return fmt.Errorf("listen for port forwarding: %w", err)
			}
			defer listener.Close() //nolint:errcheck // serveForward also closes the listener
			if asJSON {
				err = writeGuestJSON(command.OutOrStdout(), struct {
					SandboxID string `json:"sandbox_id"`
					Address   string `json:"address"`
					GuestPort uint16 `json:"guest_port"`
				}{record.ID.String(), listener.Addr().String(), guestPort})
			} else {
				_, err = fmt.Fprintf(command.OutOrStdout(), "Forwarding %s -> %s:127.0.0.1:%d (Ctrl-C to stop)\n", listener.Addr(), args[0], guestPort)
			}
			if err != nil {
				return err
			}
			return serveForward(command.Context(), listener, func(ctx context.Context, local net.Conn) error {
				return service.ForwardPort(ctx, record.ID.String(), guestPort, local)
			}, func(err error) {
				_, _ = fmt.Fprintf(command.ErrOrStderr(), "Forward connection failed: %v\n", err)
			})
		},
	}
	command.Flags().StringVar(&address, "address", "127.0.0.1", "host loopback IP address to listen on")
	command.Flags().BoolVar(&asJSON, "json", false, "print the bound address and guest port as JSON")
	return command
}

func parseForwardPorts(value string) (uint16, uint16, error) {
	local, guest, found := strings.Cut(value, ":")
	if !found {
		return 0, 0, errors.New("expected LOCAL_PORT:GUEST_PORT")
	}
	localPort, err := strconv.ParseUint(local, 10, 16)
	if err != nil {
		return 0, 0, errors.New("local port must be between 0 and 65535")
	}
	guestPort, err := strconv.ParseUint(guest, 10, 16)
	if err != nil || guestPort == 0 {
		return 0, 0, errors.New("guest port must be between 1 and 65535")
	}
	return uint16(localPort), uint16(guestPort), nil
}

// serveForward owns the listener and every accepted connection. On cancellation
// or listener failure it closes streams, cancels handlers and joins them before
// the caller can close its metadata service. Connection errors do not stop other
// clients. Saturation rejects new connections without spawning more goroutines.
func serveForward(parent context.Context, listener net.Listener, forward func(context.Context, net.Conn) error, report func(error)) error {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	var handlers sync.WaitGroup
	defer func() { cancel(); _ = listener.Close(); handlers.Wait() }()
	slots := make(chan struct{}, maxForwardConnections)
	var reportMu sync.Mutex
	reportError := func(err error) {
		reportMu.Lock()
		defer reportMu.Unlock()
		report(err)
	}
	for {
		local, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept forwarded connection: %w", err)
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = local.Close()
			reportError(errors.New("forward connection limit reached"))
			continue
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			defer func() { <-slots }()
			stopClose := context.AfterFunc(ctx, func() { _ = local.Close() })
			defer stopClose()
			defer local.Close() //nolint:errcheck // handler owns this stream
			if err := forward(ctx, local); err != nil && ctx.Err() == nil {
				reportError(err)
			}
		}()
	}
}

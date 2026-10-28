package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/api"
	"github.com/kumabox/kumabox/api/e2b"
	"github.com/kumabox/kumabox/client"
	"github.com/kumabox/kumabox/config"
)

// newServeCommand exposes the local application over a loopback-only HTTP API.
// Remote clients should connect through an authenticated SSH tunnel or TLS proxy.
func newServeCommand(provideConfig func() config.Config) *cobra.Command {
	var address, tokenFile string
	command := &cobra.Command{
		Use:   "serve",
		Short: "Serve the authenticated KumaBox HTTP API",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) (returnErr error) {
			if tokenFile == "" {
				return errors.New("--token-file is required")
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("invalid listen address: %w", err)
			}
			ip := net.ParseIP(host)
			if ip == nil || !ip.IsLoopback() {
				return errors.New("--listen must use a loopback IP; use an SSH tunnel or TLS proxy for remote access")
			}
			// #nosec G304 -- the operator selects this local token path; its mode is checked below.
			file, err := os.Open(tokenFile)
			if err != nil {
				return fmt.Errorf("open API token file: %w", err)
			}
			defer func() { _ = file.Close() }()
			info, err := file.Stat()
			if err != nil {
				return fmt.Errorf("stat API token file: %w", err)
			}
			if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
				return errors.New("API token file must be regular and readable only by its owner (mode 0600)")
			}
			content, err := io.ReadAll(io.LimitReader(file, 4097))
			if err != nil {
				return fmt.Errorf("read API token file: %w", err)
			}
			if len(content) > 4096 {
				return errors.New("API token file is too large")
			}
			cfg := provideConfig()
			node := client.New(cfg.SocketPath())
			defer func() { _ = node.Close() }()
			nodeInfo, err := node.Info(command.Context())
			if err != nil {
				return err
			}
			sandboxClient, err := client.OpenSandbox(command.Context(), cfg, nil)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, sandboxClient.Close()) }()
			snapshotClient, err := client.OpenSnapshots(command.Context(), cfg, nil)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, snapshotClient.Close()) }()
			sandboxes := apiSandboxService{service: sandboxClient}
			snapshots := apiSnapshotService{service: snapshotClient}
			handler, err := api.NewHandler(api.Services{
				Sandboxes: sandboxes,
				Snapshots: snapshots,
			}, strings.TrimSpace(string(content)))
			if err != nil {
				return err
			}
			e2bHandler, err := e2b.NewHandler(sandboxes, snapshots, strings.TrimSpace(string(content)), filepath.Join(nodeInfo.DataRoot, "e2b"))
			if err != nil {
				return err
			}
			listener, err := net.Listen("tcp", address)
			if err != nil {
				return fmt.Errorf("listen for API: %w", err)
			}
			routes := http.NewServeMux()
			routes.Handle("/v1/", handler)
			routes.Handle("/", e2bHandler)
			server := &http.Server{Handler: routes, ReadHeaderTimeout: 5 * time.Second}
			stopped := make(chan struct{})
			shutdownDone := make(chan struct{})
			go func() {
				defer close(shutdownDone)
				select {
				case <-command.Context().Done():
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					_ = server.Shutdown(ctx)
				case <-stopped:
				}
			}()
			if _, err := fmt.Fprintf(command.OutOrStdout(), "KumaBox API listening on %s\n", listener.Addr()); err != nil {
				close(stopped)
				<-shutdownDone
				_ = listener.Close()
				return err
			}
			serveErr := server.Serve(listener)
			close(stopped)
			<-shutdownDone
			if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				return fmt.Errorf("serve API: %w", serveErr)
			}
			return nil
		},
	}
	command.Flags().StringVar(&address, "listen", "127.0.0.1:8765", "loopback listen address")
	command.Flags().StringVar(&tokenFile, "token-file", "", "path to an owner-only file containing the API token")
	return command
}

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

const reseedEntropyBytes = 32

// ReseedOptions separates guest initialization from immediate disk durability.
// The zero value preserves synchronous machine-ID persistence for older callers.
type ReseedOptions struct {
	// RegenerateMachineID assigns a new identity to a cloned guest.
	RegenerateMachineID bool
	// SkipMachineIDSync acknowledges the identity write without forcing it to
	// stable storage. The write and kernel reseed still finish before the reply.
	SkipMachineIDSync bool
}

// Reseed sends host-generated entropy to the guest and waits for confirmation.
// The caller owns and closes the transport after this single operation.
func Reseed(ctx context.Context, connection io.ReadWriteCloser, entropy []byte, regenerateMachineID bool) error {
	return ReseedWithOptions(ctx, connection, entropy, ReseedOptions{RegenerateMachineID: regenerateMachineID})
}

// ReseedWithOptions waits for the requested initialization to finish. Agents
// predating SkipMachineIDSync safely retain their synchronous persistence policy.
func ReseedWithOptions(ctx context.Context, connection io.ReadWriteCloser, entropy []byte, options ReseedOptions) error {
	if len(entropy) != reseedEntropyBytes {
		return fmt.Errorf("reseed needs %d entropy bytes", reseedEntropyBytes)
	}
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	if err := NewEncoder(connection).Encode(Message{
		Type: MessageReseed, Data: entropy, RegenMachineID: options.RegenerateMachineID,
		SkipMachineIDSync: options.SkipMachineIDSync,
	}); err != nil {
		return err
	}
	response, err := NewDecoder(connection).Decode()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("read reseed response: %w", err)
	}
	switch response.Type {
	case MessageExit:
		if response.ExitCode != 0 {
			return fmt.Errorf("guest reseed exited with status %d", response.ExitCode)
		}
		return nil
	case MessageError:
		return fmt.Errorf("guest agent: %s", response.Message)
	default:
		return fmt.Errorf("unexpected reseed response %q", response.Type)
	}
}

// writeMachineIDAt derives a fresh persistent identity directly from host
// entropy, so it stays unique even if a guest kernel reseed reports an error.
// Writing in place also supports /etc/machine-id when systemd bind-mounts it.
func writeMachineIDAt(entropy []byte, path string, syncIdentity bool) error {
	if len(entropy) != reseedEntropyBytes {
		return fmt.Errorf("reseed needs %d entropy bytes", reseedEntropyBytes)
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	hash := sha256.Sum256(append([]byte("kumabox:machine-id:"), entropy...))
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0) //nolint:gosec // fixed guest identity path; tests inject a temporary file
	if err != nil {
		return fmt.Errorf("open machine ID: %w", err)
	}
	if _, err := fmt.Fprintln(file, hex.EncodeToString(hash[:16])); err != nil {
		return errors.Join(err, file.Close())
	}
	if syncIdentity {
		return errors.Join(file.Sync(), file.Close())
	}
	return file.Close()
}

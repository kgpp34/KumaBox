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

// Reseed sends host-generated entropy to the guest and waits for confirmation.
// The caller owns and closes the transport after this single operation.
func Reseed(ctx context.Context, connection io.ReadWriteCloser, entropy []byte, regenerateMachineID bool) error {
	if len(entropy) != reseedEntropyBytes {
		return fmt.Errorf("reseed needs %d entropy bytes", reseedEntropyBytes)
	}
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	if err := NewEncoder(connection).Encode(Message{Type: MessageReseed, Data: entropy, RegenMachineID: regenerateMachineID}); err != nil {
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
func writeMachineIDAt(entropy []byte, path string) error {
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
	return errors.Join(file.Sync(), file.Close())
}

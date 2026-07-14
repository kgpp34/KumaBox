package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"syscall"
)

// CopyResult describes the durable copy created for one snapshot disk.
type CopyResult struct {
	Strategy           string
	LogicalSizeBytes   int64
	AllocatedSizeBytes int64
	SHA256             string
}

// CopyFile preserves sparse allocation where supported and fsyncs the result.
func CopyFile(ctx context.Context, source, destination string) (CopyResult, error) {
	strategy, err := copyPlatform(ctx, source, destination)
	if err != nil {
		return CopyResult{}, err
	}
	file, err := os.Open(destination) //nolint:gosec
	if err != nil {
		return CopyResult{}, fmt.Errorf("open copied disk: %w", err)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		_ = file.Close()
		return CopyResult{}, fmt.Errorf("checksum copied disk: %w", err)
	}
	info, err := file.Stat()
	closeErr := file.Close()
	if err != nil {
		return CopyResult{}, fmt.Errorf("stat copied disk: %w", err)
	}
	if closeErr != nil {
		return CopyResult{}, fmt.Errorf("close copied disk: %w", closeErr)
	}
	allocated := info.Size()
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		allocated = stat.Blocks * 512
	}
	return CopyResult{
		Strategy: strategy, LogicalSizeBytes: info.Size(), AllocatedSizeBytes: allocated,
		SHA256: hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

func bufferedCopy(ctx context.Context, source, destination string) (string, error) {
	src, err := os.Open(source) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("open source disk: %w", err)
	}
	defer src.Close()                                                              //nolint:errcheck
	dst, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("create destination disk: %w", err)
	}
	ok := false
	defer func() {
		_ = dst.Close()
		if !ok {
			_ = os.Remove(destination)
		}
	}()
	if _, err := io.Copy(dst, &contextReader{ctx: ctx, reader: src}); err != nil {
		return "", fmt.Errorf("copy disk: %w", err)
	}
	if err := dst.Sync(); err != nil {
		return "", fmt.Errorf("sync copied disk: %w", err)
	}
	if err := dst.Close(); err != nil {
		return "", fmt.Errorf("close copied disk: %w", err)
	}
	ok = true
	return "stream", nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

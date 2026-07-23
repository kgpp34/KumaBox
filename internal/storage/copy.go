package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/kumabox/kumabox/internal/fileutil"
)

// MaxConcurrentFileCopies bounds simultaneous large file copies so snapshot
// and restore operations use parallel IO without saturating the host disk.
const MaxConcurrentFileCopies = 2

// CopyResult describes the durable copy created for one snapshot disk.
type CopyResult struct {
	Strategy           string
	LogicalSizeBytes   int64
	AllocatedSizeBytes int64
	SHA256             string
}

// CopyFile preserves sparse allocation where supported, fsyncs the result, and
// computes its checksum before returning.
func CopyFile(ctx context.Context, source, destination string) (CopyResult, error) {
	staged, err := StageFile(ctx, source, destination)
	if err != nil {
		return CopyResult{}, err
	}
	return FinalizeStagedFile(ctx, destination, staged)
}

// StageFile creates a copy without reading it back or forcing it to stable
// storage. Callers with a latency-sensitive pause window must finalize it
// after the source workload has resumed.
func StageFile(ctx context.Context, source, destination string) (CopyResult, error) {
	strategy, err := copyPlatform(ctx, source, destination)
	if err != nil {
		return CopyResult{}, err
	}
	info, err := os.Stat(destination)
	if err != nil {
		return CopyResult{}, fmt.Errorf("stat staged disk: %w", err)
	}
	return copyResult(strategy, info, ""), nil
}

// FinalizeStagedFile makes a staged copy durable and computes its checksum.
func FinalizeStagedFile(ctx context.Context, path string, staged CopyResult) (CopyResult, error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec
	if err != nil {
		return CopyResult{}, fmt.Errorf("open staged disk: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return CopyResult{}, fmt.Errorf("sync staged disk: %w", err)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, &contextReader{ctx: ctx, reader: file}); err != nil {
		_ = file.Close()
		return CopyResult{}, fmt.Errorf("checksum staged disk: %w", err)
	}
	info, err := file.Stat()
	closeErr := file.Close()
	if err != nil {
		return CopyResult{}, fmt.Errorf("stat staged disk: %w", err)
	}
	if closeErr != nil {
		return CopyResult{}, fmt.Errorf("close staged disk: %w", closeErr)
	}
	return copyResult(staged.Strategy, info, hex.EncodeToString(hash.Sum(nil))), nil
}

func copyResult(strategy string, info os.FileInfo, checksum string) CopyResult {
	allocated := info.Size()
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		allocated = stat.Blocks * 512
	}
	return CopyResult{
		Strategy: strategy, LogicalSizeBytes: info.Size(), AllocatedSizeBytes: allocated,
		SHA256: checksum,
	}
}

func bufferedCopy(ctx context.Context, source, destination string) (strategy string, err error) {
	src, err := os.Open(source) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("open source disk: %w", err)
	}
	defer fileutil.CloseAndJoin(&err, src, "close source disk")
	dst, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("create destination disk: %w", err)
	}
	ok := false
	defer func() {
		fileutil.CloseAndJoin(&err, dst, "close destination disk")
		if !ok {
			_ = os.Remove(destination)
		}
	}()
	if _, err := io.Copy(dst, &contextReader{ctx: ctx, reader: src}); err != nil {
		return "", fmt.Errorf("copy disk: %w", err)
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

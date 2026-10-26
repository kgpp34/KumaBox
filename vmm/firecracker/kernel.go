package firecracker

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"

	filelock "github.com/kumabox/kumabox/lock/flock"
	"github.com/kumabox/kumabox/storage"
)

const maxKernelBytes = 256 << 20

// prepareKernel caches ELF extraction by compressed source digest, shared across
// starts and sandboxes. A lock plus rename prevents readers seeing partial data.
// Existing image artifacts are immutable and are never modified by this adapter.
func prepareKernel(ctx context.Context, cache, source string) (result string, returnErr error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	raw, direct, err := readKernel(source)
	if err != nil {
		return "", err
	}
	if direct {
		return source, nil
	}
	if err := storage.EnsureDir(cache); err != nil {
		return "", err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	target := filepath.Join(cache, digest+".elf")
	lock := filelock.New(target + ".lock")
	if err := lock.Lock(ctx); err != nil {
		return "", err
	}
	defer func() { returnErr = errors.Join(returnErr, lock.Unlock(context.WithoutCancel(ctx))) }()
	if err := storage.CheckPath(target); err != nil {
		return "", err
	}
	if _, direct, err := readKernel(target); err == nil {
		if !direct {
			return "", errors.New("cached firecracker kernel is not a valid x86-64 ELF")
		}
		return target, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	decoded, err := extractKernel(raw)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(cache, ".kernel-*")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }() //nolint:gosec // private temp path created in validated cache
	_, writeErr := temporary.Write(decoded)
	syncErr := temporary.Sync()
	if err := errors.Join(writeErr, syncErr, temporary.Close()); err != nil {
		return "", err
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		return "", err
	}
	directory, err := os.Open(cache) //nolint:gosec // validated managed cache
	if err != nil {
		return "", err
	}
	if err := errors.Join(directory.Sync(), directory.Close()); err != nil {
		return "", err
	}
	return target, nil
}

// readKernel inspects ELF headers directly from the immutable file. Copying an
// already usable kernel into the Go heap adds allocation and GC work to every
// start; only compressed inputs need their full contents for the cache digest.
func readKernel(path string) (raw []byte, direct bool, returnErr error) {
	file, err := os.Open(path) //nolint:gosec // verified image artifact or managed content-addressed cache
	if err != nil {
		return nil, false, err
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxKernelBytes {
		return nil, false, errors.New("kernel must be a regular file within the size limit")
	}
	if validELF(io.NewSectionReader(file, 0, info.Size())) {
		return nil, true, nil
	}
	raw, err = io.ReadAll(io.LimitReader(file, maxKernelBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(raw) > maxKernelBytes {
		return nil, false, errors.New("compressed kernel exceeds size limit")
	}
	return raw, false, nil
}

func validELF(reader io.ReaderAt) bool {
	file, err := elf.NewFile(reader)
	if err != nil {
		return false
	}
	return file.Class == elf.ELFCLASS64 && file.Machine == elf.EM_X86_64 && file.Type == elf.ET_EXEC && len(file.Progs) > 0
}

// extractKernel recognizes gzip/zstd payloads embedded in x86 bzImages. Reads
// and decoder memory are bounded; magic bytes alone are never accepted as ELF.
func extractKernel(raw []byte) ([]byte, error) {
	for _, magic := range [][]byte{{0x1f, 0x8b, 0x08}, {0x28, 0xb5, 0x2f, 0xfd}} {
		offset := 0
		for offset < len(raw) {
			index := bytes.Index(raw[offset:], magic)
			if index < 0 {
				break
			}
			offset += index
			payload := raw[offset:]
			offset += len(magic)
			var reader io.ReadCloser
			if magic[0] == 0x1f {
				stream, err := gzip.NewReader(bytes.NewReader(payload))
				if err != nil {
					continue
				}
				stream.Multistream(false)
				reader = stream
			} else {
				stream, err := zstd.NewReader(bytes.NewReader(payload), zstd.WithDecoderMaxMemory(maxKernelBytes), zstd.WithDecoderConcurrency(1))
				if err != nil {
					continue
				}
				reader = stream.IOReadCloser()
			}
			decoded, _ := io.ReadAll(io.LimitReader(reader, maxKernelBytes+1))
			_ = reader.Close()
			if len(decoded) <= maxKernelBytes && validELF(bytes.NewReader(decoded)) {
				return decoded, nil
			}
		}
	}
	return nil, errors.New("firecracker requires an x86-64 ELF kernel or gzip/zstd bzImage containing one")
}

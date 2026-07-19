//go:build linux

package server

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

var filesystemFreezeState struct {
	sync.Mutex
	mounts []string
}

func freezeGuestFilesystems() ([]string, error) {
	filesystemFreezeState.Lock()
	defer filesystemFreezeState.Unlock()
	if len(filesystemFreezeState.mounts) > 0 {
		return append([]string(nil), filesystemFreezeState.mounts...), nil
	}
	mounts, err := writableBlockMounts("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	if len(mounts) == 0 {
		return nil, errors.New("no writable block-backed filesystems found")
	}
	unix.Sync()
	frozen := make([]string, 0, len(mounts))
	for _, mount := range mounts {
		if err := runFSFreeze("--freeze", mount); err != nil {
			remaining, thawErr := thawMounts(frozen)
			filesystemFreezeState.mounts = remaining
			return frozen, errors.Join(err, thawErr)
		}
		frozen = append(frozen, mount)
	}
	filesystemFreezeState.mounts = frozen
	return append([]string(nil), frozen...), nil
}

func thawGuestFilesystems() ([]string, error) {
	filesystemFreezeState.Lock()
	defer filesystemFreezeState.Unlock()
	mounts := append([]string(nil), filesystemFreezeState.mounts...)
	remaining, err := thawMounts(mounts)
	filesystemFreezeState.mounts = remaining
	return mounts, err
}

func thawMounts(mounts []string) ([]string, error) {
	errs := make([]error, 0)
	remaining := make([]string, 0)
	for i := len(mounts) - 1; i >= 0; i-- {
		if err := runFSFreeze("--unfreeze", mounts[i]); err != nil {
			errs = append(errs, err)
			remaining = append(remaining, mounts[i])
		}
	}
	sort.Slice(remaining, func(i, j int) bool { return len(remaining[i]) > len(remaining[j]) })
	return remaining, errors.Join(errs...)
}

func runFSFreeze(operation, mount string) error {
	output, err := exec.Command("fsfreeze", operation, mount).CombinedOutput() //nolint:gosec
	if err != nil {
		return fmt.Errorf("fsfreeze %s %s: %w: %s", operation, mount, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func writableBlockMounts(path string) ([]string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("read mountinfo: %w", err)
	}
	mounts := make([]string, 0)
	for _, line := range strings.Split(string(raw), "\n") {
		before, after, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		fields := strings.Fields(before)
		post := strings.Fields(after)
		if len(fields) < 6 || len(post) < 2 || !mountOption(fields[5], "rw") || !strings.HasPrefix(post[1], "/dev/") {
			continue
		}
		mounts = append(mounts, unescapeMountPath(fields[4]))
	}
	sort.Slice(mounts, func(i, j int) bool { return len(mounts[i]) > len(mounts[j]) })
	return mounts, nil
}

func mountOption(options, want string) bool {
	for _, option := range strings.Split(options, ",") {
		if option == want {
			return true
		}
	}
	return false
}

func unescapeMountPath(path string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(path)
}

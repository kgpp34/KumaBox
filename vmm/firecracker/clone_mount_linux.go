//go:build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/kumabox/kumabox/snapshot"
	"github.com/kumabox/kumabox/vmm"
)

// cloneMountPlan keeps the namespace's paths and aliases together. It is
// prepared before changing thread-local mount state and consumed once at exec.
type cloneMountPlan struct {
	root, upper, work string
	directories       []string
	paths             []string
	aliases           map[string]cloneDiskBinding
}

// launchCloneProcess builds a private path view, with no source placeholders
// or host-wide mounts. Upper-layer aliases are independent of source dentries,
// so deleting the source cannot detach their bind mounts during snapshot load.
// The overlay holds names only; every actual disk is a bind of a target inode.
//
//	locked OS thread -> private mount namespace -> private root
//	                 -> bind target disks at native + target paths -> exec
//
// The disposable thread is never unlocked: it dies with the setup goroutine,
// while the child retains its namespace until process and console cleanup.
func (d *Driver) launchCloneProcess(ctx context.Context, launch processLaunch, clone *cloneSetup) (vmm.Process, error) {
	aliases := clone.aliases
	run, err := d.paths.RunDir(launch.sandboxID)
	if err != nil {
		return vmm.Process{}, err
	}
	snapshotPaths, err := snapshot.NewPaths(d.paths.Roots())
	if err != nil {
		return vmm.Process{}, err
	}
	directories := []string{"/dev", "/proc", "/sys", run, snapshotPaths.StagingDir(), clone.snapshotDir}
	for path := range aliases {
		for _, shared := range directories {
			if path == shared || strings.HasPrefix(path, shared+"/") {
				return vmm.Process{}, invalid(fmt.Errorf("clone disk alias %s overlaps shared runtime directory %s", path, shared))
			}
		}
	}
	base := filepath.Join(run, "clone-mounts")
	if strings.ContainsAny(base, ",:\\\n") {
		return vmm.Process{}, invalid(errors.New("clone runtime path contains an overlay mount separator"))
	}
	root, upper, work := filepath.Join(base, "root"), filepath.Join(base, "upper"), filepath.Join(base, "work")
	for _, directory := range []string{root, upper, work} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return vmm.Process{}, err
		}
	}
	paths := make([]string, 0, len(aliases))
	for path := range aliases {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		placeholder := filepath.Join(upper, strings.TrimPrefix(path, "/"))
		if err := os.MkdirAll(filepath.Dir(placeholder), 0o700); err != nil {
			return vmm.Process{}, err
		}
		file, err := os.OpenFile(placeholder, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // path is inside the private upper tree
		if err != nil {
			return vmm.Process{}, err
		}
		if err := file.Close(); err != nil {
			return vmm.Process{}, err
		}
	}
	for _, directory := range directories {
		if err := os.MkdirAll(filepath.Join(upper, strings.TrimPrefix(directory, "/")), 0o700); err != nil {
			return vmm.Process{}, err
		}
	}
	type result struct {
		process vmm.Process
		err     error
	}
	finished := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		process, err := d.startCloneInNamespace(ctx, launch, cloneMountPlan{
			root: root, upper: upper, work: work,
			directories: directories, paths: paths, aliases: aliases,
		})
		finished <- result{process, err}
	}()
	// Always join setup; cancellation cannot leave a late, untracked VMM exec.
	completed := <-finished
	return completed.process, completed.err
}

func (d *Driver) startCloneInNamespace(ctx context.Context, launch processLaunch, mounts cloneMountPlan) (vmm.Process, error) {
	if err := ctx.Err(); err != nil {
		return vmm.Process{}, err
	}
	if err := unix.Unshare(unix.CLONE_FS | unix.CLONE_NEWNS); err != nil {
		return vmm.Process{}, fmt.Errorf("unshare clone mount namespace: %w", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return vmm.Process{}, err
	}
	options := "lowerdir=/,upperdir=" + mounts.upper + ",workdir=" + mounts.work
	if err := unix.Mount("overlay", mounts.root, "overlay", unix.MS_NODEV, options); err != nil {
		return vmm.Process{}, fmt.Errorf("mount clone path view: %w", err)
	}
	for _, directory := range mounts.directories {
		target := filepath.Join(mounts.root, strings.TrimPrefix(directory, "/"))
		// Non-recursive binds avoid copying the new root back into itself.
		if err := unix.Mount(directory, target, "", unix.MS_BIND, ""); err != nil {
			return vmm.Process{}, fmt.Errorf("share clone runtime directory %s: %w", directory, err)
		}
	}
	for _, path := range mounts.paths {
		if err := ctx.Err(); err != nil {
			return vmm.Process{}, err
		}
		binding := mounts.aliases[path]
		file, err := os.OpenFile(binding.target, os.O_RDONLY|unix.O_NOFOLLOW, 0) //nolint:gosec // validated target-owned disk
		if err != nil {
			return vmm.Process{}, err
		}
		info, statErr := file.Stat()
		if statErr != nil || !info.Mode().IsRegular() {
			_ = file.Close()
			return vmm.Process{}, errors.Join(statErr, errors.New("clone disk is not a regular file"))
		}
		target := filepath.Join(mounts.root, strings.TrimPrefix(path, "/"))
		err = unix.Mount("/proc/self/fd/"+strconv.FormatUint(uint64(file.Fd()), 10), target, "", unix.MS_BIND, "")
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			return vmm.Process{}, errors.Join(err, closeErr)
		}
		if binding.readOnly {
			if err := unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
				return vmm.Process{}, err
			}
		}
	}
	launch.root = mounts.root
	return d.launchProcess(ctx, launch)
}

// verifyCloneDrives proves the monitor opened every target inode before it can
// execute guest code. It never follows or opens the recorded source paths.
func verifyCloneDrives(pid int, disks []cloneDiskBinding) error {
	directory := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	open := make(map[[2]uint64]bool, len(entries))
	for _, entry := range entries {
		var stat unix.Stat_t
		if unix.Stat(filepath.Join(directory, entry.Name()), &stat) == nil {
			open[[2]uint64{stat.Dev, stat.Ino}] = true
		}
	}
	for _, disk := range disks {
		var stat unix.Stat_t
		if err := unix.Stat(disk.target, &stat); err != nil {
			return err
		}
		if !open[[2]uint64{stat.Dev, stat.Ino}] {
			return fmt.Errorf("cloned VMM does not hold target disk %q", disk.id)
		}
	}
	return nil
}

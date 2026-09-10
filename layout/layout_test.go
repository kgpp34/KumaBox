package layout

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewResolvesManagedPaths(t *testing.T) {
	t.Parallel()

	root, err := New("/var/lib/kumabox")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The path is canonical, so on macOS /var becomes /private/var. Assert the
	// shape rather than one platform's spelling.
	if !filepath.IsAbs(root.Dir()) {
		t.Fatalf("Dir() = %q, want an absolute path", root.Dir())
	}
	if !strings.HasSuffix(root.Dir(), filepath.Join("var", "lib", "kumabox")) {
		t.Errorf("Dir() = %q, want it to end in var/lib/kumabox", root.Dir())
	}
	if got, want := root.DatabaseFile(), filepath.Join(root.Dir(), "metadata", "kumabox.db"); got != want {
		t.Errorf("DatabaseFile() = %q, want %q", got, want)
	}
	for _, path := range []string{root.MetadataDir(), root.BlobsDir(), root.TempDir()} {
		if !strings.HasPrefix(path, root.Dir()+string(filepath.Separator)) {
			t.Errorf("%q is not under the root %q", path, root.Dir())
		}
	}
}

func TestNewRejectsEmptyAndRelativePaths(t *testing.T) {
	t.Parallel()

	if _, err := New(""); !errors.Is(err, ErrRootEmpty) {
		t.Errorf("New(\"\") err = %v, want ErrRootEmpty", err)
	}
	for _, input := range []string{"var/lib/kumabox", "./kb", "../kb"} {
		if _, err := New(input); !errors.Is(err, ErrRootNotAbsolute) {
			t.Errorf("New(%q) err = %v, want ErrRootNotAbsolute", input, err)
		}
	}
}

func TestNewResolvesSymlinksSoContainmentChecksCompareRealPaths(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "link-to-root")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	root, err := New(link)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if root.Dir() != resolved {
		t.Errorf("Dir() = %q, want the resolved target %q", root.Dir(), resolved)
	}
}

func TestNewResolvesSymlinksInAMissingRoot(t *testing.T) {
	t.Parallel()

	// This is what --root /tmp/kb looks like on macOS, where /tmp is a symlink.
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	root, err := New(filepath.Join(link, "kb", "nested"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resolvedParent, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if want := filepath.Join(resolvedParent, "kb", "nested"); root.Dir() != want {
		t.Errorf("Dir() = %q, want %q", root.Dir(), want)
	}
}

func TestInspectReportsMissingRootWithoutCreatingIt(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "kb")
	root, err := New(missing)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	state, err := root.Inspect()
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if state.Exists {
		t.Error("Inspect must report the root as missing")
	}
	if state.FreeBytes == 0 || state.TotalBytes == 0 {
		t.Errorf("free space must be measured on the nearest existing ancestor: %+v", state)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("Inspect created the root: %v", err)
	}
}

func TestInspectRejectsAFileAsRoot(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	root, err := New(file)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := root.Inspect(); !errors.Is(err, ErrRootNotDirectory) {
		t.Fatalf("err = %v, want ErrRootNotDirectory", err)
	}
}

func TestPrepareCreatesManagedDirectories(t *testing.T) {
	t.Parallel()

	root, err := New(filepath.Join(t.TempDir(), "kb"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := root.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	for _, dir := range []string{root.Dir(), root.MetadataDir(), root.BlobsDir(), root.TempDir()} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", dir)
		}
		if got := info.Mode().Perm(); got != dirPerm {
			t.Errorf("%s mode = %o, want %o", dir, got, dirPerm)
		}
	}

	state, err := root.Inspect()
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !state.Exists || !state.Writable {
		t.Errorf("prepared root must be an existing writable directory: %+v", state)
	}
}

func TestPrepareIsIdempotent(t *testing.T) {
	t.Parallel()

	root, err := New(filepath.Join(t.TempDir(), "kb"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := root.Prepare(); err != nil {
			t.Fatalf("Prepare attempt %d: %v", attempt+1, err)
		}
	}
}

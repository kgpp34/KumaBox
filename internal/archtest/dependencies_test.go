package archtest

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const modulePath = "github.com/kumabox/kumabox"

var corePackages = []string{
	modulePath + "/internal/tenant",
	modulePath + "/internal/content",
	modulePath + "/internal/sandbox",
	modulePath + "/internal/operation",
}

var forbiddenImportPrefixes = []string{
	modulePath + "/internal/legacy",
	modulePath + "/internal/meta",
	modulePath + "/internal/network/cni",
	modulePath + "/internal/state",
	modulePath + "/internal/store",
	modulePath + "/internal/vm/runtime",
	modulePath + "/internal/vmm",
}

func TestCorePackagesDoNotImportAdaptersOrLegacyCore(t *testing.T) {
	t.Parallel()

	packages := listPackages(t, corePackages)
	for _, pkg := range packages {
		for _, imported := range pkg.Imports {
			for _, forbidden := range forbiddenImportPrefixes {
				if imported == forbidden || strings.HasPrefix(imported, forbidden+"/") {
					t.Errorf("%s imports forbidden package %s", pkg.ImportPath, imported)
				}
			}
		}
	}
}

func TestGlobalLayerDirectoriesDoNotExist(t *testing.T) {
	t.Parallel()

	root := repositoryRoot(t)
	for _, name := range []string{"app", "domain", "service"} {
		path := filepath.Join(root, "internal", name)
		_, err := os.Stat(path)
		if err == nil {
			t.Errorf("global layer directory must not exist: internal/%s", name)
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("inspect internal/%s: %v", name, err)
		}
	}
}

type listedPackage struct {
	ImportPath string
	Imports    []string
}

func listPackages(t *testing.T, importPaths []string) []listedPackage {
	t.Helper()

	args := append([]string{"list", "-json"}, importPaths...)
	command := exec.Command("go", args...)
	command.Dir = repositoryRoot(t)
	command.Env = append(os.Environ(), "GOCACHE="+t.TempDir())

	output, err := command.Output()
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			t.Fatalf("go list failed: %v\n%s", err, exitError.Stderr)
		}
		t.Fatalf("go list failed: %v", err)
	}

	decoder := json.NewDecoder(strings.NewReader(string(output)))
	packages := make([]listedPackage, 0, len(importPaths))
	for {
		var pkg listedPackage
		err = decoder.Decode(&pkg)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		packages = append(packages, pkg)
	}

	if len(packages) != len(importPaths) {
		t.Fatalf("go list returned %d packages, want %d", len(packages), len(importPaths))
	}
	for _, importPath := range importPaths {
		if !slices.ContainsFunc(packages, func(pkg listedPackage) bool {
			return pkg.ImportPath == importPath
		}) {
			t.Errorf("go list did not return %s", importPath)
		}
	}

	return packages
}

func repositoryRoot(t *testing.T) string {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve architecture test location")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}

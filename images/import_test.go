package images

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kumabox/kumabox/metadata"
	metadatasqlite "github.com/kumabox/kumabox/metadata/sqlite"
	"github.com/kumabox/kumabox/storage"
)

func TestImporterReusesLayerAndRemovesAliases(t *testing.T) {
	base := t.TempDir()
	roots := storage.Roots{
		Data: filepath.Join(base, "data"),
		Run:  filepath.Join(base, "run"),
		Log:  filepath.Join(base, "log"),
	}
	paths, err := NewPaths(roots)
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	store, err := metadatasqlite.Open(t.Context(), paths.MetadataDB(), Collections(), metadatasqlite.DefaultOptions())
	if err != nil {
		t.Fatalf("Open metadata: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	layerDigest := testDigest(t, "1")
	manifestDigest := testDigest(t, "2")
	source := fakeSource{manifest: Manifest{
		Digest: manifestDigest, Platform: Platform{OS: "linux", Architecture: "amd64"},
		Layers: []Descriptor{{Digest: layerDigest, Size: 3}},
	}}
	converter := &fakeConverter{}
	catalog := NewMetadataCatalog(store)
	importer, err := NewImporter(paths, catalog, converter, nil, Options{Parallelism: 1, Now: func() time.Time { return time.Unix(1, 0) }})
	if err != nil {
		t.Fatalf("NewImporter: %v", err)
	}
	if _, err := importer.Import(t.Context(), "first", source.manifest.Platform, source); err != nil {
		t.Fatalf("first Import: %v", err)
	}
	if _, err := importer.Import(t.Context(), "second", source.manifest.Platform, source); err != nil {
		t.Fatalf("second Import: %v", err)
	}
	if converter.Calls() != 1 {
		t.Fatalf("converter calls = %d, want 1", converter.Calls())
	}
	image, err := Verify(t.Context(), paths, catalog, "second")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(image.Names) != 2 {
		t.Fatalf("names = %v", image.Names)
	}
	if _, err := Remove(t.Context(), paths, catalog, "first"); err != nil {
		t.Fatalf("remove first alias: %v", err)
	}
	if !validFile(paths.EROFS(layerDigest)) {
		t.Fatal("shared layer removed with remaining alias")
	}
	if _, err := Remove(t.Context(), paths, catalog, "second"); err != nil {
		t.Fatalf("remove final alias: %v", err)
	}
	if _, err := os.Stat(paths.EROFS(layerDigest)); !os.IsNotExist(err) {
		t.Fatalf("layer still exists: %v", err)
	}
}

type fakeSource struct {
	manifest Manifest
}

func (f fakeSource) Resolve(context.Context, Platform) (Manifest, error) { return f.manifest, nil }

func (f fakeSource) OpenLayer(context.Context, Descriptor) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader([]byte("tar"))), nil
}

type fakeConverter struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeConverter) Convert(ctx context.Context, descriptor Descriptor, source io.Reader, workDir string) (ConvertedLayer, error) {
	if _, err := io.Copy(io.Discard, source); err != nil {
		return ConvertedLayer{}, err
	}
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	erofs := filepath.Join(workDir, "layer.erofs")
	kernel := filepath.Join(workDir, "vmlinuz")
	initrd := filepath.Join(workDir, "initrd.img")
	for path, data := range map[string][]byte{erofs: []byte("erofs"), kernel: []byte("kernel"), initrd: []byte("initrd")} {
		if err := os.WriteFile(path, data, 0o640); err != nil {
			return ConvertedLayer{}, err
		}
	}
	product, size, err := digestFileContext(ctx, erofs)
	if err != nil {
		return ConvertedLayer{}, err
	}
	return ConvertedLayer{
		SourceDigest: descriptor.Digest, EROFSPath: erofs, EROFSDigest: product,
		Size: size, BootFiles: []StagedBootFile{{Name: "vmlinuz", Path: kernel}, {Name: "initrd.img", Path: initrd}},
	}, nil
}

func (f *fakeConverter) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func testDigest(t *testing.T, digit string) Digest {
	t.Helper()
	digest, err := ParseDigest(fmt.Sprintf("sha256:%s", bytes.Repeat([]byte(digit), 64)))
	if err != nil {
		t.Fatalf("ParseDigest: %v", err)
	}
	return digest
}

func testImportState(t *testing.T, store metadata.Store) (Paths, *MetadataCatalog) {
	t.Helper()
	base := t.TempDir()
	paths, err := NewPaths(storage.Roots{Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log")})
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	if store == nil {
		memory, err := metadata.NewMemory(Collections())
		if err != nil {
			t.Fatal(err)
		}
		store = memory
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return paths, NewMetadataCatalog(store)
}

func testManifest(t *testing.T, manifestDigit string) Manifest {
	t.Helper()
	return Manifest{Digest: testDigest(t, manifestDigit), Platform: Platform{OS: "linux", Architecture: "amd64"}, Layers: []Descriptor{{Digest: testDigest(t, "1"), Size: 3}}}
}

func testImporter(t *testing.T, paths Paths, catalog Catalog, converter Converter) *Importer {
	t.Helper()
	importer, err := NewImporter(paths, catalog, converter, nil, Options{Parallelism: 2, Now: func() time.Time { return time.Unix(10, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	return importer
}

func TestImporterRepairsCorruptionAndPreservesCreationTime(t *testing.T) {
	paths, catalog := testImportState(t, nil)
	converter := &fakeConverter{}
	importer := testImporter(t, paths, catalog, converter)
	manifest := testManifest(t, "2")
	first, err := importer.Import(t.Context(), "aaaaaaaaaaaa", manifest.Platform, fakeSource{manifest: manifest})
	if err != nil {
		t.Fatal(err)
	}
	importer.options.Now = func() time.Time { return time.Unix(20, 0) }
	// Same length corruption must be detected by content digest, not stat.
	if err := os.WriteFile(paths.Kernel(manifest.Layers[0].Digest), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(t.Context(), paths, catalog, "aaaaaaaaaaaa"); err == nil {
		t.Fatal("verify accepted corrupt kernel")
	}
	second, err := importer.Import(t.Context(), "alias", manifest.Platform, fakeSource{manifest: manifest})
	if err != nil {
		t.Fatal(err)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) || converter.Calls() != 2 {
		t.Fatalf("repeat import = created %s, conversions %d", second.CreatedAt, converter.Calls())
	}
	if _, err := Verify(t.Context(), paths, catalog, "alias"); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(t.Context(), paths, catalog, "aaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	remaining, err := catalog.Resolve(t.Context(), "alias")
	if err != nil || len(remaining.Names) != 1 {
		t.Fatalf("hex-looking name removed aliases: %v, %v", remaining.Names, err)
	}
}

type gatedConverter struct {
	fakeConverter
	started chan struct{}
	release chan struct{}
}

func (f *gatedConverter) Convert(ctx context.Context, descriptor Descriptor, reader io.Reader, workDir string) (ConvertedLayer, error) {
	select {
	case f.started <- struct{}{}:
	case <-ctx.Done():
		return ConvertedLayer{}, ctx.Err()
	}
	select {
	case <-f.release:
	case <-ctx.Done():
		return ConvertedLayer{}, ctx.Err()
	}
	return f.fakeConverter.Convert(ctx, descriptor, reader, workDir)
}

func TestImporterConcurrentSharedLayerAndLastReferenceRemoval(t *testing.T) {
	paths, catalog := testImportState(t, nil)
	converter := &gatedConverter{started: make(chan struct{}, 2), release: make(chan struct{})}
	importer := testImporter(t, paths, catalog, converter)
	results := make(chan error, 2)
	for _, digit := range []string{"2", "3"} {
		manifest := testManifest(t, digit)
		go func() {
			_, err := importer.Import(t.Context(), digit, manifest.Platform, fakeSource{manifest: manifest})
			results <- err
		}()
	}
	for range 2 {
		select {
		case <-converter.started:
		case <-time.After(5 * time.Second):
			t.Fatal("conversion blocked on publication lock")
		}
	}
	close(converter.release)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	items, err := catalog.List(t.Context())
	if err != nil || len(items) != 2 {
		t.Fatalf("images = %d, %v", len(items), err)
	}
	if _, err := Remove(t.Context(), paths, catalog, "2"); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(t.Context(), paths, catalog, "3"); err != nil {
		t.Fatalf("shared layer deleted: %v", err)
	}
	if _, err := Remove(t.Context(), paths, catalog, "3"); err != nil {
		t.Fatal(err)
	}
	if validFile(paths.EROFS(testDigest(t, "1"))) {
		t.Fatal("unreferenced layer retained")
	}
	entries, err := os.ReadDir(paths.StagingDir())
	if err != nil || len(entries) != 0 {
		t.Fatalf("staging = %v, %v", entries, err)
	}
}

type failingStore struct {
	metadata.Store
	fail    bool
	failure error
}

func (s *failingStore) Update(ctx context.Context, fn func(metadata.Writer) error) error {
	if s.fail {
		return s.failure
	}
	return s.Store.Update(ctx, fn)
}

func TestImporterCommitFailureLeavesInvisibleOrphansAndRetryRebuilds(t *testing.T) {
	memory, err := metadata.NewMemory(Collections())
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("injected commit failure")
	store := &failingStore{Store: memory, fail: true, failure: failure}
	paths, catalog := testImportState(t, store)
	converter := &fakeConverter{}
	importer := testImporter(t, paths, catalog, converter)
	manifest := testManifest(t, "2")
	if _, err := importer.Import(t.Context(), "tiny", manifest.Platform, fakeSource{manifest: manifest}); !errors.Is(err, failure) {
		t.Fatalf("commit error = %v", err)
	}
	items, err := catalog.List(t.Context())
	if err != nil || len(items) != 0 {
		t.Fatalf("failed import became visible: %v, %v", items, err)
	}
	// A final orphan is not authorized by metadata and must be replaced, including boot files.
	if err := os.WriteFile(paths.Kernel(testDigest(t, "1")), []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.fail = false
	if _, err := importer.Import(t.Context(), "tiny", manifest.Platform, fakeSource{manifest: manifest}); err != nil {
		t.Fatal(err)
	}
	if converter.Calls() != 2 {
		t.Fatalf("retry trusted orphan; conversions = %d", converter.Calls())
	}
	if _, err := Verify(t.Context(), paths, catalog, "tiny"); err != nil {
		t.Fatal(err)
	}
}

type badConverter struct {
	fakeConverter
	fail     error
	omitBoot bool
	escape   string
}

func (f *badConverter) Convert(ctx context.Context, descriptor Descriptor, source io.Reader, workDir string) (ConvertedLayer, error) {
	if f.fail != nil {
		return ConvertedLayer{}, f.fail
	}
	artifact, err := f.fakeConverter.Convert(ctx, descriptor, source, workDir)
	if f.omitBoot {
		artifact.BootFiles = nil
	}
	if f.escape != "" {
		artifact.EROFSPath = f.escape
	}
	return artifact, err
}

func TestImporterFailureAndCancellationDoNotCommit(t *testing.T) {
	for _, name := range []string{"converter", "missing boot", "cancel", "escape"} {
		t.Run(name, func(t *testing.T) {
			paths, catalog := testImportState(t, nil)
			converter := &badConverter{}
			ctx := t.Context()
			switch name {
			case "converter":
				converter.fail = errors.New("conversion failed")
			case "missing boot":
				converter.omitBoot = true
			case "cancel":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			case "escape":
				outside := filepath.Join(t.TempDir(), "outside")
				if err := os.WriteFile(outside, []byte("erofs"), 0o600); err != nil {
					t.Fatal(err)
				}
				converter.escape = outside
			}
			importer := testImporter(t, paths, catalog, converter)
			manifest := testManifest(t, "2")
			if _, err := importer.Import(ctx, "tiny", manifest.Platform, fakeSource{manifest: manifest}); err == nil {
				t.Fatal("bad import succeeded")
			}
			items, err := catalog.List(t.Context())
			if err != nil || len(items) != 0 {
				t.Fatalf("bad import committed: %v, %v", items, err)
			}
			entries, err := os.ReadDir(paths.StagingDir())
			if err != nil || len(entries) != 0 {
				t.Fatalf("bad import left staging: %v, %v", entries, err)
			}
		})
	}
}

func TestSelectBootAppliesOverwritesAndWhiteouts(t *testing.T) {
	first, second := testDigest(t, "1"), testDigest(t, "2")
	layers := []Layer{
		{SourceDigest: first, BootFiles: []BootFile{{Name: "vmlinuz-1"}, {Name: "vmlinuz-2"}, {Name: "initrd.img"}}},
		{SourceDigest: second, Whiteouts: []string{"vmlinuz-2"}, BootFiles: []BootFile{{Name: "initrd.img"}}},
	}
	boot, err := selectBoot(layers)
	if err != nil || boot.KernelFile != "vmlinuz-1" || boot.KernelLayer != first || boot.InitrdLayer != second {
		t.Fatalf("merged boot = %+v, %v", boot, err)
	}
	layers[1].BootOpaque = true
	if _, err := selectBoot(layers); err == nil {
		t.Fatal("opaque layer retained older kernel")
	}
}

func TestImporterCancellationDuringConversionCanRetry(t *testing.T) {
	paths, catalog := testImportState(t, nil)
	converter := &gatedConverter{started: make(chan struct{}, 1), release: make(chan struct{})}
	importer := testImporter(t, paths, catalog, converter)
	manifest := testManifest(t, "2")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := importer.Import(ctx, "tiny", manifest.Platform, fakeSource{manifest: manifest})
		done <- err
	}()
	select {
	case <-converter.started:
	case <-time.After(5 * time.Second):
		t.Fatal("conversion did not start")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight cancellation = %v", err)
	}
	items, err := catalog.List(t.Context())
	if err != nil || len(items) != 0 {
		t.Fatalf("canceled import committed: %v, %v", items, err)
	}
	entries, err := os.ReadDir(paths.StagingDir())
	if err != nil || len(entries) != 0 {
		t.Fatalf("canceled staging = %v, %v", entries, err)
	}
	close(converter.release)
	if _, err := importer.Import(t.Context(), "tiny", manifest.Platform, fakeSource{manifest: manifest}); err != nil {
		t.Fatalf("retry = %v", err)
	}
}

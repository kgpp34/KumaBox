package catalog

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/images"
	imagecatalog "github.com/kumabox/kumabox/images/catalog"
	"github.com/kumabox/kumabox/metadata"
	"github.com/kumabox/kumabox/types"
)

func TestResolveRejectsDanglingNameBinding(t *testing.T) {
	store, err := metadata.NewMemory(Collections())
	if err != nil {
		t.Fatal(err)
	}
	id := types.SandboxID("123e4567-e89b-42d3-a456-426614174000")
	raw, err := json.Marshal(nameData{ID: id.String()})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(t.Context(), func(writer metadata.Writer) error {
		return writer.Put(t.Context(), CollectionNames, "dangling", raw)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(store, imagecatalog.Reader{}).Resolve(t.Context(), "dangling"); err == nil {
		t.Fatal("resolved a name whose sandbox record is missing")
	} else if code, ok := errdefs.CodeOf(err); !ok || code != errdefs.CodeArtifactCorrupt {
		t.Fatalf("Resolve error code = %q, %v; want %q", code, err, errdefs.CodeArtifactCorrupt)
	}
}

func TestReservationPinsImageInsideRemovalTransaction(t *testing.T) {
	collections := append(imagecatalog.Collections(), Collections()...)
	store, err := metadata.NewMemory(collections)
	if err != nil {
		t.Fatal(err)
	}
	imageStore := imagecatalog.New(store, imagecatalog.WithImageUsage(Usage{}))
	sandboxStore := New(store, imagecatalog.Reader{})
	manifest := testDigest(t, 'a')
	layerDigest := testDigest(t, 'b')
	erofsDigest := testDigest(t, 'c')
	kernelDigest := testDigest(t, 'd')
	initrdDigest := testDigest(t, 'e')
	layer := types.Layer{
		SourceDigest: layerDigest, EROFSDigest: erofsDigest, Size: 4096,
		BootFiles: []types.BootFile{
			{Name: "vmlinuz", Digest: kernelDigest, Size: 10},
			{Name: "initrd.img", Digest: initrdDigest, Size: 20},
		},
	}
	boot, err := images.SelectBoot([]types.Layer{layer})
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	if err := imageStore.CommitImport(t.Context(), images.ImportCommit{
		Name: "demo", Manifest: types.Manifest{Digest: manifest, Platform: types.Platform{OS: "linux", Architecture: "amd64"}, Layers: []types.Descriptor{{Digest: layerDigest, Size: 100}}},
		Layers: []types.Layer{layer}, Boot: boot, Size: layer.Size, Created: created,
	}); err != nil {
		t.Fatal(err)
	}
	id := types.SandboxID("123e4567-e89b-42d3-a456-426614174000")
	record := types.Sandbox{
		ID: id, Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
		ImageDigest: manifest, State: types.SandboxStateCreating, Generation: 1, CreatedAt: created, UpdatedAt: created,
	}
	if err := sandboxStore.Reserve(t.Context(), "demo", manifest, record); err != nil {
		t.Fatal(err)
	}
	other := record
	other.ID = types.SandboxID("223e4567-e89b-42d3-a456-426614174000")
	if err := sandboxStore.Reserve(t.Context(), "demo", manifest, other); err == nil {
		t.Fatal("reserved a duplicate sandbox name")
	} else if code, _ := errdefs.CodeOf(err); code != errdefs.CodeNameTaken {
		t.Fatalf("duplicate name error = %v", err)
	}
	if _, err := imageStore.Remove(t.Context(), "demo", manifest); err == nil {
		t.Fatal("removed an image pinned by a sandbox")
	} else if code, _ := errdefs.CodeOf(err); code != errdefs.CodeReferenced {
		t.Fatalf("Remove error = %v", err)
	}
	if _, err := imageStore.Resolve(t.Context(), "demo"); err != nil {
		t.Fatalf("referenced image removal did not roll back: %v", err)
	}
	createdRecord, err := sandboxStore.MarkCreated(t.Context(), id, 1, created.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if createdRecord.State != types.SandboxStateCreated || createdRecord.Generation != 2 {
		t.Fatalf("created record = %+v", createdRecord)
	}
	if _, err := sandboxStore.MarkCreated(t.Context(), id, 1, created.Add(2*time.Second)); err == nil {
		t.Fatal("stale generation transition succeeded")
	} else if code, _ := errdefs.CodeOf(err); code != errdefs.CodeStateConflict {
		t.Fatalf("stale transition error = %v", err)
	}
	for _, reference := range []string{"box", id.String()} {
		resolved, err := sandboxStore.Resolve(t.Context(), reference)
		if err != nil {
			t.Fatalf("Resolve %q: %v", reference, err)
		}
		if resolved.ID != id || resolved.State != types.SandboxStateCreated {
			t.Fatalf("Resolve %q = %+v", reference, resolved)
		}
	}
	deleting, err := sandboxStore.BeginDelete(t.Context(), id, createdRecord.Generation, created.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if deleting.State != types.SandboxStateDeleting || deleting.Generation != 3 {
		t.Fatalf("deleting record = %+v", deleting)
	}
	resumed, err := sandboxStore.BeginDelete(t.Context(), id, deleting.Generation, created.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Generation != deleting.Generation || !resumed.UpdatedAt.Equal(deleting.UpdatedAt) {
		t.Fatalf("resumed deletion changed record: before=%+v after=%+v", deleting, resumed)
	}
	if _, err := imageStore.Remove(t.Context(), "demo", manifest); err == nil {
		t.Fatal("removed image before sandbox deletion finalized")
	} else if code, _ := errdefs.CodeOf(err); code != errdefs.CodeReferenced {
		t.Fatalf("referenced deleting image error = %v", err)
	}
	if err := sandboxStore.FinalizeDelete(t.Context(), id, deleting.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := sandboxStore.Resolve(t.Context(), "box"); err == nil {
		t.Fatal("resolved finalized sandbox")
	} else if code, _ := errdefs.CodeOf(err); code != errdefs.CodeNotFound {
		t.Fatalf("finalized sandbox error = %v", err)
	}
	if _, err := imageStore.Remove(t.Context(), "demo", manifest); err != nil {
		t.Fatalf("remove image after sandbox finalization: %v", err)
	}
}

func testDigest(t *testing.T, char byte) types.Digest {
	t.Helper()
	value := make([]byte, 71)
	copy(value, "sha256:")
	for index := 7; index < len(value); index++ {
		value[index] = char
	}
	digest, err := types.ParseDigest(string(value))
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

package core

import (
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
)

func TestClonePullUsesSnapshotDigestAfterTagMoves(t *testing.T) {
	service, _, _ := newTestSnapshotService(t)
	base := t.TempDir()
	converter := filepath.Join(base, "mkfs.erofs")
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then printf 'mkfs.erofs 1.8.10\\n'; exit 0; fi\nfor output do :; done\n/bin/cat > \"$output\"\n"
	if err := os.WriteFile(converter, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	configuration := config.Default()
	configuration.Paths = storage.Roots{Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log")}
	configuration.Images.EROFSBinary = converter
	service.configuration = configuration

	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	defer server.Close()
	ref, err := name.NewTag(strings.TrimPrefix(server.URL, "http://")+"/guest:v1", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := layout.FromPath("../testdata/oci-layout")
	if err != nil {
		t.Fatal(err)
	}
	index, err := fixture.ImageIndex()
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := index.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	image, err := fixture.Image(manifest.Manifests[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	imageConfig, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	imageConfig.Architecture = runtime.GOARCH
	image, err = mutate.ConfigFile(image, imageConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, image, remote.WithContext(t.Context())); err != nil {
		t.Fatal(err)
	}
	identity, err := image.Digest()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := types.ParseDigest(identity.String())
	if err != nil {
		t.Fatal(err)
	}
	moved := mutate.Annotations(image, map[string]string{"revision": "next"}).(v1.Image)
	if err := remote.Write(ref, moved, remote.WithContext(t.Context())); err != nil {
		t.Fatal(err)
	}
	guard := service.lifecycle.dependencies.images.(fakeGuard)
	guard.afterUse = errdefs.New(errdefs.ClassNotFound, errdefs.CodeNotFound, errors.New("target image is absent"))
	service.lifecycle.dependencies.images = guard
	service.images = guard
	capture := types.Snapshot{ImageDigest: digest, RegistryReference: ref.String()}
	if err := service.ensureCloneImage(t.Context(), capture); err != nil {
		t.Fatal(err)
	}
	store, err := OpenImages(t.Context(), configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck // test cleanup
	stored, err := store.Catalog.Resolve(t.Context(), digest.String())
	if err != nil || stored.ManifestDigest != digest {
		t.Fatalf("pulled image = %+v, %v", stored, err)
	}
}

func TestCloneGuestScriptUsesNewIdentityAndAddress(t *testing.T) {
	record := types.Sandbox{
		Config: types.SandboxConfig{Name: "clone-box", CPUs: 2, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage, NICs: 1, NetworkName: "test"},
		Network: types.NetworkSetup{Interfaces: []types.NetworkInterface{{
			Index: 0, Name: "eth0", TAP: "new-tap", MAC: "02:00:00:00:00:02",
			Queues: 4, QueueSize: 512, Network: "test",
			IPv4: &types.IPv4Config{Address: "10.0.0.3", Gateway: "10.0.0.1", Prefix: 24},
		}}},
	}
	script, err := cloneGuestScript(record, []string{"1.1.1.1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"rm -f /etc/systemd/network/10-kumabox-*.network",
		"MACAddress=02:00:00:00:00:02", "Address=10.0.0.3/24", "Gateway=10.0.0.1",
		"DNS=1.1.1.1", "hostname 'clone-box'", "systemctl restart systemd-networkd",
	} {
		if !strings.Contains(script, expected) {
			t.Fatalf("guest script misses %q: %s", expected, script)
		}
	}
}

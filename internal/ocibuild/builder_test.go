// SPDX-License-Identifier: MIT

package ocibuild

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kumabox/kumabox/internal/ocistore"
)

func TestEnsureEROFSBuildsAndReusesLayer(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	layerPath := filepath.Join(dir, "layer.tar.gz")
	layerBytes := gzipTar(t, map[string]string{"hello.txt": "hello"})
	if err := os.WriteFile(layerPath, layerBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(layerBytes)
	layerDigest := "sha256:" + hex.EncodeToString(sum[:])

	mkfs := filepath.Join(dir, "mkfs.erofs")
	if err := os.WriteFile(mkfs, []byte("#!/bin/sh\ncat > \"$7\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	builder := New(dir)
	rec, err := builder.ensureEROFS(context.Background(), mkfs, ocistore.BlobRecord{
		Digest:    layerDigest,
		Path:      layerPath,
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		SizeBytes: int64(len(layerBytes)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Filesystem != "erofs" || rec.SourceLayer != layerDigest {
		t.Fatalf("unexpected EROFS record: %+v", rec)
	}
	if _, err := os.Stat(rec.Path); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(rec.Path) != strings.TrimPrefix(layerDigest, "sha256:")+".erofs" {
		t.Fatalf("unexpected EROFS path: %s", rec.Path)
	}

	cached, err := builder.ensureEROFS(context.Background(), mkfs, ocistore.BlobRecord{
		Digest:    layerDigest,
		Path:      layerPath,
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		SizeBytes: int64(len(layerBytes)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cached.Path != rec.Path || cached.Digest != rec.Digest {
		t.Fatalf("cached record = %+v, want %+v", cached, rec)
	}
}

func TestResolveBootProfileExtractsKernelAndInitrd(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	layerPath := filepath.Join(dir, "layer.tar")
	layerBytes := plainTar(t, map[string]string{
		"boot/vmlinuz-6.8.0":    "kernel",
		"boot/initrd.img-6.8.0": "initrd",
	})
	if err := os.WriteFile(layerPath, layerBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(layerBytes)

	boot, err := New(dir).resolveBootProfile([]ocistore.BlobRecord{{
		Digest:    "sha256:" + hex.EncodeToString(sum[:]),
		Path:      layerPath,
		MediaType: "application/vnd.oci.image.layer.v1.tar",
		SizeBytes: int64(len(layerBytes)),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if boot.Mode != "direct" || boot.Kernel == "" || boot.Initrd == "" || boot.Cmdline == "" {
		t.Fatalf("boot profile = %+v", boot)
	}
	for _, path := range []string{boot.Kernel, boot.Initrd} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(boot.Cmdline, "kumabox.layers={{layers}}") || !strings.Contains(boot.Cmdline, "kumabox.cow={{cow}}") {
		t.Fatalf("cmdline template missing overlay placeholders: %s", boot.Cmdline)
	}
	if !strings.Contains(boot.Cmdline, "boot=kumabox-overlay") || strings.Contains(boot.Cmdline, "root=/dev/ram0") {
		t.Fatalf("cmdline template does not select KumaBox overlay boot: %s", boot.Cmdline)
	}
	if !strings.Contains(boot.Cmdline, "loglevel=3") || !strings.Contains(boot.Cmdline, "clocksource=kvm-clock") {
		t.Fatalf("cmdline template is missing fast-boot parameters: %s", boot.Cmdline)
	}
}

func TestResolveBootProfileRejectsMissingAssets(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	layerPath := filepath.Join(dir, "layer.tar")
	layerBytes := plainTar(t, map[string]string{"etc/os-release": "ID=test"})
	if err := os.WriteFile(layerPath, layerBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(layerBytes)

	_, err := New(dir).resolveBootProfile([]ocistore.BlobRecord{{
		Digest:    "sha256:" + hex.EncodeToString(sum[:]),
		Path:      layerPath,
		MediaType: "application/vnd.oci.image.layer.v1.tar",
		SizeBytes: int64(len(layerBytes)),
	}})
	if err == nil || !strings.Contains(err.Error(), "BOOT_PROFILE_UNSUPPORTED") {
		t.Fatalf("expected BOOT_PROFILE_UNSUPPORTED, got %v", err)
	}
}

func TestDecodeOCIImageConfigPreservesExecutionMetadata(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{
		"config": {
			"Env": ["A=1", "B=2"],
			"Cmd": ["/sbin/init"],
			"Entrypoint": [],
			"WorkingDir": "/work",
			"User": "1000:1000",
			"Labels": {"org.opencontainers.image.title": "kumabox"}
		}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := decodeOCIImageConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env == nil || strings.Join(*cfg.Env, ",") != "A=1,B=2" {
		t.Fatalf("env = %#v", cfg.Env)
	}
	if cfg.Cmd == nil || strings.Join(*cfg.Cmd, ",") != "/sbin/init" {
		t.Fatalf("cmd = %#v", cfg.Cmd)
	}
	if cfg.Entrypoint == nil || len(*cfg.Entrypoint) != 0 {
		t.Fatalf("entrypoint = %#v", cfg.Entrypoint)
	}
	if cfg.Workdir == nil || *cfg.Workdir != "/work" {
		t.Fatalf("workdir = %#v", cfg.Workdir)
	}
	if cfg.User == nil || *cfg.User != "1000:1000" {
		t.Fatalf("user = %#v", cfg.User)
	}
	if cfg.Labels == nil || (*cfg.Labels)["org.opencontainers.image.title"] != "kumabox" {
		t.Fatalf("labels = %#v", cfg.Labels)
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"entrypoint":[]`) {
		t.Fatalf("explicit empty entrypoint was not preserved: %s", raw)
	}
}

func TestDecodeOCIImageConfigOmitsMissingFields(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"config":{"Cmd":[]}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := decodeOCIImageConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cmd == nil || len(*cfg.Cmd) != 0 {
		t.Fatalf("cmd = %#v", cfg.Cmd)
	}
	if cfg.Env != nil || cfg.Entrypoint != nil || cfg.Workdir != nil || cfg.User != nil || cfg.Labels != nil {
		t.Fatalf("missing fields should remain nil: %+v", cfg)
	}
}

func TestInspectAgentProfileDetectsEmbeddedAgent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	layerPath := filepath.Join(dir, "layer.tar")
	layerBytes := plainTar(t, map[string]string{
		"usr/local/bin/kumabox-agent":              "agent",
		"etc/systemd/system/kumabox-agent.service": "unit",
	})
	if err := os.WriteFile(layerPath, layerBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	profile, err := New(dir).inspectAgentProfile([]ocistore.BlobRecord{{
		Path:      layerPath,
		MediaType: "application/vnd.oci.image.layer.v1.tar",
	}}, "required")
	if err != nil {
		t.Fatal(err)
	}
	if profile.Injection != "embedded" || profile.BinaryPath == "" || profile.ServicePath == "" {
		t.Fatalf("profile = %+v", profile)
	}
	if len(profile.Capabilities) != 5 {
		t.Fatalf("capabilities = %v", profile.Capabilities)
	}
}

func TestInspectAgentProfileRejectsRequiredAgentWhenMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	layerPath := filepath.Join(dir, "layer.tar")
	if err := os.WriteFile(layerPath, plainTar(t, map[string]string{"etc/os-release": "ID=ubuntu"}), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := New(dir).inspectAgentProfile([]ocistore.BlobRecord{{
		Path:      layerPath,
		MediaType: "application/vnd.oci.image.layer.v1.tar",
	}}, "required")
	if err == nil || !strings.Contains(err.Error(), "AGENT_INJECTION_FAILED") {
		t.Fatalf("error = %v, want AGENT_INJECTION_FAILED", err)
	}
}

func gzipTar(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	tw := tar.NewWriter(gz)
	writeTarFiles(t, tw, files)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func plainTar(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	writeTarFiles(t, tw, files)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func writeTarFiles(t *testing.T, tw *tar.Writer, files map[string]string) {
	t.Helper()

	for name, content := range files {
		body := []byte(content)
		if err := tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(body)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
}

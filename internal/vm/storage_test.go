package vm

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateStorageContract(t *testing.T) {
	rootDir := t.TempDir()
	ownerDir := filepath.Join(rootDir, "storage", "vms", "kb_test")
	baseRecord := func() *VMRecord {
		return &VMRecord{
			ID:     "kb_test",
			RunDir: filepath.Join(rootDir, "run", "vms", "kb_test"),
			Image:  &ImageRef{ID: "img_oci", BootMode: "direct"},
			StorageConfigs: []StorageConfig{
				{
					ID:         "layer0",
					Role:       StorageRoleLayer,
					Path:       filepath.Join(rootDir, "oci", "layer.erofs"),
					Readonly:   true,
					Format:     "raw",
					Filesystem: "erofs",
				},
				{
					ID:               "cow",
					Role:             StorageRoleCOW,
					Path:             filepath.Join(ownerDir, "cow.ext4"),
					Format:           "raw",
					Filesystem:       "ext4",
					VirtualSizeBytes: 64 << 20,
					Base: &StorageBase{
						Family:       "oci",
						ImageID:      "img_oci",
						Digest:       "sha256:manifest",
						LayerDigests: []string{"sha256:layer"},
					},
				},
			},
		}
	}

	tests := []struct {
		name   string
		mutate func(*VMRecord)
		valid  bool
	}{
		{name: "valid OCI contract", valid: true},
		{name: "read-only COW", mutate: func(rec *VMRecord) { rec.StorageConfigs[1].Readonly = true }},
		{name: "writable layer", mutate: func(rec *VMRecord) { rec.StorageConfigs[0].Readonly = false }},
		{name: "unsupported role", mutate: func(rec *VMRecord) { rec.StorageConfigs[1].Role = "cache" }},
		{name: "duplicate id", mutate: func(rec *VMRecord) { rec.StorageConfigs[1].ID = "layer0" }},
		{name: "unsafe id", mutate: func(rec *VMRecord) { rec.StorageConfigs[1].ID = "../cow" }},
		{name: "writable path outside owner", mutate: func(rec *VMRecord) { rec.StorageConfigs[1].Path = filepath.Join(rootDir, "escape.ext4") }},
		{name: "missing base digest", mutate: func(rec *VMRecord) { rec.StorageConfigs[1].Base.Digest = "" }},
		{name: "wrong OCI format", mutate: func(rec *VMRecord) { rec.StorageConfigs[1].Format = "qcow2" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := cloneRecord(baseRecord())
			if tt.mutate != nil {
				tt.mutate(rec)
			}
			err := ValidateStorageContract(rec, rootDir)
			if tt.valid && err != nil {
				t.Fatalf("ValidateStorageContract() error = %v", err)
			}
			if !tt.valid && !errors.Is(err, ErrInvalidStorageContract) {
				t.Fatalf("ValidateStorageContract() error = %v, want ErrInvalidStorageContract", err)
			}
		})
	}
}

func TestStoreReadsLegacyP3StorageRecord(t *testing.T) {
	rootDir := t.TempDir()
	backendDir := filepath.Join(rootDir, "backends", backendCloudHypervisor)
	if err := os.MkdirAll(backendDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(rootDir, "run", "vms", "kb_legacy")
	index := `{
  "vms": {
    "kb_legacy": {
      "id": "kb_legacy",
      "name": "legacy",
      "backend": "cloud-hypervisor",
      "state": "stopped",
      "image": {"id":"img_legacy","name":"legacy","bootMode":"direct"},
      "storageConfigs": [
        {"id":"layer0","type":"layer","path":"/tmp/layer.erofs","readonly":true,"imageType":"raw","filesystem":"erofs"},
        {"id":"cow","type":"cow","path":"` + filepath.ToSlash(filepath.Join(runDir, "cow.ext4")) + `","imageType":"raw","filesystem":"ext4","sizeBytes":67108864}
      ],
      "runDir": "` + filepath.ToSlash(runDir) + `",
      "logDir": "` + filepath.ToSlash(filepath.Join(rootDir, "log", "vms", "kb_legacy")) + `",
      "config": "` + filepath.ToSlash(filepath.Join(runDir, "cloud-hypervisor.json")) + `",
      "createdAt": "2026-07-14T00:00:00Z",
      "updatedAt": "2026-07-14T00:00:00Z"
    }
  },
  "names": {"legacy":"kb_legacy"}
}`
	if err := os.WriteFile(filepath.Join(backendDir, "index.json"), []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}

	rec, err := New(rootDir).Inspect("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if rec.StorageConfigs[0].EffectiveRole() != StorageRoleLayer || rec.StorageConfigs[1].EffectiveRole() != StorageRoleCOW {
		t.Fatalf("legacy roles were not resolved: %+v", rec.StorageConfigs)
	}
	if rec.StorageConfigs[1].EffectiveVirtualSize() != 64<<20 {
		t.Fatalf("legacy size = %d", rec.StorageConfigs[1].EffectiveVirtualSize())
	}
}

func TestCreatePlacesCOWInDurableOwnerDirectory(t *testing.T) {
	rootDir := t.TempDir()
	store := New(rootDir)
	rec, err := store.Create(CreateRequest{
		Name:   "durable-cow",
		Kernel: "vmlinuz",
		Initrd: "initrd",
		Image:  &ImageRef{ID: "img_oci", Name: "oci", BootMode: "direct"},
		StorageConfigs: []StorageConfig{
			{ID: "layer0", Role: StorageRoleLayer, Path: filepath.Join(rootDir, "layer.erofs"), Readonly: true, Format: "raw", Filesystem: "erofs"},
			{ID: "cow", Role: StorageRoleCOW, Format: "raw", Filesystem: "ext4", VirtualSizeBytes: 64 << 20, Base: &StorageBase{Family: "oci", ImageID: "img_oci", Digest: "sha256:manifest", LayerDigests: []string{"sha256:layer"}}},
		},
		RunDir: filepath.Join(rootDir, "run"),
		LogDir: filepath.Join(rootDir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(rootDir, "storage", "vms", rec.ID, "cow.ext4")
	if rec.StorageConfigs[1].Path != want {
		t.Fatalf("COW path = %s, want %s", rec.StorageConfigs[1].Path, want)
	}
}

func TestCreateNormalizesManagedDataDisks(t *testing.T) {
	rootDir := t.TempDir()
	store := New(rootDir)
	rec, err := store.Create(CreateRequest{
		Name: "data-disks", Kernel: "vmlinuz", Initrd: "initrd",
		Image:          &ImageRef{ID: "img_oci", Name: "oci", BootMode: "direct"},
		StorageConfigs: []StorageConfig{{ID: "layer0", Role: StorageRoleLayer, Path: filepath.Join(rootDir, "layer.erofs"), Readonly: true, Format: FormatRaw, Filesystem: FilesystemEROFS}, {ID: "cow", Role: StorageRoleCOW, Format: FormatRaw, Filesystem: FilesystemEXT4, VirtualSizeBytes: 64 << 20, Base: &StorageBase{Family: BaseFamilyOCI, ImageID: "img_oci", Digest: "sha256:manifest", LayerDigests: []string{"sha256:layer"}}}},
		DataDisks:      []DataDiskRequest{{Name: "workspace", SizeBytes: 16 << 20}},
		RunDir:         filepath.Join(rootDir, "run"), LogDir: filepath.Join(rootDir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.StorageConfigs) != 3 {
		t.Fatalf("storage count = %d, want 3", len(rec.StorageConfigs))
	}
	disk := rec.StorageConfigs[2]
	if disk.ID != "data-workspace" || disk.Serial != "workspace" || disk.MountPoint != "/mnt/workspace" || disk.Filesystem != FilesystemEXT4 {
		t.Fatalf("data disk = %+v", disk)
	}
	wantPath := filepath.Join(rootDir, "storage", "vms", rec.ID, "data-workspace.raw")
	if disk.Path != wantPath {
		t.Fatalf("data disk path = %s, want %s", disk.Path, wantPath)
	}
}

func TestCreateRejectsInvalidManagedDataDisk(t *testing.T) {
	rootDir := t.TempDir()
	store := New(rootDir)
	_, err := store.Create(CreateRequest{
		Name: "invalid-data", Kernel: "vmlinuz", Initrd: "initrd", RootDisk: filepath.Join(rootDir, "root.raw"),
		DataDisks: []DataDiskRequest{{Name: "bad.name", SizeBytes: 16 << 20}},
		RunDir:    filepath.Join(rootDir, "run"), LogDir: filepath.Join(rootDir, "log"),
	})
	if err == nil {
		t.Fatal("Create() error = nil, want invalid data disk error")
	}
}

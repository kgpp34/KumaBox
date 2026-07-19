package vmstore

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrInvalidStorageContract identifies a VM record whose disk ownership or
// immutable backing references violate KumaBox storage invariants.
var ErrInvalidStorageContract = errors.New("invalid storage contract")

// ValidateStorageContract validates the durable disk model for a VM record.
// Legacy P3 Type/ImageType records remain readable, but newly modeled writable
// disks must live under the VM's durable owner directory.
func ValidateStorageContract(rec *VMRecord, rootDir string) error {
	if rec == nil {
		return storageError("VM record is nil")
	}
	if rec.ID == "" || rec.ID == "." || rec.ID == ".." || strings.ContainsAny(rec.ID, `/\\`) {
		return storageError("VM id %q is not safe for managed paths", rec.ID)
	}
	if len(rec.StorageConfigs) == 0 {
		return nil
	}

	ownerDir := filepath.Join(rootDir, "storage", "vms", rec.ID)
	ids := make(map[string]struct{}, len(rec.StorageConfigs))
	cowCount := 0
	for i, storage := range rec.StorageConfigs {
		if storage.ID == "" || storage.ID == "." || storage.ID == ".." || strings.ContainsAny(storage.ID, `/\\`) {
			return storageError("storage %d has unsafe id %q", i, storage.ID)
		}
		if _, exists := ids[storage.ID]; exists {
			return storageError("duplicate storage id %q", storage.ID)
		}
		ids[storage.ID] = struct{}{}

		role := storage.EffectiveRole()
		if !validStorageRole(role) {
			return storageError("storage %q has unsupported role %q", storage.ID, role)
		}
		if storage.Path == "" || !filepath.IsAbs(storage.Path) {
			return storageError("storage %q path must be absolute", storage.ID)
		}
		if err := validateStorageAccess(storage, role); err != nil {
			return err
		}
		if err := validateStorageShape(storage, role); err != nil {
			return err
		}

		if role == StorageRoleCOW || role == StorageRoleData {
			legacy := storage.Role == "" && storage.Type != ""
			if !pathWithin(storage.Path, ownerDir) && !(legacy && pathWithin(storage.Path, rec.RunDir)) {
				return storageError("writable storage %q is outside VM owner directory", storage.ID)
			}
		}
		if role == StorageRoleCOW {
			cowCount++
			if err := validateCOWBase(rec, storage); err != nil {
				return err
			}
		}
	}
	if cowCount > 1 {
		return storageError("VM has %d root COW disks; at most one is allowed", cowCount)
	}
	return nil
}

func validStorageRole(role StorageRole) bool {
	switch role {
	case StorageRoleLayer, StorageRoleBase, StorageRoleCOW, StorageRoleData, StorageRoleCidata:
		return true
	default:
		return false
	}
}

func validateStorageAccess(storage StorageConfig, role StorageRole) error {
	wantReadonly := role == StorageRoleLayer || role == StorageRoleBase || role == StorageRoleCidata
	if storage.Readonly != wantReadonly {
		access := "writable"
		if wantReadonly {
			access = "read-only"
		}
		return storageError("storage %q with role %q must be %s", storage.ID, role, access)
	}
	return nil
}

func validateStorageShape(storage StorageConfig, role StorageRole) error {
	format := storage.EffectiveFormat()
	switch role {
	case StorageRoleLayer:
		if format != FormatRaw || storage.Filesystem != FilesystemEROFS {
			return storageError("layer %q must use raw EROFS", storage.ID)
		}
	case StorageRoleCOW:
		if format == FormatRaw && storage.Filesystem == FilesystemEXT4 {
			break
		}
		if format == FormatQCOW2 && storage.Filesystem == "" {
			break
		}
		return storageError("COW storage %q must use raw ext4 or qcow2", storage.ID)
	case StorageRoleData:
		if format != FormatRaw && format != FormatQCOW2 {
			return storageError("data storage %q must use raw or qcow2 format", storage.ID)
		}
	case StorageRoleCidata:
		if format != FormatRaw {
			return storageError("cidata storage %q must use raw format", storage.ID)
		}
	case StorageRoleBase:
		if format != FormatQCOW2 && format != FormatRaw {
			return storageError("base storage %q must use raw or qcow2 format", storage.ID)
		}
	}
	if (role == StorageRoleCOW || role == StorageRoleData) && storage.EffectiveVirtualSize() <= 0 {
		return storageError("writable storage %q virtual size must be positive", storage.ID)
	}
	return nil
}

func validateCOWBase(rec *VMRecord, storage StorageConfig) error {
	base := storage.Base
	if base == nil {
		if storage.Role == "" && storage.Type != "" {
			return nil
		}
		return storageError("COW storage %q has no immutable base reference", storage.ID)
	}
	if base.Family != "cloudimg" && base.Family != "oci" {
		return storageError("COW storage %q has unsupported base family %q", storage.ID, base.Family)
	}
	if base.ImageID == "" || base.Digest == "" {
		return storageError("COW storage %q base image id and digest are required", storage.ID)
	}
	if rec.Image == nil || rec.Image.ID != base.ImageID {
		return storageError("COW storage %q base image does not match VM image", storage.ID)
	}
	format := storage.EffectiveFormat()
	if base.Family == "cloudimg" {
		if format != FormatQCOW2 || base.Format != FormatQCOW2 || base.Path == "" {
			return storageError("cloudimg COW storage %q requires a qcow2 base path", storage.ID)
		}
		return nil
	}
	if format != FormatRaw || storage.Filesystem != FilesystemEXT4 || len(base.LayerDigests) == 0 {
		return storageError("OCI COW storage %q requires raw ext4 and layer digests", storage.ID)
	}
	return nil
}

func pathWithin(path, parent string) bool {
	if path == "" || parent == "" {
		return false
	}
	rel, err := filepath.Rel(parent, path)
	if err != nil {
		return false
	}
	return rel != ".." && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func storageError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidStorageContract, fmt.Sprintf(format, args...))
}

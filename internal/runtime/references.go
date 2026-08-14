package runtime

import (
	"context"

	"github.com/kumabox/kumabox/internal/reference"
	"github.com/kumabox/kumabox/internal/vm"
)

const (
	referenceKindVM       = "vm"
	referenceKindImage    = "image"
	referenceKindSnapshot = "snapshot"
)

func imageReferenceID(vmID string) string { return "vm-image:" + vmID }
func snapshotImageReferenceID(snapshotID string) string {
	return "snapshot-image:" + snapshotID
}
func vmSnapshotReferenceID(vmID, snapshotID string) string {
	return "vm-snapshot:" + vmID + ":" + snapshotID
}

func (r *Runtime) recordVMImageReference(ctx context.Context, rec *vm.VMRecord) error {
	if r.storeSet.References == nil || rec == nil || rec.Image == nil || rec.Image.ID == "" {
		return nil
	}
	return r.storeSet.References.Upsert(ctx, reference.Record{
		ID: imageReferenceID(rec.ID), SourceKind: referenceKindVM, SourceID: rec.ID,
		TargetKind: referenceKindImage, TargetID: rec.Image.ID, Mode: "runtime",
	})
}

func (r *Runtime) recordSnapshotImageReference(ctx context.Context, snapshotID, imageID string) error {
	if r.storeSet.References == nil || snapshotID == "" || imageID == "" {
		return nil
	}
	return r.storeSet.References.Upsert(ctx, reference.Record{
		ID: snapshotImageReferenceID(snapshotID), SourceKind: referenceKindSnapshot, SourceID: snapshotID,
		TargetKind: referenceKindImage, TargetID: imageID, Mode: "base",
	})
}

func (r *Runtime) recordVMSnapshotReference(ctx context.Context, vmID, snapshotID string) error {
	if r.storeSet.References == nil || vmID == "" || snapshotID == "" {
		return nil
	}
	return r.storeSet.References.Upsert(ctx, reference.Record{
		ID: vmSnapshotReferenceID(vmID, snapshotID), SourceKind: referenceKindVM, SourceID: vmID,
		TargetKind: referenceKindSnapshot, TargetID: snapshotID, Mode: "restore",
	})
}

func (r *Runtime) removeVMSnapshotReference(ctx context.Context, vmID, snapshotID string) error {
	if r.storeSet.References == nil || vmID == "" || snapshotID == "" {
		return nil
	}
	return r.storeSet.References.Delete(ctx, vmSnapshotReferenceID(vmID, snapshotID))
}

func (r *Runtime) removeVMReferences(ctx context.Context, vmID string) error {
	if r.storeSet.References == nil {
		return nil
	}
	return r.storeSet.References.DeleteSource(ctx, referenceKindVM, vmID)
}

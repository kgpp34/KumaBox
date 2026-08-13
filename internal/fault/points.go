package fault

const (
	MetadataJSONBeforeRename Point = "metadata.json.before-rename"
	MetadataJSONAfterRename  Point = "metadata.json.after-rename"
	MetadataConvertNamespace Point = "metadata.convert.after-namespace"
	MetadataConvertAfterCopy Point = "metadata.convert.after-copy"
	MetadataConvertRetired   Point = "metadata.convert.after-source-retire"
	MetadataBackupBeforeSwap Point = "metadata.backup.before-swap"
	SnapshotBeforePublish    Point = "snapshot.before-publish"
	SnapshotAfterRename      Point = "snapshot.after-rename"
	NetworkAfterAdd          Point = "network.after-add"
	NetworkAfterDelete       Point = "network.after-del"
	CloneAfterStage          Point = "clone.after-stage"
	CloneAfterDiskCommit     Point = "clone.after-disk-commit"
	DeleteBeforeRecordDelete Point = "delete.before-record-delete"
	GCBeforeDelete           Point = "gc.before-delete"
)

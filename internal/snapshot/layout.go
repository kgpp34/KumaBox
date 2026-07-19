package snapshot

// Snapshot payload layout is part of the on-disk compatibility contract.
const (
	ManifestFile       = "snapshot.json"
	NativePayloadDir   = "native"
	DiskPayloadDir     = "disks"
	NativeConfigFile   = "config.json"
	NativeStateFile    = "state.json"
	NativeMemoryPrefix = "memory-range"
	NativePathPrefix   = NativePayloadDir + "/"
	DiskPathPrefix     = DiskPayloadDir + "/"
	NativeType         = "native"
	NativeSchemaV2     = "kumabox.snapshot.v2"
)

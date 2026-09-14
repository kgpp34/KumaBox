package errdefs

// Code is a stable machine-readable failure code.
type Code string

const (
	CodeNotFound            Code = "NOT_FOUND"
	CodeNameTaken           Code = "NAME_TAKEN"
	CodeInvalidArgument     Code = "INVALID_ARGUMENT"
	CodeHostIncompatible    Code = "HOST_INCOMPATIBLE"
	CodeDigestMismatch      Code = "IMAGE_DIGEST_MISMATCH"
	CodeArtifactCorrupt     Code = "ARTIFACT_CORRUPT"
	CodeArtifactUnavailable Code = "ARTIFACT_UNAVAILABLE"
	CodeReferenced          Code = "REFERENCED"
	CodeStoreBusy           Code = "STORE_BUSY"
	CodeInternal            Code = "INTERNAL"
)

// Class groups codes that share handling policy.
type Class uint8

const (
	ClassUnknown Class = iota
	ClassNotFound
	ClassInvalid
	ClassConflict
	ClassUnavailable
	ClassCorrupt
	ClassInternal
)

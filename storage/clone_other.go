//go:build !linux

package storage

// CloneFile falls back to sparse copying on platforms without Linux reflinks.
func CloneFile(destination, source string) error { return CopySparse(destination, source) }

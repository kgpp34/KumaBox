//go:build !linux

package storage

import "context"

func copyPlatform(ctx context.Context, source, destination string) (string, error) {
	return bufferedCopy(ctx, source, destination)
}

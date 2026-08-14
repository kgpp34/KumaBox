//go:build !linux

package disk

import "context"

func copyPlatform(ctx context.Context, source, destination string) (string, error) {
	return bufferedCopy(ctx, source, destination)
}

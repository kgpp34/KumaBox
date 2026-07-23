package fileutil

import (
	"errors"
	"fmt"
	"io"
)

// CloseAndJoin closes a resource during deferred cleanup without discarding
// the error. A cleanup failure is joined with the operation error so the
// original failure remains discoverable with errors.Is and errors.As.
func CloseAndJoin(errp *error, resource io.Closer, description string) {
	if errp == nil || resource == nil {
		return
	}
	if err := resource.Close(); err != nil {
		*errp = errors.Join(*errp, fmt.Errorf("%s: %w", description, err))
	}
}

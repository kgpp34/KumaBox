package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/kumabox/kumabox/internal/operation"
)

func (r *Runtime) beginOperation(ctx context.Context, kind, resourceID string) (string, error) {
	return r.beginOperationWithRelated(ctx, kind, resourceID, "")
}

func (r *Runtime) beginOperationWithRelated(ctx context.Context, kind, resourceID, relatedID string) (string, error) {
	if r.operations == nil {
		return "", nil
	}
	id, err := operation.NewID()
	if err != nil {
		return "", err
	}
	if _, err := r.operations.BeginWithRelated(ctx, id, kind, resourceID, relatedID); err != nil {
		return "", err
	}
	return id, nil
}

func (r *Runtime) finishOperation(ctx context.Context, id string, operationErr error) error {
	if id == "" || r.operations == nil {
		return operationErr
	}
	var recordErr error
	if operationErr != nil {
		_, recordErr = r.operations.Fail(ctx, id, operationErr.Error())
	} else {
		_, recordErr = r.operations.Complete(ctx, id)
	}
	if recordErr != nil {
		return errors.Join(operationErr, fmt.Errorf("record operation %s: %w", id, recordErr))
	}
	return operationErr
}

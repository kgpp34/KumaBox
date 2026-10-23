package cloudhypervisor

import (
	"context"
	"errors"
	"io"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/vmm"
)

// DialVsock verifies the exact VMM process and opens one guest port through
// Cloud Hypervisor's hybrid Unix-socket transport.
func (d *Driver) DialVsock(ctx context.Context, process vmm.Process, port uint32) (io.ReadWriteCloser, error) {
	if port == 0 {
		return nil, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("vsock port must be positive"))
	}
	if err := process.Validate(); err != nil {
		return nil, err
	}
	alive, err := vmm.VerifyProcess(process)
	if err != nil {
		return nil, err
	}
	if !alive {
		return nil, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, errors.New("cloud-hypervisor process is absent"))
	}
	socket, err := d.paths.Vsock(process.SandboxID)
	if err != nil {
		return nil, err
	}
	connection, err := vmm.DialHybridVsock(ctx, socket, port)
	if err != nil {
		return nil, err
	}
	alive, err = vmm.VerifyProcess(process)
	if err != nil || !alive {
		_ = connection.Close()
		if err != nil {
			return nil, err
		}
		return nil, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, errors.New("cloud-hypervisor exited while opening guest vsock"))
	}
	return connection, nil
}

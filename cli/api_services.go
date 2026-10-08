package cli

import (
	"context"
	"io"

	"github.com/kumabox/kumabox/api"
	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/types"
)

// apiServices adapt the public HTTP contracts to the application services.
// They are assembled beside the CLI entry point, preserving the direction
// from application composition toward API transport.
type apiSandboxService struct{ service *core.SandboxService }

func (a apiSandboxService) Create(ctx context.Context, input api.CreateSandboxInput) (types.Sandbox, error) {
	return a.service.Create(ctx, core.CreateSandboxRequest{ImageReference: input.ImageReference, Config: input.Config})
}

func (a apiSandboxService) Run(ctx context.Context, input api.CreateSandboxInput) (types.Sandbox, error) {
	return a.service.Run(ctx, core.CreateSandboxRequest{ImageReference: input.ImageReference, Config: input.Config})
}

func (a apiSandboxService) List(ctx context.Context, all bool) ([]types.Sandbox, error) {
	return a.service.List(ctx, all)
}

func (a apiSandboxService) Inspect(ctx context.Context, ref string) (types.Sandbox, error) {
	return a.service.Inspect(ctx, ref)
}

func (a apiSandboxService) Start(ctx context.Context, ref string) (types.Sandbox, error) {
	return a.service.Start(ctx, ref)
}

func (a apiSandboxService) Stop(ctx context.Context, ref string) (types.Sandbox, error) {
	return a.service.Stop(ctx, ref)
}

func (a apiSandboxService) Remove(ctx context.Context, ref string) (types.Sandbox, error) {
	return a.service.Remove(ctx, ref)
}

func (a apiSandboxService) Exec(ctx context.Context, ref string, command types.Command, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	return a.service.Exec(ctx, ref, command, stdin, stdout, stderr)
}

type apiSnapshotService struct{ service *core.SnapshotService }

func (a apiSnapshotService) Save(ctx context.Context, input api.SaveSnapshotInput) (types.Snapshot, error) {
	return a.service.Save(ctx, core.SaveSnapshotRequest{
		SandboxReference: input.SandboxReference, Name: input.Name, Description: input.Description,
	})
}

func (a apiSnapshotService) Hibernate(ctx context.Context, input api.SaveSnapshotInput) (types.Snapshot, error) {
	return a.service.Hibernate(ctx, core.SaveSnapshotRequest{
		SandboxReference: input.SandboxReference, Name: input.Name, Description: input.Description,
	})
}

func (a apiSnapshotService) List(ctx context.Context) ([]types.Snapshot, error) {
	return a.service.List(ctx)
}

func (a apiSnapshotService) Inspect(ctx context.Context, ref string) (types.Snapshot, error) {
	return a.service.Inspect(ctx, ref)
}

func (a apiSnapshotService) Remove(ctx context.Context, ref string) (types.Snapshot, error) {
	return a.service.Remove(ctx, ref)
}

func (a apiSnapshotService) Clone(ctx context.Context, ref, name string) (types.Sandbox, error) {
	return a.service.Clone(ctx, ref, name)
}

func (a apiSnapshotService) Restore(ctx context.Context, sandbox, snapshot string) (types.Sandbox, error) {
	return a.service.Restore(ctx, sandbox, snapshot)
}

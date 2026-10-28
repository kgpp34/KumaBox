package daemon

import (
	"context"
	"net/http"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/rpc"
	"github.com/kumabox/kumabox/types"
)

func registerOperations(mux *http.ServeMux, app *core.Application) {
	rpc.Register(mux, "sandbox.create", func(ctx context.Context, session *rpc.Session, p types.CreateSandboxRequest) (any, error) {
		service := app.Sandboxes.WithReporter(sandboxReporter{session})
		return service.Create(ctx, p)
	})
	rpc.Register(mux, "sandbox.run", func(ctx context.Context, session *rpc.Session, p types.CreateSandboxRequest) (any, error) {
		service := app.Sandboxes.WithReporter(sandboxReporter{session})
		return service.Run(ctx, p)
	})
	rpc.Register(mux, "sandbox.list", func(ctx context.Context, session *rpc.Session, p bool) (any, error) {
		service := app.Sandboxes.WithReporter(sandboxReporter{session})
		return service.List(ctx, p)
	})
	rpc.Register(mux, "sandbox.inspect", func(ctx context.Context, session *rpc.Session, p rpc.Ref) (any, error) {
		service := app.Sandboxes.WithReporter(sandboxReporter{session})
		return service.Inspect(ctx, p.Reference)
	})
	rpc.Register(mux, "sandbox.start", func(ctx context.Context, session *rpc.Session, p rpc.Ref) (any, error) {
		service := app.Sandboxes.WithReporter(sandboxReporter{session})
		return service.Start(ctx, p.Reference)
	})
	rpc.Register(mux, "sandbox.stop", func(ctx context.Context, session *rpc.Session, p rpc.Target[types.StopOptions]) (any, error) {
		service := app.Sandboxes.WithReporter(sandboxReporter{session})
		return service.StopWithOptions(ctx, p.Reference, p.Options)
	})
	rpc.Register(mux, "sandbox.remove", func(ctx context.Context, session *rpc.Session, p rpc.Ref) (any, error) {
		service := app.Sandboxes.WithReporter(sandboxReporter{session})
		return service.Remove(ctx, p.Reference)
	})
	rpc.Register(mux, "sandbox.status", func(ctx context.Context, session *rpc.Session, p []string) (any, error) {
		service := app.Sandboxes.WithReporter(sandboxReporter{session})
		return service.Status(ctx, p...)
	})
	rpc.Register(mux, "sandbox.reseed", func(ctx context.Context, session *rpc.Session, p rpc.Target[bool]) (any, error) {
		service := app.Sandboxes.WithReporter(sandboxReporter{session})
		return nil, service.Reseed(ctx, p.Reference, p.Options)
	})
	rpc.Register(mux, "sandbox.net.configure", func(ctx context.Context, session *rpc.Session, p rpc.Ref) (any, error) {
		service := app.Sandboxes.WithReporter(sandboxReporter{session})
		return service.NetConfigure(ctx, p.Reference)
	})
	rpc.Register(mux, "sandbox.net.resize", func(ctx context.Context, session *rpc.Session, p rpc.Target[int]) (any, error) {
		service := app.Sandboxes.WithReporter(sandboxReporter{session})
		return service.NetResize(ctx, p.Reference, p.Options)
	})
	rpc.Register(mux, "sandbox.disk.attach", func(ctx context.Context, session *rpc.Session, p rpc.Target[types.ExternalDisk]) (any, error) {
		service := app.Sandboxes.WithReporter(sandboxReporter{session})
		return service.AttachDisk(ctx, p.Reference, p.Options)
	})
	rpc.Register(mux, "sandbox.disk.detach", func(ctx context.Context, session *rpc.Session, p rpc.Named) (any, error) {
		service := app.Sandboxes.WithReporter(sandboxReporter{session})
		return service.DetachDisk(ctx, p.Reference, p.Name)
	})
	rpc.Register(mux, "sandbox.fs.attach", func(ctx context.Context, session *rpc.Session, p rpc.Target[types.FileShare]) (any, error) {
		service := app.Sandboxes.WithReporter(sandboxReporter{session})
		return service.AttachFileShare(ctx, p.Reference, p.Options)
	})
	rpc.Register(mux, "sandbox.fs.detach", func(ctx context.Context, session *rpc.Session, p rpc.Named) (any, error) {
		service := app.Sandboxes.WithReporter(sandboxReporter{session})
		return service.DetachFileShare(ctx, p.Reference, p.Name)
	})
	rpc.Register(mux, "sandbox.pci.attach", func(ctx context.Context, session *rpc.Session, p rpc.PCIRequest) (any, error) {
		service := app.Sandboxes.WithReporter(sandboxReporter{session})
		return service.AttachPCIDevice(ctx, p.Reference, p.PCI, p.ID)
	})
	rpc.Register(mux, "sandbox.pci.detach", func(ctx context.Context, session *rpc.Session, p rpc.Named) (any, error) {
		service := app.Sandboxes.WithReporter(sandboxReporter{session})
		return service.DetachPCIDevice(ctx, p.Reference, p.Name)
	})
	rpc.Register(mux, "sandbox.devices", func(ctx context.Context, session *rpc.Session, p rpc.Ref) (any, error) {
		service := app.Sandboxes.WithReporter(sandboxReporter{session})
		return service.AttachedDevices(ctx, p.Reference)
	})
	rpc.Register(mux, "sandbox.spawn", func(ctx context.Context, session *rpc.Session, p rpc.Target[types.Command]) (any, error) {
		service := app.Sandboxes.WithReporter(sandboxReporter{session})
		return service.Spawn(ctx, p.Reference, p.Options)
	})
	rpc.Register(mux, "sandbox.process.list", func(ctx context.Context, session *rpc.Session, p rpc.Ref) (any, error) {
		service := app.Sandboxes.WithReporter(sandboxReporter{session})
		return service.Processes(ctx, p.Reference)
	})
	rpc.Register(mux, "sandbox.process.signal", func(ctx context.Context, session *rpc.Session, p rpc.ProcessSignal) (any, error) {
		service := app.Sandboxes.WithReporter(sandboxReporter{session})
		return nil, service.SignalProcess(ctx, p.Reference, p.ID, p.Signal)
	})
	rpc.Register(mux, "sandbox.file.stat", func(ctx context.Context, session *rpc.Session, p rpc.FileRequest) (any, error) {
		service := app.Sandboxes.WithReporter(sandboxReporter{session})
		return service.StatFile(ctx, p.Reference, p.Path)
	})
	rpc.Register(mux, "sandbox.file.mkdir", func(ctx context.Context, session *rpc.Session, p rpc.FileRequest) (any, error) {
		service := app.Sandboxes.WithReporter(sandboxReporter{session})
		return nil, service.MakeDirectory(ctx, p.Reference, p.Path, p.Parents)
	})
	rpc.Register(mux, "sandbox.file.remove", func(ctx context.Context, session *rpc.Session, p rpc.FileRequest) (any, error) {
		service := app.Sandboxes.WithReporter(sandboxReporter{session})
		return nil, service.RemoveFile(ctx, p.Reference, p.Path, p.Recursive)
	})
	rpc.Register(mux, "sandbox.file.rename", func(ctx context.Context, session *rpc.Session, p rpc.FileRequest) (any, error) {
		service := app.Sandboxes.WithReporter(sandboxReporter{session})
		return nil, service.RenameFile(ctx, p.Reference, p.Path, p.Destination)
	})
	rpc.Register(mux, "snapshot.save", func(ctx context.Context, session *rpc.Session, p types.SaveSnapshotRequest) (any, error) {
		service := app.Snapshots.WithReporter(snapshotReporter{session})
		return service.Save(ctx, p)
	})
	rpc.Register(mux, "snapshot.hibernate", func(ctx context.Context, session *rpc.Session, p types.SaveSnapshotRequest) (any, error) {
		service := app.Snapshots.WithReporter(snapshotReporter{session})
		return service.Hibernate(ctx, p)
	})
	rpc.Register(mux, "snapshot.list", func(ctx context.Context, session *rpc.Session, p struct{}) (any, error) {
		service := app.Snapshots.WithReporter(snapshotReporter{session})
		return service.List(ctx)
	})
	rpc.Register(mux, "snapshot.list-for-sandbox", func(ctx context.Context, session *rpc.Session, p rpc.Ref) (any, error) {
		service := app.Snapshots.WithReporter(snapshotReporter{session})
		return service.ListForSandbox(ctx, p.Reference)
	})
	rpc.Register(mux, "snapshot.inspect", func(ctx context.Context, session *rpc.Session, p rpc.Ref) (any, error) {
		service := app.Snapshots.WithReporter(snapshotReporter{session})
		return service.Inspect(ctx, p.Reference)
	})
	rpc.Register(mux, "snapshot.remove", func(ctx context.Context, session *rpc.Session, p rpc.Ref) (any, error) {
		service := app.Snapshots.WithReporter(snapshotReporter{session})
		return service.Remove(ctx, p.Reference)
	})
	rpc.Register(mux, "snapshot.clone", func(ctx context.Context, session *rpc.Session, p rpc.Target[types.CloneOptions]) (any, error) {
		service := app.Snapshots.WithReporter(snapshotReporter{session})
		p.Options.SkipReseed = false
		return service.CloneWithOptions(ctx, p.Reference, p.Options)
	})
	rpc.Register(mux, "snapshot.restore", func(ctx context.Context, session *rpc.Session, p rpc.RestoreRequest) (any, error) {
		service := app.Snapshots.WithReporter(snapshotReporter{session})
		return service.RestoreWithOptions(ctx, p.Reference, p.Snapshot, p.Options)
	})
	rpc.Register(mux, "snapshot.export-directory", func(ctx context.Context, session *rpc.Session, p rpc.Named) (any, error) {
		service := app.Snapshots.WithReporter(snapshotReporter{session})
		return service.ExportDirectory(ctx, p.Reference, p.Name)
	})
}

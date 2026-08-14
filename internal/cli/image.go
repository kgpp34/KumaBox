// SPDX-License-Identifier: MIT

package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/batch"
	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/image"
	"github.com/kumabox/kumabox/internal/image/oci"
	"github.com/kumabox/kumabox/internal/lock"
	"github.com/kumabox/kumabox/internal/resources"
)

func newImageCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Manage KumaBox images",
	}
	cmd.AddCommand(newImageAddCommand(opts))
	cmd.AddCommand(newImageImportCommand(opts))
	cmd.AddCommand(newImagePullCommand(opts))
	cmd.AddCommand(newImagePullOCICommand(opts))
	cmd.AddCommand(newImageBuildCommand(opts))
	cmd.AddCommand(newImageLSCommand(opts))
	cmd.AddCommand(newImageInspectCommand(opts))
	cmd.AddCommand(newImageRMCommand(opts))
	return cmd
}

type imageSourceKind string

const (
	imageSourceLocal imageSourceKind = "local"
	imageSourceHTTP  imageSourceKind = "http"
	imageSourceOCI   imageSourceKind = "oci"
)

func newImageAddCommand(opts *rootOptions) *cobra.Command {
	var name, firmware, qemuImg, expectedSHA256 string
	var platform, source, mkfsEROFS, agentProfile string
	var concurrency int
	var progress bool

	cmd := &cobra.Command{
		Use:   "add SOURCE",
		Short: "Add a local, HTTP, or OCI image",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if concurrency < 0 {
				return errors.New("--concurrency must not be negative")
			}
			kind, err := classifyImageSource(args[0])
			if err != nil {
				return err
			}
			if kind != imageSourceOCI && firmware == "" {
				return errors.New("--firmware is required for local and HTTP cloud images")
			}
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			qemuImg = configuredQEMUImg(qemuImg, cfg)
			stores, err := configuredStores(cfg)
			if err != nil {
				return err
			}
			if stores.Metadata != nil {
				defer func() { _ = stores.Metadata.Close() }()
			}
			mutation, err := stores.Guard.BeginMutation(cmd.Context())
			if err != nil {
				return err
			}
			defer mutation.Release() //nolint:errcheck

			var record *image.ImageRecord
			switch kind {
			case imageSourceLocal:
				record, err = stores.Images.ImportLocal(image.ImportRequest{
					Name: name, File: args[0], Firmware: firmware, QemuImgPath: qemuImg,
				})
			case imageSourceHTTP:
				record, err = stores.Images.Pull(image.PullRequest{
					Name: name, URL: args[0], Firmware: firmware,
					QemuImgPath: qemuImg, SHA256: expectedSHA256,
				})
			case imageSourceOCI:
				record, err = oci.NewImagePipeline(cfg.Runtime.RootDir, stores.OCI, stores.Images).Build(cmd.Context(), oci.BuildRequest{
					Name: name, Ref: args[0], Platform: platform, Source: source,
					MkfsEROFS: mkfsEROFS, Concurrency: concurrency, AgentProfile: agentProfile,
					Progress: cliOCIProgress(cmd, progress),
				})
			default:
				return fmt.Errorf("unsupported image source kind %q", kind)
			}
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), record)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "image name")
	cmd.Flags().StringVar(&firmware, "firmware", "", "UEFI firmware path for cloud images")
	cmd.Flags().StringVar(&qemuImg, "qemu-img", "", "qemu-img binary path override")
	cmd.Flags().StringVar(&expectedSHA256, "sha256", "", "expected HTTP image sha256 digest")
	cmd.Flags().StringVar(&platform, "platform", oci.DefaultPlatform(), "OCI platform os/arch[/variant]")
	cmd.Flags().StringVar(&source, "source", "auto", "OCI source: auto, registry, or daemon")
	cmd.Flags().StringVar(&mkfsEROFS, "mkfs-erofs", "mkfs.erofs", "mkfs.erofs binary path")
	cmd.Flags().IntVar(&concurrency, "concurrency", 0, "maximum concurrent OCI layer conversions")
	cmd.Flags().StringVar(&agentProfile, "agent-profile", image.AgentProfileAuto, "guest agent profile")
	cmd.Flags().BoolVar(&progress, "progress", false, "print OCI import progress to stderr")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func classifyImageSource(source string) (imageSourceKind, error) {
	parsed, err := url.Parse(source)
	if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		if parsed.Host == "" {
			return "", fmt.Errorf("invalid HTTP image source: %s", source)
		}
		return imageSourceHTTP, nil
	}
	info, statErr := os.Stat(source)
	if statErr == nil {
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("local image source is not a regular file: %s", source)
		}
		return imageSourceLocal, nil
	}
	if !errors.Is(statErr, os.ErrNotExist) {
		return "", fmt.Errorf("inspect image source %s: %w", source, statErr)
	}
	if filepath.IsAbs(source) || strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../") {
		return "", fmt.Errorf("local image source does not exist: %s", source)
	}
	return imageSourceOCI, nil
}

func newImagePullOCICommand(opts *rootOptions) *cobra.Command {
	var platform string
	var jsonOutput bool
	var source string
	var progress bool

	cmd := &cobra.Command{
		Use:   "pull-oci REF",
		Short: "Pull OCI blobs into the content store",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			stores, err := configuredStores(cfg)
			if err != nil {
				return err
			}
			mutation, err := stores.Guard.BeginMutation(cmd.Context())
			if err != nil {
				return err
			}
			defer mutation.Release() //nolint:errcheck
			result, err := oci.NewImagePipeline(cfg.Runtime.RootDir, stores.OCI, stores.Images).Pull(cmd.Context(), oci.PullRequest{
				Ref:      args[0],
				Platform: platform,
				Source:   source,
				Progress: cliOCIProgress(cmd, progress),
			})
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), result)
		},
	}
	cmd.Flags().StringVar(&platform, "platform", oci.DefaultPlatform(), "OCI platform os/arch[/variant]")
	cmd.Flags().StringVar(&source, "source", "auto", "OCI source: auto, registry, or daemon")
	cmd.Flags().BoolVar(&progress, "progress", false, "print OCI import progress to stderr")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}

func newImageBuildCommand(opts *rootOptions) *cobra.Command {
	var name string
	var platform string
	var dryRun bool
	var jsonOutput bool
	var source string
	var mkfsEROFS string
	var concurrency int
	var agentProfile string
	var progress bool

	cmd := &cobra.Command{
		Use:   "build REF",
		Short: "Build a KumaBox image from an OCI reference",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			if name == "" {
				return errors.New("--name is required")
			}

			if dryRun {
				if !jsonOutput {
					return fmt.Errorf("P3_RESOLVE_REQUIRES_JSON: P3-01 dry-run output requires --json")
				}
				result, err := oci.Resolve(cmd.Context(), args[0], platform)
				if err != nil {
					return err
				}
				return writeJSON(cmd.OutOrStdout(), struct {
					SchemaVersion string             `json:"schemaVersion"`
					Name          string             `json:"name"`
					DryRun        bool               `json:"dryRun"`
					Result        *oci.ResolveResult `json:"result"`
				}{
					SchemaVersion: "kumabox.oci.resolve.v1",
					Name:          name,
					DryRun:        true,
					Result:        result,
				})
			}
			stores, err := configuredStores(cfg)
			if err != nil {
				return err
			}
			mutation, err := stores.Guard.BeginMutation(cmd.Context())
			if err != nil {
				return err
			}
			defer mutation.Release() //nolint:errcheck
			rec, err := oci.NewImagePipeline(cfg.Runtime.RootDir, stores.OCI, stores.Images).Build(cmd.Context(), oci.BuildRequest{
				Name:         name,
				Ref:          args[0],
				Platform:     platform,
				Source:       source,
				MkfsEROFS:    mkfsEROFS,
				Concurrency:  concurrency,
				AgentProfile: agentProfile,
				Progress:     cliOCIProgress(cmd, progress),
			})
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "image name")
	cmd.Flags().StringVar(&platform, "platform", oci.DefaultPlatform(), "OCI platform os/arch[/variant]")
	cmd.Flags().StringVar(&source, "source", "auto", "OCI source: auto, registry, or daemon")
	cmd.Flags().StringVar(&mkfsEROFS, "mkfs-erofs", "mkfs.erofs", "mkfs.erofs binary path")
	cmd.Flags().IntVar(&concurrency, "concurrency", 0, "maximum concurrent OCI layer conversions; 0 uses host CPU count")
	cmd.Flags().StringVar(&agentProfile, "agent-profile", image.AgentProfileAuto, "guest agent profile: auto, required, embedded, or unsupported")
	cmd.Flags().BoolVar(&progress, "progress", false, "print OCI import progress to stderr")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "resolve OCI metadata without publishing an image")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func cliOCIProgress(cmd *cobra.Command, enabled bool) func(oci.ProgressEvent) {
	if !enabled {
		return nil
	}
	return func(event oci.ProgressEvent) {
		if event.Total > 0 {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "oci: phase=%s item=%d/%d digest=%s cached=%t\n", event.Phase, event.Index+1, event.Total, event.Digest, event.Cached)
			return
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "oci: phase=%s digest=%s\n", event.Phase, event.Digest)
	}
}

func newImageImportCommand(opts *rootOptions) *cobra.Command {
	var name string
	var firmware string
	var qemuImg string

	cmd := &cobra.Command{
		Use:   "import FILE",
		Short: "Import a local cloud image",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			qemuImg = configuredQEMUImg(qemuImg, cfg)
			stores, err := configuredStores(cfg)
			if err != nil {
				return err
			}
			mutation, err := stores.Guard.BeginMutation(cmd.Context())
			if err != nil {
				return err
			}
			defer mutation.Release() //nolint:errcheck
			rec, err := stores.Images.ImportLocal(image.ImportRequest{
				Name:        name,
				File:        args[0],
				Firmware:    firmware,
				QemuImgPath: qemuImg,
			})
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "image name")
	cmd.Flags().StringVar(&firmware, "firmware", "", "UEFI firmware path")
	cmd.Flags().StringVar(&qemuImg, "qemu-img", "", "qemu-img binary path override")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("firmware")
	return cmd
}

func newImagePullCommand(opts *rootOptions) *cobra.Command {
	var names []string
	var firmware string
	var qemuImg string
	var sha256Digest string
	var concurrency int

	cmd := &cobra.Command{
		Use:   "pull URL...",
		Short: "Pull a cloud image URL",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateBatchConcurrency(concurrency); err != nil {
				return err
			}
			if len(names) != len(args) {
				return fmt.Errorf("provide one --name for each URL: got %d name(s) for %d URL(s)", len(names), len(args))
			}
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			qemuImg = configuredQEMUImg(qemuImg, cfg)
			stores, err := configuredStores(cfg)
			if err != nil {
				return err
			}
			mutation, err := stores.Guard.BeginMutation(cmd.Context())
			if err != nil {
				return err
			}
			defer mutation.Release() //nolint:errcheck
			result := batch.Run(cmd.Context(), args, batch.Options{Concurrency: concurrency}, "pull image", func(_ context.Context, index int, ref string) (*image.ImageRecord, error) {
				return stores.Images.Pull(image.PullRequest{
					Name: names[index], URL: ref, Firmware: firmware,
					QemuImgPath: qemuImg, SHA256: sha256Digest,
				})
			})
			return writeResourceBatchResult(func(value any) error {
				return writeJSON(cmd.OutOrStdout(), value)
			}, args, "pull image", result)
		},
	}
	cmd.Flags().StringArrayVar(&names, "name", nil, "image name, repeat once per URL")
	cmd.Flags().StringVar(&firmware, "firmware", "", "UEFI firmware path")
	cmd.Flags().StringVar(&qemuImg, "qemu-img", "", "qemu-img binary path override")
	cmd.Flags().StringVar(&sha256Digest, "sha256", "", "expected image sha256 digest")
	addResourceBatchConcurrencyFlag(cmd, &concurrency)
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("firmware")
	return cmd
}

func configuredQEMUImg(override string, cfg config.Config) string {
	if override != "" {
		return override
	}
	return cfg.Storage.QEMUImgBinary
}

func newImageLSCommand(opts *rootOptions) *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List images",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			stores, err := configuredStores(cfg)
			if err != nil {
				return err
			}
			records, err := stores.Images.List()
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), records)
			}
			return writeImageTable(cmd.OutOrStdout(), records)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}

func newImageInspectCommand(opts *rootOptions) *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "inspect IMAGE",
		Short: "Inspect an image",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			stores, err := configuredStores(cfg)
			if err != nil {
				return err
			}
			rec, err := stores.Images.Inspect(args[0])
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), rec)
			}
			return writeImageTable(cmd.OutOrStdout(), []*image.ImageRecord{rec})
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}

func newImageRMCommand(opts *rootOptions) *cobra.Command {
	var force bool
	var concurrency int

	cmd := &cobra.Command{
		Use:     "rm IMAGE...",
		Aliases: []string{"remove"},
		Short:   "Remove an unused image",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateBatchConcurrency(concurrency); err != nil {
				return err
			}
			args = batch.Distinct(args)
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			stores, err := configuredStores(cfg)
			if err != nil {
				return err
			}
			mutation, err := stores.Guard.BeginMutation(cmd.Context())
			if err != nil {
				return err
			}
			defer mutation.Release() //nolint:errcheck
			result := batch.Run(cmd.Context(), args, batch.Options{Concurrency: concurrency}, "remove image", func(ctx context.Context, _ int, ref string) (*image.ImageRecord, error) {
				return removeImage(ctx, stores, ref, force)
			})
			return writeResourceBatchResult(func(value any) error {
				return writeJSON(cmd.OutOrStdout(), value)
			}, args, "remove image", result)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "allow removal of damaged unreferenced image directories")
	addResourceBatchConcurrencyFlag(cmd, &concurrency)
	return cmd
}

func removeImage(ctx context.Context, stores resources.StoreSet, ref string, force bool) (record *image.ImageRecord, err error) {
	imageRecord, err := stores.Images.Inspect(ref)
	if err != nil {
		return nil, err
	}
	imageLock, err := stores.Guard.LockEntity(ctx, lock.EntityImage, imageRecord.ID)
	if err != nil {
		return nil, err
	}
	defer func() {
		if releaseErr := imageLock.Release(); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("release image lock: %w", releaseErr))
		}
	}()
	references, err := imageReferencesFromVMs(stores)
	if err != nil {
		return nil, fmt.Errorf("recheck image references: %w", err)
	}
	if explicit, explicitErr := explicitImageReferences(ctx, stores, ref); explicitErr != nil {
		return nil, explicitErr
	} else if len(explicit) > 0 {
		references = mergeImageReferences(references, explicit)
	}
	return stores.Images.Remove(image.RemoveRequest{Ref: ref, Force: force, References: references})
}

func mergeImageReferences(groups ...[]image.Reference) []image.Reference {
	seen := make(map[string]struct{})
	var merged []image.Reference
	for _, group := range groups {
		for _, reference := range group {
			key := reference.Kind + "\x00" + reference.VMID + "\x00" + reference.ImageID
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			merged = append(merged, reference)
		}
	}
	return merged
}

func explicitImageReferences(ctx context.Context, stores resources.StoreSet, ref string) ([]image.Reference, error) {
	if stores.References == nil {
		return nil, nil
	}
	imageRecord, err := stores.Images.Inspect(ref)
	if err != nil {
		return nil, err
	}
	records, err := stores.References.ListTarget(ctx, "image", imageRecord.ID)
	if err != nil {
		return nil, err
	}
	liveVMs, liveSnapshots, err := liveImageReferenceSources(stores)
	if err != nil {
		return nil, err
	}
	refs := make([]image.Reference, 0, len(records))
	for _, record := range records {
		live := true
		switch record.SourceKind {
		case "vm":
			_, live = liveVMs[record.SourceID]
		case "snapshot":
			_, live = liveSnapshots[record.SourceID]
		}
		if !live {
			if err := stores.References.Delete(ctx, record.ID); err != nil {
				return nil, fmt.Errorf("delete dangling image reference %s: %w", record.ID, err)
			}
			continue
		}
		refs = append(refs, image.Reference{Kind: record.SourceKind, VMID: record.SourceID, VMName: record.SourceID, ImageID: imageRecord.ID})
	}
	return refs, nil
}

func liveImageReferenceSources(stores resources.StoreSet) (map[string]struct{}, map[string]struct{}, error) {
	vms, err := stores.VM.List()
	if err != nil {
		return nil, nil, fmt.Errorf("list VMs for image references: %w", err)
	}
	liveVMs := make(map[string]struct{}, len(vms))
	for _, rec := range vms {
		if rec != nil {
			liveVMs[rec.ID] = struct{}{}
		}
	}
	snapshots, err := stores.Snapshots.List()
	if err != nil {
		return nil, nil, fmt.Errorf("list snapshots for image references: %w", err)
	}
	liveSnapshots := make(map[string]struct{}, len(snapshots))
	for _, rec := range snapshots {
		if rec != nil {
			liveSnapshots[rec.ID] = struct{}{}
		}
	}
	return liveVMs, liveSnapshots, nil
}

func imageReferencesFromVMs(stores resources.StoreSet) ([]image.Reference, error) {
	records, err := stores.VM.List()
	if err != nil {
		return nil, err
	}
	refs := make([]image.Reference, 0)
	for _, rec := range records {
		if rec == nil || rec.Image == nil {
			continue
		}
		refs = append(refs, image.Reference{
			Kind:    "vm",
			VMID:    rec.ID,
			VMName:  rec.Name,
			VMState: string(rec.State),
			ImageID: rec.Image.ID,
		})
	}
	snapshots, err := stores.Snapshots.List()
	if err != nil {
		return nil, fmt.Errorf("read snapshot references: %w", err)
	}
	for _, rec := range snapshots {
		manifest, err := stores.Snapshots.PeekManifest(context.Background(), rec.ID)
		if err != nil {
			return nil, fmt.Errorf("read snapshot %s image reference: %w", rec.ID, err)
		}
		if manifest.Base == nil || manifest.Base.ImageID == "" {
			continue
		}
		refs = append(refs, image.Reference{
			Kind: "snapshot", VMID: rec.ID, VMName: rec.Name, VMState: string(rec.State), ImageID: manifest.Base.ImageID,
		})
	}
	return refs, nil
}

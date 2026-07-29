// SPDX-License-Identifier: MIT

// Package imagestore manages imported and pulled cloud images.
//
// Image records are metadata only: they point at managed root disks and boot
// requirements. VM records copy the resolved image reference at create/run time
// so later image renames do not change existing VM intent.
package imagestore

import "time"

// Source describes where a managed image was imported from.
type Source struct {
	Type string `json:"type"`
	URI  string `json:"uri,omitempty"`
}

// RootDisk describes the managed root disk stored with an image.
type RootDisk struct {
	Path             string `json:"path,omitempty"`
	Format           string `json:"format,omitempty"`
	VirtualSizeBytes int64  `json:"virtualSizeBytes,omitempty"`
	ActualSizeBytes  int64  `json:"actualSizeBytes,omitempty"`
	SHA256           string `json:"sha256,omitempty"`
}

// Boot describes how VMs should boot from an image.
type Boot struct {
	Mode     string `json:"mode,omitempty"`
	Firmware string `json:"firmware,omitempty"`
	Kernel   string `json:"kernel,omitempty"`
	Initrd   string `json:"initrd,omitempty"`
	Cmdline  string `json:"cmdline,omitempty"`
}

// OS describes the guest operating system profile for an image.
type OS struct {
	Family  string `json:"family,omitempty"`
	Version string `json:"version,omitempty"`
	Profile string `json:"profile,omitempty"`
}

// OCIPlatform identifies the image platform selected during OCI resolution.
type OCIPlatform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

// OCIDescriptor records one digest-addressed OCI object.
type OCIDescriptor struct {
	Digest    string `json:"digest"`
	MediaType string `json:"mediaType,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
}

// EROFSLayer records the converted read-only filesystem for one OCI layer.
type EROFSLayer struct {
	Path        string `json:"path"`
	Filesystem  string `json:"filesystem"`
	Digest      string `json:"digest"`
	SizeBytes   int64  `json:"sizeBytes"`
	SourceLayer string `json:"sourceLayer"`
}

// OCILayer records one OCI layer and its converted shared filesystem.
type OCILayer struct {
	Index     int         `json:"index"`
	Digest    string      `json:"digest"`
	Serial    string      `json:"serial,omitempty"`
	Kernel    string      `json:"kernel,omitempty"`
	Initrd    string      `json:"initrd,omitempty"`
	MediaType string      `json:"mediaType"`
	SizeBytes int64       `json:"sizeBytes"`
	EROFS     *EROFSLayer `json:"erofs,omitempty"`
}

// OCIImageConfig preserves the container config fields needed by future agent
// execution without starting the OCI entrypoint as the VM init process.
type OCIImageConfig struct {
	Env        *[]string          `json:"env,omitempty"`
	Cmd        *[]string          `json:"cmd,omitempty"`
	Entrypoint *[]string          `json:"entrypoint,omitempty"`
	Workdir    *string            `json:"workdir,omitempty"`
	User       *string            `json:"user,omitempty"`
	Labels     *map[string]string `json:"labels,omitempty"`
}

// OCI records the OCI source and layer order for an image build.
type OCI struct {
	Ref            string         `json:"ref"`
	Source         string         `json:"source"`
	DigestRef      string         `json:"digestRef"`
	Platform       OCIPlatform    `json:"platform"`
	Config         OCIDescriptor  `json:"config"`
	ImageConfig    OCIImageConfig `json:"imageConfig,omitempty"`
	AgentInjection string         `json:"agentInjection,omitempty"`
	Layers         []OCILayer     `json:"layers"`
	BuiltAt        time.Time      `json:"builtAt"`
}

const (
	AgentName                 = "kumabox-agent"
	AgentBinaryPath           = "/usr/local/bin/kumabox-agent"
	AgentServicePath          = "/etc/systemd/system/kumabox-agent.service"
	AgentProfileAuto          = "auto"
	AgentProfileRequired      = "required"
	AgentInjectionEmbedded    = "embedded"
	AgentInjectionUnsupported = "unsupported"
)

// AgentProfile records how the guest agent is provided by an image.
type AgentProfile struct {
	Name         string   `json:"name"`
	Version      string   `json:"version,omitempty"`
	Injection    string   `json:"injection"`
	BinaryPath   string   `json:"binaryPath,omitempty"`
	ServicePath  string   `json:"servicePath,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// ImageRecord is the persisted metadata for one managed image.
type ImageRecord struct {
	SchemaVersion string        `json:"schemaVersion"`
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	Source        Source        `json:"source"`
	RootDisk      RootDisk      `json:"rootDisk"`
	Boot          Boot          `json:"boot"`
	OS            OS            `json:"os"`
	OCI           *OCI          `json:"oci,omitempty"`
	Agent         *AgentProfile `json:"agent,omitempty"`
	CreatedAt     time.Time     `json:"createdAt"`
	UpdatedAt     time.Time     `json:"updatedAt"`
}

func cloneRecord(rec *ImageRecord) *ImageRecord {
	if rec == nil {
		return nil
	}
	copied := *rec
	copied.Agent = cloneAgentProfile(rec.Agent)
	if rec.OCI != nil {
		oci := *rec.OCI
		oci.ImageConfig = cloneOCIImageConfig(rec.OCI.ImageConfig)
		oci.Layers = append([]OCILayer(nil), rec.OCI.Layers...)
		for i := range oci.Layers {
			if oci.Layers[i].EROFS == nil {
				continue
			}
			erofs := *oci.Layers[i].EROFS
			oci.Layers[i].EROFS = &erofs
		}
		copied.OCI = &oci
	}
	return &copied
}

func cloneAgentProfile(profile *AgentProfile) *AgentProfile {
	if profile == nil {
		return nil
	}
	copied := *profile
	copied.Capabilities = append([]string(nil), profile.Capabilities...)
	return &copied
}

func cloneOCIImageConfig(cfg OCIImageConfig) OCIImageConfig {
	copied := cfg
	if cfg.Env != nil {
		env := append([]string(nil), (*cfg.Env)...)
		copied.Env = &env
	}
	if cfg.Cmd != nil {
		cmd := append([]string(nil), (*cfg.Cmd)...)
		copied.Cmd = &cmd
	}
	if cfg.Entrypoint != nil {
		entrypoint := append([]string(nil), (*cfg.Entrypoint)...)
		copied.Entrypoint = &entrypoint
	}
	if cfg.Workdir != nil {
		workdir := *cfg.Workdir
		copied.Workdir = &workdir
	}
	if cfg.User != nil {
		user := *cfg.User
		copied.User = &user
	}
	if cfg.Labels != nil {
		labels := make(map[string]string, len(*cfg.Labels))
		for key, value := range *cfg.Labels {
			labels[key] = value
		}
		copied.Labels = &labels
	}
	return copied
}

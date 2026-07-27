// Package metadata renders NoCloud seed data for cloud-image guests.
//
// Cloud images rely on cloud-init to set hostname, users, and first-boot
// networking. KumaBox writes the standard NoCloud files both as plain files for
// inspection and as a small CIDATA disk consumed by the guest.
package metadata

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// CidataLabel is the volume label cloud-init uses to discover NoCloud media.
const CidataLabel = "CIDATA"

// Config is the input used to render NoCloud metadata, user-data, and network
// configuration.
type Config struct {
	InstanceID string
	Hostname   string
	Username   string
	Networks   []Network
	Mounts     []Mount
}

// Mount describes one cloud-init mount entry for a managed data disk.
type Mount struct {
	Device     string
	MountPoint string
	Filesystem string
	Options    string
}

// Network describes one guest interface in cloud-init network-config format.
//
// Interfaces are matched by MAC so guest interface names can be set
// deterministically even if kernel enumeration order changes.
type Network struct {
	MAC     string
	IP      string
	Prefix  int
	Gateway string
	DNS     []string
}

// Rendered contains the three NoCloud files before they are written to disk.
type Rendered struct {
	MetaData      []byte
	UserData      []byte
	NetworkConfig []byte
}

var (
	metaDataTemplate = template.Must(template.New("meta-data").Parse(`instance-id: {{.InstanceID}}
local-hostname: {{.Hostname}}
`))

	userDataTemplate = template.Must(template.New("user-data").Parse(`#cloud-config
hostname: {{.Hostname}}
manage_etc_hosts: true
users:
  - default
  - name: {{.Username}}
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
ssh_pwauth: false
{{- if .Mounts}}
mounts:
{{- range .Mounts}}
  - ["{{.Device}}", "{{.MountPoint}}", "{{.Filesystem}}", "{{.Options}}", "0", "2"]
{{- end}}
{{- end}}
`))

	networkConfigTemplate = template.Must(template.New("network-config").Parse(`version: 2
ethernets:
{{- if .Networks }}
{{- range $i, $net := .Networks }}
  eth{{$i}}:
    match:
      macaddress: "{{$net.MAC}}"
    set-name: eth{{$i}}
    addresses:
      - {{$net.IP}}/{{$net.Prefix}}
{{- if $net.Gateway }}
    gateway4: {{$net.Gateway}}
{{- end }}
{{- if $net.DNS }}
    nameservers:
      addresses:
{{- range $dns := $net.DNS }}
        - {{$dns}}
{{- end }}
{{- end }}
    optional: true
{{- end }}
{{- else }}
  fallback:
    match:
      name: "e*"
    dhcp4: true
    optional: true
{{- end }}
`))
)

// Render builds NoCloud meta-data, user-data, and network-config files.
func Render(cfg Config) (*Rendered, error) {
	if cfg.InstanceID == "" {
		return nil, fmt.Errorf("instance ID must not be empty")
	}
	if cfg.Hostname == "" {
		return nil, fmt.Errorf("hostname must not be empty")
	}
	if cfg.Username == "" {
		cfg.Username = "kumabox"
	}
	var rendered Rendered
	if err := executeTemplate(metaDataTemplate, cfg, &rendered.MetaData); err != nil {
		return nil, fmt.Errorf("render meta-data: %w", err)
	}
	if err := executeTemplate(userDataTemplate, cfg, &rendered.UserData); err != nil {
		return nil, fmt.Errorf("render user-data: %w", err)
	}
	if err := executeTemplate(networkConfigTemplate, cfg, &rendered.NetworkConfig); err != nil {
		return nil, fmt.Errorf("render network-config: %w", err)
	}
	return &rendered, nil
}

// WriteNoCloud writes NoCloud files and a CIDATA disk image.
//
// The directory files are useful for debugging. The disk image is what Cloud
// Hypervisor attaches to the guest during firmware/cloud-image boots.
func WriteNoCloud(dir, diskPath string, cfg Config) (err error) {
	rendered, err := Render(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create cidata dir: %w", err)
	}
	files := map[string][]byte{
		"meta-data":      rendered.MetaData,
		"user-data":      rendered.UserData,
		"network-config": rendered.NetworkConfig,
	}
	for name, data := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(diskPath), 0o755); err != nil {
		return fmt.Errorf("create cidata disk dir: %w", err)
	}
	file, err := os.OpenFile(diskPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create cidata disk: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close cidata disk: %w", closeErr)
		}
	}()
	if err := WriteFAT12(file, CidataLabel, files); err != nil {
		return fmt.Errorf("write cidata disk: %w", err)
	}
	return nil
}

func executeTemplate(tmpl *template.Template, cfg Config, out *[]byte) error {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, cfg); err != nil {
		return err
	}
	*out = bytes.Clone(buf.Bytes())
	return nil
}

// ContainsNoCloudFiles performs a lightweight smoke check for rendered seed data.
func ContainsNoCloudFiles(raw []byte) bool {
	s := string(raw)
	return strings.Contains(s, "instance-id:") &&
		strings.Contains(s, "#cloud-config") &&
		strings.Contains(s, "version: 2")
}

// WriteNoCloudImage writes only the CIDATA disk image to w.
func WriteNoCloudImage(w io.Writer, cfg Config) error {
	rendered, err := Render(cfg)
	if err != nil {
		return err
	}
	return WriteFAT12(w, CidataLabel, map[string][]byte{
		"meta-data":      rendered.MetaData,
		"user-data":      rendered.UserData,
		"network-config": rendered.NetworkConfig,
	})
}

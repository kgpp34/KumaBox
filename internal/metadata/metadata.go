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

const CidataLabel = "CIDATA"

type Config struct {
	InstanceID string
	Hostname   string
	Username   string
	Networks   []Network
}

type Network struct {
	MAC     string
	IP      string
	Prefix  int
	Gateway string
	DNS     []string
}

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

func WriteNoCloud(dir, diskPath string, cfg Config) error {
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
	defer file.Close() //nolint:errcheck
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

func ContainsNoCloudFiles(raw []byte) bool {
	s := string(raw)
	return strings.Contains(s, "instance-id:") &&
		strings.Contains(s, "#cloud-config") &&
		strings.Contains(s, "version: 2")
}

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

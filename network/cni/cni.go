// Package cni implements network.Provider with CNI plugins, one named network
// namespace per sandbox, and TAP devices connected through traffic-control
// redirects.
package cni

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/containernetworking/cni/libcni"
	cnitypes "github.com/containernetworking/cni/pkg/types"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/metadata"
	"github.com/kumabox/kumabox/network"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
)

// CollectionRecords stores one crash-recoverable aggregate per sandbox.
const CollectionRecords metadata.Collection = "network_records"

const (
	recordVersion     = 1
	defaultTAPPrefix  = "tap"
	namedNamespaceDir = "/var/run/netns"
)

// Collections declares the metadata owned by the CNI adapter.
func Collections() []metadata.Collection { return []metadata.Collection{CollectionRecords} }

// Options contains immutable host paths and cleanup policy for one provider.
type Options struct {
	// ConfDir contains host-installed .conflist files.
	ConfDir string
	// BinDir contains host-installed CNI plugin executables.
	BinDir string
	// CacheDir is managed persistent state used by the CNI library.
	CacheDir string
	// NamespacePrefix separates named namespaces owned by this installation.
	NamespacePrefix string
	// CleanupTimeout bounds rollback after caller cancellation.
	CleanupTimeout time.Duration
}

// Validate rejects ambiguous or unsafe provider configuration.
func (o Options) Validate() error {
	for name, path := range map[string]string{"configuration": o.ConfDir, "binary": o.BinDir, "cache": o.CacheDir} {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("CNI %s directory must be absolute", name)
		}
	}
	if o.NamespacePrefix == "" || len(o.NamespacePrefix) > 32 || strings.ContainsAny(o.NamespacePrefix, "/\x00") {
		return errors.New("CNI namespace prefix is invalid")
	}
	for _, character := range o.NamespacePrefix {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '-' && character != '_' {
			return errors.New("CNI namespace prefix is invalid")
		}
	}
	if o.CleanupTimeout <= 0 {
		return errors.New("CNI cleanup timeout must be positive")
	}
	return nil
}

// pluginRuntime executes one parsed CNI network list. The narrow seam keeps lifecycle
// recovery testable without requiring root privileges or plugin binaries.
type pluginRuntime interface {
	AddNetworkList(context.Context, *libcni.NetworkConfigList, *libcni.RuntimeConf) (cnitypes.Result, error)
	DelNetworkList(context.Context, *libcni.NetworkConfigList, *libcni.RuntimeConf) error
}

// platform owns Linux namespace, link, TAP, and traffic-control operations.
type platform interface {
	EnsureNamespace(string, string) (bool, error)
	RemoveNamespace(context.Context, string) error
	NamespaceExists(string) error
	SetupRedirect(string, string, string, int, string) (string, error)
	DeleteTAP(string, string) error
	SetLinkState(string, []string, bool) error
	VerifyTAP(string, string) error
}

// Provider is the CNI implementation of network.Provider.
type Provider struct {
	options     Options
	store       metadata.Store
	lists       map[string]*libcni.NetworkConfigList
	defaultName string
	runtime     pluginRuntime
	platform    platform
	loadErr     error
}

var _ network.Provider = (*Provider)(nil)

// New creates a provider. Conflist discovery is intentionally best-effort so a
// command can still open metadata and report or retry retained cleanup state
// after host configuration has temporarily disappeared.
func New(options Options, store metadata.Store) (*Provider, error) {
	if err := options.Validate(); err != nil {
		return nil, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	if store == nil {
		return nil, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("CNI metadata store is required"))
	}
	if err := storage.EnsureDir(options.CacheDir); err != nil {
		return nil, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, fmt.Errorf("create CNI cache: %w", err))
	}
	provider := &Provider{
		options: options, store: store, platform: newPlatform(),
		lists: make(map[string]*libcni.NetworkConfigList),
	}
	lists, defaultName, err := loadConfLists(options.ConfDir)
	if err != nil {
		provider.loadErr = err
		return provider, nil
	}
	provider.lists = lists
	provider.defaultName = defaultName
	provider.runtime = libcni.NewCNIConfigWithCacheDir([]string{options.BinDir}, options.CacheDir, nil)
	return provider, nil
}

// Type returns the durable provider identity.
func (*Provider) Type() types.NetworkBackend { return types.NetworkBackendCNI }

// confList resolves an explicit conflist name or the deterministic first file.
func (p *Provider) confList(name string) (*libcni.NetworkConfigList, error) {
	if p == nil || p.runtime == nil || len(p.lists) == 0 {
		if p != nil && p.loadErr != nil {
			return nil, fmt.Errorf("%w: load .conflist files from %s: %w", network.ErrNotConfigured, p.options.ConfDir, p.loadErr)
		}
		return nil, fmt.Errorf("%w: no .conflist files in %s", network.ErrNotConfigured, p.options.ConfDir)
	}
	resolved := cmp.Or(name, p.defaultName)
	list, exists := p.lists[resolved]
	if !exists {
		return nil, fmt.Errorf("CNI network %q not found; available networks: %s", resolved, strings.Join(slices.Sorted(maps.Keys(p.lists)), ", "))
	}
	return list, nil
}

// loadConfLists loads only explicit CNI list files. A single-plugin .conf is
// not silently treated as an application network contract.
func loadConfLists(dir string) (map[string]*libcni.NetworkConfigList, string, error) {
	files, err := libcni.ConfFiles(dir, []string{".conflist"})
	if err != nil {
		return nil, "", err
	}
	if len(files) == 0 {
		return nil, "", fmt.Errorf("no .conflist files in %s", dir)
	}
	slices.Sort(files)
	result := make(map[string]*libcni.NetworkConfigList, len(files))
	defaultName := ""
	for _, path := range files {
		list, err := libcni.ConfListFromFile(path)
		if err != nil {
			return nil, "", fmt.Errorf("parse %s: %w", path, err)
		}
		if _, exists := result[list.Name]; exists {
			return nil, "", fmt.Errorf("CNI network name %q is declared more than once", list.Name)
		}
		result[list.Name] = list
		if defaultName == "" {
			defaultName = list.Name
		}
	}
	return result, defaultName, nil
}

type (
	recordPhase    string
	interfacePhase string
)

const (
	phasePreparing recordPhase = "preparing"
	phaseReady     recordPhase = "ready"
	phaseDeleting  recordPhase = "deleting"

	interfaceStaged interfacePhase = "staged"
	interfaceAdding interfacePhase = "adding"
	interfaceReady  interfacePhase = "ready"
)

// recordData is an adapter-owned cleanup journal. The aggregate is written
// before namespace creation, and each NIC reaches adding before plugin code can
// produce host-side effects.
type recordData struct {
	Version       int             `json:"version"`
	SandboxID     string          `json:"sandbox_id"`
	Network       string          `json:"network,omitempty"`
	NamespaceName string          `json:"namespace_name"`
	NamespacePath string          `json:"namespace_path"`
	Phase         recordPhase     `json:"phase"`
	Interfaces    []interfaceData `json:"interfaces"`
}

type interfaceData struct {
	Index     int            `json:"index"`
	Name      string         `json:"name"`
	TAP       string         `json:"tap"`
	Phase     interfacePhase `json:"phase"`
	MAC       string         `json:"mac,omitempty"`
	Queues    int            `json:"queues"`
	QueueSize int            `json:"queue_size"`
	IPv4      *ipv4Data      `json:"ipv4,omitempty"`
}

type ipv4Data struct {
	Address string `json:"address"`
	Gateway string `json:"gateway,omitempty"`
	Prefix  int    `json:"prefix"`
}

func (p *Provider) namespace(id types.SandboxID) (string, string) {
	name := p.options.NamespacePrefix + id.String()
	return name, filepath.Join(namedNamespaceDir, name)
}

func (p *Provider) view(ctx context.Context, id types.SandboxID) (*recordData, error) {
	var result *recordData
	err := p.store.View(ctx, func(reader metadata.Reader) error {
		raw, exists, err := reader.Get(ctx, CollectionRecords, id.String())
		if err != nil || !exists {
			return err
		}
		result, err = decodeRecord(raw)
		return err
	})
	return result, err
}

func (p *Provider) update(ctx context.Context, id types.SandboxID, mutate func(*recordData) (*recordData, error)) error {
	return p.store.Update(ctx, func(writer metadata.Writer) error {
		raw, exists, err := writer.Get(ctx, CollectionRecords, id.String())
		if err != nil {
			return err
		}
		var record *recordData
		if exists {
			record, err = decodeRecord(raw)
			if err != nil {
				return err
			}
		}
		next, err := mutate(record)
		if err != nil {
			return err
		}
		if next == nil {
			return writer.Delete(ctx, CollectionRecords, id.String())
		}
		if next.SandboxID != id.String() {
			return errors.New("network record ID differs from its metadata key")
		}
		return putRecord(ctx, writer, next)
	})
}

func putRecord(ctx context.Context, writer metadata.Writer, record *recordData) error {
	if err := validateRecord(record); err != nil {
		return err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return writer.Put(ctx, CollectionRecords, record.SandboxID, raw)
}

func decodeRecord(raw []byte) (*recordData, error) {
	var record recordData
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, corrupt(err)
	}
	if err := validateRecord(&record); err != nil {
		return nil, corrupt(err)
	}
	return &record, nil
}

func validateRecord(record *recordData) error {
	if record == nil {
		return errors.New("network record is missing")
	}
	if record.Version != recordVersion {
		return fmt.Errorf("network record version %d is unsupported", record.Version)
	}
	if _, err := types.ParseSandboxID(record.SandboxID); err != nil {
		return err
	}
	if record.NamespaceName == "" || record.NamespacePath != filepath.Join(namedNamespaceDir, record.NamespaceName) {
		return errors.New("network record namespace is invalid")
	}
	if len(record.Interfaces) > 0 && record.Network == "" {
		return errors.New("network record with interfaces requires a conflist name")
	}
	switch record.Phase {
	case phasePreparing, phaseReady, phaseDeleting:
	default:
		return fmt.Errorf("network record phase %q is invalid", record.Phase)
	}
	seen := make(map[int]struct{}, len(record.Interfaces))
	for _, item := range record.Interfaces {
		if item.Index < 0 || item.Name != interfaceName(item.Index) || item.TAP == "" || item.Queues < 2 || item.Queues%2 != 0 || item.QueueSize <= 0 {
			return fmt.Errorf("network record interface %d is invalid", item.Index)
		}
		switch item.Phase {
		case interfaceStaged, interfaceAdding:
		case interfaceReady:
			if _, err := item.toType(record.Network); err != nil {
				return err
			}
		default:
			return fmt.Errorf("network record interface phase %q is invalid", item.Phase)
		}
		if _, exists := seen[item.Index]; exists {
			return fmt.Errorf("network record interface index %d is duplicated", item.Index)
		}
		seen[item.Index] = struct{}{}
	}
	if record.Phase == phaseReady {
		for _, item := range record.Interfaces {
			if item.Phase != interfaceReady {
				return errors.New("ready network record contains an incomplete interface")
			}
		}
	}
	return nil
}

func (item interfaceData) toType(networkName string) (types.NetworkInterface, error) {
	result := types.NetworkInterface{
		Index: item.Index, Name: item.Name, TAP: item.TAP, MAC: item.MAC,
		Queues: item.Queues, QueueSize: item.QueueSize, Network: networkName,
	}
	if item.IPv4 != nil {
		result.IPv4 = &types.IPv4Config{Address: item.IPv4.Address, Gateway: item.IPv4.Gateway, Prefix: item.IPv4.Prefix}
	}
	return result, result.Validate()
}

func fromType(value types.NetworkInterface, phase interfacePhase) interfaceData {
	result := interfaceData{
		Index: value.Index, Name: value.Name, TAP: value.TAP, Phase: phase,
		MAC: value.MAC, Queues: value.Queues, QueueSize: value.QueueSize,
	}
	if value.IPv4 != nil {
		result.IPv4 = &ipv4Data{Address: value.IPv4.Address, Gateway: value.IPv4.Gateway, Prefix: value.IPv4.Prefix}
	}
	return result
}

func corrupt(cause error) error {
	return errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, fmt.Errorf("decode CNI network record: %w", cause))
}

func interfaceName(index int) string { return fmt.Sprintf("eth%d", index) }

func findInterface(record *recordData, index int) int {
	return slices.IndexFunc(record.Interfaces, func(item interfaceData) bool { return item.Index == index })
}

func removeInterface(record *recordData, index int) {
	position := findInterface(record, index)
	if position >= 0 {
		record.Interfaces = slices.Delete(record.Interfaces, position, position+1)
	}
}

// newTestProvider constructs a provider around injected side-effect seams. It
// stays unexported so production composition always uses New.
func newTestProvider(options Options, store metadata.Store, lists map[string]*libcni.NetworkConfigList, defaultName string, executor pluginRuntime, host platform) *Provider {
	return &Provider{options: options, store: store, lists: lists, defaultName: defaultName, runtime: executor, platform: host}
}

package resources

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/fault"
	"github.com/kumabox/kumabox/internal/image"
	"github.com/kumabox/kumabox/internal/lock"
	"github.com/kumabox/kumabox/internal/meta"
	metajson "github.com/kumabox/kumabox/internal/meta/json"
	metasqlite "github.com/kumabox/kumabox/internal/meta/sqlite"
	"github.com/kumabox/kumabox/internal/metering"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/ocistore"
	"github.com/kumabox/kumabox/internal/operation"
	"github.com/kumabox/kumabox/internal/reference"
	"github.com/kumabox/kumabox/internal/snapshot"
	"github.com/kumabox/kumabox/internal/vm"
)

const convertedSuffix = ".converted-"

// ConversionResult describes a completed metadata backend switch.
type ConversionResult struct {
	Backend    string                `json:"backend"`
	Path       string                `json:"path,omitempty"`
	Namespaces []ConversionNamespace `json:"namespaces"`
}

// ConversionNamespace is the verified identity of one logical namespace.
type ConversionNamespace struct {
	Namespace meta.Namespace `json:"namespace"`
	Records   int            `json:"records"`
	Digest    string         `json:"digest"`
}

type conversionManifest struct {
	Target     string                              `json:"target"`
	StartedAt  time.Time                           `json:"startedAt"`
	Namespaces map[meta.Namespace]*conversionState `json:"namespaces"`
}

type conversionState struct {
	SourceFiles []string `json:"sourceFiles"`
	Records     int      `json:"records"`
	Digest      string   `json:"digest"`
	Done        bool     `json:"done"`
}

// ConvertMetadata performs or resumes an offline switch to the backend in cfg.
// A durable manifest is written before target data, and source files are only
// retired after every namespace has passed an engine-neutral digest check.
func ConvertMetadata(ctx context.Context, cfg config.Config) (result ConversionResult, err error) {
	if cfg.Metadata.Backend != "json" && cfg.Metadata.Backend != "sqlite" {
		return result, fmt.Errorf("unsupported metadata conversion target %q", cfg.Metadata.Backend)
	}
	databasePath := SQLiteMetadataPath(cfg)
	manifestPath := filepath.Join(filepath.Dir(databasePath), metasqlite.ConversionManifestName)
	manifest, err := loadConversionManifest(manifestPath)
	if err != nil {
		return result, err
	}
	if manifest != nil && manifest.Target != cfg.Metadata.Backend {
		return result, fmt.Errorf("metadata conversion to %q is already in progress", manifest.Target)
	}

	definitions := sqliteDefinitions()
	jsonDefinitions := metadataJSONDefinitions(cfg.Runtime.RootDir)
	if manifest == nil {
		if err := requireFreshTarget(cfg.Metadata.Backend, databasePath, jsonDefinitions); err != nil {
			return result, err
		}
		source, openErr := openConversionSource(cfg.Metadata.Backend, databasePath, definitions, jsonDefinitions)
		if openErr != nil {
			return result, openErr
		}
		if err := checkConversionQuiesced(ctx, cfg.Metadata.Backend, source, definitions, jsonDefinitions); err != nil {
			return result, errors.Join(err, source.Close())
		}
		manifest, err = newConversionManifest(ctx, cfg.Metadata.Backend, databasePath, source, definitions, jsonDefinitions)
		closeErr := source.Close()
		if err != nil {
			return result, errors.Join(err, closeErr)
		}
		if closeErr != nil {
			return result, fmt.Errorf("close metadata conversion source: %w", closeErr)
		}
		if err := saveConversionManifest(manifestPath, manifest); err != nil {
			return result, err
		}
	}
	if !conversionComplete(manifest) {
		if err := copyConversionNamespaces(ctx, cfg.Metadata.Backend, databasePath, definitions, jsonDefinitions, manifestPath, manifest); err != nil {
			return result, err
		}
	}
	if err := fault.Check(ctx, fault.MetadataConvertAfterCopy); err != nil {
		return result, err
	}
	if err := retireConversionSources(ctx, cfg.Metadata.Backend, databasePath, manifest); err != nil {
		return result, err
	}
	if err := removeConversionManifest(manifestPath); err != nil {
		return result, err
	}
	return conversionResult(cfg.Metadata.Backend, databasePath, manifest), nil
}

func checkConversionQuiesced(ctx context.Context, target string, source meta.MetaEngine, definitions []metasqlite.Namespace, jsonDefinitions []metajson.Namespace) error {
	probeContext, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if target == "json" {
		err := source.Update(probeContext, meta.Scope{Write: definitions[0].Name}, meta.CommitDurable, func(meta.Writer) error { return nil })
		if err != nil {
			return fmt.Errorf("sqlite metadata source is busy; stop KumaBox commands before converting: %w", err)
		}
		return nil
	}
	for _, definition := range jsonDefinitions {
		key := filepath.Base(definition.LockPath)
		key = strings.TrimSuffix(key, filepath.Ext(key))
		fileLock, err := lock.NewLocker(filepath.Dir(definition.LockPath)).Acquire(probeContext, key)
		if err != nil {
			return fmt.Errorf("json metadata namespace %s is busy; stop KumaBox commands before converting: %w", definition.Name, err)
		}
		if err := fileLock.Release(); err != nil {
			return err
		}
	}
	return nil
}

// ConvertJSONToSQLite is retained for callers of the previous one-way API.
func ConvertJSONToSQLite(ctx context.Context, rootDir, databasePath string) (statuses []metasqlite.NamespaceStatus, err error) {
	cfg := config.Default()
	cfg.Runtime.RootDir = rootDir
	cfg.Metadata.Backend = "sqlite"
	cfg.Metadata.Path = databasePath
	if _, err := ConvertMetadata(ctx, cfg); err != nil {
		return nil, err
	}
	store, err := metasqlite.Open(SQLiteMetadataPath(cfg), sqliteDefinitions()...)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, store.Close()) }()
	return store.Status(ctx)
}

func copyConversionNamespaces(ctx context.Context, target, databasePath string, definitions []metasqlite.Namespace, jsonDefinitions []metajson.Namespace, manifestPath string, manifest *conversionManifest) (err error) {
	source, err := openConversionSource(target, databasePath, definitions, jsonDefinitions)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, source.Close()) }()
	destination, err := openConversionTarget(ctx, target, databasePath, definitions, jsonDefinitions)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, destination.Close()) }()
	for _, definition := range definitions {
		state := manifest.Namespaces[definition.Name]
		if state == nil {
			return fmt.Errorf("metadata namespace %q is missing from conversion manifest", definition.Name)
		}
		if state.Done {
			continue
		}
		if err := copyConversionNamespace(ctx, source, destination, definition, state); err != nil {
			return fmt.Errorf("convert metadata namespace %s: %w", definition.Name, err)
		}
		if target == "sqlite" {
			sqliteStore, ok := destination.(*metasqlite.Store)
			if !ok {
				return fmt.Errorf("sqlite conversion target has unexpected type %T", destination)
			}
			if err := sqliteStore.MarkConverted(ctx, definition.Name, "json", state.Digest, state.Records); err != nil {
				return err
			}
		} else if err := duplicateJSONGeneration(jsonDefinitions, definition.Name); err != nil {
			return err
		}
		state.Done = true
		if err := saveConversionManifest(manifestPath, manifest); err != nil {
			return err
		}
		if err := fault.Check(ctx, fault.MetadataConvertNamespace); err != nil {
			return err
		}
	}
	for _, definition := range definitions {
		state := manifest.Namespaces[definition.Name]
		digest, records, err := namespaceDigest(ctx, source, definition)
		if err != nil {
			return err
		}
		if digest != state.Digest || records != state.Records {
			return fmt.Errorf("metadata source changed during conversion in namespace %s", definition.Name)
		}
	}
	return nil
}

func copyConversionNamespace(ctx context.Context, source, destination meta.MetaEngine, definition metasqlite.Namespace, state *conversionState) error {
	sourceDigest, sourceRecords, err := namespaceDigest(ctx, source, definition)
	if err != nil {
		return err
	}
	if sourceDigest != state.Digest || sourceRecords != state.Records {
		return fmt.Errorf("source changed after conversion manifest was written")
	}
	targetDigest, targetRecords, err := namespaceDigest(ctx, destination, definition)
	if err != nil {
		return err
	}
	if targetDigest == state.Digest && targetRecords == state.Records {
		return nil
	}
	if targetRecords != 0 {
		return fmt.Errorf("target is not fresh: contains %d record(s)", targetRecords)
	}
	if _, err := meta.TransferWithReport(ctx, source, destination, []meta.TableSet{{Namespace: definition.Name, Tables: definition.Tables}}); err != nil {
		return err
	}
	targetDigest, targetRecords, err = namespaceDigest(ctx, destination, definition)
	if err != nil {
		return err
	}
	if targetDigest != state.Digest || targetRecords != state.Records {
		return fmt.Errorf("target verification failed: records=%d want=%d", targetRecords, state.Records)
	}
	return nil
}

func newConversionManifest(ctx context.Context, target, databasePath string, source meta.MetaEngine, definitions []metasqlite.Namespace, jsonDefinitions []metajson.Namespace) (*conversionManifest, error) {
	manifest := &conversionManifest{Target: target, StartedAt: time.Now().UTC(), Namespaces: make(map[meta.Namespace]*conversionState, len(definitions))}
	for _, definition := range definitions {
		digest, records, err := namespaceDigest(ctx, source, definition)
		if err != nil {
			return nil, err
		}
		manifest.Namespaces[definition.Name] = &conversionState{
			SourceFiles: conversionSourceFiles(target, databasePath, jsonDefinitions, definition.Name),
			Records:     records,
			Digest:      digest,
		}
	}
	return manifest, nil
}

func namespaceDigest(ctx context.Context, engine meta.MetaEngine, definition metasqlite.Namespace) (string, int, error) {
	hash := sha256.New()
	records := 0
	err := engine.View(ctx, []meta.Namespace{definition.Name}, func(reader meta.Reader) error {
		for _, table := range definition.Tables {
			type row struct {
				id  meta.RecordID
				raw json.RawMessage
			}
			var rows []row
			if err := reader.ScanRaw(ctx, definition.Name, table, func(id meta.RecordID, raw json.RawMessage) error {
				rows = append(rows, row{id: id, raw: append(json.RawMessage(nil), raw...)})
				return nil
			}); err != nil {
				return err
			}
			sort.Slice(rows, func(i, j int) bool { return rows[i].id < rows[j].id })
			for _, row := range rows {
				_, _ = fmt.Fprintf(hash, "%s\x00%s\x00%s\x00%s\n", definition.Name, table, row.id, row.raw)
				records++
			}
		}
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), records, nil
}

func openConversionSource(target, databasePath string, definitions []metasqlite.Namespace, jsonDefinitions []metajson.Namespace) (meta.MetaEngine, error) {
	if target == "sqlite" {
		return metajson.Open(jsonDefinitions...)
	}
	if _, err := os.Stat(databasePath); err != nil {
		return nil, fmt.Errorf("open sqlite conversion source: %w", err)
	}
	return metasqlite.OpenForRecovery(databasePath, definitions...)
}

func openConversionTarget(ctx context.Context, target, databasePath string, definitions []metasqlite.Namespace, jsonDefinitions []metajson.Namespace) (meta.MetaEngine, error) {
	if target == "json" {
		return metajson.Open(jsonDefinitions...)
	}
	if _, err := os.Stat(databasePath); errors.Is(err, os.ErrNotExist) {
		if err := metasqlite.InitForRecovery(ctx, databasePath, definitions...); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return metasqlite.OpenForRecovery(databasePath, definitions...)
}

func requireFreshTarget(target, databasePath string, jsonDefinitions []metajson.Namespace) error {
	if target == "sqlite" {
		if _, err := os.Stat(databasePath); err == nil {
			return fmt.Errorf("sqlite conversion target %s already exists", databasePath)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	for _, definition := range jsonDefinitions {
		for _, path := range []string{definition.FilePath, definition.FilePath + ".prev"} {
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("json conversion target %s already exists", path)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func retireConversionSources(ctx context.Context, target, databasePath string, manifest *conversionManifest) error {
	if target == "json" {
		if _, err := os.Stat(databasePath); err == nil {
			if err := metasqlite.Checkpoint(ctx, databasePath); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	seen := make(map[string]struct{})
	for _, state := range manifest.Namespaces {
		for _, path := range state.SourceFiles {
			if _, exists := seen[path]; exists {
				continue
			}
			seen[path] = struct{}{}
			for _, candidate := range sourceFileCandidates(target, path) {
				if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
					continue
				} else if err != nil {
					return err
				}
				if err := os.Rename(candidate, candidate+convertedSuffix+stamp); err != nil {
					return fmt.Errorf("retire metadata source %s: %w", candidate, err)
				}
				if err := syncDirectory(filepath.Dir(candidate)); err != nil {
					return err
				}
				if err := fault.Check(ctx, fault.MetadataConvertRetired); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func sourceFileCandidates(target, path string) []string {
	if target == "json" {
		return []string{path, path + "-wal", path + "-shm"}
	}
	return []string{path}
}

func conversionSourceFiles(target, databasePath string, definitions []metajson.Namespace, namespace meta.Namespace) []string {
	if target == "json" {
		return []string{databasePath}
	}
	for _, definition := range definitions {
		if definition.Name == string(namespace) {
			return []string{definition.FilePath, definition.FilePath + ".prev"}
		}
	}
	return nil
}

func duplicateJSONGeneration(definitions []metajson.Namespace, namespace meta.Namespace) error {
	for _, definition := range definitions {
		if definition.Name != string(namespace) {
			continue
		}
		raw, err := os.ReadFile(definition.FilePath) //nolint:gosec
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		return writeAtomicFile(definition.FilePath+".prev", raw, 0o600)
	}
	return fmt.Errorf("json metadata namespace %q is not declared", namespace)
}

func metadataJSONDefinitions(rootDir string) []metajson.Namespace {
	definitions := []metajson.Namespace{
		vm.JSONNamespace(rootDir),
		image.JSONNamespace(rootDir),
		snapshot.JSONNamespace(rootDir),
	}
	definitions = append(definitions, kbnetwork.JSONNamespaces(rootDir)...)
	definitions = append(definitions,
		ocistore.JSONNamespace(rootDir),
		operation.JSONNamespace(rootDir),
		reference.JSONNamespace(rootDir),
		metering.JSONNamespace(rootDir),
	)
	return definitions
}

func loadConversionManifest(path string) (*conversionManifest, error) {
	raw, err := os.ReadFile(path) //nolint:gosec
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read metadata conversion manifest: %w", err)
	}
	var manifest conversionManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("decode metadata conversion manifest: %w", err)
	}
	if manifest.Target == "" || len(manifest.Namespaces) == 0 {
		return nil, fmt.Errorf("metadata conversion manifest is incomplete: %w", meta.ErrCorrupt)
	}
	return &manifest, nil
}

func saveConversionManifest(path string, manifest *conversionManifest) error {
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode metadata conversion manifest: %w", err)
	}
	raw = append(raw, '\n')
	return writeAtomicFile(path, raw, 0o600)
}

func removeConversionManifest(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove metadata conversion manifest: %w", err)
	}
	return syncDirectory(filepath.Dir(path))
}

func writeAtomicFile(path string, raw []byte, mode os.FileMode) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".kumabox-metadata-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(raw); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) (err error) {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	if err := directory.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return nil
}

func conversionComplete(manifest *conversionManifest) bool {
	for _, state := range manifest.Namespaces {
		if !state.Done {
			return false
		}
	}
	return true
}

func conversionResult(target, databasePath string, manifest *conversionManifest) ConversionResult {
	result := ConversionResult{Backend: target}
	if target == "sqlite" {
		result.Path = databasePath
	}
	for namespace, state := range manifest.Namespaces {
		result.Namespaces = append(result.Namespaces, ConversionNamespace{Namespace: namespace, Records: state.Records, Digest: state.Digest})
	}
	sort.Slice(result.Namespaces, func(i, j int) bool { return result.Namespaces[i].Namespace < result.Namespaces[j].Namespace })
	return result
}

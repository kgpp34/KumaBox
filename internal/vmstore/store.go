package vmstore

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

type Store struct {
	indexPath string
	lockPath  string
}

func New(rootDir string) *Store {
	backendDir := filepath.Join(rootDir, "backends", BackendCloudHypervisor)
	return &Store{
		indexPath: filepath.Join(backendDir, "index.json"),
		lockPath:  filepath.Join(backendDir, "index.lock"),
	}
}

func (s *Store) Create(req CreateRequest) (*VMRecord, error) {
	if err := validateCreateRequest(req); err != nil {
		return nil, err
	}

	var created *VMRecord
	err := s.update(func(idx *VMIndex) error {
		if _, ok := idx.Names[req.Name]; ok {
			return fmt.Errorf("%w: %s", ErrNameConflict, req.Name)
		}

		id, err := newID()
		if err != nil {
			return err
		}
		for {
			if _, exists := idx.VMs[id]; !exists {
				break
			}
			id, err = newID()
			if err != nil {
				return err
			}
		}

		rootDisk, err := normalizePath(req.RootDisk)
		if err != nil {
			return fmt.Errorf("resolve root disk: %w", err)
		}
		kernel, err := normalizePath(req.Kernel)
		if err != nil {
			return fmt.Errorf("resolve kernel: %w", err)
		}
		initrd, err := normalizePath(req.Initrd)
		if err != nil {
			return fmt.Errorf("resolve initrd: %w", err)
		}

		now := time.Now().UTC()
		rec := &VMRecord{
			ID:        id,
			Name:      req.Name,
			Backend:   BackendCloudHypervisor,
			State:     StateCreated,
			RootDisk:  rootDisk,
			Kernel:    kernel,
			Initrd:    initrd,
			RunDir:    filepath.Join(req.RunDir, "vms", id),
			LogDir:    filepath.Join(req.LogDir, "vms", id),
			CreatedAt: now,
			UpdatedAt: now,
		}

		idx.VMs[id] = rec
		idx.Names[req.Name] = id
		created = cloneRecord(rec)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return created, nil
}

func (s *Store) Inspect(ref string) (*VMRecord, error) {
	var rec *VMRecord
	err := s.withIndex(func(idx *VMIndex) error {
		id, err := idx.Resolve(ref)
		if err != nil {
			return err
		}
		rec = cloneRecord(idx.VMs[id])
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rec, nil
}

func (s *Store) List() ([]*VMRecord, error) {
	var records []*VMRecord
	err := s.withIndex(func(idx *VMIndex) error {
		records = make([]*VMRecord, 0, len(idx.VMs))
		for _, rec := range idx.VMs {
			records = append(records, cloneRecord(rec))
		}
		sort.Slice(records, func(i, j int) bool {
			return records[i].CreatedAt.Before(records[j].CreatedAt)
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

func (s *Store) withIndex(fn func(*VMIndex) error) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()

	idx, err := s.load()
	if err != nil {
		return err
	}
	return fn(idx)
}

func (s *Store) update(fn func(*VMIndex) error) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()

	idx, err := s.load()
	if err != nil {
		return err
	}
	if err := fn(idx); err != nil {
		return err
	}
	return s.write(idx)
}

func (s *Store) load() (*VMIndex, error) {
	raw, err := os.ReadFile(s.indexPath) //nolint:gosec
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			idx := &VMIndex{}
			idx.Init()
			return idx, nil
		}
		return nil, fmt.Errorf("read VM index: %w", err)
	}

	var idx VMIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("parse VM index: %w", err)
	}
	idx.Init()
	return &idx, nil
}

func (s *Store) write(idx *VMIndex) error {
	if err := os.MkdirAll(filepath.Dir(s.indexPath), 0o755); err != nil {
		return fmt.Errorf("create VM index dir: %w", err)
	}

	raw, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal VM index: %w", err)
	}
	raw = append(raw, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(s.indexPath), ".index-*.tmp")
	if err != nil {
		return fmt.Errorf("create VM index temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) //nolint:errcheck

	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write VM index temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync VM index temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close VM index temp file: %w", err)
	}
	if err := os.Rename(tmpPath, s.indexPath); err != nil {
		return fmt.Errorf("commit VM index: %w", err)
	}
	return nil
}

func (s *Store) lock() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(s.lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("create VM index lock dir: %w", err)
	}

	file, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open VM index lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock VM index: %w", err)
	}

	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

func validateCreateRequest(req CreateRequest) error {
	if req.Name == "" {
		return errors.New("name must not be empty")
	}
	if req.RootDisk == "" {
		return errors.New("root disk must not be empty")
	}
	if req.Kernel == "" {
		return errors.New("kernel must not be empty")
	}
	if req.Initrd == "" {
		return errors.New("initrd must not be empty")
	}
	if req.RunDir == "" {
		return errors.New("run dir must not be empty")
	}
	if req.LogDir == "" {
		return errors.New("log dir must not be empty")
	}
	return nil
}

func newID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate VM ID: %w", err)
	}
	return "kb_" + hex.EncodeToString(raw[:]), nil
}

func cloneRecord(rec *VMRecord) *VMRecord {
	if rec == nil {
		return nil
	}
	copied := *rec
	return &copied
}

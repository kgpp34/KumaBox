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

	"github.com/kumabox/kumabox/internal/fileutil"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
)

type Store struct {
	indexPath string
	lockPath  string
}

func New(rootDir string) *Store {
	backendDir := filepath.Join(rootDir, "backends", backendCloudHypervisor)
	return &Store{
		indexPath: filepath.Join(backendDir, "index.json"),
		lockPath:  filepath.Join(backendDir, "index.lock"),
	}
}

type CreateRequest struct {
	Name     string
	RootDisk string
	Kernel   string
	Initrd   string
	Firmware string
	Image    *ImageRef
	Network  string
	RunDir   string
	LogDir   string
}

func (s *Store) Create(req CreateRequest) (*VMRecord, error) {
	if err := validateCreateRequest(req); err != nil {
		return nil, err
	}

	var created *VMRecord
	err := s.update(func(idx *vmIndex) error {
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

		now := time.Now().UTC()
		rec, err := newRecord(id, req, now)
		if err != nil {
			return fmt.Errorf("create VM record: %w", err)
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
	err := s.withIndex(func(idx *vmIndex) error {
		id, err := idx.resolve(ref)
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

func (s *Store) Delete(ref string) error {
	return s.update(func(idx *vmIndex) error {
		id, err := idx.resolve(ref)
		if err != nil {
			return err
		}
		rec := idx.VMs[id]
		if rec != nil {
			delete(idx.Names, rec.Name)
		}
		delete(idx.VMs, id)
		return nil
	})
}

func (s *Store) MarkRunning(ref string, pid int, apiSocket string) (*VMRecord, error) {
	var updated *VMRecord
	err := s.update(func(idx *vmIndex) error {
		id, err := idx.resolve(ref)
		if err != nil {
			return err
		}
		rec := idx.VMs[id]
		now := time.Now().UTC()
		rec.State = StateRunning
		rec.PID = pid
		rec.APISocket = apiSocket
		rec.Error = ""
		rec.StartedAt = &now
		if rec.Metadata != nil {
			rec.FirstBooted = true
		}
		rec.UpdatedAt = now
		updated = cloneRecord(rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *Store) MarkError(ref string, message string) (*VMRecord, error) {
	var updated *VMRecord
	err := s.update(func(idx *vmIndex) error {
		id, err := idx.resolve(ref)
		if err != nil {
			return err
		}
		rec := idx.VMs[id]
		now := time.Now().UTC()
		rec.State = StateError
		rec.Error = message
		rec.UpdatedAt = now
		updated = cloneRecord(rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *Store) MarkStopped(ref string) (*VMRecord, error) {
	var updated *VMRecord
	err := s.update(func(idx *vmIndex) error {
		id, err := idx.resolve(ref)
		if err != nil {
			return err
		}
		rec := idx.VMs[id]
		now := time.Now().UTC()
		rec.State = StateStopped
		rec.PID = 0
		rec.APISocket = ""
		rec.Error = ""
		rec.StoppedAt = &now
		rec.UpdatedAt = now
		updated = cloneRecord(rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *Store) SetNetworkConfigs(ref string, configs []kbnetwork.Config) (*VMRecord, error) {
	var updated *VMRecord
	err := s.update(func(idx *vmIndex) error {
		id, err := idx.resolve(ref)
		if err != nil {
			return err
		}
		rec := idx.VMs[id]
		now := time.Now().UTC()
		rec.NetworkConfigs = cloneNetworkConfigs(configs)
		rec.UpdatedAt = now
		updated = cloneRecord(rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *Store) List() ([]*VMRecord, error) {
	var records []*VMRecord
	err := s.withIndex(func(idx *vmIndex) error {
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

func (s *Store) withIndex(fn func(*vmIndex) error) error {
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

func (s *Store) update(fn func(*vmIndex) error) error {
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

func (s *Store) load() (*vmIndex, error) {
	raw, err := os.ReadFile(s.indexPath) //nolint:gosec
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			idx := &vmIndex{}
			idx.init()
			return idx, nil
		}
		return nil, fmt.Errorf("read VM index: %w", err)
	}

	var idx vmIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("parse VM index: %w", err)
	}
	idx.init()
	return &idx, nil
}

func (s *Store) write(idx *vmIndex) error {
	if err := fileutil.WriteJSONAtomic(s.indexPath, idx, ".index-*.tmp"); err != nil {
		return fmt.Errorf("write VM index: %w", err)
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
	if req.Firmware == "" && req.Kernel == "" {
		return errors.New("kernel must not be empty for direct boot")
	}
	if req.Firmware == "" && req.Initrd == "" {
		return errors.New("initrd must not be empty for direct boot")
	}
	if req.Firmware != "" && (req.Kernel != "" || req.Initrd != "") {
		return errors.New("firmware boot cannot be combined with kernel or initrd")
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

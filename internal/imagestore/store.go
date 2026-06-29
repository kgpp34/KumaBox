package imagestore

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
)

type Store struct {
	indexPath string
	lockPath  string
}

func New(rootDir string) *Store {
	imageDir := filepath.Join(rootDir, "images")
	return &Store{
		indexPath: filepath.Join(imageDir, "index.json"),
		lockPath:  filepath.Join(imageDir, "index.lock"),
	}
}

type CreateRequest struct {
	Name     string
	Source   Source
	RootDisk RootDisk
	Boot     Boot
	OS       OS
}

func (s *Store) Create(req CreateRequest) (*ImageRecord, error) {
	if err := validateCreateRequest(req); err != nil {
		return nil, err
	}

	var created *ImageRecord
	err := s.update(func(idx *imageIndex) error {
		if _, ok := idx.Names[req.Name]; ok {
			return fmt.Errorf("%w: %s", ErrNameConflict, req.Name)
		}

		id, err := newID()
		if err != nil {
			return err
		}
		for {
			if _, exists := idx.Images[id]; !exists {
				break
			}
			id, err = newID()
			if err != nil {
				return err
			}
		}

		now := time.Now().UTC()
		rec := &ImageRecord{
			SchemaVersion: "kumabox.image.v1",
			ID:            id,
			Name:          req.Name,
			Source:        req.Source,
			RootDisk:      req.RootDisk,
			Boot:          req.Boot,
			OS:            req.OS,
			CreatedAt:     now,
			UpdatedAt:     now,
		}

		idx.Images[id] = rec
		idx.Names[req.Name] = id
		created = cloneRecord(rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

func (s *Store) Inspect(ref string) (*ImageRecord, error) {
	var rec *ImageRecord
	err := s.withIndex(func(idx *imageIndex) error {
		id, err := idx.resolve(ref)
		if err != nil {
			return err
		}
		rec = cloneRecord(idx.Images[id])
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rec, nil
}

func (s *Store) List() ([]*ImageRecord, error) {
	var records []*ImageRecord
	err := s.withIndex(func(idx *imageIndex) error {
		records = make([]*ImageRecord, 0, len(idx.Images))
		for _, rec := range idx.Images {
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

func (s *Store) withIndex(fn func(*imageIndex) error) error {
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

func (s *Store) update(fn func(*imageIndex) error) error {
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

func (s *Store) load() (*imageIndex, error) {
	raw, err := os.ReadFile(s.indexPath) //nolint:gosec
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			idx := &imageIndex{}
			idx.init()
			return idx, nil
		}
		return nil, fmt.Errorf("read image index: %w", err)
	}

	var idx imageIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("parse image index: %w", err)
	}
	idx.init()
	return &idx, nil
}

func (s *Store) write(idx *imageIndex) error {
	if err := fileutil.WriteJSONAtomic(s.indexPath, idx, ".index-*.tmp"); err != nil {
		return fmt.Errorf("write image index: %w", err)
	}
	return nil
}

func (s *Store) lock() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(s.lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("create image index lock dir: %w", err)
	}

	file, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open image index lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock image index: %w", err)
	}

	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

func validateCreateRequest(req CreateRequest) error {
	if req.Name == "" {
		return errors.New("image name must not be empty")
	}
	return nil
}

func newID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate image ID: %w", err)
	}
	return "img_" + hex.EncodeToString(raw[:]), nil
}

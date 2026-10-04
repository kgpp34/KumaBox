package e2b

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	filelock "github.com/kumabox/kumabox/lock/flock"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
)

const defaultTimeout = 300 * time.Second

// lease is API-owned lifecycle policy. Native KumaBox sandboxes have no lease.
// Keeping it outside the sandbox aggregate avoids changing its state machine.
type lease struct {
	ExpiresAt         time.Time          `json:"expires_at"`
	AutoPause         bool               `json:"auto_pause,omitempty"`
	PausedSnapshot    types.SnapshotID   `json:"paused_snapshot,omitempty"`
	RetainedSnapshots []types.SnapshotID `json:"retained_snapshots,omitempty"`
}

// leaseStore persists E2B deadlines independently of the HTTP process. The
// configured data root can be overridden when the KumaBox host uses a custom
// root; an owner-only directory keeps E2B control data private.
type leaseStore struct {
	dir    string
	mu     sync.Mutex
	timers map[types.SandboxID]*time.Timer
}

func openLeaseStore() (*leaseStore, error) {
	root := os.Getenv("KUMABOX_E2B_STATE_DIR")
	if root == "" {
		dataRoot := os.Getenv("KUMABOX_PATHS_DATA")
		if dataRoot == "" {
			dataRoot = storage.DefaultDataRoot
		}
		root = filepath.Join(dataRoot, "e2b")
	}
	if !filepath.IsAbs(root) {
		return nil, errors.New("KUMABOX_E2B_STATE_DIR must be absolute")
	}
	if err := storage.EnsureDir(root); err != nil {
		return nil, err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("secure E2B lifecycle state directory: %w", err)
	}
	return &leaseStore{dir: root, timers: make(map[types.SandboxID]*time.Timer)}, nil
}

func (s *leaseStore) path(id types.SandboxID) (string, error) {
	if _, err := types.ParseSandboxID(id.String()); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, id.String()+".json"), nil
}

func (s *leaseStore) lock(ctx context.Context, id types.SandboxID) (func(), error) {
	path, err := s.path(id)
	if err != nil {
		return nil, err
	}
	lock := filelock.New(path + ".lock")
	if err := lock.Lock(ctx); err != nil {
		return nil, err
	}
	return func() {
		if err := lock.Unlock(context.WithoutCancel(ctx)); err != nil {
			slog.Error("release E2B sandbox lock", "sandbox", id, "error", err)
		}
	}, nil
}

func (s *leaseStore) get(id types.SandboxID) (lease, bool, error) {
	path, err := s.path(id)
	if err != nil {
		return lease{}, false, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return lease{}, false, nil
	}
	if err != nil {
		return lease{}, false, err
	}
	var value lease
	if err := json.Unmarshal(data, &value); err != nil {
		return lease{}, false, fmt.Errorf("decode E2B lease %s: %w", id, err)
	}
	return value, true, nil
}

func (s *leaseStore) put(id types.SandboxID, value lease) (returnErr error) {
	path, err := s.path(id)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(s.dir, ".lease-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if err := json.NewEncoder(file).Encode(value); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, dir.Close()) }()
	return dir.Sync()
}

func (s *leaseStore) remove(id types.SandboxID) error {
	path, err := s.path(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.mu.Lock()
	if timer := s.timers[id]; timer != nil {
		timer.Stop()
		delete(s.timers, id)
	}
	s.mu.Unlock()
	return nil
}

func (s *leaseStore) schedule(id types.SandboxID, deadline time.Time, expire func(types.SandboxID)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if timer := s.timers[id]; timer != nil {
		timer.Stop()
		delete(s.timers, id)
	}
	if !deadline.IsZero() {
		s.timers[id] = time.AfterFunc(max(time.Until(deadline), 0), func() { expire(id) })
	}
}

func (s *leaseStore) load(expire func(types.SandboxID)) error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id, err := types.ParseSandboxID(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return fmt.Errorf("invalid E2B lease filename %q: %w", entry.Name(), err)
		}
		value, exists, err := s.get(id)
		if err != nil {
			return err
		}
		if exists {
			s.schedule(id, value.ExpiresAt, expire)
		}
	}
	return nil
}

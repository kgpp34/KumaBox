package json

import (
	"context"
	"crypto/sha256"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kumabox/kumabox/internal/fault"
	"github.com/kumabox/kumabox/internal/lock"
	"github.com/kumabox/kumabox/internal/meta"
)

const previousSuffix = ".prev"

// Namespace describes one logical metadata namespace and its legacy JSON
// representation.
type Namespace struct {
	Name     string
	FilePath string
	LockPath string
	Codec    Codec
}

// Store is the JSON MetaEngine. It owns no domain records; codecs and callers
// define their table meaning.
type Store struct {
	namespaces map[string]Namespace
	mu         sync.Mutex
	subs       map[*subscription]struct{}
	closed     bool
}

type subscription struct {
	changes chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}
	stop    sync.Once
}

func (s *subscription) close() {
	s.stop.Do(func() {
		s.cancel()
		<-s.done
		close(s.changes)
	})
}

var _ meta.MetaEngine = (*Store)(nil)

// Open validates namespace definitions without creating files.
func Open(definitions ...Namespace) (*Store, error) {
	if len(definitions) == 0 {
		return nil, fmt.Errorf("JSON metadata engine requires a namespace")
	}
	namespaces := make(map[string]Namespace, len(definitions))
	for _, definition := range definitions {
		if definition.Name == "" || definition.FilePath == "" || definition.LockPath == "" || definition.Codec == nil {
			return nil, fmt.Errorf("metadata namespace %q has incomplete definition: %w", definition.Name, meta.ErrScope)
		}
		if _, exists := namespaces[definition.Name]; exists {
			return nil, fmt.Errorf("metadata namespace %q declared twice: %w", definition.Name, meta.ErrScope)
		}
		namespaces[definition.Name] = definition
	}
	return &Store{namespaces: namespaces, subs: make(map[*subscription]struct{})}, nil
}

func (s *Store) View(ctx context.Context, requested []meta.Namespace, fn func(meta.Reader) error) error {
	if fn == nil {
		return fmt.Errorf("metadata view callback must not be nil: %w", meta.ErrScope)
	}
	definitions, err := s.resolve(requested, "")
	if err != nil {
		return err
	}
	locks, err := s.acquire(ctx, definitions)
	if err != nil {
		return err
	}
	defer releaseLocks(locks)

	models, err := s.load(ctx, definitions)
	if err != nil {
		return err
	}
	return fn(&reader{models: models, allowed: names(definitions)})
}

func (s *Store) Update(ctx context.Context, scope meta.Scope, mode meta.CommitMode, fn func(meta.Writer) error) error {
	if fn == nil {
		return fmt.Errorf("metadata update callback must not be nil: %w", meta.ErrScope)
	}
	definitions, err := s.resolve(append([]meta.Namespace{scope.Write}, scope.Read...), scope.Write)
	if err != nil {
		return err
	}
	locks, err := s.acquire(ctx, definitions)
	if err != nil {
		return err
	}
	defer releaseLocks(locks)

	models, err := s.load(ctx, definitions)
	if err != nil {
		return err
	}
	writer := &writer{
		reader:         reader{models: models, allowed: names(definitions)},
		writeNamespace: scope.Write,
		dirty:          false,
	}
	if err := fn(writer); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if writer.dirty {
		if err := s.commit(ctx, definitions, models, scope.Write, mode); err != nil {
			return err
		}
		s.notify()
	}
	return nil
}

func (s *Store) Events(ctx context.Context) (<-chan struct{}, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	fingerprint, err := s.fingerprint()
	if err != nil {
		return nil, nil, err
	}
	watchCtx, cancel := context.WithCancel(ctx)
	sub := &subscription{changes: make(chan struct{}, 1), cancel: cancel, done: make(chan struct{})}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cancel()
		return nil, nil, meta.ErrClosed
	}
	s.subs[sub] = struct{}{}
	s.mu.Unlock()
	go s.watchFiles(watchCtx, sub, fingerprint)

	var once sync.Once
	release := func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.subs, sub)
			s.mu.Unlock()
			sub.close()
		})
	}
	return sub.changes, release, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	subs := make([]*subscription, 0, len(s.subs))
	for sub := range s.subs {
		subs = append(subs, sub)
		delete(s.subs, sub)
	}
	s.mu.Unlock()
	for _, sub := range subs {
		sub.close()
	}
	return nil
}

func (s *Store) notify() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	for sub := range s.subs {
		select {
		case sub.changes <- struct{}{}:
		default:
		}
	}
}

func (s *Store) watchFiles(ctx context.Context, sub *subscription, previous [32]byte) {
	defer close(sub.done)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current, err := s.fingerprint()
			if err != nil || current == previous {
				continue
			}
			previous = current
			s.mu.Lock()
			if _, ok := s.subs[sub]; ok && !s.closed {
				select {
				case sub.changes <- struct{}{}:
				default:
				}
			}
			s.mu.Unlock()
		}
	}
}

func (s *Store) fingerprint() ([32]byte, error) {
	hash := sha256.New()
	names := make([]string, 0, len(s.namespaces))
	for name := range s.namespaces {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := s.namespaces[name].FilePath
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			_, _ = fmt.Fprintf(hash, "%s:missing\n", path)
			continue
		}
		if err != nil {
			return [32]byte{}, fmt.Errorf("fingerprint metadata %s: %w", path, err)
		}
		_, _ = fmt.Fprintf(hash, "%s:%d:", path, len(raw))
		_, _ = hash.Write(raw)
	}
	var fingerprint [32]byte
	copy(fingerprint[:], hash.Sum(nil))
	return fingerprint, nil
}

func (s *Store) resolve(requested []meta.Namespace, write meta.Namespace) ([]Namespace, error) {
	seen := make(map[string]struct{}, len(requested))
	for _, name := range requested {
		if name == "" {
			return nil, fmt.Errorf("metadata namespace must not be empty: %w", meta.ErrScope)
		}
		if _, ok := s.namespaces[string(name)]; !ok {
			return nil, fmt.Errorf("metadata namespace %q is not declared: %w", name, meta.ErrScope)
		}
		seen[string(name)] = struct{}{}
	}
	if write != "" {
		if _, ok := seen[string(write)]; !ok {
			return nil, fmt.Errorf("write namespace %q is outside scope: %w", write, meta.ErrScope)
		}
	}
	definitions := make([]Namespace, 0, len(seen))
	for name := range seen {
		definitions = append(definitions, s.namespaces[string(name)])
	}
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Name < definitions[j].Name })
	return definitions, nil
}

func (s *Store) acquire(ctx context.Context, definitions []Namespace) ([]*lock.Lock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, meta.ErrClosed
	}
	locks := make([]*lock.Lock, 0, len(definitions))
	for _, definition := range definitions {
		fileLock, err := lock.NewLocker(filepath.Dir(definition.LockPath)).Acquire(ctx, lockKey(definition.LockPath))
		if err != nil {
			releaseLocks(locks)
			return nil, fmt.Errorf("lock metadata namespace %s: %w", definition.Name, err)
		}
		locks = append(locks, fileLock)
	}
	return locks, nil
}

func (s *Store) load(ctx context.Context, definitions []Namespace) (map[string]*loaded, error) {
	models := make(map[string]*loaded, len(definitions))
	for _, definition := range definitions {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		loaded, err := loadNamespace(definition)
		if err != nil {
			return nil, fmt.Errorf("load metadata namespace %s: %w", definition.Name, err)
		}
		models[definition.Name] = loaded
	}
	return models, nil
}

func (s *Store) commit(ctx context.Context, definitions []Namespace, models map[string]*loaded, write meta.Namespace, _ meta.CommitMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, definition := range definitions {
		if definition.Name != string(write) {
			continue
		}
		current := models[string(write)]
		raw, err := definition.Codec.Encode(current.model)
		if err != nil {
			return fmt.Errorf("encode metadata namespace %s: %w", write, err)
		}
		if err := writeAtomic(ctx, definition.FilePath, raw, current.raw); err != nil {
			return fmt.Errorf("commit metadata namespace %s: %w", write, err)
		}
		return nil
	}
	return fmt.Errorf("write namespace %q was not resolved: %w", write, meta.ErrScope)
}

type loaded struct {
	model     *Model
	raw       []byte
	recovered bool
}

func loadNamespace(definition Namespace) (*loaded, error) {
	raw, err := os.ReadFile(definition.FilePath)
	if errors.Is(err, os.ErrNotExist) {
		model, decodeErr := definition.Codec.Decode(nil)
		if decodeErr != nil {
			return nil, fmt.Errorf("initialize empty metadata namespace: %w", decodeErr)
		}
		return &loaded{model: model}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read metadata file: %w", err)
	}
	model, decodeErr := definition.Codec.Decode(raw)
	if decodeErr == nil {
		return &loaded{model: model, raw: append([]byte(nil), raw...)}, nil
	}
	previous, previousErr := os.ReadFile(definition.FilePath + previousSuffix)
	if previousErr == nil {
		previousModel, previousDecodeErr := definition.Codec.Decode(previous)
		if previousDecodeErr == nil {
			return &loaded{model: previousModel, raw: append([]byte(nil), previous...), recovered: true}, nil
		}
	}
	return nil, fmt.Errorf("decode metadata file: %w: %v", meta.ErrCorrupt, decodeErr)
}

type reader struct {
	models  map[string]*loaded
	allowed map[string]struct{}
}

func (r reader) GetRaw(ctx context.Context, namespace meta.Namespace, table meta.Table, id meta.RecordID) (stdjson.RawMessage, bool, error) {
	if err := contextErr(ctx); err != nil {
		return nil, false, err
	}
	if err := r.checkRead(namespace); err != nil {
		return nil, false, err
	}
	model := r.models[string(namespace)].model
	records := model.Tables[string(table)]
	if records == nil {
		return nil, false, nil
	}
	raw, ok := records[string(id)]
	return cloneRaw(raw), ok, nil
}

func (r reader) ScanRaw(ctx context.Context, namespace meta.Namespace, table meta.Table, fn func(meta.RecordID, stdjson.RawMessage) error) error {
	if fn == nil {
		return fmt.Errorf("metadata scan callback must not be nil: %w", meta.ErrScope)
	}
	if err := r.checkRead(namespace); err != nil {
		return err
	}
	records := r.models[string(namespace)].model.Tables[string(table)]
	ids := make([]string, 0, len(records))
	for id := range records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := contextErr(ctx); err != nil {
			return err
		}
		if err := fn(meta.RecordID(id), cloneRaw(records[id])); err != nil {
			return err
		}
	}
	return nil
}

func (r reader) checkRead(namespace meta.Namespace) error {
	if _, ok := r.allowed[string(namespace)]; !ok {
		return fmt.Errorf("cannot read metadata namespace %q outside transaction scope: %w", namespace, meta.ErrScope)
	}
	return nil
}

type writer struct {
	reader
	writeNamespace meta.Namespace
	dirty          bool
}

func (w *writer) PutRaw(ctx context.Context, namespace meta.Namespace, table meta.Table, id meta.RecordID, raw stdjson.RawMessage) error {
	if err := w.checkWrite(ctx, namespace, table, id); err != nil {
		return err
	}
	if raw == nil || !stdjson.Valid(raw) {
		return fmt.Errorf("metadata record %s/%s is invalid JSON: %w", table, id, meta.ErrIO)
	}
	model := w.models[string(namespace)].model
	if model.Tables == nil {
		model.Tables = map[string]map[string]stdjson.RawMessage{}
	}
	if model.Tables[string(table)] == nil {
		model.Tables[string(table)] = map[string]stdjson.RawMessage{}
	}
	model.Tables[string(table)][string(id)] = cloneRaw(raw)
	w.dirty = true
	return nil
}

func (w *writer) DeleteRaw(ctx context.Context, namespace meta.Namespace, table meta.Table, id meta.RecordID) error {
	if err := w.checkWrite(ctx, namespace, table, id); err != nil {
		return err
	}
	delete(w.models[string(namespace)].model.Tables[string(table)], string(id))
	w.dirty = true
	return nil
}

func (w *writer) checkWrite(ctx context.Context, namespace meta.Namespace, table meta.Table, id meta.RecordID) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	if namespace != w.writeNamespace {
		return fmt.Errorf("cannot write metadata namespace %q from %q transaction: %w", namespace, w.writeNamespace, meta.ErrScope)
	}
	if table == "" || id == "" {
		return fmt.Errorf("metadata table and id must not be empty: %w", meta.ErrScope)
	}
	return nil
}

func writeAtomic(ctx context.Context, path string, raw, previous []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create metadata directory: %w", err)
	}
	if len(previous) > 0 {
		if err := writeFileSync(ctx, path+previousSuffix, previous, ".prev-*.tmp", false); err != nil {
			return fmt.Errorf("preserve previous metadata generation: %w", err)
		}
	}
	if err := writeFileSync(ctx, path, raw, ".meta-*.tmp", true); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func writeFileSync(ctx context.Context, path string, raw []byte, pattern string, inject bool) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), pattern)
	if err != nil {
		return fmt.Errorf("create metadata temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) //nolint:errcheck
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write metadata temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync metadata temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close metadata temporary file: %w", err)
	}
	if inject {
		if err := fault.Check(ctx, fault.MetadataJSONBeforeRename); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("publish metadata file: %w", err)
	}
	if inject {
		if err := fault.Check(ctx, fault.MetadataJSONAfterRename); err != nil {
			return err
		}
	}
	return nil
}

func syncDirectory(path string) (err error) {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open metadata directory: %w", err)
	}
	defer func() {
		if closeErr := dir.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close metadata directory: %w", closeErr)
		}
	}()
	if err := dir.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return fmt.Errorf("sync metadata directory: %w", err)
	}
	return nil
}

func releaseLocks(locks []*lock.Lock) {
	for i := len(locks) - 1; i >= 0; i-- {
		_ = locks[i].Release()
	}
}

func lockKey(path string) string {
	key := filepath.Base(path)
	return strings.TrimSuffix(key, filepath.Ext(key))
}

func names(definitions []Namespace) map[string]struct{} {
	allowed := make(map[string]struct{}, len(definitions))
	for _, definition := range definitions {
		allowed[definition.Name] = struct{}{}
	}
	return allowed
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

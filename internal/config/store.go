package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/home"
)

// Config access of one process, every call resolving its home from ctx so one process serves every profile
type Store struct {
	homes        *home.Resolver
	file         *fileBackend
	backends     map[string]Backend
	bootOnce     sync.Once
	bootstrapEnv func(ctx context.Context)
	env          func(ctx context.Context, name string) (string, bool)
	parseEnvFile func(data []byte) (map[string]string, error)
	firstStamp   func(ctx context.Context, home string, onDisk, toWrite *yaml.Node) error
	onSave       func(ctx context.Context, before, after *YAMLDocument)
	worker       *workerSnapshot
	defOnce      sync.Once
	defTree      *yaml.Node
	defErr       error

	mu       sync.Mutex
	cacheMu  sync.Mutex
	loads    map[string]*loadEntry
	raws     map[string]*rawEntry
	effects  map[string]*effectiveEntry
	lastGood map[string]*yaml.Node
	lastUser map[string]*yaml.Node
	warned   map[string]bool
	failures map[string]failureRecord
	managed  managedCache
}

// Setting of a store given at construction
type Option func(*Store)

// Registers the backend kind ICEBATH_CONFIG_BACKEND may select, the remote backend among them
func WithBackend(kind string, b Backend) Option {
	return func(s *Store) { s.backends[strings.ToLower(kind)] = b }
}

// Binds the frozen config a managed worker serves for life, a bypass worker freezing a copy of the schema
func WithWorkerSnapshot(snapshot *YAMLDocument, ignoreUserConfig bool) Option {
	return func(s *Store) {
		s.worker = &workerSnapshot{root: plainCopy(snapshot.root), ignoreUserConfig: ignoreUserConfig}
		if s.worker.root == nil || isNull(s.worker.root) {
			s.worker.root = emptyMapping()
		}
	}
}

// Observer each completed save calls outside the store lock with the user document before and the state written
func OnSave(fn func(ctx context.Context, before, after *YAMLDocument)) Option {
	return func(s *Store) { s.onSave = fn }
}

// Store resolving homes through r
func NewStore(r *home.Resolver, opts ...Option) *Store {
	s := &Store{
		homes:    r,
		backends: map[string]Backend{},
		env:      func(_ context.Context, name string) (string, bool) { return os.LookupEnv(name) },
		loads:    map[string]*loadEntry{},
		raws:     map[string]*rawEntry{},
		effects:  map[string]*effectiveEntry{},
		lastGood: map[string]*yaml.Node{},
		lastUser: map[string]*yaml.Node{},
		warned:   map[string]bool{},
		failures: map[string]failureRecord{},
	}
	s.file = &fileBackend{store: s}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Frozen config of a managed worker
type workerSnapshot struct {
	root             *yaml.Node
	ignoreUserConfig bool
}

// Reports a config.yaml read inside a worker serving a frozen snapshot, whose cache keys stay stat only
func (s *Store) frozenWorker(path string) bool {
	return s.worker != nil && filepath.Base(path) == filepath.Base(home.Config.In(""))
}

// Context key marking the store lock as held by the caller
type heldStoreKey struct{ s *Store }

// One hold of the store lock, reentrant only while it is still held
type storeHold struct {
	held atomic.Bool
	once sync.Once
}

// Takes the store lock that serializes config rebuilds and writes, reentrant through the returned ctx until released
func (s *Store) Lock(ctx context.Context) (context.Context, func()) {
	if h, ok := ctx.Value(heldStoreKey{s}).(*storeHold); ok && h.held.Load() {
		return ctx, func() {}
	}
	s.mu.Lock()
	h := &storeHold{}
	h.held.Store(true)
	return context.WithValue(ctx, heldStoreKey{s}, h), func() {
		h.once.Do(func() {
			h.held.Store(false)
			s.mu.Unlock()
		})
	}
}

// Lock file beside a config file that serializes its writers across processes
func configLockPath(path string) string {
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".lock")
}

// Takes the store lock and the lock beside a local file so a read and its write meet no writer
func (s *Store) lockWrite(ctx context.Context, path string) (context.Context, func() error, error) {
	ctx, unlock := s.Lock(ctx)
	b, _, err := s.route(ctx, path)
	if err != nil {
		unlock()
		return nil, nil, err
	}
	fb, local := b.(*fileBackend)
	if !local {
		return ctx, func() error { unlock(); return nil }, nil
	}
	ctx, release, err := fb.lockFile(ctx, path)
	if err != nil {
		unlock()
		return nil, nil, err
	}
	return ctx, func() error {
		err := release()
		unlock()
		return err
	}, nil
}

// Home of ctx and the path of its config.yaml
func (s *Store) configPath(ctx context.Context) (string, string, error) {
	h, err := s.homes.Home(ctx)
	if err != nil {
		return "", "", err
	}
	return h, home.Config.In(h), nil
}

// Stat signature of a file, its modification time leading and size, inode and change time after it
func fileSignature(fi fs.FileInfo) Revision {
	ino, ctime := statIdentity(fi)
	return Revision{Stamp: fi.ModTime().UnixNano(), Rest: fmt.Sprintf("%d:%d:%d", fi.Size(), ino, ctime)}
}

// Reports a file revision too fresh to rule out a write in the same timestamp tick, judging the later stamp
func racy(v Revision) bool {
	stamp := v.Stamp
	parts := strings.Split(strings.SplitN(v.Rest, "|", 2)[0], ":")
	if len(parts) == 3 {
		if c, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
			stamp = max(stamp, c)
		}
	}
	return time.Now().UnixNano()-stamp < racyWindowNS
}

// Short digest of data for cache keys
func contentDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

// Version of a file plus a digest of its bytes, hashed on every call when always is set
func fileCacheVersion(path string, always bool) (Revision, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Revision{}, err
	}
	v := fileSignature(fi)
	if always {
		v.Rest += "|" + fileDigest(path)
		return v, nil
	}
	v.Rest += "|" + racyDigest(path, v)
	return v, nil
}

// Value tree of the schema default config, encoded once per store
func (s *Store) defaults() (*yaml.Node, error) {
	s.defOnce.Do(func() {
		n, err := encodeValue(reflect.ValueOf(Default()))
		if err != nil {
			s.defErr = fmt.Errorf("encode the schema defaults: %w", err)
			return
		}
		s.defTree = plainCopy(n)
	})
	return s.defTree, s.defErr
}

package config

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

// Environment variable naming the backend the user config layer lives in, read before any config
const EnvConfigBackend = "ICEBATH_CONFIG_BACKEND"

// Kind of the backend serving the local config.yaml of a home
const FileBackendKind = "file"

// Change signature of a user layer, equal only while the layer is unchanged
type Revision struct {
	Stamp int64
	Rest  string
}

// Number that changes with the version, zero when there is no layer, the file stamp read as seconds
func (v Revision) Token() float64 {
	switch {
	case v == Revision{}:
		return 0
	case v.Stamp != 0:
		return float64(v.Stamp) / 1e9
	}
	if c := crc32.ChecksumIEEE([]byte(v.Rest)); c != 0 {
		return float64(c)
	}
	return 1
}

// One dotted key a write stores, a nil value a kept null or a removal from a file
type KeyValue struct {
	Path  string
	Value any
}

// One write to a user layer, the document applied first, then the keys set and removed in order
type Changes struct {
	Document      *YAMLDocument
	Set           []KeyValue
	Unset         []string
	ExtraOnCreate string
}

// Warning, hint and issue code a backend reports to the doctor after a successful read
type DoctorWarning struct {
	Warning string
	Hint    string
	Issue   string
}

// Where the user config layer of a home lives, keyed per call by home so one process serves every profile
type Backend interface {
	Name() string
	Read(ctx context.Context, home string) (*YAMLDocument, error)
	ReadReadonly(ctx context.Context, home string) (*YAMLDocument, error)
	Version(ctx context.Context, home string) (Revision, error)
	CacheVersion(ctx context.Context, home string, v Revision) (Revision, error)
	Exists(ctx context.Context, home string) (bool, error)
	ProbeReadable(ctx context.Context, home string) error
	WriteChanges(ctx context.Context, home string, c Changes) error
	Locked(ctx context.Context, home, dotted string) (level string, locked bool)
	SupportsFileTooling() bool
	HonorsManagedConfig() bool
	Boot(ctx context.Context, home string) error
	ProtectedEnvNames() []string
	DeploymentEnvNames() []string
	BootstrapEnvNames() []string
	PreserveReadBase(ctx context.Context) context.Context
	DoctorWarnings(ctx context.Context, home string) []DoctorWarning
	ApplyInMemory(ctx context.Context, home string, doc *YAMLDocument) error
	Describe(ctx context.Context, home string) string
	Destination(home string) string
}

// Matched by every refused or failed config write, nothing having changed
var ErrWriteRefused = errors.New("config write refused")

// Matched by the failure to serve config from the selected backend, which callers must not replace with defaults
var ErrBackendUnavailable = errors.New("config backend unavailable")

// Selected backend that cannot serve config
type BackendUnavailableError struct {
	Message string
}

// The reason in one line
func (e *BackendUnavailableError) Error() string { return e.Message }

// Matches ErrBackendUnavailable
func (e *BackendUnavailableError) Unwrap() error { return ErrBackendUnavailable }

// Write a backend refused or could not complete, with a machine code
type WriteError struct {
	Message string
	Code    string
}

// The reason in one line
func (e *WriteError) Error() string { return e.Message }

// Matches ErrWriteRefused
func (e *WriteError) Unwrap() error { return ErrWriteRefused }

// Write touching a path an upper level locks
type LockedError struct {
	Path     string
	LockedBy string
	Message  string
}

// The locked path and the level that locks it
func (e *LockedError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("%s is locked by the %s level", e.Path, e.LockedBy)
}

// Machine code of a lock refusal
func (e *LockedError) Code() string { return "config_key_locked" }

// Matches ErrWriteRefused
func (e *LockedError) Unwrap() error { return ErrWriteRefused }

// Value a backend cannot store, such as a type only YAML has or a secret literal
type RejectedValueError struct {
	Message string
	Code    string
}

// The reason in one line
func (e *RejectedValueError) Error() string { return e.Message }

// Matches ErrWriteRefused
func (e *RejectedValueError) Unwrap() error { return ErrWriteRefused }

// Grammar of a backend kind, one package name and never a path
var backendKind = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Selected user layer store, the file one unless ICEBATH_CONFIG_BACKEND names a registered kind
func (s *Store) backend(ctx context.Context) (Backend, error) {
	var bootErr error
	s.bootOnce.Do(func() { bootErr = s.bootstrapEnv(ctx) })
	if bootErr != nil {
		return nil, bootErr
	}
	kind := strings.ToLower(strings.TrimSpace(os.Getenv(EnvConfigBackend)))
	if kind == "" || kind == FileBackendKind {
		return s.file, nil
	}
	if b, ok := s.backends[kind]; ok && backendKind.MatchString(kind) {
		return b, nil
	}
	return nil, &BackendUnavailableError{Message: fmt.Sprintf("%s=%s is not a config backend this build provides (expected 'file' or 'remote').",
		EnvConfigBackend, quoted(kind))}
}

// Text in single quotes, or double quotes when it holds a single quote and no double quote, escapes included
func quoted(s string) string {
	q := "'"
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		q = `"`
	}
	var b strings.Builder
	b.WriteString(q)
	for _, r := range s {
		switch {
		case r == '\\' || string(r) == q:
			b.WriteRune('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString(q)
	return b.String()
}

// Store serving a config path and its home, a file not named config.yaml served as an explicit file
func (s *Store) route(ctx context.Context, path string) (Backend, string, error) {
	if filepath.Base(path) == filepath.Base(home.Config.In("")) {
		b, err := s.backend(ctx)
		return b, filepath.Dir(path), err
	}
	return &fileBackend{store: s, explicit: path}, filepath.Dir(path), nil
}

// Backend serving the local file at path whichever backend is selected
func (s *Store) LocalFile(path string) Backend {
	return &fileBackend{store: s, explicit: path}
}

// Context markers of the writes a document save makes
type (
	refuseLockedKey struct{}
	pinnedKeysKey   struct{}
)

// Context in which a document save changing a key under one of prefixes refuses when that key is locked
func RefusingLockedOmissions(ctx context.Context, prefixes ...string) context.Context {
	return context.WithValue(ctx, refuseLockedKey{}, append(RefusedLockedOmissionPrefixes(ctx), prefixes...))
}

// Dotted prefixes a document save in ctx must write whole
func RefusedLockedOmissionPrefixes(ctx context.Context) []string {
	v, _ := ctx.Value(refuseLockedKey{}).([]string)
	return append([]string(nil), v...)
}

// Context in which a document save stores keys at the user layer even when unchanged from an upper level
func PinningKeys(ctx context.Context, keys ...string) context.Context {
	return context.WithValue(ctx, pinnedKeysKey{}, append(PinnedKeys(ctx), keys...))
}

// Dotted keys a document save in ctx stores whatever an upper level supplies
func PinnedKeys(ctx context.Context) []string {
	v, _ := ctx.Value(pinnedKeysKey{}).([]string)
	return append([]string(nil), v...)
}

// User layer kept as config.yaml on local disk, or as one explicit file
type fileBackend struct {
	store    *Store
	explicit string
	mu       sync.Mutex
	cache    map[string]readonlyEntry
}

// Parsed file shared by readonly reads while its cache version holds
type readonlyEntry struct {
	version Revision
	doc     *YAMLDocument
}

// Path of the user layer of home
func (f *fileBackend) path(h string) string {
	if f.explicit != "" {
		return f.explicit
	}
	return home.Config.In(h)
}

// Kind name
func (f *fileBackend) Name() string { return FileBackendKind }

// Parsed file as written, the error of the read or parse when it fails
func (f *fileBackend) Read(_ context.Context, h string) (*YAMLDocument, error) {
	return readYAMLFile(f.path(h))
}

// Parsed file re-read only when its cache version changes, the result shared and never to be changed
func (f *fileBackend) ReadReadonly(ctx context.Context, h string) (*YAMLDocument, error) {
	v, err := f.Version(ctx, h)
	if err != nil {
		return nil, err
	}
	if v, err = f.CacheVersion(ctx, h, v); err != nil {
		return nil, err
	}
	path := f.path(h)
	f.mu.Lock()
	if e, ok := f.cache[path]; ok && e.version == v {
		f.mu.Unlock()
		return e.doc, nil
	}
	f.mu.Unlock()
	doc, err := readYAMLFile(path)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	if f.cache == nil {
		f.cache = map[string]readonlyEntry{}
	}
	f.cache[path] = readonlyEntry{version: v, doc: doc}
	f.mu.Unlock()
	return doc, nil
}

// Stat signature of the file, an error matching fs.ErrNotExist when it is absent
func (f *fileBackend) Version(_ context.Context, h string) (Revision, error) {
	fi, err := os.Stat(f.path(h))
	if err != nil {
		return Revision{}, err
	}
	return fileSignature(fi), nil
}

// Version with a digest of the bytes while the stamps are too fresh to rule out a same tick rewrite
func (f *fileBackend) CacheVersion(_ context.Context, h string, v Revision) (Revision, error) {
	path := f.path(h)
	if !f.store.frozenWorker(path) {
		v.Rest += "|" + racyDigest(path, v)
	}
	return v, nil
}

// Reports whether the file exists
func (f *fileBackend) Exists(_ context.Context, h string) (bool, error) {
	_, err := os.Stat(f.path(h))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// Error while the file cannot be opened and read, without parsing it
func (f *fileBackend) ProbeReadable(_ context.Context, h string) error {
	fh, err := os.Open(f.path(h))
	if err != nil {
		return err
	}
	defer fh.Close()
	if _, err := fh.Read(make([]byte, 1)); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// Cross process lock of the file at path, its directory made first so a deleted profile is never brought back
func (f *fileBackend) lockFile(ctx context.Context, path string) (context.Context, func() error, error) {
	if err := f.store.homes.Mkdir(ctx, filepath.Dir(path)); err != nil {
		return nil, nil, err
	}
	l, err := fileutil.OpenLock(ctx, configLockPath(path), fileutil.LockPerm(0o600))
	if err != nil {
		return nil, nil, err
	}
	return l.Context(ctx), l.Release, nil
}

// Applies the document with comments kept, then each key set, a null set removing it, then each key removed
func (f *fileBackend) WriteChanges(ctx context.Context, h string, c Changes) (err error) {
	path := f.path(h)
	ctx, release, err := f.lockFile(ctx, path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()
	if c.Document != nil {
		if err := f.store.roundTripSave(ctx, path, c.Document, c.ExtraOnCreate); err != nil {
			return err
		}
	}
	for _, kv := range c.Set {
		n, err := nodeFor(kv.Value)
		if err != nil {
			return err
		}
		if isNull(n) {
			n = nil
		}
		if err := f.store.roundTripUpdate(ctx, path, kv.Path, n); err != nil {
			return err
		}
	}
	for _, key := range c.Unset {
		if err := f.store.roundTripUpdate(ctx, path, key, nil); err != nil {
			return err
		}
	}
	return nil
}

// A local file has no levels, so nothing is locked
func (f *fileBackend) Locked(context.Context, string, string) (string, bool) { return "", false }

// Tools that copy, edit or back up the file may run
func (f *fileBackend) SupportsFileTooling() bool { return true }

// The managed overlay applies on top of the file
func (f *fileBackend) HonorsManagedConfig() bool { return true }

// A file needs no fetch before the first read
func (f *fileBackend) Boot(context.Context, string) error { return nil }

// No env name is reserved for the file
func (f *fileBackend) ProtectedEnvNames() []string { return nil }

// Selecting the file needs no deployment variables
func (f *fileBackend) DeploymentEnvNames() []string { return nil }

// The file needs no variables before its first read
func (f *fileBackend) BootstrapEnvNames() []string { return nil }

// Reads never become a diff base for the file, so ctx is kept as it is
func (f *fileBackend) PreserveReadBase(ctx context.Context) context.Context { return ctx }

// The file reports nothing beyond the parse
func (f *fileBackend) DoctorWarnings(context.Context, string) []DoctorWarning { return nil }

// Refuses, since the file backend writes migrations to config.yaml itself
func (f *fileBackend) ApplyInMemory(context.Context, string, *YAMLDocument) error {
	return errors.New("the file backend persists migrations to config.yaml")
}

// The file path
func (f *fileBackend) Describe(_ context.Context, h string) string { return f.path(h) }

// The file path
func (f *fileBackend) Destination(h string) string { return f.path(h) }

// Parsed YAML file with its bytes kept for a write back, read and parse errors returned
func readYAMLFile(path string) (*YAMLDocument, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseYAMLDocument(data)
}

// Age within which a file stamp may still miss a write made in the same filesystem tick
const racyWindowNS = 2_000_000_000

// Digest of the bytes of path while v is racily fresh, empty once settled, an error class when unreadable
func racyDigest(path string, v Revision) string {
	if !racy(v) {
		return ""
	}
	return fileDigest(path)
}

// Digest of the bytes of path, the error class name for a file that stats but cannot be read
func fileDigest(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "<" + errorClass(err) + ">"
	}
	return contentDigest(data)
}

// Class of a read failure as a short name no digest can equal
func errorClass(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "missing"
	case errors.Is(err, fs.ErrPermission):
		return "denied"
	}
	return "unreadable"
}

// Atomically writes rendered bytes over path as they are, keeping mode and owner, a new file at the umask mode
func writeConfigText(ctx context.Context, path string, data []byte) error {
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return fileutil.WriteFile(ctx, path, data, fileutil.KeepOwner(), fileutil.TempPrefix("."+stem+"_"))
}

// Store of the user layer of ctx and the home keying it, for queries the store does not wrap
func (s *Store) Backend(ctx context.Context) (Backend, string, error) {
	h, err := s.homes.Home(ctx)
	if err != nil {
		return nil, "", err
	}
	b, err := s.backend(ctx)
	return b, h, err
}

// Refusal of a tool that copies the local config.yaml when the selected backend keeps no such file
func (s *Store) RequireFileTooling(ctx context.Context, what string) error {
	b, err := s.backend(ctx)
	if err != nil {
		return err
	}
	if b.SupportsFileTooling() {
		return nil
	}
	return &BackendUnavailableError{Message: fmt.Sprintf("%s copies the local config.yaml, and the %s config backend has none. It is not available with this backend.",
		what, quoted(b.Name()))}
}

// Revision of the config of ctx, ok false when it is absent or cannot be stat'ed
func (s *Store) ConfigRevision(ctx context.Context) (Revision, bool) {
	b, h, err := s.Backend(ctx)
	if err != nil {
		return Revision{}, false
	}
	v, err := b.Version(ctx, h)
	return v, err == nil
}

// Revision a cache of the parsed config of ctx keys on, ok false when absent or unreadable
func (s *Store) ConfigCacheRevision(ctx context.Context) (Revision, bool) {
	b, h, err := s.Backend(ctx)
	if err != nil {
		return Revision{}, false
	}
	v, err := s.cacheVersion(ctx, b, h)
	return v, err == nil
}

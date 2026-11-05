package config

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	ConfigFilename = "config.yaml"
	BackendEnv     = "ICEBATH_CONFIG_BACKEND"
)

// One read of a profile user layer
type UserLayer struct {
	Doc        any
	Version    []any
	Locks      map[string]string
	Provenance string
}

// Dotted key and value of one set in a write, nil unsetting it in WriteConfigKeys
type KeyValue struct {
	Key   string
	Value any
}

// Write to one profile user layer, the document applied first
type Changes struct {
	Document             map[string]any
	Set                  []KeyValue
	Unset                []string
	ExtraContentOnCreate string
}

// Message, hint and issue line of one doctor warning a backend raises
type DoctorWarning struct {
	Message string
	Hint    string
	Issue   string
}

// Store of the user config layer every config read and write goes through
type ConfigBackend interface {
	Name() string
	ReadUserDoc(ctx context.Context, home string) (any, error)
	ReadUserDocReadonly(ctx context.Context, home string) (any, error)
	Version(ctx context.Context, home string) ([]any, error)
	CacheVersion(ctx context.Context, home string, version []any) ([]any, error)
	Exists(ctx context.Context, home string) (bool, error)
	ProbeReadable(ctx context.Context, home string) error
	WriteChanges(ctx context.Context, home string, changes Changes) error
	Locked(ctx context.Context, home, dotted string) (string, bool, error)
	SupportsFileTooling() bool
	HonorsManagedConfig() bool
	Boot(ctx context.Context, home string) error
	ProtectedEnvNames() []string
	DeploymentEnvNames() []string
	BootstrapEnvNames() []string
	PreserveReadBase(ctx context.Context) (restore func())
	DoctorWarnings(ctx context.Context, home string) []DoctorWarning
	ApplyInMemory(ctx context.Context, home string, doc map[string]any) error
	Describe(ctx context.Context, home string) string
	Destination(ctx context.Context, home string) (string, error)
}

// Selected backend cannot serve config, an exit and never a fall back to defaults
type ConfigBackendUnavailable struct{ Message string }

func (e *ConfigBackendUnavailable) Error() string          { return e.Message }
func (e *ConfigBackendUnavailable) SystemExitCode() string { return e.Message }

// Config write the backend refused or could not complete with nothing changed
type ConfigWriteError struct {
	Message string
	Code    string
}

func (e *ConfigWriteError) Error() string { return e.Message }

// Builds a write refusal, the code defaulting to config_write_failed
func NewConfigWriteError(message, code string) *ConfigWriteError {
	if code == "" {
		code = "config_write_failed"
	}
	return &ConfigWriteError{Message: message, Code: code}
}

// Write touching a path an upper level locks
type ConfigLockedError struct {
	*ConfigWriteError
	Path     string
	LockedBy string
}

func (e *ConfigLockedError) Unwrap() error { return e.ConfigWriteError }

// Builds a lock refusal with the default message naming the locking level
func NewConfigLockedError(path, lockedBy, message string) *ConfigLockedError {
	if message == "" {
		message = fmt.Sprintf("%s is locked by the %s level", path, lockedBy)
	}
	return &ConfigLockedError{ConfigWriteError: &ConfigWriteError{Message: message, Code: "config_key_locked"}, Path: path, LockedBy: lockedBy}
}

// Value the backend cannot store with nothing sent
type ConfigValueError struct{ *ConfigWriteError }

func (e *ConfigValueError) Unwrap() error { return e.ConfigWriteError }

// Builds a value refusal, the code defaulting to config_value_invalid
func NewConfigValueError(message, code string) *ConfigValueError {
	if code == "" {
		code = "config_value_invalid"
	}
	return &ConfigValueError{&ConfigWriteError{Message: message, Code: code}}
}

// User layer as a local YAML file, a home config.yaml or one explicit path
type FileBackend struct{ explicit string }

// Stat signature of a file as the file backend versions it
func (FileBackend) VersionPath(path string) ([]any, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	sig := fileSignature(fi)
	return []any{sig[0], sig[1], sig[2], sig[3]}, nil
}

// Parsed root of a file, nil when empty, the open error when it cannot be read
func (FileBackend) ReadPath(path string) (any, error) {
	data, err := readYAMLFile(path)
	if err != nil {
		return nil, err
	}
	return FastSafeLoad(data)
}

// Shared cached parse of a file, never to be mutated
func (FileBackend) ReadPathReadonly(path string) (any, error) { return LoadYAMLFileReadonly(path) }

// Applies the document, then each set key, then each unset key with comment preserving writers
func (FileBackend) WritePath(ctx context.Context, path string, changes Changes) error {
	if changes.Document != nil {
		if err := atomicRoundtripYAMLSave(ctx, path, changes.Document, changes.ExtraContentOnCreate); err != nil {
			return err
		}
	}
	for _, kv := range changes.Set {
		if err := atomicRoundtripYAMLUpdate(path, kv.Key, kv.Value); err != nil {
			return err
		}
	}
	for _, key := range changes.Unset {
		if err := atomicRoundtripYAMLUpdate(path, key, nil); err != nil {
			return err
		}
	}
	return nil
}

func (b FileBackend) Name() string { return "file" }

// File a home user layer lives in, or the explicit file
func (b FileBackend) ConfigPath(home string) string {
	if b.explicit != "" {
		return b.explicit
	}
	return filepath.Join(home, ConfigFilename)
}

func (b FileBackend) ReadUserDoc(_ context.Context, home string) (any, error) {
	return b.ReadPath(b.ConfigPath(home))
}

func (b FileBackend) ReadUserDocReadonly(_ context.Context, home string) (any, error) {
	return b.ReadPathReadonly(b.ConfigPath(home))
}

func (b FileBackend) Version(_ context.Context, home string) ([]any, error) {
	return b.VersionPath(b.ConfigPath(home))
}

// Version plus the racy content digest of the file taken from its own signature
func (b FileBackend) CacheVersion(ctx context.Context, home string, version []any) ([]any, error) {
	path := b.ConfigPath(home)
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	out := append(append([]any{}, version...), RacyContentDigest(path, fileSignature(fi)))
	return out, nil
}

func (b FileBackend) Exists(_ context.Context, home string) (bool, error) {
	_, err := os.Stat(b.ConfigPath(home))
	return err == nil, nil
}

// Reads one byte so an unreadable file reports its error without a parse
func (b FileBackend) ProbeReadable(_ context.Context, home string) error {
	f, err := os.Open(b.ConfigPath(home))
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Read(make([]byte, 1))
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func (b FileBackend) WriteChanges(ctx context.Context, home string, changes Changes) error {
	return b.WritePath(ctx, b.ConfigPath(home), changes)
}

// A local file has no levels so nothing is ever locked
func (FileBackend) Locked(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}
func (FileBackend) SupportsFileTooling() bool                              { return true }
func (FileBackend) HonorsManagedConfig() bool                              { return true }
func (FileBackend) Boot(context.Context, string) error                     { return nil }
func (FileBackend) ProtectedEnvNames() []string                            { return nil }
func (FileBackend) DeploymentEnvNames() []string                           { return nil }
func (FileBackend) BootstrapEnvNames() []string                            { return nil }
func (FileBackend) PreserveReadBase(context.Context) func()                { return func() {} }
func (FileBackend) DoctorWarnings(context.Context, string) []DoctorWarning { return nil }

// The file backend persists migrations to the file and refuses an in memory apply
func (FileBackend) ApplyInMemory(context.Context, string, map[string]any) error {
	return errors.New("the file backend persists migrations to config.yaml")
}

func (b FileBackend) Describe(_ context.Context, home string) string { return b.ConfigPath(home) }
func (b FileBackend) Destination(_ context.Context, home string) (string, error) {
	return b.ConfigPath(home), nil
}

var (
	backendKindRe     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	backendFactoryMu  sync.RWMutex
	backendFactories  = map[string]func() ConfigBackend{}
	bootstrapMu       sync.Mutex
	bootstrapped      atomic.Bool
	bootstrapEnvMu    sync.RWMutex
	bootstrapEnvApply func(ctx context.Context, backendNames func(kind string) []string, home, projectEnv string) error
)

type bootstrapOwnerKey struct{}

// Serves backend kind from factory, panicking on an invalid kind or a second registration
func RegisterConfigBackend(kind string, factory func() ConfigBackend) {
	if kind == "file" || !backendKindRe.MatchString(kind) || factory == nil {
		panic(fmt.Sprintf("invalid config backend registration %q", kind))
	}
	backendFactoryMu.Lock()
	defer backendFactoryMu.Unlock()
	if _, dup := backendFactories[kind]; dup {
		panic(fmt.Sprintf("config backend %q registered twice", kind))
	}
	backendFactories[kind] = factory
}

// Installs the env loader publication of the deployment env the bootstrap runs before the first selection
func RegisterConfigBootstrapEnv(apply func(ctx context.Context, backendNames func(kind string) []string, home, projectEnv string) error) {
	bootstrapEnvMu.Lock()
	bootstrapEnvApply = apply
	bootstrapEnvMu.Unlock()
}

// Backend of a kind, the file backend for file and the registered factory otherwise
func backendFor(kind string) (ConfigBackend, error) {
	if kind == "file" {
		return FileBackend{}, nil
	}
	unknown := &ConfigBackendUnavailable{Message: fmt.Sprintf(
		"%s=%s is not a config backend this build provides (expected 'file' or 'remote').", BackendEnv, PyRepr(kind))}
	if !backendKindRe.MatchString(kind) {
		return nil, unknown
	}
	backendFactoryMu.RLock()
	factory := backendFactories[kind]
	backendFactoryMu.RUnlock()
	if factory == nil {
		return nil, unknown
	}
	return factory(), nil
}

// Env names backend kind needs before the first read, none for a kind this build lacks
func bootstrapNames(kind string) []string {
	backend, err := backendFor(kind)
	if err != nil {
		return nil
	}
	return backend.BootstrapEnvNames()
}

// Publishes the deployment once per process before the first selection
func ensureBootstrapEnv(ctx context.Context) error {
	if bootstrapped.Load() {
		return nil
	}
	return runBootstrap(ctx, "", "", true)
}

// Marks the process as not yet bootstrapped so the next backend selection publishes the deployment again
func ResetBootstrapForTests() { bootstrapped.Store(false) }

// Publishes the deployment from an explicit dotenv load home and project file before its first config read
func BootstrapDeployment(ctx context.Context, home, projectEnv string) error {
	return runBootstrap(ctx, home, projectEnv, false)
}

// Runs the env loader publication once at a time, skipping a reentry from the publishing call itself
func runBootstrap(ctx context.Context, home, projectEnv string, once bool) error {
	if ctx.Value(bootstrapOwnerKey{}) != nil {
		return nil
	}
	bootstrapMu.Lock()
	defer bootstrapMu.Unlock()
	if once && bootstrapped.Load() {
		return nil
	}
	defer bootstrapped.Store(true)
	bootstrapEnvMu.RLock()
	apply := bootstrapEnvApply
	bootstrapEnvMu.RUnlock()
	if apply == nil {
		return nil
	}
	return apply(context.WithValue(ctx, bootstrapOwnerKey{}, true), bootstrapNames, home, projectEnv)
}

// Backend the environment selects, file by default, unavailable for a kind this build lacks
func GetConfigBackend(ctx context.Context) (ConfigBackend, error) {
	if err := ensureBootstrapEnv(ctx); err != nil {
		return nil, err
	}
	kind := strings.ToLower(strings.TrimSpace(os.Getenv(BackendEnv)))
	if kind == "" {
		kind = "file"
	}
	return backendFor(kind)
}

// Backend and home serving a config path, a home config.yaml through the selected backend
func route(ctx context.Context, configPath string) (ConfigBackend, string, error) {
	if filepath.Base(configPath) == ConfigFilename {
		backend, err := GetConfigBackend(ctx)
		return backend, filepath.Dir(configPath), err
	}
	return FileBackend{explicit: configPath}, filepath.Dir(configPath), nil
}

// Local file backend of a path whichever backend is selected, for the PM dependency runtime
func LocalFileLayer(configPath string) (ConfigBackend, string) {
	return FileBackend{explicit: configPath}, filepath.Dir(configPath)
}

// Parsed root of a config file, nil when empty
func ReadConfigDoc(ctx context.Context, configPath string) (any, error) {
	backend, h, err := route(ctx, configPath)
	if err != nil {
		return nil, err
	}
	return backend.ReadUserDoc(ctx, h)
}

// Signature cached parse of a config file, shared and never to be mutated
func ReadConfigDocReadonly(ctx context.Context, configPath string) (any, error) {
	backend, h, err := route(ctx, configPath)
	if err != nil {
		return nil, err
	}
	return backend.ReadUserDocReadonly(ctx, h)
}

// Cache signature of a config file, the stat error when it cannot be stat'ed
func ConfigVersion(ctx context.Context, configPath string) ([]any, error) {
	backend, h, err := route(ctx, configPath)
	if err != nil {
		return nil, err
	}
	return backend.Version(ctx, h)
}

// Signature for a parse cache that also changes on a same tick same size rewrite
func ConfigCacheVersion(ctx context.Context, configPath string) ([]any, error) {
	version, err := ConfigVersion(ctx, configPath)
	if err != nil {
		return nil, err
	}
	backend, h, err := route(ctx, configPath)
	if err != nil {
		return nil, err
	}
	return backend.CacheVersion(ctx, h, version)
}

// Cache version, nil when the config is absent or cannot be read, other errors returned
func ConfigCacheVersionOrNone(ctx context.Context, configPath string) ([]any, error) {
	version, err := ConfigCacheVersion(ctx, configPath)
	if err != nil && IsOSError(err) {
		return nil, nil
	}
	return version, err
}

// Version, nil when the config is absent or cannot be stat'ed, other errors returned
func ConfigVersionOrNone(ctx context.Context, configPath string) ([]any, error) {
	version, err := ConfigVersion(ctx, configPath)
	if err != nil && IsOSError(err) {
		return nil, nil
	}
	return version, err
}

// Number that changes with a backend version, zero for no config
func VersionToken(version []any) float64 {
	if len(version) == 0 {
		return 0
	}
	switch n := version[0].(type) {
	case bool:
		if n {
			return 1.0 / 1_000_000_000
		}
		return 0
	case int:
		return float64(n) / 1_000_000_000
	case int64:
		return float64(n) / 1_000_000_000
	case uint64:
		return float64(n) / 1_000_000_000
	}
	items := make([]string, len(version))
	for i, v := range version {
		items[i] = PyRepr(v)
	}
	repr := "(" + strings.Join(items, ", ")
	if len(items) == 1 {
		repr += ","
	}
	sum := crc32.ChecksumIEEE([]byte(repr + ")"))
	if sum == 0 {
		return 1
	}
	return float64(sum)
}

// Error while a config file cannot be read, with no parse
func ProbeConfigReadable(ctx context.Context, configPath string) error {
	backend, h, err := route(ctx, configPath)
	if err != nil {
		return err
	}
	return backend.ProbeReadable(ctx, h)
}

// Reports whether a config file exists
func ConfigExists(ctx context.Context, configPath string) (bool, error) {
	backend, h, err := route(ctx, configPath)
	if err != nil {
		return false, err
	}
	return backend.Exists(ctx, h)
}

// Replaces a config document, merged comment preserving onto the file by the file backend
func WriteConfigDocument(ctx context.Context, configPath string, document map[string]any, extraContentOnCreate string) error {
	backend, h, err := route(ctx, configPath)
	if err != nil {
		return err
	}
	return backend.WriteChanges(ctx, h, Changes{Document: document, ExtraContentOnCreate: extraContentOnCreate})
}

// Where a write to a config path lands, the file path or the backend own name
func ConfigDestination(ctx context.Context, configPath string) (string, error) {
	backend, h, err := route(ctx, configPath)
	if err != nil {
		return "", err
	}
	return backend.Destination(ctx, h)
}

// Sets one dotted key, nil removing it
func WriteConfigKey(ctx context.Context, configPath, keyPath string, value any) error {
	return WriteConfigKeys(ctx, configPath, []KeyValue{{Key: keyPath, Value: value}})
}

// Sets several related dotted keys as one write, a nil value removing its key
func WriteConfigKeys(ctx context.Context, configPath string, updates []KeyValue) error {
	backend, h, err := route(ctx, configPath)
	if err != nil {
		return err
	}
	var changes Changes
	for _, kv := range updates {
		if kv.Value == nil {
			changes.Unset = append(changes.Unset, kv.Key)
		} else {
			changes.Set = append(changes.Set, kv)
		}
	}
	return backend.WriteChanges(ctx, h, changes)
}

// Runs a pre write guard read without making it the base a later document save diffs against
func KeepingReadBase[T any](ctx context.Context, read func() (T, error)) (T, error) {
	backend, err := GetConfigBackend(ctx)
	if err != nil {
		var zero T
		return zero, err
	}
	restore := backend.PreserveReadBase(ctx)
	defer restore()
	return read()
}

type refusedLockedOmissionsKey struct{}
type pinnedKeysKey struct{}

// Context in which a document save refuses a locked changed key under prefixes instead of omitting it
func RefusingLockedOmissions(ctx context.Context, prefixes ...string) context.Context {
	held := RefusedLockedOmissionPrefixes(ctx)
	return context.WithValue(ctx, refusedLockedOmissionsKey{}, append(append([]string{}, held...), prefixes...))
}

// Dotted prefixes a document save in ctx must write whole
func RefusedLockedOmissionPrefixes(ctx context.Context) []string {
	held, _ := ctx.Value(refusedLockedOmissionsKey{}).([]string)
	return held
}

// Context in which a document save stores keys at the user layer even when unchanged from the inherited value
func PinningKeys(ctx context.Context, keys ...string) context.Context {
	held := PinnedKeys(ctx)
	return context.WithValue(ctx, pinnedKeysKey{}, append(append([]string{}, held...), keys...))
}

// Dotted keys a document save in ctx stores even when unchanged
func PinnedKeys(ctx context.Context) []string {
	held, _ := ctx.Value(pinnedKeysKey{}).([]string)
	return held
}

// One changes write where a nil set value stores an explicit null for a backend with levels
func WriteChanges(ctx context.Context, configPath string, changes Changes) error {
	backend, h, err := route(ctx, configPath)
	if err != nil {
		return err
	}
	return backend.WriteChanges(ctx, h, changes)
}

// Deployment env names of the selected backend, never launch residue
func DeploymentEnvNames(ctx context.Context) ([]string, error) {
	backend, err := GetConfigBackend(ctx)
	if err != nil {
		return nil, err
	}
	return backend.DeploymentEnvNames(), nil
}

// Reports whether tools that copy, edit or back up the config file itself may run
func SupportsFileTooling(ctx context.Context) (bool, error) {
	backend, err := GetConfigBackend(ctx)
	if err != nil {
		return false, err
	}
	return backend.SupportsFileTooling(), nil
}

// Refuses a file copy tool when the selected backend has no config file
func RequireFileTooling(ctx context.Context, what string) error {
	backend, err := GetConfigBackend(ctx)
	if err != nil {
		return err
	}
	if backend.SupportsFileTooling() {
		return nil
	}
	return &ConfigBackendUnavailable{Message: fmt.Sprintf(
		"%s copies the local config.yaml, and the %s config backend has none. It is not available with this backend.",
		what, PyRepr(backend.Name()))}
}

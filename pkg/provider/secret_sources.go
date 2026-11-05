package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// SecretSourceAPIVersion gates plugin secret source compatibility
const SecretSourceAPIVersion = 1

const (
	DefaultSecretFetchTimeout = 120 * time.Second
	DefaultSecretCLITimeout   = 30 * time.Second
)

// SourceEnvironment is the per fetch environment view a source reads bootstrap credentials from
type SourceEnvironment struct {
	vars    map[string]string
	process bool
}

// ProcessSourceEnvironment reads and writes the process environment
func ProcessSourceEnvironment() *SourceEnvironment { return &SourceEnvironment{process: true} }

// MappingSourceEnvironment reads and writes a private mapping, never the process environment
func MappingSourceEnvironment(vars map[string]string) *SourceEnvironment {
	if vars == nil {
		vars = map[string]string{}
	}
	return &SourceEnvironment{vars: vars}
}

// IsProcess reports whether the view is the process environment itself
func (e *SourceEnvironment) IsProcess() bool { return e == nil || e.process }

// Get reads one variable from the view
func (e *SourceEnvironment) Get(name string) (string, bool) {
	if e.IsProcess() {
		return os.LookupEnv(name)
	}
	v, ok := e.vars[name]
	return v, ok
}

// Value reads one variable or an empty string
func (e *SourceEnvironment) Value(name string) string {
	v, _ := e.Get(name)
	return v
}

// Set writes one variable into the view
func (e *SourceEnvironment) Set(name, value string) {
	if e.IsProcess() {
		_ = os.Setenv(name, value)
		return
	}
	e.vars[name] = value
}

// Unset removes one variable from the view
func (e *SourceEnvironment) Unset(name string) {
	if e.IsProcess() {
		_ = os.Unsetenv(name)
		return
	}
	delete(e.vars, name)
}

// Snapshot copies the view into a plain map
func (e *SourceEnvironment) Snapshot() map[string]string {
	out := map[string]string{}
	if e.IsProcess() {
		for _, kv := range os.Environ() {
			if k, v, ok := strings.Cut(kv, "="); ok {
				out[k] = v
			}
		}
		return out
	}
	for k, v := range e.vars {
		out[k] = v
	}
	return out
}

// Mapping is the private mapping behind a non process view
func (e *SourceEnvironment) Mapping() map[string]string {
	if e.IsProcess() {
		return nil
	}
	return e.vars
}

// SourceChildEnv is the environment for a helper child, the scrubbed process env or only the per fetch view
func SourceChildEnv(environ *SourceEnvironment) map[string]string {
	if environ.IsProcess() {
		child, err := tools.BuildSubprocessEnv(context.Background(), nil, tools.SubprocessEnvOptions{})
		if err != nil {
			panic(err)
		}
		return child
	}
	return environ.Snapshot()
}

// SecretErrorKind is the failure taxonomy of a fetch
type SecretErrorKind string

const (
	SecretErrorNotConfigured SecretErrorKind = "not_configured"
	SecretErrorBinaryMissing SecretErrorKind = "binary_missing"
	SecretErrorAuthFailed    SecretErrorKind = "auth_failed"
	SecretErrorAuthExpired   SecretErrorKind = "auth_expired"
	SecretErrorRefInvalid    SecretErrorKind = "ref_invalid"
	SecretErrorNetwork       SecretErrorKind = "network"
	SecretErrorEmptyValue    SecretErrorKind = "empty_value"
	SecretErrorTimeout       SecretErrorKind = "timeout"
	SecretErrorInternal      SecretErrorKind = "internal"
)

// SecretErrorRule maps substrings of CLI failure text onto a kind, first match wins
type SecretErrorRule struct {
	Kind   SecretErrorKind
	Tokens []string
}

// ClassifySecretCLIError maps helper CLI failure text onto the taxonomy
func ClassifySecretCLIError(message string, rules []SecretErrorRule) SecretErrorKind {
	lowered := strings.ToLower(message)
	for _, rule := range rules {
		for _, tok := range rule.Tokens {
			if strings.Contains(lowered, tok) {
				return rule.Kind
			}
		}
	}
	return SecretErrorInternal
}

// CoerceFloat reads a config number with a default for malformed values
func CoerceFloat(value any, def float64) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	case bool:
		if v {
			return 1
		}
		return 0
	}
	return def
}

// SecretFetchResult is the outcome of one source's fetch
type SecretFetchResult struct {
	Secrets    map[string]string
	Applied    []string
	Skipped    []string
	Warnings   []string
	Error      string
	ErrorKind  SecretErrorKind
	BinaryPath string
	failed     bool
}

// NewSecretFetchResult is an empty successful result
func NewSecretFetchResult() *SecretFetchResult {
	return &SecretFetchResult{Secrets: map[string]string{}}
}

// OK reports no error
func (r *SecretFetchResult) OK() bool { return !r.failed }

// Fail records the error and kind and returns the result
func (r *SecretFetchResult) Fail(err string, kind SecretErrorKind) *SecretFetchResult {
	r.Error, r.ErrorKind, r.failed = err, kind, true
	return r
}

var genericSecretRemediation = map[SecretErrorKind]string{
	SecretErrorNotConfigured: "Run `icebath secrets {name} setup` to finish configuration.",
	SecretErrorBinaryMissing: "Run `icebath secrets {name} setup` to install the helper CLI.",
	SecretErrorAuthFailed:    "Credentials rejected — run `icebath secrets {name} setup` to re-authenticate.",
	SecretErrorAuthExpired:   "Credentials expired — run `icebath secrets {name} setup` to re-authenticate.",
	SecretErrorNetwork:       "Network problem reaching the secrets backend — check connectivity and retry.",
	SecretErrorTimeout:       "Backend was slow — raise secrets.{name}.timeout_seconds if this recurs.",
}

// SecretSourceSpec is the static description of one secret backend
type SecretSourceSpec struct {
	APIVersion              int
	Name                    string
	Label                   string
	Shape                   string
	Scheme                  string
	TokenEnvKey             string
	DefaultTokenEnv         string
	OverrideExistingDefault bool
	RemediationHints        map[SecretErrorKind]string
}

// SecretSource is one external secret backend
type SecretSource interface {
	Spec() SecretSourceSpec
	// Resolves the source's secrets, never panics, cfg is the raw secrets.<name> section
	Fetch(cfg map[string]any, homePath string, environ *SourceEnvironment) *SecretFetchResult
	ConfigSchema() map[string]map[string]any
}

// SecretSourceEnabled reads the enabled flag of a section
func SecretSourceEnabled(cfg map[string]any) bool { return truthyAny(cfg["enabled"]) }

// SecretSourceOverrideExisting reports whether the source may overwrite .env and shell values
func SecretSourceOverrideExisting(spec SecretSourceSpec, cfg map[string]any) bool {
	if v, ok := cfg["override_existing"]; ok {
		return truthyAny(v)
	}
	return spec.OverrideExistingDefault
}

// SecretSourceTokenEnv is the env var holding the source's bootstrap credential
func SecretSourceTokenEnv(spec SecretSourceSpec, cfg map[string]any) string {
	if spec.TokenEnvKey != "" {
		if v := anyString(cfg[spec.TokenEnvKey]); v != "" {
			return v
		}
	}
	return spec.DefaultTokenEnv
}

// SecretSourceProtectedEnvVars are the names no source may overwrite
func SecretSourceProtectedEnvVars(spec SecretSourceSpec, cfg map[string]any) map[string]bool {
	if spec.TokenEnvKey == "" {
		return map[string]bool{}
	}
	return map[string]bool{SecretSourceTokenEnv(spec, cfg): true}
}

// SecretSourceFetchTimeout is the wall clock budget enforced around a fetch
func SecretSourceFetchTimeout(cfg map[string]any) time.Duration {
	val := CoerceFloat(cfg["timeout_seconds"], DefaultSecretFetchTimeout.Seconds())
	if _, ok := cfg["timeout_seconds"]; !ok {
		val = DefaultSecretFetchTimeout.Seconds()
	}
	if val <= 0 {
		return DefaultSecretFetchTimeout
	}
	return time.Duration(val * float64(time.Second))
}

// SecretSourceRemediation is the one line fix for a failed fetch, empty suppresses the hint
func SecretSourceRemediation(spec SecretSourceSpec, kind SecretErrorKind, cfg map[string]any) string {
	if kind == "" {
		return ""
	}
	template := spec.RemediationHints[kind]
	if template == "" {
		template = genericSecretRemediation[kind]
	}
	return strings.NewReplacer("{name}", spec.Name, "{token_env}", SecretSourceTokenEnv(spec, cfg)).Replace(template)
}

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// IsValidEnvName is true for a legal environment variable name
func IsValidEnvName(name string) bool { return name != "" && envNameRe.MatchString(name) }

// Whole CSI and OSC sequences including unterminated OSC from a CLI killed mid write
var secretANSIRe = regexp.MustCompile("\x1b(?:\\[[0-9;?]*[ -/]*[@-~]|\\][^\x07\x1b]*(?:\x07|\x1b\\\\)?)")

// ScrubANSI strips ANSI escape sequences from helper output
func ScrubANSI(text string) string { return secretANSIRe.ReplaceAllString(text, "") }

// CLIResult is a completed helper process
type CLIResult struct {
	Stdout     string
	Stderr     string
	ReturnCode int
}

// RunCLI runs an argv list with a captured utf-8 result, timeout and spawn failure become errors
func RunCLI(argv []string, env map[string]string, timeout time.Duration, label, timeoutMessage string, stdin *os.File) (CLIResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = envList(env)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return CLIResult{}, errors.New(timeoutMessage)
	}
	res := CLIResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ReturnCode = exitErr.ExitCode()
			return res, nil
		}
		return CLIResult{}, fmt.Errorf("failed to invoke %s: %v", label, err)
	}
	return res, nil
}

func envList(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// RunSecretCLI runs a secret manager helper with a minimal allowlisted env and ANSI scrubbed stderr
func RunSecretCLI(argv []string, allowEnv []string, extraEnv map[string]string, timeout time.Duration) (CLIResult, error) {
	keep := append([]string{"PATH", "HOME", "USERPROFILE", "SYSTEMROOT", "TMPDIR", "TEMP", "LANG", "LC_ALL", "XDG_CONFIG_HOME", "XDG_DATA_HOME"}, allowEnv...)
	env := map[string]string{}
	for _, k := range keep {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	for k, v := range extraEnv {
		env[k] = v
	}
	if _, ok := env["NO_COLOR"]; !ok {
		env["NO_COLOR"] = "1"
	}
	name := filepath.Base(argv[0])
	res, err := RunCLI(argv, env, timeout, name, fmt.Sprintf("%s timed out after %.0fs", name, timeout.Seconds()), nil)
	if err != nil {
		return res, err
	}
	res.Stderr = ScrubANSI(res.Stderr)
	return res, nil
}

// ---- cache substrate ----

// SecretFingerprint is a SHA-256 prefix used as a cache key, never logged or displayed
func SecretFingerprint(material string) string {
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])[:16]
}

// CachedFetch is a set of fetched secret values plus when they were fetched
type CachedFetch struct {
	Secrets   map[string]string
	FetchedAt float64
}

// IsFresh reports the entry is younger than the TTL
func (c CachedFetch) IsFresh(ttl float64) bool {
	return ttl > 0 && nowEpoch()-c.FetchedAt < ttl
}

// ResolveCacheHome is the given home else the process home
func ResolveCacheHome(homePath string) string {
	if homePath == "" {
		return home.Process()
	}
	return homePath
}

// CurrentCachePath is the home level cache file, deleting a legacy copy under cache first
func CurrentCachePath(h, name string) string {
	_ = os.Remove(filepath.Join(h, "cache", name))
	return filepath.Join(h, name)
}

// RemoveCacheFile deletes the cache file and any legacy copy
func RemoveCacheFile(h, name string) {
	_ = os.Remove(filepath.Join(h, name))
	_ = os.Remove(filepath.Join(h, "cache", name))
}

// CachedFetchFromPayload decodes a secrets and fetched_at object, only string pairs survive
func CachedFetchFromPayload(payload any) (CachedFetch, bool) {
	m, ok := payload.(map[string]any)
	if !ok {
		return CachedFetch{}, false
	}
	secrets, ok := m["secrets"].(map[string]any)
	if !ok {
		return CachedFetch{}, false
	}
	fetchedAt, ok := anyFloat(m["fetched_at"])
	if !ok {
		return CachedFetch{}, false
	}
	typed := map[string]string{}
	for k, v := range secrets {
		if s, isStr := v.(string); isStr {
			typed[k] = s
		}
	}
	return CachedFetch{Secrets: typed, FetchedAt: fetchedAt}, true
}

// AtomicWriteCacheJSON writes a secret cache entry 0600 with the containing dir tightened to 0700
func AtomicWriteCacheJSON(path string, payload any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	secureParentDir(path)
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return config.AtomicWrite(path, raw, 0o600)
}

// SecretDiskCache is the profile aware on disk cache of one backend
type SecretDiskCache struct {
	basename string
}

// NewSecretDiskCache binds a disk cache to a basename under the home
func NewSecretDiskCache(basename string) *SecretDiskCache {
	return &SecretDiskCache{basename: basename}
}

// Path is the cache file for a home
func (c *SecretDiskCache) Path(homePath string) string {
	return filepath.Join(ResolveCacheHome(homePath), c.basename)
}

func (c *SecretDiskCache) currentPath(homePath string) string {
	return CurrentCachePath(ResolveCacheHome(homePath), c.basename)
}

// Read is the fresh cached entry for key, false on mismatch or staleness, an error only for undecodable bytes
func (c *SecretDiskCache) Read(key string, ttl float64, homePath string) (CachedFetch, bool, error) {
	if ttl <= 0 {
		return CachedFetch{}, false, nil
	}
	raw, err := os.ReadFile(c.currentPath(homePath))
	if err != nil {
		return CachedFetch{}, false, nil
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	for i := 0; i < len(raw); {
		r, size := utf8.DecodeRune(raw[i:])
		if r == utf8.RuneError && size <= 1 {
			return CachedFetch{}, false, pystr.UTF8DecodeError(raw, i)
		}
		i += size
	}
	parsed, err := pyjson.Loads(string(raw))
	if err != nil {
		return CachedFetch{}, false, nil
	}
	obj, isDict := parsed.(*pyjson.Object)
	if !isDict {
		return CachedFetch{}, false, nil
	}
	if stored, isStr := obj.Vals["key"].(string); !isStr || stored != key {
		return CachedFetch{}, false, nil
	}
	payload := map[string]any{"fetched_at": obj.Vals["fetched_at"]}
	if secrets, isObj := obj.Vals["secrets"].(*pyjson.Object); isObj {
		payload["secrets"] = secrets.Vals
	}
	entry, ok := CachedFetchFromPayload(payload)
	if !ok || !entry.IsFresh(ttl) {
		return CachedFetch{}, false, nil
	}
	return entry, true, nil
}

// Write persists the entry atomically, a no-op when the TTL is off or on an I/O error
func (c *SecretDiskCache) Write(key string, entry CachedFetch, ttl float64, homePath string) {
	if ttl <= 0 {
		return
	}
	_ = AtomicWriteCacheJSON(c.currentPath(homePath), map[string]any{"key": key, "secrets": entry.Secrets, "fetched_at": entry.FetchedAt})
}

// Clear deletes the cache file if present
func (c *SecretDiskCache) Clear(homePath string) {
	RemoveCacheFile(ResolveCacheHome(homePath), c.basename)
}

// SecretCache is an in process layer over a disk cache
type SecretCache struct {
	mu     sync.Mutex
	memory map[string]CachedFetch
	Disk   *SecretDiskCache
}

// NewSecretCache builds the two layer cache for a basename
func NewSecretCache(basename string) *SecretCache {
	return &SecretCache{memory: map[string]CachedFetch{}, Disk: NewSecretDiskCache(basename)}
}

// Lookup is the fresh entry from memory, else from disk promoted into memory, erring on an undecodable file
func (c *SecretCache) Lookup(key string, ttl float64, homePath string, readDisk func() (CachedFetch, bool)) (CachedFetch, bool, error) {
	c.mu.Lock()
	cached, ok := c.memory[key]
	c.mu.Unlock()
	if ok && cached.IsFresh(ttl) {
		return cached, true, nil
	}
	var entry CachedFetch
	var found bool
	if readDisk != nil {
		entry, found = readDisk()
	} else {
		var err error
		if entry, found, err = c.Disk.Read(key, ttl, homePath); err != nil {
			return CachedFetch{}, false, err
		}
	}
	if found {
		c.mu.Lock()
		c.memory[key] = entry
		c.mu.Unlock()
	}
	return entry, found, nil
}

// Store writes the entry to both layers
func (c *SecretCache) Store(key string, entry CachedFetch, ttl float64, homePath string) {
	c.mu.Lock()
	c.memory[key] = entry
	c.mu.Unlock()
	c.Disk.Write(key, entry, ttl, homePath)
}

// StoreMemory writes the entry to the in process layer only
func (c *SecretCache) StoreMemory(key string, entry CachedFetch) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.memory[key] = entry
}

// Clear drops both layers
func (c *SecretCache) Clear(homePath string) {
	c.mu.Lock()
	c.memory = map[string]CachedFetch{}
	c.mu.Unlock()
	c.Disk.Clear(homePath)
}

// ---- registry ----

var (
	secretRegistryMu    sync.Mutex
	secretSources       = map[string]SecretSource{}
	secretSourceOrder   []string
	secretSourceOrigins = map[string]string{}
	scopedSecretSources = map[string]map[string]SecretSource{}
	scopedSecretOrder   = map[string][]string{}
	builtinSourcesReady bool
)

// Reason a source is unregistrable, empty when it is fine
func validateSecretSource(source SecretSource) string {
	if source == nil {
		return "Ignoring secret source <nil>: does not implement SecretSource"
	}
	spec := source.Spec()
	name := spec.Name
	if name == "" || strings.ToLower(name) != name || !isAlnumUnderscore(name) {
		return fmt.Sprintf("Ignoring secret source with invalid name %q", name)
	}
	if spec.APIVersion != SecretSourceAPIVersion {
		return fmt.Sprintf("Ignoring secret source '%s': built against secret-source API v%d, this Icebath speaks v%d", name, spec.APIVersion, SecretSourceAPIVersion)
	}
	if spec.Shape != "mapped" && spec.Shape != "bulk" {
		return fmt.Sprintf("Ignoring secret source '%s': shape must be 'mapped' or 'bulk', got %q", name, spec.Shape)
	}
	return ""
}

func isAlnumUnderscore(s string) bool {
	if strings.ReplaceAll(s, "_", "") == "" {
		return false
	}
	for _, r := range s {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func normalizeScope(scope string) string {
	if strings.TrimSpace(scope) == "" {
		return ""
	}
	return home.Key(scope)
}

// RegisterSecretSource registers a source, true on success, rejections are logged never raised
func RegisterSecretSource(source SecretSource, replace, builtin bool, scope string) bool {
	if problem := validateSecretSource(source); problem != "" {
		slog.Default().Warn(problem)
		return false
	}
	scope = normalizeScope(scope)
	spec := source.Spec()
	secretRegistryMu.Lock()
	defer secretRegistryMu.Unlock()
	effective := map[string]SecretSource{}
	for k, v := range secretSources {
		effective[k] = v
	}
	if scope != "" {
		for k, v := range scopedSecretSources[scope] {
			effective[k] = v
		}
	}
	if _, exists := effective[spec.Name]; exists && !replace {
		slog.Default().Warn(fmt.Sprintf("Secret source '%s' already registered; ignoring duplicate", spec.Name))
		return false
	}
	for n, other := range effective {
		if n != spec.Name && spec.Scheme != "" && other.Spec().Scheme == spec.Scheme {
			slog.Default().Warn(fmt.Sprintf("Ignoring secret source '%s': scheme '%s://' is already owned by source '%s'", spec.Name, spec.Scheme, n))
			return false
		}
	}
	if scope == "" {
		if _, exists := secretSources[spec.Name]; !exists {
			secretSourceOrder = append(secretSourceOrder, spec.Name)
		}
		secretSources[spec.Name] = source
		if builtin {
			secretSourceOrigins[spec.Name] = "builtin"
		} else {
			secretSourceOrigins[spec.Name] = "plugin"
		}
		return true
	}
	target := scopedSecretSources[scope]
	if target == nil {
		target = map[string]SecretSource{}
		scopedSecretSources[scope] = target
	}
	if _, exists := target[spec.Name]; !exists {
		scopedSecretOrder[scope] = append(scopedSecretOrder[scope], spec.Name)
	}
	target[spec.Name] = source
	return true
}

// Global sources overlaid with the scope's registrations in order, lock held
func mergedSecretSources(scope string) ([]string, map[string]SecretSource) {
	key := scope
	if key == "" {
		key = home.Key(home.Process())
	} else {
		key = normalizeScope(scope)
	}
	merged := map[string]SecretSource{}
	var order []string
	for _, n := range secretSourceOrder {
		merged[n] = secretSources[n]
		order = append(order, n)
	}
	for _, n := range scopedSecretOrder[key] {
		if _, exists := merged[n]; !exists {
			order = append(order, n)
		}
		merged[n] = scopedSecretSources[key][n]
	}
	return order, merged
}

// GetSecretSource is the registered source by name for the scope
func GetSecretSource(name, scope string) SecretSource {
	EnsureBuiltinSecretSources()
	secretRegistryMu.Lock()
	defer secretRegistryMu.Unlock()
	_, merged := mergedSecretSources(scope)
	return merged[name]
}

// SnapshotSecretRegistration is the registration owned by exactly one registry layer
func SnapshotSecretRegistration(name, scope string) SecretSource {
	EnsureBuiltinSecretSources()
	scope = normalizeScope(scope)
	secretRegistryMu.Lock()
	defer secretRegistryMu.Unlock()
	if scope == "" {
		return secretSources[name]
	}
	return scopedSecretSources[scope][name]
}

// RestoreSecretRegistration restores a host owned registration when it is still current
func RestoreSecretRegistration(name string, current, previous SecretSource, scope string) bool {
	EnsureBuiltinSecretSources()
	scope = normalizeScope(scope)
	secretRegistryMu.Lock()
	defer secretRegistryMu.Unlock()
	target := secretSources
	order := &secretSourceOrder
	if scope != "" {
		target = scopedSecretSources[scope]
		if target == nil {
			target = map[string]SecretSource{}
			scopedSecretSources[scope] = target
		}
		o := scopedSecretOrder[scope]
		order = &o
		defer func() { scopedSecretOrder[scope] = o }()
	}
	if target[name] != current {
		return false
	}
	if previous == nil {
		delete(target, name)
		kept := (*order)[:0:0]
		for _, n := range *order {
			if n != name {
				kept = append(kept, n)
			}
		}
		*order = kept
	} else {
		target[name] = previous
	}
	if scope != "" && len(target) == 0 {
		delete(scopedSecretSources, scope)
		delete(scopedSecretOrder, scope)
	}
	return true
}

// ListSecretSources lists the sources visible to the scope in apply order
func ListSecretSources(scope string) []SecretSource {
	EnsureBuiltinSecretSources()
	secretRegistryMu.Lock()
	defer secretRegistryMu.Unlock()
	order, merged := mergedSecretSources(scope)
	out := make([]SecretSource, 0, len(order))
	for _, n := range order {
		out = append(out, merged[n])
	}
	return out
}

// ListPluginSecretSources lists sources registered outside the bundled set, global plugin origins plus the scope's
func ListPluginSecretSources() []SecretSource {
	EnsureBuiltinSecretSources()
	secretRegistryMu.Lock()
	defer secretRegistryMu.Unlock()
	key := home.Key(home.Process())
	var out []SecretSource
	seen := map[string]bool{}
	for _, n := range secretSourceOrder {
		if secretSourceOrigins[n] == "plugin" {
			out = append(out, secretSources[n])
			seen[n] = true
		}
	}
	for _, n := range scopedSecretOrder[key] {
		if seen[n] {
			for i, s := range out {
				if s.Spec().Name == n {
					out[i] = scopedSecretSources[key][n]
				}
			}
			continue
		}
		out = append(out, scopedSecretSources[key][n])
	}
	return out
}

// EnsureBuiltinSecretSources idempotently registers the bundled sources
func EnsureBuiltinSecretSources() {
	secretRegistryMu.Lock()
	ready := builtinSourcesReady
	builtinSourcesReady = true
	secretRegistryMu.Unlock()
	if ready {
		return
	}
	for _, s := range []SecretSource{&BitwardenSource{}, &OnePasswordSource{}, &CommandSource{}} {
		if !RegisterSecretSource(s, false, true, "") {
			slog.Default().Warn(fmt.Sprintf("Failed to register bundled %s secret source", s.Spec().Label))
		}
	}
}

// ResetSecretRegistryForTests empties the registry
func ResetSecretRegistryForTests() {
	secretRegistryMu.Lock()
	defer secretRegistryMu.Unlock()
	secretSources, secretSourceOrder, secretSourceOrigins = map[string]SecretSource{}, nil, map[string]string{}
	scopedSecretSources, scopedSecretOrder, builtinSourcesReady = map[string]map[string]SecretSource{}, map[string][]string{}, false
}

// ---- orchestrated apply ----

// AppliedVar is the provenance record for one env var the orchestrator set
type AppliedVar struct {
	Name          string
	Source        string
	Shape         string
	OverrodeEnv   bool
	Authoritative bool
}

// SourceReport is one source's outcome within an apply pass
type SourceReport struct {
	Name             string
	Label            string
	Result           *SecretFetchResult
	Applied          []string
	SkippedExisting  []string
	SkippedClaimed   []string
	SkippedProtected []string
	SkippedInvalid   []string
}

// ApplyReport is the merged outcome of one orchestrated apply pass
type ApplyReport struct {
	Sources    []*SourceReport
	Provenance map[string]AppliedVar
	Conflicts  []string
}

// AppliedAny reports whether any var was set
func (r *ApplyReport) AppliedAny() bool { return len(r.Provenance) > 0 }

// Runs the fetch under a wall clock budget, a source that blows it is reported as a timeout
func fetchWithTimeout(source SecretSource, cfg map[string]any, homePath string, environ *SourceEnvironment) *SecretFetchResult {
	timeout := SecretSourceFetchTimeout(cfg)
	done := make(chan *SecretFetchResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- NewSecretFetchResult().Fail(fmt.Sprintf("fetch raised %v", r), SecretErrorInternal)
			}
		}()
		done <- source.Fetch(cfg, homePath, environ)
	}()
	select {
	case result := <-done:
		if result == nil {
			return NewSecretFetchResult().Fail("fetch returned nothing instead of a FetchResult", SecretErrorInternal)
		}
		if result.Secrets == nil {
			result.Secrets = map[string]string{}
		}
		return result
	case <-time.After(timeout):
		return NewSecretFetchResult().Fail(fmt.Sprintf("fetch exceeded %.0fs budget — startup continued without this source (raise secrets.%s.timeout_seconds if the backend is just slow)", timeout.Seconds(), source.Spec().Name), SecretErrorTimeout)
	}
}

func secretsSection(secretsCfg map[string]any, name string) map[string]any {
	if cfg, ok := secretsCfg[name].(map[string]any); ok {
		return cfg
	}
	return map[string]any{}
}

// Enabled sources in secrets.sources order then registration order
func orderedEnabledSecretSources(secretsCfg map[string]any, scope string) []SecretSource {
	all := ListSecretSources(scope)
	byName := map[string]SecretSource{}
	var known []string
	for _, s := range all {
		byName[s.Spec().Name] = s
		known = append(known, s.Spec().Name)
	}
	var names []string
	if explicit, ok := secretsCfg["sources"].([]any); ok {
		for _, e := range explicit {
			if s, isStr := e.(string); isStr {
				names = append(names, s)
			}
		}
	}
	var unknown []string
	for _, n := range names {
		if _, ok := byName[n]; !ok {
			unknown = append(unknown, n)
		}
	}
	if len(unknown) > 0 {
		knownText := strings.Join(known, ", ")
		if knownText == "" {
			knownText = "none"
		}
		slog.Default().Warn(fmt.Sprintf("secrets.sources names unknown source(s): %s (known: %s)", strings.Join(unknown, ", "), knownText))
	}
	var order []string
	seen := map[string]bool{}
	for _, n := range append(names, known...) {
		if _, ok := byName[n]; ok && !seen[n] {
			seen[n] = true
			order = append(order, n)
		}
	}
	var enabled []SecretSource
	for _, n := range order {
		if SecretSourceEnabled(secretsSection(secretsCfg, n)) {
			enabled = append(enabled, byName[n])
		}
	}
	return enabled
}

// EnabledSecretSourceNames are the sources an apply would fetch for the home right now
func EnabledSecretSourceNames(secretsCfg map[string]any, homePath string) map[string]bool {
	out := map[string]bool{}
	for _, s := range orderedEnabledSecretSources(secretsCfg, homePath) {
		out[s.Spec().Name] = true
	}
	return out
}

// Active profile name for a home, empty for the default profile
func activeProfileName(homePath string) string {
	if homePath != "" {
		if filepath.Base(filepath.Dir(homePath)) == "profiles" && filepath.Base(homePath) != "" {
			return filepath.Base(homePath)
		}
	}
	for _, envName := range []string{"ICEBATH_PROFILE_NAME", "ICEBATH_PROFILE"} {
		if v := strings.TrimSpace(os.Getenv(envName)); v != "" && v != "default" {
			return v
		}
	}
	return ""
}

var aliasSuffixes = []string{"_API_KEY", "_TOKEN", "_SECRET", "_KEY", "_PASSWORD"}

// Maps a profile suffixed credential name onto its canonical name when safe
func profileAliasTarget(variable, profile string) string {
	if profile == "" {
		return ""
	}
	suffix := "_" + strings.ToUpper(strings.ReplaceAll(profile, "-", "_"))
	if !strings.HasSuffix(variable, suffix) {
		return ""
	}
	alias := variable[:len(variable)-len(suffix)]
	if alias == "" || !IsValidEnvName(alias) {
		return ""
	}
	for _, s := range aliasSuffixes {
		if strings.HasSuffix(alias, s) {
			return alias
		}
	}
	return ""
}

type secretApplier struct {
	env       *SourceEnvironment
	report    *ApplyReport
	protected map[string]string
	preserve  map[string]bool
	claimed   map[string]string
}

func (a *secretApplier) applySource(source SecretSource, cfg map[string]any, result *SecretFetchResult, profile string, suppliedDirectly map[string]bool) {
	spec := source.Spec()
	label := spec.Label
	if label == "" {
		label = spec.Name
	}
	sr := &SourceReport{Name: spec.Name, Label: label, Result: result}
	a.report.Sources = append(a.report.Sources, sr)
	if !result.OK() {
		return
	}
	override := SecretSourceOverrideExisting(spec, cfg)
	names := make([]string, 0, len(result.Secrets))
	for n := range result.Secrets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, variable := range names {
		value := result.Secrets[variable]
		if !a.tryApply(sr, spec, override, variable, value) || profile == "" {
			continue
		}
		alias := profileAliasTarget(variable, profile)
		if alias == "" || suppliedDirectly[alias] {
			continue
		}
		if _, taken := a.claimed[alias]; taken {
			continue
		}
		if a.tryApply(sr, spec, override, alias, value) {
			result.Warnings = append(result.Warnings, fmt.Sprintf("applied profile-scoped %s as %s (active profile %q)", variable, alias, profile))
		}
	}
}

// Applies one var through the shared guard chain, true when applied
func (a *secretApplier) tryApply(sr *SourceReport, spec SecretSourceSpec, override bool, variable, value string) bool {
	if !IsValidEnvName(variable) {
		sr.SkippedInvalid = append(sr.SkippedInvalid, variable)
		return false
	}
	if _, protected := a.protected[variable]; protected {
		sr.SkippedProtected = append(sr.SkippedProtected, variable)
		return false
	}
	if owner, taken := a.claimed[variable]; taken {
		sr.SkippedClaimed = append(sr.SkippedClaimed, variable)
		a.report.Conflicts = append(a.report.Conflicts, fmt.Sprintf("%s: kept value from %s; %s also supplies it (first source wins — remove one binding or reorder secrets.sources)", variable, owner, spec.Name))
		return false
	}
	existed := a.env.Value(variable) != ""
	if existed && (a.preserve[variable] || !override) {
		sr.SkippedExisting = append(sr.SkippedExisting, variable)
		return false
	}
	a.env.Set(variable, value)
	a.claimed[variable] = spec.Name
	sr.Applied = append(sr.Applied, variable)
	a.report.Provenance[variable] = AppliedVar{Name: variable, Source: spec.Name, Shape: spec.Shape, OverrodeEnv: existed, Authoritative: override && !a.preserve[variable]}
	return true
}

// ApplyAllSecretSources fetches from every enabled source and applies the merged result to the environment
func ApplyAllSecretSources(secretsCfg map[string]any, homePath string, environ *SourceEnvironment) *ApplyReport {
	if environ == nil {
		environ = ProcessSourceEnvironment()
	}
	report := &ApplyReport{Provenance: map[string]AppliedVar{}}
	if secretsCfg == nil {
		secretsCfg = map[string]any{}
	}
	enabled := orderedEnabledSecretSources(secretsCfg, homePath)
	if len(enabled) == 0 {
		return report
	}
	preserve := map[string]bool{}
	if raw, ok := secretsCfg["preserve_existing"].([]any); ok {
		for _, n := range raw {
			if s, isStr := n.(string); isStr && strings.TrimSpace(s) != "" {
				preserve[strings.TrimSpace(s)] = true
			}
		}
	}
	profile := ""
	if v, ok := secretsCfg["profile_alias"]; !ok || truthyAny(v) {
		profile = activeProfileName(homePath)
	}
	var ordered []SecretSource
	for _, s := range enabled {
		if s.Spec().Shape == "mapped" {
			ordered = append(ordered, s)
		}
	}
	for _, s := range enabled {
		if s.Spec().Shape == "bulk" {
			ordered = append(ordered, s)
		}
	}
	type fetched struct {
		source SecretSource
		cfg    map[string]any
		result *SecretFetchResult
	}
	var fetches []fetched
	protected := map[string]string{}
	for _, source := range ordered {
		cfg := secretsSection(secretsCfg, source.Spec().Name)
		result := fetchWithTimeout(source, cfg, homePath, environ)
		fetches = append(fetches, fetched{source, cfg, result})
		for v := range SecretSourceProtectedEnvVars(source.Spec(), cfg) {
			if _, has := protected[v]; !has {
				protected[v] = source.Spec().Name
			}
		}
	}
	suppliedDirectly := map[string]bool{}
	for _, f := range fetches {
		if f.result.OK() {
			for v := range f.result.Secrets {
				suppliedDirectly[v] = true
			}
		}
	}
	applier := &secretApplier{env: environ, report: report, protected: protected, preserve: preserve, claimed: map[string]string{}}
	for _, f := range fetches {
		applier.applySource(f.source, f.cfg, f.result, profile, suppliedDirectly)
	}
	return report
}

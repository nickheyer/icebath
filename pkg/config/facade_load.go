package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
)

// Collaborators the config facade runs from packages that import config
type FacadeHooks struct {
	ApplyUserMoAPresets func(merged map[string]any, raw any)
	SkipDeepMerge       func(prefix, key string) bool
	IsPresetFieldPath   func(parts []string) bool
	Stamp               func() StampHooks
	DotenvUnreadable    func(path string) bool
	ExtraEnvKeys        func() []string
	// Ladder collaborators owned by provider and tools, the facade fills its own readers and writers
	Migration                   func(h string) MigrationSources
	MaskedSecretPrompt          func(prompt string) (string, error)
	ScopedSecret                func(ctx context.Context, key string) (string, bool, error)
	SecretScopeBound            func(ctx context.Context) bool
	PublishScopedEnv            func(ctx context.Context, key string, value *string) bool
	SaveProviderEnvCredential   func(ctx context.Context, h, key, value string) error
	RemoveProviderEnvCredential func(ctx context.Context, h, key string) (bool, error)
	CoreDeclaredEnvNames        func() map[string]bool
}

var (
	facadeMu      sync.RWMutex
	facadeHooks   FacadeHooks
	policyNote    func(setter, homeKey string, raw any)
	savedObserver func(h string, oldRaw, newConfig map[string]any)
	rawRecorder   RawConfigWriteRecorder
)

// Runner of one raw user config write that reports what it changed in the file it replaced
type RawConfigWriteRecorder func(ctx context.Context, h, configPath string, userConfig map[string]any, write func(configPath string, userConfig map[string]any) error) error

// Installs the facade collaborators, called from an init of the package that owns them
func RegisterConfigFacade(h FacadeHooks) {
	facadeMu.Lock()
	facadeHooks = h
	facadeMu.Unlock()
}

// Installs the redaction policy recorder the log formatter reads, setter names the policy kind
func RegisterPolicyNote(note func(setter, homeKey string, raw any)) {
	facadeMu.Lock()
	policyNote = note
	facadeMu.Unlock()
}

// Installs the observer a user saved config is reported to after the write with the home that owns it
func RegisterConfigSavedObserver(observe func(h string, oldRaw, newConfig map[string]any)) {
	facadeMu.Lock()
	savedObserver = observe
	facadeMu.Unlock()
}

// Installs the recorder every raw user config write runs through
func RegisterRawConfigWriteRecorder(record RawConfigWriteRecorder) {
	facadeMu.Lock()
	rawRecorder = record
	facadeMu.Unlock()
}

func hooks() FacadeHooks {
	facadeMu.RLock()
	defer facadeMu.RUnlock()
	return facadeHooks
}

// Runtime key owners with the moa preset field rule the facade registration supplies
func FacadeRuntimeKeyOwners(owners RuntimeKeyOwners) RuntimeKeyOwners {
	if owners.IsPresetFieldPath == nil {
		owners.IsPresetFieldPath = hooks().IsPresetFieldPath
	}
	return owners
}

// Records the loaded profile redaction policy when a policy recorder is installed
func notePolicy(setter, h string, raw any) {
	facadeMu.RLock()
	note := policyNote
	facadeMu.RUnlock()
	if note != nil {
		note(setter, home.Key(h), raw)
	}
}

// Applies the explicit user moa preset map over merged defaults through the registered owner
func applyUserMoAPresets(merged map[string]any, raw any) {
	if fn := hooks().ApplyUserMoAPresets; fn != nil {
		fn(merged, raw)
	}
}

var configLock sync.Mutex

type configLockKey struct{}

// Takes the process config lock unless ctx already holds it, returning the holding ctx and its release
func LockConfig(ctx context.Context) (context.Context, func()) {
	if ctx.Value(configLockKey{}) != nil {
		return ctx, func() {}
	}
	configLock.Lock()
	return context.WithValue(ctx, configLockKey{}, true), configLock.Unlock
}

// Takes the process config lock without blocking, true when ctx already holds it or the lock was free
func TryLockConfig(ctx context.Context) (context.Context, func(), bool) {
	if ctx.Value(configLockKey{}) != nil {
		return ctx, func() {}, true
	}
	if !configLock.TryLock() {
		return ctx, func() {}, false
	}
	return context.WithValue(ctx, configLockKey{}, true), configLock.Unlock, true
}

// Cached effective config with the signatures and env values it was built against
type loadEntry struct {
	sig        []any
	data       Tree
	env        map[string]*string
	managedEnv map[string]*string
}

var (
	loadCacheMu  sync.Mutex
	loadCache    = map[string]*loadEntry{}
	rawCache     = map[string]rawEntry{}
	lastExpanded = map[string]map[string]any{}
)

type rawEntry struct {
	sig  []any
	data map[string]any
}

var zeroManagedSig = []any{0, 0, 0, 0, ""}

// Newest parseable backup reader, replaced in tests to count rebuilds
var loadNewestGoodBackupFn = LoadNewestGoodBackup

// User signature and combined cache signature of a config path, both nil when neither file exists
func loadConfigCacheSig(ctx context.Context, path string) ([]any, []any, error) {
	userSig, err := ConfigCacheVersion(ctx, path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, nil, err
		}
		userSig = nil
	}
	managedSig := zeroManagedSig
	if dir := ManagedDir(); dir != "" {
		if v, merr := FileCacheVersion(filepath.Join(dir, "config.yaml"), false); merr == nil {
			managedSig = v
		}
	}
	if userSig == nil && reflect.DeepEqual(managedSig, zeroManagedSig) {
		return nil, nil, nil
	}
	base := userSig
	if base == nil {
		base = zeroManagedSig
	}
	return userSig, append(append([]any{}, base...), managedSig...), nil
}

// Cached config while its signature and every referenced env value still match, nil otherwise
func loadConfigCacheHit(ctx context.Context, path string, sig []any, lookup Lookup) (Tree, error) {
	if sig == nil {
		return nil, nil
	}
	loadCacheMu.Lock()
	entry := loadCache[path]
	loadCacheMu.Unlock()
	if entry == nil || !reflect.DeepEqual(entry.sig, sig) {
		return nil, nil
	}
	if failed := FailedReadError(entry.data); failed != nil && IsOSError(failed) {
		if err := ProbeConfigReadable(ctx, path); err != nil {
			if IsOSError(err) {
				return entry.data, nil
			}
			return nil, err
		}
		return nil, nil
	}
	if SnapshotCurrent(entry.env, lookup) && SnapshotCurrent(entry.managedEnv, processLookup) {
		return entry.data, nil
	}
	return nil, nil
}

func storeLoadEntry(path string, entry *loadEntry) {
	loadCacheMu.Lock()
	loadCache[path] = entry
	loadCacheMu.Unlock()
}

// Python exception text a non mapping config root raises while being merged
func nonMappingRootError(doc any) error {
	name := PyTypeName(doc)
	switch doc.(type) {
	case []any, string:
		attr := "items"
		if containsMaxTurns(doc) {
			attr = "get"
		}
		return fmt.Errorf("'%s' object has no attribute '%s'", name, attr)
	}
	return fmt.Errorf("argument of type '%s' is not iterable", name)
}

// Reports whether a list or string root holds max_turns the way Python membership tests it
func containsMaxTurns(doc any) bool {
	switch c := doc.(type) {
	case []any:
		for _, v := range c {
			if s, ok := v.(string); ok && s == "max_turns" {
				return true
			}
		}
	case string:
		return len(c) >= len("max_turns") && containsSubstring(c, "max_turns")
	}
	return false
}

func containsSubstring(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Python dict() of an agent section value, the TypeError or ValueError text when it cannot convert
func pyDictOf(v any) (map[string]any, error) {
	if !PyTruthy(v) {
		return map[string]any{}, nil
	}
	switch c := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(c))
		for k, val := range c {
			out[k] = val
			if pk, ok := MappingPyKey(c, k); ok {
				SetMappingPyKey(out, k, pk)
			}
		}
		SetMappingKeys(out, MappingKeys(c))
		return out, nil
	case string:
		return nil, fmt.Errorf("dictionary update sequence element #0 has length 1; 2 is required")
	case []any:
		out := map[string]any{}
		keys := []string{}
		for i, item := range c {
			pair, ok := item.([]any)
			if !ok {
				if s, isStr := item.(string); isStr {
					return nil, fmt.Errorf("dictionary update sequence element #%d has length %d; 2 is required", i, len([]rune(s)))
				}
				return nil, fmt.Errorf("cannot convert dictionary update sequence element #%d to a sequence", i)
			}
			if len(pair) != 2 {
				return nil, fmt.Errorf("dictionary update sequence element #%d has length %d; 2 is required", i, len(pair))
			}
			k := PyStr(pair[0])
			if _, seen := out[k]; !seen {
				keys = append(keys, k)
			}
			out[k] = pair[1]
		}
		SetMappingKeys(out, keys)
		return out, nil
	}
	return nil, fmt.Errorf("'%s' object is not iterable", PyTypeName(v))
}

// Defaults with the user document merged in, legacy root max_turns moved under agent first
func mergeUserConfig(ctx context.Context, path string, cfg map[string]any) (map[string]any, error) {
	doc, err := ReadConfigDoc(ctx, path)
	if err != nil {
		return nil, err
	}
	if !PyTruthy(doc) {
		doc = map[string]any{}
	}
	user, ok := doc.(map[string]any)
	if !ok {
		return nil, nonMappingRootError(doc)
	}
	forgetParseFailure(path)
	if raw, has := user["max_turns"]; has {
		agent, aerr := pyDictOf(user["agent"])
		if aerr != nil {
			return nil, aerr
		}
		if agent["max_turns"] == nil {
			if _, present := agent["max_turns"]; !present {
				AppendMappingKey(agent, "max_turns")
			}
			agent["max_turns"] = raw
		}
		if _, present := user["agent"]; !present {
			AppendMappingKey(user, "agent")
		}
		user["agent"] = agent
		delete(user, "max_turns")
	}
	merged := DeepMerge(cfg, user)
	applyUserMoAPresets(merged, user)
	if _, err := BackupConfig(ctx, path, "good", DefaultBackupKeep); err != nil {
		return nil, err
	}
	return merged, nil
}

// Drops the recorded parse failure of a config that reads again
func forgetParseFailure(path string) {
	parseFailureMu.Lock()
	delete(configParseFailure, path)
	parseFailureMu.Unlock()
}

// Expanded config with the managed overlay merged at the leaves, plus the managed document it applied
func mergeManagedOverlay(expanded map[string]any) (map[string]any, map[string]any) {
	managed := LoadManagedConfig()
	if len(managed) == 0 {
		return expanded, nil
	}
	normalized := normalizeModelKeys(managed)
	if model, ok := normalized["model"].(string); ok {
		normalized = Clone(normalized).(map[string]any)
		normalized["model"] = map[string]any{"default": model}
	}
	return DeepMerge(expanded, Expand(normalized, processLookup).(map[string]any)), managed
}

// Last good config served after a parse failure, nil when there is none and defaults apply
func lastKnownGoodFallback(ctx context.Context, path string, sig []any, readErr error, lookup Lookup) (Tree, error) {
	loadCacheMu.Lock()
	lkg := lastExpanded[path]
	loadCacheMu.Unlock()
	fallback := "last-known-good"
	if lkg == nil {
		rawGood, gerr := loadNewestGoodBackupFn(ctx, path)
		if gerr != nil {
			return nil, gerr
		}
		if rawGood != nil {
			mergedGood := DeepMerge(Clone(map[string]any(Defaults())).(map[string]any), rawGood)
			applyUserMoAPresets(mergedGood, rawGood)
			normalizedGood, cerr := Canonicalize(mergedGood)
			if cerr != nil {
				return nil, cerr
			}
			expandedGood := Expand(normalizedGood, lookup).(map[string]any)
			lkg, _ = mergeManagedOverlay(expandedGood)
			fallback = "last-known-good-backup"
		}
	}
	if lkg == nil {
		fallback = "defaults"
	}
	if err := WarnConfigParseFailure(ctx, path, readErr, fallback); err != nil {
		return nil, err
	}
	if lkg == nil {
		return nil, nil
	}
	served := Tree(Expand(Clone(lkg), lookup).(map[string]any))
	markFailedRead(served, readErr)
	if sig != nil {
		storeLoadEntry(path, &loadEntry{sig: sig, data: served})
	}
	return served, nil
}

// Effective config of a home without the defensive copy, the cached value must never be mutated
func LoadReadonly(ctx context.Context, h string, lookup Lookup) (Tree, error) {
	return loadConfigImpl(ctx, h, lookup, false)
}

// Copy of a cached config for a caller that may mutate it, the shared value otherwise
func copyIfWanted(t Tree, wantDeepcopy bool) Tree {
	if wantDeepcopy && t != nil {
		return t.Clone()
	}
	return t
}

// Effective config through the signature cache, rebuilt under the config lock on a miss
func loadConfigImpl(ctx context.Context, h string, lookup Lookup, wantDeepcopy bool) (Tree, error) {
	NoteRuntimeImport("hermes_cli.config")
	if lookup == nil {
		lookup = processLookup
	}
	if snap := workerConfigSnapshot(); snap != nil {
		return Tree(DeepMerge(Clone(map[string]any(Defaults())).(map[string]any), snap)), nil
	}
	path := GetConfigPath(h)
	loadCacheMu.Lock()
	_, cached := loadCache[path]
	loadCacheMu.Unlock()
	if cached {
		if _, fastSig, err := loadConfigCacheSig(ctx, path); err == nil {
			if hit, herr := loadConfigCacheHit(ctx, path, fastSig, lookup); herr == nil && hit != nil {
				return copyIfWanted(hit, wantDeepcopy), nil
			}
		}
	}
	ctx, unlock := LockConfig(ctx)
	defer unlock()
	if err := EnsureIcebathHome(ctx, h); err != nil {
		return nil, err
	}
	userSig, sig, err := loadConfigCacheSig(ctx, path)
	if err != nil {
		return nil, err
	}
	if hit, herr := loadConfigCacheHit(ctx, path, sig, lookup); herr != nil || hit != nil {
		return copyIfWanted(hit, wantDeepcopy), herr
	}
	cfg := Clone(map[string]any(Defaults())).(map[string]any)
	if userSig != nil {
		merged, rerr := mergeUserConfig(ctx, path, cfg)
		if rerr != nil {
			var exit SystemExitError
			if errors.As(rerr, &exit) {
				return nil, rerr
			}
			lkg, lerr := lastKnownGoodFallback(ctx, path, sig, rerr, lookup)
			if lerr != nil || lkg != nil {
				return copyIfWanted(lkg, wantDeepcopy), lerr
			}
			base, cerr := Canonicalize(cfg)
			if cerr != nil {
				return nil, cerr
			}
			expanded, _ := mergeManagedOverlay(Expand(base, lookup).(map[string]any))
			fallback := Tree(expanded)
			markFailedRead(fallback, rerr)
			if sig != nil {
				storeLoadEntry(path, &loadEntry{sig: sig, data: fallback})
			}
			return copyIfWanted(fallback, wantDeepcopy), nil
		}
		cfg = merged
	}
	normalized, err := Canonicalize(cfg)
	if err != nil {
		return nil, err
	}
	expanded, managed := mergeManagedOverlay(Expand(normalized, lookup).(map[string]any))
	loadCacheMu.Lock()
	lastExpanded[path] = Clone(expanded).(map[string]any)
	loadCacheMu.Unlock()
	security, _ := expanded["security"].(map[string]any)
	notePolicy("note_config_policy", h, security["redact_secrets"])
	if sig == nil {
		loadCacheMu.Lock()
		delete(loadCache, path)
		loadCacheMu.Unlock()
		return Tree(expanded), nil
	}
	entry := &loadEntry{sig: sig, data: Tree(Clone(expanded).(map[string]any)), env: RefSnapshot(normalized, lookup)}
	if managed != nil {
		entry.managedEnv = RefSnapshot(managed, processLookup)
	}
	storeLoadEntry(path, entry)
	if !wantDeepcopy {
		return entry.data, nil
	}
	return Tree(expanded), nil
}

// Effective config of a home, defaults merged under the user file, references expanded and the managed overlay on top
func LoadContext(ctx context.Context, h string, lookup Lookup) (Tree, error) {
	return loadConfigImpl(ctx, h, lookup, true)
}

// Effective config of a home through the process context
func Load(h string, lookup Lookup) (Tree, error) {
	return LoadContext(context.Background(), h, lookup)
}

// Forgets every cached config document and the last good expansions
func ResetCache() {
	loadCacheMu.Lock()
	loadCache = map[string]*loadEntry{}
	rawCache = map[string]rawEntry{}
	lastExpanded = map[string]map[string]any{}
	effectiveCache = map[string]effectiveEntry{}
	lastGoodUserRaw = map[string]map[string]any{}
	loadCacheMu.Unlock()
}

// Cached raw config of a path when its signature equals sig, shared by the lock free and locked checks
func rawConfigCacheHit(path string, sig []any) (map[string]any, bool) {
	loadCacheMu.Lock()
	hit, ok := rawCache[path]
	loadCacheMu.Unlock()
	if ok && reflect.DeepEqual(hit.sig, sig) {
		return hit.data, true
	}
	return nil, false
}

// Publishes a caller owned copy as the raw config of a path at sig in one replace
func rawConfigCacheStore(path string, sig []any, data map[string]any) {
	loadCacheMu.Lock()
	rawCache[path] = rawEntry{sig: sig, data: data}
	loadCacheMu.Unlock()
}

// Raw user document of a home as written, a failed read marked so writers refuse it
func readRawConfigImpl(ctx context.Context, h string) (map[string]any, error) {
	if snap := workerConfigSnapshot(); snap != nil {
		return snap, nil
	}
	path := GetConfigPath(h)
	if sig, err := ConfigCacheVersion(ctx, path); err == nil {
		if hit, ok := rawConfigCacheHit(path, sig); ok {
			return hit, nil
		}
	}
	ctx, unlock := LockConfig(ctx)
	defer unlock()
	sig, err := ConfigCacheVersion(ctx, path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]any{}, nil
		}
		if IsOSError(err) {
			failed := map[string]any{}
			markFailedRead(failed, err)
			return failed, nil
		}
		return nil, err
	}
	if hit, ok := rawConfigCacheHit(path, sig); ok {
		return hit, nil
	}
	doc, err := ReadConfigDoc(ctx, path)
	if err != nil {
		var exit SystemExitError
		if errors.As(err, &exit) {
			return nil, err
		}
		if werr := WarnConfigParseFailure(ctx, path, err, "defaults"); werr != nil {
			return nil, werr
		}
		failed := map[string]any{}
		markFailedRead(failed, err)
		return failed, nil
	}
	if !PyTruthy(doc) {
		doc = map[string]any{}
	}
	data, ok := doc.(map[string]any)
	if !ok {
		failed := map[string]any{}
		markFailedRead(failed, fmt.Errorf("top-level YAML must be a mapping, got %s", PyTypeName(doc)))
		return failed, nil
	}
	forgetParseFailure(path)
	cached := Clone(data).(map[string]any)
	rawConfigCacheStore(path, sig, cached)
	return cached, nil
}

// Raw config.yaml of a home with no defaults or expansion, a private copy the caller may mutate
func ReadRawConfig(ctx context.Context, h string) (map[string]any, error) {
	data, err := readRawConfigImpl(ctx, h)
	if err != nil {
		return nil, err
	}
	return Clone(data).(map[string]any), nil
}

// Raw config.yaml of a home shared with the cache, the result must never be mutated
func ReadRawConfigReadonly(ctx context.Context, h string) (map[string]any, error) {
	return readRawConfigImpl(ctx, h)
}

// User config.yaml as written with no cache, ONLY legal for write-back round-trips and raw-file diagnostics, behavioral reads use load_config()
func ReadUserConfigRaw(ctx context.Context, configPath string) (map[string]any, error) {
	doc, err := ReadConfigDoc(ctx, configPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	if m, ok := doc.(map[string]any); ok {
		return m, nil
	}
	return map[string]any{}, nil
}

// Config option absent from the loaded config with the default it would take
type MissingConfigField struct {
	Key         string
	Default     any
	Description string
}

// Default options the loaded config lacks, recursing into sections both sides define
func GetMissingConfigFields(ctx context.Context, h string, lookup Lookup) ([]MissingConfigField, error) {
	current, err := LoadReadonly(ctx, h, lookup)
	if err != nil {
		return nil, err
	}
	return MissingConfigFields(current), nil
}

// Default options a loaded config lacks, never descending where the registered owner forbids it
func MissingConfigFields(current map[string]any) []MissingConfigField {
	skip := hooks().SkipDeepMerge
	var missing []MissingConfigField
	var check func(defaults, cur map[string]any, prefix string)
	check = func(defaults, cur map[string]any, prefix string) {
		for _, key := range MappingKeys(defaults) {
			if len(key) > 0 && key[0] == '_' {
				continue
			}
			full := key
			if prefix != "" {
				full = prefix + "." + key
			}
			value := defaults[key]
			got, has := cur[key]
			if !has {
				missing = append(missing, MissingConfigField{Key: full, Default: value, Description: "New config option: " + full})
				continue
			}
			dm, dok := value.(map[string]any)
			cm, cok := got.(map[string]any)
			if dok && cok && (skip == nil || !skip(prefix, key)) {
				check(dm, cm, full)
			}
		}
	}
	check(Defaults(), current, "")
	return missing
}

// Structure issues of the loaded config, a single load issue when the config cannot be loaded
func ValidateLoadedConfigStructure(ctx context.Context, h string, lookup Lookup, removedNote func(section, name string) string) []ConfigIssue {
	cfg, err := LoadContext(ctx, h, lookup)
	if err != nil {
		return []ConfigIssue{ConfigLoadIssue(err)}
	}
	return ValidateConfigStructure(cfg, removedNote)
}

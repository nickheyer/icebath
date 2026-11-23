package settings

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Value of a setting in effect and the layer it came from
type Value struct {
	Key    string `json:"key"`
	Value  any    `json:"value"`
	Source Source `json:"source"`
	// An administrator pins it, so no scope can change it
	Locked bool `json:"locked,omitempty"`
}

// Value a dotted path holds in a document
func lookupPath(doc map[string]any, key string) (any, bool) {
	if doc == nil {
		return nil, false
	}
	return config.Tree(doc).Lookup(key)
}

// One read of a home's config.yaml and the administrator's pins, reused until either file changes
type layers struct {
	// Versions of config.yaml and the managed config the read saw
	sig []any
	// The user's config.yaml in canonical form, references unexpanded
	doc map[string]any
	// Every key the administrator pins with its value
	managed map[string]any
	// The home's runtime state settings
	state map[string]any
}

// Latest layers read per home
var layerCache = struct {
	sync.Mutex
	byHome map[string]*layers
}{byHome: map[string]*layers{}}

// Versions of the files a home's layers come from: one stat of config.yaml on the file backend, the backend's own version otherwise
func layerSignature(ctx context.Context, h string) []any {
	sig := []any{}
	path := configPathOf(h)
	if backend := strings.TrimSpace(os.Getenv(config.BackendEnv)); backend == "" || strings.EqualFold(backend, "file") {
		if fi, err := os.Stat(path); err == nil {
			sig = append(sig, config.FileCacheVersionOf(path, fi)...)
		}
	} else if version, err := config.ConfigCacheVersionOrNone(ctx, path); err == nil {
		sig = append(sig, version...)
	} else {
		sig = append(sig, err.Error())
	}
	sig = append(sig, "|")
	sig = append(sig, stateSignature(h)...)
	sig = append(sig, "|")
	if dir := config.ManagedDir(); dir != "" {
		if version, err := config.FileCacheVersion(filepath.Join(dir, "config.yaml"), false); err == nil {
			sig = append(sig, version...)
		}
	}
	return sig
}

// Resolved homes keyed by the override or env text that named them
var homeMemo sync.Map

// Home of ctx, remembered per override or ICEBATH_HOME text so hot reads skip path expansion
func homeDir(ctx context.Context) string {
	raw := home.Override(ctx)
	if raw == "" {
		raw = os.Getenv(home.EnvHome)
		if strings.TrimSpace(raw) == "" {
			return home.Dir(ctx)
		}
		raw = "env:" + raw
	}
	if h, ok := homeMemo.Load(raw); ok {
		return h.(string)
	}
	h := home.Dir(ctx)
	homeMemo.Store(raw, h)
	return h
}

// config.yaml paths keyed by home
var configPaths sync.Map

// config.yaml of a home
func configPathOf(h string) string {
	if p, ok := configPaths.Load(h); ok {
		return p.(string)
	}
	p := config.GetConfigPath(h)
	configPaths.Store(h, p)
	return p
}

// Dotted keys of a managed document mapped to their values, references expanded against the process env
func flattenManaged(doc map[string]any, prefix string, out map[string]any) {
	for k, v := range doc {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if child, ok := v.(map[string]any); ok && len(child) > 0 {
			flattenManaged(child, key, out)
			continue
		}
		out[key] = config.Expand(v, os.LookupEnv)
	}
}

// Layers of a home, read again only when config.yaml or the managed config changed; an unreadable config keeps the last good read
func layersOf(ctx context.Context, h string) *layers {
	sig := layerSignature(ctx, h)
	layerCache.Lock()
	cached := layerCache.byHome[h]
	layerCache.Unlock()
	if cached != nil && slices.Equal(cached.sig, sig) {
		return cached
	}
	next := &layers{sig: sig, doc: map[string]any{}, managed: map[string]any{}, state: map[string]any{}}
	if state, err := readState(h); err != nil {
		pkgLog().Warn("settings: state file unreadable", "home", h, "err", err)
	} else {
		next.state = state
	}
	raw, err := config.ReadRawConfigReadonly(ctx, h)
	switch {
	case err != nil:
		pkgLog().Warn("settings: config.yaml unreadable", "home", h, "err", err)
		if cached != nil {
			return cached
		}
	case config.FailedReadError(raw) != nil:
		if cached != nil {
			return cached
		}
	default:
		doc, cerr := config.Canonicalize(raw)
		if cerr != nil {
			pkgLog().Warn("settings: config.yaml has a broken agent section", "home", h, "err", cerr)
			doc = raw
		}
		next.doc = doc
		noteUnknownKeys(h, doc)
	}
	flattenManaged(config.LoadManagedConfig(), "", next.managed)
	layerCache.Lock()
	layerCache.byHome[h] = next
	layerCache.Unlock()
	return next
}

// Keys already reported as unknown, per home
var unknownNoted = struct {
	sync.Mutex
	seen map[string]bool
}{seen: map[string]bool{}}

// Dotted sections that hold registered keys, rebuilt when the registry grows
var sectionCache = struct {
	sync.Mutex
	size     int
	sections map[string]bool
}{}

// Whether a dotted path is a section above at least one registered key
func isSection(path string) bool {
	sectionCache.Lock()
	defer sectionCache.Unlock()
	entries := All()
	if sectionCache.sections == nil || sectionCache.size != len(entries) {
		sections := map[string]bool{}
		for _, e := range entries {
			key := e.Info().Key
			for i := strings.IndexByte(key, '.'); i >= 0; i = nextDot(key, i) {
				sections[key[:i]] = true
			}
		}
		sectionCache.sections, sectionCache.size = sections, len(entries)
	}
	return sectionCache.sections[path]
}

// Index of the next dot after i, -1 when none
func nextDot(key string, i int) int {
	j := strings.IndexByte(key[i+1:], '.')
	if j < 0 {
		return -1
	}
	return i + 1 + j
}

// Logs one plain line for each key of a config file no setting reads, once per home and key; the value is ignored
func noteUnknownKeys(h string, doc map[string]any) {
	var unknown []string
	var walk func(m map[string]any, prefix string)
	walk = func(m map[string]any, prefix string) {
		for k, v := range m {
			path := prefix + k
			if FindKey(path) != nil {
				continue
			}
			if child, isMap := v.(map[string]any); isMap && isSection(path) {
				walk(child, path+".")
				continue
			}
			unknown = append(unknown, path)
		}
	}
	walk(doc, "")
	slices.Sort(unknown)
	for _, path := range unknown {
		unknownNoted.Lock()
		first := !unknownNoted.seen[h+"\x00"+path]
		unknownNoted.seen[h+"\x00"+path] = true
		unknownNoted.Unlock()
		if first {
			pkgLog().Warn("config.yaml holds " + path + ", which is not a setting; it is ignored")
		}
	}
}

// Forgets every cached read so the next resolve reads the files again
func forgetLayers() {
	layerCache.Lock()
	clear(layerCache.byHome)
	layerCache.Unlock()
}

// Stored value at a key of the user's config.yaml with references expanded for ctx
func storedValue(ctx context.Context, doc map[string]any, key string) (any, bool) {
	v, ok := lookupPath(doc, key)
	if !ok {
		return nil, false
	}
	return config.Expand(v, config.EnvRefLookupContext(ctx)), true
}

// Stored value of an entry in the profile layer and whether the user set it
func globalValue(ctx context.Context, e Entry, l *layers) (any, bool) {
	m := e.Info()
	if _, isSecret := e.(*Secret); isSecret {
		if m.Env == "" {
			return nil, false
		}
		v, ok := config.LoadEnv(homeDir(ctx))[m.Env]
		return v, ok && v != ""
	}
	stored := l.doc
	if m.State {
		stored = l.state
	}
	if v, set := storedValue(ctx, stored, m.Key); set {
		if mp, isMap := e.(*Map); isMap && len(mp.Default) > 0 {
			if user := config.AsMap(v); user != nil {
				return config.DeepMerge(config.Clone(mp.Default).(map[string]any), user), true
			}
		}
		return v, true
	}
	return nil, false
}

// Resolves a setting through every layer: default, global, platform, session, env override, managed pin
func Resolve(ctx context.Context, key string) (Value, error) {
	e := Find(key)
	if e == nil {
		return Value{}, ErrUnknownKey
	}
	return resolveEntry(ctx, e), nil
}

// Typed value of a stored value, the default when it does not fit
func typedOr(e Entry, v any) any {
	if typed, ok := e.read(v); ok {
		return typed
	}
	pkgLog().Debug("settings: stored value does not fit, using the default", "key", e.Info().Key)
	typed, _ := e.read(e.DefaultValue())
	return typed
}

// Layered value of an entry with its source
func resolveEntry(ctx context.Context, e Entry) Value {
	m := e.Info()
	if defaultsOnly(ctx) {
		if v, ok := platformDefault(ctx, m, PlatformFrom(ctx)); ok {
			return Value{Key: m.Key, Value: v, Source: SourceDefault}
		}
		return Value{Key: m.Key, Value: e.DefaultValue(), Source: SourceDefault}
	}
	l := layersOf(ctx, homeDir(ctx))
	if v, ok := l.managed[m.Key]; ok {
		return Value{Key: m.Key, Value: v, Source: SourceManaged, Locked: true}
	}
	if _, isSecret := e.(*Secret); !isSecret && m.Env != "" {
		if v, ok := os.LookupEnv(m.Env); ok && strings.TrimSpace(v) != "" {
			if coerced, err := e.Coerce(v); err == nil {
				return Value{Key: m.Key, Value: coerced, Source: SourceEnv}
			}
		}
	}
	if m.Derive != nil {
		v, source := m.Derive(ctx)
		return Value{Key: m.Key, Value: v, Source: source}
	}
	if v, ok := sessionValue(ctx, SessionFrom(ctx), m.Key); ok {
		return Value{Key: m.Key, Value: v, Source: SourceSession}
	}
	platform := PlatformFrom(ctx)
	if v, ok := platformValue(ctx, l, m, platform); ok {
		return Value{Key: m.Key, Value: v, Source: SourceGlobal}
	}
	if v, ok := globalValue(ctx, e, l); ok && !(m.PlatformOnly && platform != "" && platform != LocalPlatform) {
		return Value{Key: m.Key, Value: v, Source: SourceGlobal}
	}
	if secret, isSecret := e.(*Secret); isSecret && secret.Env != "" {
		if v, ok := os.LookupEnv(secret.Env); ok && v != "" {
			return Value{Key: m.Key, Value: v, Source: SourceEnv}
		}
	}
	if v, ok := platformDefault(ctx, m, platform); ok {
		return Value{Key: m.Key, Value: v, Source: SourceDefault}
	}
	return Value{Key: m.Key, Value: e.DefaultValue(), Source: SourceDefault}
}

// Typed value of an entry in effect for ctx
func resolveTyped(ctx context.Context, e Entry) any {
	return typedOr(e, resolveEntry(ctx, e).Value)
}

type defaultsCtxKey struct{}

// Context whose reads skip every stored layer and answer each setting's default
func DefaultsOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, defaultsCtxKey{}, true)
}

// Whether reads of ctx answer only defaults
func defaultsOnly(ctx context.Context) bool {
	on, _ := ctx.Value(defaultsCtxKey{}).(bool)
	return on
}

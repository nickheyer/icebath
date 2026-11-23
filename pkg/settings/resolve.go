package settings

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

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
	// The administrator's config document, references expanded against the process env and nulls dropped
	managed map[string]any
	// The home's runtime state settings
	state map[string]any
	// Values the derived settings written in doc give the keys they stand for
	derived map[string]any
	// The keys of doc no setting reads were reported
	unknownTold atomic.Bool
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

// The administrator's config document with references expanded against the process env, so a user cannot shadow a pinned literal
func managedDoc() map[string]any {
	doc, _ := config.Expand(config.LoadManagedConfig(), os.LookupEnv).(map[string]any)
	if doc == nil {
		return map[string]any{}
	}
	return doc
}

// Whether a setting's value is a mapping of named parts, which a pin may cover in part
func takesMapping(e Entry) bool {
	kind := e.Kind()
	return kind == KindMap || kind == KindObject
}

// Value the administrator pins for a setting: whole when it replaces the setting, not whole when a mapping pins only the parts it names
func managedPin(l *layers, e Entry) (v any, whole, pinned bool) {
	v, ok := lookupPath(l.managed, e.Info().Key)
	if !ok || v == nil {
		return nil, false, false
	}
	if _, isMapping := v.(map[string]any); isMapping && takesMapping(e) {
		return v, false, true
	}
	return v, true, true
}

// Layers of a home, read again only when config.yaml or the managed config changed; an unreadable config keeps the last good read
func layersOf(ctx context.Context, h string) *layers {
	sig := layerSignature(ctx, h)
	layerCache.Lock()
	cached := layerCache.byHome[h]
	layerCache.Unlock()
	if cached != nil && slices.Equal(cached.sig, sig) {
		tellUnknownKeys(h, cached)
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
		next.derived = derivedCompanions(ctx, doc)
	}
	next.managed = managedDoc()
	tellMisfits(ctx, h, next)
	layerCache.Lock()
	layerCache.byHome[h] = next
	layerCache.Unlock()
	tellUnknownKeys(h, next)
	return next
}

// Notices already given, so a person hears of each problem once per home
var noticesGiven = struct {
	sync.Mutex
	seen map[string]bool
}{seen: map[string]bool{}}

// Whether a notice is given for the first time
func firstNotice(parts ...string) bool {
	id := strings.Join(parts, "\x00")
	noticesGiven.Lock()
	defer noticesGiven.Unlock()
	if noticesGiven.seen[id] {
		return false
	}
	noticesGiven.seen[id] = true
	return true
}

// Whether every settings block a plugin declares is registered, so a key no setting names is truly unknown
var pluginKeysKnown atomic.Bool

// Marks the settings blocks of every discovered plugin as registered, which lets the notices about unknown config keys out
func PluginKeysRegistered() { pluginKeysKnown.Store(true) }

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

// Walks a stored document, handing each value a setting covers to known and each key no setting reads to unknown
func walkStored(doc map[string]any, known func(e Entry, key string, v any), unknown func(key string)) {
	var walk func(m map[string]any, prefix string)
	walk = func(m map[string]any, prefix string) {
		for k, v := range m {
			path := prefix + k
			if e := FindKey(path); e != nil {
				if known != nil {
					known(e, path, v)
				}
				continue
			}
			if child, isMap := v.(map[string]any); isMap && isSection(path) {
				walk(child, path+".")
				continue
			}
			if unknown != nil {
				unknown(path)
			}
		}
	}
	walk(doc, "")
}

// Logs one plain line for each key of a home's config.yaml no setting reads, once per home and key, as soon as the plugin settings blocks are registered
func tellUnknownKeys(h string, l *layers) {
	if !pluginKeysKnown.Load() || l.unknownTold.Load() || l.unknownTold.Swap(true) {
		return
	}
	var unknown []string
	walkStored(l.doc, func(e Entry, key string, _ any) {
		if e.Info().State {
			unknown = append(unknown, key)
		}
	}, func(key string) { unknown = append(unknown, key) })
	slices.Sort(unknown)
	for _, key := range unknown {
		if firstNotice(h, "unknown", key) {
			pkgLog().Warn(storePlace() + " holds " + key + ", which is not a setting; it is ignored")
		}
	}
	var unknownPins []string
	walkStored(l.managed, nil, func(key string) { unknownPins = append(unknownPins, key) })
	slices.Sort(unknownPins)
	for _, key := range unknownPins {
		if firstNotice(managedPlace, "unknown", key) {
			pkgLog().Warn(managedPlace + " holds " + key + ", which is not a setting; it is ignored")
		}
	}
}

// Name of the administrator's config file in notices
const managedPlace = "The administrator's config.yaml"

// Name notices give the place a home's settings are stored: the file, or Remote Config where a config service holds them
func storePlace() string {
	if backend := strings.TrimSpace(os.Getenv(config.BackendEnv)); backend == "" || strings.EqualFold(backend, "file") {
		return "config.yaml"
	}
	return "Remote Config"
}

// Calls report for every part of a stored value its entry does not take, with the full key of the part; field is nil for a name an object has no field for
func eachMisfit(e Entry, key string, v any, report func(key string, v any, field Entry)) {
	if child, isChild := e.(*mapChild); isChild {
		e = child.Entry
	}
	if v == nil {
		return
	}
	if _, ok := readPart(e, v); !ok {
		report(key, v, e)
		return
	}
	switch t := e.(type) {
	case *List:
		if t.Item == nil {
			return
		}
		items, _ := t.items(v)
		for i, item := range items {
			eachMisfit(t.Item, key+"."+strconv.Itoa(i), item, report)
		}
	case *Map:
		if t.Value == nil {
			return
		}
		for name, item := range config.AsMap(v) {
			eachMisfit(t.Value, key+"."+name, item, report)
		}
	case *Object:
		for _, part := range t.parts("", config.AsMap(v)) {
			switch {
			case part.field != nil:
				eachMisfit(part.field, key+"."+part.name, part.value, report)
			case t.holds(part.name):
				report(key+"."+part.name, part.value, &Object{Meta: Meta{Key: key + "." + part.name}})
			default:
				report(key+"."+part.name, part.value, nil)
			}
		}
	}
}

// What a setting takes, said the way a refusal of the same value says it
func takes(e Entry, v any) string {
	if _, err := coercePart(e, v); err != nil {
		return reasonOf(err)
	}
	switch e.Kind() {
	case KindList:
		return "takes a list"
	case KindMap, KindObject:
		return "takes a mapping"
	}
	return "takes another kind of value"
}

// Plain text of a stored value for a notice
func storedText(v any) string {
	switch t := v.(type) {
	case nil:
		return "nothing"
	case string:
		return fmt.Sprintf("%q", t)
	case []any:
		return "a list"
	case map[string]any:
		return "a mapping"
	}
	return fmt.Sprint(v)
}

// Plain text of the default a setting falls back to
func defaultText(e Entry) string {
	row := Row{Value: e.DefaultValue()}
	schemaOf(&row, e)
	return valueText(row)
}

// Tells the person once about every stored value of a home a setting does not take: where it is, the key, the value, what the setting takes and what is used instead
func tellMisfits(ctx context.Context, h string, l *layers) {
	tell := func(place string) func(e Entry, key string, v any) {
		return func(e Entry, key string, v any) {
			eachMisfit(e, key, config.Expand(v, config.EnvRefLookupContext(ctx)), func(part string, bad any, field Entry) {
				shown := storedText(bad)
				if !firstNotice(h, place, part, shown) {
					return
				}
				switch {
				case field == nil:
					pkgLog().Warn(place + " holds " + part + ", which is not a setting; it is ignored")
				case part == e.Info().Key:
					pkgLog().Warn(fmt.Sprintf("%s has %s set to %s, but it %s. Using the default (%s) instead.", place, part, shown, takes(field, bad), defaultText(e)))
				default:
					pkgLog().Warn(fmt.Sprintf("%s has %s set to %s, but it %s. It is ignored.", place, part, shown, takes(field, bad)))
				}
			})
		}
	}
	walkStored(l.doc, func(e Entry, key string, v any) {
		if !e.Info().State {
			tell(storePlace())(e, key, v)
		}
	}, nil)
	walkStored(l.state, func(e Entry, key string, v any) {
		if e.Info().State {
			tell(stateFile)(e, key, v)
		}
	}, nil)
	walkStored(l.managed, tell(managedPlace), nil)
}

// Settings that store nothing of their own and write the keys they stand for instead
var derivedEntries = sync.OnceValue(func() []Entry {
	var out []Entry
	for _, e := range All() {
		if m := e.Info(); m.Derive != nil && m.Companions != nil {
			out = append(out, e)
		}
	}
	return out
})

// Values a derived setting written by hand in a config document gives the keys it stands for, so the written word applies wherever those keys are not set themselves
func derivedCompanions(ctx context.Context, doc map[string]any) map[string]any {
	var out map[string]any
	for _, e := range derivedEntries() {
		m := e.Info()
		raw, set := storedValue(ctx, doc, m.Key)
		if !set || raw == nil {
			continue
		}
		value, err := e.Coerce(raw)
		if err != nil || value == nil || (m.Validate != nil && m.Validate(value) != nil) {
			continue
		}
		for _, kv := range m.Companions(value) {
			if out == nil {
				out = map[string]any{}
			}
			out[kv.Key] = kv.Value
		}
	}
	return out
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
	if v, ok := l.derived[m.Key]; ok && !m.State {
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

// Typed value of a stored value, the default when it does not fit; tellMisfits has told the person about the misfit
func typedOr(e Entry, v any) any {
	if typed, ok := readPart(e, v); ok {
		return typed
	}
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
	pin, whole, pinned := managedPin(l, e)
	if pinned && whole {
		return Value{Key: m.Key, Value: pin, Source: SourceManaged, Locked: true}
	}
	v := resolveUnpinned(ctx, e, l)
	if pinned {
		v.Value, v.Source = config.DeepMerge(config.AsMap(v.Value), pin.(map[string]any)), SourceManaged
	}
	return v
}

// Value of a setting from every layer under the administrator's pins: env override, session, platform, global, default
func resolveUnpinned(ctx context.Context, e Entry, l *layers) Value {
	m := e.Info()
	if _, isSecret := e.(*Secret); !isSecret && m.Env != "" {
		if v, ok := os.LookupEnv(m.Env); ok && strings.TrimSpace(v) != "" {
			coerced, err := e.Coerce(v)
			if err == nil {
				return Value{Key: m.Key, Value: coerced, Source: SourceEnv}
			}
			if firstNotice("env", m.Env, v) {
				pkgLog().Warn(fmt.Sprintf("The %s environment variable is set to %q, but %s %s. It is ignored.", m.Env, v, m.Key, reasonOf(err)))
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

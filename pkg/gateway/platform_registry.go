package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Builds an adapter for a platform config
type AdapterFactory func(ctx context.Context, cfg *PlatformConfig) (any, error)

// Dashboard check of one env value with the 400 detail returned when a non empty value fails it
type EnvValueRule struct {
	Check   func(value string) bool
	Message string
}

// Metadata and factory of one platform adapter, py tags naming the keyword each field takes
type PlatformEntry struct {
	Name                       string                                                                                                                       `py:"name"`
	Label                      string                                                                                                                       `py:"label"`
	AdapterFactory             AdapterFactory                                                                                                               `py:"adapter_factory"`
	CheckFn                    func() bool                                                                                                                  `py:"check_fn"`
	ScopedCheckFn              func(ctx context.Context) bool                                                                                               `py:"scoped_check_fn"`
	ValidateConfig             func(cfg *PlatformConfig) (bool, error)                                                                                      `py:"validate_config"`
	ScopedValidateConfig       func(ctx context.Context, cfg *PlatformConfig) (bool, error)                                                                 `py:"scoped_validate_config"`
	EnsureDepsFn               func(ctx context.Context) (bool, error)                                                                                      `py:"ensure_deps_fn"`
	IsConnected                func(ctx context.Context, cfg *PlatformConfig) (bool, error)                                                                 `py:"is_connected"`
	RequiredEnv                []string                                                                                                                     `py:"required_env"`
	InstallHint                string                                                                                                                       `py:"install_hint"`
	SetupFn                    func(ctx context.Context) error                                                                                              `py:"setup_fn"`
	Source                     string                                                                                                                       `py:"source"`
	PluginName                 string                                                                                                                       `py:"plugin_name"`
	AllowedUsersEnv            string                                                                                                                       `py:"allowed_users_env"`
	AllowAllEnv                string                                                                                                                       `py:"allow_all_env"`
	MaxMessageLength           int                                                                                                                          `py:"max_message_length"`
	PIISafe                    bool                                                                                                                         `py:"pii_safe"`
	Emoji                      string                                                                                                                       `py:"emoji"`
	AllowUpdateCommand         bool                                                                                                                         `py:"allow_update_command"`
	PlatformHint               string                                                                                                                       `py:"platform_hint"`
	EnvEnablementFn            func(ctx context.Context) map[string]any                                                                                     `py:"env_enablement_fn"`
	ApplyYAMLConfigFn          func(ctx context.Context, yamlCfg, block map[string]any) (map[string]any, error)                                             `py:"apply_yaml_config_fn"`
	CronDeliverEnvVar          string                                                                                                                       `py:"cron_deliver_env_var"`
	ParseTargetRefFn           func(ref string) (chatID string, threadID *string, ok bool)                                                                  `py:"parse_target_ref_fn"`
	ValidateTargetRefFn        func(ref string) any                                                                                                         `py:"validate_target_ref_fn"`
	SendMessageHandler         func(ctx context.Context, args map[string]any, chatID, platformName string, pconfig *PlatformConfig) (map[string]any, error) `py:"send_message_handler"`
	StandaloneSenderFn         func(ctx context.Context, kwargs map[string]any) (map[string]any, error)                                                     `py:"standalone_sender_fn"`
	TrustedInbound             bool                                                                                                                         `py:"trusted_inbound"`
	DisplayTier                string                                                                                                                       `py:"display_tier"`
	DisplayOverrides           map[string]any                                                                                                               `py:"display_overrides"`
	SharedEnvPrefixes          []string                                                                                                                     `py:"shared_env_prefixes"`
	ConfigConnectedFn          func(cfg *PlatformConfig) bool                                                                                               `py:"config_connected_fn"`
	AllowBotsEnv               string                                                                                                                       `py:"allow_bots_env"`
	OwnPolicyEnv               []string                                                                                                                     `py:"own_policy_env"`
	GroupAllowedChatsEnv       string                                                                                                                       `py:"group_allowed_chats_env"`
	AllowlistSuppressesPairing bool                                                                                                                         `py:"allowlist_suppresses_pairing"`
	TargetRefSyntaxFn          func(ref string) any                                                                                                         `py:"target_ref_syntax_fn"`
	StandaloneSend             *StandaloneSendPolicy                                                                                                        `py:"standalone_send"`
	DashboardLabel             string                                                                                                                       `py:"dashboard_label"`
	DashboardDescription       string                                                                                                                       `py:"dashboard_description"`
	DocsURL                    string                                                                                                                       `py:"docs_url"`
	DashboardEnvVars           []string                                                                                                                     `py:"dashboard_env_vars"`
	DashboardRequiredEnv       []string                                                                                                                     `py:"dashboard_required_env"`
	EnvValueRules              map[string]EnvValueRule                                                                                                      `py:"env_value_rules"`
	EnvOverrides               []EnvStep                                                                                                                    `py:"env_overrides"`
	EnvEnableCredentials       []string                                                                                                                     `py:"env_enable_credentials"`
}

// Result of the entry's check, the scoped check reading the caller's profile secret scope when declared
func (e *PlatformEntry) RunCheck(ctx context.Context) bool {
	if e.ScopedCheckFn != nil {
		return e.ScopedCheckFn(ctx)
	}
	return e.CheckFn()
}

// Whether the entry declares a config validator, scoped or plain
func (e *PlatformEntry) HasValidateConfig() bool {
	return e.ScopedValidateConfig != nil || e.ValidateConfig != nil
}

// Result of the entry's config validator, the scoped one reading the caller's profile secret scope when declared
func (e *PlatformEntry) RunValidateConfig(ctx context.Context, cfg *PlatformConfig) (bool, error) {
	if e.ScopedValidateConfig != nil {
		return e.ScopedValidateConfig(ctx, cfg)
	}
	return e.ValidateConfig(cfg)
}

// Entry with the dataclass defaults, plugin source, plug emoji and the update command allowed
func NewPlatformEntry(name, label string, factory AdapterFactory, check func() bool) *PlatformEntry {
	return &PlatformEntry{Name: name, Label: label, AdapterFactory: factory, CheckFn: check, RequiredEnv: []string{},
		Source: "plugin", Emoji: "\U0001F50C", AllowUpdateCommand: true, DisplayOverrides: map[string]any{},
		EnvValueRules: map[string]EnvValueRule{}}
}

// Sets entry fields from keyword arguments, TypeError for an unknown keyword or a value of the wrong type
func (e *PlatformEntry) SetKeywords(kwargs map[string]any) error {
	v := reflect.ValueOf(e).Elem()
	fields := map[string]int{}
	for i := 0; i < v.NumField(); i++ {
		fields[v.Type().Field(i).Tag.Get("py")] = i
	}
	keys := make([]string, 0, len(kwargs))
	for k := range kwargs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		i, ok := fields[k]
		if !ok {
			return &PyError{"TypeError", fmt.Sprintf("PlatformEntry.__init__() got an unexpected keyword argument '%s'", k)}
		}
		f := v.Field(i)
		val := kwargs[k]
		if val == nil {
			f.Set(reflect.Zero(f.Type()))
			continue
		}
		rv := reflect.ValueOf(val)
		switch {
		case rv.Type().AssignableTo(f.Type()):
			f.Set(rv)
		case f.Type() == reflect.TypeOf([]string(nil)) && rv.Kind() == reflect.Slice:
			out := make([]string, rv.Len())
			for j := range out {
				s, ok := rv.Index(j).Interface().(string)
				if !ok {
					return &PyError{"TypeError", fmt.Sprintf("PlatformEntry %s items must be strings", k)}
				}
				out[j] = s
			}
			f.Set(reflect.ValueOf(out))
		case f.Type() == reflect.TypeOf(e.ValidateConfig) && rv.Type() == reflect.TypeOf(e.ConfigConnectedFn):
			fn := val.(func(*PlatformConfig) bool)
			f.Set(reflect.ValueOf(func(cfg *PlatformConfig) (bool, error) { return fn(cfg), nil }))
		case f.Type() == reflect.TypeOf(e.IsConnected) && rv.Type() == reflect.TypeOf(e.ValidateConfig):
			fn := val.(func(*PlatformConfig) (bool, error))
			f.Set(reflect.ValueOf(func(_ context.Context, cfg *PlatformConfig) (bool, error) { return fn(cfg) }))
		case f.Type() == reflect.TypeOf(e.IsConnected) && rv.Type() == reflect.TypeOf(e.ConfigConnectedFn):
			fn := val.(func(*PlatformConfig) bool)
			f.Set(reflect.ValueOf(func(_ context.Context, cfg *PlatformConfig) (bool, error) { return fn(cfg), nil }))
		default:
			return &PyError{"TypeError", fmt.Sprintf("PlatformEntry %s takes %s, not %T", k, f.Type(), val)}
		}
	}
	return nil
}

// Whether core ships an adapter for the name, a static member or a bundled platform plugin
func CoreShipsPlatform(name any) bool {
	s, _ := name.(string)
	value := pystr.Lower(pystr.Strip(s))
	if BuiltinPlatformValues[value] {
		return true
	}
	names, _ := scanBundledPluginPlatforms()
	return names[value]
}

// Owner scope and platform name of a deferred load, an empty scope meaning process global
type loadKey struct{ scope, name string }

// Context key of the load keys whose loader runs in the current flow
type loadingCtxKey struct{ r *PlatformRegistry }

// Central registry of platform adapters, plugin entries isolated per profile home and loaded lazily
type PlatformRegistry struct {
	mu                sync.Mutex
	entries           *pydict.Dict[string, *PlatformEntry]
	scopedEntries     map[string]*pydict.Dict[string, *PlatformEntry]
	deferred          map[string]*plugins.DeferredPlatformLoader
	scopedDeferred    map[string]map[string]*plugins.DeferredPlatformLoader
	inflight          map[loadKey]chan struct{}
	inflightLoaders   map[loadKey]*plugins.DeferredPlatformLoader
	cancelledInflight map[loadKey]bool
	consumedLoaders   map[loadKey]*plugins.DeferredPlatformLoader
	activationEnv     map[string]map[string]bool
}

// Empty registry
func NewPlatformRegistry() *PlatformRegistry {
	return &PlatformRegistry{
		entries: pydict.New[string, *PlatformEntry](), scopedEntries: map[string]*pydict.Dict[string, *PlatformEntry]{},
		deferred: map[string]*plugins.DeferredPlatformLoader{}, scopedDeferred: map[string]map[string]*plugins.DeferredPlatformLoader{},
		inflight: map[loadKey]chan struct{}{}, inflightLoaders: map[loadKey]*plugins.DeferredPlatformLoader{},
		cancelledInflight: map[loadKey]bool{}, consumedLoaders: map[loadKey]*plugins.DeferredPlatformLoader{},
		activationEnv: map[string]map[string]bool{},
	}
}

// Process wide platform registry
var Registry = NewPlatformRegistry()

// Scope key of the home in ctx
func CurrentScopeKey(ctx context.Context) string { return home.Key(home.Dir(ctx)) }

// Entry and loader maps of a scope, created on demand when asked
func (r *PlatformRegistry) scopeMaps(scope string, create bool) (*pydict.Dict[string, *PlatformEntry], map[string]*plugins.DeferredPlatformLoader) {
	if scope == "" {
		return r.entries, r.deferred
	}
	if create {
		if r.scopedEntries[scope] == nil {
			r.scopedEntries[scope] = pydict.New[string, *PlatformEntry]()
		}
		if r.scopedDeferred[scope] == nil {
			r.scopedDeferred[scope] = map[string]*plugins.DeferredPlatformLoader{}
		}
	}
	return r.scopedEntries[scope], r.scopedDeferred[scope]
}

// Entry and loader of a name, the loader falling back to in flight then consumed
func (r *PlatformRegistry) registrationState(scope, name string, create bool) (*PlatformEntry, *plugins.DeferredPlatformLoader) {
	entries, deferred := r.scopeMaps(scope, create)
	entry, _ := entries.Get(name)
	loader := deferred[name]
	if entry == nil && loader == nil {
		key := loadKey{scope, name}
		if loader = r.inflightLoaders[key]; loader == nil {
			loader = r.consumedLoaders[key]
		}
	}
	return entry, loader
}

// Drops empty maps of a profile scope
func (r *PlatformRegistry) pruneScope(scope string) {
	if scope == "" {
		return
	}
	if r.scopedEntries[scope].Len() == 0 {
		delete(r.scopedEntries, scope)
	}
	if len(r.scopedDeferred[scope]) == 0 {
		delete(r.scopedDeferred, scope)
	}
}

// Registers a lazy loader run at most once on first lookup, a concrete registration dropping it
func (r *PlatformRegistry) RegisterDeferred(name string, loader *plugins.DeferredPlatformLoader, scope string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entries, deferred := r.scopeMaps(scope, true)
	delete(r.consumedLoaders, loadKey{scope, name})
	if !entries.Has(name) {
		deferred[name] = loader
	}
}

// Records the env names a deferred platform's manifest declares
func (r *PlatformRegistry) NoteActivationEnv(name string, envNames []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.activationEnv[name]
	if set == nil {
		set = map[string]bool{}
		r.activationEnv[name] = set
	}
	for _, n := range envNames {
		if s := pystr.Strip(n); s != "" {
			set[s] = true
		}
	}
}

// Env names recorded for a deferred platform
func (r *PlatformRegistry) ActivationEnv(name string) map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]bool{}
	for n := range r.activationEnv[name] {
		out[n] = true
	}
	return out
}

// Concrete and deferred state of a name without resolving it
func (r *PlatformRegistry) SnapshotRegistration(name, scope string) (*PlatformEntry, *plugins.DeferredPlatformLoader) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.registrationState(scope, name, false)
}

// Restores a previous registration when the full state is still the current one
func (r *PlatformRegistry) RestoreRegistration(name string, curEntry *PlatformEntry, curLoader *plugins.DeferredPlatformLoader,
	prevEntry *PlatformEntry, prevLoader *plugins.DeferredPlatformLoader, scope string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, loader := r.registrationState(scope, name, true)
	if entry != curEntry || loader != curLoader {
		return false
	}
	key := loadKey{scope, name}
	entries, deferred := r.scopeMaps(scope, false)
	if prevEntry == nil {
		entries.Delete(name)
	} else {
		entries.Set(name, prevEntry)
	}
	if prevLoader == nil {
		delete(deferred, name)
	} else {
		deferred[name] = prevLoader
	}
	if _, ok := r.inflight[key]; ok {
		r.cancelledInflight[key] = true
	}
	delete(r.consumedLoaders, key)
	r.pruneScope(scope)
	return true
}

// Load keys running in the flow of ctx
func (r *PlatformRegistry) loading(ctx context.Context) map[loadKey]bool {
	m, _ := ctx.Value(loadingCtxKey{r}).(map[loadKey]bool)
	return m
}

// Runs the pending deferred loader of a name, waiting on a concurrent load of the same key
func (r *PlatformRegistry) resolve(ctx context.Context, name, scope string) {
	r.mu.Lock()
	activeScope := scope
	if activeScope == "" {
		activeScope = CurrentScopeKey(ctx)
	}
	entries, deferred := r.scopeMaps(activeScope, false)
	scopedKey, globalKey := loadKey{activeScope, name}, loadKey{"", name}
	event, hasEvent := r.inflight[scopedKey]
	key := scopedKey
	loading := r.loading(ctx)
	inEntries := entries.Has(name)
	if !hasEvent && !inEntries && len(loading) > 0 && plugins.InPluginLoadWorker(ctx) {
		r.mu.Unlock()
		return
	}
	var loader *plugins.DeferredPlatformLoader
	if !hasEvent && !inEntries {
		loader = deferred[name]
		delete(deferred, name)
	}
	if !hasEvent && loader == nil && !inEntries {
		key = globalKey
		if event, hasEvent = r.inflight[globalKey]; !hasEvent {
			loader = r.deferred[name]
			delete(r.deferred, name)
		}
	}
	isLoader := false
	if !hasEvent && loader != nil {
		event, hasEvent, isLoader = make(chan struct{}), true, true
		r.inflight[key] = event
		r.inflightLoaders[key] = loader
	}
	if !hasEvent {
		r.mu.Unlock()
		return
	}
	if !isLoader && loading[key] {
		r.mu.Unlock()
		slog.Warn(fmt.Sprintf("Deferred platform '%s' recursively requested while loading", name))
		return
	}
	r.mu.Unlock()
	if !isLoader {
		<-event
		r.resolve(ctx, name, activeScope)
		return
	}
	if r.runLoader(ctx, name, key, loader, loading, event) {
		r.resolve(ctx, name, activeScope)
	}
}

// Runs one loader with its key marked loading, then settles the in flight state, true when teardown cancelled it
func (r *PlatformRegistry) runLoader(ctx context.Context, name string, key loadKey, loader *plugins.DeferredPlatformLoader,
	loading map[loadKey]bool, event chan struct{}) (wasCancelled bool) {
	nested := make(map[loadKey]bool, len(loading)+1)
	for k := range loading {
		nested[k] = true
	}
	nested[key] = true
	defer func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		wasCancelled = r.cancelledInflight[key]
		entries, deferred := r.scopeMaps(key.scope, false)
		inEntries := entries.Has(name)
		_, inDeferred := deferred[name]
		if !wasCancelled && !inEntries && !inDeferred {
			r.consumedLoaders[key] = loader
		}
		delete(r.inflight, key)
		delete(r.inflightLoaders, key)
		delete(r.cancelledInflight, key)
		close(event)
	}()
	loader.Load(context.WithValue(ctx, loadingCtxKey{r}, nested))
	return false
}

// Whether ownership teardown cancelled an in flight loader
func (r *PlatformRegistry) IsDeferredLoadCancelled(name, scope string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelledInflight[loadKey{scope, name}]
}

// Deferred or in flight names of a scope, scoped first, without loading them
func (r *PlatformRegistry) pendingNames(activeScope string) []string {
	r.mu.Lock()
	_, scopedDeferred := r.scopeMaps(activeScope, false)
	scoped, global := map[string]bool{}, map[string]bool{}
	for n := range scopedDeferred {
		scoped[n] = true
	}
	for n := range r.deferred {
		global[n] = true
	}
	for k := range r.inflight {
		if k.scope == activeScope {
			scoped[k.name] = true
		} else if k.scope == "" {
			global[k.name] = true
		}
	}
	r.mu.Unlock()
	out := sortedSet(scoped)
	for _, n := range sortedSet(global) {
		if !scoped[n] {
			out = append(out, n)
		}
	}
	return out
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Runs every pending loader of the current scope, or only those wanted accepts
func (r *PlatformRegistry) resolveAll(ctx context.Context, wanted func(string) bool) {
	activeScope := CurrentScopeKey(ctx)
	for _, name := range r.pendingNames(activeScope) {
		if wanted == nil || wanted(name) {
			r.resolve(ctx, name, activeScope)
		}
	}
}

// Deferred names of the current scope that have not loaded yet
func (r *PlatformRegistry) PendingNames(ctx context.Context) []string {
	return r.pendingNames(CurrentScopeKey(ctx))
}

// Global entries overlaid with the scope's entries in registration order
func (r *PlatformRegistry) visibleEntries(scope string) []*PlatformEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := pydict.Merge(r.entries, r.scopedEntries[scope]).Values()
	if out == nil {
		out = []*PlatformEntry{}
	}
	return out
}

// Loaded entries after materializing only the deferred platforms wanted accepts
func (r *PlatformRegistry) ConfiguredEntries(ctx context.Context, wanted func(string) bool) []*PlatformEntry {
	r.resolveAll(ctx, wanted)
	return r.visibleEntries(CurrentScopeKey(ctx))
}

// Entries already materialized, never running a deferred loader
func (r *PlatformRegistry) LoadedEntries(ctx context.Context) []*PlatformEntry {
	return r.visibleEntries(CurrentScopeKey(ctx))
}

// Module path of a Go function value, empty for anything else
func callableModule(fn any) string {
	v := reflect.ValueOf(fn)
	if !v.IsValid() || v.Kind() != reflect.Func || v.IsNil() {
		return ""
	}
	f := runtime.FuncForPC(v.Pointer())
	if f == nil {
		return ""
	}
	return funcModule(f.Name())
}

// Package path part of a qualified Go function name
func funcModule(full string) string {
	slash := strings.LastIndex(full, "/")
	if dot := strings.Index(full[slash+1:], "."); dot >= 0 {
		return full[:slash+1+dot]
	}
	return full
}

// Plugin scope the tool registry binds a module to
func pluginScopeForModule(ctx context.Context, module string) (string, error) {
	if module == "" {
		return "", nil
	}
	return tools.Default.PluginScopeForModule(home.Dir(ctx), module)
}

// Plugin scope of the module that defined a callable registered outside the plugin context
func pluginScopeFromCallable(ctx context.Context, fn any) (string, error) {
	return pluginScopeForModule(ctx, callableModule(fn))
}

// Plugin scope of the code that called the registry method
func callerPluginScope(ctx context.Context) (string, error) {
	pc, _, _, ok := runtime.Caller(2)
	if !ok {
		return "", nil
	}
	f := runtime.FuncForPC(pc)
	if f == nil {
		return "", nil
	}
	return pluginScopeForModule(ctx, funcModule(f.Name()))
}

// Registers an entry, last writer wins, a plugin entry's scope inferred from its code when not given
func (r *PlatformRegistry) Register(ctx context.Context, entry *PlatformEntry, scope string) error {
	if scope == "" && entry.Source == "plugin" {
		var err error
		if scope, err = callerPluginScope(ctx); err != nil {
			return err
		}
		if scope == "" {
			if scope, err = pluginScopeFromCallable(ctx, entry.AdapterFactory); err != nil {
				return err
			}
		}
		if scope == "" {
			if scope, err = pluginScopeFromCallable(ctx, entry.CheckFn); err != nil {
				return err
			}
		}
	}
	r.register(entry, scope)
	return nil
}

// Stores an entry in a known scope, dropping any pending loader of the name
func (r *PlatformRegistry) register(entry *PlatformEntry, scope string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entries, deferred := r.scopeMaps(scope, true)
	delete(r.consumedLoaders, loadKey{scope, entry.Name})
	delete(deferred, entry.Name)
	if prev, _ := entries.Get(entry.Name); prev != nil {
		slog.Info(fmt.Sprintf("Platform '%s' re-registered (was %s, now %s)", entry.Name, prev.Source, entry.Source))
	}
	entries.Set(entry.Name, entry)
	slog.Debug(fmt.Sprintf("Registered platform adapter: %s (%s)", entry.Name, entry.Source))
}

// Removes an entry, true when it existed, the scope inferred from the caller when not given
func (r *PlatformRegistry) Unregister(ctx context.Context, name, scope string) (bool, error) {
	inferred := scope
	if inferred == "" {
		var err error
		if inferred, err = callerPluginScope(ctx); err != nil {
			return false, err
		}
	}
	return r.unregister(ctx, name, inferred), nil
}

// Removes an entry from the inferred or current scope, else from the process wide maps
func (r *PlatformRegistry) unregister(ctx context.Context, name, inferred string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	active := inferred
	if active == "" {
		active = CurrentScopeKey(ctx)
	}
	entries, deferred := r.scopeMaps(active, false)
	inEntries := entries.Has(name)
	_, inDeferred := deferred[name]
	if inferred != "" || inEntries || inDeferred {
		delete(deferred, name)
		entries.Delete(name)
		r.pruneScope(active)
		return inEntries
	}
	delete(r.deferred, name)
	return r.entries.Delete(name)
}

// Whether a lookup must run or await a deferred loader, lock held
func (r *PlatformRegistry) loadPending(scope, name string) bool {
	_, deferred := r.scopeMaps(scope, false)
	_, inScoped := deferred[name]
	inGlobalEntries := r.entries.Has(name)
	_, inGlobalDeferred := r.deferred[name]
	_, scopedInflight := r.inflight[loadKey{scope, name}]
	_, globalInflight := r.inflight[loadKey{"", name}]
	return inScoped || (!inGlobalEntries && inGlobalDeferred) || scopedInflight || globalInflight
}

// Entry of a name, running its deferred loader first when one is pending
func (r *PlatformRegistry) Get(ctx context.Context, name string) *PlatformEntry {
	scope := CurrentScopeKey(ctx)
	r.mu.Lock()
	entries, _ := r.scopeMaps(scope, false)
	have := entries.Has(name)
	needsResolve := !have && r.loadPending(scope, name)
	r.mu.Unlock()
	if needsResolve {
		r.resolve(ctx, name, scope)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entries, _ = r.scopeMaps(scope, false)
	if e, _ := entries.Get(name); e != nil {
		return e
	}
	e, _ := r.entries.Get(name)
	return e
}

// Every registered entry, loading every deferred platform
func (r *PlatformRegistry) AllEntries(ctx context.Context) []*PlatformEntry {
	r.resolveAll(ctx, nil)
	return r.visibleEntries(CurrentScopeKey(ctx))
}

// Entries whose source is a plugin
func (r *PlatformRegistry) PluginEntries(ctx context.Context) []*PlatformEntry {
	out := []*PlatformEntry{}
	for _, e := range r.AllEntries(ctx) {
		if e.Source == "plugin" {
			out = append(out, e)
		}
	}
	return out
}

// Concrete and deferred names of the current scope and process wide, never loading an adapter
func (r *PlatformRegistry) RegisteredNames(ctx context.Context) map[string]bool {
	return r.registeredNamesIn(CurrentScopeKey(ctx))
}

func (r *PlatformRegistry) registeredNamesIn(scope string) map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]bool{}
	entries, deferred := r.scopeMaps(scope, false)
	for _, m := range []*pydict.Dict[string, *PlatformEntry]{entries, r.entries} {
		for n := range m.All() {
			out[n] = true
		}
	}
	for _, m := range []map[string]*plugins.DeferredPlatformLoader{deferred, r.deferred} {
		for n := range m {
			out[n] = true
		}
	}
	return out
}

// Required env of every loaded entry of the current scope and process wide
func (r *PlatformRegistry) RequiredEnvNames(ctx context.Context) map[string]bool {
	return r.requiredEnvNamesIn(CurrentScopeKey(ctx))
}

func (r *PlatformRegistry) requiredEnvNamesIn(scope string) map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]bool{}
	entries, _ := r.scopeMaps(scope, false)
	for _, m := range []*pydict.Dict[string, *PlatformEntry]{r.entries, entries} {
		for _, e := range m.All() {
			for _, n := range e.RequiredEnv {
				out[n] = true
			}
		}
	}
	return out
}

// Whether a name is registered or pending, never loading the adapter
func (r *PlatformRegistry) IsRegistered(ctx context.Context, name string) bool {
	scope := CurrentScopeKey(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	entries, _ := r.scopeMaps(scope, false)
	inScoped := entries.Has(name)
	inGlobal := r.entries.Has(name)
	return inScoped || inGlobal || r.loadPending(scope, name)
}

// Adapter of a platform, nil when no entry exists, deps are missing, validation fails or the factory errors
func (r *PlatformRegistry) CreateAdapter(ctx context.Context, name string, cfg *PlatformConfig) any {
	entry := r.Get(ctx, name)
	if entry == nil {
		return nil
	}
	probe := func(fn func() (bool, error), failureMsg string) (ok bool) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Warn(fmt.Sprintf(failureMsg, entry.Label, pyrepr.Str(rec)))
				ok = false
			}
		}()
		got, err := fn()
		if err != nil {
			slog.Warn(fmt.Sprintf(failureMsg, entry.Label, err))
			return false
		}
		return got
	}
	depsOK := probe(func() (bool, error) {
		if entry.CheckFn == nil {
			return false, errors.New("'NoneType' object is not callable")
		}
		return entry.RunCheck(ctx), nil
	}, "Platform '%s' check_fn raised: %s")
	if !depsOK && entry.EnsureDepsFn != nil {
		slog.Info(fmt.Sprintf("Platform '%s' dependencies missing — attempting install...", entry.Label))
		depsOK = probe(func() (bool, error) { return entry.EnsureDepsFn(ctx) }, "Platform '%s' dependency install raised: %s")
	}
	if !depsOK {
		hint := ""
		if entry.InstallHint != "" {
			hint = " (" + entry.InstallHint + ")"
		}
		slog.Warn(fmt.Sprintf("Platform '%s' requirements not met%s", entry.Label, hint))
		return nil
	}
	if entry.HasValidateConfig() {
		valid, err := guardedCall(func() (bool, error) { return entry.RunValidateConfig(ctx, cfg) })
		if err != nil {
			slog.Warn(fmt.Sprintf("Platform '%s' config validation error: %s", entry.Label, err))
			return nil
		}
		if !valid {
			slog.Warn(fmt.Sprintf("Platform '%s' config validation failed", entry.Label))
			return nil
		}
	}
	adapter, err := guardedCall(func() (any, error) {
		if entry.AdapterFactory == nil {
			return nil, errors.New("'NoneType' object is not callable")
		}
		return entry.AdapterFactory(ctx, cfg)
	})
	if err != nil {
		slog.Error(fmt.Sprintf("Failed to create adapter for platform '%s': %s", entry.Label, err))
		return nil
	}
	return adapter
}

// Result of fn with a panic returned as its error the way a raising Python call surfaces
func guardedCall[T any](fn func() (T, error)) (out T, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			var zero T
			out, err = zero, errors.New(pyrepr.Str(rec))
		}
	}()
	return fn()
}

// Converts a plugin's register_platform keywords into Go field values, installed by the process wiring over the hosted platform package
var HostedEntryKeywords func(name string, kwargs map[string]any) (map[string]any, error)

// Whether a keyword value holds a callable or object living in the plugin host
func containsHosted(v any) bool {
	switch x := v.(type) {
	case *plugins.HostedCallable, *plugins.HostedObject:
		return true
	case []any:
		for _, item := range x {
			if containsHosted(item) {
				return true
			}
		}
	case map[string]any:
		for _, item := range x {
			if containsHosted(item) {
				return true
			}
		}
	}
	return false
}

// The registry as the plugin manager's platform service, scopes given as home paths
type platformRegistryService struct{ r *PlatformRegistry }

// Platform registry service the wiring installs into plugins.Services.Platforms
func PlatformRegistryService() plugins.PlatformRegistry { return platformRegistryService{Registry} }

// Scope key of a home path, empty staying process global
func serviceScope(scope string) string {
	if scope == "" {
		return ""
	}
	return home.Key(scope)
}

func (s platformRegistryService) CoreShipsPlatform(name string) bool { return CoreShipsPlatform(name) }

func (s platformRegistryService) NewEntry(spec plugins.PlatformSpec) (any, error) {
	e := NewPlatformEntry(spec.Name, spec.Label, nil, spec.Check)
	kwargs := map[string]any{"adapter_factory": spec.AdapterFactory, "validate_config": spec.ValidateConfig,
		"required_env": spec.RequiredEnv, "install_hint": spec.InstallHint}
	for k, v := range spec.Extra {
		kwargs[k] = v
	}
	if containsHosted(kwargs) {
		if HostedEntryKeywords == nil {
			return nil, &PyError{"RuntimeError", "hosted platform keyword conversion is not wired"}
		}
		converted, err := HostedEntryKeywords(spec.Name, kwargs)
		if err != nil {
			return nil, err
		}
		kwargs = converted
	}
	if err := e.SetKeywords(kwargs); err != nil {
		return nil, err
	}
	return e, nil
}

func (s platformRegistryService) Snapshot(name, scope string) plugins.PlatformSlot {
	e, l := s.r.SnapshotRegistration(name, serviceScope(scope))
	if e == nil {
		return plugins.PlatformSlot{Loader: l}
	}
	return plugins.PlatformSlot{Entry: e, Loader: l}
}

func (s platformRegistryService) Register(entry any, scope string) {
	s.r.register(entry.(*PlatformEntry), serviceScope(scope))
}

func (s platformRegistryService) RegisterDeferred(name string, loader *plugins.DeferredPlatformLoader, scope string) {
	s.r.RegisterDeferred(name, loader, serviceScope(scope))
}

func (s platformRegistryService) NoteActivationEnv(name string, env []string) {
	s.r.NoteActivationEnv(name, env)
}

func (s platformRegistryService) IsDeferredLoadCancelled(name, scope string) bool {
	return s.r.IsDeferredLoadCancelled(name, serviceScope(scope))
}

func (s platformRegistryService) Restore(name string, current, previous plugins.PlatformSlot, scope string) bool {
	ce, _ := current.Entry.(*PlatformEntry)
	pe, _ := previous.Entry.(*PlatformEntry)
	return s.r.RestoreRegistration(name, ce, current.Loader, pe, previous.Loader, serviceScope(scope))
}

func (s platformRegistryService) Unregister(name string) {
	ctx := context.Background()
	inferred, err := callerPluginScope(ctx)
	if err != nil {
		panic(err)
	}
	s.r.unregister(ctx, name, inferred)
}

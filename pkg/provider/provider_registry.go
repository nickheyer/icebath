package provider

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"runtime/debug"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Python TypeError a provider registry raises for an object that is not an instance of its provider class
type ProviderTypeError struct{ Message string }

func (e *ProviderTypeError) Error() string { return e.Message }

// Registry key of a name stripped of whitespace and lowercased for case insensitive registries
func LowerKey(name string) string { return pystr.Lower(pystr.Strip(name)) }

// Construction options of a provider registry, Normalize defaulting to whitespace stripping
type ProviderRegistryOptions struct {
	Label              string
	Logger             *slog.Logger
	Normalize          func(string) string
	BuiltinNames       map[string]bool
	OnBuiltinCollision func(key string) error
}

// Cache fingerprint of a registry, the global generation and the active scope generation
type ProviderRegistryGeneration struct {
	Global int
	Scoped int
}

// Global and per profile scope provider maps with plugin snapshot and restore support
type ProviderRegistry[P NamedProvider] struct {
	Label              string
	ProviderCls        string
	Logger             *slog.Logger
	Normalize          func(string) string
	BuiltinNames       map[string]bool
	onBuiltinCollision func(string) error
	providers          *pydict.Dict[string, P]
	scopedProviders    *pydict.Dict[string, *pydict.Dict[string, P]]
	generation         int
	scopedGenerations  map[string]int
	mu                 sync.Mutex
	logLabel           string
}

// Registry for providers of type P, an all uppercase label keeping its case in log lines
func NewProviderRegistry[P NamedProvider](opts ProviderRegistryOptions) *ProviderRegistry[P] {
	if opts.Label == "" {
		panic("string index out of range")
	}
	r := &ProviderRegistry[P]{
		Label: opts.Label, ProviderCls: providerClassName[P](), Logger: opts.Logger, Normalize: opts.Normalize,
		BuiltinNames: opts.BuiltinNames, onBuiltinCollision: opts.OnBuiltinCollision,
		providers: pydict.New[string, P](), scopedProviders: pydict.New[string, *pydict.Dict[string, P]](),
		scopedGenerations: map[string]int{},
	}
	if r.Logger == nil {
		r.Logger = slog.Default()
	}
	if r.Normalize == nil {
		r.Normalize = pystr.Strip
	}
	if r.BuiltinNames == nil {
		r.BuiltinNames = map[string]bool{}
	}
	r.logLabel = opts.Label
	if !pystr.IsUpper(opts.Label) {
		first, size := pystr.DecodeRune(opts.Label)
		r.logLabel = pystr.Lower(string(first)) + opts.Label[size:]
	}
	return r
}

// Python class name of the provider type, pointer types named by their element
func providerClassName[P any]() string {
	t := reflect.TypeFor[P]()
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Name() != "" {
		return t.Name()
	}
	return t.String()
}

// Slot map of a normalized write scope, global for the empty scope, nil for a missing scope without create
func (r *ProviderRegistry[P]) target(scope string, create bool) *pydict.Dict[string, P] {
	if scope == "" {
		return r.providers
	}
	if m, ok := r.scopedProviders.Get(scope); ok {
		return m
	}
	if !create {
		return nil
	}
	m := pydict.New[string, P]()
	r.scopedProviders.Set(scope, m)
	return m
}

// Advances the generation of a normalized write scope, the global one for the empty scope
func (r *ProviderRegistry[P]) bump(scope string) {
	if scope == "" {
		r.generation++
		return
	}
	r.scopedGenerations[scope]++
}

// Read scope key, the active home key when scope is empty
func readScopeKey(ctx context.Context, scope string) string {
	if scope == "" {
		return home.Key(home.Dir(ctx))
	}
	return home.Key(scope)
}

// Registers a provider in the global layer or a scope, same name re-registration overwriting for hot reload
func (r *ProviderRegistry[P]) Register(provider P, scope string) error {
	_, err := r.register(provider, scope)
	return err
}

// Register body over an untyped value, true when the provider now occupies its slot
func (r *ProviderRegistry[P]) register(provider any, scope string) (bool, error) {
	p, ok := asProvider[P](provider)
	if !ok {
		article := "a"
		if strings.ContainsRune("AEIOU", []rune(r.ProviderCls + " ")[0]) {
			article = "an"
		}
		typeName := pyrepr.TypeName(provider)
		if providerIsNil(provider) {
			typeName = "NoneType"
		}
		return false, &ProviderTypeError{fmt.Sprintf("register_provider() expects %s %s instance, got %s", article, r.ProviderCls, typeName)}
	}
	rawName := p.Name()
	if pystr.Strip(rawName) == "" {
		return false, &ValueError{fmt.Sprintf("%s provider .name must be a non-empty string", r.Label)}
	}
	key := r.Normalize(rawName)
	if r.BuiltinNames[key] {
		if r.onBuiltinCollision != nil {
			if err := r.onBuiltinCollision(key); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	r.mu.Lock()
	scope = home.NormalizeScope(scope)
	target := r.target(scope, true)
	existing, had := target.Get(key)
	target.Set(key, p)
	r.bump(scope)
	r.mu.Unlock()
	if had {
		r.Logger.Debug(fmt.Sprintf("%s provider '%s' re-registered (was %s)", r.Label, key, pyrepr.StrRepr(pyrepr.TypeName(existing))))
	} else {
		r.Logger.Debug(fmt.Sprintf("Registered %s provider '%s' (%s)", r.logLabel, key, pyrepr.TypeName(p)))
	}
	return true, nil
}

// Copy of the global map overlaid with the active profile scope map
func (r *ProviderRegistry[P]) Merged(ctx context.Context, scope string) *pydict.Dict[string, P] {
	key := readScopeKey(ctx, scope)
	r.mu.Lock()
	defer r.mu.Unlock()
	scoped, _ := r.scopedProviders.Get(key)
	return pydict.Merge(r.providers, scoped)
}

// Every provider visible in the active scope sorted by name
func (r *ProviderRegistry[P]) ListProviders(ctx context.Context, scope string) []P {
	out := r.Merged(ctx, scope).Values()
	slices.SortStableFunc(out, func(a, b P) int { return strings.Compare(a.Name(), b.Name()) })
	return out
}

// Provider registered under name in the active scope, then globally, the zero value when absent
func (r *ProviderRegistry[P]) GetProvider(ctx context.Context, name, scope string) P {
	key := r.Normalize(name)
	active := readScopeKey(ctx, scope)
	r.mu.Lock()
	defer r.mu.Unlock()
	if scoped, ok := r.scopedProviders.Get(active); ok {
		if p, ok := scoped.Get(key); ok {
			return p
		}
	}
	p, _ := r.providers.Get(key)
	return p
}

// Global and active scope generation pair callers compare to invalidate caches
func (r *ProviderRegistry[P]) RegistryGeneration(ctx context.Context, scope string) ProviderRegistryGeneration {
	active := readScopeKey(ctx, scope)
	r.mu.Lock()
	defer r.mu.Unlock()
	return ProviderRegistryGeneration{Global: r.generation, Scoped: r.scopedGenerations[active]}
}

// Exact slot occupant with no global fallback, the zero value when the slot is empty
func (r *ProviderRegistry[P]) SnapshotRegistration(name, scope string) P {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, _ := r.target(home.NormalizeScope(scope), false).Get(r.Normalize(name))
	return p
}

// Restores previous, or removes the slot for a nil previous, only while current still occupies it
func (r *ProviderRegistry[P]) RestoreRegistration(name string, current, previous P, scope string) bool {
	return r.restore(name, current, previous, scope)
}

// Restore body over an untyped current so foreign values never match a slot
func (r *ProviderRegistry[P]) restore(name string, current any, previous P, scope string) bool {
	key := r.Normalize(name)
	scope = home.NormalizeScope(scope)
	r.mu.Lock()
	defer r.mu.Unlock()
	target := r.target(scope, true)
	installed, ok := target.Get(key)
	if ok && !sameProvider(installed, current) || !ok && !providerIsNil(current) {
		return false
	}
	if providerIsNil(previous) {
		target.Delete(key)
	} else {
		target.Set(key, previous)
	}
	r.bump(scope)
	if scope != "" && target.Len() == 0 {
		r.scopedProviders.Delete(scope)
	}
	return true
}

// Clears every registration and advances the global generation, for tests only
func (r *ProviderRegistry[P]) ResetForTests() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers.Clear()
	r.scopedProviders.Clear()
	clear(r.scopedGenerations)
	r.generation++
}

// Historical module level API of a provider registry module plus its providers, scoped providers and lock test hooks
type ProviderRegistryAPI[P NamedProvider] struct {
	Providers            *pydict.Dict[string, P]
	ScopedProviders      *pydict.Dict[string, *pydict.Dict[string, P]]
	Lock                 *sync.Mutex
	RegisterProvider     func(provider P, scope string) error
	ListProviders        func(ctx context.Context, scope string) []P
	GetProvider          func(ctx context.Context, name, scope string) P
	SnapshotRegistration func(name, scope string) P
	RestoreRegistration  func(name string, current, previous P, scope string) bool
	RegistryGeneration   func(ctx context.Context, scope string) ProviderRegistryGeneration
	ResetForTests        func()
}

// Bound method values and live test hooks a registry module exposes under its historical names
func (r *ProviderRegistry[P]) Export() ProviderRegistryAPI[P] {
	return ProviderRegistryAPI[P]{
		Providers: r.providers, ScopedProviders: r.scopedProviders, Lock: &r.mu,
		RegisterProvider: r.Register, ListProviders: r.ListProviders, GetProvider: r.GetProvider,
		SnapshotRegistration: r.SnapshotRegistration, RestoreRegistration: r.RestoreRegistration,
		RegistryGeneration: r.RegistryGeneration, ResetForTests: r.ResetForTests,
	}
}

// Plugin context view of the registry that register category provider methods drive
func (r *ProviderRegistry[P]) Scoped() plugins.ScopedRegistry { return scopedProviderRegistry[P]{r} }

type scopedProviderRegistry[P NamedProvider] struct{ r *ProviderRegistry[P] }

var _ plugins.ScopedRegistry = scopedProviderRegistry[ProviderBase]{}

// Whether the value is an instance of the registry provider type
func (s scopedProviderRegistry[P]) Accepts(provider any) bool {
	_, ok := asProvider[P](provider)
	return ok
}

// Raw name of an accepted provider, empty for any other value
func (s scopedProviderRegistry[P]) ProviderName(provider any) string {
	if p, ok := asProvider[P](provider); ok {
		return p.Name()
	}
	return ""
}

// Category registries admit every accepted provider by type and name alone
func (s scopedProviderRegistry[P]) Identity(any) error { return nil }

// Exact slot occupant as an untyped value, nil when the slot is empty
func (s scopedProviderRegistry[P]) Snapshot(name, scope string) any {
	p := s.r.SnapshotRegistration(name, scope)
	if providerIsNil(p) {
		return nil
	}
	return p
}

// Registers an untyped provider, true when it now occupies its slot
func (s scopedProviderRegistry[P]) Register(provider any, scope string) (bool, error) {
	return s.r.register(provider, scope)
}

// Restores the replacement snapshot while current still occupies the slot
func (s scopedProviderRegistry[P]) Restore(name string, current, replacement any, scope string) bool {
	previous, ok := asProvider[P](replacement)
	if !ok && !providerIsNil(replacement) {
		return false
	}
	return s.r.restore(name, current, previous, scope)
}

// Value as a provider of type P, false for nil, a nil pointer or a value of another type
func asProvider[P any](v any) (P, bool) {
	var zero P
	if providerIsNil(v) {
		return zero, false
	}
	p, ok := v.(P)
	return p, ok
}

// Python None test of a provider value, true for nil and nil pointer, map, slice, func or channel values
func providerIsNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface, reflect.UnsafePointer:
		return rv.IsNil()
	}
	return false
}

// Python identity test of two provider values, reference kinds compared by pointer and other comparable values by equality
func sameProvider(a, b any) bool {
	if providerIsNil(a) || providerIsNil(b) {
		return providerIsNil(a) && providerIsNil(b)
	}
	av, bv := reflect.ValueOf(a), reflect.ValueOf(b)
	if av.Type() != bv.Type() {
		return false
	}
	switch av.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return av.Pointer() == bv.Pointer()
	}
	return av.Type().Comparable() && a == b
}

// Provider whose availability probe is_available_safe guards
type AvailabilityProvider interface {
	NamedProvider
	IsAvailable() bool
}

// Availability of a provider, a panicking probe reading as unavailable and logged at level with its traceback when excInfo
func IsAvailableSafe(provider AvailabilityProvider, logger *slog.Logger, format string, level slog.Level, excInfo bool) (available bool) {
	defer func() {
		rec := recover()
		if rec == nil {
			return
		}
		if logger == nil {
			logger = slog.Default()
		}
		var attrs []any
		if excInfo {
			attrs = append(attrs, "exc_info", fmt.Sprintf("%v\n%s", rec, debug.Stack()))
		}
		logger.Log(context.Background(), level, fmt.Sprintf(format, provider.Name(), panicText(rec)), attrs...)
		available = false
	}()
	return provider.IsAvailable()
}

// Python str of a recovered panic value
func panicText(rec any) string {
	if err, ok := rec.(error); ok {
		return err.Error()
	}
	return pyrepr.Str(rec)
}

// Configured section provider from the read only config, stripped, with the managed Nous selection read as fal
func ConfiguredProviderName(ctx context.Context, section string, logger *slog.Logger) string {
	configured := ""
	cfg, err := config.LoadReadonly(ctx, home.Dir(ctx), nil)
	if err != nil {
		if logger == nil {
			logger = slog.Default()
		}
		logger.Debug(fmt.Sprintf("Could not read %s.provider from config: %s", section, err))
	} else if raw, ok := configBlock(configBlock(map[string]any(cfg))[section])["provider"].(string); ok && pystr.Strip(raw) != "" {
		configured = pystr.Strip(raw)
	}
	if configured != "" && pystr.Lower(configured) == tools.NousManagedProvider {
		configured = "fal"
	}
	return configured
}

// Mapping value of a config node, nil for any non mapping
func configBlock(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case config.Tree:
		return m
	}
	return nil
}

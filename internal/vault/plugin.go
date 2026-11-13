package vault

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Grammar of a plugin login backend name, a lowercase identifier
var pluginName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Login backend a trusted plugin supplies, only ever handed handles inside its own namespace
type PluginBackend interface {
	List(ctx context.Context) ([]ItemMeta, error)
	Meta(ctx context.Context, handle string) (ItemMeta, bool, error)
	ResolvePassword(ctx context.Context, handle string) (string, error)
	ResolveOTP(ctx context.Context, handle string) (string, error)
	ResolveSecret(ctx context.Context, handle string) ([]Field, error)
	Fill(ctx context.Context, fill func(context.Context) error) error
}

// Fixed text refusal of a plugin backend, its own error text never surfaced since it may hold a credential
type PluginError struct {
	Message string
}

// Fixed refusal text
func (e *PluginError) Error() string { return e.Message }

// One registered plugin backend, compared by identity when its registration is removed
type pluginEntry struct {
	backend PluginBackend
}

// Plugin login backends registered process wide and per profile home
type pluginRegistry struct {
	mu     sync.Mutex
	global map[string]*pluginEntry
	scoped map[string]map[string]*pluginEntry
}

// Empty plugin registry
func newPluginRegistry() *pluginRegistry {
	return &pluginRegistry{global: map[string]*pluginEntry{}, scoped: map[string]map[string]*pluginEntry{}}
}

// Why name cannot be a plugin backend namespace, empty when it can
func namespaceError(name string) string {
	if !pluginName.MatchString(name) {
		return "login backend name must be a lowercase identifier"
	}
	for _, reserved := range []string{localName, onePasswordName, bitwardenName, "vault", "op", "bw"} {
		if name == reserved {
			return "builtin login backend namespace is reserved"
		}
	}
	for _, prefix := range []string{localPrefix, onePasswordPfx, bitwardenPrefix} {
		if strings.HasPrefix(name+":", prefix) {
			return "builtin login backend namespace is reserved"
		}
	}
	return ""
}

// Merged view of global registrations overlaid with those scoped to key, the registry lock held
func (r *pluginRegistry) mergedLocked(key string) map[string]*pluginEntry {
	out := maps.Clone(r.global)
	maps.Copy(out, r.scoped[key])
	return out
}

// Merged view of global registrations overlaid with those scoped to key
func (r *pluginRegistry) merged(key string) map[string]*pluginEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mergedLocked(key)
}

// Registers a plugin login backend under name, scoped to the profile home scope names or process wide when empty
func (v *Vault) RegisterPlugin(ctx context.Context, scope, name string, backend PluginBackend) (func(), error) {
	if msg := namespaceError(name); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	key, err := v.resolver.NormalizeScope(scope)
	if err != nil {
		return nil, err
	}
	active := key
	if active == "" {
		h, err := v.resolver.Home(ctx)
		if err != nil {
			return nil, err
		}
		if active, err = v.resolver.Key(h); err != nil {
			return nil, err
		}
	}
	r := v.plugins
	entry := &pluginEntry{backend: backend}
	r.mu.Lock()
	if _, taken := r.mergedLocked(active)[name]; taken {
		r.mu.Unlock()
		return nil, fmt.Errorf("login backend name %s is already registered", quote(name))
	}
	target := r.global
	if key != "" {
		if r.scoped[key] == nil {
			r.scoped[key] = map[string]*pluginEntry{}
		}
		target = r.scoped[key]
	}
	target[name] = entry
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		target := r.global
		if key != "" {
			target = r.scoped[key]
		}
		if target[name] == entry {
			delete(target, name)
			if key != "" && len(target) == 0 {
				delete(r.scoped, key)
			}
		}
	}, nil
}

// Registered backends the profile turned on with vault.name.enabled, wrapped and sorted by name
func (v *Vault) pluginBackends(ctx context.Context, opts Options) ([]LoginBackend, error) {
	h, err := v.resolver.Home(ctx)
	if err != nil {
		return nil, err
	}
	key, err := v.resolver.Key(h)
	if err != nil {
		return nil, err
	}
	registered := v.plugins.merged(key)
	var out []LoginBackend
	for _, name := range slices.Sorted(maps.Keys(registered)) {
		if namespaceError(name) == "" && opts.Plugins[name] {
			out = append(out, &pluginLogin{name: name, plugin: registered[name].backend})
		}
	}
	return out, nil
}

// Context a plugin call runs under, the caller's deadline and cancellation kept and every value withheld
type pluginContext struct {
	parent context.Context
}

// Time limit of the caller
func (c pluginContext) Deadline() (time.Time, bool) { return c.parent.Deadline() }

// Cancellation of the caller
func (c pluginContext) Done() <-chan struct{} { return c.parent.Done() }

// Cancellation cause of the caller
func (c pluginContext) Err() error { return c.parent.Err() }

// Nothing, so no prompt surface, session or page the caller holds reaches the plugin
func (pluginContext) Value(any) any { return nil }

// Core owned wrapper deciding ownership itself and replacing every plugin failure with fixed text
type pluginLogin struct {
	name   string
	plugin PluginBackend
}

// Registered name of the plugin backend
func (p *pluginLogin) Name() string { return p.name }

// Core composed display name, so a plugin cannot present itself as a built in manager
func (p *pluginLogin) DisplayName() string { return p.name + " (plugin)" }

// Namespace prefix taken from the registry key
func (p *pluginLogin) Prefix() string { return p.name + ":" }

// Reports false, a plugin never receives the master password prompt
func (*pluginLogin) NeedsUnlock() bool { return false }

// Reports whether handle lies in the plugin's namespace
func (p *pluginLogin) Owns(handle string) bool { return strings.HasPrefix(handle, p.Prefix()) }

// Reports true, a plugin backend owns its own unlock state
func (*pluginLogin) Unlocked(context.Context) bool { return true }

// Refusal for a plugin call that returned an error or panicked
func (p *pluginLogin) failed() error {
	return &PluginError{Message: fmt.Sprintf("The %s login backend failed.", p.name)}
}

// Refusal for a value of the wrong kind
func (p *pluginLogin) invalid() error {
	return &PluginError{Message: fmt.Sprintf("The %s login backend returned an invalid value.", p.name)}
}

// Refusal for a handle in another namespace
func (p *pluginLogin) outside() error {
	return &PluginError{Message: fmt.Sprintf("Handle is outside the %s login backend's namespace.", p.name)}
}

// Runs one plugin call, its error or panic replaced by the fixed failure so no plugin text escapes
func (p *pluginLogin) guard(call func() error) (err error) {
	defer func() {
		if recover() != nil {
			err = p.failed()
		}
	}()
	if call() != nil {
		return p.failed()
	}
	return nil
}

// Copy of plugin metadata, false when its kind is not one the vault knows
func copyMeta(m ItemMeta) (ItemMeta, bool) {
	if !slices.Contains(Kinds(), m.Kind) {
		return ItemMeta{}, false
	}
	m.AllowedOrigins = slices.Clone(m.AllowedOrigins)
	return m, true
}

// Valid items of the plugin inside its namespace, the whole listing refused when any item is invalid
func (p *pluginLogin) List(ctx context.Context) ([]ItemMeta, error) {
	var items []ItemMeta
	if err := p.guard(func() (err error) { items, err = p.plugin.List(pluginContext{ctx}); return err }); err != nil {
		return nil, err
	}
	var out []ItemMeta
	for _, item := range items {
		m, ok := copyMeta(item)
		if !ok {
			return nil, p.invalid()
		}
		if p.Owns(m.ID) {
			out = append(out, m)
		}
	}
	return out, nil
}

// Metadata of handle, none for a handle outside the namespace or an item claiming another id
func (p *pluginLogin) Meta(ctx context.Context, handle string) (ItemMeta, bool, error) {
	if !p.Owns(handle) {
		return ItemMeta{}, false, nil
	}
	var item ItemMeta
	var found bool
	if err := p.guard(func() (err error) { item, found, err = p.plugin.Meta(pluginContext{ctx}, handle); return err }); err != nil {
		return ItemMeta{}, false, err
	}
	if !found {
		return ItemMeta{}, false, nil
	}
	m, ok := copyMeta(item)
	if !ok {
		return ItemMeta{}, false, p.invalid()
	}
	if m.ID != handle {
		return ItemMeta{}, false, nil
	}
	return m, true, nil
}

// Password of an owned handle
func (p *pluginLogin) ResolvePassword(ctx context.Context, handle string) (Secret, error) {
	if !p.Owns(handle) {
		return Secret{}, p.outside()
	}
	var password string
	if err := p.guard(func() (err error) { password, err = p.plugin.ResolvePassword(pluginContext{ctx}, handle); return err }); err != nil {
		return Secret{}, err
	}
	return secretOf(secretField{PasswordField, password}), nil
}

// One time code of an owned handle, no code when the plugin has none
func (p *pluginLogin) ResolveOTP(ctx context.Context, handle string) (Secret, error) {
	if !p.Owns(handle) {
		return Secret{}, p.outside()
	}
	var code string
	if err := p.guard(func() (err error) { code, err = p.plugin.ResolveOTP(pluginContext{ctx}, handle); return err }); err != nil {
		return Secret{}, err
	}
	if code == "" {
		return Secret{}, nil
	}
	return secretOf(secretField{CodeField, code}), nil
}

// Full payload of an owned handle
func (p *pluginLogin) ResolveSecret(ctx context.Context, handle string) (Secret, error) {
	if !p.Owns(handle) {
		return Secret{}, p.outside()
	}
	var fields []Field
	if err := p.guard(func() (err error) { fields, err = p.plugin.ResolveSecret(pluginContext{ctx}, handle); return err }); err != nil {
		return Secret{}, err
	}
	copied := make([]secretField, len(fields))
	for i, f := range fields {
		copied[i] = secretField{f.Name, f.Value}
	}
	return secretOf(copied...), nil
}

// Runs fill exactly once inside the plugin's own context, which never sees the core's error or values
func (p *pluginLogin) Fill(ctx context.Context, fill func(context.Context) error) error {
	var mu sync.Mutex
	ran, closed := false, false
	var fillErr error
	var fillPanic any
	run := func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if ran || closed {
			return nil
		}
		ran = true
		func() {
			defer func() { fillPanic = recover() }()
			fillErr = fill(ctx)
		}()
		return nil
	}
	err := p.guard(func() error { return p.plugin.Fill(pluginContext{ctx}, run) })
	mu.Lock()
	defer mu.Unlock()
	closed = true
	if fillPanic != nil {
		panic(fillPanic)
	}
	if err != nil {
		return err
	}
	if !ran {
		return p.failed()
	}
	return fillErr
}

// Reports true, a plugin's fill context may freeze state the item must be re-read under
func (*pluginLogin) Rereads() bool { return true }

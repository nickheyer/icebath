package settings

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
)

// One applied change, what every subscriber hears
type Change struct {
	Key       string
	Scope     Scope
	Home      string
	SessionID string
	Old       Value
	New       Value
	Apply     Apply
	// The change removed the value at its scope
	Reset bool
}

// Receives every applied change after it is stored
type Listener func(ctx context.Context, c Change)

// Subscribers to applied changes
var listeners = struct {
	sync.RWMutex
	next int
	fns  map[int]Listener
}{fns: map[int]Listener{}}

// Calls fn after every applied change, the returned func ending the subscription
func Subscribe(fn Listener) (cancel func()) {
	listeners.Lock()
	id := listeners.next
	listeners.next++
	listeners.fns[id] = fn
	listeners.Unlock()
	return func() {
		listeners.Lock()
		delete(listeners.fns, id)
		listeners.Unlock()
	}
}

// Tells every subscriber about a change, a panicking subscriber only logged
func notify(ctx context.Context, c Change) {
	listeners.RLock()
	fns := make([]Listener, 0, len(listeners.fns))
	for _, fn := range listeners.fns {
		fns = append(fns, fn)
	}
	listeners.RUnlock()
	for _, fn := range fns {
		func() {
			defer func() {
				if p := recover(); p != nil {
					pkgLog().Warn("settings: change listener failed", "key", c.Key, "panic", fmt.Sprint(p))
				}
			}()
			fn(ctx, c)
		}()
	}
}

// Scope a write goes to when the caller names none, global when allowed
func defaultScope(m *Meta) Scope {
	for _, s := range []Scope{Global, Session, Workspace} {
		if m.Scopes&s != 0 {
			return s
		}
	}
	return Global
}

// Refuses a write an administrator pins or a remote config level locks
func checkLocked(ctx context.Context, h string, m *Meta) error {
	if config.IsKeyManaged(m.Key) {
		return fmt.Errorf("%w: %s is managed by your administrator", ErrLocked, m.Key)
	}
	if m.State {
		return nil
	}
	if m.Env != "" && config.IsEnvManaged(m.Env) {
		return fmt.Errorf("%w: %s is managed by your administrator", ErrLocked, m.Key)
	}
	if config.IsManaged(ctx, h) {
		return fmt.Errorf("%w: this install is managed by %s", ErrLocked, config.ManagedSystem(ctx, h))
	}
	backend, err := config.GetConfigBackend(ctx)
	if err != nil {
		return err
	}
	level, locked, err := backend.Locked(ctx, h, m.Key)
	if err != nil {
		return err
	}
	if locked && level != "" {
		return fmt.Errorf("%w: %s is locked by the %s level", ErrLocked, m.Key, level)
	}
	return nil
}

// Checks, stores and announces one setting at a scope, zero picking the setting's default scope
func Set(ctx context.Context, key string, value any, scope Scope) (Change, error) {
	e := Find(key)
	if e == nil {
		return Change{}, fmt.Errorf("%w: %s", ErrUnknownKey, key)
	}
	if value == nil {
		return Reset(ctx, key, scope)
	}
	m := e.Info()
	coerced, err := e.Coerce(value)
	if err != nil {
		return Change{}, err
	}
	if m.Validate != nil && coerced != nil {
		if err := m.Validate(coerced); err != nil {
			return Change{}, &ValueError{Key: m.Key, Reason: err.Error()}
		}
	}
	return write(ctx, e, coerced, scope, false)
}

// Removes the value a scope holds so the next layer down applies
func Reset(ctx context.Context, key string, scope Scope) (Change, error) {
	e := Find(key)
	if e == nil {
		return Change{}, fmt.Errorf("%w: %s", ErrUnknownKey, key)
	}
	return write(ctx, e, nil, scope, true)
}

// Stores a coerced value or a reset at a scope and notifies
func write(ctx context.Context, e Entry, value any, scope Scope, reset bool) (Change, error) {
	m := e.Info()
	if scope == 0 {
		scope = defaultScope(m)
	}
	if m.Scopes&scope == 0 {
		return Change{}, fmt.Errorf("%w: %s takes %s", ErrScope, m.Key, m.Scopes)
	}
	h := homeDir(ctx)
	change := Change{Key: m.Key, Scope: scope, Home: h, Apply: m.Apply, Reset: reset}
	switch scope {
	case Session:
		sid := SessionFrom(ctx)
		if sid == "" {
			return Change{}, ErrNoSession
		}
		if v, whole, pinned := managedPin(layersOf(ctx, h), e); pinned && whole {
			return Change{}, fmt.Errorf("%w: %s is pinned to %v", ErrLocked, m.Key, v)
		}
		change.SessionID = sid
		change.Old = resolveEntry(ctx, e)
		set, drop, err := sessionWrites(ctx, e, value, reset)
		if err != nil {
			return Change{}, err
		}
		if err := storeSessionValues(ctx, sid, set, drop); err != nil {
			return Change{}, err
		}
	case Global:
		if err := checkLocked(ctx, h, m); err != nil {
			return Change{}, err
		}
		change.Old = resolveEntry(ctx, e)
		err := writeGlobal(ctx, h, e, value, reset)
		forgetLayers()
		if err != nil {
			return Change{}, err
		}
	default:
		return Change{}, fmt.Errorf("%w: %s scope is not writable yet", ErrScope, scope)
	}
	change.New = resolveEntry(ctx, e)
	notify(ctx, change)
	return change, nil
}

// Session layer keys a session write stores and drops: the key itself unless derived, its companions, and a map entry folded into its map
func sessionWrites(ctx context.Context, e Entry, value any, reset bool) (map[string]any, []string, error) {
	m := e.Info()
	set := map[string]any{}
	var drop []string
	if child, isChild := e.(*mapChild); isChild {
		parent := child.parent.Key
		entries := config.Clone(resolveTyped(ctx, child.parent)).(map[string]any)
		name := strings.TrimPrefix(m.Key, parent+".")
		if reset {
			config.Tree(entries).Unset(name)
		} else if err := config.Tree(entries).Set(name, value); err != nil {
			return nil, nil, invalid(m.Key, "%s", err.Error())
		}
		set[parent] = entries
		return set, nil, nil
	}
	if m.Derive == nil {
		if reset {
			drop = append(drop, m.Key)
		} else {
			set[m.Key] = value
		}
	}
	if m.Companions == nil {
		return set, drop, nil
	}
	if reset {
		for _, kv := range m.Companions(e.DefaultValue()) {
			drop = append(drop, kv.Key)
		}
		return set, drop, nil
	}
	for _, kv := range m.Companions(value) {
		set[kv.Key] = kv.Value
	}
	return set, drop, nil
}

// Writes a value or a reset to the profile layer under the config lock, dropping the old keys it replaces
func writeGlobal(ctx context.Context, h string, e Entry, value any, reset bool) error {
	m := e.Info()
	if secret, isSecret := e.(*Secret); isSecret {
		if secret.Env == "" {
			return fmt.Errorf("settings: secret %s names no env entry", m.Key)
		}
		if reset {
			_, err := config.RemoveEnvValue(config.GetEnvPath(h), secret.Env)
			return err
		}
		return config.SaveEnvValueContext(ctx, h, secret.Env, value.(string))
	}
	if m.State {
		return writeState(ctx, h, m.Key, value, reset)
	}
	ctx, unlock := config.LockConfig(ctx)
	defer unlock()
	path := config.GetConfigPath(h)
	if _, err := config.RequireReadableConfigBeforeWrite(ctx, path); err != nil {
		return err
	}
	var changes config.Changes
	switch {
	case m.Derive != nil && reset:
		for _, kv := range m.Companions(e.DefaultValue()) {
			changes.Unset = append(changes.Unset, kv.Key)
		}
	case m.Derive != nil:
		changes.Set = append(changes.Set, m.Companions(value)...)
	case reset:
		changes.Unset = append(changes.Unset, m.Key)
	default:
		changes.Set = append(changes.Set, config.KeyValue{Key: m.Key, Value: value})
		if m.Companions != nil {
			changes.Set = append(changes.Set, m.Companions(value)...)
		}
	}
	return config.WriteChanges(config.PinningKeys(ctx, m.Key), path, changes)
}

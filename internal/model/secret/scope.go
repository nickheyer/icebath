package secret

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
)

// Secret read under multiplexing with no profile scope bound, never answered from the process environment
type UnscopedError struct {
	// Variable that was read, empty when unknown
	Name string
	// Diagnosis for a developer, kept out of the one sentence a user sees
	Detail string
}

// The one sentence an end user can act on
func (e *UnscopedError) Error() string {
	what := "this profile's API key"
	if e.Name != "" {
		what = "this profile's " + e.Name
	}
	return "icebath could not read " + what + " (an internal profile-scoping bug on the multiplexed gateway, not your configuration). " +
		"Run `icebath gateway restart`; if it keeps happening, report it with `icebath debug share`."
}

// Matches ErrUnscoped
func (e *UnscopedError) Is(target error) bool { return target == ErrUnscoped }

// Sentinel every unscoped read matches
var ErrUnscoped = errors.New("secret read without a profile scope")

// Scope is the secret scope the config env layer reads and writes through
var _ config.SecretScope = (*Scope)(nil)

// Secret mapping bound to a task with the home it was built for
type boundScope struct {
	mu      sync.RWMutex
	values  map[string]string
	homeDir string
}

type (
	boundKey     struct{}
	multiplexKey struct{}
	launchKey    struct{}
)

// What a Scope is built from
type ScopeOptions struct {
	Homes *home.Resolver
	// Variables the launch process bridged from its own config into its environment, seeded into its own profile mapping only
	Bridged func() map[string]string
}

// Profile scoped credential lookup, fail closed while one process serves several profiles
type Scope struct {
	homes   *home.Resolver
	bridged func() map[string]string

	mu         sync.Mutex
	multiplex  bool
	autoPinned string
	hasAutoPin bool
}

// Scope of a process that serves a single profile until told otherwise
func NewScope(o ScopeOptions) (*Scope, error) {
	if o.Homes == nil {
		return nil, errors.New("secret: a home resolver is required")
	}
	return &Scope{homes: o.Homes, bridged: o.Bridged}, nil
}

// Stable identity of a home path
func (s *Scope) key(path string) string {
	if key, err := s.homes.Key(path); err == nil {
		return key
	}
	return path
}

// Marks the process a profile multiplexer or not, pinning the launch home meanwhile unless a host pinned one
func (s *Scope) SetMultiplex(active bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.multiplex = active
	if active {
		if home.ProcessHomePinned() {
			return nil
		}
		launch, err := home.RoutingHome()
		if err != nil {
			return err
		}
		s.autoPinned, s.hasAutoPin = launch, true
		home.PinProcessHome(launch)
		return nil
	}
	if s.hasAutoPin {
		if launch, err := home.RoutingHome(); err == nil && launch == s.autoPinned {
			home.UnpinProcessHome()
		}
		s.autoPinned, s.hasAutoPin = "", false
	}
	return nil
}

// Context whose task runs under multiplex semantics whatever the process flag says
func (s *Scope) WithMultiplex(ctx context.Context, active bool) context.Context {
	return context.WithValue(ctx, multiplexKey{}, active)
}

// Reports a multiplexing process or a task running under multiplex semantics
func (s *Scope) Multiplexed(ctx context.Context) bool {
	s.mu.Lock()
	active := s.multiplex
	s.mu.Unlock()
	if active {
		return true
	}
	on, _ := ctx.Value(multiplexKey{}).(bool)
	return on
}

// Context with a profile secret mapping in force, homeDir naming the home it was built for, nil clearing it
func (s *Scope) Bind(ctx context.Context, secrets map[string]string, homeDir string) context.Context {
	if secrets == nil {
		return context.WithValue(ctx, boundKey{}, (*boundScope)(nil))
	}
	return context.WithValue(ctx, boundKey{}, &boundScope{values: secrets, homeDir: homeDir})
}

// Binding of ctx, nil when none is in force
func bound(ctx context.Context) *boundScope {
	b, _ := ctx.Value(boundKey{}).(*boundScope)
	return b
}

// Reports whether a secret mapping is bound to ctx
func (s *Scope) Bound(ctx context.Context) bool { return bound(ctx) != nil }

// Copy of the bound mapping, false when none is bound
func (s *Scope) Current(ctx context.Context) (map[string]string, bool) {
	b := bound(ctx)
	if b == nil {
		return nil, false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(map[string]string, len(b.values))
	for name, value := range b.values {
		out[name] = value
	}
	return out, true
}

// Home the bound mapping was stamped with, empty when unstamped or unbound
func (s *Scope) CurrentHome(ctx context.Context) string {
	if b := bound(ctx); b != nil {
		return b.homeDir
	}
	return ""
}

// Context of a launch profile body that began before multiplexing, resolver giving its own mapping once reads fail closed
func (s *Scope) WithLaunchBody(ctx context.Context, resolver func() map[string]string) context.Context {
	return context.WithValue(ctx, launchKey{}, resolver)
}

// Reports a task running for a profile other than the one the process launched for
func (s *Scope) Routed(ctx context.Context) bool {
	if s.Multiplexed(ctx) {
		return true
	}
	launch, err := home.RoutingHome()
	if err != nil {
		return true
	}
	own := s.key(launch)
	if b := bound(ctx); b != nil && b.homeDir != "" && s.key(b.homeDir) != own {
		return true
	}
	override := home.Override(ctx)
	return override != "" && s.key(override) != own
}

// Credential by variable name under the profile scope of ctx, an UnscopedError for an unscoped read while multiplexing
func (s *Scope) Lookup(ctx context.Context, name string) (string, bool, error) {
	if config.IsGlobalEnv(name) {
		v, ok := os.LookupEnv(name)
		return v, ok, nil
	}
	if b := bound(ctx); b != nil {
		b.mu.RLock()
		v, ok := b.values[name]
		b.mu.RUnlock()
		if ok || s.Routed(ctx) {
			return v, ok, nil
		}
		v, ok = os.LookupEnv(name)
		return v, ok, nil
	}
	if s.Multiplexed(ctx) {
		if resolver, ok := ctx.Value(launchKey{}).(func() map[string]string); ok && resolver != nil {
			v, found := resolver()[name]
			return v, found, nil
		}
		return "", false, &UnscopedError{Name: name, Detail: fmt.Sprintf(
			"secret %q was read with no profile secret scope bound while multiplexing is on; the read must run inside a bound scope, "+
				"because the process environment could hold another profile's value", name)}
	}
	v, ok := os.LookupEnv(name)
	return v, ok, nil
}

// Value of a credential, def when it is unset, an unscoped read still an error
func (s *Scope) Get(ctx context.Context, name, def string) (string, error) {
	v, ok, err := s.Lookup(ctx, name)
	if err != nil {
		return "", err
	}
	if !ok {
		return def, nil
	}
	return v, nil
}

// Writes one variable into the bound mapping, a nil value removing it, false when nothing is bound
func (s *Scope) Store(ctx context.Context, name string, value *string) bool {
	b := bound(ctx)
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if value == nil {
		delete(b.values, name)
	} else {
		b.values[name] = *value
	}
	return true
}

// Secret mapping of a profile home from its env file, secret sources and managed env file, without process settings
func (s *Scope) Build(ctx context.Context, env *config.Env, homeDir string) (map[string]string, error) {
	secrets, err := env.Profile(home.WithOverride(ctx, homeDir))
	if err != nil {
		return nil, err
	}
	if s.bridged == nil {
		return secrets, nil
	}
	launch, err := home.RoutingHome()
	if err != nil || s.key(launch) != s.key(homeDir) {
		return secrets, nil
	}
	for name, value := range s.bridged() {
		if _, has := secrets[name]; !has {
			secrets[name] = value
		}
	}
	return secrets, nil
}

// Rebuilds the mapping bound to ctx from homeDir in place, names no longer supplied dropped, false when nothing is bound
func (s *Scope) Refresh(ctx context.Context, env *config.Env, homeDir string) (bool, error) {
	b := bound(ctx)
	if b == nil {
		return false, nil
	}
	rebuilt, err := s.Build(ctx, env, homeDir)
	if err != nil {
		return false, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for name, value := range rebuilt {
		b.values[name] = value
	}
	for name := range b.values {
		if _, kept := rebuilt[name]; !kept {
			delete(b.values, name)
		}
	}
	return true, nil
}

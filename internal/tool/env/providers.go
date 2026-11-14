package env

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/tool"
)

var builtinBackendNames = []string{"local", "docker", "modal", "managed_modal", "daytona", "vercel_sandbox", "ssh"}

func BuiltinBackendNames() []string { return slices.Clone(builtinBackendNames) }

func IsBuiltinBackend(name string) bool {
	return slices.Contains(builtinBackendNames, strings.ToLower(strings.TrimSpace(name)))
}

type ProviderFlags struct {
	Remote                           bool
	Container                        bool
	SkipContainerGuards              bool
	CachePathBase                    string
	DefaultCwd                       string
	StripEnvKeys                     []string
	SessionIsolatedWhenNonpersistent bool
	SupportsStreams                  bool
}

type DoctorCheck struct {
	OK     bool
	Label  string
	Detail string
}

type ContainerConfig struct {
	CPU        float64
	Memory     int
	Disk       int
	Persistent bool
}

type ProviderOptions struct {
	Cwd       string
	Timeout   time.Duration
	TaskID    string
	Image     string
	Container *ContainerConfig
}

type Provider interface {
	Name() string
	DisplayName() string
	Description() string
	EnvDescription() string
	Setup() tool.ProviderRow
	Flags() ProviderFlags
	Available(ctx context.Context) bool
	CheckRequirements(ctx context.Context, cfg config.TerminalConfig) bool
	Probe(ctx context.Context) (status, detail string)
	SetupInstructions() []string
	PostSetup(ctx context.Context) error
	DoctorChecks(ctx context.Context) []DoctorCheck
	New(ctx context.Context, o ProviderOptions) (Backend, error)
}

type ReservedBackendError struct {
	Name string
}

func (e *ReservedBackendError) Error() string {
	return fmt.Sprintf("Terminal backend name '%s' is reserved for the built-in %s backend and cannot be registered by a plugin", e.Name, e.Name)
}

type ProviderNameError struct{}

func (ProviderNameError) Error() string {
	return "Terminal environment provider .name must be a non-empty string"
}

type Providers struct {
	homes *home.Resolver

	mu        sync.Mutex
	global    map[string]Provider
	scoped    map[string]map[string]Provider
	gen       int
	scopedGen map[string]int
}

func NewProviders(homes *home.Resolver) *Providers {
	return &Providers{homes: homes, global: map[string]Provider{}, scoped: map[string]map[string]Provider{}, scopedGen: map[string]int{}}
}

func providerKey(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func (r *Providers) readScope(ctx context.Context, scope string) (string, error) {
	if scope == "" {
		h, err := r.homes.Home(ctx)
		if err != nil {
			return "", err
		}
		scope = h
	}
	return r.homes.Key(scope)
}

func (r *Providers) targetLocked(scope string, create bool) map[string]Provider {
	if scope == "" {
		return r.global
	}
	m, ok := r.scoped[scope]
	if !ok && create {
		m = map[string]Provider{}
		r.scoped[scope] = m
	}
	return m
}

func (r *Providers) bumpLocked(scope string) {
	if scope == "" {
		r.gen++
		return
	}
	r.scopedGen[scope]++
}

func (r *Providers) Register(p Provider, scope string) error {
	if p == nil {
		return ProviderNameError{}
	}
	raw := p.Name()
	if strings.TrimSpace(raw) == "" {
		return ProviderNameError{}
	}
	key := providerKey(raw)
	if IsBuiltinBackend(key) {
		return &ReservedBackendError{Name: key}
	}
	normalized, err := r.homes.NormalizeScope(scope)
	if err != nil {
		return err
	}
	r.mu.Lock()
	target := r.targetLocked(normalized, true)
	existing, had := target[key]
	target[key] = p
	r.bumpLocked(normalized)
	r.mu.Unlock()
	if had {
		slog.Debug(fmt.Sprintf("Terminal environment provider '%s' re-registered (was %T)", key, existing))
	} else {
		slog.Debug(fmt.Sprintf("Registered terminal environment provider '%s' (%T)", key, p))
	}
	return nil
}

func (r *Providers) merged(ctx context.Context, scope string) (map[string]Provider, error) {
	key, err := r.readScope(ctx, scope)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]Provider, len(r.global))
	for k, v := range r.global {
		out[k] = v
	}
	for k, v := range r.scoped[key] {
		out[k] = v
	}
	return out, nil
}

func (r *Providers) List(ctx context.Context, scope string) ([]Provider, error) {
	m, err := r.merged(ctx, scope)
	if err != nil {
		return nil, err
	}
	out := make([]Provider, 0, len(m))
	for _, p := range m {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

func (r *Providers) Names(ctx context.Context, scope string) ([]string, error) {
	list, err := r.List(ctx, scope)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(list))
	for i, p := range list {
		out[i] = providerKey(p.Name())
	}
	return out, nil
}

func (r *Providers) Get(ctx context.Context, name, scope string) (Provider, bool, error) {
	key, err := r.readScope(ctx, scope)
	if err != nil {
		return nil, false, err
	}
	name = providerKey(name)
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.scoped[key][name]; ok {
		return p, true, nil
	}
	p, ok := r.global[name]
	return p, ok, nil
}

func (r *Providers) Flags(ctx context.Context, name, scope string) (ProviderFlags, bool, error) {
	p, ok, err := r.Get(ctx, name, scope)
	if err != nil || !ok {
		return ProviderFlags{}, false, err
	}
	return p.Flags(), true, nil
}

func (r *Providers) Generation(ctx context.Context, scope string) (int, int, error) {
	key, err := r.readScope(ctx, scope)
	if err != nil {
		return 0, 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gen, r.scopedGen[key], nil
}

func (r *Providers) Snapshot(name, scope string) (Provider, bool, error) {
	normalized, err := r.homes.NormalizeScope(scope)
	if err != nil {
		return nil, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.targetLocked(normalized, false)[providerKey(name)]
	return p, ok, nil
}

func (r *Providers) Restore(name string, current, previous Provider, scope string) (bool, error) {
	normalized, err := r.homes.NormalizeScope(scope)
	if err != nil {
		return false, err
	}
	key := providerKey(name)
	r.mu.Lock()
	defer r.mu.Unlock()
	target := r.targetLocked(normalized, true)
	if target[key] != current {
		return false, nil
	}
	if previous == nil {
		delete(target, key)
	} else {
		target[key] = previous
	}
	r.bumpLocked(normalized)
	if normalized != "" && len(target) == 0 {
		delete(r.scoped, normalized)
	}
	return true, nil
}

func (r *Providers) StripEnvKeys() []string {
	r.mu.Lock()
	all := make([]Provider, 0, len(r.global))
	for _, p := range r.global {
		all = append(all, p)
	}
	for _, m := range r.scoped {
		for _, p := range m {
			all = append(all, p)
		}
	}
	r.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, p := range all {
		for _, k := range p.Flags().StripEnvKeys {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

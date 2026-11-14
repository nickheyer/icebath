package secret

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
)

// Where a registered source came from
type Origin string

// Sources compiled into the binary and sources a plugin registered
const (
	Builtin Origin = "builtin"
	Plugin  Origin = "plugin"
)

// How a source is registered
type Registration struct {
	// Contract version the source was built against
	APIVersion int
	// Lets the source take the place of one already registered under its name
	Replace bool
	// Profile home the registration belongs to, empty for every home
	Home   string
	Origin Origin
}

// Ordered set of secret sources, a process wide layer with a layer per profile home over it
type Registry struct {
	homes *home.Resolver

	mu      sync.Mutex
	order   []string
	global  map[string]Source
	origins map[string]Origin
	scoped  map[string]*layer
}

// Sources registered for one home in registration order
type layer struct {
	order   []string
	sources map[string]Source
}

// Empty registry resolving home scopes through homes
func NewRegistry(homes *home.Resolver) *Registry {
	return &Registry{homes: homes, global: map[string]Source{}, origins: map[string]Origin{}, scoped: map[string]*layer{}}
}

// Scope key of a home, empty for the process wide layer
func (r *Registry) scopeKey(homeDir string) string {
	if homeDir == "" {
		return ""
	}
	if key, err := r.homes.NormalizeScope(homeDir); err == nil {
		return key
	}
	return filepath.Clean(homeDir)
}

// Reports a name of lower case letters, digits and underscores with at least one letter or digit
func validSourceName(name string) bool {
	if name == "" || name != strings.ToLower(name) || strings.Trim(name, "_") == "" {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '_' {
			return false
		}
	}
	return true
}

// Why a source cannot be registered, empty when it can
func validateSource(src Source, version int) string {
	name := src.Name()
	switch {
	case !validSourceName(name):
		return fmt.Sprintf("Ignoring secret source with invalid name '%s'", name)
	case version != APIVersion:
		return fmt.Sprintf("Ignoring secret source '%s': built against secret-source API v%d, this icebath speaks v%d", name, version, APIVersion)
	case src.Shape() != Mapped && src.Shape() != Bulk:
		return fmt.Sprintf("Ignoring secret source '%s': shape must be 'mapped' or 'bulk', got '%s'", name, src.Shape())
	}
	return ""
}

// Sources visible to a scope in order, its own layer over the process wide one, the caller holding the lock
func (r *Registry) merged(scope string) ([]string, map[string]Source) {
	order := append([]string(nil), r.order...)
	sources := make(map[string]Source, len(r.global))
	for name, src := range r.global {
		sources[name] = src
	}
	if l := r.scoped[scope]; scope != "" && l != nil {
		for _, name := range l.order {
			if _, seen := sources[name]; !seen {
				order = append(order, name)
			}
			sources[name] = l.sources[name]
		}
	}
	return order, sources
}

// Registers a source, false with a logged reason when its name, version, shape or scheme is refused
func (r *Registry) Register(src Source, reg Registration) bool {
	if problem := validateSource(src, reg.APIVersion); problem != "" {
		slog.Warn(problem)
		return false
	}
	scope := r.scopeKey(reg.Home)
	name := src.Name()
	r.mu.Lock()
	defer r.mu.Unlock()
	_, effective := r.merged(scope)
	if _, exists := effective[name]; exists && !reg.Replace {
		slog.Warn(fmt.Sprintf("Secret source '%s' already registered; ignoring duplicate", name))
		return false
	}
	if scheme := src.Scheme(); scheme != "" {
		for other, owner := range effective {
			if other != name && owner.Scheme() == scheme {
				slog.Warn(fmt.Sprintf("Ignoring secret source '%s': scheme '%s://' is already owned by source '%s'", name, scheme, other))
				return false
			}
		}
	}
	if scope == "" {
		if _, exists := r.global[name]; !exists {
			r.order = append(r.order, name)
		}
		r.global[name] = src
		r.origins[name] = reg.Origin
		return true
	}
	l := r.scoped[scope]
	if l == nil {
		l = &layer{sources: map[string]Source{}}
		r.scoped[scope] = l
	}
	if _, exists := l.sources[name]; !exists {
		l.order = append(l.order, name)
	}
	l.sources[name] = src
	return true
}

// Source visible to a home under name
func (r *Registry) Get(name, homeDir string) (Source, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, sources := r.merged(r.scopeKey(homeDir))
	src, ok := sources[name]
	return src, ok
}

// Sources visible to a home in apply order
func (r *Registry) List(homeDir string) []Source {
	r.mu.Lock()
	defer r.mu.Unlock()
	order, sources := r.merged(r.scopeKey(homeDir))
	out := make([]Source, 0, len(order))
	for _, name := range order {
		out = append(out, sources[name])
	}
	return out
}

// Sources a plugin registered, process wide ones and those of a home
func (r *Registry) Plugins(homeDir string) []Source {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Source
	seen := map[string]int{}
	for _, name := range r.order {
		if r.origins[name] == Plugin {
			seen[name] = len(out)
			out = append(out, r.global[name])
		}
	}
	if l := r.scoped[r.scopeKey(homeDir)]; l != nil {
		for _, name := range l.order {
			if at, ok := seen[name]; ok {
				out[at] = l.sources[name]
				continue
			}
			out = append(out, l.sources[name])
		}
	}
	return out
}

// Registration one layer holds under name, the process wide layer for an empty home
func (r *Registry) Snapshot(name, homeDir string) (Source, bool) {
	scope := r.scopeKey(homeDir)
	r.mu.Lock()
	defer r.mu.Unlock()
	if scope == "" {
		src, ok := r.global[name]
		return src, ok
	}
	if l := r.scoped[scope]; l != nil {
		src, ok := l.sources[name]
		return src, ok
	}
	return nil, false
}

// Puts back previous under name when current is still what the layer holds, nil removing the registration
func (r *Registry) Restore(name string, current, previous Source, homeDir string) bool {
	scope := r.scopeKey(homeDir)
	r.mu.Lock()
	defer r.mu.Unlock()
	if scope == "" {
		if held, ok := r.global[name]; !ok || held != current {
			return false
		}
		if previous != nil {
			r.global[name] = previous
			return true
		}
		delete(r.global, name)
		delete(r.origins, name)
		r.order = without(r.order, name)
		return true
	}
	l := r.scoped[scope]
	if l == nil {
		return false
	}
	if held, ok := l.sources[name]; !ok || held != current {
		return false
	}
	if previous != nil {
		l.sources[name] = previous
		return true
	}
	delete(l.sources, name)
	l.order = without(l.order, name)
	if len(l.sources) == 0 {
		delete(r.scoped, scope)
	}
	return true
}

// List with one name left out
func without(list []string, name string) []string {
	out := list[:0]
	for _, item := range list {
		if item != name {
			out = append(out, item)
		}
	}
	return out
}

// Subsection of the secrets section for one source
func sourceSection(secrets config.Raw, name string) config.Raw {
	node := sectionOf(secrets).sub(name).node
	if node == nil {
		return config.Raw{}
	}
	raw, err := config.RawOf(node)
	if err != nil {
		return config.Raw{}
	}
	return raw
}

// Enabled sources of a home, the configured order first and registration order after it
func (r *Registry) enabled(secrets config.Raw, homeDir string) []Source {
	listed := r.List(homeDir)
	byName := make(map[string]Source, len(listed))
	var known []string
	for _, src := range listed {
		byName[src.Name()] = src
		known = append(known, src.Name())
	}
	explicit, _ := sectionOf(secrets).stringList("sources")
	var order, unknown []string
	seen := map[string]bool{}
	for _, name := range explicit {
		if _, ok := byName[name]; !ok {
			unknown = append(unknown, name)
			continue
		}
		if !seen[name] {
			seen[name] = true
			order = append(order, name)
		}
	}
	if len(unknown) > 0 {
		names := strings.Join(known, ", ")
		if names == "" {
			names = "none"
		}
		slog.Warn(fmt.Sprintf("secrets.sources names unknown source(s): %s (known: %s)", strings.Join(unknown, ", "), names))
	}
	for _, name := range known {
		if !seen[name] {
			seen[name] = true
			order = append(order, name)
		}
	}
	var out []Source
	for _, name := range order {
		if byName[name].Enabled(sourceSection(secrets, name)) {
			out = append(out, byName[name])
		}
	}
	return out
}

// Names of the sources an apply pass for a home would fetch now
func (r *Registry) EnabledNames(secrets config.Raw, homeDir string) []string {
	var names []string
	for _, src := range r.enabled(secrets, homeDir) {
		names = append(names, src.Name())
	}
	return names
}

// Variables holding the profile name of a process started for a named profile
const (
	envProfileName = "ICEBATH_PROFILE_NAME"
	envProfile     = "ICEBATH_PROFILE"
)

// Name of the profile a home serves, empty for the default profile
func activeProfileName(homeDir string) string {
	if homeDir != "" && filepath.Base(filepath.Dir(homeDir)) == "profiles" && filepath.Base(homeDir) != "" {
		return filepath.Base(homeDir)
	}
	for _, name := range []string{envProfileName, envProfile} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" && value != "default" {
			return value
		}
	}
	return ""
}

// Name endings that mark a variable as a credential worth an unsuffixed alias
var aliasSuffixes = []string{"_API_KEY", "_TOKEN", "_SECRET", "_KEY", "_PASSWORD"}

// Unsuffixed credential name a profile suffixed variable also hydrates, empty when there is none
func profileAliasTarget(name, profile string) string {
	suffix := "_" + strings.ToUpper(strings.ReplaceAll(profile, "-", "_"))
	if profile == "" || !strings.HasSuffix(name, suffix) {
		return ""
	}
	alias := strings.TrimSuffix(name, suffix)
	if !ValidEnvName(alias) {
		return ""
	}
	for _, ending := range aliasSuffixes {
		if strings.HasSuffix(alias, ending) {
			return alias
		}
	}
	return ""
}

// Runs one fetch under its wall clock budget, a late result discarded and reported as a timeout
func fetchWithTimeout(ctx context.Context, src Source, in FetchInput) Result {
	timeout := src.Timeout(in.Config)
	bounded, cancel := context.WithTimeout(ctx, timeout)
	done := make(chan Result, 1)
	go func() {
		defer cancel()
		defer func() {
			if recovered := recover(); recovered != nil {
				done <- Result{}.fail(fmt.Sprintf("fetch raised %v", recovered), Internal)
			}
		}()
		done <- src.Fetch(bounded, in)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-done:
		return result
	case <-timer.C:
		cancel()
		return Result{}.fail(fmt.Sprintf("fetch exceeded %.0fs budget — startup continued without this source (raise secrets.%s.timeout_seconds if the backend is just slow)",
			timeout.Seconds(), src.Name()), Timeout)
	case <-ctx.Done():
		cancel()
		return Result{}.fail("fetch cancelled: "+ctx.Err().Error(), Internal)
	}
}

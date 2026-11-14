package providers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
)

type CustomNames interface {
	HasNamedCustomProvider(ctx context.Context, name string) bool
}

type RegistryOptions struct {
	Builtin []Profile
	Homes   *home.Resolver
	Loader  PluginLoader
	Config  *config.Store
	Custom  CustomNames
	Ref     *RegistryRef
	Safe    bool
}

type layer struct {
	key       string
	home      string
	scan      sync.Mutex
	profiles  map[string]Profile
	order     []string
	aliases   map[string]string
	stamps    [2]stamp
	stamped   bool
	checkedAt time.Time
	misses    map[string]time.Time
	errors    map[string]string
	loaded    map[string]bool
}

type hostMemoKey struct {
	home     string
	host     string
	metadata bool
}

type hostMemoEntry struct {
	gen     uint64
	profile Profile
}

type customMemo struct {
	rev     config.Revision
	known   bool
	answers map[string]bool
}

type Registry struct {
	homes  *home.Resolver
	loader PluginLoader
	cfg    *config.Store
	custom CustomNames
	safe   bool

	bedrock BedrockOwner
	vertex  VertexOwner
	entra   EntraOwner

	mu            sync.RWMutex
	shared        map[string]Profile
	sharedOrder   []string
	sharedAliases map[string]string
	bundled       []Profile
	layers        map[string]*layer
	hostMemo      map[hostMemoKey]hostMemoEntry
	customMemos   map[string]*customMemo
	rowViews      map[string]*rowView
	policy        *policy
	gen           atomic.Uint64
}

var ErrRegistryOptions = errors.New("providers: registry needs a home resolver, a plugin loader, a config store and a custom provider resolver")

func NewRegistry(o RegistryOptions) (*Registry, error) {
	if o.Homes == nil || o.Loader == nil || o.Config == nil || o.Custom == nil {
		return nil, ErrRegistryOptions
	}
	r := &Registry{
		homes: o.Homes, loader: o.Loader, cfg: o.Config, custom: o.Custom, safe: o.Safe,
		shared: map[string]Profile{}, sharedAliases: map[string]string{},
		layers: map[string]*layer{}, hostMemo: map[hostMemoKey]hostMemoEntry{}, customMemos: map[string]*customMemo{},
		rowViews: map[string]*rowView{}, policy: newPolicy(),
	}
	if o.Ref != nil {
		o.Ref.r.Store(r)
	}
	for _, p := range o.Builtin {
		if owner, ok := p.(BedrockOwner); ok {
			r.bedrock = owner
		}
		if owner, ok := p.(VertexOwner); ok {
			r.vertex = owner
		}
		if owner, ok := p.(EntraOwner); ok {
			r.entra = owner
		}
	}
	if o.Safe {
		return r, nil
	}
	seen := map[string]int{}
	for _, p := range o.Builtin {
		name := p.Spec().Name
		if i, ok := seen[name]; ok {
			r.bundled[i] = p
		} else {
			seen[name] = len(r.bundled)
			r.bundled = append(r.bundled, p)
		}
		r.registerShared(p)
	}
	registerEnvMetadata(o.Config.EnvRegistry(), r.bundled)
	return r, nil
}

func (r *Registry) registerShared(p Profile) {
	s := p.Spec()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gen.Add(1)
	if _, ok := r.shared[s.Name]; !ok {
		r.sharedOrder = append(r.sharedOrder, s.Name)
	}
	r.shared[s.Name] = p
	for _, alias := range s.Aliases {
		r.sharedAliases[alias] = s.Name
	}
}

func (l *layer) register(r *Registry, p Profile) {
	s := p.Spec()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gen.Add(1)
	if _, ok := l.profiles[s.Name]; !ok {
		l.order = append(l.order, s.Name)
	}
	l.profiles[s.Name] = p
	for _, alias := range s.Aliases {
		l.aliases[alias] = s.Name
	}
}

func (r *Registry) Generation() uint64 { return r.gen.Load() }

func (r *Registry) BedrockOwner() (BedrockOwner, bool) { return r.bedrock, r.bedrock != nil }

func (r *Registry) VertexOwner() (VertexOwner, bool) { return r.vertex, r.vertex != nil }

func (r *Registry) EntraOwner() (EntraOwner, bool) { return r.entra, r.entra != nil }

func (r *Registry) Bundled() []Profile {
	if r.safe {
		return nil
	}
	return slices.Clone(r.bundled)
}

func (r *Registry) boundLayer(ctx context.Context) *layer {
	homeDir, key := "", ""
	if h, err := r.homes.Home(ctx); err == nil {
		if k, kerr := r.homes.Key(h); kerr == nil {
			homeDir, key = h, k
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.layers[key]
	if !ok {
		l = &layer{key: key, home: homeDir, profiles: map[string]Profile{}, aliases: map[string]string{},
			misses: map[string]time.Time{}, errors: map[string]string{}, loaded: map[string]bool{}}
		r.layers[key] = l
	}
	return l
}

func (r *Registry) refresh(ctx context.Context, l *layer, force bool) bool {
	if l.home == "" || r.safe {
		return false
	}
	l.scan.Lock()
	defer l.scan.Unlock()
	now := time.Now()
	if !force && !l.checkedAt.IsZero() && now.Sub(l.checkedAt) < stampTTL {
		return false
	}
	stamps := readStamps(l.home)
	if !l.stamped || stamps != l.stamps {
		settled := stampsSettled(stamps)
		r.scanLayer(ctx, l)
		l.stamps, l.stamped = stamps, settled
	}
	l.checkedAt = now
	return true
}

func (r *Registry) scanLayer(ctx context.Context, l *layer) {
	for _, dir := range providerPluginDirs(l.home, l.key) {
		if l.loaded[dir.Name] {
			continue
		}
		profiles, found, err := r.loader.LoadProviders(ctx, dir)
		if err != nil {
			slog.Warn(fmt.Sprintf("Failed to load user provider plugin %s: %v", dir.Name, err))
			r.mu.Lock()
			l.errors[dir.Name] = err.Error()
			r.mu.Unlock()
			continue
		}
		if !found {
			continue
		}
		r.mu.Lock()
		delete(l.errors, dir.Name)
		r.mu.Unlock()
		for _, p := range profiles {
			l.register(r, p)
		}
		l.loaded[dir.Name] = true
	}
}

func (r *Registry) homeLayer(ctx context.Context, force bool) *layer {
	l := r.boundLayer(ctx)
	r.refresh(ctx, l, force)
	return l
}

func (r *Registry) RefreshHome(ctx context.Context) {
	l := r.homeLayer(ctx, true)
	r.mu.Lock()
	clear(l.misses)
	r.mu.Unlock()
}

func (r *Registry) LoadError(ctx context.Context, plugin string) string {
	l := r.homeLayer(ctx, false)
	r.mu.RLock()
	defer r.mu.RUnlock()
	return l.errors[plugin]
}

func (r *Registry) lookup(l *layer, name string) Profile {
	r.mu.RLock()
	defer r.mu.RUnlock()
	canonical, ok := l.aliases[name]
	if !ok || canonical == "" {
		canonical = name
		if shared, ok := r.sharedAliases[name]; ok {
			canonical = shared
		}
	}
	if p, ok := l.profiles[canonical]; ok {
		return p
	}
	return r.shared[canonical]
}

func (r *Registry) missRecheckDue(l *layer, name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if last, ok := l.misses[name]; ok && now.Sub(last) < stampTTL {
		return false
	}
	if len(l.misses) >= missLimit {
		clear(l.misses)
	}
	l.misses[name] = now
	return true
}

func (r *Registry) Get(ctx context.Context, name string) (Profile, bool) {
	if r.safe {
		return nil, false
	}
	l := r.boundLayer(ctx)
	checked := r.refresh(ctx, l, false)
	p := r.lookup(l, name)
	customRoute := strings.HasPrefix(strings.ToLower(name), "custom:")
	if p == nil && !customRoute && !checked && r.missRecheckDue(l, name) {
		if r.refresh(ctx, l, true) {
			p = r.lookup(l, name)
		}
	}
	if p == nil && customRoute {
		p = r.lookup(l, ProviderCustom)
	}
	if p == nil && !customRoute && r.hasNamedCustom(ctx, l, name) {
		p = r.lookup(l, ProviderCustom)
	}
	return p, p != nil
}

func (r *Registry) hasNamedCustom(ctx context.Context, l *layer, name string) bool {
	rev, known := r.cfg.ConfigCacheRevision(home.WithOverride(ctx, l.home))
	r.mu.Lock()
	memo := r.customMemos[l.key]
	if memo == nil || memo.known != known || memo.rev != rev {
		memo = &customMemo{rev: rev, known: known, answers: map[string]bool{}}
		r.customMemos[l.key] = memo
	}
	answer, ok := memo.answers[name]
	r.mu.Unlock()
	if ok {
		return answer
	}
	answer = r.custom.HasNamedCustomProvider(ctx, name)
	r.mu.Lock()
	memo.answers[name] = answer
	r.mu.Unlock()
	return answer
}

func (r *Registry) BoundKey(ctx context.Context) (string, uint64) {
	l := r.homeLayer(ctx, false)
	return l.key, r.Generation()
}

func (r *Registry) Source(ctx context.Context, name string) string {
	l := r.homeLayer(ctx, false)
	r.mu.RLock()
	defer r.mu.RUnlock()
	canonical, ok := l.aliases[name]
	if !ok || canonical == "" {
		canonical = name
		if shared, ok := r.sharedAliases[name]; ok {
			canonical = shared
		}
	}
	if _, ok := l.profiles[canonical]; ok {
		return "user"
	}
	if _, ok := r.shared[canonical]; ok {
		return "bundled"
	}
	return ""
}

func (r *Registry) all(l *layer) []Profile {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Profile, 0, len(r.sharedOrder)+len(l.order))
	for _, name := range r.sharedOrder {
		if _, shadowed := l.profiles[name]; !shadowed {
			out = append(out, r.shared[name])
		}
	}
	for _, name := range l.order {
		out = append(out, l.profiles[name])
	}
	return out
}

func (r *Registry) All(ctx context.Context) []Profile {
	if r.safe {
		return nil
	}
	return r.all(r.homeLayer(ctx, false))
}

func hostClaims(host, claimed string) bool {
	claimed = strings.TrimRight(strings.ToLower(strings.TrimSpace(claimed)), ".")
	return claimed != "" && (host == claimed || strings.HasSuffix(host, "."+claimed))
}

func (r *Registry) ForHost(ctx context.Context, rawURL string, metadata bool) (Profile, bool) {
	host := ""
	if rawURL != "" {
		host = URLHostname(rawURL)
	}
	if host == "" || r.safe {
		return nil, false
	}
	l := r.homeLayer(ctx, false)
	gen := r.Generation()
	key := hostMemoKey{home: l.key, host: host, metadata: metadata}
	r.mu.RLock()
	cached, ok := r.hostMemo[key]
	r.mu.RUnlock()
	if ok && cached.gen == gen {
		return cached.profile, cached.profile != nil
	}
	var found Profile
	for _, p := range r.all(l) {
		s := p.Spec()
		hosts := s.Hostnames
		if metadata {
			hosts = append(slices.Clone(s.Hostnames), s.MetadataHostnames...)
		}
		if slices.ContainsFunc(hosts, func(c string) bool { return hostClaims(host, c) }) {
			found = p
			break
		}
	}
	r.mu.Lock()
	r.hostMemo[key] = hostMemoEntry{gen: gen, profile: found}
	r.mu.Unlock()
	return found, found != nil
}

func (r *Registry) Listed(ctx context.Context, p Profile, pools PoolReader) (bool, error) {
	if gate, ok := p.(ListingGate); ok {
		return gate.Listed(ctx, pools)
	}
	s := p.Spec()
	if !s.Hidden {
		return true, nil
	}
	names, err := pools.PoolProviders(ctx)
	if err != nil {
		return false, err
	}
	return slices.Contains(names, s.Name), nil
}

func (r *Registry) Unlisted(ctx context.Context, pools PoolReader) map[string]bool {
	hidden := map[string]bool{}
	for _, p := range r.All(ctx) {
		listed, err := r.Listed(ctx, p, pools)
		if err != nil {
			slog.Debug(fmt.Sprintf("provider %s listed() raised; hiding it", p.Spec().Name), "error", err)
		}
		if err != nil || !listed {
			hidden[p.Spec().Name] = true
		}
	}
	return hidden
}

func (r *Registry) RejectsVisionToolMessages(ctx context.Context, provider, model string, routingAggregator bool) bool {
	name := strings.ToLower(strings.TrimSpace(provider))
	if p, ok := r.Get(ctx, name); ok && p.Spec().RejectsVisionToolMessages {
		return true
	}
	if !routingAggregator {
		return false
	}
	target, _, found := strings.Cut(strings.TrimSpace(model), "/")
	if !found || target == "" {
		return false
	}
	p, ok := r.Get(ctx, strings.ToLower(strings.TrimSpace(target)))
	return ok && p.Spec().RejectsVisionToolMessages
}

func (r *Registry) resolver(ctx context.Context) func(name string) (user, shared Profile) {
	l := r.homeLayer(ctx, false)
	return func(name string) (Profile, Profile) {
		r.mu.RLock()
		defer r.mu.RUnlock()
		canonical := name
		if a, ok := r.sharedAliases[name]; ok {
			canonical = a
		}
		userName, ok := l.aliases[name]
		if !ok || userName == "" {
			userName = canonical
		}
		return l.profiles[userName], r.shared[canonical]
	}
}

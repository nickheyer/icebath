package env

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/model/secret"
	"github.com/nickheyer/icebath/internal/tool"
)

const (
	skillRefusal = "env passthrough: refusing to register icebath provider " +
		"credential %s (blocked by the provider env blocklist). " +
		"Skills must not override the execute_code sandbox's " +
		"credential scrubbing; see GHSA-rhgp-j443-p4rf."
	configRefusal = "env passthrough: refusing to register icebath " +
		"provider credential %s from config.yaml (blocked " +
		"by the provider env blocklist). Operator " +
		"configuration must not override the execute_code " +
		"sandbox's credential scrubbing; see " +
		"GHSA-rhgp-j443-p4rf."
)

type PassthroughOptions struct {
	Policy  *Policy
	Config  *config.Store
	Homes   *home.Resolver
	Secrets *secret.Scope
}

type Passthrough struct {
	policy  *Policy
	cfg     *config.Store
	homes   *home.Resolver
	secrets *secret.Scope

	mu      sync.Mutex
	allowed map[string]bool
	byHome  map[string]map[string]bool
}

func NewPassthrough(o PassthroughOptions) (*Passthrough, error) {
	switch {
	case o.Policy == nil:
		return nil, &tool.UnfilledError{Name: "env.PassthroughOptions.Policy"}
	case o.Config == nil:
		return nil, &tool.UnfilledError{Name: "env.PassthroughOptions.Config"}
	case o.Homes == nil:
		return nil, &tool.UnfilledError{Name: "env.PassthroughOptions.Homes"}
	case o.Secrets == nil:
		return nil, &tool.UnfilledError{Name: "env.PassthroughOptions.Secrets"}
	}
	return &Passthrough{policy: o.Policy, cfg: o.Config, homes: o.Homes, secrets: o.Secrets,
		allowed: map[string]bool{}, byHome: map[string]map[string]bool{}}, nil
}

func (p *Passthrough) providerCredential(ctx context.Context, name string) (bool, error) {
	if IsInternalSecret(name) {
		return true, nil
	}
	return p.policy.ProviderBlocklisted(ctx, name, nil)
}

func (p *Passthrough) accepted(ctx context.Context, names []string, refusal string) ([]string, error) {
	var out []string
	for _, name := range names {
		if name == "" {
			continue
		}
		blocked, err := p.providerCredential(ctx, name)
		if err != nil {
			return out, err
		}
		if blocked {
			slog.Warn(fmt.Sprintf(refusal, quotedPath(name)))
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

func (p *Passthrough) Register(ctx context.Context, names []string) error {
	trimmed := make([]string, len(names))
	for i, n := range names {
		trimmed[i] = strings.TrimSpace(n)
	}
	ok, err := p.accepted(ctx, trimmed, skillRefusal)
	p.mu.Lock()
	for _, n := range ok {
		p.allowed[n] = true
		slog.Debug("env passthrough: registered " + n)
	}
	p.mu.Unlock()
	return err
}

func (p *Passthrough) configured(ctx context.Context) map[string]bool {
	key := ""
	if h, err := p.homes.Home(ctx); err == nil {
		if k, err := p.homes.Key(h); err == nil {
			key = k
		}
	}
	p.mu.Lock()
	cached, ok := p.byHome[key]
	p.mu.Unlock()
	if ok {
		return cached
	}
	result := map[string]bool{}
	cfg, err := p.cfg.Load(ctx)
	if err == nil {
		var items []string
		for _, i := range cfg.Terminal.EnvPassthrough {
			items = append(items, strings.TrimSpace(i))
		}
		var names []string
		names, err = p.accepted(ctx, items, configRefusal)
		for _, n := range names {
			result[n] = true
		}
	}
	if err != nil {
		slog.Debug("Could not read terminal.env_passthrough from config: " + err.Error())
	}
	p.mu.Lock()
	p.byHome[key] = result
	p.mu.Unlock()
	return result
}

func (p *Passthrough) Allowed(ctx context.Context, name string) (bool, error) {
	p.mu.Lock()
	skill := p.allowed[name]
	p.mu.Unlock()
	if !skill && !p.configured(ctx)[name] {
		return false, nil
	}
	blocked, err := p.providerCredential(ctx, name)
	if err != nil {
		return false, err
	}
	return !blocked, nil
}

func (p *Passthrough) All(ctx context.Context) ([]string, error) {
	union := map[string]bool{}
	p.mu.Lock()
	for n := range p.allowed {
		union[n] = true
	}
	p.mu.Unlock()
	for n := range p.configured(ctx) {
		union[n] = true
	}
	var out []string
	for n := range union {
		blocked, err := p.providerCredential(ctx, n)
		if err != nil {
			return nil, err
		}
		if !blocked {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (p *Passthrough) Resolve(ctx context.Context, name string, fallback *string) (*string, error) {
	if config.IsGlobalEnv(name) && fallback != nil {
		return fallback, nil
	}
	multiplex := p.secrets.Multiplexed(ctx)
	if !p.secrets.Bound(ctx) && !multiplex {
		return fallback, nil
	}
	v, ok, err := p.secrets.Lookup(ctx, name)
	if err != nil {
		return nil, err
	}
	if ok {
		return &v, nil
	}
	if multiplex || !p.secrets.Bound(ctx) {
		return nil, nil
	}
	return fallback, nil
}

func (p *Passthrough) ScopedAdditions(ctx context.Context, present map[string]string) (map[string]string, error) {
	scope, ok := p.secrets.Current(ctx)
	if !ok || len(scope) == 0 {
		return map[string]string{}, nil
	}
	names, err := p.All(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, name := range names {
		if _, has := present[name]; has || config.IsGlobalEnv(name) {
			continue
		}
		if v, ok := scope[name]; ok {
			out[name] = v
		}
	}
	return out, nil
}

func (p *Passthrough) Clear() {
	p.mu.Lock()
	p.allowed = map[string]bool{}
	p.mu.Unlock()
}

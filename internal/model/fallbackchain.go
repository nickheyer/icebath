package model

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/auth"
)

func (r *Router) FallbackChain(ctx context.Context, cfg *config.Config) []config.FallbackEntry {
	if cfg == nil {
		return nil
	}
	return r.chainOf(r.bind(ctx), cfg.FallbackProviders, cfg.Extra.FallbackModel)
}

func (r *Router) chainOf(ctx context.Context, chains ...config.FallbackChain) []config.FallbackEntry {
	var out []config.FallbackEntry
	seen := map[[3]string]bool{}
	for _, chain := range chains {
		for _, e := range chain.Entries {
			provider, model := strings.TrimSpace(e.Provider), strings.TrimSpace(e.Model)
			if !e.Valid() || provider == "" || model == "" {
				continue
			}
			e.Provider, e.Model = provider, model
			if base := trimURL(e.BaseURL); base != "" {
				e.BaseURL = base
			}
			identity := [3]string{r.c.o.Providers.NormalizeProvider(ctx, provider), strings.ToLower(model), strings.ToLower(trimURL(e.BaseURL))}
			if !seen[identity] {
				seen[identity] = true
				out = append(out, e)
			}
		}
	}
	return out
}

func (r *Router) ScopedFallbackChain(ctx context.Context, inherited []config.FallbackEntry, declared *config.FallbackChain, pinned bool, owner string) []config.FallbackEntry {
	var def []config.FallbackEntry
	if !pinned && len(inherited) > 0 {
		def = inherited
	}
	if declared == nil {
		return def
	}
	if declared.Other == nil && !declared.Single && len(declared.Entries) == 0 {
		return nil
	}
	normalized := r.chainOf(r.bind(ctx), *declared)
	if len(normalized) == 0 {
		which := "inherited"
		if pinned {
			which = "pinned"
		}
		slog.Warn(fmt.Sprintf("%s fallback_providers has no usable routes; using the %s default", owner, which))
		return def
	}
	return normalized
}

func (r *Router) FallbackEntryAPIKey(ctx context.Context, entry config.FallbackEntry) (string, error) {
	if inline := strings.TrimSpace(entry.APIKey); inline != "" {
		return inline, nil
	}
	if keyEnv := strings.TrimSpace(firstOf(entry.KeyEnv, entry.APIKeyEnv)); keyEnv != "" {
		v, err := r.c.o.Secrets.Get(r.bind(ctx), keyEnv, "")
		return strings.TrimSpace(v), err
	}
	return "", nil
}

func EffectiveRuntimeProvider(entry *config.FallbackEntry, rt *Route) string {
	if rt == nil {
		return ""
	}
	resolved := strings.TrimSpace(rt.Provider)
	if strings.ToLower(resolved) != "custom" {
		return resolved
	}
	requested := rt.Requested
	if requested == "" && entry != nil {
		requested = entry.Provider
	}
	if requested = strings.TrimSpace(requested); requested != "" && strings.ToLower(requested) != "custom" {
		return requested
	}
	return resolved
}

func FallbackNotice(primaryProvider, primaryModel, fallbackProvider, fallbackModel string) string {
	join := func(def string, parts ...string) string {
		var kept []string
		for _, p := range parts {
			if p != "" {
				kept = append(kept, strings.TrimSpace(p))
			}
		}
		if s := strings.Join(kept, "/"); s != "" {
			return s
		}
		return def
	}
	return fmt.Sprintf("⚠️ Provider fallback: %s unavailable; using %s for this response.",
		join("primary", primaryProvider, primaryModel), join("fallback", fallbackProvider, fallbackModel))
}

func (r *Router) ResolveWithFallback(ctx context.Context, t Target) (*Route, *config.FallbackEntry, error) {
	x, err := r.begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	rt, primary := x.resolve(t)
	if primary == nil {
		return rt, nil, nil
	}
	if !IsAuthError(primary) {
		return nil, nil, primary
	}
	for _, entry := range r.FallbackChain(x.ctx, x.cfg) {
		provider, model := strings.ToLower(strings.TrimSpace(entry.Provider)), strings.TrimSpace(entry.Model)
		if provider == "" || model == "" {
			continue
		}
		ft := Target{Provider: provider, Model: model, BaseURL: entry.BaseURL}
		key, err := r.FallbackEntryAPIKey(x.ctx, entry)
		if err != nil {
			return nil, nil, err
		}
		if key != "" {
			ft.APIKey = auth.NewSecret(key)
		}
		frt, ferr := x.resolve(ft)
		if ferr != nil {
			if IsAuthError(ferr) {
				slog.Debug(fmt.Sprintf("Fallback entry %s/%s failed: %s", provider, model, ferr))
			} else {
				slog.Warn(fmt.Sprintf("Fallback entry %s/%s is misconfigured and was skipped: %s", provider, model, ferr))
			}
			continue
		}
		frt.Provider = EffectiveRuntimeProvider(&entry, frt)
		wording, _ := auth.PrimaryFailureWording(primary)
		slog.Warn(fmt.Sprintf("Primary provider %s (%s). Falling back to %s/%s", wording, primary, provider, model))
		chosen := entry
		return frt, &chosen, nil
	}
	return nil, nil, primary
}

func (r *Router) FormatResolveError(ctx context.Context, err error) string {
	if IsAuthError(err) {
		return r.c.o.Auth.FormatError(r.bind(ctx), err)
	}
	return err.Error()
}

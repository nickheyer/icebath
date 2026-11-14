package model

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/portal"
	"github.com/nickheyer/icebath/internal/model/secret"
)

type ProviderQuery struct {
	Requested       string
	ExplicitKey     bool
	ExplicitBaseURL bool
	SkipFreeTier    bool
}

func (r *Router) ResolveProvider(ctx context.Context, q ProviderQuery) (string, error) {
	x, err := r.begin(ctx)
	if err != nil {
		return "", err
	}
	return x.resolveProvider(q)
}

func (r *Router) IsRuntimeProviderRoutable(ctx context.Context, provider string) (bool, error) {
	normalized := strings.ToLower(strings.TrimSpace(provider))
	if normalized == "" {
		return false, nil
	}
	switch normalized {
	case "auto", "openrouter", "custom", "moa":
		return true, nil
	}
	if strings.HasPrefix(normalized, auth.CustomPoolPrefix) {
		return true, nil
	}
	if _, err := r.ResolveProvider(ctx, ProviderQuery{Requested: normalized}); err != nil {
		if IsAuthError(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (x *resolution) pluginAlias(name string) string {
	if alias, ok := auth.ProviderAlias(name); ok {
		return alias
	}
	for _, p := range x.o().Providers.All(x.ctx) {
		for _, a := range p.Spec().Aliases {
			if a == name {
				return p.Spec().Name
			}
		}
	}
	return name
}

func (x *resolution) scopedKeyEnv(name string) (string, error) {
	if name == "" {
		return "", nil
	}
	v, err := x.secret(name, "")
	if errors.Is(err, secret.ErrUnscoped) {
		return strings.TrimSpace(os.Getenv(name)), nil
	}
	return strings.TrimSpace(v), err
}

func (x *resolution) canonicalName(name string) (string, bool) {
	normalized := x.pluginAlias(strings.ToLower(strings.TrimSpace(firstOf(name, "auto"))))
	if normalized == "openrouter" || normalized == "custom" {
		return normalized, true
	}
	_, ok := x.o().Providers.RegistryRow(x.ctx, normalized)
	return normalized, ok
}

func (x *resolution) resolveProvider(q ProviderQuery) (string, error) {
	normalized, known := x.canonicalName(q.Requested)
	if known {
		return normalized, nil
	}
	if normalized != "auto" {
		tail := " Check 'icebath model' for available providers, or run 'icebath doctor' to diagnose config issues."
		if hint := x.o().Auth.UnknownProviderHint(x.ctx, normalized); hint != "" {
			tail = "\n\n" + hint
		}
		return "", authError(fmt.Sprintf("Unknown provider '%s'.%s", normalized, tail), "", "invalid_provider")
	}
	if q.ExplicitKey || q.ExplicitBaseURL {
		return "openrouter", nil
	}
	mapping, cfgProv := x.configModelProvider()
	if cfgProv != "" {
		return cfgProv, nil
	}
	detected, err := x.openRouterAutoDetected()
	if err != nil {
		return "", err
	}
	if detected {
		if err := x.o().Auth.RefuseEnvAdoptionIfConfigCorrupt(x.ctx); err != nil {
			return "", err
		}
		return "openrouter", nil
	}
	oauthActive := x.loggedInOAuthActive(q.SkipFreeTier)
	envProvider, err := x.envKeyAutoDetected(oauthActive)
	if err != nil {
		return "", err
	}
	if envProvider != "" {
		return envProvider, nil
	}
	if oauthActive != "" {
		if mapping {
			slog.Warn(fmt.Sprintf("Provider resolved to logged-in OAuth provider '%s' because "+
				"config.yaml `model` has no `provider` key. If you meant a "+
				"different provider, set `model.provider` explicitly.", oauthActive))
		}
		return oauthActive, nil
	}
	acct := x.o().Portal
	if !q.SkipFreeTier && acct.FreeInferenceActive(x.ctx) {
		return portal.Provider, nil
	}
	if owner, ok := x.o().Providers.BedrockOwner(); ok {
		access, err := owner.Bedrock(x.ctx, !acct.OnLocalDesktopBackend(x.ctx))
		if err != nil {
			return "", err
		}
		if access.HasCredentials(x.ctx) {
			return "bedrock", nil
		}
	}
	shown, err := home.Display(x.r.c.o.Home)
	if err != nil {
		return "", err
	}
	return "", authError("icebath is not connected to any AI provider yet. Run `icebath model` to pick one, type `/login` in chat, "+
		"or add a key with `icebath auth add <provider>`. (Advanced: put an API key such as OPENROUTER_API_KEY in "+
		shown+"/.env.)", "", "no_provider_configured")
}

func (x *resolution) configModelProvider() (mapping bool, provider string) {
	live, err := x.liveConfig()
	if err != nil {
		slog.Debug("Could not read config.yaml model.provider for auto-resolution", "error", err)
		return false, ""
	}
	m := live.Model
	if m.Bare {
		return false, ""
	}
	mapping = m.Default.ID != "" || m.Provider != "" || m.BaseURL != "" || m.APIKey != "" || m.APIMode != "" || m.API != "" ||
		m.KeyEnv != "" || m.APIKeyEnv != "" || m.AuthMode != "" || m.OpenAIRuntime != "" || m.ContextLength != 0
	mapping = mapping && strings.TrimSpace(m.Provider) == ""
	p := x.pluginAlias(strings.ToLower(strings.TrimSpace(m.Provider)))
	if p == "custom" || strings.HasPrefix(p, auth.CustomPoolPrefix) {
		return mapping, "custom"
	}
	if p == "openrouter" {
		return mapping, p
	}
	if _, ok := x.o().Providers.RegistryRow(x.ctx, p); ok && p != "" {
		return mapping, p
	}
	if p != "" {
		if nc, err := x.namedCustomProvider(p, nil, true); err == nil && nc != nil {
			return mapping, "custom"
		}
	}
	if base := strings.TrimSpace(m.BaseURL); base != "" && x.trustworthyForBareCustom(base, p) {
		return mapping, "custom"
	}
	return mapping, ""
}

func (x *resolution) openRouterAutoDetected() (bool, error) {
	key, err := x.scopedKeyEnv("OPENROUTER_API_KEY")
	if err != nil {
		return false, err
	}
	if auth.HasUsableSecret(key, 4) {
		return true, nil
	}
	legacy, err := x.scopedKeyEnv("OPENAI_API_KEY")
	if err != nil {
		return false, err
	}
	if auth.HasUsableSecret(legacy, 4) && auth.LooksLikeOpenRouterKey(legacy) {
		return true, nil
	}
	pool, err := x.o().Auth.Pool(x.ctx, "openrouter")
	if err != nil {
		slog.Debug("Could not check OpenRouter credential pool", "error", err)
		return false, nil
	}
	return pool != nil && pool.HasCredentials(), nil
}

func (x *resolution) loggedInOAuthActive(skipFreeTier bool) string {
	active, err := x.o().Auth.Active(x.ctx)
	if err != nil {
		slog.Debug("Could not pre-read active auth provider", "error", err)
		return ""
	}
	if active == portal.Provider {
		acct := x.o().Portal
		if acct.HasGuest(x.ctx) && (skipFreeTier || !acct.FreeInferenceActive(x.ctx)) {
			return ""
		}
	}
	if active == "" {
		return ""
	}
	if _, ok := x.o().Providers.RegistryRow(x.ctx, active); !ok {
		return ""
	}
	st, err := x.o().Auth.Status(x.ctx, active)
	if err != nil {
		slog.Debug("Could not pre-read active auth provider", "error", err)
		return ""
	}
	if st.LoggedIn {
		return active
	}
	return ""
}

func (x *resolution) envKeyAutoDetected(oauthActive string) (string, error) {
	ambient := map[string]bool{}
	for _, p := range x.o().Providers.All(x.ctx) {
		if p.Spec().AmbientCredentials {
			ambient[p.Spec().Name] = true
		}
	}
	for _, row := range x.o().Providers.RegistryRows(x.ctx) {
		if row.AuthType != "api_key" || row.ID == "lmstudio" || ambient[row.ID] {
			continue
		}
		for _, envVar := range row.APIKeyEnvVars {
			v, err := x.scopedKeyEnv(envVar)
			if err != nil {
				return "", err
			}
			if !auth.HasUsableSecret(v, 4) {
				continue
			}
			if oauthActive != "" && oauthActive != row.ID {
				slog.Warn(fmt.Sprintf("Provider resolved to '%s' via %s, preempting your "+
					"logged-in OAuth provider '%s'. If you meant to use the "+
					"OAuth login, unset %s or set `model.provider` "+
					"explicitly.", row.ID, envVar, oauthActive, envVar))
			}
			return row.ID, nil
		}
	}
	return "", nil
}

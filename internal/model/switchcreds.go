package model

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/catalog"
	"github.com/nickheyer/icebath/internal/model/local"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

func (st *switchState) providerLabel() string {
	cat := st.r.c.o.Catalog
	label := cat.Label(st.ctx, st.target)
	if st.target == "custom" && st.in.CurrentBaseURL != "" {
		label = "Custom endpoint"
	}
	if strings.HasPrefix(st.target, "custom:") {
		if def, ok := cat.ResolveProviderFull(st.ctx, st.target, st.in.User, st.in.Custom); ok {
			label = def.Name
		}
	}
	return label
}

func (r *Router) entryConfiguredKey(ctx context.Context, e config.ProviderEntry, read func(name string) string) string {
	key := strings.TrimSpace(e.APIKey)
	if strings.HasPrefix(key, "${") && strings.HasSuffix(key, "}") {
		key = read(key[2 : len(key)-1])
	}
	if key == "" {
		if keyEnv := strings.TrimSpace(firstOf(e.KeyEnv, e.APIKeyEnv)); keyEnv != "" {
			key = read(keyEnv)
		}
	}
	return key
}

func extraHeadersOf(e config.ProviderEntry) map[string]string {
	if e.ExtraHeaders.Len() == 0 {
		return map[string]string{}
	}
	out := map[string]string{}
	for _, k := range e.ExtraHeaders.Keys() {
		v, _ := e.ExtraHeaders.Get(k)
		out[k] = v
	}
	return out
}

func (st *switchState) userEntry(names ...string) (config.ProviderEntry, bool) {
	for _, n := range names {
		if e, ok := st.in.User.Get(n); ok {
			return e, true
		}
	}
	return config.ProviderEntry{}, false
}

func (st *switchState) credsForSwitchedProvider() *SwitchFailure {
	explicitNorm := strings.ToLower(strings.TrimSpace(st.explicit))
	var userDef providers.Definition
	hasUser := false
	if st.explicit != "" && st.in.User.Len() > 0 {
		if userDef, hasUser = catalog.ResolveUserProvider(explicitNorm, st.in.User); !hasUser {
			userDef, hasUser = catalog.ResolveUserProvider(st.target, st.in.User)
		}
	}
	switch {
	case hasUser && userDef.BaseURL != "":
		ucfg, _ := st.userEntry(explicitNorm, st.target)
		ukey := st.r.entryConfiguredKey(st.ctx, ucfg, func(n string) string { return st.r.scopedKeyEnv(st.ctx, n) })
		st.headers = extraHeadersOf(ucfg)
		if err := st.resolveRuntime(st.target, auth.NewSecret(ukey), userDef.BaseURL); err == nil {
			if !keyPresent(st.cred) {
				st.cred = staticKey(ukey, "")
			}
			st.baseURL = firstOf(st.baseURL, userDef.BaseURL)
		} else {
			st.cred, st.baseURL, st.mode = staticKey(ukey, ""), userDef.BaseURL, ""
		}
	case st.target == "custom" && st.in.CurrentBaseURL != "":
		key, url := st.in.CurrentKey, st.in.CurrentBaseURL
		if st.in.CurrentProvider != "custom" {
			if err := st.resolveRuntime("custom", auth.Secret{}, ""); err == nil && st.baseURL != "" && !st.fellBackToOpenRouterDefault() {
				key, url = st.cred, st.baseURL
			}
		}
		st.cred, st.baseURL = key, url
		st.mode = st.r.c.o.Catalog.DetermineMode(st.ctx, st.target, st.baseURL, "")
	default:
		aliasURL := ""
		if st.alias != "" {
			if da, ok := st.aliases.get(st.alias); ok && st.x.resolvesToCustom(st.target) {
				aliasURL = da.BaseURL
			}
		}
		if err := st.resolveRuntime(st.target, auth.Secret{}, aliasURL); err != nil {
			if slices.Contains(catalog.LlamaCppAliases, strings.ToLower(strings.TrimSpace(st.target))) {
				return st.failOnTarget(err.Error())
			}
			return st.failOnTarget(fmt.Sprintf("%s is not connected: no API key or login was found for it. Add one with `icebath auth add %s`, or pick a connected provider in /model.\n  Details: %s",
				st.label, st.target, err))
		}
	}
	return nil
}

func (st *switchState) openRouterMirror() string {
	v, err := st.r.c.o.Secrets.Get(st.ctx, "OPENROUTER_BASE_URL", "")
	if err != nil {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(v), "/")
}

func (st *switchState) customEndpointSource() string {
	env, err := st.r.c.o.Secrets.Get(st.ctx, "CUSTOM_BASE_URL", "")
	if err != nil {
		return ""
	}
	if env = strings.TrimSpace(env); env != "" {
		return env
	}
	mc := st.x.model()
	base := strings.TrimSpace(mc.BaseURL)
	if base != "" && st.x.trustworthyForBareCustom(base, mc.Provider) {
		return base
	}
	return ""
}

func (st *switchState) fellBackToOpenRouterDefault() bool {
	if mirror := st.openRouterMirror(); mirror != "" && strings.TrimRight(st.baseURL, "/") == mirror && st.customEndpointSource() == "" {
		return true
	}
	return providers.HostMatches(st.baseURL, "openrouter.ai") && !providers.HostMatches(st.in.CurrentBaseURL, "openrouter.ai")
}

func (st *switchState) ollamaConfiguredBase() (config.ProviderEntry, string) {
	cfg, err := st.r.c.o.Config.LoadReadonly(st.ctx)
	if err != nil {
		return config.ProviderEntry{}, ""
	}
	e, _ := cfg.Providers.Get("ollama")
	return e, strings.TrimSpace(firstOf(e.BaseURL, e.API, e.URL))
}

func (st *switchState) credsForCurrentProvider() {
	lr := st.r.c.o.Local
	keep := false
	var ollamaHeaders map[string]string
	if st.in.CurrentProvider == "custom" && st.in.CurrentBaseURL != "" {
		headers, err := lr.OllamaRequestHeaders(st.ctx)
		if err == nil {
			ollamaHeaders = headers
			if _, cfgBase := st.ollamaConfiguredBase(); cfgBase == "" || !local.SameOllamaNativeRoot(st.in.CurrentBaseURL, cfgBase) {
				ollamaHeaders = map[string]string{}
				st.suppressOllama = true
			}
			if keep, err = lr.ShouldUseOllamaNativeCatalog(st.ctx, st.in.CurrentProvider, st.in.CurrentBaseURL, ollamaHeaders); err != nil {
				keep = false
			}
		}
	}
	if keep {
		st.cred = st.in.CurrentKey
		if !keyPresent(st.cred) {
			st.cred = staticKey(noKeyPlaceholder, "")
		}
		st.baseURL = st.in.CurrentBaseURL
		st.mode = st.r.c.o.Catalog.DetermineMode(st.ctx, st.in.CurrentProvider, st.baseURL, "")
		st.headers = ollamaHeaders
		return
	}
	_ = st.resolveRuntime(st.in.CurrentProvider, auth.Secret{}, "")
	cur := st.in.CurrentProvider
	if (cur == "custom" || cur == "local") && st.in.CurrentBaseURL != "" && (st.baseURL == "" || st.fellBackToOpenRouterDefault()) {
		st.baseURL, st.cred = st.in.CurrentBaseURL, st.in.CurrentKey
		st.mode = st.r.c.o.Catalog.DetermineMode(st.ctx, cur, st.baseURL, "")
	}
}

func (st *switchState) applyDirectAliasEndpoint(da DirectAlias) {
	aliasKey := st.r.DirectAliasAPIKey(st.ctx, da)
	sameHost := mayReuseSessionCredential(st.baseURL, da.BaseURL)
	current := keyText(st.cred)
	switch {
	case !aliasKey.Empty():
		st.baseURL, st.cred = da.BaseURL, staticKey(aliasKey.Reveal(), "")
	case keyPresent(st.cred) && current != noKeyPlaceholder && sameHost:
		st.baseURL = da.BaseURL
	default:
		requested, explicit := st.r.DirectAliasRuntimeRequest(st.ctx, da)
		var resolved auth.Credential
		base := ""
		if rt, err := st.r.Resolve(st.ctx, Target{Provider: requested, APIKey: explicit, BaseURL: da.BaseURL, Model: st.newModel}); err == nil {
			resolved, base = rt.Credential, rt.BaseURL
		}
		st.baseURL = firstOf(base, da.BaseURL)
		if keyText(resolved) == noKeyPlaceholder {
			resolved = auth.Credential{}
		}
		switch {
		case keyPresent(resolved):
			st.cred = resolved
		case sameHost && keyPresent(st.cred):
		default:
			st.cred = staticKey(noKeyPlaceholder, "")
		}
	}
	if aliasKey.Empty() && strings.ToLower(strings.TrimSpace(st.target)) == "ollama" {
		entry, cfgBase := st.ollamaConfiguredBase()
		if cfgBase != "" && local.SameOllamaNativeRoot(st.baseURL, cfgBase) {
			read := func(n string) string {
				v, err := st.r.c.o.Secrets.Get(st.ctx, n, "")
				if err != nil {
					return ""
				}
				return strings.TrimSpace(v)
			}
			if k := st.r.entryConfiguredKey(st.ctx, entry, read); k != "" {
				st.cred = staticKey(k, "")
			}
		} else {
			st.headers, st.suppressOllama, st.cred = map[string]string{}, true, staticKey(noKeyPlaceholder, "")
		}
	}
	if !keyPresent(st.cred) {
		st.cred = staticKey(noKeyPlaceholder, "")
	}
	st.mode = ""
}

func (st *switchState) resolveCredentials() *SwitchFailure {
	st.label = st.providerLabel()
	st.cred, st.baseURL = st.in.CurrentKey, st.in.CurrentBaseURL
	if st.providerChanged() || st.explicit != "" {
		if f := st.credsForSwitchedProvider(); f != nil {
			return f
		}
	} else {
		st.credsForCurrentProvider()
	}
	if st.alias != "" {
		if da, ok := st.aliases.get(st.alias); ok && da.BaseURL != "" {
			st.applyDirectAliasEndpoint(da)
			st.aliasApplied = true
		}
	}
	var mandated wire.Mode
	ok := false
	if st.r.c.o.Providers.IsActualRoute(st.ctx, st.target, st.baseURL) {
		mandated, ok = wire.ModeChat, true
	} else {
		mandated, ok = providers.HostMandatedMode(st.baseURL)
	}
	if ok && st.mode != wire.ModeCodexAppServer {
		st.mode = mandated
	}
	if st.mode == "" {
		st.mode = st.r.c.o.Catalog.DetermineMode(st.ctx, st.target, st.baseURL, "")
	}
	return nil
}

func (st *switchState) namedCustomModelID(model string) string {
	provider := strings.ToLower(strings.TrimSpace(st.target))
	if !strings.HasPrefix(provider, "custom:") || !strings.Contains(model, "/") {
		return model
	}
	prefix, candidate, _ := strings.Cut(model, "/")
	prefix, candidate = strings.TrimSpace(prefix), strings.TrimSpace(candidate)
	if prefix == "" || candidate == "" {
		return model
	}
	for _, e := range st.in.Custom {
		slugs := catalog.CustomProviderAliases(e.Text("name"), e.Text("provider_key"))
		if slices.Contains(slugs, provider) && slices.Contains(slugs, "custom:"+strings.ToLower(prefix)) {
			for _, id := range declaredIDsOfNode(yamlChild(e.Node(), "models")) {
				if strings.EqualFold(id, candidate) {
					return id
				}
			}
		}
	}
	return model
}

func (st *switchState) configDeclaresModel() bool {
	if st.in.User.Len() > 0 {
		if e, ok := st.in.User.Get(st.target); ok && providerEnabled(e) && slices.Contains(catalog.DeclaredModelIDs(e.Models), st.newModel) {
			return true
		}
	}
	for _, e := range st.in.Custom {
		aliases := catalog.CustomProviderAliases(e.Text("name"), e.Text("provider_key"))
		if (slices.Contains(aliases, strings.ToLower(st.target)) || e.Text("base_url") == st.baseURL) &&
			(st.newModel == e.Text("model") || slices.Contains(declaredIDsOfNode(yamlChild(e.Node(), "models")), st.newModel)) {
			return true
		}
	}
	return false
}

func (st *switchState) validate() *SwitchFailure {
	cat := st.r.c.o.Catalog
	st.newModel = st.namedCustomModelID(st.newModel)
	st.newModel = cat.NormalizeModelForProvider(st.ctx, st.newModel, st.target)
	if cat.IsKnownNonChatModel(st.newModel) {
		f := st.failOnTarget(fmt.Sprintf("`%s` is a generation model and cannot be used for chat. Pick a chat model, or use image generation for image models.", st.newModel))
		f.NewModel = st.newModel
		return f
	}
	var headers map[string]string
	if strings.ToLower(strings.TrimSpace(st.target)) == "ollama" {
		switch {
		case st.suppressOllama:
			headers = map[string]string{}
		case len(st.headers) > 0:
			headers = st.headers
		default:
			if h, err := st.r.c.o.Local.OllamaRequestHeaders(st.ctx); err == nil {
				headers = h
			}
		}
	} else {
		headers = st.headers
		if len(headers) == 0 {
			if e, ok := st.in.User.Get(st.target); ok {
				headers = extraHeadersOf(e)
			}
		}
	}
	validateAs := st.target
	if !strings.HasPrefix(strings.ToLower(validateAs), "custom") {
		if def, ok := cat.ResolveProviderFull(st.ctx, validateAs, st.in.User, st.in.Custom); ok && def.Source == catalog.RowSourceUserConfig && def.BaseURL != "" {
			validateAs = "custom:" + validateAs
		}
	}
	v, err := cat.ValidateRequestedModel(st.ctx, catalog.ValidationRequest{Model: st.newModel, Provider: validateAs, Key: st.cred,
		BaseURL: st.baseURL, Mode: string(st.mode), Headers: headers})
	if err != nil {
		v = catalog.Verdict{Message: fmt.Sprintf("Could not validate `%s`: %s", st.newModel, err)}
	}
	if !v.Accepted {
		if !st.configDeclaresModel() {
			f := st.failOnTarget(v.Message)
			f.NewModel = st.newModel
			return f
		}
		v = catalog.Verdict{Accepted: true, Persist: true, Message: v.Message}
	}
	st.verdict = v
	return nil
}

func (r *Router) ModelDerivedMode(ctx context.Context, provider, model string, apiKey auth.Secret) (wire.Mode, bool, error) {
	x, err := r.begin(ctx)
	if err != nil {
		return "", false, err
	}
	return r.modelDerivedMode(x, provider, model, apiKey)
}

func (r *Router) modelDerivedMode(x *resolution, provider, model string, apiKey auth.Secret) (wire.Mode, bool, error) {
	reg := r.c.o.Providers
	key := strings.ToLower(strings.TrimSpace(provider))
	name := key
	if family, ok := reg.OpenCodeFamily(x.ctx, key); ok {
		name = family
	}
	switch name {
	case "opencode-zen", "opencode-go", "opencode":
		return reg.OpenCodeModelMode(x.ctx, key, model), true, nil
	case "portal", "icebath-portal":
		m, err := x.portalMode(model)
		if err != nil {
			return "", false, err
		}
		return m, true, nil
	}
	if key == "" {
		return "", false, nil
	}
	p, ok := reg.Get(x.ctx, key)
	if !ok {
		return "", false, nil
	}
	m, ok := reg.ModeForModel(x.ctx, p, model, &apiKey)
	return m, ok, nil
}

func (st *switchState) build() (*SwitchResult, error) {
	cat, reg := st.r.c.o.Catalog, st.r.c.o.Providers
	if derived, ok, err := st.r.modelDerivedMode(st.x, st.target, st.newModel, auth.NewSecret(keyText(st.cred))); err != nil {
		return nil, err
	} else if ok {
		st.mode = derived
	}
	if st.mode == "" {
		st.mode = cat.DetermineMode(st.ctx, st.target, st.baseURL, st.newModel)
	}
	if _, ok := reg.OpenCodeFamily(st.ctx, st.target); ok {
		st.baseURL = reg.NormalizeOpenCodeBaseURL(st.ctx, st.target, st.mode, st.baseURL)
	}
	res := &SwitchResult{NewModel: st.newModel, TargetProvider: st.target, ProviderChanged: st.providerChanged(), Credential: st.cred,
		BaseURL: st.baseURL, Mode: st.mode, ProviderLabel: st.label, ResolvedViaAlias: st.alias, IsGlobal: st.in.IsGlobal,
		AliasEndpointApplied: st.aliasApplied, RequestOverrides: map[string]json.RawMessage{}}
	res.Capabilities, res.HasCapabilities = cat.Capabilities(st.ctx, st.target, st.newModel, true, st.x.cfg)
	res.ModelInfo, res.HasModelInfo = cat.ModelInfo(st.ctx, st.target, st.newModel, true, st.x.cfg)
	if st.verdict.Message != "" {
		res.Warning = st.verdict.Message
	}
	res.UsageNotice = st.r.c.o.LargeModelNotice(st.ctx, st.newModel, st.target)
	if nc, err := st.x.namedCustomProvider(st.target, nil, false); err == nil && nc != nil && len(nc.ExtraBody) > 0 {
		res.RequestOverrides["extra_body"] = objectJSON(nc.ExtraBody)
	}
	return res, nil
}

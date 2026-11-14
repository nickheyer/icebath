package model

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/catalog"
	"github.com/nickheyer/icebath/internal/model/local"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

var directAPIBaseURLs = map[string]string{"openai": "https://api.openai.com/v1"}

type namedCustom struct {
	Name         string
	BaseURL      string
	APIKey       string
	Model        string
	KeyCmd       string
	KeyEnv       string
	ProviderKey  string
	APIMode      wire.Mode
	ExtraBody    map[string]json.RawMessage
	ExtraHeaders map[string]string
	Capabilities map[string]bool
}

func (c *namedCustom) declaresCredential() bool {
	return strings.TrimSpace(c.APIKey) != "" || strings.TrimSpace(c.KeyEnv) != "" || strings.TrimSpace(c.KeyCmd) != ""
}

type ConfigPath struct {
	Section string
	Key     string
	Index   int
}

func (x *resolution) shadowedByBuiltin(norm string) bool {
	if norm == "custom" || strings.HasPrefix(norm, "custom:") {
		return false
	}
	canonical, known := x.canonicalName(norm)
	if !known && canonical == "auto" {
		resolved, err := x.resolveProvider(ProviderQuery{Requested: norm})
		if err != nil {
			return false
		}
		canonical, known = resolved, true
	}
	return known && strings.ToLower(strings.TrimSpace(canonical)) == norm
}

func (x *resolution) endpointsOf(cfg *config.Config) ([]config.CustomEndpoint, error) {
	if cfg == nil || cfg == x.cfg {
		return x.customEndpoints()
	}
	tree, err := config.ValueNode(cfg)
	if err != nil {
		return nil, err
	}
	return x.o().Config.CustomEndpointsOf(tree), nil
}

func rawMapJSON(m config.Map[config.Raw]) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for _, k := range m.Keys() {
		v, _ := m.Get(k)
		out[k] = jsonOfNode(v.Node())
	}
	return out
}

func rawMapBools(m config.Map[config.Raw]) map[string]bool {
	out := map[string]bool{}
	for _, k := range m.Keys() {
		v, _ := m.Get(k)
		var s config.Scalar
		if n := v.Node(); n != nil && n.Kind == yaml.ScalarNode && n.Decode(&s) == nil {
			if b, ok := s.AsBool(); ok {
				out[k] = b
			}
		}
	}
	return out
}

func (x *resolution) namedCustomProvider(requested string, cfg *config.Config, resolveCreds bool) (*namedCustom, error) {
	nc, _, err := x.selectNamedCustom(requested, cfg, resolveCreds)
	return nc, err
}

func (x *resolution) selectNamedCustom(requested string, cfg *config.Config, resolveCreds bool) (*namedCustom, ConfigPath, error) {
	norm := normalizeCustomName(requested)
	if norm == "" || norm == "auto" || x.shadowedByBuiltin(norm) {
		return nil, ConfigPath{}, nil
	}
	if cfg == nil {
		cfg = x.cfg
	}
	for _, key := range cfg.Providers.Keys() {
		entry, _ := cfg.Providers.Get(key)
		if !entry.Valid() || !providerEnabled(entry) {
			continue
		}
		if !slices.Contains(catalog.CustomProviderAliases(firstOf(entry.Name, key), key), norm) {
			continue
		}
		base := firstOf(entry.API, entry.URL, entry.BaseURL)
		if base == "" {
			continue
		}
		keyEnv := strings.TrimSpace(firstOf(entry.KeyEnv, entry.APIKeyEnv))
		apiKey := ""
		if keyEnv != "" && resolveCreds {
			v, err := x.secretTrim(keyEnv)
			if err != nil {
				return nil, ConfigPath{}, err
			}
			apiKey = v
		}
		nc := &namedCustom{
			Name: firstOf(entry.Name, key), BaseURL: strings.TrimSpace(base), APIKey: firstOf(apiKey, strings.TrimSpace(entry.APIKey)),
			Model: entry.DefaultModel, KeyCmd: strings.TrimSpace(entry.KeyCmd), KeyEnv: keyEnv, ProviderKey: strings.TrimSpace(key),
			APIMode: x.parseMode(firstOf(entry.APIMode, entry.Transport)),
		}
		if entry.ExtraBody.Len() > 0 {
			nc.ExtraBody = rawMapJSON(entry.ExtraBody)
		}
		if entry.ExtraHeaders.Len() > 0 {
			nc.ExtraHeaders = map[string]string{}
			for _, k := range entry.ExtraHeaders.Keys() {
				nc.ExtraHeaders[k], _ = entry.ExtraHeaders.Get(k)
			}
		}
		if caps := rawMapBools(entry.Capabilities); len(caps) > 0 {
			nc.Capabilities = caps
		}
		return nc, ConfigPath{Section: "providers", Key: key}, nil
	}
	if other := cfg.Extra.CustomProviders.Other; other != nil && other.Kind == yaml.MappingNode {
		slog.Warn("custom_providers in config.yaml is a dict, not a list. " +
			"Each entry must be prefixed with '-' in YAML. " +
			"Run 'icebath doctor' for details.")
		return nil, ConfigPath{}, nil
	}
	endpoints, err := x.endpointsOf(cfg)
	if err != nil {
		return nil, ConfigPath{}, err
	}
	for _, e := range endpoints {
		name, base := e.Text("name"), e.Text("base_url")
		providerKey := strings.TrimSpace(e.Text("provider_key"))
		if !slices.Contains(catalog.CustomProviderAliases(name, providerKey), norm) {
			continue
		}
		node := e.Node()
		nc := &namedCustom{
			Name: strings.TrimSpace(name), BaseURL: strings.TrimSpace(base), APIKey: strings.TrimSpace(e.Text("api_key")),
			Model: strings.TrimSpace(e.Text("model")), KeyEnv: strings.TrimSpace(e.Text("key_env")), ProviderKey: providerKey,
			APIMode: x.parseMode(e.Text("api_mode")), ExtraBody: objectMapOfNode(yamlChild(node, "extra_body")),
		}
		if headers := e.ExtraHeaders(); len(headers) > 0 {
			nc.ExtraHeaders = headerMap(headers)
		}
		if caps := boolMapOfNode(yamlChild(node, "capabilities")); len(caps) > 0 {
			nc.Capabilities = caps
		}
		path, err := x.compatibleEntryPath(cfg, e)
		if err != nil {
			return nil, ConfigPath{}, err
		}
		return nc, path, nil
	}
	return nil, ConfigPath{}, nil
}

func (x *resolution) compatibleEntryPath(cfg *config.Config, e config.CustomEndpoint) (ConfigPath, error) {
	if key := e.Text("provider_key"); key != "" {
		for _, k := range cfg.Providers.Keys() {
			if strings.TrimSpace(k) == key {
				return ConfigPath{Section: "providers", Key: k}, nil
			}
		}
		return ConfigPath{}, nil
	}
	want := string(jsonOfNode(e.Node()))
	for i, raw := range cfg.Extra.CustomProviders.Entries {
		node, err := config.ValueNode(raw)
		if err != nil {
			return ConfigPath{}, err
		}
		tree := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "custom_providers"},
			{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{node}},
		}}
		for _, n := range x.o().Config.CustomEndpointsOf(tree) {
			if string(jsonOfNode(n.Node())) == want {
				return ConfigPath{Section: "custom_providers", Index: i}, nil
			}
		}
	}
	return ConfigPath{}, nil
}

func (r *Router) NamedCustomConfigPath(ctx context.Context, requested string, cfg *config.Config) (ConfigPath, bool, error) {
	x, err := r.begin(ctx)
	if err != nil {
		return ConfigPath{}, false, err
	}
	nc, path, err := x.selectNamedCustom(requested, cfg, false)
	if err != nil || nc == nil || path.Section == "" {
		return ConfigPath{}, false, err
	}
	return path, true, nil
}

func (r *Router) CustomConfigPathForURL(ctx context.Context, baseURL string, cfg *config.Config) (ConfigPath, bool, error) {
	x, err := r.begin(ctx)
	if err != nil {
		return ConfigPath{}, false, err
	}
	if cfg == nil {
		cfg = x.cfg
	}
	url := trimURL(baseURL)
	if url == "" {
		return ConfigPath{}, false, nil
	}
	if other := cfg.Extra.CustomProviders.Other; other != nil && other.Kind == yaml.MappingNode {
		return ConfigPath{}, false, nil
	}
	endpoints, err := x.endpointsOf(cfg)
	if err != nil {
		return ConfigPath{}, false, err
	}
	for _, e := range endpoints {
		if trimURL(e.Text("base_url")) == url {
			path, err := x.compatibleEntryPath(cfg, e)
			if err != nil || path.Section == "" {
				return ConfigPath{}, false, err
			}
			return path, true, nil
		}
	}
	return ConfigPath{}, false, nil
}

func (r *Router) HasNamedCustomProvider(ctx context.Context, name string) bool {
	x, err := r.begin(ctx)
	if err != nil {
		return false
	}
	nc, err := x.namedCustomProvider(name, nil, true)
	return err == nil && nc != nil
}

func (x *resolution) codexProviderID(requested string) (string, bool) {
	if n := normalizeCustomName(requested); n == "" || n == "custom" {
		return "", false
	}
	nc, err := x.namedCustomProvider(requested, nil, true)
	if err != nil || nc == nil {
		return "", false
	}
	_, id, _ := strings.Cut(catalog.CustomProviderSlug(nc.Name, nc.ProviderKey), ":")
	return id, id != ""
}

func (r *Router) CodexProviderID(ctx context.Context, requested string) (string, bool) {
	x, err := r.begin(ctx)
	if err != nil {
		return "", false
	}
	return x.codexProviderID(requested)
}

type customRecord struct {
	url          string
	model        string
	defaultModel string
	models       []string
}

func (rec customRecord) serves(target string) bool {
	for _, v := range []string{rec.model, rec.defaultModel} {
		if strings.ToLower(strings.TrimSpace(v)) == target {
			return true
		}
	}
	for _, id := range rec.models {
		if strings.ToLower(strings.TrimSpace(id)) == target {
			return true
		}
	}
	return false
}

func routeKey(v string) string { return catalog.NormalizeRouteBaseURL(strings.TrimSpace(v)) }

func modelIDsOfNode(n *yaml.Node) []string {
	var out []string
	switch {
	case n == nil:
	case n.Kind == yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			out = append(out, n.Content[i].Value)
		}
	case n.Kind == yaml.SequenceNode:
		for _, item := range n.Content {
			switch item.Kind {
			case yaml.ScalarNode:
				out = append(out, item.Value)
			case yaml.MappingNode:
				out = append(out, firstOf(nodeText(yamlChild(item, "id")), nodeText(yamlChild(item, "name"))))
			}
		}
	}
	return out
}

func nodeText(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

func (x *resolution) findCustomIdentity(matches func(customRecord) bool, cfg *config.Config, prefer string) (string, error) {
	if cfg == nil {
		cfg = x.cfg
	}
	first := ""
	owned := map[string]bool{}
	for _, key := range cfg.Providers.Keys() {
		entry, _ := cfg.Providers.Get(key)
		if !entry.Valid() || !providerEnabled(entry) {
			continue
		}
		slug := catalog.CustomProviderSlug(key, key)
		url := firstOf(entry.API, entry.URL, entry.BaseURL)
		if url != "" {
			owned[slug] = true
		}
		rec := customRecord{url: url, model: entry.Model, defaultModel: entry.DefaultModel, models: entry.Models.IDs()}
		if matches(rec) {
			if prefer == "" || slug == prefer {
				return slug, nil
			}
			first = firstOf(first, slug)
		}
	}
	endpoints, err := x.endpointsOf(cfg)
	if err != nil {
		slog.Debug("custom provider identity lookup could not read the endpoint view", "error", err)
		endpoints = nil
	}
	for _, e := range endpoints {
		name := e.Text("name")
		if strings.TrimSpace(name) == "" {
			continue
		}
		node := e.Node()
		rec := customRecord{url: e.Text("base_url"), model: e.Text("model"), defaultModel: e.Text("default_model"), models: modelIDsOfNode(yamlChild(node, "models"))}
		if !matches(rec) {
			continue
		}
		slug := catalog.CustomProviderSlug(name, e.Text("provider_key"))
		if owned[slug] && e.Text("provider_key") == "" {
			continue
		}
		if prefer == "" || slug == prefer {
			return slug, nil
		}
		first = firstOf(first, slug)
	}
	return first, nil
}

func (x *resolution) identityByURL(baseURL string) (string, error) {
	target := routeKey(baseURL)
	if target == "" {
		return "", nil
	}
	return x.findCustomIdentity(func(rec customRecord) bool { return routeKey(rec.url) == target }, nil, "")
}

func (x *resolution) identityByModel(model string) (string, error) {
	target := strings.ToLower(strings.TrimSpace(model))
	if target == "" {
		return "", nil
	}
	return x.findCustomIdentity(func(rec customRecord) bool { return rec.serves(target) }, nil, "")
}

func (r *Router) RoutedCustomIdentity(ctx context.Context, baseURL, model string, cfg *config.Config, held string) (string, error) {
	x, err := r.begin(ctx)
	if err != nil {
		return "", err
	}
	url := routeKey(baseURL)
	target := strings.ToLower(strings.TrimSpace(model))
	onURL := func(rec customRecord) bool { return url == "" || routeKey(rec.url) == url }
	if target != "" {
		id, err := x.findCustomIdentity(func(rec customRecord) bool { return onURL(rec) && rec.serves(target) }, cfg, held)
		if err != nil || id != "" {
			return id, err
		}
	}
	if url != "" {
		return x.findCustomIdentity(onURL, cfg, "")
	}
	return "", nil
}

func (x *resolution) endpointIdentity(baseURL string) (string, error) {
	id, err := x.identityByURL(baseURL)
	if err != nil || id != "" {
		return id, err
	}
	if ep := x.o().Local.ManagedEndpoint(); ep != nil && routeKey(baseURL) == routeKey(ep.BaseURL) {
		return catalog.LlamaCppProviderID, nil
	}
	return "", nil
}

func (r *Router) CanonicalCustomIdentity(ctx context.Context, baseURL, configProvider, model, requested string) (string, error) {
	x, err := r.begin(ctx)
	if err != nil {
		return "", err
	}
	return x.canonicalCustomIdentity(baseURL, configProvider, model, requested)
}

func (x *resolution) canonicalCustomIdentity(baseURL, configProvider, model, requested string) (string, error) {
	identity := "custom"
	if baseURL != "" {
		var err error
		if identity, err = x.sessionProviderIdentity("custom", requested, baseURL, "", nil); err != nil {
			return "", err
		}
	}
	if identity != "custom" {
		return identity, nil
	}
	if model != "" {
		if id, err := x.identityByModel(model); err != nil || id != "" {
			return id, err
		}
	}
	candidate := strings.TrimSpace(configProvider)
	if candidate == "" {
		candidate = strings.TrimSpace(x.model().Provider)
	}
	if candidate == "" {
		candidate = strings.TrimSpace(os.Getenv(envInferenceProvider))
	}
	norm := normalizeCustomName(candidate)
	if norm == "" || norm == "custom" || norm == "auto" || norm == "openrouter" {
		return "", nil
	}
	nc, err := x.namedCustomProvider(candidate, nil, true)
	if err != nil || nc == nil {
		return "", nil
	}
	id, err := x.identityByURL(nc.BaseURL)
	if err != nil {
		return "", nil
	}
	return firstOf(id, catalog.CustomProviderSlug(norm, "")), nil
}

func (r *Router) SessionProviderIdentity(ctx context.Context, provider, requested, baseURL, model string, cfg *config.Config) (string, error) {
	x, err := r.begin(ctx)
	if err != nil {
		return "", err
	}
	return x.sessionProviderIdentity(provider, requested, baseURL, model, cfg)
}

func (x *resolution) sessionProviderIdentity(provider, requested, baseURL, model string, cfg *config.Config) (string, error) {
	adapter := strings.TrimSpace(provider)
	if strings.ToLower(adapter) != "custom" {
		return adapter, nil
	}
	req := strings.TrimSpace(requested)
	url := routeKey(baseURL)
	if lower := strings.ToLower(req); url != "" && lower != "" && lower != "custom" && lower != "auto" {
		nc, err := x.namedCustomProvider(req, cfg, false)
		if err != nil {
			return "", err
		}
		if nc != nil && routeKey(nc.BaseURL) == url {
			return req, nil
		}
	}
	if url != "" {
		id, err := x.endpointIdentity(strings.TrimSpace(baseURL))
		if err != nil {
			return "", err
		}
		return firstOf(id, adapter), nil
	}
	id, err := x.canonicalCustomIdentity("", "", strings.TrimSpace(model), "")
	if err != nil {
		return "", err
	}
	return firstOf(id, adapter), nil
}

func (r *Router) IsRoutableProvider(ctx context.Context, provider string) bool {
	name := strings.TrimSpace(provider)
	switch strings.ToLower(name) {
	case "", "auto":
		return true
	case "custom":
		return false
	}
	x, err := r.begin(ctx)
	if err != nil {
		return false
	}
	endpoints, err := x.customEndpoints()
	if err != nil {
		return false
	}
	_, ok := x.o().Catalog.ResolveProviderFull(x.ctx, name, x.cfg.Providers, endpoints)
	return ok
}

func (x *resolution) owningEntry(baseURL, providerName string) (auth.CustomEntry, bool) {
	return x.o().Auth.CustomEntryForEndpoint(x.ctx, baseURL, providerName, x.frozenEntries())
}

func (x *resolution) namedCustomEntry(name string) (auth.CustomEntry, bool) {
	normalized := normalizeCustomName(name)
	if normalized == "" {
		return auth.CustomEntry{}, false
	}
	aliases := []string{normalized}
	if suffix, ok := strings.CutPrefix(normalized, auth.CustomPoolPrefix); ok {
		if suffix = normalizeCustomName(suffix); suffix != "" {
			aliases = append(aliases, suffix)
		}
	}
	entries := x.frozenEntries()
	if entries == nil {
		entries = x.o().Auth.CustomEntries(x.ctx)
	}
	for _, e := range entries {
		own := []string{e.Name}
		if key := normalizeCustomName(e.ProviderKey); key != "" && key != e.Name {
			own = append(own, key)
		}
		if slices.ContainsFunc(aliases, func(a string) bool { return slices.Contains(own, a) }) {
			return e, true
		}
	}
	return auth.CustomEntry{}, false
}

func (x *resolution) keyEnvSecret(keyEnv, label string) (string, error) {
	name := strings.TrimSpace(keyEnv)
	if name == "" {
		return "", nil
	}
	v, err := x.secretTrim(name)
	if err != nil {
		return "", err
	}
	if v == "" {
		slog.Warn(label + ": key_env " + name + " is set but the variable is empty/unset — unless another credential " +
			"is configured for it, the request carries the placeholder no-key-required and the " +
			"endpoint will reject it")
	}
	return v, nil
}

func (x *resolution) entryCredential(keyCmd, apiKey, keyEnv, name string) (auth.Credential, error) {
	if cmd := strings.TrimSpace(keyCmd); cmd != "" {
		if ts, ok := x.o().Auth.CommandTokenSource(cmd, name); ok {
			return tokenKey(ts, ""), nil
		}
	}
	envValue, err := x.keyEnvSecret(keyEnv, "custom provider '"+name+"'")
	if err != nil {
		return auth.Credential{}, err
	}
	return firstUsable([]auth.Credential{staticKey(strings.TrimSpace(apiKey), ""), staticKey(envValue, "")}), nil
}

func (x *resolution) owningEntryCredential(baseURL string) (bool, auth.Credential, error) {
	entry, ok := x.owningEntry(baseURL, "")
	if !ok || !entry.DeclaresCredential {
		return false, noKey(""), nil
	}
	cred, err := x.entryCredential(entry.KeyCmd, entry.APIKey.Reveal(), firstOf(entry.KeyEnv, entry.APIKeyEnv), firstOf(entry.DisplayName, "custom"))
	return true, cred, err
}

func (x *resolution) modelKeyEnvFor(mc *modelCfg, baseURL string) (string, error) {
	cfgBase := trimURL(mc.BaseURL)
	if cfgBase == "" || cfgBase != trimURL(baseURL) {
		return "", nil
	}
	return x.keyEnvSecret(firstOf(mc.KeyEnv, mc.APIKeyEnv), "model")
}

func (x *resolution) customPool(baseURL, label string, modeOverride wire.Mode, providerName string) (*Route, error) {
	store := x.o().Auth
	raw := store.CustomPoolKeyCandidates(x.ctx, baseURL, providerName, x.frozenEntries())
	var owner *auth.CustomEntry
	if e, ok := x.owningEntry(baseURL, providerName); ok {
		owner = &e
	}
	var keys []string
	for _, k := range raw {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" && !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	for _, key := range keys {
		loaded, err := store.Pool(x.ctx, key)
		if err != nil || loaded == nil {
			continue
		}
		pool := store.EntryScopedPool(x.ctx, loaded, key, owner)
		if pool == nil || !pool.HasCredentials() {
			continue
		}
		entry, ok, err := pool.Select(x.ctx, "")
		if err != nil || !ok {
			continue
		}
		text := entry.RuntimeAPIKey().Reveal()
		if text == "" {
			continue
		}
		if !auth.HasUsableSecret(text, 4) && x.loopback(providers.URLHostname(baseURL)) {
			text = noKeyPlaceholder
		}
		rt := x.route(label, firstMode(modeOverride, detectModeForURL(baseURL), wire.ModeChat), baseURL, staticKey(text, ""), "pool:"+key, "")
		rt.Pool = pool
		return rt, nil
	}
	return nil, nil
}

func (x *resolution) applyCustomExtras(nc *namedCustom, target string, rt *Route) {
	model := firstOf(target, nc.Model)
	if model != "" {
		rt.Model = model
	}
	if len(nc.Capabilities) > 0 {
		rt.Capabilities = map[string]bool{}
		for k, v := range nc.Capabilities {
			rt.Capabilities[k] = v
		}
	}
	if len(nc.ExtraHeaders) > 0 {
		rt.Headers = map[string]string{}
		for k, v := range nc.ExtraHeaders {
			rt.Headers[k] = v
		}
	}
	if len(nc.ExtraBody) > 0 {
		if rt.ExtraBody == nil {
			rt.ExtraBody = map[string]json.RawMessage{}
		}
		for k, v := range nc.ExtraBody {
			rt.ExtraBody[k] = v
		}
	}
}

func (x *resolution) customRuntime(baseURL string, key auth.Credential, mode wire.Mode, source, requested string) *Route {
	if !keyPresent(key) {
		key = staticKey(noKeyPlaceholder, "")
	}
	return x.route("custom", firstMode(mode, detectModeForURL(baseURL), wire.ModeChat), baseURL, key, source, requested)
}

func (x *resolution) expandDirectAlias(provider, existingBase string) (string, string, error) {
	if provider == "" {
		return provider, existingBase, nil
	}
	target, ok := directAPIBaseURLs[strings.ToLower(strings.TrimSpace(provider))]
	if !ok {
		return provider, existingBase, nil
	}
	nc, err := x.namedCustomProvider(provider, nil, true)
	if err != nil {
		return "", "", err
	}
	if nc != nil {
		return provider, existingBase, nil
	}
	if base := strings.TrimSpace(existingBase); base != "" {
		return "custom", base, nil
	}
	env, err := x.secretTrim("OPENAI_BASE_URL")
	if err != nil {
		return "", "", err
	}
	return "custom", firstOf(strings.TrimRight(env, "/"), target), nil
}

func (r *Router) ExpandDirectAlias(ctx context.Context, provider, baseURL string) (string, string, error) {
	x, err := r.begin(ctx)
	if err != nil {
		return "", "", err
	}
	return x.expandDirectAlias(provider, baseURL)
}

func (x *resolution) directAliasRuntime(requested string, ek explicitCreds, explicitBase string) (*Route, error) {
	base := trimURL(explicitBase)
	if !usableKey(ek.key) {
		pooled, err := x.customPool(base, "custom", "", "")
		if err != nil {
			return nil, err
		}
		if pooled != nil {
			pooled.Source = "direct-alias"
			pooled.Credential.Source = pooled.Source
			return pooled, nil
		}
	}
	declared, entryKey, err := x.owningEntryCredential(base)
	if err != nil {
		return nil, err
	}
	keyEnv, err := x.modelKeyEnvFor(x.model(), base)
	if err != nil {
		return nil, err
	}
	candidates := []auth.Credential{ek.key, staticKey(keyEnv, "")}
	if !declared {
		gated, err := x.hostGatedCandidates(base, true)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, gated...)
	}
	key := firstUsable(candidates)
	if !keyPresent(key) {
		key = entryKey
	}
	return x.customRuntime(base, key, "", "direct-alias", requested), nil
}

func (x *resolution) llamaCppRuntime(requested string, ek explicitCreds) (*Route, error) {
	if ep := x.o().Local.ResolveLlamaCppEndpoint(x.ctx, local.DefaultBootWait); ep != nil {
		key := ek.key
		if !keyPresent(key) {
			key = staticKey(firstOf(ep.APIKey, noKeyPlaceholder), "")
		}
		return x.route("custom", wire.ModeChat, ep.BaseURL, key, "local-runtime", requested), nil
	}
	if x.cfg.LocalRuntime.Enabled {
		return nil, &LocalRuntimeUnavailableError{Message: "The local model server isn't running. It may still be " +
			"starting — try again in a moment, or switch to another " +
			"model with /model.", Retryable: true}
	}
	return nil, &LocalRuntimeUnavailableError{Message: "The local model server is turned off. Start it from the icebath " +
		"desktop app or `icebath model` → Local models, or switch to " +
		"another model with /model."}
}

func (x *resolution) openCodeFamilyForCustom(requested, baseURL string) (string, bool) {
	if family, ok := x.o().Providers.OpenCodeFamily(x.ctx, requested); ok {
		return family, true
	}
	if strings.ToLower(providers.URLHostname(baseURL)) == "opencode.ai" {
		if strings.Contains(strings.ToLower(baseURL), "/zen/go") {
			return "opencode-go", true
		}
		return "opencode-zen", true
	}
	return "", false
}

func (x *resolution) namedCustomRuntime(requested string, ek explicitCreds, explicitBase, target string) (*Route, error) {
	norm := strings.ToLower(strings.TrimSpace(requested))
	var nc *namedCustom
	var err error
	if slices.Contains(catalog.LlamaCppAliases, norm) && explicitBase == "" {
		if nc, err = x.namedCustomProvider(requested, nil, true); err != nil {
			return nil, err
		}
		if nc == nil {
			return x.llamaCppRuntime(requested, ek)
		}
	}
	if norm != "" && norm != "custom" && x.resolvesToCustom(norm) {
		norm = "custom"
	}
	if norm == "custom" && explicitBase != "" {
		return x.directAliasRuntime(requested, ek, explicitBase)
	}
	if nc == nil {
		if nc, err = x.namedCustomProvider(requested, nil, true); err != nil {
			return nil, err
		}
	}
	if nc == nil {
		return nil, nil
	}
	base := strings.TrimRight(firstOf(strings.TrimSpace(explicitBase), nc.BaseURL), "/")
	if base == "" {
		return nil, nil
	}
	if !usableKey(ek.key) {
		pooled, err := x.customPool(base, "custom", nc.APIMode, firstOf(nc.ProviderKey, nc.Name))
		if err != nil {
			return nil, err
		}
		if pooled != nil {
			x.applyCustomExtras(nc, target, pooled)
			return pooled, nil
		}
	}
	name := firstOf(nc.Name, "custom")
	key := ek.key
	if !usableKey(key) {
		if key, err = x.entryCredential(nc.KeyCmd, nc.APIKey, nc.KeyEnv, name); err != nil {
			return nil, err
		}
		if !keyPresent(key) && !nc.declaresCredential() {
			gated, err := x.hostGatedCandidates(base, false)
			if err != nil {
				return nil, err
			}
			key = firstUsable(gated)
		}
	}
	rt := x.customRuntime(base, key, nc.APIMode, "custom_provider:"+nc.Name, requested)
	x.applyCustomExtras(nc, target, rt)
	if family, ok := x.openCodeFamilyForCustom(requested, base); ok && nc.APIMode == "" {
		reg := x.o().Providers
		if eff := strings.TrimSpace(firstOf(target, nc.Model, x.model().Default)); eff != "" {
			rt.Mode = reg.OpenCodeModelMode(x.ctx, family, eff)
		}
		rt.BaseURL = reg.NormalizeOpenCodeBaseURL(x.ctx, family, rt.Mode, rt.BaseURL)
	}
	return rt, nil
}

func (r *Router) OpenCodeTransport(ctx context.Context, provider, model, baseURL string) (wire.Mode, string, bool, error) {
	x, err := r.begin(ctx)
	if err != nil {
		return "", "", false, err
	}
	family, ok := x.openCodeFamilyForCustom(provider, baseURL)
	if !ok {
		return "", baseURL, false, nil
	}
	nc, err := x.namedCustomProvider(provider, nil, true)
	if err != nil {
		return "", "", false, err
	}
	if nc != nil && nc.APIMode != "" {
		return "", baseURL, false, nil
	}
	reg := x.o().Providers
	mode := reg.OpenCodeModelMode(x.ctx, family, reg.NormalizeOpenCodeModel(x.ctx, provider, model))
	return mode, reg.NormalizeOpenCodeBaseURL(x.ctx, provider, mode, baseURL), true, nil
}

func (r *Router) SessionAffinityHeaders(ctx context.Context, provider, baseURL, sessionID string, existing map[string]string) map[string]string {
	ctx = r.bind(ctx)
	opencode := r.c.o.Providers.OpenCodeSessionHeaders(ctx, provider, baseURL, sessionID)
	custom := providers.CustomAffinityHeaders(ctx, r.EndpointAffinityHeader(ctx, baseURL), sessionID)
	if len(opencode) == 0 && len(custom) == 0 {
		return existing
	}
	return providers.MergeAffinityHeaders(existing, custom, opencode)
}

func headerMap(headers []config.KeyValue) map[string]string {
	out := make(map[string]string, len(headers))
	for _, h := range headers {
		if s, ok := h.Value.(string); ok {
			out[h.Path] = s
		}
	}
	return out
}

func (r *Router) routeEntries(ctx context.Context, baseURL string) []config.CustomEndpoint {
	var frozen []config.CustomEndpoint
	if r.frozen != nil {
		frozen = r.frozen.custom
	}
	return r.c.o.Catalog.RouteEntries(r.bind(ctx), baseURL, frozen)
}

func (r *Router) EndpointMode(ctx context.Context, baseURL string) wire.Mode {
	for _, e := range r.routeEntries(ctx, baseURL) {
		if m := e.APIMode(); m != "" {
			return wire.Mode(m)
		}
	}
	return ""
}

func (r *Router) EndpointTLS(ctx context.Context, baseURL string) (httpclient.TLSOptions, bool) {
	entries := r.routeEntries(ctx, baseURL)
	if len(entries) == 0 {
		return httpclient.TLSOptions{}, false
	}
	ca, verify := entries[0].TLS()
	opts := httpclient.TLSOptions{RootCAFile: ca, Insecure: verify != nil && !*verify}
	return opts, ca != "" || verify != nil
}

func (r *Router) EndpointHeaders(ctx context.Context, baseURL string) map[string]string {
	for _, e := range r.routeEntries(ctx, baseURL) {
		if headers := e.ExtraHeaders(); len(headers) > 0 {
			return headerMap(headers)
		}
	}
	return nil
}

func (r *Router) EndpointAffinityHeader(ctx context.Context, baseURL string) string {
	for _, e := range r.routeEntries(ctx, baseURL) {
		if h := e.SessionAffinityHeader(); h != "" {
			return h
		}
	}
	return ""
}

func (r *Router) EndpointContextLength(ctx context.Context, model, baseURL string) (int, bool) {
	if model == "" || baseURL == "" {
		return 0, false
	}
	entries := r.routeEntries(ctx, baseURL)
	for _, e := range entries {
		if n, ok := e.ModelContextLength(model); ok {
			return n, true
		}
	}
	for _, e := range entries {
		if n, ok := e.ContextLength(); ok {
			return n, true
		}
	}
	return 0, false
}

func (r *Router) EndpointCapability(ctx context.Context, model, baseURL, capability string) (value, set bool) {
	if model == "" || baseURL == "" || capability == "" {
		return false, false
	}
	for _, e := range r.routeEntries(ctx, baseURL) {
		if v, ok := e.ModelCapability(model, capability); ok {
			return v, true
		}
	}
	return false, false
}

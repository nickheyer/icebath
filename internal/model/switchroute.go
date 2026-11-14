package model

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/catalog"
	"github.com/nickheyer/icebath/internal/model/portal"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

var aggregatorProviders = map[string]bool{"portal": true, "openrouter": true, "ai-gateway": true, "kilocode": true}

type SwitchInput struct {
	Raw              string
	CurrentProvider  string
	CurrentModel     string
	CurrentBaseURL   string
	CurrentKey       auth.Credential
	IsGlobal         bool
	ExplicitProvider string
	User             config.Map[config.ProviderEntry]
	Custom           []config.CustomEndpoint
}

type SwitchResult struct {
	NewModel             string
	TargetProvider       string
	ProviderChanged      bool
	Credential           auth.Credential
	BaseURL              string
	Mode                 wire.Mode
	RequestOverrides     map[string]json.RawMessage
	Warning              string
	UsageNotice          string
	ProviderLabel        string
	ResolvedViaAlias     string
	Capabilities         catalog.Capabilities
	HasCapabilities      bool
	ModelInfo            catalog.ModelInfo
	HasModelInfo         bool
	IsGlobal             bool
	AliasEndpointApplied bool
}

type SwitchFailure struct {
	Message        string
	NewModel       string
	TargetProvider string
	ProviderLabel  string
	IsGlobal       bool
}

func (e *SwitchFailure) Error() string { return e.Message }

type switchState struct {
	r              *Router
	x              *resolution
	ctx            context.Context
	in             SwitchInput
	explicit       string
	newModel       string
	target         string
	alias          string
	aliasApplied   bool
	label          string
	cred           auth.Credential
	baseURL        string
	mode           wire.Mode
	headers        map[string]string
	suppressOllama bool
	verdict        catalog.Verdict
	aliases        *directAliases
}

func (st *switchState) fail(message string) *SwitchFailure {
	return &SwitchFailure{Message: message, IsGlobal: st.in.IsGlobal}
}

func (st *switchState) failOnTarget(message string) *SwitchFailure {
	f := st.fail(message)
	f.TargetProvider, f.ProviderLabel = st.target, st.label
	return f
}

func (st *switchState) providerChanged() bool { return st.target != st.in.CurrentProvider }

func (st *switchState) resolveRuntime(requested string, key auth.Secret, baseURL string) error {
	t := Target{Provider: requested, Model: st.newModel, BaseURL: baseURL, APIKey: key}
	rt, err := st.r.Resolve(st.ctx, t)
	if err != nil {
		return err
	}
	st.cred, st.baseURL, st.mode = rt.Credential, rt.BaseURL, rt.Mode
	if len(rt.Headers) > 0 {
		st.headers = rt.Headers
	}
	return nil
}

func (r *Router) ResolveSwitch(ctx context.Context, in SwitchInput) (*SwitchResult, error) {
	x, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	st := &switchState{r: r, x: x, ctx: x.ctx, in: in, explicit: in.ExplicitProvider, newModel: strings.TrimSpace(in.Raw),
		target: in.CurrentProvider, aliases: r.directAliases(x.ctx)}
	if in.ExplicitProvider != "" {
		if f := st.routeExplicit(); f != nil {
			return nil, f
		}
	} else if f := st.routeFromInput(); f != nil {
		return nil, f
	}
	if f := st.resolveCredentials(); f != nil {
		return nil, f
	}
	if f := st.validate(); f != nil {
		return nil, f
	}
	return st.build()
}

func (st *switchState) bareCustomDef() (providers.Definition, bool) {
	base := strings.TrimSpace(st.in.CurrentBaseURL)
	if base == "" {
		return providers.Definition{}, false
	}
	return providers.Definition{ID: "custom", Name: "Custom endpoint", Transport: "openai_chat", BaseURL: base, AuthType: "api_key", Source: catalog.RowSourceModelConfig}, true
}

func (st *switchState) routeExplicit() *SwitchFailure {
	cat := st.r.c.o.Catalog
	def, ok := cat.ResolveProviderFull(st.ctx, st.explicit, st.in.User, st.in.Custom)
	if !ok && strings.ToLower(strings.TrimSpace(st.explicit)) == "custom" {
		def, ok = st.bareCustomDef()
	}
	if !ok {
		return st.fail(st.r.unknownProviderMessage(st.ctx, st.explicit))
	}
	st.target, st.label = def.ID, def.Name
	if st.target == "moa" && st.newModel == "" {
		st.newModel = st.r.moaDefaultPreset(st.ctx)
	}
	if msg := st.r.aggregatorAliasError(st.ctx, st.explicit, st.target, st.in.CurrentProvider, st.in.User, st.in.Custom); msg != "" {
		return st.failOnTarget(msg)
	}
	if st.newModel == "" {
		if def.BaseURL == "" {
			return st.failOnTarget(fmt.Sprintf("Provider '%s' has no base URL configured. Specify a model: /model <model-name> --provider %s", def.Name, st.explicit))
		}
		if st.newModel = st.x.autoDetectLocalModel(def.BaseURL); st.newModel == "" {
			return st.failOnTarget(fmt.Sprintf("No model detected on %s (%s). Specify the model explicitly: /model <model-name> --provider %s", def.Name, def.BaseURL, st.explicit))
		}
	}
	m, found, err := st.r.resolveAlias(st.ctx, st.newModel, st.target, st.in.User, st.in.Custom)
	if f := st.ambiguous(err, true); f != nil {
		return f
	}
	if found {
		st.newModel = m.model
		if st.r.providerIdentity(st.ctx, m.provider, st.in.User, st.in.Custom) == st.r.providerIdentity(st.ctx, st.target, st.in.User, st.in.Custom) {
			st.alias = m.alias
		}
	}
	return nil
}

func (st *switchState) ambiguous(err error, withTarget bool) *SwitchFailure {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if ae, ok := err.(*AmbiguousAliasError); ok {
		msg = ae.Message()
	}
	f := st.fail(msg)
	if withTarget {
		f.TargetProvider = st.target
	}
	return f
}

func (r *Router) unknownProviderMessage(ctx context.Context, explicit string) string {
	if moved := r.c.o.Auth.UnknownProviderHint(ctx, explicit); moved != "" {
		return fmt.Sprintf("Unknown provider '%s'. %s", explicit, moved)
	}
	msg := fmt.Sprintf("Unknown provider '%s'. Check 'icebath model' for available providers, or define it in config.yaml under 'providers:'.", explicit)
	if issues, err := r.c.o.Config.Validate(ctx); err == nil && len(issues) > 0 {
		msg += "\n\nRun 'icebath doctor' — config issues detected:"
		for _, issue := range issues[:min(3, len(issues))] {
			msg += "\n  • " + issue.Message
		}
	}
	return msg
}

func (r *Router) aggregatorAliasError(ctx context.Context, explicit, target, current string, user config.Map[config.ProviderEntry], custom []config.CustomEndpoint) string {
	norm := strings.ToLower(strings.TrimSpace(explicit))
	aliasTarget, ok := providers.Alias(norm)
	if !(ok && aliasTarget == target && target != norm && aggregatorProviders[target]) {
		return ""
	}
	authed := r.AuthenticatedProviderSlugs(ctx, current, user, custom)
	if slices.Contains(authed, target) {
		return ""
	}
	var suggestions []string
	for _, s := range authed {
		if strings.HasPrefix(s, norm) && s != norm {
			suggestions = append(suggestions, s)
		}
	}
	hint := ""
	if len(suggestions) > 0 {
		hint = " Did you mean: " + strings.Join(suggestions, ", ") + "?"
	}
	return fmt.Sprintf("Provider '%s' is an alias that routes through %s, which has no credentials configured.%s", norm, r.c.o.Catalog.Label(ctx, target), hint)
}

func moaEnabled(s config.Scalar) bool {
	switch s.Kind() {
	case config.KindNull:
		return true
	case config.KindBool:
		v, _ := s.AsBool()
		return v
	case config.KindString:
		switch strings.ToLower(strings.TrimSpace(s.String())) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
		return true
	}
	f, _ := s.AsFloat()
	return f != 0
}

type moaPresetView struct {
	names   []string
	enabled map[string]bool
	def     string
}

func moaPresets(cfg *config.Config) moaPresetView {
	v := moaPresetView{enabled: map[string]bool{}}
	for _, name := range cfg.MoA.Presets.Keys() {
		clean := strings.TrimSpace(name)
		if clean == "" {
			continue
		}
		p, _ := cfg.MoA.Presets.Get(name)
		if _, seen := v.enabled[clean]; !seen {
			v.names = append(v.names, clean)
		}
		v.enabled[clean] = moaEnabled(p.Enabled)
	}
	if len(v.names) == 0 {
		v.names = []string{"default"}
		v.enabled["default"] = moaEnabled(cfg.MoA.Flat.Enabled)
	}
	v.def = strings.TrimSpace(cfg.MoA.DefaultPreset)
	if _, ok := v.enabled[v.def]; v.def == "" || !ok {
		v.def = v.names[0]
	}
	return v
}

func (r *Router) moaDefaultPreset(ctx context.Context) string {
	cfg, err := r.c.o.Config.LoadReadonly(ctx)
	if err != nil {
		return "default"
	}
	return moaPresets(cfg).def
}

func (r *Router) exactMoAPreset(ctx context.Context, text string) string {
	wanted := strings.TrimSpace(text)
	if wanted == "" {
		return ""
	}
	cfg, err := r.c.o.Config.LoadReadonly(ctx)
	if err != nil {
		return ""
	}
	enabled, ok := moaPresets(cfg).enabled[wanted]
	if !ok || !enabled {
		return ""
	}
	return wanted
}

func (st *switchState) routeAliasFallback(key string) *SwitchFailure {
	authed := st.r.AuthenticatedProviderSlugs(st.ctx, st.in.CurrentProvider, st.in.User, st.in.Custom)
	m, ok, err := st.r.resolveAliasFallback(st.ctx, st.in.Raw, authed, st.in.User, st.in.Custom)
	if f := st.ambiguous(err, false); f != nil {
		return f
	}
	if !ok {
		identity, _ := catalog.ModelAlias(key)
		return st.fail(fmt.Sprintf("Alias '%s' maps to %s/%s but no matching model was found in any provider catalog. Try specifying the full model name.",
			key, identity.Vendor, identity.Family))
	}
	st.target, st.newModel, st.alias = m.provider, m.model, m.alias
	slog.Debug(fmt.Sprintf("Alias '%s' resolved via fallback to %s on %s", st.alias, st.newModel, st.target))
	return nil
}

func (st *switchState) convertVendorColonSlug() {
	raw := st.in.Raw
	colon := strings.Index(raw, ":")
	cur := strings.ToLower(strings.TrimSpace(st.in.CurrentProvider))
	if colon <= 0 || strings.Contains(raw, "/") || strings.HasPrefix(cur, "custom") || cur == "ollama" {
		return
	}
	left, right := strings.ToLower(strings.TrimSpace(raw[:colon])), strings.TrimSpace(raw[colon+1:])
	if left == "" || right == "" {
		return
	}
	if !st.r.c.o.Catalog.IsAggregator(st.ctx, st.in.CurrentProvider) && !st.namesKnownProvider(left) {
		return
	}
	st.newModel = left + "/" + right
	slog.Debug(fmt.Sprintf("Converted vendor:model '%s' to slug '%s'", raw, st.newModel))
}

func (st *switchState) namesKnownProvider(name string) bool {
	cat := st.r.c.o.Catalog
	if _, ok := cat.ResolveProviderFull(st.ctx, name, st.in.User, st.in.Custom); ok {
		return true
	}
	_, ok := cat.Provider(st.ctx, name, false)
	return ok
}

type configuredMatches struct {
	order []string
	ids   map[string]string
}

func (m *configuredMatches) setDefault(slug, id string) {
	if _, ok := m.ids[slug]; ok {
		return
	}
	m.order = append(m.order, slug)
	m.ids[slug] = id
}

func declaredIDsOfNode(n *yaml.Node) []string {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	var candidates []string
	switch {
	case n == nil:
		return nil
	case n.Kind == yaml.ScalarNode && n.Tag != "!!null":
		candidates = []string{n.Value}
	case n.Kind == yaml.MappingNode:
		for _, kv := range mappingPairs(n) {
			if k := kv[0].Value; k != "__explicit_model_allowlist__" && k != "__discovered_model_catalog__" {
				candidates = append(candidates, k)
			}
		}
	case n.Kind == yaml.SequenceNode:
		for _, item := range n.Content {
			for item != nil && item.Kind == yaml.AliasNode {
				item = item.Alias
			}
			switch {
			case item == nil:
			case item.Kind == yaml.MappingNode:
				id := yamlChild(item, "id")
				if id == nil || id.Kind != yaml.ScalarNode || strings.TrimSpace(id.Value) == "" {
					id = yamlChild(item, "name")
				}
				if id != nil && id.Kind == yaml.ScalarNode && id.Tag == "!!str" {
					candidates = append(candidates, id.Value)
				}
			case item.Kind == yaml.ScalarNode && item.Tag == "!!str":
				candidates = append(candidates, item.Value)
			}
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, c := range candidates {
		id := strings.TrimSpace(c)
		if id != "" && !seen[strings.ToLower(id)] {
			seen[strings.ToLower(id)] = true
			out = append(out, id)
		}
	}
	return out
}

func entryDeclaredIDs(e config.ProviderEntry, key string) []string {
	switch key {
	case "models":
		return catalog.DeclaredModelIDs(e.Models)
	case "model":
		if id := strings.TrimSpace(e.Model); id != "" {
			return []string{id}
		}
	case "default_model":
		if id := strings.TrimSpace(e.DefaultModel); id != "" {
			return []string{id}
		}
	}
	return nil
}

type providerIdentity struct{ name, endpoint, credential, mode string }

func identityOf(slug, name, base, apiKey, keyEnv, keyCmd, mode string) providerIdentity {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		n = strings.ToLower(strings.TrimPrefix(slug, "custom:"))
	}
	apiKey, keyEnv = strings.TrimSpace(apiKey), strings.TrimSpace(keyEnv)
	if strings.HasPrefix(apiKey, "${") && strings.HasSuffix(apiKey, "}") {
		apiKey, keyEnv = "", strings.TrimSpace(apiKey[2:len(apiKey)-1])
	}
	cred := ""
	switch {
	case apiKey != "":
		cred = "key:" + apiKey
	case keyEnv != "":
		cred = "env:" + keyEnv
	case strings.TrimSpace(keyCmd) != "":
		cred = "cmd:" + strings.TrimSpace(keyCmd)
	}
	if mode = strings.TrimSpace(mode); mode != "" {
		mode = strings.ToLower(config.CanonicalAPIMode(mode))
	}
	return providerIdentity{name: strings.ReplaceAll(n, " ", "-"), endpoint: strings.ToLower(strings.TrimRight(strings.TrimSpace(base), "/")), credential: cred, mode: mode}
}

func userIdentity(slug string, e config.ProviderEntry) providerIdentity {
	return identityOf(slug, e.Name, firstOf(e.BaseURL, e.URL, e.API), e.APIKey, firstOf(e.KeyEnv, e.APIKeyEnv), e.KeyCmd, firstOf(e.APIMode, e.Transport))
}

func customIdentity(slug string, c config.CustomEndpoint) providerIdentity {
	return identityOf(slug, c.Text("name"), firstOf(c.Text("base_url"), c.Text("url"), c.Text("api")), c.Text("api_key"),
		firstOf(c.Text("key_env"), c.Text("api_key_env")), c.Text("key_cmd"), firstOf(c.Text("api_mode"), c.Text("transport")))
}

func (r *Router) configuredProviderMatches(model string, user config.Map[config.ProviderEntry], custom []config.CustomEndpoint) *configuredMatches {
	out := &configuredMatches{ids: map[string]string{}}
	target := strings.ToLower(strings.TrimSpace(model))
	if target == "" {
		return out
	}
	rows := map[string]providerIdentity{}
	var rowOrder []string
	for _, slug := range user.Keys() {
		e, _ := user.Get(slug)
		rows[slug] = userIdentity(slug, e)
		rowOrder = append(rowOrder, slug)
	}
	hit := func(lists ...[]string) string {
		for _, ids := range lists {
			for _, id := range ids {
				if strings.ToLower(id) == target {
					return id
				}
			}
		}
		return ""
	}
	for _, slug := range rowOrder {
		e, _ := user.Get(slug)
		if id := hit(entryDeclaredIDs(e, "models"), entryDeclaredIDs(e, "model"), entryDeclaredIDs(e, "default_model")); id != "" {
			out.setDefault(slug, id)
		}
	}
	for _, c := range custom {
		name := strings.TrimSpace(c.Text("name"))
		if name == "" {
			continue
		}
		slug := "custom:" + c.Text("name")
		n := c.Node()
		id := hit(declaredIDsOfNode(yamlChild(n, "models")), declaredIDsOfNode(yamlChild(n, "model")), declaredIDsOfNode(yamlChild(n, "default_model")))
		if id == "" {
			continue
		}
		owner := ""
		if _, isRow := rows[slug]; !isRow {
			ident := customIdentity(slug, c)
			pk := strings.ToLower(strings.TrimSpace(c.Text("provider_key")))
			for _, rowSlug := range rowOrder {
				row := rows[rowSlug]
				if ident == row || (pk == strings.ToLower(rowSlug) && ident.endpoint == row.endpoint && ident.credential == row.credential) {
					owner = rowSlug
					break
				}
			}
		}
		out.setDefault(firstOf(owner, slug), id)
	}
	return out
}

func (st *switchState) currentProviderMatch(m *configuredMatches) string {
	if _, ok := m.ids[st.in.CurrentProvider]; ok {
		return st.in.CurrentProvider
	}
	current := strings.ToLower(strings.TrimSpace(st.in.CurrentProvider))
	for _, slug := range m.order {
		e, ok := st.in.User.Get(slug)
		if ok && slices.Contains(catalog.CustomProviderAliases(e.Name, slug), current) {
			return slug
		}
	}
	return ""
}

func (st *switchState) routeConfigured() (bool, *SwitchFailure) {
	m := st.r.configuredProviderMatches(st.newModel, st.in.User, st.in.Custom)
	if len(m.order) == 0 {
		return false, nil
	}
	if slug := st.currentProviderMatch(m); slug != "" {
		st.newModel = m.ids[slug]
		return true, nil
	}
	slugs := slices.Clone(m.order)
	slices.Sort(slugs)
	if len(slugs) > 1 {
		return false, st.fail(fmt.Sprintf("'%s' is declared by multiple configured providers (%s). Re-run with --provider <slug> to choose which one to use.",
			st.newModel, strings.Join(slugs, ", ")))
	}
	st.target, st.newModel = slugs[0], m.ids[slugs[0]]
	slog.Debug(fmt.Sprintf("Configured-provider detection routed '%s' to %s", st.newModel, st.target))
	if _, ok := st.in.User.Get(st.target); ok {
		st.explicit = st.target
	}
	return true, nil
}

func aggregatorCatalogMatch(model string, cat []string) (string, bool) {
	wanted := strings.ToLower(model)
	for _, id := range cat {
		if strings.ToLower(id) == wanted {
			return id, true
		}
	}
	for _, id := range cat {
		if _, bare, ok := strings.Cut(id, "/"); ok && strings.ToLower(bare) == wanted {
			return id, true
		}
	}
	return "", false
}

func (st *switchState) routeFromInput() *SwitchFailure {
	raw, current := st.in.Raw, st.in.CurrentProvider
	cat := st.r.c.o.Catalog
	if preset := st.r.exactMoAPreset(st.ctx, raw); preset != "" {
		st.target, st.newModel, st.alias = "moa", preset, ""
	} else {
		m, ok, err := st.r.resolveAlias(st.ctx, raw, current, st.in.User, st.in.Custom)
		if f := st.ambiguous(err, false); f != nil {
			return f
		}
		key := strings.ToLower(strings.TrimSpace(raw))
		_, known := catalog.ModelAlias(key)
		switch {
		case ok:
			st.target, st.newModel, st.alias = m.provider, m.model, m.alias
			slog.Debug(fmt.Sprintf("Alias '%s' resolved to %s on %s", st.alias, st.newModel, st.target))
		case known:
			if f := st.routeAliasFallback(key); f != nil {
				return f
			}
		default:
			st.convertVendorColonSlug()
		}
	}
	inCurrent := false
	if cat.IsAggregator(st.ctx, st.target) && st.alias == "" {
		if listed := cat.ListProviderModels(st.ctx, st.target, true); len(listed) > 0 {
			if matched, ok := aggregatorCatalogMatch(st.newModel, listed); ok {
				st.newModel, inCurrent = matched, true
			}
		}
	}
	if st.alias != "" || inCurrent || st.target != current {
		return nil
	}
	if current == portal.Provider && portal.RouteIsWelcomeHost(st.in.CurrentBaseURL) && st.newModel != portal.GuestModel {
		return st.fail(st.newModel + " needs an Icebath Portal account or an API key. Use /login to sign in, or /model to pick another provider.")
	}
	routed, f := st.routeConfigured()
	if f != nil {
		return f
	}
	host := providers.URLHostname(st.in.CurrentBaseURL)
	custom := current == "custom" || current == "local" || strings.HasPrefix(current, "custom:") || host == "localhost" || host == "127.0.0.1"
	if !routed && !custom {
		if p, m, ok := cat.DetectProviderForModel(st.ctx, st.newModel, current); ok {
			st.target, st.newModel = p, m
		}
	}
	return nil
}

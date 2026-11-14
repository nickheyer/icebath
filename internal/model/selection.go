package model

import (
	"context"
	"os"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/catalog"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	SwitchErrOnceWithGlobal     = "once_with_global"
	SwitchErrOnceRequiresTarget = "once_requires_target"
	SwitchErrBadReasoning       = "bad_reasoning"
	SwitchErrResetWithFlags     = "reset_with_flags"
	SwitchResetToken            = "reset"
)

var switchErrorText = map[string]string{
	SwitchErrOnceWithGlobal:     "/model --once cannot be combined with --global",
	SwitchErrOnceRequiresTarget: "/model --once requires a model or provider.",
	SwitchErrBadReasoning:       "/model --reasoning takes none, minimal, low, medium, high, xhigh, max or ultra.",
	SwitchErrResetWithFlags:     "/model reset takes no flags — it clears this session's model override so the configured default applies again.",
}

var unicodeFlagDash = regexp.MustCompile(`[\x{2012}\x{2013}\x{2014}\x{2015}](provider|reasoning|global|session|refresh|once)`)

var opaqueModelPrefixes = []string{"ri.language-model-service..language-model."}

func SwitchErrorText(code string) string { return switchErrorText[code] }

type SwitchRequest struct {
	Raw              string
	Target           string
	ExplicitProvider string
	ReasoningEffort  string
	IsGlobal         bool
	IsSession        bool
	IsOnce           bool
	ForceRefresh     bool
	Scope            string
	Errors           []string
}

func (r SwitchRequest) ErrorMessages() []string {
	out := make([]string, len(r.Errors))
	for i, code := range r.Errors {
		out[i] = switchErrorText[code]
	}
	return out
}

func (r SwitchRequest) IsReset() bool {
	return strings.ToLower(strings.TrimSpace(r.Target)) == SwitchResetToken && len(r.Errors) == 0
}

func ParseReasoningEffort(word string) (*wire.Reasoning, bool) {
	w := strings.ToLower(strings.TrimSpace(word))
	switch w {
	case "none", "false", "disabled":
		return &wire.Reasoning{Enabled: false}, true
	}
	if slices.Contains(config.ReasoningLevels(), w) {
		return &wire.Reasoning{Enabled: true, Effort: w}, true
	}
	return nil, false
}

func ParseSwitchArgs(raw string) SwitchRequest {
	normalized := unicodeFlagDash.ReplaceAllString(raw, "--$1")
	req := SwitchRequest{Raw: raw, Scope: "default"}
	var filtered []string
	tokens := strings.Fields(normalized)
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		switch tok {
		case "--global":
			req.IsGlobal = true
		case "--session":
			req.IsSession = true
		case "--refresh":
			req.ForceRefresh = true
		case "--once":
			req.IsOnce = true
		case "--provider", "--reasoning":
			if i+1 >= len(tokens) {
				filtered = append(filtered, tok)
				continue
			}
			i++
			if tok == "--provider" {
				req.ExplicitProvider = tokens[i]
			} else {
				req.ReasoningEffort = tokens[i]
			}
		default:
			filtered = append(filtered, tok)
		}
	}
	req.Target = strings.TrimSpace(strings.Join(filtered, " "))
	if req.IsOnce && req.IsGlobal {
		req.Errors = append(req.Errors, SwitchErrOnceWithGlobal)
	}
	if req.IsOnce && req.Target == "" && req.ExplicitProvider == "" {
		req.Errors = append(req.Errors, SwitchErrOnceRequiresTarget)
	}
	if req.ReasoningEffort != "" {
		if _, ok := ParseReasoningEffort(req.ReasoningEffort); !ok {
			req.Errors = append(req.Errors, SwitchErrBadReasoning)
		}
	}
	if strings.ToLower(req.Target) == SwitchResetToken &&
		(req.ExplicitProvider != "" || req.ReasoningEffort != "" || req.IsGlobal || req.IsSession || req.IsOnce) {
		req.Errors = append(req.Errors, SwitchErrResetWithFlags)
	}
	switch {
	case req.IsOnce:
		req.Scope = "once"
	case req.IsSession:
		req.Scope = "session"
	case req.IsGlobal:
		req.Scope = "global"
	}
	return req
}

func (r *Router) PersistBehavior(ctx context.Context, isGlobal, isSession, isOnce bool, explicitProvider string) bool {
	if isOnce || isSession {
		return false
	}
	if isGlobal {
		return true
	}
	cfg, err := r.c.o.Config.LoadReadonly(r.bind(ctx))
	if err != nil {
		return false
	}
	m := cfg.Model
	if m.Bare {
		return strings.TrimSpace(m.Default.ID) == ""
	}
	if m.Default.ID == "" && m.Provider == "" {
		return true
	}
	return m.PersistSwitchByDefault
}

func EffectiveModel(tiers ...string) string {
	for _, t := range tiers {
		if c := strings.TrimSpace(t); c != "" {
			return c
		}
	}
	return ""
}

func FormatModelForDisplay(model string) string {
	for _, prefix := range opaqueModelPrefixes {
		if model != "" && strings.HasPrefix(model, prefix) {
			return firstOf(model[len(prefix):], model)
		}
	}
	return model
}

type SelectionUpdate struct {
	Key   string
	Value string
	Clear bool
}

func modelBlockOf(doc *config.YAMLDocument) *yaml.Node {
	if doc == nil {
		return nil
	}
	n, ok := doc.Get("model")
	if !ok {
		return nil
	}
	return n
}

func childText(block *yaml.Node, key string) string {
	n := yamlChild(block, key)
	if n == nil || n.Kind != yaml.ScalarNode || n.Tag == "!!null" {
		return ""
	}
	return n.Value
}

func hasKey(block *yaml.Node, key string) bool {
	for _, kv := range mappingPairs(block) {
		if kv[0].Value == key {
			return true
		}
	}
	return false
}

func mappingOrNil(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	return n
}

func SelectionRouteChanged(block *yaml.Node, res *SwitchResult) bool {
	block = mappingOrNil(block)
	if strings.ToLower(strings.TrimSpace(childText(block, "provider"))) != strings.ToLower(strings.TrimSpace(res.TargetProvider)) {
		return true
	}
	return catalog.NormalizeRouteBaseURL(childText(block, "base_url")) != catalog.NormalizeRouteBaseURL(res.BaseURL)
}

func (r *Router) SelectionUpdates(ctx context.Context, res *SwitchResult, block *yaml.Node) []SelectionUpdate {
	ctx = r.bind(ctx)
	block = mappingOrNil(block)
	optional := func(key, v string) SelectionUpdate { return SelectionUpdate{Key: key, Value: v, Clear: v == ""} }
	updates := []SelectionUpdate{{Key: "default", Value: res.NewModel}, {Key: "provider", Value: res.TargetProvider},
		optional("base_url", res.BaseURL), optional("api_mode", string(res.Mode))}
	if hasKey(block, "context_length") {
		configured := firstOf(childText(block, "default"), childText(block, "model"))
		if r.ShouldClearContextPin(ctx, configured, res.NewModel, childText(block, "base_url"), res.BaseURL, childText(block, "provider"), res.TargetProvider) {
			updates = append(updates, SelectionUpdate{Key: "context_length", Clear: true})
		}
	}
	target := strings.ToLower(strings.TrimSpace(res.TargetProvider))
	changed := SelectionRouteChanged(block, res)
	var stale []string
	if !strings.HasPrefix(target, "custom") || changed {
		stale = append(stale, "api_key", "api")
	}
	if changed {
		stale = append(stale, "key_env", "api_key_env")
	}
	for _, key := range stale {
		if hasKey(block, key) {
			updates = append(updates, SelectionUpdate{Key: key, Clear: true})
		}
	}
	return updates
}

func scalarNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

func nullNode() *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
}

func mergeSelection(block *yaml.Node, updates []SelectionUpdate, maskCleared bool) *yaml.Node {
	out := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if m := mappingOrNil(block); m != nil {
		for _, kv := range mappingPairs(m) {
			out.Content = append(out.Content, kv[0], kv[1])
		}
	}
	for _, u := range updates {
		idx := -1
		for i := 0; i+1 < len(out.Content); i += 2 {
			if out.Content[i].Value == u.Key {
				idx = i
				break
			}
		}
		var value *yaml.Node
		switch {
		case !u.Clear:
			value = scalarNode(u.Value)
		case maskCleared && idx >= 0:
			value = nullNode()
		}
		switch {
		case value == nil && idx >= 0:
			out.Content = append(out.Content[:idx], out.Content[idx+2:]...)
		case value != nil && idx >= 0:
			out.Content[idx+1] = value
		case value != nil:
			out.Content = append(out.Content, scalarNode(u.Key), value)
		}
	}
	return out
}

func (r *Router) ApplyModelSelection(ctx context.Context, block *yaml.Node, res *SwitchResult, maskCleared bool) *yaml.Node {
	return mergeSelection(block, r.SelectionUpdates(ctx, res, block), maskCleared)
}

func (r *Router) RemoteModelSelectionGuard(ctx context.Context, before, after *yaml.Node) error {
	ctx = r.bind(ctx)
	backend, homeDir, err := r.c.o.Config.Backend(ctx)
	if err != nil {
		return err
	}
	if backend.SupportsFileTooling() {
		return nil
	}
	values := func(n *yaml.Node) map[string]string {
		out := map[string]string{}
		for _, kv := range mappingPairs(mappingOrNil(n)) {
			out[kv[0].Value] = string(jsonOfNode(kv[1]))
		}
		return out
	}
	b, a := values(before), values(after)
	var keys []string
	for k := range b {
		keys = append(keys, k)
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		bv, bok := b[k]
		av, aok := a[k]
		if bok == aok && bv == av {
			continue
		}
		if level, locked := backend.Locked(ctx, homeDir, "model."+k); locked {
			return &config.LockedError{Path: "model." + k, LockedBy: level}
		}
	}
	return nil
}

func (r *Router) PersistModelSelection(ctx context.Context, res *SwitchResult) error {
	ctx = r.bind(ctx)
	store := r.c.o.Config
	backend, _, err := store.Backend(ctx)
	if err != nil {
		return err
	}
	doc, err := store.RawDocument(ctx)
	if err != nil {
		return err
	}
	planned := modelBlockOf(doc)
	updates := r.SelectionUpdates(ctx, res, planned)
	if !backend.SupportsFileTooling() {
		masked := mergeSelection(planned, updates, true)
		var c config.Changes
		for _, u := range updates {
			v := yamlChild(masked, u.Key)
			if v == nil {
				c.Unset = append(c.Unset, "model."+u.Key)
				continue
			}
			var value any
			if v.Tag != "!!null" {
				value = v.Value
			}
			c.Set = append(c.Set, config.KeyValue{Path: "model." + u.Key, Value: value})
		}
		return store.WriteChanges(ctx, c)
	}
	lctx, release := store.Lock(ctx)
	defer release()
	doc, err = store.RawDocument(lctx)
	if err != nil {
		return err
	}
	current := modelBlockOf(doc)
	if string(jsonOfNode(current)) != string(jsonOfNode(planned)) {
		updates = r.SelectionUpdates(lctx, res, current)
	}
	if mappingOrNil(current) != nil {
		for _, u := range updates {
			if u.Clear {
				doc.Unset("model." + u.Key)
				continue
			}
			if err := doc.Set("model."+u.Key, u.Value); err != nil {
				return err
			}
		}
	} else if err := doc.Set("model", mergeSelection(current, updates, false)); err != nil {
		return err
	}
	if err := store.ReplaceDocument(lctx, doc); err != nil {
		return err
	}
	if path, _, perr := store.Paths(lctx); perr == nil {
		_ = os.Chmod(path, 0o600)
	}
	return nil
}

type StartupQuery struct {
	Raw              string
	ExplicitProvider string
	CurrentProvider  string
	User             config.Map[config.ProviderEntry]
	Custom           []config.CustomEndpoint
	AliasConfig      *config.YAMLDocument
}

type StartupRoute struct {
	Model    string
	Provider string
	BaseURL  string
	APIKey   auth.Secret
}

func (r *Router) StartupModelRoute(ctx context.Context, q StartupQuery) (StartupRoute, bool) {
	ctx = r.bind(ctx)
	raw := strings.TrimSpace(q.Raw)
	if raw == "" {
		return StartupRoute{}, false
	}
	aliases := r.directAliases(ctx)
	if q.AliasConfig != nil {
		aliases = loadDirectAliases(q.AliasConfig)
	}
	if da, ok := aliases.get(strings.ToLower(raw)); ok {
		if q.ExplicitProvider != "" {
			return StartupRoute{Model: da.Model, Provider: q.ExplicitProvider, BaseURL: da.BaseURL}, true
		}
		provider, key := r.DirectAliasRuntimeRequest(ctx, da)
		return StartupRoute{Model: da.Model, Provider: provider, BaseURL: da.BaseURL, APIKey: key}, true
	}
	if q.ExplicitProvider != "" {
		return StartupRoute{}, false
	}
	cat := r.c.o.Catalog
	customIDs := []string{}
	for _, key := range q.User.Keys() {
		e, _ := q.User.Get(key)
		customIDs = append(customIDs, catalog.CustomProviderSlug(firstOf(e.Name, key), key))
	}
	for _, c := range q.Custom {
		if name := c.Text("name"); strings.TrimSpace(name) != "" {
			customIDs = append(customIDs, catalog.CustomProviderSlug(name, ""))
		}
	}
	if qp, qm := cat.ParseModelInput(ctx, raw, "", customIDs); qp != "" {
		return StartupRoute{Model: qm, Provider: qp}, true
	}
	prefix, model, found := strings.Cut(raw, "/")
	prefix, model = strings.TrimSpace(prefix), strings.TrimSpace(model)
	if !found || prefix == "" || model == "" {
		return StartupRoute{}, false
	}
	if q.CurrentProvider != "" && cat.IsRoutingAggregator(ctx, cat.NormalizeProvider(ctx, q.CurrentProvider)) {
		if _, ok := r.findOpenRouterSlug(ctx, raw); ok {
			return StartupRoute{}, false
		}
	}
	configured := map[string]bool{}
	for _, key := range q.User.Keys() {
		if k := strings.ToLower(strings.TrimSpace(key)); k != "" {
			configured[k] = true
		}
	}
	for _, c := range q.Custom {
		if name := strings.TrimSpace(c.Text("name")); name != "" {
			configured["custom:"+strings.ToLower(name)] = true
		}
	}
	canonical := cat.NormalizeProvider(ctx, prefix)
	var provider string
	switch {
	case configured[strings.ToLower(prefix)]:
		provider = prefix
	case configured[strings.ToLower(canonical)]:
		provider = canonical
	default:
		return StartupRoute{}, false
	}
	if cat.IsAggregator(ctx, canonical) {
		return StartupRoute{}, false
	}
	return StartupRoute{Model: model, Provider: provider}, true
}

func (r *Router) findOpenRouterSlug(ctx context.Context, model string) (string, bool) {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return "", false
	}
	ids := r.c.o.Catalog.OpenRouterModelIDs(ctx, false)
	for _, id := range ids {
		if strings.ToLower(id) == name {
			return id, true
		}
	}
	for _, id := range ids {
		if _, bare, ok := strings.Cut(id, "/"); ok && strings.ToLower(bare) == name {
			return id, true
		}
	}
	return "", false
}

type DisplayContextQuery struct {
	Model              string
	Provider           string
	BaseURL            string
	Key                auth.Credential
	Info               *catalog.ModelInfo
	Custom             []config.CustomEndpoint
	Configured         int
	ConfiguredModel    string
	ConfiguredProvider string
	ConfiguredBaseURL  string
}

func (r *Router) DisplayContextLength(ctx context.Context, q DisplayContextQuery) (int, bool) {
	ctx = r.bind(ctx)
	configured := q.Configured
	if configured > 0 && (q.ConfiguredModel != "" || q.ConfiguredProvider != "" || q.ConfiguredBaseURL != "") &&
		r.ShouldClearContextPin(ctx, q.ConfiguredModel, q.Model, q.ConfiguredBaseURL, q.BaseURL, q.ConfiguredProvider, q.Provider) {
		configured = 0
	}
	if n, _ := r.c.o.Catalog.ContextLength(ctx, catalog.ContextQuery{Model: q.Model, BaseURL: q.BaseURL, Key: q.Key, Configured: configured,
		Provider: q.Provider, Custom: q.Custom}); n > 0 {
		return n, true
	}
	if q.Info != nil && q.Info.ContextWindow > 0 {
		return q.Info.ContextWindow, true
	}
	return 0, false
}

package model

import (
	"context"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/catalog"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/model/wire/anthropic"
)

const (
	stablePrefixMaxEntries = 32
	stablePrefixMaxChars   = 4 * 1024 * 1024
	CacheTTLAuto           = "auto"
)

var (
	alibabaFamilyProviders = []string{"opencode", "opencode-go", "opencode-zen", "alibaba"}
	measured1hProviders    = []string{"opencode-go"}
	no1hTierModels         = []string{"minimax-m2.5"}
	machinePacedSources    = []string{"subagent", "cron", "oneshot", "webhook", "kanban", "api", "tool", "batch"}
	cacheDisableWords      = []string{"off", "false", "disabled", "no", "none"}
)

type stablePrefixes struct {
	mu    sync.Mutex
	order []string
}

func (p *stablePrefixes) register(prefix string) {
	if prefix == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.touch(prefix)
	for len(p.order) > stablePrefixMaxEntries {
		p.order = p.order[1:]
	}
	for len(p.order) > 1 && p.chars() > stablePrefixMaxChars {
		p.order = p.order[1:]
	}
}

func (p *stablePrefixes) chars() int {
	n := 0
	for _, s := range p.order {
		n += len([]rune(s))
	}
	return n
}

func (p *stablePrefixes) touch(prefix string) {
	for i, s := range p.order {
		if s == prefix {
			p.order = append(p.order[:i:i], p.order[i+1:]...)
			break
		}
	}
	p.order = append(p.order, prefix)
}

func (p *stablePrefixes) find(content string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	best, found := "", false
	for _, prefix := range p.order {
		if strings.HasPrefix(content, prefix) && strings.TrimSpace(content[len(prefix):]) != "" && (!found || len(prefix) > len(best)) {
			best, found = prefix, true
		}
	}
	if found {
		p.touch(best)
	}
	return best, found
}

func (r *Router) RegisterStablePrefix(prefix string) { r.c.prefixes.register(prefix) }

func AutoCacheTTL(source string) string {
	for _, s := range machinePacedSources {
		if strings.ToLower(strings.TrimSpace(source)) == s {
			return "5m"
		}
	}
	return "1h"
}

func flatModel(model string) string {
	m := strings.TrimSpace(model)
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	return strings.ToLower(m)
}

func isQwenModel(model string) bool { return strings.Contains(strings.ToLower(model), "qwen") }

func containsFold(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func EffectiveCacheTTL(ttl, model, provider string) string {
	if ttl != "1h" {
		if ttl == "" {
			return "5m"
		}
		return ttl
	}
	lower := strings.ToLower(provider)
	if containsFold(measured1hProviders, lower) {
		if containsFold(no1hTierModels, flatModel(model)) {
			return "5m"
		}
		return "1h"
	}
	if isQwenModel(model) || containsFold(alibabaFamilyProviders, lower) {
		return "5m"
	}
	return "1h"
}

func CacheTTLDisabled(raw *config.Scalar) bool {
	if raw == nil {
		return false
	}
	text := raw.String()
	if text == "5m" || text == "1h" {
		return false
	}
	if raw.Kind() == config.KindNull {
		return true
	}
	if on, ok := raw.AsBool(); ok && raw.Kind() == config.KindBool {
		return !on
	}
	return containsFold(cacheDisableWords, strings.ToLower(text))
}

func configuredCacheTTL(raw *config.Scalar, source string) string {
	if raw == nil {
		return "5m"
	}
	switch text := raw.String(); text {
	case "5m", "1h":
		return text
	case CacheTTLAuto:
		return AutoCacheTTL(source)
	}
	return ""
}

func hasLiteLLMToken(value, delimiters string) bool {
	if value == "" {
		return false
	}
	fields := strings.FieldsFunc(value, func(r rune) bool { return r == ' ' || strings.ContainsRune(delimiters, r) })
	for _, f := range fields {
		if f == "litellm" {
			return true
		}
	}
	return false
}

func isLiteLLMRoute(providerLower, baseURL string) bool {
	return hasLiteLLMToken(providerLower, ":-_/") || hasLiteLLMToken(providers.URLHostname(baseURL), ".-")
}

func EnvelopeToolPartMarkers(provider, baseURL string) bool {
	return !isLiteLLMRoute(strings.ToLower(strings.TrimSpace(provider)), baseURL)
}

type cacheDestination struct {
	provider, baseURL, model string
	mode                     wire.Mode
	stub                     bool
}

func (r *Router) routeMayBeCustom(ctx context.Context, d cacheDestination, providerLower string) bool {
	if !d.stub {
		custom, err := r.customList(ctx)
		if err != nil || len(custom) == 0 {
			return false
		}
		ids := map[string]bool{providerLower: true, strings.TrimPrefix(providerLower, "custom:"): true}
		url := catalog.NormalizeRouteBaseURL(d.baseURL)
		for _, e := range custom {
			for _, alias := range catalog.CustomProviderAliases(e.Text("name"), e.Text("provider_key")) {
				if ids[alias] {
					return true
				}
			}
			if url != "" && catalog.NormalizeRouteBaseURL(e.Text("base_url")) == url {
				return true
			}
		}
		return false
	}
	def, ok := r.c.o.Catalog.Provider(ctx, d.provider, false)
	if !ok {
		return true
	}
	return def.BaseURL != "" && providers.URLHostname(def.BaseURL) != providers.URLHostname(d.baseURL)
}

func (r *Router) customList(ctx context.Context) ([]config.CustomEndpoint, error) {
	if r.frozen != nil {
		return r.frozen.custom, nil
	}
	return r.c.o.Config.CustomEndpoints(ctx)
}

func (r *Router) PromptCachePolicy(ctx context.Context, provider, baseURL string, mode wire.Mode, model string) (cache, native bool) {
	return r.cachePolicy(r.bind(ctx), cacheDestination{provider: provider, baseURL: baseURL, mode: mode, model: model, stub: true})
}

func (r *Router) cachePolicy(ctx context.Context, d cacheDestination) (cache, native bool) {
	if strings.ToLower(strings.TrimSpace(d.provider)) == "moa" {
		aggProvider, aggModel, ok := r.c.o.MoAAggregator(ctx, d.model)
		if !ok || aggProvider == "" || aggModel == "" {
			return false, false
		}
		agg := cacheDestination{provider: aggProvider, model: aggModel, stub: d.stub}
		if rt, err := r.Resolve(ctx, Target{Provider: aggProvider, Model: aggModel}); err == nil {
			agg.baseURL, agg.mode = rt.BaseURL, rt.Mode
		}
		return r.cachePolicy(ctx, agg)
	}
	modelLower := strings.ToLower(d.model)
	providerLower := strings.ToLower(d.provider)
	isClaude := strings.Contains(modelLower, "claude")
	isKimi := anthropic.IsKimiFamilyModel(d.model) || strings.Contains(modelLower, "moonshot")
	isOpenRouter := providers.HostMatches(d.baseURL, "openrouter.ai")
	isPortal := providers.HostMatches(d.baseURL, "icebath.app")
	anthropicWire := d.mode == wire.ModeAnthropic
	nativeAnthropic := anthropicWire && (d.provider == "anthropic" || providers.URLHostname(d.baseURL) == "api.anthropic.com")
	supportsMarkers := d.mode == wire.ModeAnthropic || d.mode == wire.ModeChat
	litellmWire := d.mode == wire.ModeChat && isClaude && isLiteLLMRoute(providerLower, d.baseURL)
	if supportsMarkers && (anthropicWire || litellmWire || r.routeMayBeCustom(ctx, d, providerLower)) {
		if v, set := r.EndpointCapability(ctx, d.model, d.baseURL, "prompt_caching"); set {
			return v, v && anthropicWire
		}
	}
	minimax := providerLower == "minimax" || providerLower == "minimax-cn" ||
		providers.HostMatches(d.baseURL, "api.minimax.io") || providers.HostMatches(d.baseURL, "api.minimaxi.com")
	if anthropicWire && minimax && catalog.ModelSuggestsMiniMaxM3(d.model) {
		return false, false
	}
	switch {
	case nativeAnthropic:
		return true, true
	case (isOpenRouter || isPortal) && (isClaude || isKimi) && !anthropicWire:
		return true, false
	case isPortal && strings.Contains(modelLower, "qwen"):
		return true, false
	case anthropicWire && isClaude:
		return true, true
	case litellmWire:
		return true, false
	case anthropicWire && minimax:
		return true, true
	case containsFold(alibabaFamilyProviders, providerLower) && isQwenModel(modelLower):
		return true, false
	}
	return false, false
}

func directNativeToolCache(mode wire.Mode, baseURL string) bool {
	return mode == wire.ModeAnthropic && providers.URLHostname(baseURL) == "api.anthropic.com"
}

type cachePlanner struct {
	prefixes   *stablePrefixes
	marker     *wire.CacheMarker
	native     bool
	toolMarker bool
}

func textPart(text string, marker *wire.CacheMarker) wire.Part {
	return wire.Part{Kind: wire.PartText, TypeName: "text", Text: text, Cache: cloneMarker(marker)}
}

func cloneMarker(m *wire.CacheMarker) *wire.CacheMarker {
	if m == nil {
		return nil
	}
	c := *m
	return &c
}

func buildMarker(ttl string) *wire.CacheMarker {
	if ttl == "1h" {
		return &wire.CacheMarker{TTL: "1h"}
	}
	return &wire.CacheMarker{}
}

func (p *cachePlanner) scaffoldParts(m *wire.Message, marker *wire.CacheMarker) ([]wire.Part, bool) {
	if m.Role != wire.RoleUser || m.Parts != nil {
		return nil, false
	}
	prefix, ok := p.prefixes.find(m.Text)
	if !ok {
		return nil, false
	}
	return []wire.Part{textPart(prefix, marker), textPart(m.Text[len(prefix):], nil)}, true
}

func emptyContent(m *wire.Message) bool { return m.Parts == nil && m.Text == "" }

func (p *cachePlanner) applyMarker(m *wire.Message, native bool) {
	if m.Role == wire.RoleTool && !native && !p.toolMarker {
		return
	}
	switch {
	case (m.Role == wire.RoleTool && native) || emptyContent(m):
		if !((m.Role == wire.RoleTool || m.Role == wire.RoleAssistant) && !native) {
			m.Cache = cloneMarker(p.marker)
		}
	case m.Parts == nil:
		if parts, ok := p.scaffoldParts(m, p.marker); ok {
			m.Parts = parts
		} else {
			m.Parts = []wire.Part{textPart(m.Text, p.marker)}
		}
		m.Text = ""
	case len(m.Parts) > 0:
		m.Parts[len(m.Parts)-1].Cache = cloneMarker(p.marker)
	}
}

func canCarryMarker(m *wire.Message, native, toolMarker bool) bool {
	if native {
		return true
	}
	if m.Role == wire.RoleTool && !toolMarker {
		return false
	}
	if m.Parts != nil {
		return len(m.Parts) > 0
	}
	return m.Text != ""
}

func (p *cachePlanner) systemMarkers(m *wire.Message, static string, native, markSuffix, fallbackWhole bool) int {
	if static != "" && m.Parts == nil && strings.HasPrefix(m.Text, static) {
		suffix := m.Text[len(static):]
		if strings.TrimSpace(suffix) != "" {
			var suffixMarker *wire.CacheMarker
			if markSuffix {
				suffixMarker = p.marker
			}
			m.Parts, m.Text = []wire.Part{textPart(static, p.marker), textPart(suffix, suffixMarker)}, ""
			if markSuffix {
				return 2
			}
			return 1
		}
	} else if !fallbackWhole {
		return 0
	}
	p.applyMarker(m, native)
	return 1
}

func plainTextPart(part *wire.Part) bool {
	if part.Kind != wire.PartText || (part.TypeName != "" && part.TypeName != "text") {
		return false
	}
	v, err := wire.ParseValue(part.JSON())
	if err != nil || !v.IsObject() || !v.Get("text").IsString() {
		return false
	}
	for _, f := range v.Fields() {
		if f.Key != "type" && f.Key != "text" {
			return false
		}
	}
	return true
}

func (p *cachePlanner) stripDecoration(m *wire.Message) {
	m.Cache = nil
	if m.Parts == nil {
		return
	}
	userPair := m.Role == wire.RoleUser && len(m.Parts) == 2
	firstMarked := userPair && m.Parts[0].Cache != nil && m.Parts[1].Cache == nil
	marked := false
	for i := range m.Parts {
		if m.Parts[i].Cache != nil {
			marked = true
		}
	}
	if marked {
		parts := make([]wire.Part, len(m.Parts))
		copy(parts, m.Parts)
		for i := range parts {
			parts[i].Cache = nil
		}
		m.Parts = parts
	}
	if len(m.Parts) == 0 {
		return
	}
	var joined strings.Builder
	for i := range m.Parts {
		if !plainTextPart(&m.Parts[i]) {
			return
		}
		joined.WriteString(m.Parts[i].Text)
	}
	text := joined.String()
	skill := firstMarked
	if !skill && userPair {
		if prefix, ok := p.prefixes.find(text); ok && prefix == m.Parts[0].Text {
			skill = true
		}
	}
	if len(m.Parts) == 1 || (m.Role == wire.RoleSystem && len(m.Parts) == 2) || skill {
		m.Parts, m.Text = nil, text
	}
}

func (p *cachePlanner) splitUnmarkedScaffolds(msgs []wire.Message) {
	for i := range msgs {
		if parts, ok := p.scaffoldParts(&msgs[i], nil); ok {
			msgs[i].Parts, msgs[i].Text = parts, ""
		}
	}
}

func completedEndpoints(msgs []wire.Message) []int {
	toolRunEnd := func(start int) int {
		end := start
		for end < len(msgs) && msgs[end].Role == wire.RoleTool {
			end++
		}
		return end
	}
	var out []int
	for i := 0; i < len(msgs); {
		m := &msgs[i]
		if m.Role == wire.RoleSystem {
			i++
			continue
		}
		if m.Role == wire.RoleAssistant && len(m.ToolCalls) > 0 {
			end := toolRunEnd(i + 1)
			if end > i+1 {
				out = append(out, end-1)
			}
			i = end
			continue
		}
		if m.Role == wire.RoleTool {
			i = toolRunEnd(i)
			continue
		}
		open := (m.Role == wire.RoleUser && i+1 < len(msgs)) || (m.Role == wire.RoleAssistant && emptyContent(m))
		if !open {
			out = append(out, i)
		}
		i++
	}
	return out
}

func stripToolMarkers(tools []wire.Tool) []wire.Tool {
	out := make([]wire.Tool, len(tools))
	copy(out, tools)
	for i := range out {
		out[i].Cache = nil
	}
	return out
}

func (r *Router) planCache(msgs []wire.Message, tools []wire.Tool, ttl string, native bool, static string, directTools, toolMarker bool) ([]wire.Message, []wire.Tool) {
	p := &cachePlanner{prefixes: r.c.prefixes, marker: buildMarker(ttl), native: native, toolMarker: toolMarker}
	planned := stripToolMarkers(tools)
	out := cloneMessages(msgs)
	if len(out) == 0 {
		return out, planned
	}
	for i := range out {
		p.stripDecoration(&out[i])
	}
	if directTools && len(planned) > 0 {
		if out[0].Role == wire.RoleSystem {
			p.systemMarkers(&out[0], static, true, false, false)
		}
		planned[len(planned)-1].Cache = cloneMarker(p.marker)
		ends := completedEndpoints(out)
		if len(ends) > 2 {
			ends = ends[len(ends)-2:]
		}
		for _, i := range ends {
			p.applyMarker(&out[i], true)
		}
		p.splitUnmarkedScaffolds(out)
		return out, planned
	}
	used := 0
	if out[0].Role == wire.RoleSystem {
		used = p.systemMarkers(&out[0], static, native, true, true)
	}
	var candidates []int
	for i := range out {
		if out[i].Role != wire.RoleSystem && canCarryMarker(&out[i], native, toolMarker) {
			candidates = append(candidates, i)
		}
	}
	if keep := 4 - used; len(candidates) > keep {
		candidates = candidates[len(candidates)-keep:]
	}
	for _, i := range candidates {
		p.applyMarker(&out[i], native)
	}
	p.splitUnmarkedScaffolds(out)
	return out, planned
}

func (r *Router) StripCacheDecoration(msgs []wire.Message, tools []wire.Tool) ([]wire.Message, []wire.Tool) {
	p := &cachePlanner{prefixes: r.c.prefixes}
	out := cloneMessages(msgs)
	for i := range out {
		p.stripDecoration(&out[i])
	}
	return out, stripToolMarkers(tools)
}

func (r *Router) PlanCacheForDestination(ctx context.Context, msgs []wire.Message, tools []wire.Tool, provider, baseURL string, mode wire.Mode, model string, static string) ([]wire.Message, []wire.Tool, error) {
	ctx = r.bind(ctx)
	cfg, err := r.c.o.Config.LoadReadonly(ctx)
	if err != nil {
		return nil, nil, err
	}
	if CacheTTLDisabled(cfg.PromptCaching.CacheTTL) {
		out, planned := r.StripCacheDecoration(msgs, tools)
		return out, planned, nil
	}
	cache, native := r.cachePolicy(ctx, cacheDestination{provider: provider, baseURL: baseURL, mode: mode, model: model, stub: true})
	if !cache {
		out, planned := r.StripCacheDecoration(msgs, tools)
		return out, planned, nil
	}
	ttl := ""
	if raw := cfg.PromptCaching.CacheTTL; raw != nil && (raw.String() == "5m" || raw.String() == "1h") {
		ttl = raw.String()
	}
	out, planned := r.planCache(msgs, tools, EffectiveCacheTTL(ttl, model, provider), native, static,
		directNativeToolCache(mode, baseURL), EnvelopeToolPartMarkers(provider, baseURL))
	return out, planned, nil
}

func IsOneShotRequest(msgs []wire.Message, tools []wire.Tool) bool {
	if len(tools) > 0 {
		return false
	}
	users := 0
	for i := range msgs {
		m := &msgs[i]
		if m.Cache != nil {
			return false
		}
		for _, p := range m.Parts {
			if p.Cache != nil {
				return false
			}
		}
		switch m.Role {
		case wire.RoleUser:
			users++
		case wire.RoleAssistant, wire.RoleTool:
			return false
		}
	}
	return users <= 1
}

package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Release string stamped into user agents and attribution tags, set by the build
var BuildVersion = "dev"

// Attribution headers sent to aggregators that identify client applications
const attributionReferer = "https://github.com/nickheyer/icebath"
const attributionTitle = "Icebath Agent"

// User agent of every OpenAI wire request
func userAgent(product string) string {
	return product + "/" + BuildVersion
}

// JSON object that marshals its keys in insertion order
type omap struct {
	keys []string
	vals map[string]any
}

func newOmap() *omap { return &omap{vals: map[string]any{}} }

func (o *omap) Set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *omap) Get(k string) (any, bool) {
	v, ok := o.vals[k]
	return v, ok
}

func (o *omap) Has(k string) bool {
	_, ok := o.vals[k]
	return ok
}

func (o *omap) Del(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, key := range o.keys {
		if key == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

func (o *omap) Len() int { return len(o.keys) }

// Plain map copy for estimators and tests
func (o *omap) Map() map[string]any {
	out := make(map[string]any, len(o.vals))
	for k, v := range o.vals {
		if inner, ok := v.(*omap); ok {
			out[k] = inner.Map()
			continue
		}
		out[k] = v
	}
	return out
}

func (o *omap) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		buf.Write(kb)
		buf.WriteByte(':')
		vb, err := json.Marshal(o.vals[k])
		if err != nil {
			return nil, err
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// Lowercased hostname of a base URL, empty when absent
func ccHostname(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// Reports whether the base URL host is the domain or one of its subdomains
func hostMatches(baseURL, domain string) bool {
	h := ccHostname(baseURL)
	d := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	return h != "" && d != "" && (h == d || strings.HasSuffix(h, "."+d))
}

var localHosts = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true, "0.0.0.0": true}
var containerLocalSuffixes = []string{".docker.internal", ".containers.internal", ".lima.internal"}

// Reports whether a base URL points at loopback, LAN, container or Tailscale space
func isLocalEndpoint(baseURL string) bool {
	host := ccHostname(baseURL)
	if host == "" {
		return false
	}
	if localHosts[host] || strings.HasSuffix(host, ".local") || (!strings.Contains(host, ".") && !strings.Contains(host, ":")) {
		return true
	}
	for _, suf := range containerLocalSuffixes {
		if strings.HasSuffix(host, suf) {
			return true
		}
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return true
		}
		if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return true
		}
	}
	parts := strings.Split(host, ".")
	if len(parts) != 4 {
		return false
	}
	first, err1 := strconv.Atoi(parts[0])
	second, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return first == 10 || (first == 172 && second >= 16 && second <= 31) || (first == 192 && second == 168) || (first == 100 && second >= 64 && second <= 127)
}

// Resolved facts about one route that every quirk table keys on
type ccRoute struct {
	route      Route
	provider   string
	baseURL    string
	host       string
	modelLower string
	bare       string
	local      bool
}

func newCCRoute(r Route) *ccRoute {
	ml := strings.ToLower(strings.TrimSpace(r.Model))
	bare := bareModel(r.Model)
	return &ccRoute{
		route: r, provider: strings.ToLower(strings.TrimSpace(r.Provider)), baseURL: strings.TrimSpace(r.BaseURL),
		host: ccHostname(r.BaseURL), modelLower: ml, bare: bare, local: r.Local || isLocalEndpoint(r.BaseURL),
	}
}

func (c *ccRoute) hostIs(domain string) bool { return hostMatches(c.baseURL, domain) }

func (c *ccRoute) isOpenRouter() bool { return c.provider == "openrouter" || c.hostIs("openrouter.ai") }
func (c *ccRoute) isNous() bool {
	return c.provider == "nous" || c.provider == "nous-portal" || c.provider == "nousresearch" || c.hostIs("nousresearch.com")
}
func (c *ccRoute) isQwenPortal() bool   { return c.hostIs("portal.qwen.ai") }
func (c *ccRoute) isDirectOpenAI() bool { return c.host == "api.openai.com" }
func (c *ccRoute) isAzureOpenAI() bool  { return c.hostIs("openai.azure.com") }
func (c *ccRoute) isAzureHost() bool {
	return c.isAzureOpenAI() || c.hostIs("services.ai.azure.com") || c.hostIs("cognitiveservices.azure.com")
}
func (c *ccRoute) isKimiHost() bool {
	return c.hostIs("api.kimi.com") || c.hostIs("moonshot.ai") || c.hostIs("moonshot.cn")
}
func (c *ccRoute) isTokenHubHost() bool {
	return c.hostIs("tokenhub.tencentmaas.com") || c.hostIs("tokenhub-intl.tencentcloudmaas.com")
}
func (c *ccRoute) isNativeGemini() bool {
	n := strings.ToLower(strings.TrimRight(c.baseURL, "/"))
	if strings.HasSuffix(n, "/openai") {
		return false
	}
	return strings.Contains(n, "generativelanguage.googleapis.com") || isVertexExpressBaseURL(n)
}
func (c *ccRoute) isGeminiOpenAICompat() bool {
	n := strings.ToLower(strings.TrimRight(c.baseURL, "/"))
	return strings.Contains(n, "generativelanguage.googleapis.com") && strings.HasSuffix(n, "/openai")
}

// Reports whether an aiplatform host speaks the native API with an API key
func isVertexExpressBaseURL(normalized string) bool {
	if !strings.Contains(normalized, "aiplatform.googleapis.com") {
		return false
	}
	return !strings.Contains(normalized, "/endpoints/openapi")
}

// Provider names with a declared profile, the legacy host flag path serves every other name
var knownProviders = map[string]bool{
	"openrouter": true, "nous": true, "nous-portal": true, "nousresearch": true, "deepseek": true, "kimi-coding": true,
	"kimi-coding-cn": true, "qwen-oauth": true, "lmstudio": true, "azure-foundry": true, "ollama-cloud": true,
	"minimax": true, "minimax-cn": true, "minimax-oauth": true, "xai": true, "xai-oauth": true, "zai": true, "custom": true,
	"openai-codex": true, "alibaba": true, "alibaba-cn": true, "alibaba-token-plan": true, "alibaba-token-plan-cn": true,
	"alibaba-coding-plan": true, "alibaba-coding-plan-cn": true, "gemini": true, "tencent-tokenhub": true, "nvidia": true,
	"actual": true, "opencode-zen": true, "opencode-go": true, "meta-ai": true, "fireworks": true, "upstage": true,
	"nebius-token-factory": true, "solstice": true, "commandcode": true, "commandcode-anthropic": true, "deepinfra": true,
	"kilocode": true, "ai-gateway": true, "novita": true, "gmi": true, "stepfun": true, "arcee": true, "xiaomi": true,
	"huggingface": true, "router": true, "openai": true, "openai-api": true, "anthropic": true, "bedrock": true, "vertex": true,
}

// ---- effort vocabulary ----

var effortLadder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}
var openAICompatWireEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}
var codexGPT56Efforts = []string{"none", "low", "medium", "high", "xhigh", "max"}
var codexLegacyEfforts = []string{"none", "low", "medium", "high", "xhigh"}
var codexAstraEfforts = []string{"low", "medium", "high", "xhigh", "max"}
var astraModelIDs = map[string]bool{"gpt-6-astra": true, "gpt-6-astra-900k": true}
var gpt6TierPrefixes = []string{"gpt-6-sol", "gpt-6-luna"}
var noDisableTierPrefixes = []string{"gpt-6.1-sol"}
var daybreakModelIDs = map[string]bool{"gpt-daybreak-blue-latest": true, "gpt-daybreak-blue-latest-900k": true}
var xaiGrok46Efforts = []string{"low", "medium", "high", "xhigh"}
var xaiLegacyEfforts = []string{"low", "medium", "high"}
var actualRelayEfforts = []string{"none", "low", "medium", "high", "max"}
var kimiK3Efforts = []string{"low", "high", "max"}
var kimiK2Efforts = []string{"low", "medium", "high"}
var kimiK3Overrides = map[string]string{"medium": "high", "xhigh": "max"}
var oxAlphaEfforts = []string{"low", "high", "max"}
var oxAlphaOverrides = map[string]string{"xhigh": "max"}
var tokenhubEfforts = []string{"low", "medium", "high"}
var nebiusEfforts = []string{"low", "medium", "high"}
var solarEfforts = []string{"low", "medium", "high"}
var glm52Efforts = []string{"high", "max"}
var glm52Overrides = map[string]string{"xhigh": "max"}
var glm53Efforts = []string{"low", "medium", "high", "max"}
var glm53Overrides = map[string]string{"xhigh": "max"}
var deepseekV4Efforts = []string{"low", "medium", "high", "max"}
var deepseekV4Overrides = map[string]string{"xhigh": "max"}
var ollamaCloudEfforts = []string{"none", "low", "medium", "high", "max"}
var ollamaCloudOverrides = map[string]string{"xhigh": "max"}
var metaAIEfforts = []string{"minimal", "low", "medium", "high", "xhigh"}
var gemini4ThinkingLevels = []string{"low", "medium", "high"}

var kimiK3SlugRe = regexp.MustCompile(`(?:^|[^a-z0-9])k3(?:[^a-z0-9]|$)`)

func ladderIndex(level string) int {
	for i, l := range effortLadder {
		if l == level {
			return i
		}
	}
	return -1
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Nearest weaker supported effort, the request verbatim when supported or outside the ladder
func clampEffort(effort string, supported []string, overrides map[string]string) string {
	requested := strings.ToLower(strings.TrimSpace(effort))
	if requested == "" || len(supported) == 0 {
		return effort
	}
	var norm []string
	for _, s := range supported {
		l := strings.ToLower(strings.TrimSpace(s))
		if ladderIndex(l) >= 0 {
			norm = append(norm, l)
		}
	}
	if len(norm) == 0 || containsStr(norm, requested) {
		return effort
	}
	if ov, ok := overrides[requested]; ok && containsStr(norm, ov) {
		return ov
	}
	ri := ladderIndex(requested)
	if ri < 0 {
		return effort
	}
	var cands []string
	for _, l := range norm {
		if l != "none" {
			cands = append(cands, l)
		}
	}
	if len(cands) == 0 {
		return effort
	}
	best, bestIdx := "", -1
	minLevel, minIdx := "", len(effortLadder)
	for _, l := range cands {
		idx := ladderIndex(l)
		if idx < ri && idx > bestIdx {
			best, bestIdx = l, idx
		}
		if idx < minIdx {
			minLevel, minIdx = l, idx
		}
	}
	if best != "" {
		return best
	}
	return minLevel
}

func isAstraModel(model string) bool {
	return astraModelIDs[bareModel(model)]
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// Effort vocabulary of an OpenAI or Codex Responses model generation
func codexSupportedEfforts(model string) []string {
	bare := bareModel(model)
	if isAstraModel(model) || hasAnyPrefix(bare, noDisableTierPrefixes) {
		return codexAstraEfforts
	}
	if strings.Contains(bare, "gpt-5.6") || hasAnyPrefix(bare, gpt6TierPrefixes) || daybreakModelIDs[bare] {
		return codexGPT56Efforts
	}
	return codexLegacyEfforts
}

func kimiSupportedEfforts(model string) []string {
	if kimiK3SlugRe.MatchString(bareModel(model)) {
		return kimiK3Efforts
	}
	return kimiK2Efforts
}

// Reasoning ask of a request in the shape the quirk tables read
type reasoningCfg struct {
	set      bool
	disabled bool
	effort   string
}

func reasoningFromRequest(r Reasoning) reasoningCfg {
	cfg := reasoningCfg{effort: strings.ToLower(strings.TrimSpace(r.Effort))}
	cfg.set = cfg.effort != "" || r.Enabled != nil
	cfg.disabled = r.Enabled != nil && !*r.Enabled
	return cfg
}

// Wire object of a reasoning ask, enabled and effort keys
func (c reasoningCfg) object() map[string]any {
	m := map[string]any{"enabled": !c.disabled}
	if c.effort != "" {
		m["effort"] = c.effort
	}
	return m
}

// Explicit effort unless absent or disabled
func requestedEffort(c reasoningCfg) string {
	if !c.set || c.disabled {
		return ""
	}
	return c.effort
}

// Effort clamped onto the OpenAI compatible wire vocabulary at entry
func clampReasoningCfg(c reasoningCfg, supported []string) reasoningCfg {
	if c.effort == "" {
		return c
	}
	c.effort = clampEffort(c.effort, supported, nil)
	return c
}

// Moonshot or DeepSeek style thinking toggle and top level effort split
func thinkingToggleExtras(c reasoningCfg, efforts []string, overrides map[string]string, alwaysToggle bool) (map[string]any, map[string]any) {
	if c.set && c.disabled {
		return map[string]any{"thinking": map[string]any{"type": "disabled"}}, map[string]any{}
	}
	effort := requestedEffort(c)
	if effort == "none" {
		effort = ""
	}
	clamped := clampEffort(effort, efforts, overrides)
	if effort != "" && containsStr(efforts, clamped) {
		extra := map[string]any{}
		if alwaysToggle {
			extra["thinking"] = map[string]any{"type": "enabled"}
		}
		return extra, map[string]any{"reasoning_effort": clamped}
	}
	return map[string]any{"thinking": map[string]any{"type": "enabled"}}, map[string]any{}
}

func tokenhubEffort(requested string) string {
	if requested == "" {
		return "high"
	}
	return clampEffort(requested, tokenhubEfforts, nil)
}

// Generic nested reasoning fallback for routes without a declared shape
func genericNestedReasoning(c reasoningCfg) map[string]any {
	if !c.set {
		return map[string]any{}
	}
	if c.disabled {
		return map[string]any{"reasoning": map[string]any{"enabled": false}}
	}
	effort := c.effort
	if effort == "" {
		effort = "medium"
	}
	return map[string]any{"reasoning": map[string]any{"enabled": true, "effort": effort}}
}

var dashscopePreserveModels = []string{"qwen3.6", "qwen3.7", "qwen3.8", "kimi-k2.6", "kimi-k2.7"}

func dashscopePreserveThinkingExtras(model string) map[string]any {
	m := strings.ToLower(strings.TrimSpace(model))
	for _, tok := range dashscopePreserveModels {
		if strings.Contains(m, tok) {
			return map[string]any{"preserve_thinking": true}
		}
	}
	if strings.HasPrefix(m, "glm-5") {
		return map[string]any{"clear_thinking": false}
	}
	return map[string]any{}
}

var lmValidEfforts = map[string]bool{"none": true, "minimal": true, "low": true, "medium": true, "high": true, "xhigh": true}
var lmEffortAliases = map[string]string{"off": "none", "on": "medium"}
var lmEffortClamp = map[string]string{"max": "xhigh", "ultra": "xhigh"}

// LM Studio reasoning_effort for a config and the published allowed options, empty to omit
func resolveLMStudioEffort(c reasoningCfg, allowed []string) (string, bool) {
	effort := "medium"
	if c.set {
		if c.disabled {
			effort = "none"
		} else {
			raw := c.effort
			if a, ok := lmEffortAliases[raw]; ok {
				raw = a
			}
			if a, ok := lmEffortClamp[raw]; ok {
				raw = a
			}
			if lmValidEfforts[raw] {
				effort = raw
			}
		}
	}
	if len(allowed) > 0 {
		ok := false
		for _, opt := range allowed {
			if a, has := lmEffortAliases[opt]; has {
				opt = a
			}
			if opt == effort {
				ok = true
			}
		}
		if !ok {
			return "", false
		}
	}
	return effort, true
}

// ---- Gemini thinking config on the OpenAI compatible path ----

// Major and minor of a Gemini model id, zero for unversioned or non Gemini ids
func geminiVersion(model string) (int, int) {
	return geminiNativeVersion(model)
}

func geminiUsesThinkingLevel(model string) bool {
	bare := strings.ToLower(strings.TrimSpace(model))
	for _, p := range []string{"google/", "gemini/"} {
		bare = strings.TrimPrefix(bare, p)
	}
	major, _ := geminiVersion(bare)
	return (strings.HasPrefix(bare, "gemini-") && strings.HasSuffix(bare, "-latest")) || major >= 3
}

func geminiSamplingDeprecated(model string) bool {
	major, minor := geminiVersion(model)
	return major > 3 || (major == 3 && minor >= 6)
}

var highEfforts = map[string]bool{"high": true, "xhigh": true, "max": true, "ultra": true}

// Gemini thinkingConfig for a reasoning ask, nil for non Gemini models or no ask
func buildGeminiThinkingConfig(model string, c reasoningCfg) map[string]any {
	if !c.set {
		return nil
	}
	norm := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "google/")
	if !strings.HasPrefix(norm, "gemini") {
		return nil
	}
	effort := c.effort
	if effort == "" {
		effort = "medium"
	}
	if strings.HasPrefix(norm, "gemini-4") {
		disabled := c.disabled || effort == "none"
		level := "low"
		if !disabled {
			level = clampEffort(effort, gemini4ThinkingLevels, nil)
			if !containsStr(gemini4ThinkingLevels, level) {
				level = "medium"
			}
		}
		return map[string]any{"includeThoughts": !disabled, "thinkingLevel": level}
	}
	if c.disabled || effort == "none" {
		cfg := map[string]any{"includeThoughts": false}
		if strings.HasPrefix(norm, "gemini-2.5-") {
			cfg["thinkingBudget"] = 0
		} else if geminiUsesThinkingLevel(norm) {
			cfg["thinkingLevel"] = "low"
		}
		return cfg
	}
	cfg := map[string]any{"includeThoughts": true}
	if strings.HasPrefix(norm, "gemini-2.5-") {
		return cfg
	}
	if ladderIndex(effort) < 0 || effort == "none" {
		effort = "medium"
	}
	if geminiUsesThinkingLevel(norm) {
		if strings.Contains(norm, "pro") {
			if highEfforts[effort] {
				cfg["thinkingLevel"] = "high"
			} else {
				cfg["thinkingLevel"] = "low"
			}
		} else {
			switch {
			case effort == "minimal" || effort == "low":
				cfg["thinkingLevel"] = "low"
			case highEfforts[effort]:
				cfg["thinkingLevel"] = "high"
			default:
				cfg["thinkingLevel"] = "medium"
			}
		}
	}
	return cfg
}

// Gemini thinking config keys in the OpenAI compatible snake case spelling
func snakeCaseGeminiThinkingConfig(cfg map[string]any) map[string]any {
	if len(cfg) == 0 {
		return nil
	}
	out := map[string]any{}
	if inc, ok := cfg["includeThoughts"].(bool); ok {
		out["include_thoughts"] = inc
	}
	if lvl, ok := cfg["thinkingLevel"].(string); ok && strings.TrimSpace(lvl) != "" {
		out["thinking_level"] = strings.ToLower(strings.TrimSpace(lvl))
	}
	switch b := cfg["thinkingBudget"].(type) {
	case int:
		out["thinking_budget"] = b
	case float64:
		out["thinking_budget"] = int(b)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Published Gemini output ceiling that thinking enabled requests are raised to
const geminiDefaultMaxOutputTokens = 65535

// Output cap raised to the ceiling when Gemini thinking bills against it
func raiseGeminiThinkingMaxTokens(model string, c reasoningCfg, requested int) int {
	cfg := buildGeminiThinkingConfig(model, c)
	if cfg == nil {
		return requested
	}
	if inc, ok := cfg["includeThoughts"].(bool); ok && !inc {
		if requested <= 0 {
			return geminiDefaultMaxOutputTokens
		}
		return requested
	}
	if requested < geminiDefaultMaxOutputTokens {
		return geminiDefaultMaxOutputTokens
	}
	return requested
}

// ---- reasoning carriers and must echo families ----

const carrierRC, carrierR, carrierRD = "reasoning_content", "reasoning", "reasoning_details"

type carrierRoute struct {
	carriers  map[string]bool
	providers map[string]bool
	hosts     []string
}

func carrierSet(keys ...string) map[string]bool {
	m := map[string]bool{}
	for _, k := range keys {
		m[k] = true
	}
	return m
}

var reasoningCarrierRoutes = []carrierRoute{
	{carrierSet(), carrierSet("mistral", "groq", "cerebras", "sambanova"), []string{"mistral.ai", "groq.com", "cerebras.ai", "sambanova.ai", "hunyuan.cloud.tencent.com"}},
	{carrierSet(carrierRC), carrierSet("fireworks"), []string{"fireworks.ai"}},
	{carrierSet(carrierRC, carrierR, carrierRD), carrierSet("openrouter", "kilocode", "ai-gateway", "nous"), []string{"openrouter.ai", "kilo.ai", "ai-gateway.vercel.sh", "nousresearch.com"}},
	{carrierSet(carrierRC, carrierRD), carrierSet("novita", "tencent-tokenhub", "minimax", "minimax-cn"), []string{"novita.ai", "tokenhub.tencentmaas.com", "tokenhub-intl.tencentcloudmaas.com", "minimax.io", "minimaxi.com"}},
	{carrierSet(carrierRC, carrierR), carrierSet("ollama-cloud", "custom", "lmstudio", "upstage"), []string{"ollama.com", "upstage.ai"}},
}

// Process memory of reasoning keys a provider host model triple rejected
var rejectedReasoningMu sync.Mutex
var rejectedReasoningRoutes = map[string]map[string]bool{}

func reasoningRouteKey(provider, baseURL, model string) string {
	return strings.ToLower(strings.TrimSpace(provider)) + "|" + ccHostname(baseURL) + "|" + strings.TrimSpace(model)
}

// RecordReasoningFieldRejection remembers the reasoning keys a route rejected and returns the new ones
func RecordReasoningFieldRejection(route Route, errBody string, sentMessages []*omap) map[string]bool {
	fields := RejectedReasoningFields(errBody, sentMessages)
	if NeedsReasoningEcho(route.Provider, route.Model, route.BaseURL) {
		delete(fields, carrierRC)
	}
	if len(fields) == 0 {
		return fields
	}
	key := reasoningRouteKey(route.Provider, route.BaseURL, route.Model)
	rejectedReasoningMu.Lock()
	defer rejectedReasoningMu.Unlock()
	known := rejectedReasoningRoutes[key]
	if known == nil {
		known = map[string]bool{}
		rejectedReasoningRoutes[key] = known
	}
	fresh := map[string]bool{}
	for k := range fields {
		if !known[k] {
			known[k] = true
			fresh[k] = true
		}
	}
	return fresh
}

func rejectedReasoningCarriers(route Route) map[string]bool {
	rejectedReasoningMu.Lock()
	defer rejectedReasoningMu.Unlock()
	out := map[string]bool{}
	for k := range rejectedReasoningRoutes[reasoningRouteKey(route.Provider, route.BaseURL, route.Model)] {
		out[k] = true
	}
	return out
}

// ---- think block stripping ----

// Reasoning tag names every surface hides
var ThinkTagNames = []string{"think", "thinking", "reasoning", "thought", "REASONING_SCRATCHPAD", "思考", "反思", "推理", "推敲"}

var thinkTagAlt = func() string {
	parts := make([]string, len(ThinkTagNames))
	for i, n := range ThinkTagNames {
		parts[i] = regexp.QuoteMeta(n)
	}
	return strings.Join(parts, "|")
}()

var closedThinkRe = regexp.MustCompile(`(?is)<(` + thinkTagAlt + `)>.*?</(?:` + thinkTagAlt + `)>[ \t\r]*\n?`)
var unterminatedThinkRe = regexp.MustCompile(`(?is)(?:^|\n)[ \t]*<(?:` + thinkTagAlt + `)>.*$`)
var orphanThinkRe = regexp.MustCompile(`(?i)</?(?:` + thinkTagAlt + `)>[ \t\r]*`)
var toolCallBlockRe = regexp.MustCompile(`(?is)<tool_call>.*?</tool_call>[ \t\r]*\n?`)
var namedFunctionBlockRe = regexp.MustCompile(`(?is)(?:^|\n)[ \t]*<function\s*=\s*[^>]+>.*?</function>[ \t\r]*\n?`)
var strayToolCloserRe = regexp.MustCompile(`(?i)</(?:tool_call|function)>[ \t\r]*`)
var unterminatedToolCallRe = regexp.MustCompile(`(?is)(?:^|\n)[ \t]*<tool_call>.*$`)
var inlineThinkRe = regexp.MustCompile(`(?is)<(?:` + thinkTagAlt + `)>(.*?)</(?:` + thinkTagAlt + `)>`)

// StripThinkBlocks removes reasoning blocks and stray tool call XML from assistant text
func StripThinkBlocks(content string) string {
	if content == "" {
		return ""
	}
	for _, re := range []*regexp.Regexp{closedThinkRe, toolCallBlockRe, namedFunctionBlockRe, unterminatedThinkRe, orphanThinkRe, strayToolCloserRe, unterminatedToolCallRe} {
		content = re.ReplaceAllString(content, "")
	}
	return content
}

// Inline reasoning blocks of a content string joined with blank lines
func inlineReasoning(content string) string {
	var parts []string
	for _, m := range inlineThinkRe.FindAllStringSubmatch(content, -1) {
		if t := strings.TrimSpace(m[1]); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n")
}

// ---- message conversion ----

var mistralToolIDRe = regexp.MustCompile(`^[A-Za-z0-9]{9}$`)

const mistralIDAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

var mistralModelMarkers = []string{"mistral", "magistral", "mixtral", "devstral", "codestral", "ministral", "pixtral", "voxtral"}

func routeRequiresMistralToolIDs(provider, model, baseURL string) bool {
	if strings.ToLower(strings.TrimSpace(provider)) == "mistral" || hostMatches(baseURL, "mistral.ai") {
		return true
	}
	ml := strings.ToLower(model)
	for _, m := range mistralModelMarkers {
		if strings.Contains(ml, m) {
			return true
		}
	}
	return false
}

// Nine character base62 digest of a tool call id unless already valid
func mistralToolID(key string) string {
	if mistralToolIDRe.MatchString(key) {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	n := new(big.Int).SetBytes(sum[:8])
	sixtyTwo := big.NewInt(62)
	out := make([]byte, 9)
	for i := 0; i < 9; i++ {
		var rem big.Int
		n.DivMod(n, sixtyTwo, &rem)
		out[i] = mistralIDAlphabet[rem.Int64()]
	}
	return string(out)
}

// DeveloperRoleModels are model name substrings whose system prompt is sent under the developer role
var DeveloperRoleModels = []string{"gpt-5", "codex"}

// Private reasoning_details record type carrying provider native assistant carriers
const nativeCarrierType = "icebath.native_assistant"

// Reports whether the route consumes Gemini thought signatures
func modelConsumesThoughtSignature(model, baseURL, provider string) bool {
	m := strings.ToLower(model)
	p := strings.ToLower(strings.TrimSpace(provider))
	return strings.Contains(m, "gemini") || strings.Contains(m, "gemma") || p == "gemini" || p == "vertex" ||
		profileReplaysToolCallSignatures(provider) ||
		hostMatches(baseURL, "generativelanguage.googleapis.com") || hostMatches(baseURL, "aiplatform.googleapis.com")
}

// Whether a Gemini route reads the tool call extra_content thought signature, exported for the agent strict sanitizer
func ModelConsumesThoughtSignature(model, baseURL, provider string) bool {
	return modelConsumesThoughtSignature(model, baseURL, provider)
}

// Gemini thought signature carried by an extra_content record
func thoughtSignature(extra any) string {
	m, ok := extra.(map[string]any)
	if !ok {
		return ""
	}
	if g, ok := m["google"].(map[string]any); ok {
		if s, ok := g["thought_signature"].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
		return ""
	}
	if s, ok := m["thought_signature"].(string); ok && strings.TrimSpace(s) != "" {
		return s
	}
	return ""
}

// Content with text parts normalized to the Qwen portal part list shape
func qwenNormalizeContent(content any) any {
	switch c := content.(type) {
	case string:
		return []any{map[string]any{"type": "text", "text": c}}
	case []any:
		out := make([]any, 0, len(c))
		for _, p := range c {
			switch pv := p.(type) {
			case string:
				out = append(out, map[string]any{"type": "text", "text": pv})
			case map[string]any:
				out = append(out, pv)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return content
}

// Union of connector data use requirements over tool messages
func dataUseRequirement(msgs []*state.Message) map[string]bool {
	out := map[string]bool{}
	for _, m := range msgs {
		if m.DisplayMetadata == nil {
			continue
		}
		if list, ok := m.DisplayMetadata["data_use"].([]any); ok {
			for _, v := range list {
				if s, ok := v.(string); ok && s != "" {
					out[s] = true
				}
			}
		}
	}
	return out
}

// Wire copies of the request messages for the chat completions route
func (c *ccRoute) convertMessages(msgs []*state.Message, echoOptIn bool) []*omap {
	carriers := routeReasoningCarriers(c.provider, c.baseURL)
	for k := range rejectedReasoningCarriers(c.route) {
		delete(carriers, k)
	}
	pad := (echoOptIn || NeedsReasoningEcho(c.route.Provider, c.route.Model, c.baseURL)) && carriers[carrierRC]
	keepSig := modelConsumesThoughtSignature(c.route.Model, c.baseURL, c.provider)
	mistral := routeRequiresMistralToolIDs(c.provider, c.route.Model, c.baseURL)
	qwen := c.isQwenPortal() || c.provider == "qwen-oauth"
	developer := false
	for _, p := range DeveloperRoleModels {
		if strings.Contains(c.modelLower, p) {
			developer = true
		}
	}
	out := make([]*omap, 0, len(msgs))
	for i, m := range msgs {
		if m == nil {
			continue
		}
		om := newOmap()
		role := m.Role
		if i == 0 && role == "system" && developer {
			role = "developer"
		}
		om.Set("role", role)
		content := m.Content
		if (m.Role == "user" || m.Role == "assistant") && m.APIContent != "" {
			content = m.APIContent
		}
		if qwen {
			content = qwenNormalizeContent(content)
		}
		om.Set("content", content)
		if m.Role == "tool" {
			tcid := m.ToolCallID
			if mistral {
				if key := state.CoalesceToolCallID(map[string]any{"id": tcid}); key != "" {
					tcid = mistralToolID(key)
				}
			}
			om.Set("tool_call_id", tcid)
			out = append(out, om)
			continue
		}
		if m.Role != "assistant" {
			out = append(out, om)
			continue
		}
		text := ""
		if strings.TrimSpace(m.ReasoningContent) != "" {
			text = m.ReasoningContent
		} else if strings.TrimSpace(m.Reasoning) != "" {
			text = m.Reasoning
		}
		if carriers[carrierRC] && (text != "" || pad) {
			if text == "" {
				text = " "
			}
			om.Set(carrierRC, text)
		}
		if carriers[carrierR] && text != "" && strings.TrimSpace(m.ReasoningContent) == "" && strings.TrimSpace(m.Reasoning) != "" {
			om.Set(carrierR, m.Reasoning)
		} else if carriers[carrierR] && text != "" {
			om.Set(carrierR, text)
		}
		details, record := splitNativeCarrier(m.ReasoningDetails)
		if carriers[carrierRD] && len(details) > 0 {
			om.Set(carrierRD, details)
		}
		if keepSig && record != nil {
			if sig := thoughtSignature(record["extra_content"]); sig != "" {
				om.Set("extra_content", map[string]any{"google": map[string]any{"thought_signature": sig}})
			}
		}
		toolCarriers := nativeToolCallCarriers(record)
		if len(m.ToolCalls) > 0 {
			tcs := make([]any, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				id := tc.ID
				if mistral {
					if key := state.CoalesceToolCallID(map[string]any{"id": id}); key != "" {
						id = mistralToolID(key)
					}
				}
				typ := tc.Type
				if typ == "" {
					typ = "function"
				}
				t := newOmap()
				t.Set("id", id)
				t.Set("type", typ)
				fn := newOmap()
				fn.Set("name", tc.Function.Name)
				fn.Set("arguments", tc.Function.Arguments)
				if carrier := toolCarriers[tc.ID]; keepSig && carrier != nil {
					if sig, ok := carrier["thought_signature"].(string); ok && sig != "" {
						fn.Set("thought_signature", sig)
					}
					if extra := carrier["extra_content"]; extra != nil && thoughtSignature(extra) != "" {
						t.Set("extra_content", extra)
					}
				}
				t.Set("function", fn)
				tcs = append(tcs, t)
			}
			om.Set("tool_calls", tcs)
		}
		out = append(out, om)
	}
	if qwen {
		for _, om := range out {
			if r, _ := om.Get("role"); r == "system" {
				if parts, ok := om.vals["content"].([]any); ok && len(parts) > 0 {
					if last, ok := parts[len(parts)-1].(map[string]any); ok {
						cp := make(map[string]any, len(last)+1)
						for k, v := range last {
							cp[k] = v
						}
						cp["cache_control"] = map[string]any{"type": "ephemeral"}
						parts[len(parts)-1] = cp
					}
				}
				break
			}
		}
	}
	return out
}

// Tool call carriers of the native record keyed by call id
func nativeToolCallCarriers(record map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	if record == nil {
		return out
	}
	list, _ := record["tool_calls"].([]any)
	for _, raw := range list {
		if m, ok := raw.(map[string]any); ok {
			if id := stringOf(m["id"]); id != "" {
				out[id] = m
			}
		}
	}
	return out
}

// Reasoning details split into wire records and the private native carrier record
func splitNativeCarrier(raw json.RawMessage) ([]any, map[string]any) {
	if len(raw) == 0 {
		return nil, nil
	}
	var list []any
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, nil
	}
	var kept []any
	var record map[string]any
	for _, d := range list {
		if dm, ok := d.(map[string]any); ok {
			if t, _ := dm["type"].(string); strings.HasSuffix(t, ".native_assistant") {
				if t == nativeCarrierType || t == "hermes.native_assistant" {
					record = dm
				}
				continue
			}
		}
		kept = append(kept, d)
	}
	return kept, record
}

// ---- tools ----

// Alias xAI reserves for the client tool search bridge
const xaiToolSearchAlias = "icebath_tool_search"

// Reserved names aliased on the wire, with the alias to original map
func aliasReservedTools(tools []map[string]any, reserved []string, nameOf func(map[string]any) string, rename func(map[string]any, string) map[string]any) ([]map[string]any, map[string]string) {
	taken := map[string]bool{}
	for _, t := range tools {
		if n := nameOf(t); n != "" {
			taken[n] = true
		}
	}
	out := make([]map[string]any, 0, len(tools))
	aliases := map[string]string{}
	for _, t := range tools {
		name := nameOf(t)
		if !containsStr(reserved, name) {
			out = append(out, t)
			continue
		}
		base := "icebath_" + name
		alias := base
		for n := 2; taken[alias]; n++ {
			alias = fmt.Sprintf("%s_%d", base, n)
		}
		taken[alias] = true
		aliases[alias] = name
		out = append(out, rename(t, alias))
	}
	return out, aliases
}

func chatToolName(t map[string]any) string {
	fn, _ := t["function"].(map[string]any)
	return stringOf(fn["name"])
}

func chatToolRename(t map[string]any, alias string) map[string]any {
	fn, _ := t["function"].(map[string]any)
	nf := make(map[string]any, len(fn))
	for k, v := range fn {
		nf[k] = v
	}
	nf["name"] = alias
	nt := make(map[string]any, len(t))
	for k, v := range t {
		nt[k] = v
	}
	nt["function"] = nf
	return nt
}

// Tool schemas shaped for the route, with the alias map the response normalizer reverses
func (c *ccRoute) convertTools(tools []map[string]any) ([]map[string]any, map[string]string) {
	if len(tools) == 0 {
		return nil, map[string]string{}
	}
	aliases := map[string]string{}
	if c.provider == "xai" || c.provider == "xai-oauth" || c.host == "api.x.ai" {
		tools, aliases = aliasReservedTools(tools, []string{"tool_search"}, chatToolName, chatToolRename)
	}
	if isMoonshotModel(c.route.Model) {
		tools = sanitizeMoonshotTools(tools)
	}
	return tools, aliases
}

// ---- Moonshot schema ----

var moonshotSchemaMapKeys = map[string]bool{"properties": true, "patternProperties": true, "$defs": true, "definitions": true}
var moonshotSchemaListKeys = map[string]bool{"anyOf": true, "oneOf": true, "allOf": true, "prefixItems": true}
var moonshotSchemaNodeKeys = map[string]bool{"items": true, "contains": true, "not": true, "additionalProperties": true, "propertyNames": true, "if": true, "then": true, "else": true}
var moonshotScalarTypes = map[string]bool{"string": true, "integer": true, "number": true, "boolean": true}

func isMoonshotModel(model string) bool {
	if model == "" {
		return false
	}
	bare := strings.ToLower(strings.TrimSpace(model))
	tail := bare
	if i := strings.LastIndex(bare, "/"); i >= 0 {
		tail = bare[i+1:]
	}
	if strings.HasPrefix(tail, "kimi-") || tail == "kimi" || tail == "k3" || strings.HasPrefix(tail, "k3.") || strings.HasPrefix(tail, "k3-") {
		return true
	}
	return strings.Contains(bare, "moonshot") || strings.Contains(bare, "/kimi") || strings.HasPrefix(bare, "kimi")
}

func moonshotEmptyObject() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{}}
}

// Schema node rewritten into the Moonshot flavored JSON schema subset
func moonshotRepairSchema(node any) any {
	switch n := node.(type) {
	case []any:
		out := make([]any, len(n))
		for i, it := range n {
			out[i] = moonshotRepairSchema(it)
		}
		return out
	case map[string]any:
		repaired := make(map[string]any, len(n))
		for k, v := range n {
			switch {
			case moonshotSchemaMapKeys[k]:
				if vm, ok := v.(map[string]any); ok {
					sub := make(map[string]any, len(vm))
					for sk, sv := range vm {
						sub[sk] = moonshotRepairSchema(sv)
					}
					repaired[k] = sub
					continue
				}
			case moonshotSchemaListKeys[k]:
				if _, ok := v.([]any); ok {
					repaired[k] = moonshotRepairSchema(v)
					continue
				}
			case moonshotSchemaNodeKeys[k]:
				if _, ok := v.(map[string]any); ok {
					repaired[k] = moonshotRepairSchema(v)
					continue
				}
			}
			repaired[k] = v
		}
		if anyOf, ok := repaired["anyOf"].([]any); ok {
			delete(repaired, "type")
			var nonNull []any
			for _, b := range anyOf {
				if bm, ok := b.(map[string]any); ok && stringOf(bm["type"]) == "null" {
					continue
				}
				nonNull = append(nonNull, b)
			}
			if len(nonNull) == 0 || len(nonNull) == len(anyOf) {
				return repaired
			}
			if len(nonNull) > 1 {
				repaired["anyOf"] = nonNull
				return repaired
			}
			merged := map[string]any{}
			for k, v := range repaired {
				if k != "anyOf" {
					merged[k] = v
				}
			}
			if only, ok := nonNull[0].(map[string]any); ok {
				for k, v := range only {
					merged[k] = v
				}
			}
			repaired = merged
		}
		delete(repaired, "nullable")
		if _, hasRef := repaired["$ref"]; !hasRef {
			repaired = moonshotFillMissingType(repaired)
		}
		if enum, ok := repaired["enum"].([]any); ok && moonshotScalarTypes[stringOf(repaired["type"])] {
			var cleaned []any
			for _, v := range enum {
				if v == nil || v == "" {
					continue
				}
				cleaned = append(cleaned, v)
			}
			if len(cleaned) > 0 {
				repaired["enum"] = cleaned
			} else {
				delete(repaired, "enum")
			}
		}
		if stringOf(repaired["type"]) == "object" {
			repaired = moonshotEnsureRequired(repaired)
		}
		return repaired
	}
	return node
}

func moonshotEnsureRequired(node map[string]any) map[string]any {
	props, hasProps := node["properties"].(map[string]any)
	if req, ok := node["required"].([]any); ok {
		if hasProps {
			kept := make([]any, 0, len(req))
			for _, r := range req {
				if rs, ok := r.(string); ok {
					if _, in := props[rs]; in {
						kept = append(kept, rs)
					}
				}
			}
			node["required"] = kept
		}
		return node
	}
	node["required"] = []any{}
	return node
}

func moonshotFillMissingType(node map[string]any) map[string]any {
	if list, ok := node["type"].([]any); ok {
		concrete := "string"
		for _, t := range list {
			if ts, ok := t.(string); ok && ts != "" && ts != "null" {
				concrete = ts
				break
			}
		}
		node["type"] = concrete
		return node
	}
	if t, ok := node["type"].(string); ok && t != "" {
		return node
	}
	_, hasProps := node["properties"]
	_, hasReq := node["required"]
	_, hasAdd := node["additionalProperties"]
	_, hasItems := node["items"]
	_, hasPrefix := node["prefixItems"]
	switch {
	case hasProps || hasReq || hasAdd:
		node["type"] = "object"
	case hasItems || hasPrefix:
		node["type"] = "array"
	default:
		if enum, ok := node["enum"].([]any); ok && len(enum) > 0 {
			switch enum[0].(type) {
			case bool:
				node["type"] = "boolean"
			case float64:
				if f := enum[0].(float64); f == float64(int64(f)) {
					node["type"] = "integer"
				} else {
					node["type"] = "number"
				}
			default:
				node["type"] = "string"
			}
			return node
		}
		for _, k := range []string{"if", "then", "else"} {
			if _, ok := node[k]; ok {
				return node
			}
		}
		node["type"] = "string"
	}
	return node
}

func sanitizeMoonshotToolParameters(params any) map[string]any {
	pm, ok := params.(map[string]any)
	if !ok {
		return moonshotEmptyObject()
	}
	repaired, ok := moonshotRepairSchema(deepCopyJSON(pm)).(map[string]any)
	if !ok {
		return moonshotEmptyObject()
	}
	repaired["type"] = "object"
	if _, ok := repaired["properties"]; !ok {
		repaired["properties"] = map[string]any{}
	}
	return moonshotEnsureRequired(repaired)
}

func sanitizeMoonshotTools(tools []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			out = append(out, tool)
			continue
		}
		nf := make(map[string]any, len(fn))
		for k, v := range fn {
			nf[k] = v
		}
		nf["parameters"] = sanitizeMoonshotToolParameters(fn["parameters"])
		nt := make(map[string]any, len(tool))
		for k, v := range tool {
			nt[k] = v
		}
		nt["function"] = nf
		out = append(out, nt)
	}
	return out
}

// Structural copy of JSON shaped data
func deepCopyJSON(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return v
	}
	return out
}

// ---- temperature and output caps ----

// Temperature policy, omit when true, else the fixed value when non nil
func (c *ccRoute) fixedTemperature() (omit bool, fixed *float64) {
	if c.provider == "kimi-coding" || c.provider == "kimi-coding-cn" {
		return true, nil
	}
	omit, fixed, err := auxFixedTemperatureForModel(c.route.Model, c.route.BaseURL, "")
	if err != nil {
		return false, nil
	}
	return omit, fixed
}

func modelForcesMaxCompletionTokens(model string) bool {
	return hasAnyPrefix(bareModel(model), []string{"gpt-4o", "gpt-4.1", "gpt-5", "o1", "o3", "o4"})
}

// Model whose chat completions requests must send max_completion_tokens instead of max_tokens
func ModelForcesMaxCompletionTokens(model string) bool {
	return modelForcesMaxCompletionTokens(model)
}

// Name of the output cap parameter the route accepts
func (c *ccRoute) maxTokensParam() string {
	if c.isDirectOpenAI() || c.isAzureOpenAI() || modelForcesMaxCompletionTokens(c.route.Model) {
		return "max_completion_tokens"
	}
	return "max_tokens"
}

var profileDefaultMaxTokens = map[string]int{"kimi-coding": 32000, "kimi-coding-cn": 32000, "qwen-oauth": 65536, "nvidia": 16384, "meta-ai": 16384}

// Output cap of the route profile when the request sets none, zero to send nothing
func (c *ccRoute) profileMaxTokens() int {
	if c.provider == "opencode-go" && c.bare == "mimo-v2.5-pro" {
		return 131072
	}
	return profileDefaultMaxTokens[c.provider]
}

// ---- prompt cache key ----

var cronSessionIDRe = regexp.MustCompile(`^(cron_.+)_\d{8}_\d{6}$`)

// Stable logical cache scope of a physical session id
func cacheScopeFromSessionID(sid string) string {
	if m := cronSessionIDRe.FindStringSubmatch(sid); m != nil {
		return m[1]
	}
	return sid
}

// Provider safe cache key of at most 64 characters
func boundedPromptCacheKey(v any) string {
	key := strings.TrimSpace(firstString(v))
	if key == "" {
		return ""
	}
	if len(key) <= 64 {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return "pck_" + hex.EncodeToString(sum[:])[:24]
}

// Content addressed cache key over the scope, instructions and name sorted tools
func contentCacheKey(instructions string, tools []map[string]any, scope string) string {
	if instructions == "" && len(tools) == 0 {
		return ""
	}
	toolsPart := ""
	if len(tools) > 0 {
		sorted := make([]map[string]any, len(tools))
		copy(sorted, tools)
		sort.SliceStable(sorted, func(i, j int) bool {
			return toolSortName(sorted[i]) < toolSortName(sorted[j])
		})
		b, _ := json.Marshal(sorted)
		toolsPart = string(b)
	}
	sum := sha256.Sum256([]byte(scope + "\x00" + instructions + "\x00" + toolsPart))
	return "pck_" + hex.EncodeToString(sum[:])[:24]
}

func toolSortName(t map[string]any) string {
	if n := stringOf(t["name"]); n != "" {
		return n
	}
	return stringOf(t["type"])
}

// Static leading system prefix of the messages used for cache routing
func staticPromptInstructions(msgs []*omap) string {
	if len(msgs) == 0 {
		return ""
	}
	role, _ := msgs[0].Get("role")
	if role != "system" && role != "developer" {
		return ""
	}
	content, _ := msgs[0].Get("content")
	if s, ok := content.(string); ok {
		return s
	}
	b, err := json.Marshal(content)
	if err != nil {
		return fmt.Sprint(content)
	}
	return string(b)
}

// ---- structured output memo ----

// Reports whether the shared rejection memo or the provider profile rules out the response format type
func (c *ccRoute) responseFormatUnsupported(ctx context.Context, model string, formatType any) (bool, error) {
	known, _, err := auxResponseFormatKnownUnsupported(ctx, c.provider, c.baseURL, model, formatType)
	return known, err
}

// ---- OpenRouter and Nous reasoning capability catalogs ----

// Per model reasoning capability a catalog advertises
type reasoningCaps struct {
	known            bool
	supports         bool
	supportedEfforts []string
	mandatory        bool
}

type capsCatalog struct {
	mu          sync.Mutex
	url         string
	cache       map[string]reasoningCaps
	failedAt    time.Time
	diskChecked bool
	warmStarted bool
}

const reasoningCapsDiskTTL = 24 * time.Hour

var capsCatalogs = map[string]*capsCatalog{}
var capsCatalogsMu sync.Mutex

func catalogFor(url string) *capsCatalog {
	capsCatalogsMu.Lock()
	defer capsCatalogsMu.Unlock()
	c := capsCatalogs[url]
	if c == nil {
		c = &capsCatalog{url: url}
		capsCatalogs[url] = c
	}
	return c
}

func reasoningCapsDiskPath() string {
	return filepath.Join(home.CacheDir(home.Process()), "reasoning_caps.json")
}

// One catalog entry parsed from the OpenRouter models schema
func parseReasoningCaps(item map[string]any) (reasoningCaps, bool) {
	params, ok := item["supported_parameters"].([]any)
	if !ok {
		return reasoningCaps{}, false
	}
	has := false
	for _, p := range params {
		if p == "reasoning" {
			has = true
		}
	}
	if !has {
		return reasoningCaps{known: true}, true
	}
	caps := reasoningCaps{known: true, supports: true}
	if r, ok := item["reasoning"].(map[string]any); ok {
		if list, ok := r["supported_efforts"].([]any); ok {
			seen := map[string]bool{}
			for _, e := range list {
				s := strings.ToLower(strings.TrimSpace(firstString(e)))
				if s != "" && !seen[s] {
					seen[s] = true
					caps.supportedEfforts = append(caps.supportedEfforts, s)
				}
			}
		}
		caps.mandatory = r["mandatory"] == true
	}
	return caps, true
}

func (c *capsCatalog) parseItems(items []any) map[string]reasoningCaps {
	out := map[string]reasoningCaps{}
	for _, it := range items {
		im, ok := it.(map[string]any)
		if !ok {
			continue
		}
		id := strings.TrimSpace(stringOf(im["id"]))
		if id == "" {
			continue
		}
		if caps, known := parseReasoningCaps(im); known {
			out[id] = caps
		} else {
			out[id] = reasoningCaps{}
		}
	}
	return out
}

type capsDiskEntry struct {
	TS   float64                `json:"ts"`
	Caps map[string]capsDiskRow `json:"caps"`
}

type capsDiskRow struct {
	Known     bool     `json:"known"`
	Supports  bool     `json:"supports_reasoning"`
	Efforts   []string `json:"supported_efforts,omitempty"`
	Mandatory bool     `json:"mandatory"`
}

func (c *capsCatalog) saveDisk(caps map[string]reasoningCaps) {
	path := reasoningCapsDiskPath()
	data := pyjson.NewObject()
	if raw, err := os.ReadFile(path); err == nil {
		if v, err := pyjson.Loads(string(raw)); err == nil {
			if obj, ok := v.(*pyjson.Object); ok {
				data = obj
			}
		}
	}
	ids := make([]string, 0, len(caps))
	for id := range caps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rows := pyjson.NewObject()
	for _, id := range ids {
		cp := caps[id]
		row := pyjson.NewObject()
		row.Set("known", cp.known)
		row.Set("supports_reasoning", cp.supports)
		if len(cp.supportedEfforts) > 0 {
			efforts := make([]any, len(cp.supportedEfforts))
			for i, e := range cp.supportedEfforts {
				efforts[i] = e
			}
			row.Set("supported_efforts", efforts)
		}
		row.Set("mandatory", cp.mandatory)
		rows.Set(id, row)
	}
	entry := pyjson.NewObject()
	entry.Set("ts", float64(time.Now().UnixNano())/1e9)
	entry.Set("caps", rows)
	data.Set(c.url, entry)
	if err := modelsWriteJSONCache(path, data, pyjson.Indent(0), pyjson.Separators(",", ":")); err != nil {
		catalogLog.Debug(fmt.Sprintf("Failed to save reasoning-caps disk cache: %v", err))
	}
}

func (c *capsCatalog) loadDisk() (map[string]reasoningCaps, time.Duration) {
	raw, err := os.ReadFile(reasoningCapsDiskPath())
	if err != nil {
		return nil, 0
	}
	data := map[string]capsDiskEntry{}
	if json.Unmarshal(raw, &data) != nil {
		return nil, 0
	}
	entry, ok := data[c.url]
	if !ok || len(entry.Caps) == 0 {
		return nil, 0
	}
	out := make(map[string]reasoningCaps, len(entry.Caps))
	for id, row := range entry.Caps {
		out[id] = reasoningCaps{known: row.Known, supports: row.Supports, supportedEfforts: row.Efforts, mandatory: row.Mandatory}
	}
	age := time.Since(time.Unix(int64(entry.TS), 0))
	if age < 0 {
		age = 0
	}
	return out, age
}

// Fetches the catalog and remembers the result or the failure
func (c *capsCatalog) fetch() {
	c.mu.Lock()
	if !c.failedAt.IsZero() && time.Since(c.failedAt) < time.Minute {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	req, err := http.NewRequest(http.MethodGet, c.url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent("icebath-cli"))
	client := &http.Client{Timeout: 6 * time.Second}
	resp, err := client.Do(req)
	var caps map[string]reasoningCaps
	if err == nil {
		defer resp.Body.Close()
		var payload struct {
			Data []any `json:"data"`
		}
		if json.NewDecoder(resp.Body).Decode(&payload) == nil && len(payload.Data) > 0 {
			caps = c.parseItems(payload.Data)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(caps) == 0 {
		c.failedAt = time.Now()
		return
	}
	c.cache = caps
	c.saveDisk(caps)
}

// Cache only lookup, hydrating from disk once and warming in the background when cold
func (c *capsCatalog) lookup(model string) (reasoningCaps, bool) {
	c.mu.Lock()
	if c.cache == nil && !c.diskChecked {
		c.diskChecked = true
		if caps, age := c.loadDisk(); caps != nil {
			c.cache = caps
			if age >= reasoningCapsDiskTTL {
				go c.fetch()
			}
		}
	}
	cache := c.cache
	if cache == nil && !c.warmStarted {
		c.warmStarted = true
		go c.fetch()
	}
	c.mu.Unlock()
	if cache == nil {
		return reasoningCaps{}, false
	}
	caps, ok := cache[strings.TrimSpace(model)]
	return caps, ok && caps.known
}

const openRouterCatalogURL = "https://openrouter.ai/api/v1/models"

func openRouterCaps(model string) (reasoningCaps, bool) {
	return catalogFor(openRouterCatalogURL).lookup(model)
}

func nousCaps(baseURL, model string) (reasoningCaps, bool) {
	base := strings.TrimRight(baseURL, "/")
	if base == "" {
		base = "https://inference-api.nousresearch.com/v1"
	}
	return catalogFor(base + "/models").lookup(model)
}

// ---- local server probes ----

type probeEntry struct {
	value      any
	definitive bool
	at         time.Time
}

var probeMu sync.Mutex
var probeCache = map[string]probeEntry{}

const probeTTL = 60 * time.Second

func cachedProbe(key string, probe func() (any, bool)) any {
	probeMu.Lock()
	e, ok := probeCache[key]
	probeMu.Unlock()
	if ok && (e.definitive || time.Since(e.at) < probeTTL) {
		return e.value
	}
	v, definitive := probe()
	probeMu.Lock()
	probeCache[key] = probeEntry{value: v, definitive: definitive, at: time.Now()}
	probeMu.Unlock()
	return v
}

func serverRoot(baseURL string) string {
	root := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	return strings.TrimSuffix(root, "/v1")
}

// Reasoning allowed options LM Studio publishes for the model
func lmStudioReasoningOptions(model, baseURL, apiKey string) []string {
	v := cachedProbe("lmstudio|"+model+"|"+baseURL, func() (any, bool) {
		root := serverRoot(baseURL)
		if root == "" {
			return []string{}, false
		}
		req, err := http.NewRequest(http.MethodGet, root+"/api/v1/models", nil)
		if err != nil {
			return []string{}, false
		}
		req.Header.Set("Accept", "application/json")
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			return []string{}, false
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return []string{}, false
		}
		var payload map[string]any
		if json.NewDecoder(resp.Body).Decode(&payload) != nil {
			return []string{}, false
		}
		items, _ := payload["data"].([]any)
		if items == nil {
			items, _ = payload["models"].([]any)
		}
		for _, it := range items {
			im, ok := it.(map[string]any)
			if !ok || (stringOf(im["key"]) != model && stringOf(im["id"]) != model) {
				continue
			}
			caps, _ := im["capabilities"].(map[string]any)
			reasoning, _ := caps["reasoning"].(map[string]any)
			opts, _ := reasoning["allowed_options"].([]any)
			var out []string
			for _, o := range opts {
				if s, ok := o.(string); ok {
					out = append(out, strings.ToLower(strings.TrimSpace(s)))
				}
			}
			return out, len(out) > 0
		}
		return []string{}, false
	})
	opts, _ := v.([]string)
	return opts
}

// Reports whether an Ollama server advertises the thinking capability for the model
func ollamaSupportsThinking(model, baseURL, apiKey string) bool {
	v := cachedProbe("ollama|"+model+"|"+baseURL, func() (any, bool) {
		root := serverRoot(baseURL)
		bare := strings.TrimSpace(model)
		for _, suf := range []string{":cloud", "-cloud"} {
			bare = strings.TrimSuffix(bare, suf)
		}
		if root == "" || bare == "" {
			return nil, false
		}
		body, _ := json.Marshal(map[string]any{"name": bare})
		req, err := http.NewRequest(http.MethodPost, root+"/api/show", bytes.NewReader(body))
		if err != nil {
			return nil, false
		}
		req.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			return nil, false
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, false
		}
		var payload map[string]any
		if json.NewDecoder(resp.Body).Decode(&payload) != nil {
			return nil, false
		}
		caps, ok := payload["capabilities"].([]any)
		if !ok {
			return nil, false
		}
		for _, c := range caps {
			if c == "thinking" {
				return true, true
			}
		}
		return false, true
	})
	b, _ := v.(bool)
	return b
}

func looksLikeOllamaEndpoint(baseURL string) bool {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return false
	}
	if !strings.Contains(raw, "://") {
		raw = "//" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Port() == "11434" {
		return true
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return false
	}
	if host == "ollama.com" || strings.HasSuffix(host, ".ollama.com") {
		return true
	}
	for _, label := range strings.Split(host, ".") {
		if label == "ollama" {
			return true
		}
	}
	return false
}

var openRouterReasoningPrefixes = []string{"deepseek/", "anthropic/", "openai/", "x-ai/", "google/gemini-2", "google/gemma-4", "qwen/qwen3", "tencent/hy", "xiaomi/"}

// Reports whether a reasoning extra body is safe to send on this route
func (c *ccRoute) supportsReasoning() bool {
	if c.hostIs("nousresearch.com") || c.hostIs("ai-gateway.vercel.sh") {
		return true
	}
	if c.provider == "lmstudio" {
		for _, opt := range lmStudioReasoningOptions(c.route.Model, c.baseURL, c.route.APIKey) {
			if opt != "" && opt != "off" {
				return true
			}
		}
		return false
	}
	if c.hostIs("ollama.com") {
		return ollamaSupportsThinking(c.route.Model, c.baseURL, c.route.APIKey)
	}
	if !c.isOpenRouter() || c.hostIs("api.mistral.ai") {
		return false
	}
	if caps, ok := openRouterCaps(c.route.Model); ok {
		return caps.supports
	}
	return hasAnyPrefix(c.modelLower, openRouterReasoningPrefixes)
}

// ---- OpenRouter pins and anthropic mandatory reasoning ----

var anthropicReasoningOptionalSubstrings = []string{
	"claude-3", "claude-opus-4-0", "claude-opus-4.0", "claude-opus-4-1", "claude-opus-4.1", "claude-sonnet-4-0", "claude-sonnet-4.0",
	"claude-opus-4-2025", "claude-sonnet-4-2025", "claude-opus-4-5", "claude-opus-4.5", "claude-sonnet-4-5", "claude-sonnet-4.5",
	"claude-haiku-4-5", "claude-haiku-4.5",
}

func anthropicReasoningIsMandatory(model string) bool {
	m := strings.ToLower(model)
	if !strings.HasPrefix(m, "anthropic/") && !strings.HasPrefix(m, "claude") && !strings.Contains(m, "claude") {
		return false
	}
	for _, sub := range anthropicReasoningOptionalSubstrings {
		if strings.Contains(m, sub) {
			return false
		}
	}
	return true
}

type endpointPin struct {
	base string
	tags []string
}

var openRouterEndpointPins = func() map[string]endpointPin {
	out := map[string]endpointPin{}
	tiers := map[string][]string{"": {"openai", "azure", "azure/us"}, "-fast": {"openai/fast"}, "-flex": {"openai/flex"}}
	for _, base := range []string{"openai/gpt-6-astra", "openai/gpt-6-astra-pro"} {
		for suffix, tags := range tiers {
			out[base+suffix] = endpointPin{base: base, tags: tags}
		}
	}
	return out
}()

var grokAggregatorPrefixes = []string{"x-ai/grok-", "xai/grok-"}

// Reasoning config clamped to the OpenRouter catalog, omitted for a mandatory route disable
func clampReasoningToOpenRouterCatalog(c reasoningCfg, model string) (map[string]any, bool) {
	disabled := c.disabled || c.effort == "none"
	if c.effort == "" && !disabled {
		return c.object(), true
	}
	caps, ok := openRouterCaps(model)
	if !ok || !caps.supports {
		return c.object(), true
	}
	if disabled {
		noDisable := strings.HasPrefix(model, "openai/") && !containsStr(codexSupportedEfforts(model), "none")
		if caps.mandatory || noDisable {
			return nil, false
		}
		return c.object(), true
	}
	clamped := clampEffort(c.effort, caps.supportedEfforts, nil)
	if clamped != "" && clamped != c.effort {
		c.effort = clamped
	}
	return c.object(), true
}

// ---- build ----

// One wire request ready to send
type ccBuild struct {
	body      *omap
	messages  []*omap
	headers   map[string]string
	aliases   map[string]string
	extraBody *omap
	err       error
}

// Settings read from the home configuration that shape requests
type wireSettings struct {
	minCodingScore        float64
	streamMaxLifetime     float64
	streamDrainTimeout    float64
	localStreamStale      float64
	streamStale           float64
	showCommentary        bool
	textVerbosity         string
	reasoningEcho         bool
	providerRouting       map[string]any
	providers             map[string]any
	compressionEnabled    bool
	nativeCompaction      bool
	compactThreshold      any
	compressionThreshold  any
	compressionThresholdT any
	replayRecentImages    int
}

var settingsMu sync.Mutex

// Configuration knobs of the home in effect for the context
func loadWireSettings(ctx context.Context) wireSettings {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	h := home.Dir(ctx)
	tree, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		tree = config.Defaults()
	}
	return wireSettings{
		minCodingScore:        tree.Float("openrouter.min_coding_score", 0.65),
		streamMaxLifetime:     tree.Float("agent.stream_max_lifetime", 0),
		streamDrainTimeout:    tree.Float("agent.stream_drain_timeout", 2.0),
		localStreamStale:      tree.Float("agent.local_stream_stale_timeout", 900),
		streamStale:           tree.Float("agent.stream_stale_timeout", 180),
		showCommentary:        tree.Bool("display.show_commentary", true),
		textVerbosity:         tree.String("agent.text_verbosity", ""),
		reasoningEcho:         tree.Bool("agent.reasoning_echo", false) || tree.Bool("model.reasoning_echo", false),
		providerRouting:       tree.Map("provider_routing"),
		providers:             tree.Map("providers"),
		compressionEnabled:    tree.Bool("compression.enabled", true),
		nativeCompaction:      tree.Bool("codex_responses.native_compaction", false),
		compactThreshold:      tree.Get("codex_responses.compact_threshold"),
		compressionThreshold:  tree.Get("compression.threshold"),
		compressionThresholdT: tree.Get("compression.threshold_tokens"),
		replayRecentImages:    tree.Int("vision.replay_recent_images", 3),
	}
}

// Per provider request or stale timeout from the providers configuration block
func configuredProviderTimeout(providers map[string]any, provider, model, modelKey, providerKey string) (time.Duration, bool) {
	f, ok := configuredProviderSeconds(providers, provider, model, modelKey, providerKey)
	if !ok {
		return 0, false
	}
	return secondsDuration(f), true
}

// Environment float with an ICEBATH prefixed name, the default when unset or unparseable
func envFloat(name string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func envInt(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envTruthy(name string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// Validated OpenRouter provider routing object from the provider_routing configuration
func providerPreferences(cfg map[string]any, model string) map[string]any {
	if cfg == nil {
		return nil
	}
	flat := map[string]any{}
	for _, k := range []string{"only", "ignore", "order", "sort", "require_parameters", "data_collection"} {
		if v, ok := cfg[k]; ok {
			flat[k] = v
		}
	}
	if models, ok := cfg["models"].(map[string]any); ok {
		for _, variant := range modelVariants(model) {
			if entry, ok := models[variant].(map[string]any); ok {
				for k, v := range entry {
					if _, allowed := map[string]bool{"only": true, "ignore": true, "order": true, "sort": true, "require_parameters": true, "data_collection": true}[k]; allowed {
						flat[k] = v
					}
				}
				break
			}
		}
	}
	if s, ok := validatedOpenRouterProviderSort(flat["sort"]); ok {
		flat["sort"] = s
	} else {
		delete(flat, "sort")
	}
	if config.Truthy(flat["require_parameters"]) {
		flat["require_parameters"] = true
	} else {
		delete(flat, "require_parameters")
	}
	out := map[string]any{}
	for k, v := range flat {
		if config.Truthy(v) {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Spellings of a model id a per model override table may use
func modelVariants(model string) []string {
	m := strings.TrimSpace(model)
	variants := []string{m, strings.ToLower(m)}
	if i := strings.LastIndex(m, "/"); i >= 0 {
		variants = append(variants, m[i+1:], strings.ToLower(m[i+1:]))
	}
	return variants
}

// Merges override keys into the body, nesting an extra_body override into the extra body object
func mergeOverrides(body, extra *omap, overrides map[string]any) {
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := overrides[k]
		if k == "extra_body" {
			if m, ok := v.(map[string]any); ok {
				ik := make([]string, 0, len(m))
				for kk := range m {
					ik = append(ik, kk)
				}
				sort.Strings(ik)
				for _, kk := range ik {
					extra.Set(kk, m[kk])
				}
			}
			continue
		}
		body.Set(k, v)
	}
}

// Builds the chat completions request body and headers for a route and request
func (c *ccRoute) build(ctx context.Context, req *Request, settings wireSettings) *ccBuild {
	rcfg := reasoningFromRequest(req.Reasoning)
	if !rcfg.set {
		switch c.provider {
		case "custom":
			rcfg = reasoningCfg{set: true, effort: "medium"}
		case "tencent-tokenhub":
			rcfg = reasoningCfg{set: true, effort: "high"}
		}
		if c.provider == "custom" && looksLikeOllamaEndpoint(c.baseURL) && !ollamaSupportsThinking(c.route.Model, c.baseURL, c.route.APIKey) {
			rcfg = reasoningCfg{}
		}
	}
	rcfg = clampReasoningCfg(rcfg, openAICompatWireEfforts)
	msgs := c.convertMessages(req.Messages, settings.reasoningEcho)
	tools, aliases := c.convertTools(req.Tools)
	body := newOmap()
	extra := newOmap()
	top := map[string]any{}
	var buildErr error
	body.Set("model", req.Model)
	body.Set("messages", msgs)
	omitTemp, fixed := c.fixedTemperature()
	if !omitTemp {
		if fixed != nil {
			body.Set("temperature", *fixed)
		} else if req.Temperature != nil {
			body.Set("temperature", *req.Temperature)
		}
	}
	if req.TopP != nil && !omitTemp {
		body.Set("top_p", *req.TopP)
	}
	if len(req.Stop) > 0 {
		body.Set("stop", req.Stop)
	}
	if len(tools) > 0 {
		body.Set("tools", tools)
		if req.ToolChoice != nil {
			body.Set("tool_choice", req.ToolChoice)
		}
		if req.ParallelToolCalls != nil {
			body.Set("parallel_tool_calls", *req.ParallelToolCalls)
		}
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = c.profileMaxTokens()
	}
	if c.provider == "gemini" {
		maxTokens = raiseGeminiThinkingMaxTokens(req.Model, rcfg, maxTokens)
	}
	if maxTokens > 0 {
		body.Set(c.maxTokensParam(), maxTokens)
	}
	supports := c.supportsReasoning()
	c.applyProviderReasoning(req, rcfg, supports, settings, body, extra, top)
	for k, v := range top {
		if k == "extra_headers" {
			continue
		}
		body.Set(k, v)
	}
	if req.ResponseFormat != nil {
		unsupported, err := c.responseFormatUnsupported(ctx, req.Model, req.ResponseFormat["type"])
		if err != nil {
			buildErr = err
		} else if !unsupported {
			body.Set("response_format", req.ResponseFormat)
		}
	}
	if tier := c.serviceTier(req); tier != "" {
		body.Set("service_tier", tier)
	}
	mergeOverrides(body, extra, c.route.ExtraBody)
	mergeOverrides(body, extra, req.ExtraBody)
	if c.isNativeGemini() {
		for _, k := range extra.keys {
			if k != "thinking_config" && k != "thinkingConfig" && k != "max_output_tokens" {
				extra.Del(k)
			}
		}
	}
	if pck, ok := body.Get("prompt_cache_key"); ok {
		if b := boundedPromptCacheKey(pck); b != "" {
			body.Set("prompt_cache_key", b)
		} else {
			body.Del("prompt_cache_key")
		}
	} else if pck, ok := extra.Get("prompt_cache_key"); ok {
		if b := boundedPromptCacheKey(pck); b != "" {
			extra.Set("prompt_cache_key", b)
		} else {
			extra.Del("prompt_cache_key")
		}
	} else if c.isDirectOpenAI() {
		scope := cacheScopeFromSessionID(firstNonEmpty(req.CacheScope, req.SessionID))
		if key := contentCacheKey(staticPromptInstructions(msgs), tools, scope); key != "" {
			body.Set("prompt_cache_key", key)
		}
	}
	headers := c.requestHeaders(req, top)
	return &ccBuild{body: body, messages: msgs, headers: headers, aliases: aliases, extraBody: extra, err: buildErr}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

var anthropicFastModeModels = map[string]bool{"claude-opus-4-8": true, "claude-opus-5": true, "claude-opus-5-5": true}
var openAIFastModePrefixes = []string{"gpt-5", "gpt-6", "gpt-daybreak"}
var openAIUltrafastModels = map[string]bool{"gpt-6-astra": true, "gpt-6.1-sol": true}

func isGrok46Family(model string) bool {
	name := strings.ReplaceAll(bareModel(model), "_", "-")
	return name == "grok-4.6" || strings.HasPrefix(name, "grok-4.6-")
}

// Service tier the route accepts for the request, empty to send none
func (c *ccRoute) serviceTier(req *Request) string {
	tier := strings.ToLower(strings.TrimSpace(req.ServiceTier))
	if tier == "" || tier == "normal" || tier == "default" || tier == "standard" {
		return ""
	}
	bare := strings.Split(bareModel(req.Model), ":")[0]
	if _, ok := anthropicFastModeModels[regexp.MustCompile(`-\d{8}$`).ReplaceAllString(regexp.MustCompile(`(\d)\.(\d)`).ReplaceAllString(bare, "$1-$2"), "")]; ok {
		return ""
	}
	if isGrok46Family(req.Model) {
		if (c.provider == "xai" || c.provider == "xai-oauth" || c.host == "" || c.host == "api.x.ai") && tier == "priority" {
			return "priority"
		}
		return ""
	}
	if !hasAnyPrefix(bare, openAIFastModePrefixes) || strings.Contains(bare, "codex") {
		return ""
	}
	if c.provider != "" && c.provider != "openai" && c.provider != "openai-api" && c.provider != "openai-codex" {
		return ""
	}
	if c.host != "" && c.host != "api.openai.com" && c.host != "chatgpt.com" {
		return ""
	}
	if tier == "ultrafast" {
		if openAIUltrafastModels[strings.TrimSuffix(bare, "-900k")] {
			return "ultrafast"
		}
		return ""
	}
	if tier == "priority" || tier == "fast" || tier == "on" {
		return "priority"
	}
	return ""
}

// Provider specific reasoning and extra body shaping for the chat completions wire
func (c *ccRoute) applyProviderReasoning(req *Request, rcfg reasoningCfg, supports bool, settings wireSettings, body, extra *omap, top map[string]any) {
	sticky := cacheScopeFromSessionID(firstNonEmpty(req.CacheScope, req.SessionID))
	known := knownProviders[c.provider]
	putExtra := func(m map[string]any) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			extra.Set(k, m[k])
		}
	}
	switch {
	case c.provider == "openrouter":
		if sticky != "" {
			extra.Set("session_id", sticky)
		}
		prefs := providerPreferences(settings.providerRouting, req.Model)
		if pin, ok := openRouterEndpointPins[req.Model]; ok {
			userOnly := prefs != nil && config.Truthy(prefs["only"])
			if !(pin.base == req.Model && userOnly) {
				if prefs == nil {
					prefs = map[string]any{}
				}
				prefs["only"] = pin.tags
			}
			if pin.base != req.Model {
				body.Set("model", pin.base)
			}
		}
		if prefs != nil {
			extra.Set("provider", prefs)
		}
		if req.Model == "openrouter/pareto-code" && settings.minCodingScore >= 0 && settings.minCodingScore <= 1 {
			extra.Set("plugins", []any{map[string]any{"id": "pareto-router", "min_coding_score": settings.minCodingScore}})
		}
		if supports {
			if anthropicReasoningIsMandatory(req.Model) {
				if rcfg.set && !rcfg.disabled && rcfg.effort != "" && rcfg.effort != "none" {
					top["verbosity"] = rcfg.effort
				}
			} else if rcfg.set {
				if obj, ok := clampReasoningToOpenRouterCatalog(rcfg, req.Model); ok {
					extra.Set("reasoning", obj)
				}
			} else {
				extra.Set("reasoning", map[string]any{"enabled": true, "effort": "medium"})
			}
		}
		if sticky != "" && hasAnyPrefix(c.modelLower, grokAggregatorPrefixes) {
			top["extra_headers"] = map[string]string{"x-grok-conv-id": sticky}
		}
	case c.isNous() && known:
		tags := []any{"product=icebath-agent", "client=icebath-client-v" + BuildVersion}
		if conv := firstNonEmpty(req.CacheScope, req.SessionID); conv != "" {
			tags = append(tags, "conversation="+conv)
		}
		extra.Set("tags", tags)
		if sticky != "" {
			extra.Set("session_id", sticky)
		}
		if du := dataUseRequirement(req.Messages); len(du) > 0 {
			prov := map[string]any{"data_collection": "deny"}
			if du["zero_retention"] {
				prov["zdr"] = true
			}
			extra.Set("provider", prov)
		}
		if supports {
			if !rcfg.set {
				extra.Set("reasoning", map[string]any{"enabled": true, "effort": "medium"})
			} else if rcfg.disabled {
				if caps, ok := nousCaps(c.baseURL, req.Model); ok && caps.supports && !caps.mandatory {
					extra.Set("reasoning", rcfg.object())
				}
			} else {
				extra.Set("reasoning", rcfg.object())
			}
		}
	case c.provider == "deepseek" || (c.provider == "commandcode" && strings.HasPrefix(c.modelLower, "deepseek/")):
		m := c.bare
		if c.provider == "commandcode" {
			m = strings.ToLower(strings.SplitN(req.Model, "/", 2)[1])
		}
		versioned := strings.HasPrefix(m, "deepseek-v") && !strings.HasPrefix(m, "deepseek-v3")
		if versioned || m == "deepseek-flash" {
			if rcfg.set && (rcfg.effort == "none" || rcfg.effort == "false" || rcfg.effort == "disabled") {
				extra.Set("thinking", map[string]any{"type": "disabled"})
			} else {
				e, t := thinkingToggleExtras(rcfg, deepseekV4Efforts, deepseekV4Overrides, true)
				putExtra(e)
				for k, v := range t {
					top[k] = v
				}
			}
		}
	case c.provider == "kimi-coding" || c.provider == "kimi-coding-cn":
		e, t := thinkingToggleExtras(rcfg, kimiK3Efforts, kimiK3Overrides, false)
		if th, ok := e["thinking"].(map[string]any); ok && th["type"] == "enabled" && strings.Contains(c.modelLower, "k2") {
			th["keep"] = "all"
		}
		putExtra(e)
		for k, v := range t {
			top[k] = v
		}
	case c.provider == "qwen-oauth":
		extra.Set("vl_high_resolution_images", true)
		sid := req.SessionID
		if sid == "" {
			sid = "icebath"
		}
		top["metadata"] = map[string]any{"sessionId": sid, "promptId": uuid.NewString()}
	case c.provider == "lmstudio":
		opts := lmStudioReasoningOptions(req.Model, c.baseURL, c.route.APIKey)
		if len(opts) == 0 {
			putExtra(genericNestedReasoning(rcfg))
		} else if supports {
			if effort, ok := resolveLMStudioEffort(rcfg, opts); ok {
				top["reasoning_effort"] = effort
			}
		}
	case c.provider == "ollama-cloud":
		if supports && rcfg.set {
			if rcfg.disabled || rcfg.effort == "none" {
				top["reasoning_effort"] = "none"
			} else if rcfg.effort != "" {
				clamped := clampEffort(rcfg.effort, ollamaCloudEfforts, ollamaCloudOverrides)
				if containsStr(ollamaCloudEfforts, clamped) {
					top["reasoning_effort"] = clamped
				}
			}
		}
	case c.provider == "custom":
		if rcfg.set {
			switch {
			case rcfg.effort == "none" || rcfg.disabled:
				top["reasoning_effort"] = "none"
				if looksLikeOllamaEndpoint(c.baseURL) {
					extra.Set("think", false)
				}
			case rcfg.effort != "" && c.hostIs("api.groq.com"):
				top["reasoning_effort"] = "default"
			case rcfg.effort != "":
				top["reasoning_effort"] = clampEffort(rcfg.effort, openAICompatWireEfforts, nil)
			}
		}
	case c.provider == "zai":
		is52 := containsAnySub(c.modelLower, "glm-5.2", "glm-5-2", "glm-5p2", "glm-5.3", "glm-5-3", "glm-5p3")
		is53 := containsAnySub(c.modelLower, "glm-5.3", "glm-5-3", "glm-5p3")
		if glmSupportsThinking(c.modelLower) || is52 {
			if rcfg.set {
				enabled := !rcfg.disabled
				if !enabled && is53 {
					enabled = true
					top["reasoning_effort"] = "low"
				}
				if enabled {
					extra.Set("thinking", map[string]any{"type": "enabled", "clear_thinking": false})
				} else {
					extra.Set("thinking", map[string]any{"type": "disabled"})
				}
			}
			if is52 {
				if effort := requestedEffort(rcfg); effort != "" && effort != "none" {
					efforts, overrides, floor := glm52Efforts, glm52Overrides, "high"
					if is53 {
						efforts, overrides, floor = glm53Efforts, glm53Overrides, "low"
					}
					clamped := clampEffort(effort, efforts, overrides)
					if !containsStr(efforts, clamped) {
						clamped = floor
					}
					top["reasoning_effort"] = clamped
				}
			}
		}
	case c.provider == "gemini":
		if raw := buildGeminiThinkingConfig(req.Model, rcfg); raw != nil {
			if c.isGeminiOpenAICompat() {
				if snake := snakeCaseGeminiThinkingConfig(raw); snake != nil {
					extra.Set("extra_body", map[string]any{"google": map[string]any{"thinking_config": snake}})
				}
			} else {
				extra.Set("thinking_config", raw)
			}
		}
	case c.provider == "tencent-tokenhub":
		if rcfg.set && !rcfg.disabled {
			top["reasoning_effort"] = tokenhubEffort(requestedEffort(rcfg))
		}
	case c.provider == "minimax" || c.provider == "minimax-cn" || c.provider == "minimax-oauth":
		u, err := url.Parse(c.baseURL)
		isM3 := c.modelLower == "minimax-m3" || c.modelLower == "minimax/minimax-m3"
		if err == nil && strings.ToLower(u.Hostname()) == "api.minimax.io" && strings.ToLower(strings.TrimRight(u.Path, "/")) == "/v1" && isM3 {
			extra.Set("reasoning_split", true)
			if rcfg.set && rcfg.disabled {
				extra.Set("thinking", map[string]any{"type": "disabled"})
			} else if rcfg.set {
				extra.Set("thinking", map[string]any{"type": "adaptive"})
			}
		}
	case strings.HasPrefix(c.provider, "alibaba"):
		putExtra(dashscopePreserveThinkingExtras(req.Model))
	case c.provider == "opencode-go":
		switch {
		case containsAnySub(c.bare, "glm-5.2", "glm-5-2", "glm-5p2"):
			if effort := requestedEffort(rcfg); effort != "" && effort != "none" {
				clamped := clampEffort(effort, glm52Efforts, glm52Overrides)
				if !containsStr(glm52Efforts, clamped) {
					clamped = "high"
				}
				top["reasoning_effort"] = clamped
			}
		case strings.HasPrefix(c.bare, "kimi-k2"):
			if rcfg.set {
				e, t := thinkingToggleExtras(rcfg, kimiK2Efforts, nil, false)
				putExtra(e)
				for k, v := range t {
					top[k] = v
				}
			}
		case (strings.HasPrefix(c.bare, "deepseek-v") && !strings.HasPrefix(c.bare, "deepseek-v3")) || c.bare == "deepseek-reasoner" || c.bare == "deepseek-flash":
			e, t := thinkingToggleExtras(rcfg, deepseekV4Efforts, deepseekV4Overrides, false)
			putExtra(e)
			for k, v := range t {
				top[k] = v
			}
		}
	case c.provider == "opencode-zen":
		if c.bare == "x-preview-f-free" {
			effort := requestedEffort(rcfg)
			if effort == "none" {
				effort = ""
			}
			clamped := clampEffort(effort, oxAlphaEfforts, oxAlphaOverrides)
			if effort != "" && containsStr(oxAlphaEfforts, clamped) {
				top["reasoning_effort"] = clamped
			}
		}
	case c.provider == "fireworks":
		if rcfg.set {
			if rcfg.disabled {
				top["reasoning_effort"] = "none"
			} else if rcfg.effort != "" {
				top["reasoning_effort"] = rcfg.effort
			}
		}
	case c.provider == "upstage":
		if !regexp.MustCompile(`solar-mini(?:[-:.@]|$)|syn-pro`).MatchString(c.modelLower) {
			switch {
			case !rcfg.set:
				top["reasoning_effort"] = "medium"
			case rcfg.disabled:
			case rcfg.effort == "":
				top["reasoning_effort"] = "medium"
			case rcfg.effort == "minimal":
			default:
				mapped := clampEffort(rcfg.effort, solarEfforts, nil)
				if !containsStr(solarEfforts, mapped) {
					if ladderIndex(rcfg.effort) < 0 {
						mapped = "high"
					} else {
						mapped = ""
					}
				}
				if mapped != "" {
					top["reasoning_effort"] = mapped
				}
			}
		}
	case c.provider == "nebius-token-factory":
		markers := []string{"deepseek-r1", "deepseek-v4", "deepseek-reasoner", "gpt-oss", "glm-5", "kimi-k2", "minimax-m2", "qwen3"}
		if supports || containsAnySub(c.bare, markers...) {
			effort := rcfg.effort
			if effort == "" {
				effort = "medium"
			}
			if !(rcfg.set && rcfg.disabled) && effort != "none" && effort != "off" && effort != "disabled" {
				clamped := clampEffort(effort, nebiusEfforts, nil)
				if clamped == "" {
					clamped = "medium"
				}
				top["reasoning_effort"] = clamped
			}
		}
	case c.provider == "meta-ai":
		if rcfg.set && (rcfg.disabled || rcfg.effort == "none") {
			top["reasoning_effort"] = "minimal"
		} else {
			clamped := clampEffort(rcfg.effort, metaAIEfforts, nil)
			if !containsStr(metaAIEfforts, clamped) {
				clamped = "medium"
			}
			top["reasoning_effort"] = clamped
		}
	case c.provider == "deepinfra":
		if rcfg.set && rcfg.disabled {
			top["reasoning_effort"] = "none"
		} else if effort := requestedEffort(rcfg); effort != "" {
			clamped := clampEffort(effort, openAICompatWireEfforts, nil)
			if containsStr(openAICompatWireEfforts, clamped) {
				top["reasoning_effort"] = clamped
			}
		}
	case c.provider == "actual":
		if rcfg.set {
			if !rcfg.disabled && rcfg.effort == "none" {
				extra.Set("thinking", map[string]any{"type": "disabled"})
				top["reasoning_effort"] = "none"
			} else {
				e, t := thinkingToggleExtras(rcfg, actualRelayEfforts, nil, true)
				putExtra(e)
				for k, v := range t {
					top[k] = v
				}
			}
		}
	case c.provider == "ai-gateway":
		if supports {
			if rcfg.set {
				extra.Set("reasoning", rcfg.object())
			} else {
				extra.Set("reasoning", map[string]any{"enabled": true, "effort": "medium"})
			}
		}
	case !known:
		thinkingOff := rcfg.set && rcfg.disabled
		effort := requestedEffort(rcfg)
		if c.isKimiHost() && !thinkingOff {
			supported := kimiSupportedEfforts(req.Model)
			isK3 := len(supported) == len(kimiK3Efforts) && supported[0] == kimiK3Efforts[0]
			if effort == "" {
				if isK3 {
					top["reasoning_effort"] = "high"
				} else {
					top["reasoning_effort"] = "medium"
				}
			} else if isK3 {
				top["reasoning_effort"] = clampEffort(effort, supported, kimiK3Overrides)
			} else {
				top["reasoning_effort"] = clampEffort(effort, supported, nil)
			}
			if thinkingOff {
				extra.Set("thinking", map[string]any{"type": "disabled"})
			} else {
				extra.Set("thinking", map[string]any{"type": "enabled"})
			}
		}
		if c.isTokenHubHost() && !thinkingOff {
			top["reasoning_effort"] = tokenhubEffort(effort)
		}
		if c.isOpenRouter() {
			if prefs := providerPreferences(settings.providerRouting, req.Model); prefs != nil {
				extra.Set("provider", prefs)
			}
			if req.Model == "openrouter/pareto-code" && settings.minCodingScore >= 0 && settings.minCodingScore <= 1 {
				extra.Set("plugins", []any{map[string]any{"id": "pareto-router", "min_coding_score": settings.minCodingScore}})
			}
		}
		if supports {
			e := "medium"
			if rcfg.set && rcfg.effort != "" {
				e = rcfg.effort
			}
			off := thinkingOff || e == "none"
			if off {
				extra.Set("reasoning", map[string]any{"enabled": false, "effort": "none"})
			} else {
				extra.Set("reasoning", map[string]any{"enabled": true, "effort": e})
			}
		}
	}
}

func containsAnySub(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

var glmVersionRe = regexp.MustCompile(`^glm-(\p{Nd}+)(?:\.(\p{Nd}+))?`)

// True for GLM 4.5 and later thinking capable families, version digits read as Python int reads Unicode digits
func glmSupportsThinking(model string) bool {
	m := glmVersionRe.FindStringSubmatch(strings.ToLower(strings.TrimSpace(model)))
	if m == nil {
		return false
	}
	major, minor := unicodeDigitsInt(m[1]), big.NewInt(0)
	if m[2] != "" {
		minor = unicodeDigitsInt(m[2])
	}
	if c := major.Cmp(big.NewInt(4)); c != 0 {
		return c > 0
	}
	return minor.Cmp(big.NewInt(5)) >= 0
}

// Static and per request headers of the route, route headers winning
func (c *ccRoute) requestHeaders(req *Request, top map[string]any) map[string]string {
	h := map[string]string{"User-Agent": userAgent("IcebathAgent")}
	switch c.provider {
	case "kimi-coding", "kimi-coding-cn":
		h["HTTP-Referer"], h["X-Title"], h["Accept-Encoding"] = attributionReferer, attributionTitle, "gzip"
	case "openrouter", "ai-gateway", "fireworks", "opencode-zen", "opencode-go":
		h["HTTP-Referer"], h["X-Title"] = attributionReferer, attributionTitle
	case "xai", "xai-oauth", "router":
		h["User-Agent"] = userAgent("Icebath-Agent")
	}
	if c.provider == "opencode-zen" || c.provider == "opencode-go" || strings.HasPrefix(c.provider, "opencode-") || c.hostIs("opencode.ai") {
		key := cacheScopeFromSessionID(firstNonEmpty(req.CacheScope, req.SessionID))
		if key == "" {
			key = "oneshot-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
		}
		h["x-opencode-session"] = key
	}
	if eh, ok := top["extra_headers"].(map[string]string); ok {
		for k, v := range eh {
			h[k] = v
		}
	}
	for k, v := range c.route.Headers {
		h[k] = v
	}
	return h
}

func containsAnyStr(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// Normalized OpenRouter provider.sort value, warning about and dropping a string outside the allowed set
func validatedOpenRouterProviderSort(raw any) (string, bool) {
	text, ok := raw.(string)
	if !ok {
		return "", false
	}
	value := pystr.Lower(pystr.Strip(text))
	if value == "" {
		return "", false
	}
	switch value {
	case "throughput", "latency", "price":
		return value, true
	}
	slog.Warn(fmt.Sprintf("Ignoring invalid OpenRouter provider.sort value %s (allowed: latency, price, throughput)", pyrepr.Repr(raw)))
	return "", false
}

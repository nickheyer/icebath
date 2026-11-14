// Anthropic Messages wire protocol, native and over Bedrock InvokeModel
package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/proc"
)

const (
	apiVersion              = "2023-06-01"
	bedrockVersion          = "bedrock-2023-05-31"
	toolStreamingBeta       = "fine-grained-tool-streaming-2025-05-14"
	interleavedThinkingBeta = "interleaved-thinking-2025-05-14"
	context1MBeta           = "context-1m-2025-08-07"
	fastModeBeta            = "fast-mode-2026-02-01"
	claudeCodeFallback      = "2.1.74"
	defaultOutputLimit      = 128000
	GuardrailActionField    = "amazon-bedrock-guardrailAction"
)

var oauthBetas = []string{"claude-code-20250219", "oauth-2025-04-20"}

var outputLimits = map[string]int{
	"claude-fable": 128000, "claude-sonnet-5": 128000, "claude-opus-4-8": 128000,
	"claude-opus-4-7": 128000, "claude-opus-4-6": 128000, "claude-sonnet-4-6": 64000,
	"claude-opus-4-5": 64000, "claude-sonnet-4-5": 64000, "claude-haiku-4-5": 64000,
	"claude-opus-4": 32000, "claude-sonnet-4": 64000, "claude-3-7-sonnet": 128000,
	"claude-3-5-sonnet": 8192, "claude-3-5-haiku": 8192, "claude-3-opus": 4096,
	"claude-3-sonnet": 4096, "claude-3-haiku": 4096, "minimax": 131072, "qwen3": 65536,
}

var thinkingBudgets = map[string]int{"xhigh": 32000, "high": 16000, "medium": 8000, "low": 4000}

var adaptiveEfforts = map[string]string{
	"ultra": "max", "max": "max", "xhigh": "xhigh", "high": "high", "medium": "medium", "low": "low", "minimal": "low",
}

var (
	legacyManualThinking = []string{
		"claude-3", "claude-opus-4-0", "claude-opus-4.0", "claude-opus-4-1", "claude-opus-4.1",
		"claude-sonnet-4-0", "claude-sonnet-4.0", "claude-opus-4-2025", "claude-sonnet-4-2025",
		"claude-opus-4-5", "claude-opus-4.5", "claude-sonnet-4-5", "claude-sonnet-4.5", "claude-haiku-4-5", "claude-haiku-4.5",
	}
	noXHigh           = []string{"claude-opus-4-6", "claude-opus-4.6", "claude-sonnet-4-6", "claude-sonnet-4.6"}
	mandatoryThinking = []string{"claude-fable", "claude-mythos", "claude-opus-5-5", "claude-opus-5.5"}
	betweenToolsOff   = []string{"claude-sonnet-5-5", "claude-sonnet-5.5"}
	fastModeModels    = map[string]bool{"claude-opus-4-8": true, "claude-opus-5": true, "claude-opus-5-5": true}
	bedrockPrefixes   = []string{"global.", "us.", "eu.", "apac.", "ap.", "au.", "jp.", "ca.", "sa.", "me.", "af.", "anthropic."}
	claudeCodeNames   = []string{"claude", "claude-code"}
	claudeCodeDirs    = []string{"~/.local/bin", "~/.claude/local", "~/bin", "~/.npm-global/bin", "~/.bun/bin", "~/.volta/bin", "/opt/homebrew/bin", "/usr/local/bin"}
	dottedVersionRE   = regexp.MustCompile(`(\d)\.(\d)`)
	dateSuffixRE      = regexp.MustCompile(`-\d{8}$`)
	finishReasons     = map[string]wire.FinishReason{
		"end_turn": wire.FinishStop, "tool_use": wire.FinishToolCalls, "max_tokens": wire.FinishLength, "stop_sequence": wire.FinishStop,
		"refusal": wire.FinishContentFilter, "model_context_window_exceeded": wire.FinishLength,
	}
)

type Protocol struct {
	Version    string
	once       sync.Once
	claudeCode string
}

var _ wire.Protocol = (*Protocol)(nil)

func modelMatches(model string, substrings []string) bool {
	m := strings.ToLower(model)
	for _, s := range substrings {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

func isClaude(model string) bool { return strings.Contains(strings.ToLower(model), "claude") }

func MaxOutput(model string) int {
	m := strings.ReplaceAll(strings.ToLower(model), ".", "-")
	best := ""
	for key := range outputLimits {
		if strings.Contains(m, key) && (len(key) > len(best) || (len(key) == len(best) && key < best)) {
			best = key
		}
	}
	if best == "" {
		return defaultOutputLimit
	}
	return outputLimits[best]
}

func SupportsAdaptiveThinking(model string) bool {
	return IsKimiFamilyModel(model) || (isClaude(model) && !modelMatches(model, legacyManualThinking))
}

func supportsXHigh(model string) bool {
	return SupportsAdaptiveThinking(model) && !modelMatches(model, noXHigh)
}

func acceptsThinkingDisable(model string) bool {
	return isClaude(model) && SupportsAdaptiveThinking(model) && !modelMatches(model, mandatoryThinking) && !modelMatches(model, betweenToolsOff)
}

func isPreThinkingHaiku(model string) bool {
	if !strings.Contains(strings.ToLower(model), "haiku") {
		return false
	}
	family, major, minor, ok := claudeFamilyVersion(model)
	return !ok || family != "haiku" || major < 5 || (major == 5 && minor < 5)
}

func ForbidsSampling(model string) bool {
	return isClaude(model) && !modelMatches(model, noXHigh) && !modelMatches(model, legacyManualThinking)
}

func SupportsFastMode(model string) bool {
	name := strings.ToLower(strings.TrimSpace(model))
	name, _, _ = strings.Cut(name, ":")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = dottedVersionRE.ReplaceAllString(name, "$1-$2")
	return fastModeModels[dateSuffixRE.ReplaceAllString(name, "")]
}

func isBedrockModelID(model string) bool {
	return hasAnyPrefix(strings.ToLower(model), bedrockPrefixes)
}

func NormalizeModelName(model string, preserveDots bool) string {
	if strings.HasPrefix(strings.ToLower(model), "anthropic/") {
		model = model[len("anthropic/"):]
	}
	lower := strings.ToLower(model)
	if !preserveDots && !isBedrockModelID(model) && (strings.HasPrefix(lower, "claude-") || strings.HasPrefix(lower, "anthropic/")) {
		model = strings.ReplaceAll(model, ".", "-")
	}
	return model
}

type Thinking struct {
	Thinking     json.RawMessage
	OutputConfig json.RawMessage
	Temperature  bool
	MaxTokens    int
}

func ThinkingFields(r *wire.Reasoning, model string, maxTokens int) Thinking {
	if r == nil {
		return Thinking{}
	}
	if !r.Enabled {
		if modelMatches(model, betweenToolsOff) {
			return Thinking{Thinking: json.RawMessage(`{"type":"between_tools"}`)}
		}
		if acceptsThinkingDisable(model) {
			return Thinking{Thinking: json.RawMessage(`{"type":"disabled"}`)}
		}
		return Thinking{}
	}
	if isPreThinkingHaiku(model) {
		return Thinking{}
	}
	effort := strings.ToLower(r.Effort)
	if effort == "" {
		effort = "medium"
	}
	if SupportsAdaptiveThinking(model) {
		adaptive, ok := adaptiveEfforts[effort]
		if !ok {
			adaptive = "medium"
		}
		if adaptive == "xhigh" && !supportsXHigh(model) {
			adaptive = "max"
		}
		return Thinking{
			Thinking:     json.RawMessage(`{"type":"adaptive","display":"summarized"}`),
			OutputConfig: (&wire.Dict{}).SetString("effort", adaptive).JSON(),
		}
	}
	budget, ok := thinkingBudgets[effort]
	if !ok {
		budget = 8000
	}
	return Thinking{
		Thinking:    (&wire.Dict{}).SetString("type", "enabled").SetInt("budget_tokens", budget).JSON(),
		Temperature: true,
		MaxTokens:   max(maxTokens, budget+4096),
	}
}

func commonBetas(baseURL string, dropContext1M bool) []string {
	betas := []string{interleavedThinkingBeta, toolStreamingBeta}
	if needsContext1MBeta(baseURL) && !dropContext1M {
		betas = append(betas, context1MBeta)
	}
	if IsMiniMax(baseURL) {
		return []string{interleavedThinkingBeta}
	}
	return betas
}

func (p *Protocol) attribution(h http.Header, override bool) {
	for key, value := range map[string]string{"HTTP-Referer": "https://icebath.app", "X-Title": "icebath", "User-Agent": "icebath/" + p.Version} {
		if override || h.Get(key) == "" {
			h.Set(key, value)
		}
	}
}

func claudeCodeCandidates() []string {
	var out []string
	seen := map[string]bool{}
	add := func(path string) {
		if !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	for _, name := range claudeCodeNames {
		if hit, err := exec.LookPath(name); err == nil {
			add(hit)
		}
	}
	for _, dir := range claudeCodeDirs {
		for _, name := range claudeCodeNames {
			path := filepath.Join(home.ExpandUser(dir), name)
			if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
				add(path)
			}
		}
	}
	return out
}

func FindClaudeCodeCLI(command string) (string, bool) {
	if command != claudeCodeNames[0] && command != claudeCodeNames[1] {
		return "", false
	}
	if hit, err := exec.LookPath(command); err == nil {
		return hit, true
	}
	for _, dir := range claudeCodeDirs {
		path := filepath.Join(home.ExpandUser(dir), command)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path, true
		}
	}
	return "", false
}

func detectClaudeCodeVersion() string {
	for _, candidate := range claudeCodeCandidates() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.Command(candidate, "--version")
		proc.Hide(cmd)
		res, err := proc.Run(ctx, cmd)
		cancel()
		if err != nil || !res.Exit.Success() {
			continue
		}
		fields := strings.Fields(string(res.Stdout))
		if len(fields) > 0 && fields[0][0] >= '0' && fields[0][0] <= '9' {
			return fields[0]
		}
	}
	return claudeCodeFallback
}

func (p *Protocol) ClaudeCodeVersion() string {
	p.once.Do(func() { p.claudeCode = detectClaudeCodeVersion() })
	return p.claudeCode
}

func quoteModel(model string) string {
	var b strings.Builder
	for i := 0; i < len(model); i++ {
		c := model[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~', c == ':':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte("0123456789ABCDEF"[c>>4])
			b.WriteByte("0123456789ABCDEF"[c&15])
		}
	}
	return b.String()
}

func responseFormatSchema(format wire.Value) (json.RawMessage, bool) {
	if !format.IsObject() {
		return nil, false
	}
	switch format.Get("type").Text() {
	case "json_schema":
		schema := format.Path("json_schema", "schema")
		if !format.Get("json_schema").IsObject() || !schema.Exists() {
			return nil, false
		}
		return schema.Raw(), true
	case "json_object":
		return json.RawMessage(`{"type":"object"}`), true
	}
	return nil, false
}

func setOutputFormat(outputConfig *wire.Dict, schema json.RawMessage) {
	outputConfig.Set("format", (&wire.Dict{}).SetString("type", "json_schema").Set("schema", schema).JSON())
}

func (p *Protocol) Build(req *wire.Request) (*wire.Call, error) {
	o := req.Anthropic
	conv := &converter{baseURL: o.BaseURL, model: req.Model, portal: o.KeepCatalogIDs, rasterize: o.RasterizeSVG, resolve: req.Bedrock.ResolveProfile}
	system, turns := conv.convertMessages(req.Messages)
	tools := convertTools(req.Tools)
	offered := map[string]bool{}
	for _, tool := range tools {
		offered[tool.Get("name").Text()] = true
	}
	model := req.Model
	if !o.KeepCatalogIDs {
		model = NormalizeModelName(model, o.PreserveDots)
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = MaxOutput(model)
	}
	if o.ContextLength > 0 && maxTokens > o.ContextLength {
		maxTokens = max(o.ContextLength-1, 1)
	}
	var toWire func(string) string
	if o.OAuth {
		toWire = oauthNamer(tools)
		system = applyClaudeCodeIdentity(system, tools, turns, toWire)
	}
	var toolChoice json.RawMessage
	sendTools := len(tools) > 0
	if sendTools {
		forced := req.ToolChoice.Name
		mode := req.ToolChoice.Mode
		if forced == "" && mode != "" && mode != "auto" && mode != "required" && mode != "none" {
			forced = mode
		}
		switch {
		case forced != "":
			if toWire != nil {
				forced = toWire(forced)
			}
			toolChoice = (&wire.Dict{}).SetString("type", "tool").SetString("name", forced).JSON()
		case mode == "none":
			sendTools = false
		case mode == "required":
			toolChoice = json.RawMessage(`{"type":"any"}`)
		default:
			toolChoice = json.RawMessage(`{"type":"auto"}`)
		}
	}
	thinking := ThinkingFields(req.Reasoning, model, maxTokens)
	if thinking.MaxTokens > 0 {
		maxTokens = thinking.MaxTokens
	}
	outputConfig := &wire.Dict{}
	if thinking.OutputConfig != nil {
		if v, err := wire.ParseValue(thinking.OutputConfig); err == nil {
			outputConfig = wire.DictOf(v)
		}
	}
	if f := req.ResponseFormat; f != nil {
		switch f.Type {
		case "json_schema":
			if len(f.Schema) > 0 {
				setOutputFormat(outputConfig, f.Schema)
			}
		case "json_object":
			setOutputFormat(outputConfig, json.RawMessage(`{"type":"object"}`))
		}
	}
	if raw, ok := req.ExtraBody["response_format"]; ok {
		if v, err := wire.ParseValue(raw); err == nil {
			if schema, ok := responseFormatSchema(v); ok {
				setOutputFormat(outputConfig, schema)
			}
		}
	}
	forbids := ForbidsSampling(model)
	var temperature json.RawMessage
	if thinking.Temperature && !forbids {
		temperature = wire.Int(1)
	}
	if req.Temperature != nil && !req.OmitTemperature && !forbids {
		temperature = wire.Float(*req.Temperature)
	}
	betaBase := NormalizeBaseURL(o.BaseURL)
	if o.BufferToolInput && sendTools {
		for _, beta := range commonBetas(o.BaseURL, false) {
			if beta == toolStreamingBeta {
				for _, tool := range tools {
					tool.SetBool("eager_input_streaming", false)
				}
			}
		}
	}

	body := (&wire.Dict{}).SetInt("max_tokens", maxTokens).Set("messages", turnsJSON(turns))
	if !o.InvokeModel {
		body.SetString("model", model)
	}
	if outputConfig.Len() > 0 {
		body.Set("output_config", outputConfig.JSON())
	}
	if req.Stream && !o.InvokeModel {
		body.SetBool("stream", true)
	}
	if system.truthy() {
		body.Set("system", system.json())
	}
	if temperature != nil {
		body.Set("temperature", temperature)
	}
	if thinking.Thinking != nil {
		body.Set("thinking", thinking.Thinking)
	}
	if sendTools {
		if toolChoice != nil {
			body.Set("tool_choice", toolChoice)
		}
		body.Set("tools", blocksJSON(tools))
	}
	if o.InvokeModel {
		body.SetString("anthropic_version", bedrockVersion)
	}
	fast := o.FastMode && !IsThirdParty(o.BaseURL) && SupportsFastMode(model)
	if fast {
		body.SetString("speed", "fast")
	}
	extraKeys := make([]string, 0, len(req.ExtraBody))
	for key := range req.ExtraBody {
		if key != "reasoning" && key != "response_format" && !strings.HasPrefix(key, "_") {
			extraKeys = append(extraKeys, key)
		}
	}
	sort.Strings(extraKeys)
	for _, key := range extraKeys {
		body.Set(key, req.ExtraBody[key])
	}
	text, ok := wire.FormatJSON(body.JSON(), wire.TextLayout{UTF8: true})
	if !ok {
		text = string(body.JSON())
	}

	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	betas := commonBetas(betaBase, o.DropContext1MBeta)
	switch {
	case o.InvokeModel:
		betas = []string{interleavedThinkingBeta, toolStreamingBeta, context1MBeta}
		if g := req.Bedrock.Guardrail; g != nil && g.ID != "" && g.Version != "" {
			h.Set("X-Amzn-Bedrock-GuardrailIdentifier", g.ID)
			h.Set("X-Amzn-Bedrock-GuardrailVersion", g.Version)
			switch strings.ToLower(g.Trace) {
			case "enabled", "enabled_full", "true":
				h.Set("X-Amzn-Bedrock-Trace", "ENABLED")
			}
		}
	case o.OAuth:
		h.Set("anthropic-version", apiVersion)
		betas = append(betas, oauthBetas...)
		h.Set("User-Agent", "claude-code/"+p.ClaudeCodeVersion()+" (external, cli)")
		h.Set("x-app", "cli")
	default:
		h.Set("anthropic-version", apiVersion)
		if isKimiCoding(o.BaseURL) {
			p.attribution(h, true)
		}
	}
	if len(betas) > 0 {
		h.Set("anthropic-beta", strings.Join(betas, ","))
	}
	if isOpenCode(o.BaseURL) {
		p.attribution(h, false)
	}
	headerKeys := make([]string, 0, len(req.ExtraHeaders))
	for key := range req.ExtraHeaders {
		headerKeys = append(headerKeys, key)
	}
	sort.Strings(headerKeys)
	for _, key := range headerKeys {
		h.Set(key, req.ExtraHeaders[key])
	}
	if fast {
		fastBetas := commonBetas(o.BaseURL, o.DropContext1MBeta)
		if o.OAuth {
			fastBetas = append(fastBetas, oauthBetas...)
		}
		h.Set("anthropic-beta", strings.Join(append(fastBetas, fastModeBeta), ","))
	}

	path := "/v1/messages"
	switch {
	case o.InvokeModel && req.Stream:
		path = "/model/" + quoteModel(model) + "/invoke-with-response-stream"
	case o.InvokeModel:
		path = "/model/" + quoteModel(model) + "/invoke"
	case IsAzureEndpoint(betaBase) && !strings.Contains(betaBase, "api-version"):
		path += "?api-version=" + azureAPIVersion
	}
	dec := &decoder{model: model, oauth: o.OAuth, invoke: o.InvokeModel, offered: offered}
	return &wire.Call{Method: http.MethodPost, Path: path, Header: h, Body: wire.ScrubSurrogates([]byte(text)), Stream: req.Stream, Decode: dec}, nil
}

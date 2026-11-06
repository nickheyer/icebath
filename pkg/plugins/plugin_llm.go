package plugins

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyjsonschema"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Text block of a structured input list
type PluginLlmTextInput struct {
	Text string `json:"text"`
	Type string `json:"type"`
}

// Image block given as raw bytes or an http or data URL
type PluginLlmImageInput struct {
	Data     []byte `json:"data"`
	URL      string `json:"url"`
	MimeType string `json:"mime_type"`
	FileName string `json:"file_name"`
	Type     string `json:"type"`
}

// Token and cost usage of one completion, cost the host estimate when known
type PluginLlmUsage struct {
	InputTokens      int      `json:"input_tokens"`
	OutputTokens     int      `json:"output_tokens"`
	TotalTokens      int      `json:"total_tokens"`
	CacheReadTokens  int      `json:"cache_read_tokens"`
	CacheWriteTokens int      `json:"cache_write_tokens"`
	CostUSD          *float64 `json:"cost_usd"`
}

// Result of a plain completion
type PluginLlmCompleteResult struct {
	Text     string         `json:"text"`
	Provider string         `json:"provider"`
	Model    string         `json:"model"`
	AgentID  string         `json:"agent_id"`
	Usage    PluginLlmUsage `json:"usage"`
	Audit    map[string]any `json:"audit"`
}

// Result of a structured completion, parsed only when JSON was requested and returned
type PluginLlmStructuredResult struct {
	Text        string         `json:"text"`
	Provider    string         `json:"provider"`
	Model       string         `json:"model"`
	AgentID     string         `json:"agent_id"`
	Usage       PluginLlmUsage `json:"usage"`
	Parsed      any            `json:"parsed"`
	ContentType string         `json:"content_type"`
	Audit       map[string]any `json:"audit"`
}

// Resolved trust gate of one plugin, nil allowlists meaning no allowlist
type trustPolicy struct {
	pluginID              string
	allowProviderOverride bool
	allowedProviders      map[string]bool
	allowAnyProvider      bool
	allowModelOverride    bool
	allowedModels         map[string]bool
	allowAnyModel         bool
	allowAgentIDOverride  bool
	allowProfileOverride  bool
	allowTaskOverride     bool
}

var overrideFlags = []string{"allow_provider_override", "allow_model_override", "allow_agent_id_override", "allow_profile_override", "allow_task_override"}

// Allowlist and star flag of a YAML list, no allowlist for anything else
func coerceAllowlist(raw any) (map[string]bool, bool) {
	list, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	set, star := map[string]bool{}, false
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			continue
		}
		s = strings.ToLower(pyStrip(s))
		if s == "*" {
			star = true
		} else if s != "" {
			set[s] = true
		}
	}
	return set, star
}

// Trust policy read from plugins.entries.ID.llm per call, fully restrictive when missing
func resolveTrustPolicy(ctx context.Context, pluginID string) trustPolicy {
	if pluginID == "" {
		return trustPolicy{}
	}
	var node any
	if cfg, err := config.LoadReadonly(ctx, home.Dir(ctx), nil); err == nil {
		node = map[string]any(cfg)["plugins"]
	}
	for _, key := range []string{"entries", pluginID, "llm"} {
		m, ok := node.(map[string]any)
		if !ok {
			node = nil
			break
		}
		node = m[key]
	}
	llm, ok := node.(map[string]any)
	if !ok {
		return trustPolicy{pluginID: pluginID}
	}
	p := trustPolicy{pluginID: pluginID}
	p.allowedModels, p.allowAnyModel = coerceAllowlist(llm["allowed_models"])
	p.allowedProviders, p.allowAnyProvider = coerceAllowlist(llm["allowed_providers"])
	flags := map[string]*bool{"allow_provider_override": &p.allowProviderOverride, "allow_model_override": &p.allowModelOverride,
		"allow_agent_id_override": &p.allowAgentIDOverride, "allow_profile_override": &p.allowProfileOverride, "allow_task_override": &p.allowTaskOverride}
	for _, name := range overrideFlags {
		*flags[name] = pyrepr.Truthy(llm[name])
	}
	return p
}

// Refusal of an LLM override the plugin is not trusted for, a PermissionError in Python
type PluginLlmTrustError struct{ Message string }

func (e *PluginLlmTrustError) Error() string { return e.Message }

func trustDenied(pluginID, what, flag string) *PluginLlmTrustError {
	return &PluginLlmTrustError{Message: fmt.Sprintf("Plugin %s cannot %s (set plugins.entries.%s.llm.%s to true to allow).", pyrepr.Repr(pluginID), what, pluginID, flag)}
}

// Provider or model override gated by its trust flag then its allowlist
func gateRefOverride(p trustPolicy, kind, requested string) (string, error) {
	allow, allowed, anyOK := p.allowProviderOverride, p.allowedProviders, p.allowAnyProvider
	if kind == "model" {
		allow, allowed, anyOK = p.allowModelOverride, p.allowedModels, p.allowAnyModel
	}
	if !allow {
		return "", trustDenied(p.pluginID, "override the "+kind, "allow_"+kind+"_override")
	}
	if !anyOK && allowed != nil && !allowed[strings.ToLower(pyStrip(requested))] {
		return "", &PluginLlmTrustError{Message: fmt.Sprintf("Plugin %s %s override %s is not in plugins.entries.%s.llm.allowed_%ss.",
			pyrepr.Repr(p.pluginID), kind, pyrepr.Repr(requested), p.pluginID, kind)}
	}
	return pyStrip(requested), nil
}

var flagOnlyOverrides = map[string]string{"agent_id": "run completions against a non-default agent id", "profile": "override the auth profile"}

// Each override gated in the order provider, model, agent id, profile, agent id left unstripped
func checkOverrides(p trustPolicy, provider, model, agentID, profile *string) (*string, *string, *string, *string, error) {
	var outProvider, outModel, outProfile *string
	for _, ref := range []struct {
		kind string
		in   *string
		out  **string
	}{{"provider", provider, &outProvider}, {"model", model, &outModel}} {
		if ref.in != nil && *ref.in != "" {
			v, err := gateRefOverride(p, ref.kind, *ref.in)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			*ref.out = &v
		}
	}
	if agentID != nil && *agentID != "" && !p.allowAgentIDOverride {
		return nil, nil, nil, nil, trustDenied(p.pluginID, flagOnlyOverrides["agent_id"], "allow_agent_id_override")
	}
	if profile != nil && *profile != "" {
		if !p.allowProfileOverride {
			return nil, nil, nil, nil, trustDenied(p.pluginID, flagOnlyOverrides["profile"], "allow_profile_override")
		}
		v := pyStrip(*profile)
		outProfile = &v
	}
	return outProvider, outModel, agentID, outProfile, nil
}

// Task keys the plugin registered and the built in auxiliary task keys, empty when unreadable
func resolveTaskOwnership(ctx context.Context, pluginID string) (map[string]bool, map[string]bool) {
	owned, builtin := map[string]bool{}, map[string]bool{}
	func() {
		defer func() { _ = recover() }()
		for _, e := range GetPluginAuxiliaryTasks(ctx) {
			if key, ok := e["key"].(string); ok && key != "" && e["plugin"] == pluginID {
				owned[key] = true
			}
		}
	}()
	if s := installedServices(); s != nil && s.BuiltinAuxTaskKeys != nil {
		func() {
			defer func() { _ = recover() }()
			for _, k := range s.BuiltinAuxTaskKeys() {
				builtin[k] = true
			}
		}()
	}
	return owned, builtin
}

// Requested auxiliary task validated, nil for the main model path, never downgraded to auto
func checkTask(ctx context.Context, p trustPolicy, pluginID string, requested *string) (*string, error) {
	task := ""
	if requested != nil {
		task = pyStrip(*requested)
	}
	if task == "" || strings.ToLower(task) == "auto" {
		return nil, nil
	}
	owned, builtin := resolveTaskOwnership(ctx, pluginID)
	if owned[task] || (builtin[task] && p.allowTaskOverride) {
		return &task, nil
	}
	if builtin[task] {
		logWarn(fmt.Sprintf("plugin_llm task routing denied: plugin %s requested built-in auxiliary task %s without plugins.entries.%s.llm.allow_task_override",
			pyrepr.Repr(pluginID), pyrepr.Repr(task), pluginID))
		return nil, trustDenied(pluginID, "route through the built-in auxiliary task "+pyrepr.Repr(task), "allow_task_override")
	}
	logWarn(fmt.Sprintf("plugin_llm task routing denied: plugin %s requested auxiliary task %s it did not register", pyrepr.Repr(pluginID), pyrepr.Repr(task)))
	return nil, &PluginLlmTrustError{Message: fmt.Sprintf("Plugin %s cannot route through auxiliary task %s — a plugin may only pass a task key it registered itself via "+
		"ctx.register_auxiliary_task() (or a built-in key when plugins.entries.%s.llm.allow_task_override is true).", pyrepr.Repr(pluginID), pyrepr.Repr(task), pluginID)}
}

func llmValueError(msg string) error { return &HostBuiltinError{Type: "ValueError", Message: msg} }

// Structured input block coerced to a plain text or image dict
func normalizeInputBlock(block any) (map[string]any, error) {
	switch b := block.(type) {
	case PluginLlmTextInput:
		return map[string]any{"type": "text", "text": b.Text}, nil
	case *PluginLlmTextInput:
		return normalizeInputBlock(*b)
	case PluginLlmImageInput:
		d := map[string]any{"type": "image", "mime_type": b.MimeType, "file_name": b.FileName}
		if b.Data != nil {
			d["data"] = b.Data
		}
		if b.URL != "" {
			d["url"] = b.URL
		}
		return d, nil
	case *PluginLlmImageInput:
		return normalizeInputBlock(*b)
	}
	m := llmDict(block)
	if m == nil {
		return nil, llmValueError("Unsupported input block: " + wireTypeName(block))
	}
	switch kind := m["type"]; kind {
	case "text":
		text, ok := m["text"].(string)
		if !ok {
			return nil, llmValueError("text input block requires 'text' string")
		}
		return map[string]any{"type": "text", "text": text}, nil
	case "image":
		if _, has := m["data"]; !has && !pyrepr.Truthy(m["url"]) {
			return nil, llmValueError("image input block requires 'data' bytes or 'url'")
		}
		mime, name := m["mime_type"], m["file_name"]
		if !pyrepr.Truthy(mime) {
			mime = "image/png"
		}
		if !pyrepr.Truthy(name) {
			name = ""
		}
		return map[string]any{"type": "image", "data": m["data"], "url": m["url"], "mime_type": mime, "file_name": name}, nil
	default:
		return nil, llmValueError("Unknown input block type: " + pyrepr.Repr(kind))
	}
}

func llmDict(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case Record:
		return m
	}
	return nil
}

// OpenAI image_url part of a normalized image block, bytes becoming a data URL
func imagePart(norm map[string]any) (map[string]any, error) {
	url := norm["url"]
	if !pyrepr.Truthy(url) {
		var data []byte
		switch d := norm["data"].(type) {
		case nil:
		case []byte:
			data = d
		default:
			if pyrepr.Truthy(d) {
				return nil, llmValueError("image input 'data' must be bytes")
			}
		}
		mime := norm["mime_type"]
		if !pyrepr.Truthy(mime) {
			mime = "image/png"
		}
		url = "data:" + pyrepr.Str(mime) + ";base64," + base64.StdEncoding.EncodeToString(data)
	}
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}, nil
}

// Arguments of a structured completion
type PluginLlmStructuredRequest struct {
	Instructions string
	Input        []any
	JSONSchema   any
	JSONMode     bool
	SchemaName   string
	SystemPrompt string
}

// Optional system message with the JSON directive, then the user message of instructions and input blocks
func buildStructuredMessages(spec PluginLlmStructuredRequest) ([]any, error) {
	messages := []any{}
	var sysParts []string
	if spec.SystemPrompt != "" {
		sysParts = append(sysParts, pyStrip(spec.SystemPrompt))
	}
	if spec.JSONMode || spec.JSONSchema != nil {
		sysParts = append(sysParts, "Respond with a single JSON object that matches the requested shape. Do not include prose or markdown fences.")
	}
	if len(sysParts) > 0 {
		messages = append(messages, map[string]any{"role": "system", "content": strings.Join(sysParts, "\n\n")})
	}
	header := pyStrip(spec.Instructions)
	if spec.SchemaName != "" {
		header += "\n\nSchema name: " + spec.SchemaName
	}
	if spec.JSONSchema != nil {
		schemaText, err := pyjson.Dumps(spec.JSONSchema, pyjson.EnsureASCII(false), pyjson.SortKeys(true))
		if err != nil {
			schemaText = pyrepr.Str(spec.JSONSchema)
		}
		header += "\n\nJSON schema:\n" + schemaText
	}
	parts := []any{map[string]any{"type": "text", "text": header}}
	for _, block := range spec.Input {
		norm, err := normalizeInputBlock(block)
		if err != nil {
			return nil, err
		}
		if norm["type"] == "text" {
			parts = append(parts, map[string]any{"type": "text", "text": norm["text"]})
			continue
		}
		part, err := imagePart(norm)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	return append(messages, map[string]any{"role": "user", "content": parts}), nil
}

var fenceRe = regexp.MustCompile("(?is)```(?:json)?\\s*(.+?)```")

// Body of the first fenced code block, or the stripped text when unfenced
func stripCodeFences(text string) string {
	if m := fenceRe.FindStringSubmatch(text); m != nil {
		return pyStrip(m[1])
	}
	return pyStrip(text)
}

// Parsed JSON and json content type when parsing and schema validation pass, text otherwise
func parseStructuredText(text string, jsonMode bool, schema any) (any, string, error) {
	if !(jsonMode || schema != nil) || text == "" {
		return nil, "text", nil
	}
	parsed, err := pyjson.Loads(stripCodeFences(text))
	if err != nil {
		return nil, "text", nil
	}
	if schema != nil {
		var ve *pyjsonschema.ValidationError
		if err := pyjsonschema.Validate(parsed, schema); errors.As(err, &ve) {
			return nil, "", llmValueError("Plugin LLM structured output did not match schema: " + ve.Message)
		} else if err != nil {
			return nil, "", err
		}
	}
	return pyjson.Plain(parsed), "json", nil
}

// Attribute of an OpenAI shaped response object or dict, nil when absent
func responseField(v any, name string) any {
	switch m := v.(type) {
	case map[string]any:
		return m[name]
	case Record:
		return m[name]
	case *pyjson.Object:
		return m.Vals[name]
	}
	if fields, ok := recordFields(reflect.ValueOf(v)); ok {
		return fields[name]
	}
	return nil
}

// Token usage of a response tolerating the provider naming variants
func extractUsage(response any) PluginLlmUsage {
	raw := responseField(response, "usage")
	if raw == nil {
		return PluginLlmUsage{}
	}
	g := func(names ...string) int {
		for _, name := range names {
			if n, ok := pyrepr.Int(responseField(raw, name)); ok && n.Sign() != 0 && n.IsInt64() {
				return int(n.Int64())
			}
			if f, ok := responseField(raw, name).(float64); ok && int(f) != 0 {
				return int(f)
			}
			if s, ok := responseField(raw, name).(string); ok {
				if n, ok := pyrepr.Int(pyjson.ParseInt(pyStrip(s))); ok && n.Sign() != 0 && n.IsInt64() {
					return int(n.Int64())
				}
			}
		}
		return 0
	}
	in, out := g("prompt_tokens", "input_tokens"), g("completion_tokens", "output_tokens")
	total := g("total_tokens")
	if total == 0 {
		total = in + out
	}
	return PluginLlmUsage{InputTokens: in, OutputTokens: out, TotalTokens: total,
		CacheReadTokens: g("cache_read_input_tokens", "cache_read_tokens"), CacheWriteTokens: g("cache_creation_input_tokens", "cache_write_tokens")}
}

// Assistant text of a response, string content or the joined text parts
func extractText(response any) string {
	choices, ok := responseField(response, "choices").([]any)
	if !ok || len(choices) == 0 {
		return ""
	}
	switch content := responseField(responseField(choices[0], "message"), "content").(type) {
	case string:
		return content
	case []any:
		var b strings.Builder
		for _, part := range content {
			var text any
			if m := llmDict(part); m != nil {
				if m["type"] == "text" {
					text = m["text"]
				}
			} else {
				text = responseField(part, "text")
			}
			if s, ok := text.(string); ok {
				b.WriteString(s)
			}
		}
		return b.String()
	}
	return ""
}

// Current main provider or model through the auxiliary client, default when unset or unreadable
func mainConfigValue(reader, def string) (out string) {
	out = def
	s := installedServices()
	if s == nil || s.PluginLLM == nil {
		return def
	}
	defer func() {
		if recover() != nil {
			out = def
		}
	}()
	v := s.PluginLLM.ReadMainProvider()
	if reader == "_read_main_model" {
		v = s.PluginLLM.ReadMainModel()
	}
	if v = pyStrip(v); v != "" {
		return v
	}
	return def
}

// Provider and model recorded on a result, the route that ran winning over overrides and main config
func resolveAttribution(providerOverride, modelOverride *string, response any, routeInfo map[string]string) (string, string) {
	provider := routeInfo["provider"]
	if provider == "" && providerOverride != nil {
		provider = *providerOverride
	}
	if provider == "" {
		provider = mainConfigValue("_read_main_provider", "auto")
	}
	if model, ok := responseField(response, "model").(string); ok && pyStrip(model) != "" {
		return provider, pyStrip(model)
	}
	if model := routeInfo["model"]; model != "" {
		return provider, model
	}
	if modelOverride != nil && *modelOverride != "" {
		return provider, *modelOverride
	}
	return provider, mainConfigValue("_read_main_model", "default")
}

// extra_body response_format, json_object without a schema so schema blind providers get a hint
func jsonResponseFormat(jsonMode bool, schema any) map[string]any {
	if schema != nil {
		return map[string]any{"response_format": map[string]any{"type": "json_schema",
			"json_schema": map[string]any{"name": "plugin_structured_output", "schema": schema, "strict": false}}}
	}
	if jsonMode {
		return map[string]any{"response_format": map[string]any{"type": "json_object"}}
	}
	return nil
}

// Argument check of the structured methods that runs before the trust gate
func structuredSpec(name string, spec PluginLlmStructuredRequest) error {
	if pyStrip(spec.Instructions) == "" {
		return llmValueError(name + " requires non-empty instructions")
	}
	if len(spec.Input) == 0 {
		return llmValueError(name + " requires at least one input block")
	}
	return nil
}

// Keyword options of the completion methods, nil pointers and empty strings meaning None
type PluginLlmOptions struct {
	Provider, Model, AgentID, Profile, Purpose, Task *string
	Temperature                                      *float64
	MaxTokens                                        *int
	Timeout                                          *float64
}

// Call kwargs after the trust gate, what an injected caller receives
type pluginLlmCall struct {
	Messages         []any
	ProviderOverride *string
	ModelOverride    *string
	ProfileOverride  *string
	Temperature      *float64
	MaxTokens        *int
	Timeout          *float64
	ExtraBody        map[string]any
	Task             *string
}

// Injected replacement of the whole host call path returning provider, model and response
type pluginLlmCaller func(ctx context.Context, kw pluginLlmCall) (provider, model string, response any, err error)

// Keyword arguments of agent.auxiliary_client call_llm and async_call_llm for one plugin completion
type AuxLLMRequest struct {
	Task        *string
	Provider    *string
	Model       *string
	Messages    []any
	Temperature *float64
	MaxTokens   *int
	Timeout     *float64
	ExtraBody   map[string]any
	RouteInfo   map[string]string
}

// Auxiliary client the wiring installs, RouteInfo filled by the callee with the provider and model that ran
type PluginLLMClient interface {
	CallLLM(ctx context.Context, req *AuxLLMRequest) (any, error)
	AsyncCallLLM(ctx context.Context, req *AuxLLMRequest) (any, error)
	ReadMainProvider() string
	ReadMainModel() string
}

// Host owned LLM access of one trusted plugin, identity bound at construction for the trust gate
type PluginLlm struct {
	pluginID     string
	policyLoader func(ctx context.Context, pluginID string) trustPolicy
	syncCaller   pluginLlmCaller
	asyncCaller  pluginLlmCaller
}

func newPluginLlm(pluginID string) *PluginLlm {
	return &PluginLlm{pluginID: pluginID, policyLoader: resolveTrustPolicy}
}

// Facade with an injected policy and caller, no config and no provider
func makePluginLlmForTest(pluginID string, policy trustPolicy, sync, async pluginLlmCaller) *PluginLlm {
	return &PluginLlm{pluginID: pluginID, policyLoader: func(context.Context, string) trustPolicy { return policy }, syncCaller: sync, asyncCaller: async}
}

// Host chat completion against the active model, overrides gated per plugin
func (l *PluginLlm) Complete(ctx context.Context, messages []any, o PluginLlmOptions) (*PluginLlmCompleteResult, error) {
	agent, kw, err := l.gate(ctx, o, messages, nil)
	if err != nil {
		return nil, err
	}
	result, err := l.finishInvoked(ctx, "complete", agent, kw, o.Purpose, nil, false)
	if err != nil {
		return nil, err
	}
	return result.(*PluginLlmCompleteResult), nil
}

// Bounded structured completion with text and image input, JSON parsed and validated when requested
func (l *PluginLlm) CompleteStructured(ctx context.Context, spec PluginLlmStructuredRequest, o PluginLlmOptions) (*PluginLlmStructuredResult, error) {
	return l.structured(ctx, "complete_structured", spec, o, false)
}

// Async sibling of the plain completion
func (l *PluginLlm) AComplete(ctx context.Context, messages []any, o PluginLlmOptions) (*PluginLlmCompleteResult, error) {
	agent, kw, err := l.gate(ctx, o, messages, nil)
	if err != nil {
		return nil, err
	}
	result, err := l.finishInvoked(ctx, "acomplete", agent, kw, o.Purpose, nil, true)
	if err != nil {
		return nil, err
	}
	return result.(*PluginLlmCompleteResult), nil
}

// Async sibling of the structured completion
func (l *PluginLlm) ACompleteStructured(ctx context.Context, spec PluginLlmStructuredRequest, o PluginLlmOptions) (*PluginLlmStructuredResult, error) {
	return l.structured(ctx, "acomplete_structured", spec, o, true)
}

func (l *PluginLlm) structured(ctx context.Context, name string, spec PluginLlmStructuredRequest, o PluginLlmOptions, async bool) (*PluginLlmStructuredResult, error) {
	if err := structuredSpec(name, spec); err != nil {
		return nil, err
	}
	agent, kw, err := l.gate(ctx, o, nil, &spec)
	if err != nil {
		return nil, err
	}
	result, err := l.finishInvoked(ctx, name, agent, kw, o.Purpose, &spec, async)
	if err != nil {
		return nil, err
	}
	return result.(*PluginLlmStructuredResult), nil
}

// Task gate, override gate, then the structured messages and response format once trust passes
func (l *PluginLlm) gate(ctx context.Context, o PluginLlmOptions, messages []any, spec *PluginLlmStructuredRequest) (*string, pluginLlmCall, error) {
	policy := l.policyLoader(ctx, l.pluginID)
	task, err := checkTask(ctx, policy, l.pluginID, o.Task)
	if err != nil {
		return nil, pluginLlmCall{}, err
	}
	provider, model, agent, profile, err := checkOverrides(policy, o.Provider, o.Model, o.AgentID, o.Profile)
	if err != nil {
		return nil, pluginLlmCall{}, err
	}
	var extra map[string]any
	if spec != nil {
		if messages, err = buildStructuredMessages(*spec); err != nil {
			return nil, pluginLlmCall{}, err
		}
		extra = jsonResponseFormat(spec.JSONMode, spec.JSONSchema)
	}
	return agent, pluginLlmCall{Messages: messages, ProviderOverride: provider, ModelOverride: model, ProfileOverride: profile,
		Temperature: o.Temperature, MaxTokens: o.MaxTokens, Timeout: o.Timeout, ExtraBody: extra, Task: task}, nil
}

func (l *PluginLlm) finishInvoked(ctx context.Context, name string, agent *string, kw pluginLlmCall, purpose *string, spec *PluginLlmStructuredRequest, async bool) (any, error) {
	provider, model, response, err := l.invoke(ctx, kw, async)
	if err != nil {
		return nil, err
	}
	return l.finish(name, agent, kw, provider, model, response, purpose, spec)
}

func strOr(p *string, def string) string {
	if p == nil || *p == "" {
		return def
	}
	return *p
}

// Result object and audit dict of a call with the INFO audit line
func (l *PluginLlm) finish(name string, agent *string, kw pluginLlmCall, provider, model string, response any, purpose *string, spec *PluginLlmStructuredRequest) (any, error) {
	text, usage := extractText(response), extractUsage(response)
	task := strOr(kw.Task, "")
	audit := map[string]any{"plugin_id": l.pluginID, "purpose": strOr(purpose, ""), "profile": strOr(kw.ProfileOverride, "")}
	format := "plugin_llm.%s plugin=%s provider=%s model=%s task=%s purpose=%s "
	args := []any{name, l.pluginID, provider, model, task, strOr(purpose, "")}
	if spec == nil {
		audit["task"] = task
		logInfo(fmt.Sprintf(format+"tokens=%d", append(args, usage.TotalTokens)...))
		return &PluginLlmCompleteResult{Text: text, Provider: provider, Model: model, AgentID: strOr(agent, "default"), Usage: usage, Audit: audit}, nil
	}
	parsed, contentType, err := parseStructuredText(text, spec.JSONMode, spec.JSONSchema)
	if err != nil {
		return nil, err
	}
	audit["schema_name"] = spec.SchemaName
	audit["task"] = task
	logInfo(fmt.Sprintf(format+"content_type=%s tokens=%d", append(args, contentType, usage.TotalTokens)...))
	return &PluginLlmStructuredResult{Text: text, Provider: provider, Model: model, AgentID: strOr(agent, "default"), Usage: usage,
		Parsed: parsed, ContentType: contentType, Audit: audit}, nil
}

// call_llm kwargs of a gated call, the auth profile riding in extra_body metadata and route info only for a task
func hostKwargs(kw pluginLlmCall) *AuxLLMRequest {
	merged := map[string]any{}
	for k, v := range kw.ExtraBody {
		merged[k] = v
	}
	if kw.ProfileOverride != nil && *kw.ProfileOverride != "" {
		meta, ok := merged["metadata"].(map[string]any)
		if !ok {
			meta = map[string]any{}
			merged["metadata"] = meta
		}
		meta["auth_profile"] = *kw.ProfileOverride
	}
	req := &AuxLLMRequest{Task: kw.Task, Provider: kw.ProviderOverride, Model: kw.ModelOverride, Messages: kw.Messages,
		Temperature: kw.Temperature, MaxTokens: kw.MaxTokens, Timeout: kw.Timeout}
	if len(merged) > 0 {
		req.ExtraBody = merged
	}
	if kw.Task != nil && *kw.Task != "" {
		req.RouteInfo = map[string]string{}
	}
	return req
}

// Injected caller, or the host call_llm or async_call_llm with attribution of what ran
func (l *PluginLlm) invoke(ctx context.Context, kw pluginLlmCall, async bool) (string, string, any, error) {
	if caller := map[bool]pluginLlmCaller{false: l.syncCaller, true: l.asyncCaller}[async]; caller != nil {
		return caller(ctx, kw)
	}
	client, err := needService("PluginLLM", func(s *Services) PluginLLMClient { return s.PluginLLM })
	if err != nil {
		return "", "", nil, err
	}
	req := hostKwargs(kw)
	call := client.CallLLM
	if async {
		call = client.AsyncCallLLM
	}
	response, err := call(ctx, req)
	if err != nil {
		return "", "", nil, err
	}
	provider, model := resolveAttribution(kw.ProviderOverride, kw.ModelOverride, response, req.RouteInfo)
	return provider, model, response, nil
}

var pluginLlmMethods = map[string]bool{"complete": false, "complete_structured": false, "acomplete": true, "acomplete_structured": true}

// The four completion methods for a hosted plugin, the a variants as coroutine methods
func (l *PluginLlm) HostAttr(_ context.Context, name string) (any, bool, bool, error) {
	if async, ok := pluginLlmMethods[name]; ok {
		return nil, true, async, nil
	}
	return nil, false, false, &HostBuiltinError{Type: "AttributeError", Message: fmt.Sprintf("'PluginLlm' object has no attribute %s", pyrepr.Repr(name))}
}

var pluginLlmOptionParams = []pyParam{kwo("provider", nil), kwo("model", nil), kwo("temperature", nil), kwo("max_tokens", nil),
	kwo("timeout", nil), kwo("agent_id", nil), kwo("profile", nil), kwo("purpose", nil), kwo("task", nil)}

// Runs a completion method for a hosted plugin with the Python keyword binding
func (l *PluginLlm) HostCall(ctx context.Context, method string, args []any, kwargs map[string]any) (any, error) {
	async, ok := pluginLlmMethods[method]
	if !ok {
		return nil, &HostBuiltinError{Type: "AttributeError", Message: fmt.Sprintf("'PluginLlm' object has no attribute %s", pyrepr.Repr(method))}
	}
	structured := strings.HasSuffix(method, "_structured")
	params := append([]pyParam{req("messages")}, pluginLlmOptionParams...)
	if structured {
		params = append([]pyParam{kwReq("instructions"), kwReq("input"), kwo("json_schema", nil), kwo("json_mode", false),
			kwo("schema_name", nil), kwo("system_prompt", nil)}, pluginLlmOptionParams...)
	}
	a, _, err := bindPyOn("PluginLlm", method, params, args, kwargs)
	if err != nil {
		return nil, err
	}
	o, err := hostLlmOptions(a)
	if err != nil {
		return nil, err
	}
	if !structured {
		messages, _ := a["messages"].([]any)
		if async {
			return l.AComplete(ctx, messages, o)
		}
		return l.Complete(ctx, messages, o)
	}
	spec := PluginLlmStructuredRequest{JSONSchema: a["json_schema"], JSONMode: pyrepr.Truthy(a["json_mode"])}
	if pyrepr.Truthy(a["schema_name"]) {
		spec.SchemaName = pyrepr.Str(a["schema_name"])
	}
	for name, dst := range map[string]*string{"instructions": &spec.Instructions, "system_prompt": &spec.SystemPrompt} {
		v, err := hostOptStr(a[name])
		if err != nil {
			return nil, err
		}
		*dst = strOr(v, "")
	}
	if spec.Input, err = hostInputList(a["input"]); err != nil {
		return nil, err
	}
	if async {
		return l.ACompleteStructured(ctx, spec, o)
	}
	return l.CompleteStructured(ctx, spec, o)
}

// Optional string argument, a non string raising the AttributeError strip would raise
func hostOptStr(v any) (*string, error) {
	switch s := v.(type) {
	case nil:
		return nil, nil
	case string:
		return &s, nil
	}
	if !pyrepr.Truthy(v) {
		return nil, nil
	}
	return nil, &HostBuiltinError{Type: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'strip'", pyTypeName(v))}
}

// Input blocks of a hosted call, records and dicts both reaching the dict normalization
func hostInputList(v any) ([]any, error) {
	switch l := v.(type) {
	case nil:
		return nil, nil
	case []any:
		return l, nil
	}
	return nil, &HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf("'%s' object is not iterable", pyTypeName(v))}
}

// Keyword options of a hosted call converted from wire values
func hostLlmOptions(a map[string]any) (PluginLlmOptions, error) {
	var o PluginLlmOptions
	for name, dst := range map[string]**string{"agent_id": &o.AgentID, "purpose": &o.Purpose} {
		if pyrepr.Truthy(a[name]) {
			v := pyrepr.Str(a[name])
			*dst = &v
		}
	}
	for name, dst := range map[string]**string{"provider": &o.Provider, "model": &o.Model, "profile": &o.Profile, "task": &o.Task} {
		v, err := hostOptStr(a[name])
		if err != nil {
			return o, err
		}
		*dst = v
	}
	for name, dst := range map[string]**float64{"temperature": &o.Temperature, "timeout": &o.Timeout} {
		if a[name] != nil {
			f, ok := pyNumber(a[name])
			if !ok {
				return o, &HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf("%s must be a number, not %s", name, pyTypeName(a[name]))}
			}
			*dst = &f
		}
	}
	if a["max_tokens"] != nil {
		n, ok := pyrepr.Int(a["max_tokens"])
		if !ok || !n.IsInt64() {
			return o, &HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf("max_tokens must be an integer, not %s", pyTypeName(a["max_tokens"]))}
		}
		v := int(n.Int64())
		o.MaxTokens = &v
	}
	return o, nil
}

func pyNumber(v any) (float64, bool) {
	if f, ok := v.(float64); ok {
		return f, true
	}
	if n, ok := pyrepr.Int(v); ok {
		f, _ := new(big.Float).SetInt(n).Float64()
		return f, true
	}
	return 0, false
}

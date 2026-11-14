// Bedrock Converse wire protocol
package bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/auth/aws"
	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/model/wire/anthropic"
)

const (
	PlacementTools    = "tools"
	PlacementSystem   = "system"
	PlacementMessages = "messages"
)

var (
	claudeModelRE        = regexp.MustCompile(`(?i)^(?:(?:global|us|eu|apac|ap|au|jp|ca|sa|me|af)\.)?anthropic\.claude`)
	grokNoSamplingRE     = regexp.MustCompile(`(?i)^(?:[a-z]+\.)?xai\.grok`)
	cachePointPathRE     = regexp.MustCompile(`(?i)#/([A-Za-z0-9_./\[\]-]*)`)
	redactedRejectionRE  = regexp.MustCompile(`(?is)ValidationException.*(?:redacted|encrypt)`)
	nonToolCallingModels = []string{"deepseek.r1", "deepseek-r1", "stability.", "cohere.embed", "amazon.titan-embed"}
	cachePointModels     = []string{"anthropic.claude", "amazon.nova"}
	novaEfforts          = map[string]string{"minimal": "low", "low": "low", "medium": "medium"}
	cachePoint           = json.RawMessage(`{"cachePoint":{"type":"default"}}`)
)

type Protocol struct {
	mu       sync.Mutex
	rejected map[string]map[string]bool
}

var _ wire.Protocol = (*Protocol)(nil)

func IsClaudeModel(model string) bool { return claudeModelRE.MatchString(model) }

func SupportsToolUse(model string) bool {
	return !containsAny(strings.ToLower(model), nonToolCallingModels)
}

func SupportsPromptCache(model string, resolve func(arn string) string) bool {
	c := &converter{resolve: resolve}
	return containsAny(strings.ToLower(c.resolved(model)), cachePointModels)
}

func Guardrail(cfg config.BedrockGuardrail) *wire.Guardrail {
	if cfg.GuardrailIdentifier == "" || cfg.GuardrailVersion == "" {
		return nil
	}
	return &wire.Guardrail{ID: cfg.GuardrailIdentifier, Version: cfg.GuardrailVersion, Trace: string(cfg.Trace), StreamProcessing: string(cfg.StreamProcessingMode)}
}

func (p *Protocol) cachePointAllowed(model, placement string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.rejected[strings.ToLower(model)][placement]
}

func (p *Protocol) noteRejection(model, placement string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rejected == nil {
		p.rejected = map[string]map[string]bool{}
	}
	key := strings.ToLower(model)
	if p.rejected[key] == nil {
		p.rejected[key] = map[string]bool{}
	}
	p.rejected[key][placement] = true
}

func pathEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte("0123456789ABCDEF"[c>>4])
			b.WriteByte("0123456789ABCDEF"[c&15])
		}
	}
	return b.String()
}

func convertTools(tools []wire.Tool) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(tools))
	for i := range tools {
		schema := json.RawMessage(`{"type":"object","properties":{}}`)
		if len(tools[i].Parameters) > 0 {
			schema = tools[i].Parameters
		}
		spec := (&wire.Dict{}).SetString("name", tools[i].Name).SetString("description", tools[i].Description).
			Set("inputSchema", (&wire.Dict{}).Set("json", schema).JSON())
		out = append(out, (&wire.Dict{}).Set("toolSpec", spec.JSON()).JSON())
	}
	return out
}

func (p *Protocol) Build(req *wire.Request) (*wire.Call, error) {
	o := req.Bedrock
	model := req.Model
	conv := &converter{model: model, resolve: o.ResolveProfile}
	system, turns := conv.convertMessages(req.Messages)
	cacheAt := map[string]bool{}
	if o.CachePoints && SupportsPromptCache(model, o.ResolveProfile) {
		for _, placement := range []string{PlacementTools, PlacementSystem, PlacementMessages} {
			cacheAt[placement] = p.cachePointAllowed(model, placement)
		}
	}
	inference := &wire.Dict{}
	hasMaxTokens := req.MaxTokens > 0
	if hasMaxTokens {
		inference.SetInt("maxTokens", req.MaxTokens)
	}
	if !anthropic.ForbidsSampling(model) && !grokNoSamplingRE.MatchString(model) {
		if req.Temperature != nil && !req.OmitTemperature {
			inference.SetFloat("temperature", *req.Temperature)
		}
		if req.TopP != nil {
			inference.SetFloat("topP", *req.TopP)
		}
	}
	if len(req.Stop) > 0 {
		inference.Set("stopSequences", wire.Strings(req.Stop))
	}
	var toolConfig json.RawMessage
	if tools := convertTools(req.Tools); len(tools) > 0 {
		if !SupportsToolUse(model) {
			slog.Warn("model does not support tool calling, tools stripped and the agent runs in text-only mode", "model", model)
		} else {
			if cacheAt[PlacementTools] {
				tools = append(tools, cachePoint)
			}
			toolConfig = (&wire.Dict{}).Set("tools", wire.Array(tools...)).JSON()
		}
	}
	if cacheAt[PlacementMessages] && len(turns) >= 2 {
		if target := &turns[len(turns)-2]; len(target.blocks) > 0 {
			if v, err := wire.ParseValue(cachePoint); err == nil {
				target.blocks = append(target.blocks, wire.DictOf(v))
			}
		}
	}
	var extra *wire.Dict
	if r := req.Reasoning; r != nil {
		switch {
		case IsClaudeModel(model):
			thinking := anthropic.ThinkingFields(r, model, req.MaxTokens)
			fields := &wire.Dict{}
			if thinking.Thinking != nil {
				fields.Set("thinking", thinking.Thinking)
			}
			if thinking.OutputConfig != nil {
				fields.Set("output_config", thinking.OutputConfig)
			}
			if thinking.Temperature {
				if !strings.Contains(strings.ToLower(model), "claude-3") {
					fields.Set("anthropic_beta", wire.Strings([]string{"interleaved-thinking-2025-05-14"}))
				}
				inference.SetInt("temperature", 1)
				if hasMaxTokens {
					inference.SetInt("maxTokens", thinking.MaxTokens)
				}
			}
			if fields.Len() > 0 {
				extra = fields
			}
		case strings.Contains(strings.ToLower(model), "amazon.nova-2-lite") && r.Enabled:
			effort, ok := novaEfforts[strings.ToLower(r.Effort)]
			if r.Effort == "" {
				effort, ok = "medium", true
			}
			if !ok {
				effort = "high"
			}
			config := (&wire.Dict{}).SetString("type", "enabled").SetString("maxReasoningEffort", effort)
			extra = (&wire.Dict{}).Set("reasoningConfig", config.JSON())
			if effort == "high" {
				inference.Delete("temperature")
				inference.Delete("topP")
				inference.Delete("maxTokens")
			}
		}
	}

	messages := make([]json.RawMessage, len(turns))
	for i := range turns {
		messages[i] = (&wire.Dict{}).SetString("role", turns[i].role).Set("content", blocksJSON(turns[i].blocks)).JSON()
	}
	body := (&wire.Dict{}).Set("messages", wire.Array(messages...))
	if inference.Len() > 0 {
		body.Set("inferenceConfig", inference.JSON())
	}
	if len(system) > 0 {
		blocks := blocksJSON(system)
		if cacheAt[PlacementSystem] {
			items := make([]json.RawMessage, 0, len(system)+1)
			for _, b := range system {
				items = append(items, b.JSON())
			}
			blocks = wire.Array(append(items, cachePoint)...)
		}
		body.Set("system", blocks)
	}
	if toolConfig != nil {
		body.Set("toolConfig", toolConfig)
	}
	if g := o.Guardrail; g != nil && g.ID != "" && g.Version != "" {
		guard := (&wire.Dict{}).SetString("guardrailIdentifier", g.ID).SetString("guardrailVersion", g.Version)
		if g.StreamProcessing != "" {
			guard.SetString("streamProcessingMode", g.StreamProcessing)
		}
		if g.Trace != "" {
			guard.SetString("trace", g.Trace)
		}
		body.Set("guardrailConfig", guard.JSON())
	}
	if extra != nil {
		body.Set("additionalModelRequestFields", extra.JSON())
	}
	path := "/model/" + pathEscape(model) + "/converse"
	if req.Stream {
		path += "-stream"
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	return &wire.Call{Method: http.MethodPost, Path: path, Header: h, Body: encodeBody(body.JSON()), Stream: req.Stream, Decode: &decoder{model: model}}, nil
}

func encodeBody(body json.RawMessage) []byte {
	text, ok := wire.FormatJSON(body, wire.TextLayout{Spaced: true})
	if !ok {
		text = string(body)
	}
	return wire.ScrubSurrogates([]byte(text))
}

func failureText(err error) string {
	text := err.Error()
	var apiErr *wire.APIError
	if errors.As(err, &apiErr) {
		text = apiErr.Code() + " " + apiErr.Message() + " " + text
	}
	return text
}

func CachePointRejection(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	text := failureText(err)
	lowered := strings.ToLower(text)
	if !strings.Contains(lowered, "cachepoint") || !(strings.Contains(lowered, "not permitted") || strings.Contains(lowered, "extraneous")) {
		return "", false
	}
	path := ""
	if m := cachePointPathRE.FindStringSubmatch(text); m != nil {
		path = strings.ToLower(m[1])
	}
	switch {
	case strings.Contains(path, "toolconfig") || strings.Contains(path, "tools"):
		return PlacementTools, true
	case strings.Contains(path, PlacementSystem):
		return PlacementSystem, true
	case strings.Contains(path, PlacementMessages):
		return PlacementMessages, true
	}
	return PlacementTools, true
}

func withoutBlocks(blocks wire.Value, drop func(wire.Value) bool) ([]json.RawMessage, bool) {
	if !blocks.IsArray() {
		return nil, false
	}
	var kept []json.RawMessage
	removed := false
	for _, b := range blocks.Elems() {
		if drop(b) {
			removed = true
			continue
		}
		kept = append(kept, b.Raw())
	}
	return kept, removed
}

func isCachePoint(b wire.Value) bool {
	fields := b.Fields()
	return b.IsObject() && len(fields) == 1 && fields[0].Key == "cachePoint"
}

func isRedactedReasoning(b wire.Value) bool {
	reasoning := b.Get("reasoningContent")
	return b.IsObject() && reasoning.IsObject() && reasoning.Get("redactedContent").Exists()
}

func rewriteMessages(body wire.Value, drop func(wire.Value) bool, dropEmpty bool) (json.RawMessage, bool) {
	messages := body.Get("messages")
	if !messages.IsArray() {
		return nil, false
	}
	changed := false
	var out []json.RawMessage
	for _, msg := range messages.Elems() {
		kept, removed := withoutBlocks(msg.Get("content"), drop)
		if !removed {
			out = append(out, msg.Raw())
			continue
		}
		changed = true
		if dropEmpty && len(kept) == 0 {
			continue
		}
		out = append(out, wire.DictOf(msg).Set("content", wire.Array(kept...)).JSON())
	}
	if !changed {
		return nil, false
	}
	return wire.DictOf(body).Set("messages", wire.Array(out...)).JSON(), true
}

func stripCachePoints(body wire.Value, placement string) (json.RawMessage, bool) {
	switch placement {
	case PlacementMessages:
		return rewriteMessages(body, isCachePoint, false)
	case PlacementSystem:
		kept, removed := withoutBlocks(body.Get("system"), isCachePoint)
		if !removed {
			return nil, false
		}
		return wire.DictOf(body).Set("system", wire.Array(kept...)).JSON(), true
	case PlacementTools:
		toolConfig := body.Get("toolConfig")
		kept, removed := withoutBlocks(toolConfig.Get("tools"), isCachePoint)
		if !removed {
			return nil, false
		}
		return wire.DictOf(body).Set("toolConfig", wire.DictOf(toolConfig).Set("tools", wire.Array(kept...)).JSON()).JSON(), true
	}
	return nil, false
}

func retryCall(call *wire.Call, body json.RawMessage) *wire.Call {
	retry := *call
	retry.Header = call.Header.Clone()
	retry.Body = encodeBody(body)
	if dec, ok := call.Decode.(*decoder); ok {
		retry.Decode = &decoder{model: dec.model}
	}
	return &retry
}

func (p *Protocol) RecoverCachePoint(err error, call *wire.Call, model string) *wire.Call {
	placement, ok := CachePointRejection(err)
	if !ok || call == nil {
		return nil
	}
	body, perr := wire.ParseValue(call.Body)
	if perr != nil {
		return nil
	}
	stripped, changed := stripCachePoints(body, placement)
	if !changed {
		return nil
	}
	p.noteRejection(model, placement)
	slog.Warn("bedrock rejected a cachePoint block, dropping that cache marker for this model and retrying", "model", model, "placement", placement)
	return retryCall(call, stripped)
}

func RecoverRedactedReasoning(err error, call *wire.Call, model string) *wire.Call {
	if err == nil || call == nil || !redactedRejectionRE.MatchString(failureText(err)) {
		return nil
	}
	body, perr := wire.ParseValue(call.Body)
	if perr != nil {
		return nil
	}
	stripped, changed := rewriteMessages(body, isRedactedReasoning, true)
	if !changed {
		return nil
	}
	slog.Warn("bedrock rejected replayed redacted reasoning, stripping redacted blocks and resending once", "model", model)
	return retryCall(call, stripped)
}

func IsStaleConnection(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var apiErr *wire.APIError
	if errors.As(err, &apiErr) {
		return false
	}
	var transportErr *httpclient.Error
	var proxyErr *httpclient.ProxyError
	var netErr net.Error
	return errors.As(err, &transportErr) || errors.As(err, &proxyErr) || errors.As(err, &netErr) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, aws.ErrFrameTruncated) || errors.Is(err, aws.ErrChecksum)
}

func IsStreamingAccessDenied(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(failureText(err))
	if !strings.Contains(text, "invokemodelwithresponsestream") {
		return false
	}
	var apiErr *wire.APIError
	if errors.As(err, &apiErr) && apiErr.Code() != "" {
		return apiErr.Code() == "AccessDeniedException" || apiErr.Code() == "UnauthorizedException"
	}
	return strings.Contains(text, "not authorized") || strings.Contains(text, "accessdenied")
}

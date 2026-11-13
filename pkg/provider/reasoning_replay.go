package provider

import (
	"fmt"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

type echoRule struct {
	family    string
	raw       map[string]bool
	lowered   map[string]bool
	modelSubs []string
	hosts     []string
}

var reasoningEchoRules = []echoRule{
	{"kimi", map[string]bool{"kimi-coding": true, "kimi-coding-cn": true}, map[string]bool{}, nil, []string{"api.kimi.com", "moonshot.ai", "moonshot.cn"}},
	{"deepseek", map[string]bool{}, map[string]bool{"deepseek": true}, []string{"deepseek"}, []string{"api.deepseek.com"}},
	{"mimo", map[string]bool{}, map[string]bool{"xiaomi": true}, []string{"mimo"}, []string{"api.xiaomimimo.com", "xiaomimimo.com"}},
	{"missingno", map[string]bool{}, map[string]bool{}, []string{"stealth/missingno"}, nil},
}

// MatchesReasoningEchoFamily reports membership in one must echo family, a KeyError text for an unknown family
func MatchesReasoningEchoFamily(family, provider, model, baseURL string) (bool, error) {
	for _, rule := range reasoningEchoRules {
		if rule.family != family {
			continue
		}
		if rule.raw[provider] || rule.lowered[pystr.Lower(provider)] {
			return true, nil
		}
		modelLower := pystr.Lower(model)
		for _, sub := range rule.modelSubs {
			if strings.Contains(modelLower, sub) {
				return true, nil
			}
		}
		return anyHostMatches(baseURL, rule.hosts), nil
	}
	return false, fmt.Errorf("KeyError: %s", pyrepr.Repr(family))
}

// ReasoningEchoFamily names the first must echo family the route matches in table order, empty for none
func ReasoningEchoFamily(provider, model, baseURL string) string {
	for _, rule := range reasoningEchoRules {
		if matched, err := MatchesReasoningEchoFamily(rule.family, provider, model, baseURL); err == nil && matched {
			return rule.family
		}
	}
	return ""
}

// NeedsReasoningEcho reports whether the route 400s on a tool call turn without reasoning_content
func NeedsReasoningEcho(provider, model, baseURL string) bool {
	return ReasoningEchoFamily(provider, model, baseURL) != ""
}

// Reasoning carriers the chat completions route reads before any runtime rejection, a fresh set per call
func routeReasoningCarriers(provider, baseURL string) map[string]bool {
	p := pystr.Lower(pystr.Strip(provider))
	var carriers map[string]bool
	for _, r := range reasoningCarrierRoutes {
		if r.providers[p] || anyHostMatches(baseURL, r.hosts) {
			carriers = carrierSet(sortedCarrierKeys(r.carriers)...)
			break
		}
	}
	if carriers == nil {
		if baseURL != "" && isLocalEndpoint(baseURL) {
			carriers = carrierSet(carrierRC, carrierR)
		} else {
			carriers = carrierSet(carrierRC)
		}
	}
	if !carriers[carrierRD] && len(carriers) > 0 && profileDeclaresNativeDetails(p) {
		carriers[carrierRD] = true
	}
	return carriers
}

func anyHostMatches(baseURL string, hosts []string) bool {
	for _, h := range hosts {
		if hostMatches(baseURL, h) {
			return true
		}
	}
	return false
}

func sortedCarrierKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for _, k := range []string{carrierRC, carrierR, carrierRD} {
		if set[k] {
			out = append(out, k)
		}
	}
	return out
}

// True when the provider profile declares a native reasoning details type and so consumes replayed details
func profileDeclaresNativeDetails(providerLower string) bool {
	if providerLower == "" {
		return false
	}
	profile, ok := GetProviderProfile(nil, providerLower)
	return ok && profile.NativeReasoningDetailsType != ""
}

// ReasoningReplayRoute is the must echo pad flag and the carriers a wire copy keeps, nil carriers outside chat completions
type ReasoningReplayRoute struct {
	Pad      bool
	Carriers map[string]bool
}

// ReasoningReplayRouteFor decides the replay shape of one route minus the carriers it rejected
func ReasoningReplayRouteFor(apiMode, provider, model, baseURL string, echoOptIn bool, rejected map[string]bool) ReasoningReplayRoute {
	pad := echoOptIn || NeedsReasoningEcho(provider, model, baseURL)
	if apiMode == "" {
		apiMode = string(ModeChatCompletions)
	}
	if apiMode != string(ModeChatCompletions) {
		return ReasoningReplayRoute{Pad: pad}
	}
	carriers := routeReasoningCarriers(provider, baseURL)
	for k := range rejected {
		delete(carriers, k)
	}
	return ReasoningReplayRoute{Pad: pad && carriers[carrierRC], Carriers: carriers}
}

// NativeAnthropicPreservesPriorThinking reports whether the route replays and the model keeps signed prior thinking
func NativeAnthropicPreservesPriorThinking(baseURL, model string) bool {
	return nativeAnthropicPreservesPriorThinking(baseURL, model)
}

// NativeAnthropicAccountingProjection returns the native Anthropic wire shadow of the messages and the replayed thinking texts
func NativeAnthropicAccountingProjection(messages any) (any, []string) {
	var list []any
	switch t := messages.(type) {
	case []any:
		list = t
	case []map[string]any:
		list = make([]any, len(t))
		for i, m := range t {
			list[i] = m
		}
	default:
		return messages, nil
	}
	projected := make([]any, 0, len(list))
	var replayed []string
	for _, message := range list {
		m, ok := message.(map[string]any)
		if !ok || m["role"] != "assistant" {
			projected = append(projected, message)
			continue
		}
		shadow := map[string]any{"role": "assistant"}
		for _, key := range []string{"content", "tool_calls", "reasoning_content", "cache_control"} {
			if v, has := m[key]; has {
				shadow[key] = v
			}
		}
		if sidecar, ok := m["api_content"].(string); ok && sidecar != "" {
			shadow["content"] = sidecar
		}
		_, carrier := assistantReplayCarrier(m, convertOptions{})
		if len(carrier) > 0 {
			delete(shadow, "reasoning_content")
		}
		for _, block := range carrier {
			b, _ := block.(map[string]any)
			if text, ok := b["thinking"].(string); ok && b["type"] == "thinking" && text != "" {
				replayed = append(replayed, text)
			}
		}
		projected = append(projected, shadow)
	}
	return projected, replayed
}

var unknownFieldPhrases = []string{
	"extra inputs are not permitted", "is unsupported", "no such field", "unknown field", "unrecognized request argument",
	"unrecognized field", "unknown parameter", "unexpected field", "additional propert", "unknown key",
}

// Python str patterns with Unicode word lookarounds, matched over code points so surrogates count once
var namedFieldRes = map[string]*regexp2.Regexp{
	carrierRC: regexp2.MustCompile(`(?<![\p{L}\p{N}_])reasoning_content(?![\p{L}\p{N}_])`, regexp2.None),
	carrierRD: regexp2.MustCompile(`(?<![\p{L}\p{N}_])reasoning_details(?![\p{L}\p{N}_])`, regexp2.None),
	carrierR:  regexp2.MustCompile(`messages[^\n]{0,80}?(?<![\p{L}\p{N}_])reasoning(?![\p{L}\p{N}_])`, regexp2.None),
}

// Assistant messages of a sent request whatever dict shape carries them
func sentAssistantCarriers(sentMessages any) map[string]bool {
	var dicts []func(string) (any, bool)
	add := func(m any) {
		switch t := m.(type) {
		case map[string]any:
			dicts = append(dicts, func(k string) (any, bool) { v, ok := t[k]; return v, ok })
		case *omap:
			if t != nil {
				dicts = append(dicts, t.Get)
			}
		}
	}
	switch t := sentMessages.(type) {
	case []any:
		for _, m := range t {
			add(m)
		}
	case []map[string]any:
		for _, m := range t {
			add(m)
		}
	case []*omap:
		for _, m := range t {
			add(m)
		}
	}
	sent := map[string]bool{}
	for _, get := range dicts {
		if role, _ := get("role"); role != "assistant" {
			continue
		}
		for _, k := range []string{carrierRC, carrierR, carrierRD} {
			if _, has := get(k); has {
				sent[k] = true
			}
		}
	}
	return sent
}

// RejectedReasoningFields lists the reasoning keys to drop after an unknown field rejection naming one the request carried
func RejectedReasoningFields(errorBody any, sentMessages any) map[string]bool {
	text := ""
	if pyTruthy(errorBody) {
		if s, ok := errorBody.(string); ok {
			text = s
		} else {
			text = pyrepr.Str(errorBody)
		}
	}
	body := pystr.Lower(text)
	out := map[string]bool{}
	if !containsAnyStr(body, unknownFieldPhrases...) {
		return out
	}
	sent := sentAssistantCarriers(sentMessages)
	runes := pystr.Runes(body)
	named := map[string]bool{}
	for k := range sent {
		if m, err := namedFieldRes[k].FindRunesMatch(runes); err == nil && m != nil {
			named[k] = true
		}
	}
	if len(named) == 0 {
		return out
	}
	for k := range sent {
		if named[carrierRC] || k != carrierRC {
			out[k] = true
		}
	}
	return out
}

var finishReasonAliases = map[string]string{"max_tokens": "length", "end": "stop", "function_call": "tool_calls"}

// NormalizeFinishReason folds a wire finish reason to the lowercase OpenAI value, passing non strings and empty through
func NormalizeFinishReason(raw any) any {
	s, ok := raw.(string)
	if !ok || s == "" {
		return raw
	}
	lower := pystr.Lower(s)
	if alias, ok := finishReasonAliases[lower]; ok {
		return alias
	}
	return lower
}

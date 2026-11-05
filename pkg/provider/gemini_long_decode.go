package provider

import "strings"

// Total thinking plus output budget Gemini 4 gets for an effort above high
var geminiLongDecodeBudgets = map[string]int{"xhigh": 262_144, "max": 1_048_576, "ultra": 1_048_576}

// usageMetadata counts summed across long decode hops
var geminiUsageCountKeys = []string{
	"promptTokenCount", "cachedContentTokenCount", "candidatesTokenCount", "thoughtsTokenCount",
	"toolUsePromptTokenCount", "totalTokenCount",
}

// Total output budget a Gemini 4 request gets for its reasoning effort, zero for the per request cap
func longDecodeMaxOutputTokens(model string, reasoningConfig map[string]any) int {
	name := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "google/")
	if !strings.HasPrefix(name, "gemini-4") || reasoningConfig == nil {
		return 0
	}
	if enabled, ok := reasoningConfig["enabled"].(bool); ok && !enabled {
		return 0
	}
	return geminiLongDecodeBudgets[strings.ToLower(strings.TrimSpace(pyOrStr(reasoningConfig["effort"])))]
}

// Hop count that scales the non streaming stale budget of one api payload
func longDecodeStaleMultiplier(apiPayload map[string]any) int {
	extra, ok := apiPayload["extra_body"].(map[string]any)
	if !ok {
		return 1
	}
	return longDecodeHops(extra["max_output_tokens"])
}

// First candidate of a Gemini payload, empty when absent
func geminiCandidate(payload map[string]any) map[string]any {
	candidates, _ := payload["candidates"].([]any)
	if len(candidates) == 0 {
		return map[string]any{}
	}
	first, ok := candidates[0].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return first
}

// Token to resend with when the hop ended on CONTINUATION, empty on any other finish
func continuationToken(payload map[string]any) string {
	cand := geminiCandidate(payload)
	token, _ := cand["continuationToken"].(string)
	if strings.ToUpper(pyOrStr(cand["finishReason"])) == "CONTINUATION" && token != "" {
		return token
	}
	return ""
}

// Integer value of one usage count, zero when absent
func geminiCount(m map[string]any, key string) int {
	n, _ := geminiPyNumber(m[key])
	return n
}

// Every count summed across hops with the first hop prompt kept as the initial prompt
func mergeHopUsage(hops []map[string]any) map[string]any {
	merged := map[string]any{}
	for _, key := range geminiUsageCountKeys {
		present, sum := false, 0
		for _, h := range hops {
			if _, ok := h[key]; ok {
				present = true
			}
			sum += geminiCount(h, key)
		}
		if present {
			merged[key] = sum
		}
	}
	first := map[string]any{}
	if len(hops) > 0 {
		first = hops[0]
	}
	merged["initialPromptTokenCount"] = geminiCount(first, "promptTokenCount")
	merged["initialCachedContentTokenCount"] = geminiCount(first, "cachedContentTokenCount")
	return merged
}

// One response whose model turn joins every hop's parts, ending as the last hop did
func mergeHopResponses(hops []map[string]any) map[string]any {
	var parts []any
	usages := make([]map[string]any, 0, len(hops))
	for _, hop := range hops {
		content, _ := geminiCandidate(hop)["content"].(map[string]any)
		if list, ok := content["parts"].([]any); ok {
			parts = append(parts, list...)
		}
		um, _ := hop["usageMetadata"].(map[string]any)
		if um == nil {
			um = map[string]any{}
		}
		usages = append(usages, um)
	}
	last := geminiCandidate(hops[len(hops)-1])
	candidate := copyAnyMap(last)
	content := map[string]any{}
	if lc, ok := last["content"].(map[string]any); ok {
		content = copyAnyMap(lc)
	}
	content["role"] = "model"
	if parts == nil {
		parts = []any{}
	}
	content["parts"] = parts
	candidate["content"] = content
	out := copyAnyMap(hops[len(hops)-1])
	out["candidates"] = []any{candidate}
	out["usageMetadata"] = mergeHopUsage(usages)
	return out
}

// Event to translate and the token to resend with when the stream ends there
func splitStreamContinuation(event map[string]any, hopUsage *[]map[string]any) (map[string]any, string) {
	if token := continuationToken(event); token != "" {
		um, _ := event["usageMetadata"].(map[string]any)
		if um == nil {
			um = map[string]any{}
		}
		*hopUsage = append(*hopUsage, um)
		cand := map[string]any{}
		for k, v := range geminiCandidate(event) {
			if k != "finishReason" && k != "continuationToken" {
				cand[k] = v
			}
		}
		out := map[string]any{}
		for k, v := range event {
			if k != "usageMetadata" {
				out[k] = v
			}
		}
		out["candidates"] = []any{cand}
		return out, token
	}
	cand := geminiCandidate(event)
	if len(*hopUsage) > 0 && pyTruthy(cand["finishReason"]) {
		um, _ := event["usageMetadata"].(map[string]any)
		if um == nil {
			um = map[string]any{}
		}
		all := append(append([]map[string]any{}, *hopUsage...), um)
		out := copyAnyMap(event)
		out["usageMetadata"] = mergeHopUsage(all)
		return out, ""
	}
	if pyTruthy(cand["finishReason"]) {
		return event, ""
	}
	checkpoint, _ := cand["continuationToken"].(string)
	return event, checkpoint
}

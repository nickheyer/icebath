package provider

import (
	"strings"
	"sync"
)

const (
	stablePrefixMaxEntries = 32
	stablePrefixMaxChars   = 4 * 1024 * 1024
)

var (
	stablePrefixMu   sync.Mutex
	stablePrefixList []string
)

// RegisterStablePrefix records the static scaffold of a just built user message for cache boundary placement
func RegisterStablePrefix(prefix string) {
	if prefix == "" {
		return
	}
	stablePrefixMu.Lock()
	defer stablePrefixMu.Unlock()
	for i, p := range stablePrefixList {
		if p == prefix {
			stablePrefixList = append(stablePrefixList[:i], stablePrefixList[i+1:]...)
			break
		}
	}
	stablePrefixList = append(stablePrefixList, prefix)
	for len(stablePrefixList) > stablePrefixMaxEntries {
		stablePrefixList = stablePrefixList[1:]
	}
	total := func() int {
		n := 0
		for _, p := range stablePrefixList {
			n += len(p)
		}
		return n
	}
	for len(stablePrefixList) > 1 && total() > stablePrefixMaxChars {
		stablePrefixList = stablePrefixList[1:]
	}
}

// Longest registered proper prefix of content with a non whitespace tail
func findStablePrefix(content string) string {
	stablePrefixMu.Lock()
	defer stablePrefixMu.Unlock()
	best := ""
	bestIdx := -1
	for i, p := range stablePrefixList {
		if strings.HasPrefix(content, p) && strings.TrimSpace(content[len(p):]) != "" && len(p) > len(best) {
			best, bestIdx = p, i
		}
	}
	if bestIdx >= 0 {
		stablePrefixList = append(stablePrefixList[:bestIdx], stablePrefixList[bestIdx+1:]...)
		stablePrefixList = append(stablePrefixList, best)
	}
	return best
}

// Reports whether a route is a LiteLLM proxy by a whole litellm token in provider id or host
func isLitellmRoute(providerLower, baseURL string) bool {
	return hasLitellmToken(providerLower, ":-_/") || hasLitellmToken(baseURLHostname(baseURL), ".-")
}

func hasLitellmToken(value, delimiters string) bool {
	if value == "" {
		return false
	}
	fields := strings.FieldsFunc(value, func(r rune) bool { return strings.ContainsRune(delimiters, r) || r == ' ' })
	for _, f := range fields {
		if f == "litellm" {
			return true
		}
	}
	return false
}

func envelopeToolPartCacheMarkersSupported(providerName, baseURL string) bool {
	return !isLitellmRoute(strings.ToLower(strings.TrimSpace(providerName)), baseURL)
}

// A request no later call can read back from cache
func isOneShotRequest(messages []map[string]any, tools []map[string]any) bool {
	if len(tools) > 0 {
		return false
	}
	users := 0
	for _, m := range messages {
		if _, has := m["cache_control"]; has || hasPartMarker(m["content"]) {
			return false
		}
		switch asStr(m["role"]) {
		case "user":
			users++
		case "assistant", "tool":
			return false
		}
	}
	return users <= 1
}

func textPart(text string, marker map[string]any) map[string]any {
	part := map[string]any{"type": "text", "text": text}
	if marker != nil {
		part["cache_control"] = marker
	}
	return part
}

// Adds cache_control to one message in whichever shape its content takes
func applyCacheMarker(msg map[string]any, marker map[string]any, nativeAnthropic, toolPartMarkers bool) {
	role := asStr(msg["role"])
	content := msg["content"]
	if role == "tool" && !nativeAnthropic && !toolPartMarkers {
		return
	}
	contentStr, isStr := asString(content)
	if (role == "tool" && nativeAnthropic) || content == nil || (isStr && contentStr == "") {
		if !((role == "tool" || role == "assistant") && !nativeAnthropic) {
			msg["cache_control"] = marker
		}
		return
	}
	if isStr {
		if parts := scaffoldParts(msg, marker); parts != nil {
			msg["content"] = parts
		} else {
			msg["content"] = []any{textPart(contentStr, marker)}
		}
		return
	}
	if list, ok := asList(content); ok && len(list) > 0 {
		if last, ok := asMap(list[len(list)-1]); ok {
			last["cache_control"] = marker
		}
	}
}

// Scaffold and tail parts of a string user message with a registered scaffold
func scaffoldParts(msg map[string]any, marker map[string]any) []any {
	content, ok := asString(msg["content"])
	if !ok || asStr(msg["role"]) != "user" {
		return nil
	}
	prefix := findStablePrefix(content)
	if prefix == "" {
		return nil
	}
	return []any{textPart(prefix, marker), textPart(content[len(prefix):], nil)}
}

func splitUnmarkedScaffolds(messages []map[string]any) {
	for i, msg := range messages {
		if parts := scaffoldParts(msg, nil); parts != nil {
			copyM := map[string]any{}
			for k, v := range msg {
				copyM[k] = v
			}
			copyM["content"] = parts
			messages[i] = copyM
		}
	}
}

// True when a marker on this message is honored by the provider
func canCarryMarker(msg map[string]any, nativeAnthropic, toolPartMarkers bool) bool {
	if nativeAnthropic {
		return true
	}
	if asStr(msg["role"]) == "tool" && !toolPartMarkers {
		return false
	}
	content := msg["content"]
	if list, ok := asList(content); ok && len(list) > 0 {
		_, isMap := asMap(list[len(list)-1])
		return isMap
	}
	s, ok := asString(content)
	return ok && s != ""
}

func buildCacheMarker(ttl string) map[string]any {
	if ttl == "1h" {
		return map[string]any{"type": "ephemeral", "ttl": "1h"}
	}
	return map[string]any{"type": "ephemeral"}
}

var alibabaFamilyProviders = map[string]bool{"opencode": true, "opencode-go": true, "opencode-zen": true, "alibaba": true}
var measured1HProviders = map[string]bool{"opencode-go": true}
var no1HTierModels = map[string]bool{"minimax-m2.5": true}
var machinePacedSources = map[string]bool{"subagent": true, "cron": true, "oneshot": true, "webhook": true, "kanban": true, "api": true, "tool": true, "batch": true}

func flatModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	return m
}

func isQwenModel(model string) bool { return strings.Contains(strings.ToLower(model), "qwen") }

// AutoCacheTTLForSource resolves the auto tier, 5m for machine paced sources and 1h for human paced ones
func AutoCacheTTLForSource(source string) string {
	if machinePacedSources[strings.ToLower(strings.TrimSpace(source))] {
		return "5m"
	}
	return "1h"
}

// Requested cache TTL clamped to what the destination route supports
func effectiveCacheTTL(ttl, model, providerName string) string {
	if ttl != "1h" {
		if ttl == "" {
			return "5m"
		}
		return ttl
	}
	p := strings.ToLower(providerName)
	if measured1HProviders[p] {
		if no1HTierModels[flatModel(model)] {
			return "5m"
		}
		return "1h"
	}
	if isQwenModel(model) || alibabaFamilyProviders[p] {
		return "5m"
	}
	return "1h"
}

// Marks the static system prefix and optionally the full prompt, returning markers applied
func applySystemCacheMarkers(message map[string]any, marker map[string]any, staticPrefix string, nativeAnthropic, markSuffix, fallbackToWhole bool) int {
	content, isStr := asString(message["content"])
	if staticPrefix != "" && isStr && strings.HasPrefix(content, staticPrefix) {
		suffix := content[len(staticPrefix):]
		if strings.TrimSpace(suffix) != "" {
			var suffixMarker map[string]any
			if markSuffix {
				suffixMarker = marker
			}
			message["content"] = []any{textPart(staticPrefix, marker), textPart(suffix, suffixMarker)}
			if markSuffix {
				return 2
			}
			return 1
		}
	} else if !fallbackToWhole {
		return 0
	}
	applyCacheMarker(message, marker, nativeAnthropic, true)
	return 1
}

func hasPartMarker(content any) bool {
	list, ok := asList(content)
	if !ok {
		return false
	}
	for _, p := range list {
		if pm, ok := asMap(p); ok {
			if _, has := pm["cache_control"]; has {
				return true
			}
		}
	}
	return false
}

// Removes cache_control markers and undoes decoration produced list shapes
func stripAnthropicCacheControl(messages []map[string]any) []map[string]any {
	for _, msg := range messages {
		delete(msg, "cache_control")
		content, ok := asList(msg["content"])
		if !ok {
			continue
		}
		role := asStr(msg["role"])
		userPair := role == "user" && len(content) == 2
		if userPair {
			for _, p := range content {
				if _, ok := asMap(p); !ok {
					userPair = false
				}
			}
		}
		firstMarked := false
		if userPair {
			p0, _ := asMap(content[0])
			p1, _ := asMap(content[1])
			_, m0 := p0["cache_control"]
			_, m1 := p1["cache_control"]
			firstMarked = m0 && !m1
		}
		if hasPartMarker(content) {
			cleaned := make([]any, len(content))
			for i, p := range content {
				pm, ok := asMap(p)
				if !ok {
					cleaned[i] = p
					continue
				}
				if _, has := pm["cache_control"]; has {
					copyP := map[string]any{}
					for k, v := range pm {
						if k != "cache_control" {
							copyP[k] = v
						}
					}
					cleaned[i] = copyP
				} else {
					cleaned[i] = pm
				}
			}
			content = cleaned
			msg["content"] = content
		}
		plainText := len(content) > 0
		for _, p := range content {
			pm, ok := asMap(p)
			if !ok {
				plainText = false
				break
			}
			t := "text"
			if v, has := pm["type"]; has {
				t = asStr(v)
			}
			_, textIsStr := asString(pm["text"])
			if t != "text" || !textIsStr {
				plainText = false
				break
			}
			for k := range pm {
				if k != "type" && k != "text" {
					plainText = false
				}
			}
			if !plainText {
				break
			}
		}
		if !plainText {
			continue
		}
		var sb strings.Builder
		for _, p := range content {
			pm, _ := asMap(p)
			sb.WriteString(asStr(pm["text"]))
		}
		joined := sb.String()
		skillSplit := firstMarked
		if !skillSplit && userPair {
			p0, _ := asMap(content[0])
			skillSplit = findStablePrefix(joined) == asStr(p0["text"])
		}
		if len(content) == 1 || (role == "system" && len(content) == 2) || skillSplit {
			msg["content"] = joined
		}
	}
	return messages
}

// Re canonicalises planner output before re planning
func stripPriorDecoration(messages []map[string]any) []map[string]any {
	for i, msg := range messages {
		_, marked := msg["cache_control"]
		_, isList := asList(msg["content"])
		if marked || isList {
			copyM := map[string]any{}
			for k, v := range msg {
				copyM[k] = v
			}
			messages[i] = stripAnthropicCacheControl([]map[string]any{copyM})[0]
		}
	}
	return messages
}

// Copied tools without request local cache markers
func stripAnthropicToolCacheControl(tools []map[string]any) []map[string]any {
	cleaned := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		copyT, _ := asMap(deepCopyAny(t))
		delete(copyT, "cache_control")
		cleaned = append(cleaned, copyT)
	}
	return cleaned
}

// Wire visible cache markers in a request local plan
func countCacheMarkers(messages []map[string]any, tools []map[string]any) int {
	n := 0
	for _, m := range messages {
		if _, has := m["cache_control"]; has {
			n++
		}
		if list, ok := asList(m["content"]); ok {
			for _, p := range list {
				if pm, ok := asMap(p); ok {
					if _, has := pm["cache_control"]; has {
						n++
					}
				}
			}
		}
	}
	for _, t := range tools {
		if _, has := t["cache_control"]; has {
			n++
		}
	}
	return n
}

// Legal ends of completed tool runs and ordinary turns
func completedTransactionEndpointIndexes(messages []map[string]any, nativeAnthropic bool) []int {
	toolRunEnd := func(start int) int {
		end := start
		for end < len(messages) && asStr(messages[end]["role"]) == "tool" {
			end++
		}
		return end
	}
	var endpoints []int
	index := 0
	for index < len(messages) {
		message := messages[index]
		role := asStr(message["role"])
		if role == "system" {
			index++
			continue
		}
		if calls, ok := asList(message["tool_calls"]); role == "assistant" && ok && len(calls) > 0 {
			resultEnd := toolRunEnd(index + 1)
			if resultEnd > index+1 && canCarryMarker(messages[resultEnd-1], nativeAnthropic, true) {
				endpoints = append(endpoints, resultEnd-1)
			}
			index = resultEnd
			continue
		}
		if role == "tool" {
			index = toolRunEnd(index)
			continue
		}
		contentStr, isStr := asString(message["content"])
		openTurn := (role == "user" && index+1 < len(messages)) || (role == "assistant" && (message["content"] == nil || (isStr && contentStr == "")))
		if !openTurn && canCarryMarker(message, nativeAnthropic, true) {
			endpoints = append(endpoints, index)
		}
		index++
	}
	return endpoints
}

// Cache plan inputs for one resolved request destination
type cachePlanOptions struct {
	ttl                   string
	nativeAnthropic       bool
	staticSystemPrefix    string
	directNativeToolCache bool
	toolPartMarkers       bool
}

// Copy on write cache sections for one resolved request destination
func buildPromptCachePlan(apiMessages []map[string]any, tools []map[string]any, opts cachePlanOptions) ([]map[string]any, []map[string]any) {
	plannedTools := stripAnthropicToolCacheControl(tools)
	if !opts.directNativeToolCache || len(plannedTools) == 0 {
		msgs := applyAnthropicCacheControl(append([]map[string]any{}, apiMessages...), opts.ttl, opts.nativeAnthropic, opts.staticSystemPrefix, opts.toolPartMarkers)
		return msgs, plannedTools
	}
	messages := stripPriorDecoration(append([]map[string]any{}, apiMessages...))
	marker := buildCacheMarker(opts.ttl)
	if len(messages) > 0 && asStr(messages[0]["role"]) == "system" {
		first, _ := asMap(deepCopyAny(messages[0]))
		messages[0] = first
		applySystemCacheMarkers(first, marker, opts.staticSystemPrefix, true, false, false)
	}
	plannedTools[len(plannedTools)-1]["cache_control"] = deepCopyAny(marker)
	endpoints := completedTransactionEndpointIndexes(messages, true)
	if len(endpoints) > 2 {
		endpoints = endpoints[len(endpoints)-2:]
	}
	for _, endpoint := range endpoints {
		copyM, _ := asMap(deepCopyAny(messages[endpoint]))
		messages[endpoint] = copyM
		applyCacheMarker(copyM, marker, true, true)
	}
	splitUnmarkedScaffolds(messages)
	return messages, plannedTools
}

// Anthropic cache markers on the system prompt and the latest cacheable messages
func applyAnthropicCacheControl(apiMessages []map[string]any, ttl string, nativeAnthropic bool, staticSystemPrefix string, toolPartMarkers bool) []map[string]any {
	if len(apiMessages) == 0 {
		return apiMessages
	}
	messages := stripPriorDecoration(apiMessages)
	marker := buildCacheMarker(ttl)
	used := 0
	if asStr(messages[0]["role"]) == "system" {
		first, _ := asMap(deepCopyAny(messages[0]))
		messages[0] = first
		used = applySystemCacheMarkers(first, marker, staticSystemPrefix, nativeAnthropic, true, true)
	}
	var nonSys []int
	for i, m := range messages {
		if asStr(m["role"]) != "system" && canCarryMarker(m, nativeAnthropic, toolPartMarkers) {
			nonSys = append(nonSys, i)
		}
	}
	budget := 4 - used
	if budget < 0 {
		budget = 0
	}
	if len(nonSys) > budget {
		nonSys = nonSys[len(nonSys)-budget:]
	}
	for _, idx := range nonSys {
		copyM, _ := asMap(deepCopyAny(messages[idx]))
		messages[idx] = copyM
		applyCacheMarker(copyM, marker, nativeAnthropic, toolPartMarkers)
	}
	splitUnmarkedScaffolds(messages)
	return messages
}

func modelNameSuggestsMinimaxM3(model string) bool {
	return strings.Contains(flatModel(model), "minimax-m3")
}

// AnthropicPromptCachePolicy decides whether an Anthropic Messages route gets cache markers
func AnthropicPromptCachePolicy(providerName, baseURL, model string) bool {
	p := strings.ToLower(strings.TrimSpace(providerName))
	lower := strings.ToLower(model)
	isClaude := strings.Contains(lower, "claude")
	minimaxRoute := p == "minimax" || p == "minimax-cn" || baseURLHostMatches(baseURL, "api.minimax.io") || baseURLHostMatches(baseURL, "api.minimaxi.com")
	if minimaxRoute && modelNameSuggestsMinimaxM3(model) {
		return false
	}
	if p == "anthropic" || baseURLHostname(baseURL) == "api.anthropic.com" {
		return true
	}
	if isClaude || minimaxRoute {
		return true
	}
	return false
}

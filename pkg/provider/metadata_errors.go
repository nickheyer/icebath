package provider

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Canonical model at base url key with trailing slashes stripped
func contextCacheKey(model, baseURL string) string {
	return model + "@" + strings.TrimRight(baseURL, "/")
}

// Exported cache key for tests of other packages
func ContextCacheKey(model, baseURL string) string { return contextCacheKey(model, baseURL) }

// SaveContextLength persists a discovered window under model at base url, source marks Bedrock provenance
func SaveContextLength(ctx context.Context, model, baseURL string, length int, source string) {
	if length <= 0 {
		metadataLog.Warn(fmt.Sprintf("Refusing to cache non-positive context length %s@%s -> %d tokens", model, baseURL, length))
		return
	}
	key := contextCacheKey(model, baseURL)
	lockedContextCache(ctx, func() {
		cache, confirmed := loadContextCacheDocument(ctx)
		oldConfirmed := map[string]any{}
		for k, v := range confirmed {
			oldConfirmed[k] = v
		}
		for _, alias := range []string{key, model + "@" + baseURL, key + "/"} {
			delete(confirmed, alias)
		}
		if source == "bedrock-confirmed-v1" {
			confirmed[key] = length
			config.AppendMappingKey(confirmed, key)
		}
		if cur, ok := cache[key]; ok && cur == length && sameConfirmed(confirmed, oldConfirmed) {
			return
		}
		cache[key] = length
		if err := writeContextCache(ctx, cache, confirmed); err != nil {
			metadataLog.Debug("failed to save context length cache", "error", err)
			return
		}
		metadataLog.Info(fmt.Sprintf("Cached context length %s -> %s tokens", key, formatWithCommas(length)))
	})
}

func sameConfirmed(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		av, _ := intValue(v)
		bv, ok := intValue(b[k])
		if !ok || av != bv {
			return false
		}
	}
	return true
}

// SaveProviderContextLength persists a provider confirmed window, Bedrock routes get their provenance marker
func SaveProviderContextLength(ctx context.Context, model, baseURL string, length int, providerName string) {
	if isBedrockContext(baseURL, providerName) {
		if baseURL == "" {
			baseURL = "bedrock://"
		}
		SaveContextLength(ctx, model, baseURL, length, "bedrock-confirmed-v1")
		return
	}
	SaveContextLength(ctx, model, baseURL, length, "")
}

// GetCachedContextLength looks up a persisted window, optionally requiring Bedrock confirmation
func GetCachedContextLength(ctx context.Context, model, baseURL string, bedrockConfirmed bool) (int, bool) {
	key := contextCacheKey(model, baseURL)
	cache, confirmed := loadContextCacheDocument(ctx)
	matchedKey, length, found := "", 0, false
	for _, k := range []string{key, model + "@" + baseURL, key + "/"} {
		if v, ok := cache[k]; ok {
			matchedKey, length, found = k, v, true
			break
		}
	}
	if !found {
		return 0, false
	}
	if bedrockConfirmed {
		marker, ok := intValue(confirmed[matchedKey])
		if !ok || marker != length {
			return 0, false
		}
	}
	return length, true
}

type bedrockProbeKey struct{ home, url, model, region string }

// Negative only in memory memo of failed Bedrock probes
const bedrockProbeFailureTTL = 300 * time.Second

var (
	bedrockProbeFailureMu    sync.Mutex
	bedrockProbeFailureCache = map[bedrockProbeKey]time.Time{}
)

// Empties the failed probe memo, the in memory state a process restart drops
func ResetBedrockProbeFailureCache() {
	bedrockProbeFailureMu.Lock()
	clear(bedrockProbeFailureCache)
	bedrockProbeFailureMu.Unlock()
}

// Drops a stale entry, its in memory probe memos and the file rows under the cache lock
func invalidateCachedContextLength(ctx context.Context, model, baseURL string) {
	key := contextCacheKey(model, baseURL)
	bare, stripped := stripProviderPrefix(model), strings.TrimRight(baseURL, "/")
	localCtxProbeMu.Lock()
	delete(localCtxProbeCache, localProbeKey{"", bare, stripped})
	delete(localCtxProbeCache, localProbeKey{"ollama_show", bare, stripped})
	localCtxProbeMu.Unlock()
	homeKey := metadataHome(ctx)
	bedrockProbeFailureMu.Lock()
	for memoKey := range bedrockProbeFailureCache {
		if memoKey.home == homeKey && memoKey.url == stripped && (memoKey.model == model || memoKey.model == bare) {
			delete(bedrockProbeFailureCache, memoKey)
		}
	}
	bedrockProbeFailureMu.Unlock()
	dropContextCacheRows(ctx, []string{key, model + "@" + baseURL, key + "/"}, key)
}

// Exported invalidation for tests of other packages
func InvalidateCachedContextLength(ctx context.Context, model, baseURL string) {
	invalidateCachedContextLength(ctx, model, baseURL)
}

// GetNextProbeTier returns the next lower probe tier, false at the minimum
func GetNextProbeTier(current int) (int, bool) {
	for _, tier := range ContextProbeTiers {
		if tier < current {
			return tier, true
		}
	}
	return 0, false
}

var contextLimitPatterns = []*regexp.Regexp{
	regexp.MustCompile(`max_model_len\s*(?:is\s*)?[:=(]?\s*(\d{4,})`),
	regexp.MustCompile(`maximum model length\s*(?:is\s*)?[:=(]?\s*(\d{4,})`),
	regexp.MustCompile(`(?:max(?:imum)?|limit)\s*(?:context\s*)?(?:length|size|window)?\s*(?:is|of|:)?\s*(\d{4,})`),
	regexp.MustCompile(`context\s*(?:length|size|window)\s*(?:is|of|:)?\s*(\d{4,})`),
	regexp.MustCompile(`(\d{4,})\s*(?:token)?\s*(?:context|limit)`),
	regexp.MustCompile(`>\s*(\d{4,})\s*(?:max|limit|token)`),
	regexp.MustCompile(`(\d{4,})\s*(?:max(?:imum)?)\b`),
	regexp.MustCompile(`supports?\s+(?:only\s+)?up\s+to\s+(\d{4,})`),
}

// ParseContextLimitFromError returns the context window a provider error quotes
func ParseContextLimitFromError(errorMsg string) (int, bool) {
	lower := strings.ToLower(errorMsg)
	if (strings.Contains(lower, "output limit") || strings.Contains(lower, "output tokens") || strings.Contains(lower, "output token")) && !strings.Contains(lower, "context") {
		return 0, false
	}
	for _, re := range contextLimitPatterns {
		m := re.FindStringSubmatch(lower)
		if m == nil {
			continue
		}
		limit, err := strconv.Atoi(m[1])
		if err == nil && limit >= 1024 && limit <= 10_000_000 {
			return limit, true
		}
	}
	return 0, false
}

// GetContextLengthFromProviderError returns a provider reported limit lower than the current window
func GetContextLengthFromProviderError(errorMsg string, current int) (int, bool) {
	limit, ok := ParseContextLimitFromError(errorMsg)
	if ok && limit < current {
		return limit, true
	}
	return 0, false
}

var completionSplitRe = regexp.MustCompile(`\((\d+)\s+(?:tokens\s+)?in (?:the messages|your prompt|the prompt)\s*[;,]\s*(\d+)\s+(?:tokens\s+)?(?:in|for) the completion\)`)

var maximumContextLengthIsRe = regexp.MustCompile(`maximum context length is (\d+)`)

// Window minus the measured prompt from the OpenAI style parenthetical split
func completionSplitBudget(lower string) (int, bool) {
	split := completionSplitRe.FindStringSubmatch(lower)
	ctxMatch := maximumContextLengthIsRe.FindStringSubmatch(lower)
	if split == nil || ctxMatch == nil {
		return 0, false
	}
	window, _ := strconv.Atoi(ctxMatch[1])
	prompt, _ := strconv.Atoi(split[1])
	if available := window - prompt; available >= 1 {
		return available, true
	}
	return 0, false
}

var availableOutputPatterns = []*regexp.Regexp{
	regexp.MustCompile(`exceeds model(?:'s)? maximum output tokens\s*\(?\s*(\d+)\s*\)?`),
	regexp.MustCompile(`max_tokens\s*:\s*\d+\s*>\s*(\d+)\s*,?\s*which is the maximum allowed number of output tokens`),
	regexp.MustCompile(`range of max_tokens should be\s*\[\s*\d+\s*,\s*(\d+)\s*\]`),
	regexp.MustCompile(`available_tokens[:\s]+(\d+)`),
	regexp.MustCompile(`available\s+tokens[:\s]+(\d+)`),
	regexp.MustCompile(`output limit (?:of|is)\s*(\d+)`),
	regexp.MustCompile(`supports at most\s+(\d+)\s*(?:completion\s+)?tokens`),
	regexp.MustCompile(`(?:max_tokens|max_completion_tokens) is limited to\s*(\d+)`),
	regexp.MustCompile(`=\s*(\d+)\s*$`),
}

var (
	outputPartsRe         = regexp.MustCompile(`\((\d+)\s+of text input,\s*(\d+)\s+of tool input,\s*(\d+)\s+in the output\)`)
	maximumContextTokenRe = regexp.MustCompile(`maximum context length is (\d+)\s*token`)
	promptCharsRe         = regexp.MustCompile(`prompt contains (\d+)\s*character`)
	vllmInputRe           = regexp.MustCompile(`prompt contains (?:at least )?(\d+)\s*input tokens`)
	requestedOutputRe     = regexp.MustCompile(`requested (\d+)\s*output tokens`)
	sglangWindowRe        = regexp.MustCompile(`maximum context length of (\d+)\s*token`)
	sglangInputRe         = regexp.MustCompile(`(\d+)\s*tokens from the input messages`)
)

// ParseAvailableOutputTokensFromError extracts the output budget from a max_tokens too large error
func ParseAvailableOutputTokensFromError(errorMsg string) (int, bool) {
	lower := strings.ToLower(errorMsg)
	if !anyPhraseGroup(lower, parseableOutputCapSignals) {
		return 0, false
	}
	for _, re := range availableOutputPatterns {
		if m := re.FindStringSubmatch(lower); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n >= 1 {
				return n, true
			}
		}
	}
	if ctxMatch, parts := maximumContextLengthIsRe.FindStringSubmatch(lower), outputPartsRe.FindStringSubmatch(lower); ctxMatch != nil && parts != nil {
		window, _ := strconv.Atoi(ctxMatch[1])
		text, _ := strconv.Atoi(parts[1])
		tool, _ := strconv.Atoi(parts[2])
		if available := window - text - tool; available >= 1 {
			return available, true
		}
	}
	if n, ok := completionSplitBudget(lower); ok {
		return n, true
	}
	ctxTok := maximumContextTokenRe.FindStringSubmatch(lower)
	if chars := promptCharsRe.FindStringSubmatch(lower); ctxTok != nil && chars != nil {
		window, _ := strconv.Atoi(ctxTok[1])
		c, _ := strconv.Atoi(chars[1])
		if available := window - (c+2)/3; available >= 1 {
			return available, true
		}
	}
	if window, input, ok := sglangWindowAndInput(lower); ok && window-input >= 1 {
		return window - input, true
	}
	if vllm := vllmInputRe.FindStringSubmatch(lower); ctxTok != nil && vllm != nil {
		window, _ := strconv.Atoi(ctxTok[1])
		input, _ := strconv.Atoi(vllm[1])
		available := window - input
		requested := requestedOutputRe.FindStringSubmatch(lower)
		if strings.Contains(lower, "at least") && requested != nil {
			requestedOut, _ := strconv.Atoi(requested[1])
			if available >= requestedOut-1 {
				if half := requestedOut / 2; half > 1 {
					return half, true
				}
				return 1, true
			}
		}
		if available >= 1 {
			return available, true
		}
	}
	return 0, false
}

// Phrase groups where every phrase must be present
var outputCapSignals = [][]string{
	{"range of max_tokens should be"}, {"available_tokens"}, {"available tokens"},
	{"in the output", "maximum context length"}, {"requested", "output tokens"},
	{"should be"}, {"less than or equal"}, {"must be"}, {"exceeds model", "maximum output tokens"},
	{"output limit"}, {"maximum allowed number of output tokens"},
	{"max_tokens is too large", "supports at most"}, {"tokens from the input messages", "tokens for the completion"},
	{"limited to"},
}

var inputOverflowSignals = []string{
	"prompt is too long", "prompt too long", "input is too long", "input token",
	"prompt length", "prompt contains", "reduce the length",
}

var parseableOutputCapSignals = [][]string{
	{"max_tokens", "available_tokens"}, {"max_tokens", "available tokens"},
	{"in the output", "maximum context length"},
	{"maximum context length", "requested", "output tokens"},
	{"maximum context length", "in the completion"}, {"maximum context length", "for the completion"},
	{"range of max_tokens should be"}, {"exceeds model", "maximum output tokens"},
	{"output limit"}, {"max_tokens", "maximum allowed number of output tokens"},
	{"max_tokens is too large", "supports at most"}, {"tokens from the input messages", "tokens for the completion"},
	{"limited to"},
}

// Window and input tokens from the SGLang wording
func sglangWindowAndInput(lower string) (int, int, bool) {
	ctxMatch := sglangWindowRe.FindStringSubmatch(lower)
	inMatch := sglangInputRe.FindStringSubmatch(lower)
	if ctxMatch == nil || inMatch == nil {
		return 0, 0, false
	}
	window, _ := strconv.Atoi(ctxMatch[1])
	input, _ := strconv.Atoi(inMatch[1])
	return window, input, true
}

func anyPhraseGroup(text string, groups [][]string) bool {
	for _, group := range groups {
		all := true
		for _, p := range group {
			if !strings.Contains(text, p) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// IsOutputCapError reports a max_tokens cap rejection that is not an oversized input
func IsOutputCapError(errorMsg string) bool {
	lower := strings.ToLower(errorMsg)
	if _, ok := completionSplitBudget(lower); ok {
		return true
	}
	window, input, sglang := sglangWindowAndInput(lower)
	mentionsCap := false
	for _, p := range []string{"max_tokens", "max_output_tokens", "max_completion_tokens", "tokens for the completion"} {
		if strings.Contains(lower, p) {
			mentionsCap = true
			break
		}
	}
	if !mentionsCap || !anyPhraseGroup(lower, outputCapSignals) {
		return false
	}
	for _, p := range inputOverflowSignals {
		if strings.Contains(lower, p) {
			return false
		}
	}
	return !(sglang && input >= window)
}

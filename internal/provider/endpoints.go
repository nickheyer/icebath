package provider

import (
	"net/url"
	"regexp"
	"strings"
)

// Hosts that take one request format only
var hostModes = map[string]APIMode{"api.x.ai": ModeCodexResponses, "api.meta.ai": ModeCodexResponses, "api.actual.inc": ModeChatCompletions,
	"api.router.com": ModeCodexResponses, "api.anthropic.com": ModeAnthropicMessages}

// Host of an address in lower case, empty when it has none
func Hostname(address string) string {
	parsed, err := url.Parse(strings.TrimSpace(address))
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

// Request format an endpoint's address says it takes: its host's own, the Responses API of OpenAI's, Anthropic messages for an anthropic
// path or Kimi's coding endpoint; empty when the address says nothing
func ModeForURL(address string) APIMode {
	host := Hostname(address)
	if mode, ok := hostModes[host]; ok {
		return mode
	}
	if host == "api.openai.com" {
		return ModeCodexResponses
	}
	normalized := strings.ToLower(strings.TrimRight(strings.TrimSpace(address), "/"))
	path := ""
	if parsed, err := url.Parse(normalized); err == nil {
		path = parsed.Path
	}
	if strings.HasSuffix(path, "/anthropic") || strings.HasSuffix(path, "/anthropic/v1") || host == "api.kimi.com" && strings.Contains(normalized, "/coding") {
		return ModeAnthropicMessages
	}
	if NativeGemini(address) {
		return ModeGeminiNative
	}
	return ""
}

// Whether the address is Google's own Gemini API, AI Studio's or Vertex AI express mode, rather than an OpenAI compatible surface of it
func NativeGemini(address string) bool {
	normalized := strings.ToLower(strings.TrimRight(strings.TrimSpace(address), "/"))
	if strings.HasSuffix(normalized, "/openai") {
		return false
	}
	return strings.Contains(normalized, "generativelanguage.googleapis.com") ||
		strings.Contains(normalized, "aiplatform.googleapis.com") && !strings.Contains(normalized, "/endpoints/")
}

var versionSegment = regexp.MustCompile(`(?i)^v\d+(?:alpha|beta)?\d*$`)

// Native base of a Gemini address: the OpenAI suffix taken off, a Vertex OpenAI endpoint as its publishers/google path, an express base
// completed to it, the API version added when missing; AI Studio's v1beta for none
func GeminiBase(base string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(base), "/")
	if lower := strings.ToLower(trimmed); strings.Contains(lower, "aiplatform.googleapis.com") {
		if i := strings.Index(lower, "/endpoints/"); i >= 0 {
			trimmed = trimmed[:i] + "/publishers/google"
		}
	}
	if strings.HasSuffix(strings.ToLower(trimmed), "/openai") {
		trimmed = strings.TrimRight(trimmed[:len(trimmed)-len("/openai")], "/")
	}
	if trimmed == "" {
		trimmed = "https://generativelanguage.googleapis.com/v1beta"
	}
	last := trimmed[strings.LastIndex(trimmed, "/")+1:]
	lower := strings.ToLower(trimmed)
	if strings.Contains(lower, "aiplatform.googleapis.com") {
		if strings.HasSuffix(lower, "/publishers/google") {
			return trimmed
		}
		if !strings.Contains(lower, "/projects/") && !versionSegment.MatchString(last) {
			trimmed += "/v1beta1"
		}
		return trimmed + "/publishers/google"
	}
	if versionSegment.MatchString(last) {
		return trimmed
	}
	return trimmed + "/v1beta"
}

// Request formats a setting may name
func KnownMode(mode string) bool {
	switch APIMode(mode) {
	case ModeChatCompletions, ModeAnthropicMessages, ModeCodexResponses, ModeGeminiNative, ModeBedrockConverse:
		return true
	}
	return false
}

// Speed tiers a request can ask for, as their words and the tiers they stand for: normal is none
var (
	normalTiers = map[string]bool{"": true, "normal": true, "default": true, "standard": true, "off": true, "none": true}
	tierWords   = map[string]string{"fast": "priority", "priority": "priority", "on": "priority", "ultrafast": "ultrafast", "ultra": "ultrafast",
		"auto": "auto", "cold": "cold"}
	// Tiers a request asks for by itself, which need no window of their own
	staticTiers = map[string]bool{"priority": true, "ultrafast": true}
)

// Tier a word asks for, empty for the normal tier; an error for a word that names none
func Tier(word string) (string, error) {
	word = strings.ToLower(strings.TrimSpace(word))
	if normalTiers[word] {
		return "", nil
	}
	tier, ok := tierWords[word]
	if !ok {
		return "", &unknownTier{word}
	}
	return tier, nil
}

type unknownTier struct{ word string }

func (e *unknownTier) Error() string { return "unknown service tier: " + e.word }

// Whether the tier is one a single request asks for, priority or ultrafast
func StaticTier(tier string) bool { return staticTiers[tier] }

// Word a tier is shown by
func TierWord(tier string) string {
	switch tier {
	case "priority":
		return "fast"
	case "":
		return "normal"
	}
	return tier
}

var (
	// OpenAI flagships Priority Processing serves, and the models it serves Ultrafast for
	fastOpenAI     = []string{"gpt-", "o1", "o3", "o4"}
	ultrafastSlugs = map[string]bool{"gpt-6-astra": true, "gpt-6.1-sol": true}
	// Claude models that take the fast speed
	fastClaude   = map[string]bool{"claude-opus-4-8": true, "claude-opus-5": true, "claude-opus-5-5": true}
	dottedNumber = regexp.MustCompile(`(\d)\.(\d)`)
	dateSuffix   = regexp.MustCompile(`-\d{8}$`)
)

// Model id without its vendor and variant parts, in lower case
func bareModel(model string) string {
	name := strings.ToLower(strings.TrimSpace(model))
	name, _, _ = strings.Cut(name, ":")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return name
}

func fastClaudeModel(model string) bool {
	return fastClaude[dateSuffix.ReplaceAllString(dottedNumber.ReplaceAllString(bareModel(model), "$1-$2"), "")]
}

// Whether the model is of the Grok 4.6 family
func Grok46(model string) bool {
	name := strings.ReplaceAll(bareModel(model), "_", "-")
	return name == "grok-4.6" || strings.HasPrefix(name, "grok-4.6-")
}

func fastOpenAIModel(model string) bool {
	base := bareModel(model)
	if base == "" || strings.Contains(base, "codex") {
		return false
	}
	for _, prefix := range fastOpenAI {
		if strings.HasPrefix(base, prefix) {
			return true
		}
	}
	return false
}

// Request fields that ask for the tier on the route, nil when the model or the provider that bills for it does not offer it: only the
// first party endpoint of a model's maker may be sent them
func FastFields(model, kind, base, tier string) map[string]any {
	claude, grok := fastClaudeModel(model), Grok46(model)
	if !claude && !grok && !fastOpenAIModel(model) {
		return nil
	}
	allowed := map[string]string{"openai": "api.openai.com", "openai-api": "api.openai.com", "openai-codex": "chatgpt.com"}
	switch {
	case claude:
		allowed = map[string]string{"anthropic": "api.anthropic.com"}
	case grok:
		allowed = map[string]string{"xai": "api.x.ai"}
	}
	if kind != "" {
		if _, ok := allowed[strings.ToLower(kind)]; !ok {
			return nil
		}
	}
	if host := Hostname(base); host != "" {
		found := false
		for _, h := range allowed {
			found = found || h == host
		}
		if !found {
			return nil
		}
	}
	switch {
	case tier == "ultrafast" && ultrafastSlugs[strings.TrimSuffix(bareModel(model), "-900k")]:
		return map[string]any{"service_tier": "ultrafast"}
	case tier == "ultrafast":
		return nil
	case claude:
		return map[string]any{"speed": "fast"}
	}
	return map[string]any{"service_tier": "priority"}
}

// Whether the address is OpenAI's own Codex backend rather than a proxy of it
func OfficialCodex(base string) bool {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "chatgpt.com") || parsed.Port() != "" && parsed.Port() != "443" {
		return false
	}
	path := strings.TrimRight(parsed.Path, "/")
	return path == "/backend-api/codex" || strings.HasPrefix(path, "/backend-api/codex/")
}

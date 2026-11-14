package gemini

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/internal/model/wire"
)

// Native API base of Google AI Studio and the Vertex AI express surface that serves the same API
const (
	DefaultBaseURL       = "https://generativelanguage.googleapis.com/v1beta"
	VertexExpressBaseURL = "https://aiplatform.googleapis.com/v1beta1/publishers/google"
)

// Published output ceiling of every current Gemini text model, sent when a request names no cap
const DefaultMaxOutputTokens = 65535

// Smallest per-request output cap, the unit long decode hop counts are derived from
const hopTokens = 32768

// Version of a Gemini model id and a trailing API version path segment
var (
	modelVersion      = regexp.MustCompile(`^gemini-(\d+)(?:\.(\d+))?`)
	apiVersionSegment = regexp.MustCompile(`(?i)^v\d+(?:alpha|beta)?\d*$`)
)

// Trims whitespace of every script from both ends
func strip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) })
}

// Model id with Gemini's own provider prefix removed
func BareModelID(model string) string {
	name := strip(model)
	for _, prefix := range [...]string{"google/", "gemini/"} {
		if strings.HasPrefix(strings.ToLower(name), prefix) {
			if rest := strip(name[len(prefix):]); rest != "" {
				return rest
			}
			return name
		}
	}
	return name
}

// Major and minor release of a Gemini model id, zeros for other and unversioned ids
func version(model string) (major, minor int) {
	m := modelVersion.FindStringSubmatch(strings.ToLower(BareModelID(model)))
	if m == nil {
		return 0, 0
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	return major, minor
}

// Reports whether a model needs explicit ids on replayed function calls and responses, Gemini 3 and later
func RequiresToolCallIDs(model string) bool {
	major, _ := version(model)
	return major >= 3
}

// Reports whether a model steers thinking with the thinkingLevel enum, Gemini 3 and later and the latest aliases
func UsesThinkingLevel(model string) bool {
	bare := strings.ToLower(BareModelID(model))
	return (strings.HasPrefix(bare, "gemini-") && strings.HasSuffix(bare, "-latest")) || RequiresToolCallIDs(bare)
}

// Reports whether a model ignores temperature, top_p and top_k, Gemini 3.6 and later
func SamplingDeprecated(model string) bool {
	major, minor := version(model)
	return major > 3 || (major == 3 && minor >= 6)
}

// Reports whether a base URL is an aiplatform host speaking the native API with an API key
func IsVertexExpressBaseURL(baseURL string) bool {
	normalized := strings.ToLower(strip(baseURL))
	return strings.Contains(normalized, "aiplatform.googleapis.com") && !strings.Contains(normalized, "/endpoints/")
}

// Native base URL with its API version segment guaranteed and any OpenAI-compatible suffix removed
func NormalizeBaseURL(baseURL string) string {
	trimmed := strings.TrimRight(strip(baseURL), "/")
	if len(trimmed) >= 7 && strings.EqualFold(trimmed[len(trimmed)-7:], "/openai") {
		trimmed = strings.TrimRight(trimmed[:len(trimmed)-7], "/")
	}
	if trimmed == "" {
		trimmed = DefaultBaseURL
	}
	last := trimmed[strings.LastIndex(trimmed, "/")+1:]
	if IsVertexExpressBaseURL(trimmed) {
		lower := strings.ToLower(trimmed)
		if strings.HasSuffix(lower, "/publishers/google") {
			return trimmed
		}
		if !strings.Contains(lower, "/projects/") && !apiVersionSegment.MatchString(last) {
			trimmed += "/v1beta1"
		}
		return trimmed + "/publishers/google"
	}
	if apiVersionSegment.MatchString(last) {
		return trimmed
	}
	return trimmed + "/v1beta"
}

// Reports whether an endpoint speaks Gemini's native REST API and not the OpenAI-compatible one
func IsNativeBaseURL(baseURL string) bool {
	normalized := strings.ToLower(strings.TrimRight(strip(baseURL), "/"))
	if strings.HasSuffix(normalized, "/openai") {
		return false
	}
	return strings.Contains(normalized, "generativelanguage.googleapis.com") || IsVertexExpressBaseURL(normalized)
}

// Reports whether a base URL is Google's OpenAI-compatible Gemini endpoint
func IsOpenAICompatBaseURL(baseURL string) bool {
	normalized := strings.ToLower(strings.TrimRight(strip(baseURL), "/"))
	return strings.Contains(normalized, "generativelanguage.googleapis.com") && strings.HasSuffix(normalized, "/openai")
}

// Reports whether the API version of a base URL has the full JSON Schema tool and response fields
func acceptsJSONSchema(baseURL string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimRight(strip(baseURL), "/")), "/v1beta")
}

// Kind of an API key by its public prefix, standard for a legacy Cloud key, auth for the newer form
func KeyKind(apiKey string) string {
	key := strip(apiKey)
	switch {
	case strings.HasPrefix(apiKey, "AIza"):
		return "standard"
	case strings.HasPrefix(strings.ToUpper(key), "AQ."):
		return "auth"
	}
	return ""
}

// Reports whether an effort word is one of the levels above medium
func highEffort(effort string) bool {
	return effort == "high" || effort == "xhigh" || effort == "max" || effort == "ultra"
}

// Native thinkingConfig for a reasoning ask, nil for no ask and for models that take no thinking field
func ThinkingConfig(model string, r *wire.Reasoning) json.RawMessage {
	if r == nil {
		return nil
	}
	name := strings.TrimPrefix(strings.ToLower(strip(model)), "google/")
	if !strings.HasPrefix(name, "gemini") {
		return nil
	}
	effort := strings.ToLower(strip(r.Effort))
	if effort == "" {
		effort = "medium"
	}
	off := !r.Enabled || effort == "none"
	d := &wire.Dict{}
	if strings.HasPrefix(name, "gemini-4") {
		level := "low"
		if !off {
			e := wire.EffortsGemini4.Efforts()
			if level = e.Clamp(effort); !e.Has(level) {
				level = "medium"
			}
		}
		return d.SetBool("includeThoughts", !off).SetString("thinkingLevel", level).JSON()
	}
	if off {
		d.SetBool("includeThoughts", false)
		if strings.HasPrefix(name, "gemini-2.5-") {
			d.SetInt("thinkingBudget", 0)
		} else if UsesThinkingLevel(name) {
			d.SetString("thinkingLevel", "low")
		}
		return d.JSON()
	}
	d.SetBool("includeThoughts", true)
	if strings.HasPrefix(name, "gemini-2.5-") || !UsesThinkingLevel(name) {
		return d.JSON()
	}
	switch effort {
	case "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
	default:
		effort = "medium"
	}
	level := "medium"
	switch {
	case highEffort(effort):
		level = "high"
	case strings.Contains(name, "pro"), effort == "minimal", effort == "low":
		level = "low"
	}
	return d.SetString("thinkingLevel", level).JSON()
}

// Thinking config with the member names the OpenAI-compatible endpoint takes, nil for an empty config
func SnakeThinkingConfig(config json.RawMessage) json.RawMessage {
	v, err := wire.ParseValue(config)
	if err != nil || !v.IsObject() {
		return nil
	}
	d := &wire.Dict{}
	if include, ok := v.Get("includeThoughts").Bool(); ok {
		d.SetBool("include_thoughts", include)
	}
	if level := strip(v.Get("thinkingLevel").Text()); level != "" {
		d.SetString("thinking_level", strings.ToLower(level))
	}
	if budget, ok := v.Get("thinkingBudget").Int(); ok {
		d.SetInt("thinking_budget", int(budget))
	}
	if d.Len() == 0 {
		return nil
	}
	return d.JSON()
}

// Thinking config in the native member names with each value checked and normalized, nil when nothing is usable
func normalizeThinking(config wire.Value) json.RawMessage {
	if !config.IsObject() {
		return nil
	}
	pick := func(key, alias string) wire.Value {
		if v := config.Get(key); v.Exists() {
			return v
		}
		return config.Get(alias)
	}
	d := &wire.Dict{}
	if budget, ok := pick("thinkingBudget", "thinking_budget").Int(); ok {
		d.SetInt("thinkingBudget", int(budget))
	}
	if include, ok := pick("includeThoughts", "include_thoughts").Bool(); ok {
		d.SetBool("includeThoughts", include)
	}
	if level := strip(pick("thinkingLevel", "thinking_level").Text()); level != "" {
		d.SetString("thinkingLevel", strings.ToLower(level))
	}
	if d.Len() == 0 {
		return nil
	}
	return d.JSON()
}

// Reports whether thinking will spend output tokens, so a small output cap would leave no room for the answer
func thinkingNeedsHeadroom(config json.RawMessage) bool {
	v, err := wire.ParseValue(config)
	if err != nil || !v.IsObject() || v.Get("includeThoughts").False() {
		return false
	}
	budget, hasBudget := v.Get("thinkingBudget").Int()
	return !(hasBudget && budget <= 0 && !v.Get("thinkingLevel").Exists())
}

// Native output cap, the published ceiling for no cap and for an explicit one that thinking would otherwise consume
func effectiveMaxOutputTokens(requested int, thinking json.RawMessage) int {
	if requested <= 0 || thinkingNeedsHeadroom(thinking) {
		return max(requested, DefaultMaxOutputTokens)
	}
	return requested
}

// Output cap raised to the ceiling when the reasoning ask makes a Gemini model spend output tokens on thinking
func RaiseMaxTokens(model string, r *wire.Reasoning, requested int) int {
	config := ThinkingConfig(model, r)
	if config == nil {
		return requested
	}
	return effectiveMaxOutputTokens(requested, normalizeThinking(mustValue(config)))
}

// Value for JSON text this package built itself
func mustValue(raw json.RawMessage) wire.Value {
	v, _ := wire.ParseValue(raw)
	return v
}

// Total output budget a Gemini 4 request gets for an effort above high, false for the per-request cap
func LongDecodeMaxOutputTokens(model string, r *wire.Reasoning) (int, bool) {
	name := strings.TrimPrefix(strings.ToLower(strip(model)), "google/")
	if !strings.HasPrefix(name, "gemini-4") || r == nil || !r.Enabled {
		return 0, false
	}
	switch strings.ToLower(strip(r.Effort)) {
	case "xhigh":
		return 262144, true
	case "max", "ultra":
		return 1048576, true
	}
	return 0, false
}

// Most hops a request with this total output budget can take, one when it fits a single request
func LongDecodeHops(maxOutputTokens int) int {
	return max(1, (max(maxOutputTokens, 0)+hopTokens-1)/hopTokens)
}

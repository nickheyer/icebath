package provider

import (
	"context"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
)

// One request as the hooks of its kind see it, made by the wire sending it
type Call struct {
	Context context.Context
	Route   Route
	// Model id the request sends
	Model string
	// Settings of the provider in the profile, and of the session the request is for
	Settings config.View
	Config   *config.Config
	// Session the request is for, empty for none, and the scope of the provider's caches it shares: the session, a cron run folded into its job
	SessionID string
	Scope     string
	// Reasoning the request asks for, the kind's default when it asks none
	Ask Ask
	// What the provider's listings say of the model's reasoning
	Thinking Thinking
	// The request carries pictures
	Images bool
	// First request of a turn the person started
	UserTurn bool
	// Where the session comes from, such as cli, tui, cron or subagent; empty when not known
	Source string
	// Whether an Ollama server at the route's address reports thinking for the model; false when it reports none or is no Ollama
	OllamaThinks func() bool
	// Context window to ask an Ollama server for, 0 for the server's own
	Window func() int
	// Kind of provider registered under the name
	Kind func(name string) (Spec, bool)
}

// Version of icebath, dev when the build doesn't say
func Version() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		if version := strings.TrimSpace(info.Main.Version); version != "" && version != "(devel)" {
			return strings.TrimPrefix(version, "v")
		}
	}
	return "dev"
}

// User agent of a request from icebath under the product name
func UserAgent(product string) string {
	return product + "/" + Version()
}

// Author icebath's own persona names
const IdentityAuthor = "Nicholas W. Heyer"

// Persona a request that carries no system prompt of its own is sent with where the API wants one
const Identity = "You are Icebath Agent, built by " + IdentityAuthor + ". Be direct: match the length of your reply to the weight of the ask — a " +
	"one-line question gets a one-line answer, and finished work gets a short report of what changed, what's verified, and what's left, never " +
	"a replay of the process. No filler (\"Great question,\" \"I'd be happy to\"), no restating the request back, no re-summarizing what you " +
	"already said, no narrating tool calls the user can see. Plain claims over adjectives; when unsure, say so plainly. Agree because it's " +
	"right, not because the user said it. Depth is earned — give it when the user asks for detail, teaches, or the stakes demand it, not by " +
	"default."

// Attribution headers aggregators show the requests of icebath under
func Attribution() map[string]string {
	return map[string]string{"HTTP-Referer": "https://github.com/nickheyer/icebath", "X-Title": "Icebath Agent"}
}

// Model id as the kind names it
func (s Spec) NativeModel(model string) string {
	if model = strings.TrimSpace(model); model == "" || s.Native == nil {
		return model
	}
	return s.Native(s, model)
}

// Model id without a prefix naming the kind or one of its aliases
func Unprefixed(s Spec, model string) string {
	return StripOwn(model, s.Names()...)
}

// Model id with the vendor an aggregator names it under in front
func Vendored(_ Spec, model string) string {
	return WithVendor(model)
}

// Temperature a request to the kind's model is sent with: none for models that take only their own or refuse one, the kind's fixed one,
// else the one asked for
func (s Spec) Temperature(model string, wanted *float64) *float64 {
	bare := BareModel(model)
	switch {
	case s.OmitTemperature, bare == "kimi" || strings.HasPrefix(bare, "kimi-"), GeminiSamplingDeprecated(bare), ClaudeSamplingForbidden(bare),
		HasPrefix(bare, "gpt-5", "o1", "o3", "o4") && !strings.HasPrefix(bare, "gpt-5-chat"):
		return nil
	case s.FixedTemperature != nil:
		return s.FixedTemperature
	case bare == "trinity-large-thinking":
		fixed := 0.5
		return &fixed
	}
	return wanted
}

// Vendor slugs aggregators prefix model ids with, by the first part of a bare id
var vendors = []struct{ prefix, vendor string }{
	{"claude", "anthropic"}, {"gpt", "openai"}, {"o1", "openai"}, {"o3", "openai"}, {"o4", "openai"},
	{"gemini", "google"}, {"gemma", "google"}, {"deepseek", "deepseek"}, {"glm", "z-ai"}, {"kimi", "moonshotai"},
	{"minimax", "minimax"}, {"grok", "x-ai"}, {"qwen", "qwen"}, {"mimo", "xiaomi"}, {"trinity", "arcee-ai"},
	{"nemotron", "nvidia"}, {"llama", "meta-llama"}, {"step", "stepfun"},
}

// Vendor a model id names or implies, empty when none can be told
func Vendor(model string) string {
	name := strings.TrimSpace(model)
	if vendor, _, ok := strings.Cut(name, "/"); ok {
		return strings.ToLower(vendor)
	}
	lower := strings.ToLower(name)
	first, _, _ := strings.Cut(lower, "-")
	for _, v := range vendors {
		if v.prefix == first {
			return v.vendor
		}
	}
	for _, v := range vendors {
		if strings.HasPrefix(lower, v.prefix) {
			return v.vendor
		}
	}
	return ""
}

// Model id with the vendor an aggregator names it under in front, as it is when it names one or none can be told
func WithVendor(model string) string {
	if strings.Contains(model, "/") {
		return model
	}
	if vendor := Vendor(model); vendor != "" {
		return vendor + "/" + model
	}
	return model
}

// Model id without a provider prefix, slashed or coloned, that names one of the names given
func StripOwn(model string, names ...string) string {
	cut := strings.IndexAny(model, "/:")
	if cut < 0 {
		return model
	}
	prefix, rest := strings.ToLower(strings.TrimSpace(model[:cut])), strings.TrimSpace(model[cut+1:])
	if prefix == "" || rest == "" || !slices.Contains(names, prefix) {
		return model
	}
	return rest
}

// Names a provider prefix of the kind's model ids may use: its own and its aliases
func (s Spec) Names() []string {
	return append([]string{s.Name}, s.Aliases...)
}

// Model id with the vendor prefix it dropped put back, when exactly one id of the catalogue ends in it
func RepairPrefix(model string, catalogue []string) string {
	if strings.Contains(model, "/") {
		return model
	}
	needle := strings.ToLower(strings.TrimSpace(model))
	found := ""
	for _, entry := range catalogue {
		_, bare, ok := strings.Cut(entry, "/")
		if !ok || strings.ToLower(strings.TrimSpace(bare)) != needle || entry == found {
			continue
		}
		if found != "" {
			return model
		}
		found = entry
	}
	if found == "" {
		return model
	}
	return found
}

// Models DashScope's compatible mode keeps replayed reasoning of when asked
var preserving = []string{"qwen3.6", "qwen3.7", "qwen3.8", "kimi-k2.6", "kimi-k2.7"}

// Field asking DashScope's compatible mode to keep the reasoning of the turns replayed to the model: preserve_thinking for the Qwen and
// Kimi models that take it, clear_thinking off for GLM 5
func DashScopeThinking(model string) map[string]any {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case Contains(m, preserving...):
		return map[string]any{"preserve_thinking": true}
	case strings.HasPrefix(m, "glm-5"):
		return map[string]any{"clear_thinking": false}
	}
	return nil
}

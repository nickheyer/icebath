package provider

import (
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/config"
	openrouterconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openrouter/config"
	"github.com/nickheyer/icebath/internal/provider"
)

// Settings of an OpenRouter provider beside its base URL and models
func settings() []config.Setting {
	return []config.Setting{
		{Key: openrouterconfig.ResponseCache, Title: "Reuse answers", About: "Let OpenRouter reuse the answer of an identical earlier request.", Default: true},
		{Key: openrouterconfig.ResponseCacheTTL, Title: "Keep answers for", About: "How long OpenRouter keeps an answer it may reuse.", Default: 300,
			Counts: "seconds", Least: config.Num(1), Most: config.Num(86400), Advanced: true},
		{Key: openrouterconfig.Routing, Title: "Routing", About: "Rules for which providers behind OpenRouter may serve requests.",
			Help:    "Rules are only, ignore and order with lists of providers, sort, require_parameters, data_collection, and models with rules for one model.",
			Default: map[string]any{}, Advanced: true, Entry: &config.Entry{Noun: "rule", Name: "Rule name"}},
		{Key: openrouterconfig.MinCodingScore, Title: "Least coding score", About: "The lowest coding score of a model that the Pareto router may pick.",
			Default: 0.65, Least: config.Num(0), Most: config.Num(1), Advanced: true},
	}
}

// Fields of the routing setting OpenRouter takes
var routingKeys = []string{"only", "ignore", "order", "sort", "require_parameters", "data_collection"}

// Sorts OpenRouter takes for its upstream providers
var sorts = []string{"throughput", "latency", "price"}

// Endpoint a model id pins: the model sent in its place and the upstream providers it may go to
type pin struct {
	base string
	tags []string
}

// Ids of the OpenAI models only some upstream providers serve, with the speed tiers they come in
var pins = func() map[string]pin {
	out := map[string]pin{}
	tiers := map[string][]string{"": {"openai", "azure", "azure/us"}, "-fast": {"openai/fast"}, "-flex": {"openai/flex"}}
	for _, base := range []string{"openai/gpt-6-astra", "openai/gpt-6-astra-pro"} {
		for suffix, tags := range tiers {
			out[base+suffix] = pin{base, tags}
		}
	}
	return out
}()

// Model id prefixes of xAI's Grok on OpenRouter, which keeps a conversation's cache by its id
var grokPrefixes = []string{"x-ai/grok-", "xai/grok-"}

// Claude models that take reasoning as an optional field; every other Claude model always reasons
var optionalReasoning = []string{
	"claude-3", "claude-opus-4-0", "claude-opus-4.0", "claude-opus-4-1", "claude-opus-4.1", "claude-sonnet-4-0", "claude-sonnet-4.0",
	"claude-opus-4-2025", "claude-sonnet-4-2025", "claude-opus-4-5", "claude-opus-4.5", "claude-sonnet-4-5", "claude-sonnet-4.5",
	"claude-haiku-4-5", "claude-haiku-4.5",
}

// Anthropic beta that streams tool arguments as they come, which Claude on OpenRouter is asked for
const toolStreaming = "fine-grained-tool-streaming-2025-05-14"

// Whether a Claude model reasons whatever it is asked
func mandatory(model string) bool {
	m := strings.ToLower(model)
	return (strings.HasPrefix(m, "anthropic/") || strings.Contains(m, "claude")) && !provider.Contains(m, optionalReasoning...)
}

// Value of a setting of the provider, its default for a provider of another kind at OpenRouter's address, which keeps none of them
func setting(call *provider.Call, key string) any {
	if value := call.Settings.Get(key); value != nil {
		return value
	}
	for _, s := range settings() {
		if s.Key == key {
			return s.Default
		}
	}
	return nil
}

// Upstream routing of the request: the routing setting with the model's own entry over it, the sort checked and the empty fields dropped
func preferences(routing map[string]any, model string) map[string]any {
	flat := map[string]any{}
	for _, key := range routingKeys {
		if v, ok := routing[key]; ok {
			flat[key] = v
		}
	}
	if byModel, ok := routing["models"].(map[string]any); ok {
		trimmed := strings.TrimSpace(model)
		variants := []string{trimmed, strings.ToLower(trimmed)}
		if i := strings.LastIndex(trimmed, "/"); i >= 0 {
			variants = append(variants, trimmed[i+1:], strings.ToLower(trimmed[i+1:]))
		}
		for _, variant := range variants {
			if entry, ok := byModel[variant].(map[string]any); ok {
				for key, value := range entry {
					if slices.Contains(routingKeys, key) {
						flat[key] = value
					}
				}
				break
			}
		}
	}
	if sort, ok := flat["sort"].(string); ok {
		value := strings.ToLower(strings.TrimSpace(sort))
		switch {
		case slices.Contains(sorts, value):
			flat["sort"] = value
		case value != "":
			slog.Warn(fmt.Sprintf("Ignoring invalid OpenRouter provider.sort value %s (allowed: latency, price, throughput)", strconv.Quote(sort)))
			delete(flat, "sort")
		default:
			delete(flat, "sort")
		}
	} else {
		delete(flat, "sort")
	}
	if config.Truthy(flat["require_parameters"]) {
		flat["require_parameters"] = true
	} else {
		delete(flat, "require_parameters")
	}
	maps.DeleteFunc(flat, func(_ string, value any) bool { return !config.Truthy(value) })
	return flat
}

// Session the conversation's requests stick to, the routing preferences with a pinned model's upstream providers, and the pareto router's
// coding floor
func extraBody(call *provider.Call) map[string]any {
	body := map[string]any{}
	if call.Scope != "" {
		body["session_id"] = call.Scope
	}
	routing, _ := call.Settings.Get(openrouterconfig.Routing).(map[string]any)
	prefs := preferences(routing, call.Model)
	if p, ok := pins[call.Model]; ok && !(p.base == call.Model && config.Truthy(prefs["only"])) {
		prefs["only"] = slices.Clone(p.tags)
	}
	if len(prefs) > 0 {
		body["provider"] = prefs
	}
	if score := cast.ToFloat64(setting(call, openrouterconfig.MinCodingScore)); call.Model == "openrouter/pareto-code" && score >= 0 && score <= 1 {
		body["plugins"] = []any{map[string]any{"id": "pareto-router", "min_coding_score": score}}
	}
	return body
}

// The model a pinned id stands for, reasoning clamped to what the catalog lists, a mandatory Claude effort as verbosity, and Grok's
// conversation id
func shape(call *provider.Call) (map[string]any, map[string]any) {
	extra, top := map[string]any{}, map[string]any{}
	if p, ok := pins[call.Model]; ok && p.base != call.Model {
		top["model"] = p.base
	}
	if call.Thinking.Takes {
		switch ask := call.Ask; {
		case mandatory(call.Model):
			if !ask.Off && ask.Effort != "" && ask.Effort != "none" {
				top["verbosity"] = ask.Effort
			}
		case ask.Set:
			if reasoning, ok := clamped(ask, call.Model, call.Thinking); ok {
				extra["reasoning"] = reasoning
			}
		default:
			extra["reasoning"] = map[string]any{"enabled": true, "effort": "medium"}
		}
	}
	if call.Scope != "" && provider.HasPrefix(strings.ToLower(call.Model), grokPrefixes...) {
		top["extra_headers"] = map[string]string{"x-grok-conv-id": call.Scope}
	}
	return extra, top
}

// Reasoning object of the ask with its effort clamped to the catalog's for the model; false for turning off a model that can't be
func clamped(ask provider.Ask, model string, thinking provider.Thinking) (map[string]any, bool) {
	off := ask.Off || ask.Effort == "none"
	if ask.Effort == "" && !off || !thinking.Known || !thinking.Takes {
		return ask.Object(), true
	}
	if off {
		if thinking.Mandatory || strings.HasPrefix(model, "openai/") && !slices.Contains(provider.CodexEfforts(model), "none") {
			return nil, false
		}
		return ask.Object(), true
	}
	if level := provider.ClampEffort(ask.Effort, thinking.Efforts, nil); level != "" && level != ask.Effort {
		slog.Debug(fmt.Sprintf("openrouter: clamped reasoning effort %q → %q for %s (catalog supported_efforts=%v)", ask.Effort, level, model,
			thinking.Efforts))
		ask.Effort = level
	}
	return ask.Object(), true
}

// OpenRouter's attribution and categories, its response cache as the settings ask, and the tool streaming beta for Claude
func headers(call *provider.Call) map[string]string {
	h := provider.Attribution()
	h["X-OpenRouter-Categories"] = "productivity,cli-agent"
	if cast.ToBool(setting(call, openrouterconfig.ResponseCache)) {
		h["X-OpenRouter-Cache"] = "true"
		if ttl := cast.ToInt(setting(call, openrouterconfig.ResponseCacheTTL)); ttl >= 1 && ttl <= 86400 {
			h["X-OpenRouter-Cache-TTL"] = strconv.Itoa(ttl)
		}
	}
	if strings.Contains(strings.ToLower(call.Model), "claude") {
		beta := call.Route.Headers["x-anthropic-beta"]
		if !strings.Contains(beta, toolStreaming) {
			h["x-anthropic-beta"] = strings.TrimPrefix(beta+","+toolStreaming, ",")
		}
	}
	return h
}

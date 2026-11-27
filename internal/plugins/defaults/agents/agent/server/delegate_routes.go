package server

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/delegation"
	delegationconfig "github.com/nickheyer/icebath/internal/delegation/config"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	"github.com/nickheyer/icebath/internal/provider"
)

// Providers whose own SDK routes a model, which an endpoint of a subagent's own does not stand in for
var nativeProviders = map[string]bool{"bedrock": true, "vertex": true, "google": true, "google-genai": true}

// Where one task's subagent runs, as its route resolves here
type placement struct {
	// Profile its session runs on, whose own main model, keys, reasoning and fallbacks a profile route takes
	profile    string
	viaProfile bool
	// Provider of that profile and model it runs on, each empty to take what the agent that delegated runs on
	provider string
	model    string
	// Route of an endpoint of its own outside the profile's providers, nil for one of its providers
	fixed *provider.Route
	// How hard it thinks, empty for the parent's level
	effort string
	// Fields its requests carry, a speed tier's among them
	extra map[string]any
	tier  string
	// Models it falls back to, nil for the parent's chain
	fallbacks []string
	// The route names its own provider, endpoint or model, so the parent's fallbacks do not follow it
	pinned bool
	// What the results say it ran on, nil for the default backend's unlabelled route
	report *routed
	// Why the task was refused, its siblings still started
	refusal string
}

// Placements of the tasks from the backend the settings select, each task refused alone when its route is no good; the refusal of the
// whole call when no backend can run
func (a *Agent) place(parent *run, cfg *config.Config, tasks []*task) ([]*placement, string) {
	backend, err := a.delegation.Active(cfg)
	if err != nil {
		return nil, err.Error()
	}
	declared := backend.Declared(cfg)
	cache := map[string]*placement{}
	out := make([]*placement, len(tasks))
	for i, t := range tasks {
		p, err := a.placeOne(parent, cfg, backend, declared, t, cache)
		if err != nil {
			refusal, ok := errors.AsType[*delegation.Refusal](err)
			if !ok {
				refusal = &delegation.Refusal{Message: err.Error()}
			}
			p = &placement{report: &routed{Backend: backend.Name, Refused: true}, refusal: refusal.Message}
		}
		out[i] = p
	}
	return out, ""
}

func (a *Agent) placeOne(parent *run, cfg *config.Config, backend delegation.Backend, declared []delegation.Parameter, t *task,
	cache map[string]*placement) (*placement, error) {
	route, err := routeTask(cfg, backend, declared, t)
	if err != nil {
		return nil, err
	}
	if route.Profile == "" && route.ReasoningEffort != "" && route.ReasoningEffort != strings.TrimSpace(cfg.GetString(delegationconfig.ReasoningEffort)) &&
		!slices.Contains(provider.Efforts, strings.ToLower(route.ReasoningEffort)) {
		return nil, &delegation.Refusal{Message: fmt.Sprintf("Task %d: unknown reasoning_effort %s on this route. Use none or one of: %s.", t.index,
			quoted(route.ReasoningEffort), strings.Join(provider.Efforts[1:], ", "))}
	}
	if route.Profile == "" && route.Fallbacks == nil && cfg.Stored(delegationconfig.Fallbacks) != nil {
		route.Fallbacks = cfg.GetStringSlice(delegationconfig.Fallbacks)
		if route.Fallbacks == nil {
			route.Fallbacks = []string{}
		}
	}
	label := route.Label
	route.Label = ""
	key, _ := json.Marshal(route)
	p, ok := cache[string(key)]
	if !ok {
		if p, err = a.resolveRoute(parent, cfg, route, backend.Name); err != nil {
			return nil, err
		}
		cache[string(key)] = p
	}
	copied := *p
	if label != "" || backend.Name != delegation.Default {
		report := routed{Backend: backend.Name, Label: label}
		copied.report = &report
	}
	return &copied, nil
}

// Route of one task from the backend, its parameters checked against those the backend declares
func routeTask(cfg *config.Config, backend delegation.Backend, declared []delegation.Parameter, t *task) (delegation.Route, error) {
	known := map[string]map[string]any{}
	for _, p := range declared {
		known[p.Name] = p.Schema
	}
	var undeclared []string
	for name, value := range t.raw {
		if _, ok := known[name]; value != nil && !ok && !delegation.Reserved[name] {
			undeclared = append(undeclared, name)
		}
	}
	slices.Sort(undeclared)
	if len(undeclared) > 0 {
		return delegation.Route{}, &delegation.Refusal{Message: undeclaredRefusal(backend, t.index, undeclared)}
	}
	params := map[string]any{}
	for _, name := range slices.Sorted(maps.Keys(known)) {
		value, ok := t.raw[name]
		if !ok || value == nil {
			continue
		}
		if enum, ok := known[name]["enum"].([]any); ok && !slices.ContainsFunc(enum, func(allowed any) bool { return fmt.Sprint(allowed) == fmt.Sprint(value) }) {
			parts := make([]string, len(enum))
			for i, v := range enum {
				parts[i] = fmt.Sprint(v)
			}
			return delegation.Route{}, &delegation.Refusal{Message: fmt.Sprintf("Task %d: %q must be one of %s (got %s).", t.index, name,
				strings.Join(parts, ", "), quoted(fmt.Sprint(value)))}
		}
		params[name] = value
	}
	route, err := func() (route delegation.Route, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		return backend.Route(cfg, delegation.Task{Index: t.index, Goal: t.goal, Context: t.context, Params: params})
	}()
	if err == nil {
		return route, nil
	}
	if refusal, ok := errors.AsType[*delegation.Refusal](err); ok {
		return route, &delegation.Refusal{Message: fmt.Sprintf("Task %d: %s", t.index, refusal.Message)}
	}
	slog.Warn(fmt.Sprintf("delegation provider %s.route_task() raised", backend.Name), "error", err)
	return route, &delegation.Refusal{Message: fmt.Sprintf("Task %d: delegation backend %q failed: %s", t.index, backend.Name, err)}
}

// Why a task carrying parameters the backend does not take was not started, naming the backends that do
func undeclaredRefusal(backend delegation.Backend, index int, names []string) string {
	quotedNames := make([]string, len(names))
	for i, n := range names {
		quotedNames[i] = "'" + n + "'"
	}
	listed := strings.Join(quotedNames, ", ")
	head := fmt.Sprintf("Task %d: the delegation backend is '%s', which does not take %s", index, backend.Name, listed)
	fix := fmt.Sprintf("run `/delegation %s` to make '%s' the profile's backend; the next message rebuilds delegate_task for it", backend.Name, backend.Name)
	return head + ", and no other declares them, so this task was not started. Resend it without " + listed + "; if this session's " +
		"delegate_task tool offers " + listed + ", " + fix + "."
}

// Placement of a route: a profile's own, an endpoint of its own, a provider of the profile, or what the parent runs on
func (a *Agent) resolveRoute(parent *run, cfg *config.Config, route delegation.Route, backend string) (*placement, error) {
	if profile := strings.TrimSpace(route.Profile); profile != "" {
		if !config.Named(profile) || !a.config.Exists(profile) {
			return nil, &delegation.Refusal{Message: fmt.Sprintf("Profile %s does not exist (see `icebath profile list`).", quoted(profile))}
		}
		settings := a.config.In(profile)
		if _, _, err := a.routeModel(settings, "", settings.GetString(agentconfig.Provider), settings.GetString(agentconfig.Model)); err != nil {
			return nil, &delegation.Refusal{Message: fmt.Sprintf("Profile at %s has no usable model provider: %s", a.config.ProfileDir(profile), err)}
		}
		return &placement{profile: profile, viaProfile: true, pinned: true}, nil
	}
	p := &placement{profile: parent.profileName(), model: strings.TrimSpace(route.Model), fallbacks: route.Fallbacks}
	extra := map[string]any{}
	for key, value := range route.RequestOverrides {
		if key == "extra_body" {
			if body, ok := value.(map[string]any); ok {
				maps.Copy(extra, body)
			}
			continue
		}
		extra[key] = value
	}
	name, base := strings.TrimSpace(route.Provider), strings.TrimSpace(route.BaseURL)
	switch {
	case base != "" && !nativeProviders[strings.ToLower(name)]:
		p.fixed = endpointRoute(base, strings.TrimSpace(route.APIKey), strings.ToLower(strings.TrimSpace(route.APIMode)), p.model)
		p.pinned = true
	case name != "":
		settings := a.config.In(p.profile)
		instance, ok := a.provider(settings, name)
		if !ok {
			return nil, &delegation.Refusal{Message: fmt.Sprintf("Cannot resolve delegation provider '%s': this profile has no provider of that name or kind. "+
				"Check that the provider is configured (API key set, valid provider name), or set delegation.base_url/delegation.api_key for a direct "+
				"endpoint. Add it with /providers.", name)}
		}
		if _, err := a.credentials.Route(instance, cmp.Or(p.model, instance.Spec.DefaultModel)); err != nil {
			if instance.Spec.BaseURL == "" && !nativeProviders[strings.ToLower(instance.Spec.Name)] {
				return nil, &delegation.Refusal{Message: fmt.Sprintf("Delegation provider '%s' resolved without a base_url. Refusing to build a subagent "+
					"with an incomplete credential bundle — check the provider's configuration / auth, or set delegation.base_url for a direct endpoint.", name)}
			}
			return nil, &delegation.Refusal{Message: fmt.Sprintf("Delegation provider '%s' resolved but has no API key. Set the appropriate environment "+
				"variable or run 'icebath auth'.", name)}
		}
		p.provider, p.pinned = instance.ID, true
	}
	p.pinned = p.pinned || p.model != ""
	p.effort = strings.ToLower(strings.TrimSpace(route.ReasoningEffort))
	if p.effort != "" && !slices.Contains(provider.Efforts, p.effort) {
		slog.Warn(fmt.Sprintf("Unknown delegation.reasoning_effort '%s', inheriting parent level", p.effort))
		p.effort = ""
	}
	if tier := strings.TrimSpace(route.ServiceTier); tier != "" {
		fields, err := a.tierFields(parent, p, tier)
		if err != nil {
			return nil, err
		}
		for key := range extra {
			if key == "service_tier" || key == "speed" {
				delete(extra, key)
			}
		}
		maps.Copy(extra, fields)
	}
	if len(extra) > 0 {
		p.extra = extra
	}
	return p, nil
}

// Request fields of a task's speed tier on its route, refused for a word that names none or a tier the route does not offer
func (a *Agent) tierFields(parent *run, p *placement, word string) (map[string]any, error) {
	tier, err := provider.Tier(word)
	if err != nil || tier != "" && !provider.StaticTier(tier) {
		return nil, &delegation.Refusal{Message: fmt.Sprintf("Unknown service_tier '%s'. Use normal, fast, or ultrafast.", word)}
	}
	p.tier = tier
	if tier == "" {
		return nil, nil
	}
	model, kind, base := p.model, "", ""
	switch {
	case p.fixed != nil:
		kind, base = p.fixed.Spec, p.fixed.BaseURL
	default:
		instance, route, err := a.parentRoute(parent, p)
		if err == nil {
			kind, base, model = instance.Spec.Name, route.BaseURL, cmp.Or(model, route.Model)
		}
	}
	fields := provider.FastFields(model, kind, base, tier)
	if len(fields) == 0 {
		return nil, &delegation.Refusal{Message: fmt.Sprintf("service_tier '%s' is not available for model '%s' on provider '%s'. Omit service_tier or "+
			"choose a model/provider that offers it.", word, cmp.Or(model, "None"), cmp.Or(cmp.Or(p.provider, kind), "None"))}
	}
	return fields, nil
}

// Instance and route the placement inherits from the parent, for a route naming no endpoint of its own
func (a *Agent) parentRoute(parent *run, p *placement) (provider.Instance, provider.Route, error) {
	if p.provider != "" {
		settings := a.config.In(p.profile)
		return a.routeFor(settings, p.provider, p.model)
	}
	return a.current(parent)
}

// Route of an endpoint of its own: its provider and request format told by its address, an explicit format winning
func endpointRoute(base, key, mode, model string) *provider.Route {
	lower := strings.ToLower(base)
	host := provider.Hostname(base)
	name, detected := "custom", provider.ModeForURL(base)
	switch {
	case host == "chatgpt.com" && strings.Contains(lower, "/backend-api/codex"):
		name, detected = "openai-codex", provider.ModeCodexResponses
	case host == "api.anthropic.com":
		name, detected = "anthropic", provider.ModeAnthropicMessages
	}
	if detected == "" {
		detected = provider.ModeChatCompletions
	}
	if provider.KnownMode(mode) {
		detected = provider.APIMode(mode)
	}
	route := &provider.Route{Provider: name, Spec: name, Model: model, BaseURL: strings.TrimRight(base, "/"), APIKey: key, Mode: detected,
		Auth: provider.AuthAPIKey}
	if key == "" {
		route.Auth = provider.AuthNone
	}
	return route
}

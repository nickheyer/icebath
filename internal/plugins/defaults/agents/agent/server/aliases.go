package server

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	"github.com/nickheyer/icebath/internal/provider"
)

// A short name for a model the profile's model shortcuts set: the model, its provider, and for an endpoint of its own its address and key
type alias struct {
	name     string
	model    string
	provider string
	base     string
	key      string
	keyEnv   string
}

// Text of a field of a shortcut, empty for one unset or empty
func aliasField(value any) string {
	if !config.Truthy(value) {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

// Shortcuts of the profile by lowercased name, in the order of their names: one with no model left out, one with no provider on the
// profile's provider, else on custom
func aliases(settings *config.Config) []alias {
	raw, _ := settings.Get(agentconfig.Aliases).(map[string]any)
	current := strings.TrimSpace(settings.GetString(agentconfig.Provider))
	var out []alias
	at := map[string]int{}
	for _, name := range slices.Sorted(maps.Keys(raw)) {
		key := strings.ToLower(strings.TrimSpace(name))
		value, ok := raw[name].(map[string]any)
		if key == "" || !ok {
			continue
		}
		a := alias{name: key, model: aliasField(value["model"]), provider: aliasField(value["provider"]), base: aliasField(value["base_url"]),
			key: aliasField(value["api_key"]), keyEnv: aliasField(value["key_env"])}
		if a.model == "" {
			continue
		}
		a.provider = cmp.Or(a.provider, current, "custom")
		if i, seen := at[key]; seen {
			out[i] = a
			continue
		}
		at[key] = len(out)
		out = append(out, a)
	}
	return out
}

// Shortcut the model names, false for a model that names none
func lookupAlias(settings *config.Config, model string) (alias, bool) {
	key := strings.ToLower(strings.TrimSpace(model))
	if key == "" {
		return alias{}, false
	}
	for _, a := range aliases(settings) {
		if a.name == key {
			return a, true
		}
	}
	return alias{}, false
}

// Key a shortcut's endpoint takes: a ${VAR} template's variable, else the key it holds, else its key_env's variable, each read from the
// profile's environment
func (a alias) credential(settings *config.Config) string {
	raw := strings.TrimSpace(a.key)
	if len(raw) >= 3 && strings.HasPrefix(raw, "${") && strings.HasSuffix(raw, "}") {
		return strings.TrimSpace(settings.Getenv(strings.TrimSpace(raw[2 : len(raw)-1])))
	}
	if raw != "" {
		return raw
	}
	if name := strings.TrimSpace(a.keyEnv); name != "" {
		return strings.TrimSpace(settings.Getenv(name))
	}
	return ""
}

// Shortcut a model switch lands on: the one of the name, else one whose model the name is, that of the provider in use first
func (a *Agent) switchAlias(settings *config.Config, current provider.Instance, model string) (alias, bool) {
	if found, ok := lookupAlias(settings, model); ok {
		return found, true
	}
	key := strings.ToLower(strings.TrimSpace(model))
	var fallback *alias
	for _, candidate := range aliases(settings) {
		if strings.ToLower(candidate.model) != key {
			continue
		}
		if instance, ok := a.provider(settings, candidate.provider); ok && current.ID != "" && instance.ID == current.ID {
			return candidate, true
		}
		if fallback == nil {
			fallback = &candidate
		}
	}
	if fallback != nil {
		return *fallback, true
	}
	return alias{}, false
}

// Instance and route of the model: a model naming one of the profile's shortcuts runs where it says, on the provider named explicitly when
// one is (the shortcut's address kept), else on an endpoint of its own with its key when it gives an address, else on its provider with its
// key when it gives one; any other model on the provider named, else the profile's
func (a *Agent) routeModel(settings *config.Config, explicit, current, model string) (provider.Instance, provider.Route, error) {
	found, ok := lookupAlias(settings, model)
	if !ok {
		return a.routeFor(settings, cmp.Or(explicit, current), model)
	}
	if explicit != "" {
		instance, route, err := a.routeFor(settings, explicit, found.model)
		if err == nil && found.base != "" {
			route.BaseURL = strings.TrimRight(found.base, "/")
		}
		return instance, route, err
	}
	key := found.credential(settings)
	if found.base != "" {
		route := endpointRoute(found.base, key, "", found.model)
		return provider.Instance{ID: route.Provider, Spec: provider.Spec{Name: route.Spec, BaseURL: route.BaseURL, Mode: route.Mode}, Known: true}, *route, nil
	}
	instance, route, err := a.routeFor(settings, found.provider, found.model)
	if err == nil && key != "" {
		route.APIKey, route.Auth = key, provider.AuthAPIKey
	}
	return instance, route, err
}

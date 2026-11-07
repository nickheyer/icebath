package provider

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

type auxSessionAuxiliaryKey struct{}

// SessionAuxiliaryScope publishes a gateway session's frozen auxiliary section for the turn, a non mapping becomes empty
func SessionAuxiliaryScope(ctx context.Context, auxiliary any) context.Context {
	m, ok := auxiliary.(map[string]any)
	if !ok {
		m = map[string]any{}
	}
	return context.WithValue(ctx, auxSessionAuxiliaryKey{}, m)
}

// Session auxiliary section published on the context, false outside a session scope
func auxSessionAuxiliary(ctx context.Context) (map[string]any, bool) {
	if ctx == nil {
		return nil, false
	}
	m, ok := ctx.Value(auxSessionAuxiliaryKey{}).(map[string]any)
	return m, ok
}

// AuthoritativeAuxiliaryTask is a task name whose config keeps its policy but drops every route field
type AuthoritativeAuxiliaryTask string

// RouteAuthoritative marks the task as naming a complete route of its own
func (AuthoritativeAuxiliaryTask) RouteAuthoritative() bool { return true }

// NewAuthoritativeAuxiliaryTask returns the task as route authoritative, an absent task becoming the empty name
func NewAuthoritativeAuxiliaryTask(task any) AuthoritativeAuxiliaryTask {
	if t, ok := task.(AuthoritativeAuxiliaryTask); ok {
		return t
	}
	return AuthoritativeAuxiliaryTask(auxTaskName(task))
}

// Python str of a task value with a falsy task giving the empty name
func auxTaskName(task any) string {
	switch t := task.(type) {
	case AuthoritativeAuxiliaryTask:
		return string(t)
	case string:
		return t
	}
	if !pyrepr.Truthy(task) {
		return ""
	}
	return pyrepr.Str(task)
}

// Auxiliary task config with plugin defaults under the user block, route fields dropped for an authoritative task
func (r *resolver) auxiliaryTaskConfig(task any, seen map[string]bool) map[string]any {
	name := auxTaskName(task)
	if name == "" {
		return map[string]any{}
	}
	config := r.auxTaskConfig(name, seen)
	if t, ok := task.(interface{ RouteAuthoritative() bool }); ok && t.RouteAuthoritative() {
		policy := make(map[string]any, len(config))
		for k, v := range config {
			if !auxRouteKeys[k] {
				policy[k] = v
			}
		}
		return policy
	}
	return config
}

// The session section or the loaded config block of a task resolved over its plugin defaults and inherit_from base
func (r *resolver) auxTaskConfig(task string, seen map[string]bool) map[string]any {
	aux, scoped := auxSessionAuxiliary(r.ctx)
	if !scoped {
		aux, _ = r.cfg["auxiliary"].(map[string]any)
	}
	taskConfig, ok := aux[task].(map[string]any)
	if !ok {
		taskConfig = map[string]any{}
	}
	for _, entry := range pluginAuxiliaryTasks() {
		if key, isStr := entry["key"].(string); !isStr || key != task {
			continue
		}
		var defaults map[string]any
		if raw := entry["defaults"]; pyrepr.Truthy(raw) {
			m, isMap := raw.(map[string]any)
			if !isMap {
				break
			}
			defaults = m
		}
		inherit := entry["inherit_from"]
		if pyrepr.Truthy(inherit) && seen[task] {
			slog.Default().Warn(fmt.Sprintf("Auxiliary task %s has a circular inherit_from chain — ignoring inheritance", pyrepr.Repr(task)))
		}
		if !pyrepr.Truthy(inherit) || seen[task] {
			merged := copyAnyMap(defaults)
			for k, v := range taskConfig {
				merged[k] = v
			}
			return merged
		}
		next := map[string]bool{task: true}
		for k := range seen {
			next[k] = true
		}
		inherited := copyAnyMap(r.auxiliaryTaskConfig(inherit, next))
		for k, v := range defaults {
			inherited[k] = v
		}
		return layerOverInherited(inherited, taskConfig)
	}
	return taskConfig
}

// Fields that together pick where an auxiliary call goes and travel as one unit
var auxRouteKeys = map[string]bool{
	"provider": true, "model": true, "base_url": true, "api_key": true, "api_mode": true, "key_env": true,
	"api_key_env": true, "reasoning_effort": true,
}

// Exported auxiliary route fields that travel as one unit
var AuxRouteKeys = auxRouteKeys

// Python str of a value or the empty string when the value is falsy
func auxStrOrEmpty(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// Merges a task's own auxiliary block over its inherited base, a pinned route replacing the whole route
func layerOverInherited(inherited, user map[string]any) map[string]any {
	provider := pystr.Lower(pystr.Strip(auxStrOrEmpty(user["provider"])))
	pinned := (provider != "" && provider != "auto") || pystr.Strip(auxStrOrEmpty(user["model"])) != "" ||
		pystr.Strip(auxStrOrEmpty(user["base_url"])) != ""
	merged := map[string]any{}
	for k, v := range inherited {
		if pinned && auxRouteKeys[k] {
			continue
		}
		merged[k] = v
	}
	for key, value := range user {
		if s, ok := value.(string); ok && s == "" && !(pinned && auxRouteKeys[key]) {
			continue
		}
		if key == "provider" && !pinned {
			continue
		}
		merged[key] = value
	}
	return merged
}

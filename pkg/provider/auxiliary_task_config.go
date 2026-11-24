package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
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
	if !anyutil.IsTrue(task) {
		return ""
	}
	return anyutil.String(task)
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

// The auxiliary section task configs merge over: the session's frozen section, else the readonly config of the resolver's home, empty when it cannot be read
func (r *resolver) auxSection() map[string]any {
	if aux, scoped := auxSessionAuxiliary(r.ctx); scoped {
		return aux
	}
	tree, err := auxLoadConfigReadonlyFn(r.hookCtx())
	if err != nil {
		pkgLog().Debug("auxiliary task config: config.yaml unreadable", "error", err)
		return map[string]any{}
	}
	aux, _ := tree["auxiliary"].(map[string]any)
	return aux
}

// The session section or the loaded config block of a task resolved over its plugin defaults and inherit_from base
func (r *resolver) auxTaskConfig(task string, seen map[string]bool) (resolved map[string]any) {
	taskConfig, ok := r.auxSection()[task].(map[string]any)
	if !ok {
		taskConfig = map[string]any{}
	}
	defer func() {
		if failure := recover(); failure != nil {
			pkgLog().Debug(fmt.Sprintf("plugin auxiliary task lookup failed for %s", anyutil.Quote(task)), "error", failure)
			resolved = taskConfig
		}
	}()
	for _, entry := range pluginAuxiliaryTasksFn(r.ctx) {
		if key, isStr := entry["key"].(string); !isStr || key != task {
			continue
		}
		var defaults map[string]any
		if raw := entry["defaults"]; anyutil.IsTrue(raw) {
			m, isMap := raw.(map[string]any)
			if !isMap {
				break
			}
			defaults = m
		}
		inherit := entry["inherit_from"]
		if anyutil.IsTrue(inherit) && seen[task] {
			pkgLog().Warn(fmt.Sprintf("Auxiliary task %s has a circular inherit_from chain — ignoring inheritance", anyutil.Quote(task)))
		}
		if !anyutil.IsTrue(inherit) || seen[task] {
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

// Provider every auxiliary task's own settings name for ctx: each built-in task's provider setting and each plugin task's block, empty ones left out
func auxTaskProviders(ctx context.Context) []string {
	var out []string
	for _, e := range settings.All() {
		key := e.Info().Key
		if !strings.HasPrefix(key, "auxiliary.") {
			continue
		}
		var provider string
		switch h := e.(type) {
		case *settings.String:
			if strings.Count(key, ".") != 2 || !strings.HasSuffix(key, ".provider") {
				continue
			}
			provider = h.Get(ctx)
		case *settings.Map:
			if strings.Count(key, ".") != 1 {
				continue
			}
			provider = auxStrOrEmpty(h.Get(ctx)["provider"])
		default:
			continue
		}
		if provider = strings.TrimSpace(provider); provider != "" {
			out = append(out, provider)
		}
	}
	return out
}

// Fields that together pick where an auxiliary call goes and travel as one unit
var auxRouteKeys = map[string]bool{
	"provider": true, "model": true, "base_url": true, "api_key": true, "api_mode": true, "key_env": true,
	"reasoning_effort": true,
}

// Exported auxiliary route fields that travel as one unit
var AuxRouteKeys = auxRouteKeys

// Python str of a value or the empty string when the value is falsy
func auxStrOrEmpty(v any) string {
	if !anyutil.IsTrue(v) {
		return ""
	}
	return anyutil.String(v)
}

// Merges a task's own auxiliary block over its inherited base, a pinned route replacing the whole route
func layerOverInherited(inherited, user map[string]any) map[string]any {
	provider := strings.ToLower(strings.TrimSpace(auxStrOrEmpty(user["provider"])))
	pinned := (provider != "" && provider != "auto") || strings.TrimSpace(auxStrOrEmpty(user["model"])) != "" ||
		strings.TrimSpace(auxStrOrEmpty(user["base_url"])) != ""
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

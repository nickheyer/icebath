package tools

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var delegationParamNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// How core builds one child, a set Error refusing this task alone while its siblings still run
type TaskRoute struct {
	Creds           map[string]any
	RoutingCfg      map[string]any
	ReasoningEffort any
	ProfileHome     string
	Label           string
	Backend         string
	Error           string
}

// Route a backend routed result carries, nil for the default backend unlabelled route
func (r *TaskRoute) Report() *pyjson.Object {
	if r.Backend == DefaultDelegationBackend && r.Label == "" {
		return nil
	}
	out := pyjson.NewObject()
	out.Set("backend", r.Backend)
	if r.Label != "" {
		out.Set("label", r.Label)
	}
	return out
}

// Result entry for a task this route refused before any child was built
func (r *TaskRoute) RefusedEntry(index int) *pyjson.Object {
	route := r.Report()
	if route == nil {
		route = pyjson.NewObject()
		route.Set("backend", r.Backend)
	}
	route.Set("refused", true)
	out := pyjson.NewObject()
	out.Set("task_index", index)
	out.Set("status", "error")
	out.Set("summary", nil)
	out.Set("error", r.Error)
	out.Set("api_calls", 0)
	out.Set("duration_seconds", 0)
	out.Set("route", route)
	return out
}

func activeDelegationProvider(ctx context.Context, config map[string]any) (DelegationProvider, error) {
	return GetActiveDelegationProvider(ctx, config)
}

// Provider task parameters minus malformed or reserved names, a panicking provider costing only its parameters
func declaredDelegationParameters(p DelegationProvider, config map[string]any) (accepted map[string]map[string]any) {
	defer func() {
		if rec := recover(); rec != nil {
			delegateToolLog.Warn(fmt.Sprintf("delegation provider %s.task_parameters() raised; no extra parameters", p.Name()),
				"exc_info", fmt.Sprint(rec))
			accepted = map[string]map[string]any{}
		}
	}()
	raw := p.TaskParameters(config)
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)
	accepted = map[string]map[string]any{}
	for _, name := range names {
		if !delegationParamNameRE.MatchString(name) || ReservedTaskParameters[name] {
			delegateToolLog.Warn(fmt.Sprintf("delegation provider %s: task parameter %s refused (reserved or invalid name)",
				p.Name(), pyrepr.Repr(name)))
			continue
		}
		if schema := raw[name]; schema != nil {
			accepted[name] = pycopy.Of(schema)
		}
	}
	return accepted
}

// DeclaredDelegationParameters lists a provider's task parameters minus malformed or reserved names
func DeclaredDelegationParameters(p DelegationProvider, config map[string]any) map[string]map[string]any {
	return declaredDelegationParameters(p, config)
}

// Extra task properties and description addendum of the active provider, none when it cannot be resolved
func SchemaExtension(ctx context.Context, config map[string]any) (map[string]map[string]any, string) {
	p, err := activeDelegationProvider(ctx, config)
	if err != nil {
		return map[string]map[string]any{}, ""
	}
	addendum := func() (text string) {
		defer func() {
			if rec := recover(); rec != nil {
				delegateToolLog.Warn(fmt.Sprintf("delegation provider %s.description() raised", p.Name()), "exc_info", fmt.Sprint(rec))
				text = ""
			}
		}()
		return p.Description(config)
	}()
	return declaredDelegationParameters(p, config), pystr.Strip(addendum)
}

// Enum violation text for one provider task parameter, empty when the value is allowed
func delegationParamError(index int, name string, value any, schema map[string]any) string {
	enum, ok := schema["enum"].([]any)
	if !ok {
		return ""
	}
	for _, allowed := range enum {
		if pyrepr.Equal(value, allowed) {
			return ""
		}
	}
	parts := make([]string, len(enum))
	for i, v := range enum {
		parts[i] = pyrepr.Str(v)
	}
	return fmt.Sprintf("Task %d: '%s' must be one of %s (got %s).", index, name, strings.Join(parts, ", "), pyrepr.Repr(value))
}

// DelegationParamError returns the enum violation text of one task parameter, empty when allowed
func DelegationParamError(index int, name string, value any, schema map[string]any) string {
	return delegationParamError(index, name, value, schema)
}

// Route keys of a provider answer, a route error when the answer is not a mapping
func cleanDelegationRoute(providerName string, route any) (map[string]any, error) {
	m, ok := route.(map[string]any)
	if !ok {
		return nil, &DelegationRouteError{Message: fmt.Sprintf("delegation backend '%s' returned no route.", providerName)}
	}
	out := map[string]any{}
	for k, v := range m {
		if RouteKeys[k] {
			out[k] = v
		}
	}
	return out, nil
}

// CleanDelegationRoute keeps the route keys of a provider answer, a route error when it is not a mapping
func CleanDelegationRoute(providerName string, route any) (map[string]any, error) {
	return cleanDelegationRoute(providerName, route)
}

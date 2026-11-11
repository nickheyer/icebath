package dashboard

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	delegationDefaultBackend = "default"
	delegationModelMap       = "model_map"
	delegationProfileRoutes  = "profile_routes"
)

var delegationOptionTextKeys = []string{"name", "description", "provider", "model", "base_url", "api_mode"}

// Effective config of the scoped profile, the hermes_cli.config load_config
func delegationLoadConfig(ctx context.Context) (map[string]any, error) {
	tree, err := config.LoadContext(ctx, home.Dir(ctx), config.EnvRefLookupContext(ctx))
	return map[string]any(tree), err
}

// Saves a config to the scoped profile, the hermes_cli.config save_config
func delegationSaveConfig(ctx context.Context, cfg map[string]any) error {
	return config.SaveConfig(ctx, home.Dir(ctx), cfg, config.SaveOptions{})
}

// 400 with a detail
func delegationBadRequest(detail string) *HTTPException { return NewHTTPException(400, detail) }

// 409 with the managed install reason on one line
func delegationManagedInstall(message string) *HTTPException {
	return NewHTTPException(409, strings.Join(pystr.SplitWhitespace(message, -1), " "))
}

// Dict value of a key in a mapping, empty when either is not a dict
func delegationMapping(parent any, key string) map[string]any {
	m, ok := parent.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return map[string]any{}
}

// Dict value of a key, replaced by a new empty dict when it is not one
func delegationSection(cfg map[string]any, key string) map[string]any {
	if v, ok := cfg[key].(map[string]any); ok {
		return v
	}
	v := map[string]any{}
	cfg[key] = v
	return v
}

// Default plus every plugin registered backend of the scoped profile with availability
func delegationRegisteredBackends(ctx context.Context) []any {
	plugins.DiscoverPlugins(ctx, false)
	out := []any{}
	for _, b := range tools.ListDelegationBackends(ctx) {
		out = append(out, pyjson.ObjectOf("name", b.Name, "label", b.Label, "description", b.Description,
			"available", b.Available, "reason", b.Reason, "builtin", b.Name == tools.DefaultDelegationBackend))
	}
	return out
}

// Copy of a dict entry as an ordered object, false for anything else
func delegationDictCopy(entry any) (*pyjson.Object, bool) {
	o, ok := pyjson.AsObject(entry)
	if !ok {
		return nil, false
	}
	return o.Copy(), true
}

// Model routes as stored with an api key reduced to a display only preview
func delegationRedactedOptions(options any) []any {
	list, ok := options.([]any)
	if !ok {
		return []any{}
	}
	out := []any{}
	for _, entry := range list {
		row, ok := delegationDictCopy(entry)
		if !ok {
			continue
		}
		if pyrepr.Truthy(row.Vals["api_key"]) {
			row.Set("api_key", redactedCredentialPreview(row.Vals["api_key"]))
		}
		out = append(out, row)
	}
	return out
}

// Value when truthy, nil otherwise
func delegationOrNone(v any) any {
	if pyrepr.Truthy(v) {
		return v
	}
	return nil
}

// Delegation backend, its blocked reason, the registered backends, both route blocks and the profile names
func getDelegationRouting(ctx context.Context, profile string) (*pyjson.Object, error) {
	sctx, _, release, err := profileScope(ctx, profile)
	if err != nil {
		return nil, err
	}
	defer release()
	cfg, err := delegationLoadConfig(sctx)
	if err != nil {
		return nil, err
	}
	delegation := delegationMapping(cfg, "delegation")
	backends := delegationRegisteredBackends(sctx)
	configured := tools.ConfiguredBackend(delegation)
	blocks := delegationMapping(delegation, "backends")
	modelMap, routes := delegationMapping(blocks, delegationModelMap), delegationMapping(blocks, delegationProfileRoutes)
	_, reason := tools.DelegationBackendStatus(sctx, configured)
	routeProfiles := []any{}
	if list, ok := routes["profiles"].([]any); ok {
		for _, p := range list {
			if o, isDict := delegationDictCopy(p); isDict {
				routeProfiles = append(routeProfiles, o)
			}
		}
	}
	return pyjson.ObjectOf(
		"configured", configured,
		"blocked_reason", delegationOrNone(reason),
		"backends", backends,
		delegationModelMap, pyjson.ObjectOf("options", delegationRedactedOptions(modelMap["options"]), "default", delegationOrNone(modelMap["default"])),
		delegationProfileRoutes, pyjson.ObjectOf("profiles", routeProfiles, "default", delegationOrNone(routes["default"])),
		"profiles", profiles.ListProfileNames(),
	), nil
}

// Stripped text of a string entry value, empty for anything else
func delegationText(entry *pyjson.Object, key string) string {
	if s, ok := entry.Vals[key].(string); ok {
		return pystr.Strip(s)
	}
	return ""
}

// Stripped default when it names a listed entry, nil when blank and 400 when unknown
func delegationCheckDefault(def any, names []string, what string) (any, error) {
	value := ""
	if s, ok := def.(string); ok {
		value = pystr.Strip(s)
	}
	if value != "" && !slices.Contains(names, value) {
		return nil, delegationBadRequest(fmt.Sprintf("Default %s %s is not in the list.", what, pyrepr.Repr(value)))
	}
	if value == "" {
		return nil, nil
	}
	return value, nil
}

// Whether a route field value counts as unset, None, empty string, list or dict
func delegationEmptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case *pyjson.Object:
		return len(x.Keys) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// Validated model route, a redacted api key replaced by the stored key of the route it came from
func delegationCleanModelOption(index int, entry *pyjson.Object, stored map[string]*pyjson.Object) (*pyjson.Object, error) {
	row := pyjson.NewObject()
	for _, key := range delegationOptionTextKeys {
		if text := delegationText(entry, key); text != "" {
			row.Set(key, text)
		}
	}
	label := fmt.Sprintf("Route %d", index+1)
	name, _ := row.Vals["name"].(string)
	if name == "" {
		return nil, delegationBadRequest(label + " needs a name.")
	}
	label = "Route " + pyrepr.Repr(name)
	if s, _ := row.Vals["description"].(string); s == "" {
		return nil, delegationBadRequest(label + " needs a description: it is what the agent reads to choose it.")
	}
	if s, _ := row.Vals["model"].(string); s == "" {
		return nil, delegationBadRequest(label + " needs a model.")
	}
	providerName, _ := row.Vals["provider"].(string)
	baseURL, _ := row.Vals["base_url"].(string)
	if providerName == "" && baseURL == "" {
		return nil, delegationBadRequest(label + " needs a provider.")
	}
	if effort := delegationText(entry, "reasoning_effort"); effort != "" {
		if config.ParseReasoningEffort(effort) == nil {
			return nil, delegationBadRequest(fmt.Sprintf("%s: unknown reasoning effort %s. Use none or one of: %s.",
				label, pyrepr.Repr(effort), strings.Join(config.ValidReasoningEfforts, ", ")))
		}
		row.Set("reasoning_effort", effort)
	}
	if tier := delegationText(entry, "service_tier"); tier != "" {
		parsed, err := provider.ParseExactServiceTier(tier)
		if err != nil || (parsed != "" && !provider.StaticTiers[parsed]) {
			return nil, delegationBadRequest(fmt.Sprintf("%s: unknown service tier %s. Use normal, fast or ultrafast.", label, pyrepr.Repr(tier)))
		}
		row.Set("service_tier", tier)
	}
	if fallbacks, ok := entry.Vals["fallback_providers"].([]any); ok {
		row.Set("fallback_providers", fallbacks)
	}
	validated := map[string]bool{"api_key": true, "reasoning_effort": true, "service_tier": true, "fallback_providers": true}
	for _, k := range delegationOptionTextKeys {
		validated[k] = true
	}
	var extra []string
	for k := range tools.RouteKeys {
		if !validated[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, field := range extra {
		if v, ok := entry.Vals[field]; ok && !delegationEmptyValue(v) {
			row.Set(field, v)
		}
	}
	key := entry.Vals["api_key"]
	if isRedactedCredentialPreview(key) {
		source := stored[delegationText(entry, "original_name")]
		if source == nil {
			source = stored[name]
		}
		key = nil
		if source != nil {
			key = source.Vals["api_key"]
		}
	}
	if s, ok := key.(string); ok && pystr.Strip(s) != "" {
		row.Set("api_key", pystr.Strip(s))
	}
	return row, nil
}

// Replaces one delegation.backends block in the profile config with what build returns from the current block
func delegationWriteBackendBlock(ctx context.Context, profile, backend string, build func(current map[string]any) (*pyjson.Object, error)) (*pyjson.Object, error) {
	h := home.Dir(ctx)
	if config.ManagedSystem(ctx, h) != "" {
		return nil, delegationManagedInstall(config.FormatManagedMessage(ctx, h, "save delegation.backends."+backend))
	}
	sctx, release, err := configWriteScope(ctx, profile)
	if err != nil {
		return nil, err
	}
	defer release()
	cfg, err := delegationLoadConfig(sctx)
	if err != nil {
		return nil, err
	}
	blocks := delegationSection(delegationSection(cfg, "delegation"), "backends")
	block, err := build(delegationMapping(blocks, backend))
	if err != nil {
		return nil, err
	}
	blocks[backend] = pyjson.Plain(block)
	if err := delegationSaveConfig(sctx, cfg); err != nil {
		return nil, err
	}
	return block, nil
}

// Profile a write targets, the body profile when set and the query profile otherwise
func delegationTargetProfile(body *pyjson.Object, profile string) string {
	if s, ok := body.Vals["profile"].(string); ok && s != "" {
		return s
	}
	return profile
}

// Replaces delegation.backends.model_map after validating every route
func saveModelRoutes(ctx context.Context, body *pyjson.Object, profile string) (*pyjson.Object, error) {
	build := func(current map[string]any) (*pyjson.Object, error) {
		stored := map[string]*pyjson.Object{}
		if list, ok := current["options"].([]any); ok {
			for _, o := range list {
				if obj, isDict := delegationDictCopy(o); isDict {
					stored[delegationText(obj, "name")] = obj
				}
			}
		}
		bodyOptions, _ := body.Vals["options"].([]any)
		options := make([]any, 0, len(bodyOptions))
		names := make([]string, 0, len(bodyOptions))
		for i, o := range bodyOptions {
			entry, _ := delegationDictCopy(o)
			row, err := delegationCleanModelOption(i, entry, stored)
			if err != nil {
				return nil, err
			}
			options = append(options, row)
			names = append(names, row.Vals["name"].(string))
		}
		for _, n := range names {
			count := 0
			for _, m := range names {
				if m == n {
					count++
				}
			}
			if count > 1 {
				return nil, delegationBadRequest(fmt.Sprintf("Two routes are named %s; route names must be unique.", pyrepr.Repr(n)))
			}
		}
		block := pyjson.ObjectOf("options", options)
		def, err := delegationCheckDefault(body.Vals["default"], names, "route")
		if err != nil {
			return nil, err
		}
		if def != nil {
			block.Set("default", def)
		}
		return block, nil
	}
	block, err := delegationWriteBackendBlock(ctx, delegationTargetProfile(body, profile), delegationModelMap, build)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", true, delegationModelMap, pyjson.ObjectOf("options", delegationRedactedOptions(block.Vals["options"]),
		"default", block.Vals["default"])), nil
}

// Replaces delegation.backends.profile_routes, every name an existing profile listed once
func saveProfileRoutes(ctx context.Context, body *pyjson.Object, profile string) (*pyjson.Object, error) {
	build := func(map[string]any) (*pyjson.Object, error) {
		known := map[string]bool{}
		for _, n := range profiles.ListProfileNames() {
			known[n] = true
		}
		routeProfiles := []any{}
		var names []string
		bodyProfiles, _ := body.Vals["profiles"].([]any)
		for _, e := range bodyProfiles {
			entry, _ := delegationDictCopy(e)
			name := delegationText(entry, "name")
			if name == "" {
				return nil, delegationBadRequest("A profile route needs a profile name.")
			}
			if !known[name] {
				return nil, delegationBadRequest(fmt.Sprintf("Profile %s does not exist.", pyrepr.Repr(name)))
			}
			if slices.Contains(names, name) {
				return nil, delegationBadRequest(fmt.Sprintf("Profile %s is listed twice.", pyrepr.Repr(name)))
			}
			names = append(names, name)
			row := pyjson.ObjectOf("name", name)
			if description := delegationText(entry, "description"); description != "" {
				row.Set("description", description)
			}
			routeProfiles = append(routeProfiles, row)
		}
		block := pyjson.ObjectOf("profiles", routeProfiles)
		def, err := delegationCheckDefault(body.Vals["default"], names, "profile")
		if err != nil {
			return nil, err
		}
		if def != nil {
			block.Set("default", def)
		}
		return block, nil
	}
	block, err := delegationWriteBackendBlock(ctx, delegationTargetProfile(body, profile), delegationProfileRoutes, build)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", true, delegationProfileRoutes, pyjson.ObjectOf("profiles", block.Vals["profiles"],
		"default", block.Vals["default"])), nil
}

// Persists delegation.backend through the core switch, 409 for a managed install and 400 for a backend that cannot run
func selectDelegationBackend(ctx context.Context, body *pyjson.Object, profile string) (*pyjson.Object, error) {
	raw, _ := body.Vals["backend"].(string)
	name := pystr.Lower(pystr.Strip(raw))
	if name == "" {
		name = delegationDefaultBackend
	}
	sctx, release, err := configWriteScope(ctx, delegationTargetProfile(body, profile))
	if err != nil {
		return nil, err
	}
	defer release()
	plugins.DiscoverPlugins(sctx, false)
	if err := tools.SetGlobalBackend(sctx, name, "", false); err != nil {
		var de *tools.DelegationBackendError
		if errors.As(err, &de) {
			switch de.Kind {
			case "PermissionError":
				return nil, delegationManagedInstall(de.Message)
			case "ValueError":
				return nil, delegationBadRequest(de.Message)
			}
		}
		return nil, err
	}
	return pyjson.ObjectOf("ok", true, "backend", name), nil
}

// Router of the delegation backend picker and the plugin backend route blocks
func delegationRouter() *APIRouter {
	r := NewAPIRouter("")
	profileParam := QueryParam("profile", Optional(Str), nil)
	r.Get("/api/delegation/routing", &Endpoint{
		Name: "get_delegation_routing",
		Doc: `The profile's delegation backend (` + "``configured``" + `) and, when it cannot run here, why
    (` + "``blocked_reason``, ``None``" + ` when it runs: core fails closed, so subagents are refused until it is fixed), the registered
    backends, both route blocks and the profiles a profile route may name.`,
		Params: []Param{profileParam},
		Handle: func(ctx context.Context, c *Call) (any, error) { return getDelegationRouting(ctx, c.Str("profile")) },
	})
	r.Put("/api/delegation/backend", &Endpoint{
		Name: "select_delegation_backend",
		Doc: `Persist ` + "``delegation.backend``" + ` through core's ` + "``set_global_backend``" + `: only ` + "``default``" + ` or a backend
    that can run for this profile (core fails closed on any other, refusing every subagent).`,
		Params: []Param{profileParam},
		Body:   []BodyParam{Body("body", delegationBackendSelectModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return selectDelegationBackend(ctx, c.Object("body"), c.Str("profile"))
		},
	})
	r.Put("/api/delegation/backends/model_map", &Endpoint{
		Name:   "save_model_routes",
		Doc:    "Replace ``delegation.backends.model_map`` (``options`` + ``default``) after validating every route.",
		Params: []Param{profileParam},
		Body:   []BodyParam{Body("body", delegationModelMapUpdateModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return saveModelRoutes(ctx, c.Object("body"), c.Str("profile"))
		},
	})
	r.Put("/api/delegation/backends/profile_routes", &Endpoint{
		Name: "save_profile_routes",
		Doc: `Replace ` + "``delegation.backends.profile_routes`` (``profiles`` + ``default``)" + `; every name must be an
    existing profile.`,
		Params: []Param{profileParam},
		Body:   []BodyParam{Body("body", delegationProfileRoutesUpdateModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return saveProfileRoutes(ctx, c.Object("body"), c.Str("profile"))
		},
	})
	return r
}

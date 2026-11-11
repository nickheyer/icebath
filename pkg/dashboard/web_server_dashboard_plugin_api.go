package dashboard

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Why a plugin's backend api must not run, empty when it may
func pluginAPIMountSkipReason(plugin *pyjson.Object, enabled, disabled map[string]bool) string {
	source := plugin.GetOr("source", nil)
	name, _ := plugin.GetOr("name", "").(string)
	if (source == "user" || source == "bundled") && disabled[name] {
		return "explicitly disabled"
	}
	if source == "user" && !enabled[name] {
		return "not in plugins.enabled"
	}
	return ""
}

// Enabled and disabled plugin sets of the active profile scope, empty when its config is unreadable
func pluginEnableSetsHere(ctx context.Context) (map[string]bool, map[string]bool) {
	cfg, err := delegationLoadConfig(ctx)
	if err != nil {
		cfg = map[string]any{}
	}
	sets := [2]map[string]bool{{}, {}}
	for i, key := range []string{"enabled", "disabled"} {
		v, ok := config.CfgGet(cfg, "plugins", key)
		if !ok {
			continue
		}
		if list, ok := v.([]any); ok {
			for _, item := range list {
				if s, ok := item.(string); ok {
					sets[i][s] = true
				} else {
					sets[i][pyStrOf(item)] = true
				}
			}
		}
	}
	return sets[0], sets[1]
}

// True when the active scope's consent bars a request from the plugin's backend or assets
func pluginConsentWithheld(ctx context.Context, plugin *pyjson.Object) bool {
	enabled, disabled := pluginEnableSetsHere(ctx)
	return pluginAPIMountSkipReason(plugin, enabled, disabled) != ""
}

// Plugin name of an api plugins request path, empty for any other path
func pluginAPIName(scope *Scope) string {
	path := getRoutePath(scope)
	parts := strings.Split(path, "/")
	if strings.HasPrefix(path, "/api/plugins/") && len(parts) >= 4 {
		return parts[3]
	}
	return ""
}

// Runtime consent gate of one plugin api request, an unknown name counting as a user plugin
func pluginAPIRequestRefused(ctx context.Context, name string) bool {
	plugin := namedPlugin(getDashboardPlugins(ctx, false), name)
	if h, ok := secondaryProfileHome(ctx); plugin == nil && ok {
		plugin = rescannedPlugin(h, name)
	}
	if plugin == nil {
		plugin = pyjson.ObjectOf("name", name, "source", "user")
	}
	return pluginConsentWithheld(ctx, plugin)
}

// Context bound to a plugin request's profile selection with its release, an unknown profile leaving it unscoped
func pluginRequestScope(ctx context.Context, scope *Scope) (context.Context, func(), error) {
	profile := ParseQueryParams(scope.QueryString).Get("profile")
	if isCurrentProfile(profile) {
		return ctx, func() {}, nil
	}
	sctx, _, err := configProfileScope(ctx, profile)
	var he *HTTPException
	if errors.As(err, &he) {
		webServerLog().Debug(fmt.Sprintf("Plugin request for profile %s left unscoped: %v", pyQuoteOptional(profile, true), he.Detail))
		return ctx, func() {}, nil
	}
	if err != nil {
		return ctx, func() {}, err
	}
	return sctx, func() {}, nil
}

// Resolved backend file of a discovered plugin entry, empty when it has none
func pluginAPIPath(plugin *pyjson.Object) (string, error) {
	if plugin == nil {
		return "", nil
	}
	apiFile, _ := plugin.GetOr("_api_file", nil).(string)
	if apiFile == "" {
		return "", nil
	}
	dir, _ := plugin.GetOr("_dir", "").(string)
	return pypath.Resolve(filepath.Join(dir, apiFile))
}

// Active secondary profile's own entry for name, nil in the launch scope
func secondaryPlugin(ctx context.Context, name string) *pyjson.Object {
	h, ok := secondaryProfileHome(ctx)
	if !ok {
		return nil
	}
	return namedPlugin(profileDashboardPlugins(h, false), name)
}

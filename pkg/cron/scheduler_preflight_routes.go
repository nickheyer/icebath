package cron

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Preflight runs unless cron.preflight is literally false
func (s *Store) cronPreflightEnabled(cfg config.Tree) bool {
	cronCfg, ok := cfg["cron"].(map[string]any)
	if !ok {
		return true
	}
	v, present := cronCfg["preflight"]
	return !present || v != false
}

// Enabled primary gateway profile routes that target the served profile, none when this is the primary home
func primaryProfileRoutesForCurrentHome(ctx context.Context) []*gateway.ProfileRoute {
	primary, errP := pypath.Realpath(pypath.Expanduser(home.Root()), pypath.Lax)
	current, errC := pypath.Realpath(pypath.Expanduser(home.Dir(ctx)), pypath.Lax)
	if errP != nil || errC != nil || primary == current {
		return nil
	}
	layered, err := gateway.ReadYAMLLayers(ctx, pypath.Expanduser(home.Root()))
	if err != nil {
		return nil
	}
	routesRaw, present := layered["profile_routes"]
	if (!present || routesRaw == nil) && layered["gateway"] != nil {
		if gw, ok := layered["gateway"].(map[string]any); ok {
			routesRaw = gw["profile_routes"]
		}
	}
	if _, ok := routesRaw.([]any); !ok {
		return nil
	}
	routes, err := gateway.ParseProfileRoutes(ctx, routesRaw)
	if err != nil {
		return nil
	}
	var out []*gateway.ProfileRoute
	for _, r := range routes {
		if pyrepr.Truthy(r.Enabled) && profiles.ProfileMatchesHome(ctx, r.Profile, home.Dir(ctx)) {
			out = append(out, r)
		}
	}
	return out
}

// Read only adapter map of a credentialless satellite, resolving a primary adapter only for a route exact target
type SharedRouteAdapters struct {
	primary map[string]any
	routes  []*gateway.ProfileRoute
}

func newSharedRouteAdapters(adapters any, routes []*gateway.ProfileRoute) *SharedRouteAdapters {
	primary := map[string]any{}
	if m, ok := adapters.(map[string]any); ok {
		for k, v := range m {
			primary[k] = v
		}
	}
	if m, ok := adapters.(*pydict.Dict[gateway.Platform, gateway.PlatformAdapter]); ok && m != nil {
		for _, platform := range m.Keys() {
			adapter, _ := m.Get(platform)
			primary[string(platform)] = adapter
		}
	}
	return &SharedRouteAdapters{primary: primary, routes: append([]*gateway.ProfileRoute(nil), routes...)}
}

// True only while both a primary adapter and a route exist
func (a *SharedRouteAdapters) PyBool() bool { return len(a.primary) > 0 && len(a.routes) > 0 }

// Primary adapter for the platform when a target exact route accepts the target, else the default
func (a *SharedRouteAdapters) Get(ctx context.Context, platform string, target map[string]any, def any) any {
	if len(target) == 0 {
		return def
	}
	adapter, ok := a.primary[platform]
	if !ok || adapter == nil {
		return def
	}
	key := strings.ToLower(platform)
	var chatID, threadID *string
	if pyTruthy(target["chat_id"]) {
		if c := pyrepr.Str(target["chat_id"]); c != "" {
			chatID = &c
		}
	}
	if pyTruthy(target["thread_id"]) {
		t := pyrepr.Str(target["thread_id"])
		threadID = &t
	}
	for _, r := range a.routes {
		routePlatform := pyrepr.Str(r.Platform)
		if strings.ToLower(routePlatform) != key {
			continue
		}
		if !(nonEmptyPtr(r.ChatID) || nonEmptyPtr(r.ThreadID)) {
			continue
		}
		matched, err := r.Matches(ctx, gateway.RouteQuery{Platform: routePlatform, GuildID: r.GuildID, ChatID: chatID, ThreadID: threadID})
		if err == nil && matched {
			return adapter
		}
	}
	return def
}

func nonEmptyPtr(p *string) bool { return p != nil && *p != "" }

// True when the profile has a bot of its own, a local only adapter map holds no platform credential
func hasCredentialPlatform(adapters any, owned any) bool {
	if pyTruthy(owned) {
		return true
	}
	if m, ok := adapters.(map[string]any); ok {
		for platform := range m {
			if platform != "local" {
				return true
			}
		}
	}
	if m, ok := adapters.(*pydict.Dict[gateway.Platform, gateway.PlatformAdapter]); ok {
		for _, platform := range m.Keys() {
			if platform != gateway.PlatformLocal {
				return true
			}
		}
	}
	if m, ok := adapters.(map[gateway.Platform]gateway.PlatformAdapter); ok {
		for platform := range m {
			if platform != gateway.PlatformLocal {
				return true
			}
		}
	}
	return false
}

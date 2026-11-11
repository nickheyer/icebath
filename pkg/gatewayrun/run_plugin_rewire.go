package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Adapter whose plugin registered command and callback handlers can be installed again
type pluginRewirer interface {
	RewirePluginHandlers(ctx context.Context)
}

// Subscribes once per plugin manager scope so every loaded event re-wires that profile's adapters on the loop
func (r *GatewayRunner) subscribePluginRewire(manager *plugins.Manager, profileName, profileHome string) error {
	r.pluginRewireMu.Lock()
	defer r.pluginRewireMu.Unlock()
	if r.pluginRewireUnsubscribe == nil {
		r.pluginRewireUnsubscribe = map[string]func(){}
	}
	scope := manager.ScopeKey()
	if scope == "" {
		scope = fmt.Sprintf("%p", manager)
	}
	if _, ok := r.pluginRewireUnsubscribe[scope]; ok {
		return nil
	}
	loop := r.gatewayLoop
	unsubscribe, err := manager.OnPluginLoaded(func([]map[string]any) {
		if loop == nil {
			return
		}
		_ = loop.CallSoonThreadsafe(func() { r.rewirePluginHandlers(context.Background(), profileName, profileHome) })
	})
	if err != nil {
		return err
	}
	r.pluginRewireUnsubscribe[scope] = unsubscribe
	return nil
}

// Re-wires plugin handlers on every live adapter of one profile, the launch profile for an empty name
func (r *GatewayRunner) rewirePluginHandlers(ctx context.Context, profileName, profileHome string) int {
	adapters := r.adapters
	if profileName != "" {
		adapters, _ = r.profileAdapters.Get(profileName)
	}
	if adapters.Len() == 0 {
		return 0
	}
	scoped := ctx
	if profileHome != "" {
		scoped = gateway.ProfileRuntimeScope(ctx, profileHome, false)
	}
	count := 0
	for platform, adapter := range pydict.Merge(adapters).All() {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Warn(fmt.Sprintf("[%s] plugin handler re-wire failed", platform), "exc_info", pyrepr.Str(rec))
				}
			}()
			rewirer, ok := adapter.(pluginRewirer)
			if !ok {
				panic(fmt.Sprintf("'%s' object has no attribute 'rewire_plugin_handlers'", pyrepr.TypeName(adapter)))
			}
			rewirer.RewirePluginHandlers(scoped)
			count++
		}()
	}
	suffix := ""
	if profileName != "" {
		suffix = fmt.Sprintf(" for profile '%s'", profileName)
	}
	runLog().Info(fmt.Sprintf("Re-wired plugin handlers on %d adapter(s)%s", count, suffix))
	return count
}

// Live adapters of a profile, the launch profile for an empty name
func (r *GatewayRunner) countAdapters(profileName string) int {
	if profileName == "" {
		return r.adapters.Len()
	}
	adapters, _ := r.profileAdapters.Get(profileName)
	return adapters.Len()
}

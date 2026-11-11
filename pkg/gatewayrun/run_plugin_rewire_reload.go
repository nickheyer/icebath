package gatewayrun

import (
	"context"
	"fmt"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Wait for a just enabled plugin's MCP servers, inside the caller's 30 s socket timeout
const mcpConnectWait = 10 * time.Second

// Wait for the adapter count read on the gateway loop after the re-wire callback
const reloadCountWait = 5 * time.Second

// Control socket reload-plugins handler forcing re-discovery under a served home and reporting what is active now
func reloadPluginsVerb(ctx context.Context, r *GatewayRunner, loop gateway.EventLoop) gateway.ControlVerbHandler {
	return func(params map[string]any) (any, error) {
		gatewayHome := home.Dir(ctx)
		requestedText := gatewayHome
		if raw := params["home"]; pyrepr.Truthy(raw) {
			requestedText = pyrepr.Str(raw)
		}
		requested := pypath.Expanduser(pypath.PathStr(requestedText))
		requestedKey := home.Key(requested)
		profileName := ""
		if requestedKey != home.Key(gatewayHome) {
			found := false
			if r.servedProfileHomes != nil {
				for name, servedHome := range r.servedProfileHomes.All() {
					if home.Key(servedHome) == requestedKey {
						profileName, found = name, true
						break
					}
				}
			}
			if !found {
				return map[string]any{"reloaded": false, "error": "home is not served by this gateway", "home": requested}, nil
			}
			primary := r.primaryProfileName
			if primary == "" {
				primary = "default"
			}
			if profileName == primary {
				profileName = ""
			}
		}
		scoped := ctx
		if profileName != "" {
			scoped = gateway.ProfileRuntimeScope(ctx, requested, false)
		}
		plugins.DiscoverPlugins(scoped, true)
		manager := plugins.GetPluginManager(scoped)
		names := manager.PluginKeys()
		activations := append(plugins.ActivationSummaries(scoped, manager), plugins.DependencyBlockedSummaries(scoped, manager)...)
		reconcilePluginMCPServers(ctx, r, requested)
		var rewired any
		if count, ok := countAdaptersOnLoop(r, loop, profileName); ok {
			rewired = count
		}
		return map[string]any{"reloaded": true, "home": requested, "plugins": names, "activations": activations, "adapters_rewired": rewired}, nil
	}
}

// Profile adapter count read on the gateway loop after queued re-wires, false when the loop misses the wait
func countAdaptersOnLoop(r *GatewayRunner, loop gateway.EventLoop, profileName string) (int, bool) {
	if loop == nil {
		return 0, false
	}
	answer := make(chan int, 1)
	if err := loop.CallSoonThreadsafe(func() { answer <- r.countAdapters(profileName) }); err != nil {
		return 0, false
	}
	select {
	case n := <-answer:
		return n, true
	case <-time.After(reloadCountWait):
		return 0, false
	}
}

// Connects the MCP servers a just enabled plugin declares and drops a disabled one's, waiting at most the connect window
func reconcilePluginMCPServers(ctx context.Context, r *GatewayRunner, profileHome string) {
	multiplex := r.multiplexOn()
	done := make(chan struct{})
	base := context.WithoutCancel(ctx)
	go func() {
		defer close(done)
		scope := standaloneLaunchScope(base)
		if multiplex {
			scope = gateway.ProfileRuntimeScope(base, profileHome, true)
		}
		result, err := func() (result mcp.ReconcileResult, err error) {
			defer func() {
				if rec := recover(); rec != nil {
					err = fmt.Errorf("%v", rec)
				}
			}()
			return mcp.Reconcile(mcp.SuppressInteractiveOAuth(scope), nil)
		}()
		if err != nil {
			runLog().Warn(fmt.Sprintf("MCP reconcile after reload-plugins failed for %s", profileHome), "exc_info", err)
			return
		}
		if len(result.Removed) > 0 || len(result.Added) > 0 {
			runLog().Info(fmt.Sprintf("MCP servers reconciled after reload-plugins (%s): removed=%s added=%s",
				profileHome, pyrepr.Repr(stringsAny(result.Removed)), pyrepr.Repr(stringsAny(result.Added))))
		}
	}()
	select {
	case <-done:
	case <-time.After(mcpConnectWait):
	}
}

// Python list form of a string slice
func stringsAny(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

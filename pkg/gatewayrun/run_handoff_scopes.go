package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Profile store the handoff watcher polls, a nil name and empty home meaning the root store
type handoffScope struct {
	name *string
	home string
}

// Stores the handoff watcher polls, the root first then each secondary profile, the root alone when resolution fails
func handoffWatchScopes(ctx context.Context, r *GatewayRunner) []handoffScope {
	scopes := []handoffScope{{}}
	if r.config == nil || r.config.MultiplexProfiles == nil || !*r.config.MultiplexProfiles {
		return scopes
	}
	served, err := func() (served []handoffScope, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%v", rec)
			}
		}()
		homes, err := gateway.MultiplexProfileHomes(ctx, r.config)
		if err != nil {
			return nil, err
		}
		for _, p := range homes {
			if p.Home == "" || p.Name == "" || p.Name == "default" {
				continue
			}
			name := p.Name
			served = append(served, handoffScope{name: &name, home: p.Home})
		}
		return served, nil
	}()
	if err != nil {
		runLog().Debug("Could not resolve multiplex homes for handoff watcher", "exc_info", err)
		return scopes
	}
	return append(scopes, served...)
}

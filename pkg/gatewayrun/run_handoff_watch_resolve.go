package gatewayrun

import "context"

// Handoff watch scopes of a watcher tick, the root poll alone without multiplexing
func (r *GatewayRunner) resolveHandoffWatchScopes(ctx context.Context) ([]handoffScope, error) {
	if r.config != nil && (r.config.MultiplexProfiles == nil || !*r.config.MultiplexProfiles) {
		return []handoffScope{{}}, nil
	}
	out, err := r.runInExecutorWithContext(ctx, func(c context.Context) (any, error) { return handoffWatchScopes(c, r), nil })
	if err != nil {
		return nil, err
	}
	return out.([]handoffScope), nil
}

package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Every home a heartbeat origin can resolve to, the gateway home then the whole served set under multiplexing
func watchedHeartbeatHomes(ctx context.Context, r *GatewayRunner, defaultHome string) ([]string, error) {
	homes := []string{defaultHome}
	if r.multiplexOn() {
		served, err := gateway.MultiplexProfileHomes(ctx, r.config)
		if err != nil {
			return nil, err
		}
		for _, p := range served {
			homes = append(homes, p.Home)
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, h := range homes {
		key := pypath.PurePathStr(h)
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out, nil
}

// Restored heartbeat watch candidate of one session
type heartbeatCandidate struct {
	key       string
	source    *gateway.SessionSource
	sessionID string
}

// Scope key of a heartbeat restore group
type heartbeatScopeKey struct {
	home  string
	keyed bool
}

// Sessions of the routing index whose heartbeat is active, grouped and read under each origin's profile scope
func scanHeartbeatWatches(ctx context.Context, r *GatewayRunner) ([]heartbeatCandidate, error) {
	store := r.sessionStore
	var restored []heartbeatCandidate
	routingHome := store.RoutingHome()
	if routingHome == "" {
		routingHome = home.Dir(ctx)
	}
	homes, err := watchedHeartbeatHomes(ctx, r, routingHome)
	if err != nil {
		return nil, err
	}
	active := false
	for _, h := range homes {
		if profileHasActiveHeartbeat(ctx, h) {
			active = true
			break
		}
	}
	if !active {
		return restored, nil
	}
	scoped := gateway.ProfileRuntimeScope(ctx, routingHome, true)
	type member struct {
		entry  *gateway.SessionEntry
		source *gateway.SessionSource
	}
	var order []heartbeatScopeKey
	groups := map[heartbeatScopeKey][]member{}
	for _, entry := range store.ListSessions(scoped, nil) {
		if entry.Origin == nil || entry.SessionID == "" || entry.Suspended {
			continue
		}
		source, err := r.restoredSource(scoped, entry)
		if err == nil {
			var h string
			var keyed bool
			h, keyed, err = r.profileScopeKeyForSource(scoped, source)
			if err == nil {
				k := heartbeatScopeKey{home: h, keyed: keyed}
				if _, seen := groups[k]; !seen {
					order = append(order, k)
				}
				groups[k] = append(groups[k], member{entry: entry, source: source})
			}
		}
		if err != nil {
			runLog().Debug(fmt.Sprintf("heartbeat restore for %s failed", entry.SessionKey), "exc_info", err)
		}
	}
	for _, k := range order {
		group := groups[k]
		groupCtx, err := r.profileScopeForSource(scoped, group[0].source)
		if err != nil {
			runLog().Debug(fmt.Sprintf("heartbeat restore scope for %s failed", group[0].entry.SessionKey), "exc_info", err)
			continue
		}
		for _, m := range group {
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						runLog().Debug(fmt.Sprintf("heartbeat restore for %s failed", m.entry.SessionKey), "error", rec)
					}
				}()
				if agent.NewHeartbeatManager(groupCtx, m.entry.SessionID).IsActive() {
					restored = append(restored, heartbeatCandidate{key: m.entry.SessionKey, source: m.source, sessionID: m.entry.SessionID})
				}
			}()
		}
	}
	return restored, nil
}

// Recovers heartbeat watches from the persisted routing index, a failed scan retried on the next poll
func restoreHeartbeatWatches(ctx context.Context, r *GatewayRunner) error {
	out, err := r.runInExecutorWithContext(ctx, func(c context.Context) (any, error) {
		return scanHeartbeatWatches(c, r)
	})
	if err != nil {
		if isCancellation(err) || ctx.Err() != nil {
			return err
		}
		runLog().Debug("heartbeat restore scan failed; retrying on next poll", "exc_info", err)
		return nil
	}
	candidates, _ := out.([]heartbeatCandidate)
	for _, c := range candidates {
		if current, ok := r.sessionStore.PeekSessionID(c.key); ok && current == c.sessionID {
			r.registerHeartbeatWatch(ctx, c.key, c.source, c.sessionID)
		}
	}
	return nil
}

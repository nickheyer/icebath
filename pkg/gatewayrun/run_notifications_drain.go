package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Profile runtime scope of the session a completion event targets, the launch scope for an unrouted event
func (r *GatewayRunner) completionEventScope(ctx context.Context, evt map[string]any) (context.Context, error) {
	source, err := r.buildProcessEventSource(ctx, evt)
	if err != nil {
		return nil, err
	}
	if source == nil || source.Profile == nil || *source.Profile == "" {
		rawSID := rawProcessEventSessionID(evt)
		if rawSID != "" && r.config != nil && r.config.MultiplexProfiles != nil && *r.config.MultiplexProfiles {
			return r.servedAPIServerEventScope(ctx, evt, rawSID)
		}
		return provider.WithLaunchProfileScopeIfMultiplexed(ctx), nil
	}
	profileHome, err := r.resolveProfileHomeForSource(ctx, source)
	if err != nil {
		return nil, err
	}
	if home.Override(ctx) == profileHome {
		return ctx, nil
	}
	return gateway.ProfileRuntimeScope(ctx, profileHome, true), nil
}

// Scope of the served profile whose own store holds a raw api_server session, else the launch profile's
func (r *GatewayRunner) servedAPIServerEventScope(ctx context.Context, evt map[string]any, rawSID string) (context.Context, error) {
	served, err := r.servedAPIServerWakeProfile(ctx, evt, rawSID)
	if err != nil {
		return nil, err
	}
	if served == "" {
		return provider.WithLaunchProfileScopeIfMultiplexed(ctx), nil
	}
	profile := served
	profileHome, err := r.resolveProfileHomeForSource(ctx, &gateway.SessionSource{Platform: gateway.PlatformAPIServer, ChatID: rawSID, Profile: &profile})
	if err != nil {
		return nil, err
	}
	return gateway.ProfileRuntimeScope(ctx, profileHome, true), nil
}

// Drains queued watch events and injects each one its owning profile lets surface, requeueing refusals on backoff
func (r *GatewayRunner) drainWatchNotifications(ctx context.Context, queue *tools.EventQueue) error {
	for _, evt := range drainGatewayWatchEvents(queue) {
		if next, ok := evt["_watch_next_retry_at"].(float64); ok && next > pythread.Monotonic() {
			queue.Put(evt)
			continue
		}
		delivered, skip, err := r.deliverWatchEvent(ctx, evt)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		if delivered != nil && !*delivered {
			requeueRefusedWatchEvent(queue, evt)
		}
	}
	return nil
}

// One watch event delivered inside its profile scope, skip when policy or formatting drops it
func (r *GatewayRunner) deliverWatchEvent(ctx context.Context, evt map[string]any) (*bool, bool, error) {
	scoped, err := r.completionEventScope(ctx, evt)
	if err != nil {
		return nil, false, err
	}
	cfg, err := config.Load(home.Dir(scoped), nil)
	if err != nil {
		cfg = config.Defaults()
	}
	if !tools.ShouldSurfaceNotification(cfg, evt) || loadBackgroundNotificationsMode(scoped) == "off" {
		return nil, true, nil
	}
	if evt["type"] == "heartbeat" {
		delivered, err := gateway.DeliverHeartbeat(scoped, r, evt)
		return delivered, false, err
	}
	synthText := gateway.FormatGatewayProcessNotification(evt)
	if synthText == "" {
		return nil, true, nil
	}
	delivered, err := r.InjectWatchNotification(scoped, synthText, evt)
	if err != nil {
		runLog().Error(fmt.Sprintf("Watch notification injection error: %s", err))
		return boolp(false), false, nil
	}
	return delivered, false, nil
}

package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// One home channel with a live transport, profile nil for the launch profile
type homeChannelTarget struct {
	profile   *string
	platform  gateway.Platform
	cfg       *gateway.PlatformConfig
	home      *gateway.HomeChannel
	transport *gateway.DeliveryTransport
}

// Delivery transport of one target, nil and logged when resolving it fails
func safeDeliveryTransport(ctx context.Context, platform gateway.Platform, config *gateway.GatewayConfig, adapters *pydict.Dict[gateway.Platform, gateway.PlatformAdapter], profile *string) (transport *gateway.DeliveryTransport) {
	defer func() {
		if rec := recover(); rec != nil {
			prefix := ""
			if profile != nil && *profile != "" {
				prefix = *profile + ":"
			}
			inboundLog().Debug(fmt.Sprintf("Home-channel transport unavailable for %s%s: %s", prefix, platform, pyrepr.Str(rec)))
			transport = nil
		}
	}()
	live := make(map[gateway.Platform]any, adapters.Len())
	for p, a := range adapters.All() {
		live[p] = a
	}
	return gateway.ResolveDeliveryTransport(ctx, platform, config, live)
}

// Every home channel of the launch profile with a live transport
func (r *GatewayRunner) homeChannelTransports(ctx context.Context) []homeChannelTarget {
	var out []homeChannelTarget
	for _, platform := range r.config.PlatformKeys() {
		cfg := r.config.Platforms[platform]
		if cfg == nil || cfg.HomeChannel == nil || cfg.HomeChannel.ChatID == "" {
			continue
		}
		transport := safeDeliveryTransport(ctx, platform, r.config, r.adapters, nil)
		if transport == nil {
			continue
		}
		out = append(out, homeChannelTarget{platform: platform, cfg: cfg, home: cfg.HomeChannel, transport: transport})
	}
	return out
}

// Every home channel of every served profile with a live transport, the launch profile first
func (r *GatewayRunner) servedHomeChannelTransports(ctx context.Context) []homeChannelTarget {
	out := r.homeChannelTransports(ctx)
	for name, profileCfg := range r.profileConfigs.All() {
		adapters, _ := r.profileAdapters.Get(name)
		if adapters == nil {
			adapters = pydict.New[gateway.Platform, gateway.PlatformAdapter]()
		}
		for _, platform := range profileCfg.PlatformKeys() {
			cfg := profileCfg.Platforms[platform]
			if cfg == nil || cfg.HomeChannel == nil || cfg.HomeChannel.ChatID == "" {
				continue
			}
			profile := name
			transport := safeDeliveryTransport(ctx, platform, profileCfg, adapters, &profile)
			if transport == nil {
				continue
			}
			out = append(out, homeChannelTarget{profile: &profile, platform: platform, cfg: cfg, home: cfg.HomeChannel, transport: transport})
		}
	}
	return out
}

// Sends to one home channel, true on success, failures logged through the failure format
func (r *GatewayRunner) sendHomeChannelMessage(ctx context.Context, platform gateway.Platform, home *gateway.HomeChannel, transport *gateway.DeliveryTransport, message, failureFmt string) bool {
	metadata := r.threadMetadataForTarget(platform, home.ChatID, home.ThreadID, "", nil, transport.Adapter)
	if transport.IsRelay() {
		merged := map[string]any{}
		for k, v := range metadata {
			merged[k] = v
		}
		if home.UserID != nil && *home.UserID != "" {
			merged["user_id"] = *home.UserID
		}
		if home.ScopeID != nil && *home.ScopeID != "" {
			merged["scope_id"] = *home.ScopeID
		}
		metadata = merged
	}
	sendMetadata := nonConversationalMetadata(metadata, platform)
	var result gateway.SendResult
	var err error
	if sendMetadata != nil || transport.IsRelay() {
		result, err = transport.Send(ctx, platform, home.ChatID, message, sendMetadata)
	} else if adapter, ok := transport.Adapter.(gateway.PlatformAdapter); ok {
		result, err = adapter.Send(ctx, home.ChatID, message, "", nil)
	} else {
		err = fmt.Errorf("'%T' object has no attribute 'send'", transport.Adapter)
	}
	if err != nil {
		inboundLog().Warn(fmt.Sprintf(failureFmt, platform, home.ChatID, err))
		return false
	}
	if sendFailed(result) {
		inboundLog().Warn(fmt.Sprintf(failureFmt, platform, home.ChatID, sendError(result)))
		return false
	}
	return true
}

package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Profile command reply of a multiplexing runner for a Telegram dm source stamped with profile
func ProfileCommandReply(ctx context.Context, profile string) (string, error) {
	r := allocGatewayRunner()
	r.config = gateway.DefaultGatewayConfig(ctx)
	cfg := gateway.NewPlatformConfig()
	cfg.Enabled = true
	cfg.Token = "***"
	r.config.Platforms[gateway.PlatformTelegram] = cfg
	on := true
	r.config.MultiplexProfiles = &on
	source := &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "c1", ChatType: "dm", UserID: kstr("u1"), Profile: &profile}
	return r.handleProfileCommand(ctx, &gateway.MessageEvent{Text: "/profile", Source: source, MessageID: kstr("m1")})
}

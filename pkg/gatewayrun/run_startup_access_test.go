package gatewayrun

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
)

func TestAllowlistWarningRequiresAnEnabledMessagingPlatform(t *testing.T) {
	const warning = "No env user allowlists configured"
	cases := []struct {
		name          string
		platforms     map[gateway.Platform]*gateway.PlatformConfig
		expectWarning bool
	}{
		{"api_server_only", map[gateway.Platform]*gateway.PlatformConfig{gateway.PlatformAPIServer: {Enabled: true, Extra: map[string]any{"key": strings.Repeat("k", 40)}}}, false},
		{"api_server_plus_telegram", map[gateway.Platform]*gateway.PlatformConfig{gateway.PlatformAPIServer: {Enabled: true}, gateway.PlatformTelegram: {Enabled: true}}, true},
		{"telegram_disabled", map[gateway.Platform]*gateway.PlatformConfig{gateway.PlatformTelegram: {Enabled: false}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("ICEBATH_HOME", dir)
			for _, name := range append(append(builtinAllowedUsersVars(), builtinAllowAllVars()...), "GATEWAY_ALLOW_ALL_USERS") {
				t.Setenv(name, "")
			}
			ctx := home.WithOverride(context.Background(), dir)
			r := allocGatewayRunner()
			r.config = gateway.DefaultGatewayConfig(ctx)
			r.config.Platforms = tc.platforms
			r.config.SessionsDir = dir + "/sessions"
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
			t.Cleanup(func() { slog.SetDefault(previous) })
			if refused := r.startCheckAccessPolicy(ctx); refused {
				t.Fatal("startup refused")
			}
			if strings.Contains(logs.String(), warning) != tc.expectWarning {
				t.Fatalf("warning %v in %q", tc.expectWarning, logs.String())
			}
		})
	}
}

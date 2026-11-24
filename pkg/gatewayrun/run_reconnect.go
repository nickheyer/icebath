package gatewayrun

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Max seconds between platform reconnect retries, shared by the primary watcher and secondary profiles
const reconnectBackoffCap = 300

// Exponential reconnect backoff of 30s doubling per attempt, capped at five minutes
func reconnectBackoff(attempt int) float64 {
	return math.Min(30*math.Pow(2, float64(attempt-1)), reconnectBackoffCap)
}

// Seconds of continuous reconnect failure before attention is needed, read from the bound profile's settings
func reconnectAttentionAfterSecs(ctx context.Context) float64 {
	return settings.AgentReconnectAttentionAfter.Get(ctx).Seconds()
}

// Whether a reconnect queue entry waited long enough for attention, stamping a missing queued time
func reconnectNeedsAttention(ctx context.Context, info map[string]any, now float64) bool {
	threshold := reconnectAttentionAfterSecs(ctx)
	if threshold <= 0 {
		return false
	}
	queuedAt, ok := info["queued_at"].(float64)
	if !ok {
		info["queued_at"] = now
		return false
	}
	return now-queuedAt >= threshold
}

// Whether a token authenticated platform holds a usable bot credential, true for platforms without one
func platformHasBotCredential(ctx context.Context, platform gateway.Platform, platformConfig *gateway.PlatformConfig) bool {
	if platform == gateway.PlatformWhatsApp {
		session := home.IcebathDir(home.Dir(ctx), "platforms/whatsapp/session", "whatsapp/session")
		if raw, ok := platformConfig.Extra["session_path"]; ok {
			session = anyutil.String(raw)
		}
		_, err := os.Stat(filepath.Join(session, "creds.json"))
		return err == nil
	}
	if _, tokened := gateway.PlatformTokenEnvNames[platform]; !tokened {
		return true
	}
	for _, value := range []any{platformConfig.Token, platformConfig.APIKey} {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			return true
		}
	}
	if platform != gateway.PlatformMatrix {
		return false
	}
	for _, key := range []string{"homeserver", "user_id", "password"} {
		value := platformConfig.Extra[key]
		if !anyutil.IsTrue(value) || strings.TrimSpace(anyutil.String(value)) == "" {
			return false
		}
	}
	return true
}

// Disconnects an adapter that never reached the live map, logging a raised close and never failing
func disposeUnusedAdapter(ctx context.Context, adapter gateway.PlatformAdapter) error {
	if adapter == nil {
		return nil
	}
	if err := adapter.Disconnect(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		pkgLog().Debug("Adapter dispose raised on unowned adapter "+errchain.Show(adapter.Base().Name()), "error", err)
	}
	return nil
}

// Planned restart notice marker in the launch home, never a profile override
func plannedRestartNotificationPath() string {
	return gateway.PlannedRestartMarkerPath(home.Process())
}

// Whether a planned restart notice marker waits in the process home
func plannedRestartNotificationPending() bool {
	_, err := os.Stat(plannedRestartNotificationPath())
	return err == nil
}

package config

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Producer owned diagnostic classification carried on the two argument status callback, a plain string otherwise
type DiagnosticText string

// Warn events and diagnostic classified messages are warning statuses
func IsWarningStatus(eventType string, message any) bool {
	_, diagnostic := message.(DiagnosticText)
	return eventType == "warn" || diagnostic
}

// Warning or error notices and every credit service notice are diagnostics
func IsDiagnosticNotice(level, key string) bool {
	return level == "warn" || level == "error" || strings.HasPrefix(key, "credits.")
}

// Message event surface the diagnostic wake policy reads, the gateway MessageEvent satisfying it
type WakeEvent interface {
	WakeInternal() bool
	WakeMetadata() map[string]any
	WakeReplyMuted() (muted bool, set bool)
	WakeSourcePlatform() string
}

// Diagnostic notification category of a trusted internal diagnostic only wake, empty for everything else
func DiagnosticMetadata(event WakeEvent) map[string]any {
	if event != nil && event.WakeInternal() && event.WakeMetadata()["notification_category"] == "diagnostic" {
		return map[string]any{"notification_category": "diagnostic"}
	}
	return map[string]any{}
}

// Active profile effective user config, empty when it cannot be read so presentation fails open
func EffectiveUserConfig(ctx context.Context) map[string]any {
	cfg, err := loadUserEffective(ctx, "", EnvRefLookup, false)
	if err != nil || cfg == nil {
		return map[string]any{}
	}
	return cfg
}

// A diagnostic category wake mutes its turn only when the owning policy hides diagnostics
func DiagnosticTurnMuted(ctx context.Context, displayMetadata map[string]any, platform string, userConfig map[string]any) bool {
	return displayMetadata["notification_category"] == "diagnostic" && !warningPolicy(ctx, platform, userConfig)
}

// Only trusted diagnostic only wakes mute a turn, a recorded mute snapshot winning
func DiagnosticWakeMuted(ctx context.Context, event WakeEvent, userConfig map[string]any) bool {
	if event == nil {
		return false
	}
	if muted, set := event.WakeReplyMuted(); set {
		return muted
	}
	return event.WakeInternal() && DiagnosticTurnMuted(ctx, event.WakeMetadata(), event.WakeSourcePlatform(), userConfig)
}

// Runs the renderer only when its classified content is visible and reports whether it ran
func RenderNotification(ctx context.Context, render func() error, platform string, diagnostic bool, userConfig map[string]any) (bool, error) {
	if diagnostic && !warningPolicy(ctx, platform, userConfig) {
		return false, nil
	}
	return true, render()
}

// Awaits the presenter only when its classified content is visible and reports whether it ran
func PresentNotification(ctx context.Context, present func(context.Context) error, platform string, diagnostic bool, userConfig map[string]any) (bool, error) {
	if diagnostic && !warningPolicy(ctx, platform, userConfig) {
		return false, nil
	}
	return true, present(ctx)
}

var warningPolicy = WarningNotificationsEnabled

// Whether diagnostics show on the platform, a nil config reading the active profile effective config
func WarningNotificationsEnabled(ctx context.Context, platform string, userConfig map[string]any) bool {
	if userConfig == nil {
		userConfig = EffectiveUserConfig(ctx)
	}
	return !pyrepr.Truthy(ResolveDisplaySetting(ctx, userConfig, platform, "suppress_warning_notifications", false))
}

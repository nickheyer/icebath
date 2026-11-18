package gatewayrun

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Home target env var of a platform, its plugin cron delivery var else PLATFORM_HOME_CHANNEL
func homeTargetEnvVar(ctx context.Context, platformName string) string {
	if name := pluginHomeEnvVar(ctx, platformName); name != "" {
		return name
	}
	return strings.ToUpper(platformName) + "_HOME_CHANNEL"
}

// Home channel env var the platform's plugin registers, empty when none or when discovery fails
func pluginHomeEnvVar(ctx context.Context, platformName string) (name string) {
	defer func() {
		if recover() != nil {
			name = ""
		}
	}()
	plugins.DiscoverPlugins(ctx, false)
	if entry := gateway.Registry.Get(ctx, strings.ToLower(platformName)); entry != nil {
		return entry.CronDeliverEnvVar
	}
	return ""
}

// Optional thread or topic env var of a platform home target
func homeThreadEnvVar(ctx context.Context, platformName string) string {
	return homeTargetEnvVar(ctx, platformName) + "_THREAD_ID"
}

// Home channel value of the env var, the process env only for an unscoped single profile read
func homeChannelSecret(ctx context.Context, envKey string) string {
	value, _, err := provider.GetSecret(ctx, envKey)
	if err != nil {
		var unscoped *provider.UnscopedSecretError
		if errors.As(err, &unscoped) {
			return strings.TrimSpace(os.Getenv(envKey))
		}
		pkgLog().Debug("Home-channel secret "+envKey+" unreadable; treating it as unset", "error", err)
		return ""
	}
	return strings.TrimSpace(value)
}

// Appends the first contact onboarding note and sends the one time no home channel prompt for an empty session
func (r *GatewayRunner) hmwaFirstContactNotes(ctx context.Context, source *gateway.SessionSource, history []map[string]any, turnSidecarNotes *[]string, internal bool) error {
	if len(history) > 0 || internal {
		return nil
	}
	humanPlatform := source.Platform != "" && source.Platform != gateway.PlatformLocal && source.Platform != gateway.PlatformWebhook
	if humanPlatform && source.ChatType == "dm" && !r.asyncStore().Store().HasAnySessions(ctx) {
		configPath := filepath.Join(home.Dir(ctx), "config.yaml")
		if note := tools.FirstContactTurnNote(ctx, gateway.LoadGatewayConfigMap(ctx, ""), configPath, true, false); note != nil && *note != "" {
			*turnSidecarNotes = append(*turnSidecarNotes, *note)
		}
	}
	if !humanPlatform {
		return nil
	}
	platformName := string(source.Platform)
	envKey := homeTargetEnvVar(ctx, platformName)
	homeEnv := ""
	if envKey != "" {
		homeEnv = homeChannelSecret(ctx, envKey)
	}
	if homeEnv == "" && r.config != nil && r.config.GetHomeChannel(source.Platform) != nil {
		homeEnv = "set"
	}
	if homeEnv == "" {
		if profile := strings.TrimSpace(optText(source.Profile)); profile != "" && profile != "default" {
			if loaded, err := gateway.LoadGatewayConfig(ctx); err == nil && loaded != nil && loaded.GetHomeChannel(source.Platform) != nil {
				homeEnv = "set"
			}
		}
	}
	if homeEnv != "" {
		return nil
	}
	sethomeCmd := "/sethome"
	if source.Platform == gateway.PlatformSlack {
		sethomeCmd = "/icebath sethome"
	}
	return r.deliverPlatformNotice(ctx, source, i18n.T(ctx, "gateway.notify.no_home_channel", "",
		map[string]any{"platform": platformNoticeLabel(ctx, platformName), "sethome_cmd": sethomeCmd}))
}

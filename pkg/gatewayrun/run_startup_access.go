package gatewayrun

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Builtin platforms whose allowed users and allow all env vars count as an allowlist or open access opt in
var allowlistEnvPlatforms = []string{
	"TELEGRAM", "DISCORD", "WHATSAPP", "WHATSAPP_CLOUD", "SLACK", "SIGNAL", "EMAIL", "SMS",
	"MATTERMOST", "MATRIX", "DINGTALK", "FEISHU", "WECOM", "WECOM_CALLBACK", "WEIXIN",
	"BLUEBUBBLES", "QQ", "YUANBAO",
}

// Allowed users vars of the builtin platforms plus the shared group and gateway allowlists
func builtinAllowedUsersVars() []string {
	var out []string
	for _, p := range allowlistEnvPlatforms {
		out = append(out, p+"_ALLOWED_USERS")
	}
	return append(out, "SIGNAL_GROUP_ALLOWED_USERS", "TELEGRAM_GROUP_ALLOWED_USERS", "TELEGRAM_GROUP_ALLOWED_CHATS", "GATEWAY_ALLOWED_USERS")
}

// Allow all vars of the builtin platforms
func builtinAllowAllVars() []string {
	var out []string
	for _, p := range allowlistEnvPlatforms {
		out = append(out, p+"_ALLOW_ALL_USERS")
	}
	return out
}

// Policy env names of a platform declaring its own dm and group policy env, false otherwise
func ownPolicyEnvs(ctx context.Context, platform gateway.Platform) (dmEnv, groupEnv, allowAllEnv string, ok bool) {
	entry := gateway.Registry.Get(ctx, string(platform))
	own, _ := gateway.AuthzField(string(platform), "own_policy_env", entry).([]string)
	if len(own) == 0 {
		return "", "", "", false
	}
	if len(own) > 0 {
		dmEnv = own[0]
	}
	if len(own) > 1 {
		groupEnv = own[1]
	}
	if allowAll := gateway.AuthzField(string(platform), "allow_all_env", entry); pyrepr.Truthy(allowAll) {
		allowAllEnv = pyrepr.Str(allowAll)
	}
	return dmEnv, groupEnv, allowAllEnv, true
}

// Whether an env value opts in, true 1 or yes in any case
func envOptIn(value string) bool {
	switch strings.ToLower(value) {
	case "true", "1", "yes":
		return true
	}
	return false
}

// Enabled platforms of the config in its insertion order
func enabledPlatforms(cfg *gateway.GatewayConfig) []gateway.Platform {
	var out []gateway.Platform
	for _, platform := range cfg.PlatformKeys() {
		if pc := cfg.Platforms[platform]; pc != nil && pc.Enabled {
			out = append(out, platform)
		}
	}
	return out
}

// Startup abort reason for an enabled platform with an open policy and no allow all opt in, empty when none
func ownPolicyOpenStartupViolation(ctx context.Context, cfg *gateway.GatewayConfig) string {
	plugins.DiscoverPlugins(ctx, false)
	for _, platform := range enabledPlatforms(cfg) {
		dmEnv, groupEnv, allowAllEnv, ok := ownPolicyEnvs(ctx, platform)
		if !ok {
			continue
		}
		extra := cfg.Platforms[platform].Extra
		policy := func(key, env string) string {
			if value := extra[key]; pyrepr.Truthy(value) {
				return pystr.Lower(pystr.Strip(pyrepr.Str(value)))
			}
			if env == "" {
				return "pairing"
			}
			return pystr.Lower(pystr.Strip(gateway.Getenv(ctx, env, "pairing")))
		}
		if policy("dm_policy", dmEnv) != "open" && policy("group_policy", groupEnv) != "open" {
			continue
		}
		if envOptIn(gateway.Getenv(ctx, "GATEWAY_ALLOW_ALL_USERS", "")) || (allowAllEnv != "" && envOptIn(gateway.Getenv(ctx, allowAllEnv, ""))) {
			continue
		}
		return string(platform) + ": open policy without allow-all opt-in"
	}
	return ""
}

// Warns about missing allowlists and returns true when an open policy without opt in refuses startup
func (r *GatewayRunner) startCheckAccessPolicy(ctx context.Context) bool {
	allowedVars := builtinAllowedUsersVars()
	allowAllVars := append([]string{"GATEWAY_ALLOW_ALL_USERS"}, builtinAllowAllVars()...)
	func() {
		defer func() { _ = recover() }()
		entries := gateway.Registry.LoadedEntries(ctx)
		for _, platform := range enabledPlatforms(r.config) {
			if entry := gateway.Registry.Get(ctx, string(platform)); entry != nil {
				entries = append(entries, entry)
			}
		}
		for _, entry := range entries {
			if entry.Source != "plugin" {
				continue
			}
			if entry.AllowedUsersEnv != "" {
				allowedVars = append(allowedVars, entry.AllowedUsersEnv)
			}
			if entry.AllowAllEnv != "" {
				allowAllVars = append(allowAllVars, entry.AllowAllEnv)
			}
		}
	}()
	nonMessaging := map[gateway.Platform]bool{gateway.PlatformLocal: true, gateway.PlatformAPIServer: true, gateway.PlatformWebhook: true}
	hasMessaging := false
	for _, platform := range enabledPlatforms(r.config) {
		hasMessaging = hasMessaging || !nonMessaging[platform]
	}
	anyAllowed := slices.ContainsFunc(allowedVars, func(v string) bool { return os.Getenv(v) != "" })
	anyAllowAll := slices.ContainsFunc(allowAllVars, func(v string) bool { return envOptIn(os.Getenv(v)) })
	if hasMessaging && !anyAllowed && !anyAllowAll {
		runLog().Warn("No env user allowlists configured. Messaging platforms default to pairing/allowlist " +
			"policies and will deny unknown senders unless you configure platform allowlists (e.g., " +
			"TELEGRAM_ALLOWED_USERS=your_id) or explicitly opt in with GATEWAY_ALLOW_ALL_USERS=true " +
			"plus dm_policy/group_policy: open on the platform.")
	}
	reason := ownPolicyOpenStartupViolation(ctx, r.config)
	if reason == "" {
		return false
	}
	platformValue, _, _ := strings.Cut(reason, ":")
	allowAllEnv := ""
	if platform, err := gateway.ParsePlatform(ctx, platformValue); err == nil {
		_, _, allowAllEnv, _ = ownPolicyEnvs(ctx, platform)
	}
	if allowAllEnv == "" {
		allowAllEnv = "a platform allow-all flag"
	}
	runLog().Error(fmt.Sprintf("Refusing to start: %s has dm_policy/group_policy set to 'open' "+
		"but neither GATEWAY_ALLOW_ALL_USERS nor %s is enabled.", platformValue, allowAllEnv))
	writeRuntimeStatusQuiet(ctx, gateway.RuntimeStatusUpdate{Fields: map[string]any{"gateway_state": "startup_failed", "exit_reason": reason}})
	r.requestCleanExit(reason)
	return true
}

package manage

import (
	"context"

	"github.com/nickheyer/icebath/pkg/profiles"
)

// Platform member values in member order, pseudo members included
func gatewayPlatformMembers() []string { return profiles.GatewayChannelHooks.PlatformMembers() }

// Core platform enable credential table in order
func gatewayEnvEnableCredentials() []ChannelEnableCredentials {
	out := []ChannelEnableCredentials{}
	for _, row := range profiles.GatewayChannelHooks.EnvEnableCredentials() {
		names, _ := row[1].([]string)
		out = append(out, ChannelEnableCredentials{Platform: row[0].(string), Names: names})
	}
	return out
}

// Every registry entry of the ctx home scope with its env override steps
func gatewayPlatformEntries(ctx context.Context) ([]ChannelPlatformEntry, error) {
	raw, err := profiles.GatewayChannelHooks.PlatformEntries(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ChannelPlatformEntry, 0, len(raw))
	for _, e := range raw {
		entry := ChannelPlatformEntry{Name: gwStr(e["name"]), RequiredEnv: gwStrs(e["required_env"]),
			AllowedUsersEnv: gwStr(e["allowed_users_env"]), AllowAllEnv: gwStr(e["allow_all_env"]),
			CronDeliverEnvVar: gwStr(e["cron_deliver_env_var"]), EnvEnableCredentials: gwStrs(e["env_enable_credentials"]),
			SharedEnvPrefixes: gwStrs(e["shared_env_prefixes"])}
		steps, _ := e["env_overrides"].([]map[string]any)
		for _, s := range steps {
			var step ChannelEnvStep
			switch s["kind"] {
			case "cred":
				step.Cred = &ChannelEnvCred{Platform: gwStr(s["platform"]), Creds: gwAnys(s["creds"]), Token: gwStr(s["token"]),
					FixedEnv: gwAnys(s["fixed"]), OptionalEnv: gwAnys(s["optional"]), OptionalStrippedEnv: gwAnys(s["optional_stripped"]),
					WarnMissing: gwStrs(s["warn_missing"]), Home: gwStr(s["home"])}
			case "partial":
				step.Partial = &ChannelEnvPartial{Env: gwStr(s["env"]), EnvBase: gwStr(s["env_base"]), Platform: gwStr(s["platform"])}
			}
			entry.EnvOverrides = append(entry.EnvOverrides, step)
		}
		out = append(out, entry)
	}
	return out, nil
}

func gwStr(v any) string { s, _ := v.(string); return s }

func gwStrs(v any) []string { s, _ := v.([]string); return s }

func gwAnys(v any) []any { s, _ := v.([]any); return s }

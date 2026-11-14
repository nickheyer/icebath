package qqbot

import (
	"context"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Sets the home channel from the QQBOT names, else the deprecated QQ names with a rename warning
func qqHome(ctx context.Context, cfg *gateway.GatewayConfig, pc *gateway.PlatformConfig) error {
	home := pystr.Strip(gateway.Getenv(ctx, "QQBOT_HOME_CHANNEL", ""))
	nameEnv := "QQBOT_HOME_CHANNEL_NAME"
	if home == "" {
		if home = pystr.Strip(gateway.Getenv(ctx, "QQ_HOME_CHANNEL", "")); home != "" {
			nameEnv = "QQ_HOME_CHANNEL_NAME"
			slog.Warn("QQ_HOME_CHANNEL is deprecated; rename to QQBOT_HOME_CHANNEL in your .env for consistency with the platform key.")
		}
	}
	if home == "" {
		return nil
	}
	name := gateway.Getenv(ctx, "QQBOT_HOME_CHANNEL_NAME", "")
	if name == "" {
		name = gateway.Getenv(ctx, nameEnv, "Home")
	}
	h := &gateway.HomeChannel{Platform: gateway.PlatformQQBot, ChatID: home, Name: name}
	thread := gateway.Getenv(ctx, "QQBOT_HOME_CHANNEL_THREAD_ID", "")
	if thread == "" {
		thread = gateway.Getenv(ctx, "QQ_HOME_CHANNEL_THREAD_ID", "")
	}
	if thread != "" {
		h.ThreadID = &thread
	}
	h.PostInit()
	pc.HomeChannel = h
	return nil
}

var envEnableCredentials = []string{"QQ_APP_ID", "QQ_CLIENT_SECRET"}

var envOverrides = []gateway.EnvStep{
	&gateway.EnvCred{Platform: gateway.PlatformQQBot, Creds: [][]string{{"QQ_APP_ID", "QQ_CLIENT_SECRET"}},
		Optional:         []gateway.EnvSpec{{Key: "app_id", Env: "QQ_APP_ID"}, {Key: "client_secret", Env: "QQ_CLIENT_SECRET"}},
		OptionalStripped: []gateway.EnvSpec{{Key: "allow_from", Env: "QQ_ALLOWED_USERS"}, {Key: "group_allow_from", Env: "QQ_GROUP_ALLOWED_USERS"}},
		Then:             qqHome},
}

var authz = map[string]any{"allowed_users_env": "QQ_ALLOWED_USERS", "allow_all_env": "QQ_ALLOW_ALL_USERS",
	"own_policy_env": []string{"", ""}, "group_allowed_chats_env": "QQ_GROUP_ALLOWED_USERS", "allowlist_suppresses_pairing": true}

// Registers the env declarations of the bundled qqbot directory with the gateway config bridge at load
func init() {
	gateway.RegisterBundledEnv("qqbot", "", gateway.BundledEnvRows{Overrides: envOverrides, EnableCredentials: envEnableCredentials, Authz: authz})
}

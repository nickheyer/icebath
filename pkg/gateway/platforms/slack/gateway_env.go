package slack

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Creates a disabled entry when needed and keeps user and scope provenance across an unchanged chat id
func slackHome(ctx context.Context, cfg *gateway.GatewayConfig) error {
	home := gateway.EnvFirst(ctx, []string{"SLACK_HOME_CHANNEL"})
	if home == "" {
		return nil
	}
	pc, ok := cfg.Platforms[gateway.PlatformSlack]
	if !ok {
		pc = gateway.NewPlatformConfig()
		pc.Enabled = false
		cfg.SetPlatform(gateway.PlatformSlack, pc)
	}
	existing := pc.HomeChannel
	same := existing != nil && existing.ChatID == home
	h := &gateway.HomeChannel{Platform: gateway.PlatformSlack, ChatID: home, Name: gateway.EnvFirst(ctx, []string{"SLACK_HOME_CHANNEL_NAME"})}
	if thread := gateway.EnvFirst(ctx, []string{"SLACK_HOME_CHANNEL_THREAD_ID"}); thread != "" {
		h.ThreadID = &thread
	}
	if same {
		h.UserID, h.ScopeID = existing.UserID, existing.ScopeID
	}
	h.PostInit()
	pc.HomeChannel = h
	return nil
}

var envEnableCredentials = []string{"SLACK_BOT_TOKEN"}

var envOverrides = []gateway.EnvStep{
	&gateway.EnvCred{Platform: gateway.PlatformSlack, Creds: [][]string{{"SLACK_BOT_TOKEN"}}, Token: "SLACK_BOT_TOKEN"},
	gateway.EnvStepFunc(slackHome),
}

var authz = map[string]any{"allowed_users_env": "SLACK_ALLOWED_USERS", "allow_all_env": "SLACK_ALLOW_ALL_USERS", "allow_bots_env": "SLACK_ALLOW_BOTS", "allowlist_suppresses_pairing": true}

// Registers the env declarations of the bundled slack directory with the gateway config bridge
func init() {
	gateway.RegisterBundledEnv("slack", "", gateway.BundledEnvRows{Overrides: envOverrides, EnableCredentials: envEnableCredentials, Authz: authz})
}

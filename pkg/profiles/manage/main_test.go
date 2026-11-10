package manage

import (
	"context"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Runs a scratch prune child entry first, then installs gateway hook fixtures since pkg/gateway is not linked
func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	if len(os.Args) > 1 && os.Args[1] == mcpStdioFixtureArg {
		os.Exit(runMCPStdioFixture(os.Stdin, os.Stdout))
	}
	home.DispatchScratchPruneEntry()
	installGatewayFixtures()
	os.Exit(m.Run())
}

// Registry shapes and runtime answers the channel and lifecycle tests rely on
func installGatewayFixtures() {
	c := &profiles.GatewayChannelHooks
	c.PlatformMembers = func() []string {
		return []string{"local", "telegram", "discord", "whatsapp", "whatsapp_cloud", "slack", "signal", "mattermost",
			"matrix", "email", "sms", "dingtalk", "api_server", "webhook", "msgraph_webhook", "feishu", "wecom",
			"wecom_callback", "weixin", "bluebubbles", "qqbot", "yuanbao", "relay"}
	}
	c.EnvEnableCredentials = func() [][2]any { return [][2]any{{"relay", []string{"GATEWAY_RELAY_URL"}}} }
	c.PlatformEntries = func(context.Context) ([]map[string]any, error) {
		return []map[string]any{
			{"name": "email", "required_env": []string{"EMAIL_ADDRESS", "EMAIL_PASSWORD", "EMAIL_SMTP_HOST"},
				"allowed_users_env": "EMAIL_ALLOWED_USERS", "allow_all_env": "EMAIL_ALLOW_ALL_USERS", "cron_deliver_env_var": "EMAIL_HOME_ADDRESS"},
			{"name": "sms", "required_env": []string{"TWILIO_ACCOUNT_SID", "TWILIO_AUTH_TOKEN", "TWILIO_PHONE_NUMBER"},
				"allowed_users_env": "SMS_ALLOWED_USERS", "allow_all_env": "SMS_ALLOW_ALL_USERS", "cron_deliver_env_var": "SMS_HOME_CHANNEL"},
			{"name": "telegram", "required_env": []string{"TELEGRAM_BOT_TOKEN"}, "allowed_users_env": "TELEGRAM_ALLOWED_USERS"},
			{"name": "discord", "required_env": []string{"DISCORD_BOT_TOKEN"}, "allowed_users_env": "DISCORD_ALLOWED_USERS"},
			{"name": "google_chat", "required_env": []string{"GOOGLE_CHAT_SERVICE_ACCOUNT_JSON"}},
		}, nil
	}
	s := &profiles.GatewayStatusHooks
	s.ProfileRunning = func(string) bool { return false }
	s.ServesNamedProfile = func(string) bool { return false }
	s.NotifyProfilesChanged = func(string) {}
	s.RecordedServedProfiles = func() ([]string, bool, error) { return nil, false, nil }
	s.ExclusiveMaintenance = flockMaintenance
	s.PidExists = func(int) bool { return false }
	s.ProcessStartTime = func(int) (int64, bool) { return 0, false }
	s.TerminatePid = func(int, bool, *int64) error { return nil }
	s.RecordedHomeConflicts = func(map[string]any, string) bool { return false }
}

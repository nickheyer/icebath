package gateway_test

import (
	"context"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/bluebubbles"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/dingtalk"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/discord"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/email"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/feishu"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/matrix"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/mattermost"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/qqbot"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/signal"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/slack"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/sms"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/telegram"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/wecom"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/weixin"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/whatsapp"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/whatsappcloud"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/yuanbao"
	"github.com/nickheyer/icebath/pkg/home"
)

// AUTHZ and ENV_ENABLE_CREDENTIALS of every upstream plugins/platforms gateway_env module, wecom_callback under its CALLBACK_ prefix
var shippedEnvDeclarations = map[string]struct {
	authz       map[string]any
	credentials []string
}{
	"bluebubbles": {map[string]any{"allowed_users_env": "BLUEBUBBLES_ALLOWED_USERS", "allow_all_env": "BLUEBUBBLES_ALLOW_ALL_USERS", "allowlist_suppresses_pairing": true},
		[]string{"BLUEBUBBLES_SERVER_URL", "BLUEBUBBLES_PASSWORD"}},
	"dingtalk": {map[string]any{"allowed_users_env": "DINGTALK_ALLOWED_USERS", "allow_all_env": "DINGTALK_ALLOW_ALL_USERS", "allowlist_suppresses_pairing": true},
		[]string{"DINGTALK_CLIENT_ID", "DINGTALK_CLIENT_SECRET"}},
	"discord": {map[string]any{"allowed_users_env": "DISCORD_ALLOWED_USERS", "allow_all_env": "DISCORD_ALLOW_ALL_USERS", "allow_bots_env": "DISCORD_ALLOW_BOTS", "allowlist_suppresses_pairing": true},
		[]string{"DISCORD_BOT_TOKEN"}},
	"email": {map[string]any{"allowed_users_env": "EMAIL_ALLOWED_USERS", "allow_all_env": "EMAIL_ALLOW_ALL_USERS", "allowlist_suppresses_pairing": true},
		[]string{"EMAIL_ADDRESS", "EMAIL_PASSWORD", "EMAIL_IMAP_HOST", "EMAIL_SMTP_HOST"}},
	"feishu": {map[string]any{"allowed_users_env": "FEISHU_ALLOWED_USERS", "allow_all_env": "FEISHU_ALLOW_ALL_USERS", "allow_bots_env": "FEISHU_ALLOW_BOTS", "allowlist_suppresses_pairing": true},
		[]string{"FEISHU_APP_ID", "FEISHU_APP_SECRET"}},
	"matrix": {map[string]any{"allowed_users_env": "MATRIX_ALLOWED_USERS", "allow_all_env": "MATRIX_ALLOW_ALL_USERS", "allowlist_suppresses_pairing": true},
		[]string{"MATRIX_ACCESS_TOKEN", "MATRIX_PASSWORD"}},
	"mattermost": {map[string]any{"allowed_users_env": "MATTERMOST_ALLOWED_USERS", "allow_all_env": "MATTERMOST_ALLOW_ALL_USERS", "allowlist_suppresses_pairing": true},
		[]string{"MATTERMOST_TOKEN"}},
	"qqbot": {map[string]any{"allowed_users_env": "QQ_ALLOWED_USERS", "allow_all_env": "QQ_ALLOW_ALL_USERS", "own_policy_env": []string{"", ""},
		"group_allowed_chats_env": "QQ_GROUP_ALLOWED_USERS", "allowlist_suppresses_pairing": true}, []string{"QQ_APP_ID", "QQ_CLIENT_SECRET"}},
	"signal": {map[string]any{"allowed_users_env": "SIGNAL_ALLOWED_USERS", "allow_all_env": "SIGNAL_ALLOW_ALL_USERS", "allowlist_suppresses_pairing": true},
		[]string{"SIGNAL_HTTP_URL"}},
	"slack": {map[string]any{"allowed_users_env": "SLACK_ALLOWED_USERS", "allow_all_env": "SLACK_ALLOW_ALL_USERS", "allow_bots_env": "SLACK_ALLOW_BOTS", "allowlist_suppresses_pairing": true},
		[]string{"SLACK_BOT_TOKEN"}},
	"sms": {map[string]any{"allowed_users_env": "SMS_ALLOWED_USERS", "allow_all_env": "SMS_ALLOW_ALL_USERS", "allowlist_suppresses_pairing": true},
		[]string{"TWILIO_ACCOUNT_SID"}},
	"telegram": {map[string]any{"allowed_users_env": "TELEGRAM_ALLOWED_USERS", "allow_all_env": "TELEGRAM_ALLOW_ALL_USERS", "allow_bots_env": "TELEGRAM_ALLOW_BOTS",
		"group_allowed_chats_env": "TELEGRAM_GROUP_ALLOWED_CHATS", "allowlist_suppresses_pairing": true}, []string{"TELEGRAM_BOT_TOKEN"}},
	"wecom": {map[string]any{"allowed_users_env": "WECOM_ALLOWED_USERS", "allow_all_env": "WECOM_ALLOW_ALL_USERS", "own_policy_env": []string{"WECOM_DM_POLICY", "WECOM_GROUP_POLICY"},
		"allowlist_suppresses_pairing": true}, []string{"WECOM_BOT_ID", "WECOM_SECRET"}},
	"wecom_callback": {map[string]any{"allowed_users_env": "WECOM_CALLBACK_ALLOWED_USERS", "allow_all_env": "WECOM_CALLBACK_ALLOW_ALL_USERS", "allowlist_suppresses_pairing": true},
		[]string{"WECOM_CALLBACK_CORP_ID", "WECOM_CALLBACK_CORP_SECRET"}},
	"weixin": {map[string]any{"allowed_users_env": "WEIXIN_ALLOWED_USERS", "allow_all_env": "WEIXIN_ALLOW_ALL_USERS", "own_policy_env": []string{"WEIXIN_DM_POLICY", "WEIXIN_GROUP_POLICY"},
		"allowlist_suppresses_pairing": true}, []string{"WEIXIN_TOKEN", "WEIXIN_ACCOUNT_ID"}},
	"whatsapp_cloud": {map[string]any{"allowed_users_env": "WHATSAPP_CLOUD_ALLOWED_USERS", "allow_all_env": "WHATSAPP_CLOUD_ALLOW_ALL_USERS", "allowlist_suppresses_pairing": true},
		[]string{"WHATSAPP_CLOUD_PHONE_NUMBER_ID", "WHATSAPP_CLOUD_ACCESS_TOKEN"}},
	"whatsapp": {map[string]any{"allowed_users_env": "WHATSAPP_ALLOWED_USERS", "allow_all_env": "WHATSAPP_ALLOW_ALL_USERS", "own_policy_env": []string{"WHATSAPP_DM_POLICY", "WHATSAPP_GROUP_POLICY"},
		"allowlist_suppresses_pairing": true}, []string{"WHATSAPP_ENABLED"}},
	"yuanbao": {map[string]any{"allowed_users_env": "YUANBAO_ALLOWED_USERS", "allow_all_env": "YUANBAO_ALLOW_ALL_USERS", "own_policy_env": []string{"YUANBAO_DM_POLICY", "YUANBAO_GROUP_POLICY"}},
		[]string{"YUANBAO_APP_ID", "YUANBAO_APP_SECRET"}},
}

func TestCoreHintAndDisplayTablesHoldNoBundledPluginPlatform(t *testing.T) {
	gateway.UseCompiledTreeForTest(t)
	bundled, _ := gateway.ScanBundledPluginPlatformsForTest()
	if len(bundled) == 0 {
		t.Fatal("no bundled platform in the compiled tree")
	}
	for name := range agent.PlatformHints {
		if bundled[name] {
			t.Fatalf("prompt hint row for bundled %q", name)
		}
	}
	for _, name := range config.CoreDisplayPlatforms() {
		if bundled[name] {
			t.Fatalf("display default row for bundled %q", name)
		}
	}
}

func TestBundledDeclarationsServeEveryRegisteringPlatformWithoutOverride(t *testing.T) {
	gateway.UseCompiledTreeForTest(t)
	ctx := home.WithOverride(context.Background(), t.TempDir())
	for name, want := range shippedEnvDeclarations {
		if got := gateway.BundledAuthz(name); !reflect.DeepEqual(got, want.authz) {
			t.Fatalf("%s authz %v", name, got)
		}
		if got := gateway.AllowlistEnvForPlatform(ctx, " "+name+" "); got != want.authz["allowed_users_env"] {
			t.Fatalf("%s allowlist env %q", name, got)
		}
		if got := gateway.AuthzField(name, "allow_all_env", nil); got != want.authz["allow_all_env"] {
			t.Fatalf("%s allow all env %v", name, got)
		}
		overrides, credentials, ok := gateway.BundledEnvRowsForTest(name)
		if !ok || len(overrides) == 0 || !slices.Equal(credentials, want.credentials) {
			t.Fatalf("%s rows %v %v %v", name, ok, len(overrides), credentials)
		}
	}
	if len(gateway.BundledAuthz("irc")) != 0 || len(gateway.BundledAuthz("nope")) != 0 {
		t.Fatal("a bundled platform without a gateway_env module and an unknown name declare nothing")
	}
}

func TestBundledEnvFallbacksApplyWithoutOverride(t *testing.T) {
	gateway.UseCompiledTreeForTest(t)
	ctx := home.WithOverride(context.Background(), t.TempDir())
	env := map[string]string{"BLUEBUBBLES_SERVER_URL": "http://127.0.0.1:1", "BLUEBUBBLES_PASSWORD": "Zq81password77",
		"DINGTALK_CLIENT_ID": "id", "DINGTALK_CLIENT_SECRET": "Zq81secret77", "DISCORD_BOT_TOKEN": "Zq81realvalue77",
		"EMAIL_ADDRESS": "a@b.c", "EMAIL_PASSWORD": "Zq81password77", "EMAIL_IMAP_HOST": "imap.b.c", "EMAIL_SMTP_HOST": "smtp.b.c",
		"FEISHU_APP_ID": "id", "FEISHU_APP_SECRET": "Zq81secret77", "MATRIX_ACCESS_TOKEN": "Zq81realvalue77", "MATRIX_PASSWORD": "Zq81password77",
		"MATRIX_HOMESERVER": "https://m.b.c", "MATRIX_USER_ID": "@u:b.c", "MATTERMOST_TOKEN": "Zq81realvalue77", "MATTERMOST_URL": "https://mm.b.c",
		"QQ_APP_ID": "id", "QQ_CLIENT_SECRET": "Zq81secret77", "SIGNAL_HTTP_URL": "http://127.0.0.1:2", "SIGNAL_ACCOUNT": "+15550000000",
		"SLACK_BOT_TOKEN": "xoxb-1", "TWILIO_ACCOUNT_SID": "AC1", "TWILIO_AUTH_TOKEN": "Zq81realvalue77", "TELEGRAM_BOT_TOKEN": "123456:Zq81realvalue77",
		"WECOM_BOT_ID": "bot", "WECOM_SECRET": "Zq81secret77", "WECOM_CALLBACK_CORP_ID": "corp", "WECOM_CALLBACK_CORP_SECRET": "Zq81secret77",
		"WEIXIN_TOKEN": "Zq81realvalue77", "WEIXIN_ACCOUNT_ID": "acc", "WHATSAPP_CLOUD_PHONE_NUMBER_ID": "1",
		"WHATSAPP_CLOUD_ACCESS_TOKEN": "Zq81realvalue77", "WHATSAPP_ENABLED": "true", "YUANBAO_APP_ID": "id", "YUANBAO_APP_SECRET": "Zq81secret77"}
	for k, v := range env {
		t.Setenv(k, v)
	}
	cfg, err := gateway.LoadGatewayConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	withToken := map[string]bool{}
	for p, pc := range cfg.Platforms {
		if pc.Enabled {
			withToken[string(p)] = pc.Token != nil
		}
	}
	want := map[string]bool{"telegram": true, "discord": true, "whatsapp": false, "whatsapp_cloud": false, "slack": true, "signal": false,
		"mattermost": true, "matrix": true, "email": false, "dingtalk": false, "feishu": false, "wecom": false,
		"wecom_callback": false, "weixin": true, "bluebubbles": false, "yuanbao": false}
	if !maps.Equal(withToken, want) {
		t.Fatalf("enabled platforms and token presence %v", withToken)
	}
}

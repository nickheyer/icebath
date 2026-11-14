package yuanbao

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	_ "github.com/nickheyer/icebath/pkg/gateway/messagetools"
	"github.com/nickheyer/icebath/pkg/plugins"
)

func TestPluginRegistersTheYuanbaoPlatform(t *testing.T) {
	h := isolatedHome(t)
	ctx := context.Background()
	plugins.InstallServices(&plugins.Services{Platforms: gateway.PlatformRegistryService()})
	plugin, ok := plugins.BundledPlugin("platforms/yuanbao")
	if !ok {
		t.Fatal("yuanbao platform plugin is not compiled in")
	}
	manifest := plugin.Manifest()
	if err := plugin.Register(plugins.NewContext(&manifest, plugins.NewManager(h))); err != nil {
		t.Fatal(err)
	}
	var entry *gateway.PlatformEntry
	for _, candidate := range gateway.Registry.AllEntries(ctx) {
		if candidate.Name == "yuanbao" {
			entry = candidate
		}
	}
	if entry == nil {
		t.Fatal("yuanbao entry missing from the registry")
	}
	if entry.Label != "Yuanbao" || entry.Emoji != "💎" || !reflect.DeepEqual(entry.RequiredEnv, []string{"YUANBAO_APP_ID", "YUANBAO_APP_SECRET"}) ||
		entry.CronDeliverEnvVar != "YUANBAO_HOME_CHANNEL" || entry.InstallHint != "Yuanbao: websockets not installed. Run: pip install websockets" ||
		entry.AllowUpdateCommand || entry.StandaloneSend == nil || !entry.StandaloneSend.Media || !entry.StandaloneSend.TextDirect ||
		entry.StandaloneSenderFn == nil || entry.SetupFn == nil || entry.IsConnected == nil || entry.DashboardLabel != "Yuanbao (元宝)" ||
		!strings.HasPrefix(entry.PlatformHint, "You are on Yuanbao (腾讯元宝), a Chinese AI assistant platform.") ||
		entry.AllowedUsersEnv != "YUANBAO_ALLOWED_USERS" || !reflect.DeepEqual(entry.OwnPolicyEnv, []string{"YUANBAO_DM_POLICY", "YUANBAO_GROUP_POLICY"}) ||
		!entry.RunCheck(ctx) || len(entry.DashboardRequiredEnv) != 0 || len(entry.EnvOverrides) != 1 {
		t.Fatalf("entry %+v", entry)
	}
	adapter, err := entry.AdapterFactory(ctx, gateway.NewPlatformConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, isYuanbao := adapter.(*YuanbaoAdapter); !isYuanbao {
		t.Fatalf("factory built %T", adapter)
	}
	configured := gateway.NewPlatformConfig()
	configured.Extra = map[string]any{"app_id": "key", "app_secret": "secret"}
	partial := gateway.NewPlatformConfig()
	partial.Extra = map[string]any{"app_id": "key"}
	if !entry.ConfigConnectedFn(configured) || entry.ConfigConnectedFn(partial) {
		t.Fatal("config connected needs both the app id and the app secret")
	}
}

func TestSendTargetSyntax(t *testing.T) {
	explicit := map[string]string{"group:123": "group:123", " direct:abc": "direct:abc", "direct:abc ": "direct:abc ", "12345": "group:12345",
		" 678 ": "group:678"}
	for ref, want := range explicit {
		parsed, ok := parseSendTarget(ref).(*gateway.TargetRefParse)
		if !ok || parsed.ChatID == nil || *parsed.ChatID != want || parsed.ThreadID != nil {
			t.Fatalf("target %q parsed as %+v", ref, parseSendTarget(ref))
		}
	}
	for _, ref := range []string{"group:a:b", "alice", "", "12a", "group:"} {
		if parsed := parseSendTarget(ref); parsed != gateway.NotExplicit {
			t.Fatalf("target %q parsed as %+v", ref, parsed)
		}
	}
}

func TestIsConnectedReadsExtraThenEnv(t *testing.T) {
	isolatedHome(t)
	ctx := context.Background()
	both := gateway.NewPlatformConfig()
	both.Extra = map[string]any{"app_id": " key ", "app_secret": "secret"}
	blank := gateway.NewPlatformConfig()
	blank.Extra = map[string]any{"app_id": "   ", "app_secret": "secret"}
	if connected, err := isConnected(ctx, both); err != nil || !connected {
		t.Fatalf("extra credentials %v %v", connected, err)
	}
	for _, config := range []*gateway.PlatformConfig{blank, gateway.NewPlatformConfig(), nil} {
		if connected, err := isConnected(ctx, config); err != nil || connected {
			t.Fatalf("missing credentials read as connected %v %v", connected, err)
		}
	}
	t.Setenv("YUANBAO_APP_KEY", "legacy-key")
	t.Setenv("YUANBAO_APP_SECRET", "secret")
	if connected, err := isConnected(ctx, nil); err != nil || !connected {
		t.Fatalf("env credentials %v %v", connected, err)
	}
}

func TestStandaloneSendUsesTheRunningAdapter(t *testing.T) {
	isolatedHome(t)
	ctx := context.Background()
	kwargs := map[string]any{"chat_id": "direct:alice", "message": "hello"}
	refused, err := standaloneSend(ctx, kwargs)
	if err != nil || !strings.Contains(refused["error"].(string), "Yuanbao adapter is not running") {
		t.Fatalf("send without an adapter %v %v", refused, err)
	}
	_, server := connectAdapter(t, nil)
	sent, err := standaloneSend(ctx, kwargs)
	if err != nil || sent["success"] != true || sent["platform"] != "yuanbao" || sent["chat_id"] != "direct:alice" ||
		firstText(t, server.sent("send_c2c_message")[0], 5) != "hello" {
		t.Fatalf("send through the adapter %v %v", sent, err)
	}
}

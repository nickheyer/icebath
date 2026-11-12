package gatewayrun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/state"
)

// test_multiplex_busy_input_mode.py test_primary_adapter_busy_origin_uses_routed_privacy
func TestPrimaryAdapterBusyOriginUsesRoutedPrivacy(t *testing.T) {
	for _, mode := range []string{"steer", "interrupt"} {
		for _, secondaryPrivacy := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s-%t", mode, secondaryPrivacy), func(t *testing.T) {
				primaryAdapterBusyOriginUsesRoutedPrivacy(t, mode, secondaryPrivacy)
			})
		}
	}
}

func primaryAdapterBusyOriginUsesRoutedPrivacy(t *testing.T, mode string, secondaryPrivacy bool) {
	tmp := t.TempDir()
	homeDir := filepath.Join(tmp, ".icebath")
	secondary := filepath.Join(homeDir, "profiles", "research")
	if err := os.MkdirAll(secondary, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", homeDir)
	t.Setenv("ICEBATH_GATEWAY_BUSY_ACK_ENABLED", "false")
	t.Setenv("TELEGRAM_ALLOW_ALL_USERS", "true")
	home.ResetKeyCache()
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	for dir, privacy := range map[string]bool{homeDir: !secondaryPrivacy, secondary: secondaryPrivacy} {
		body := fmt.Sprintf("privacy:\n  redact_pii: %t\n", privacy)
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	r := allocGatewayRunner()
	r.config = gateway.DefaultGatewayConfig(ctx)
	on := true
	r.config.MultiplexProfiles = &on
	r.busyInputMode, r.busyTextMode = mode, "interrupt"
	r.profileAdapters = pydict.New[string, *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]]()
	r.adapters = pydict.New[gateway.Platform, gateway.PlatformAdapter]()
	r.sessions = map[string]*SessionState{}
	r.hooks = NewProfileHookRegistries()
	r.config.ProfileRoutes = routesOf(t, map[string]any{"name": "research-chat", "platform": "telegram", "profile": "research", "chat_id": "chat-1"})
	adapter := &cleanupCaptureAdapter{}
	platformConfig := gateway.NewPlatformConfig()
	platformConfig.Enabled, platformConfig.Token = true, "test-token"
	adapter.BasePlatformAdapter = gateway.NewBasePlatformAdapter(platformConfig, gateway.PlatformTelegram, adapter)
	r.adapters.Set(gateway.PlatformTelegram, adapter)
	r.wireAdapterHandlers(ctx, adapter, wireAdapterOptions{})
	adapter.GatewayRunner = r
	user := "user-1"
	event := gateway.NewMessageEvent("follow up")
	messageID := "message-1"
	event.MessageID = &messageID
	event.Source = adapter.BuildSource(ctx, "chat-1", gateway.SourceFields{UserID: &user})
	if event.Source.Profile == nil || *event.Source.Profile != "research" {
		t.Fatalf("source profile = %v", event.Source.Profile)
	}
	original := *event.Source
	key, err := r.sessionKeyForSource(ctx, event.Source)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.NewAIAgent(ctx, agent.AIAgentOptions{
		APIKey: "offline-test", BaseURL: "http://127.0.0.1:1/v1", Provider: "openai-compat", Model: "test-model",
		EnabledToolsets: []string{}, QuietMode: true, SkipContextFiles: true, SkipMemory: true, Platform: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	a.SetExecutingToolsForTests(mode == "interrupt")
	r.sessionState(key).turn.agent = a
	adapter.InstallSessionGuard(key, gateway.NewInterruptEvent())
	ambient := home.Override(ctx)
	if err := adapter.HandleMessageWhileActive(ctx, event, key); err != nil {
		t.Fatal(err)
	}
	messages := []*state.Message{{Role: "tool", ToolCallID: "probe", Content: "Tool completed."}}
	a.ApplyPendingSteer(&messages, 1)
	output := fmt.Sprint(messages[len(messages)-1].Content)
	if !strings.Contains(output, "follow up") {
		t.Fatalf("steer output = %q", output)
	}
	for _, value := range []string{event.Source.ChatID, *event.Source.UserID, *event.MessageID, "research"} {
		if absent := !strings.Contains(output, value); absent != secondaryPrivacy {
			t.Fatalf("value %q absent=%t with secondary privacy %t in %q", value, absent, secondaryPrivacy, output)
		}
	}
	after, _ := json.Marshal(event.Source)
	before, _ := json.Marshal(&original)
	if string(after) != string(before) {
		t.Fatalf("source fields changed: %s != %s", after, before)
	}
	if home.Override(ctx) != ambient {
		t.Fatalf("home override changed to %q", home.Override(ctx))
	}
	if _, parked := adapter.PeekPendingMessage(key); parked {
		t.Fatalf("pending slot holds %s", key)
	}
}

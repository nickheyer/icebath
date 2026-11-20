package gatewayrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Restart test adapter exposing the DM topic lookup a real Telegram adapter has
type dmTopicRestartAdapter struct {
	*restartTestAdapter
}

func (a *dmTopicRestartAdapter) GetDMTopicInfo(string, string) (any, error) {
	return map[string]any{"name": "Ops Topic"}, nil
}

// Relay adapter fronting Slack that records its logical sends
type relayRecorder struct {
	*restartTestAdapter
	relayMu sync.Mutex
	relayed []relayedSend
}

// One recorded relay send
type relayedSend struct {
	platform gateway.Platform
	chatID   string
	content  string
	metadata map[string]any
}

func (a *relayRecorder) FrontsPlatform(platform gateway.Platform) bool {
	return platform == gateway.PlatformSlack
}
func (a *relayRecorder) SendForPlatform(_ context.Context, platform gateway.Platform, chatID, content string, metadata map[string]any) (gateway.SendResult, error) {
	a.relayMu.Lock()
	defer a.relayMu.Unlock()
	a.relayed = append(a.relayed, relayedSend{platform, chatID, content, metadata})
	return gateway.SendResult{Success: true, MessageID: "home"}, nil
}

// Runner served only by a Slack fronting relay
func relayRestartRunner(t *testing.T, slackHome *gateway.HomeChannel) (*GatewayRunner, *relayRecorder) {
	t.Helper()
	r, _ := makeRestartRunner(t)
	relay := &relayRecorder{restartTestAdapter: newRestartTestAdapter()}
	r.adapters = adapterDict(gateway.PlatformRelay, relay)
	relayCfg := gateway.NewPlatformConfig()
	relayCfg.Enabled = true
	slackCfg := gateway.NewPlatformConfig()
	slackCfg.HomeChannel = slackHome
	r.config.Platforms = map[gateway.Platform]*gateway.PlatformConfig{gateway.PlatformRelay: relayCfg, gateway.PlatformSlack: slackCfg}
	return r, relay
}

func TestSendHomeChannelStartupNotificationPreservesThreadMetadata(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	r, base := makeRestartRunner(t)
	thread := "777"
	home := gateway.NewHomeChannel(gateway.PlatformTelegram, "parent-42", "Ops Topic")
	home.ThreadID = &thread
	r.config.Platforms[gateway.PlatformTelegram].HomeChannel = home
	adapter := &dmTopicRestartAdapter{restartTestAdapter: base}
	r.adapters.Set(gateway.PlatformTelegram, adapter)
	delivered := r.sendHomeChannelStartupNotifications(context.Background(), nil)
	want := map[noticeTargetKey]bool{{platform: "telegram", chatID: "parent-42", threadID: "777", threaded: true}: true}
	if !reflect.DeepEqual(delivered, want) {
		t.Fatalf("delivered %v", delivered)
	}
	calls := base.calls()
	if len(calls) != 1 || calls[0].chatID != "parent-42" || calls[0].content != "♻️ Gateway online — Icebath is back and ready." {
		t.Fatalf("calls %+v", calls)
	}
	wantMeta := map[string]any{"thread_id": "777", "telegram_dm_topic_reply_fallback": true, "direct_messages_topic_id": "777"}
	if !reflect.DeepEqual(calls[0].metadata, wantMeta) {
		t.Fatalf("metadata %v", calls[0].metadata)
	}
}

func TestRelayFrontedLogicalHomeGetsStartupNotification(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	user, scope := "U123", "T123"
	home := gateway.NewHomeChannel(gateway.PlatformSlack, "D123", "Owner DM")
	home.UserID, home.ScopeID = &user, &scope
	r, relay := relayRestartRunner(t, home)
	delivered := r.sendHomeChannelStartupNotifications(context.Background(), nil)
	if !reflect.DeepEqual(delivered, map[noticeTargetKey]bool{{platform: "slack", chatID: "D123"}: true}) {
		t.Fatalf("delivered %v", delivered)
	}
	if len(relay.relayed) != 1 {
		t.Fatalf("relayed %+v", relay.relayed)
	}
	got := relay.relayed[0]
	if got.platform != gateway.PlatformSlack || got.chatID != "D123" || got.content != "♻️ Gateway online — Icebath is back and ready." {
		t.Fatalf("relay send %+v", got)
	}
	if got.metadata["user_id"] != "U123" || got.metadata["scope_id"] != "T123" {
		t.Fatalf("metadata %v", got.metadata)
	}
}

func TestRelayRestartNotificationUsesLogicalPlatformAndOwner(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	notifyPath := filepath.Join(dir, ".restart_notify.json")
	raw, _ := json.Marshal(map[string]any{"platform": "slack", "chat_id": "D123", "chat_type": "dm", "user_id": "U123", "scope_id": "T123", "delivered_via_upstream_relay": true})
	if err := os.WriteFile(notifyPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	r, relay := relayRestartRunner(t, nil)
	target := r.sendRestartNotification(context.Background())
	if target == nil || target.platform != "slack" || target.chatID != "D123" || target.threadID != nil {
		t.Fatalf("target %+v", target)
	}
	if len(relay.relayed) != 1 || relay.relayed[0].platform != gateway.PlatformSlack || relay.relayed[0].chatID != "D123" {
		t.Fatalf("relayed %+v", relay.relayed)
	}
	if relay.relayed[0].metadata["user_id"] != "U123" || relay.relayed[0].metadata["scope_id"] != "T123" {
		t.Fatalf("metadata %v", relay.relayed[0].metadata)
	}
	if _, err := os.Stat(notifyPath); err == nil {
		t.Fatal("notify marker kept")
	}
}

// Telegram adapter whose sends report a provider failure
type failingRestartAdapter struct {
	*restartTestAdapter
}

func (a *failingRestartAdapter) Send(context.Context, string, string, string, map[string]any) (gateway.SendResult, error) {
	return gateway.SendResult{Success: false, Error: "Chat not found"}, nil
}

func TestSendRestartNotificationLogsWarningOnSendresultFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	notifyPath := filepath.Join(dir, ".restart_notify.json")
	if err := os.WriteFile(notifyPath, []byte(`{"platform": "telegram", "chat_id": "42"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r, base := makeRestartRunner(t)
	r.adapters.Set(gateway.PlatformTelegram, &failingRestartAdapter{restartTestAdapter: base})
	if target := r.sendRestartNotification(context.Background()); target != nil {
		t.Fatalf("target %+v", target)
	}
	if _, err := os.Stat(notifyPath); err == nil {
		t.Fatal("notify marker kept")
	}
}

// Slack adapter recording public and private notice sends
type noticeAdapter struct {
	*gateway.BasePlatformAdapter
	mu      sync.Mutex
	public  []sentCall
	private []sentCall
}

func (a *noticeAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *noticeAdapter) Disconnect(context.Context) error            { return nil }
func (a *noticeAdapter) Send(_ context.Context, chatID, content, _ string, metadata map[string]any) (gateway.SendResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.public = append(a.public, sentCall{chatID, content, metadata})
	return gateway.SendResult{Success: true, MessageID: "public-1"}, nil
}
func (a *noticeAdapter) SendPrivateNotice(_ context.Context, chatID string, userID *string, content, _ string, metadata map[string]any) (gateway.SendResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.private = append(a.private, sentCall{chatID + "|" + *userID, content, metadata})
	return gateway.SendResult{Success: true, MessageID: "private-1"}, nil
}
func (a *noticeAdapter) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"id": chatID}, nil
}

func TestDeliverPlatformNoticeUsesPrivateDeliveryWhenConfigured(t *testing.T) {
	r := allocGatewayRunner()
	r.config = gateway.DefaultGatewayConfig(context.Background())
	cfg := gateway.NewPlatformConfig()
	cfg.Enabled = true
	cfg.Token = "***"
	cfg.Extra = map[string]any{"notice_delivery": "private"}
	r.config.Platforms = map[gateway.Platform]*gateway.PlatformConfig{gateway.PlatformSlack: cfg}
	adapter := &noticeAdapter{}
	adapterCfg := gateway.NewPlatformConfig()
	adapterCfg.Enabled = true
	adapter.BasePlatformAdapter = gateway.NewBasePlatformAdapter(adapterCfg, gateway.PlatformSlack, adapter)
	r.adapters = adapterDict(gateway.PlatformSlack, adapter)
	user, thread := "U123", "111.222"
	source := &gateway.SessionSource{Platform: gateway.PlatformSlack, ChatID: "C123", ChatType: "channel", UserID: &user, ThreadID: &thread}
	if err := r.deliverPlatformNotice(context.Background(), source, "hello"); err != nil {
		t.Fatal(err)
	}
	want := []sentCall{{"C123|U123", "hello", map[string]any{"thread_id": "111.222", "user_id": "U123"}}}
	if !reflect.DeepEqual(adapter.private, want) || len(adapter.public) != 0 {
		t.Fatalf("private %+v public %+v", adapter.private, adapter.public)
	}
}

func TestGatewayDrainRetainsAndFormatsOverflowEvents(t *testing.T) {
	queue := tools.NewEventQueue()
	tripped := map[string]any{"type": "watch_overflow_tripped", "message": "watch flood detected: 47 notifications suppressed for pattern 'ERROR'", "session_id": "proc_a1b2"}
	released := map[string]any{"type": "watch_overflow_released", "message": "watch flood released: notifications resumed for pattern 'ERROR'", "session_id": "proc_a1b2"}
	queue.Put(tripped)
	queue.Put(released)
	if retained := drainGatewayWatchEvents(queue); !reflect.DeepEqual(retained, []map[string]any{tripped, released}) {
		t.Fatalf("retained %v", retained)
	}
	outTripped := gateway.FormatGatewayProcessNotification(tripped)
	if !strings.Contains(outTripped, "47 notifications suppressed") || strings.Contains(outTripped, "exit code") {
		t.Fatalf("tripped %q", outTripped)
	}
	outReleased := gateway.FormatGatewayProcessNotification(released)
	if !strings.Contains(outReleased, "notifications resumed") || strings.Contains(outReleased, "exit code") {
		t.Fatalf("released %q", outReleased)
	}
}

// Gateway config whose Slack platform carries the given extra
func configWithSlackExtra(extra map[string]any) *gateway.GatewayConfig {
	cfg := gateway.DefaultGatewayConfig(context.Background())
	slack := gateway.NewPlatformConfig()
	slack.Enabled = true
	slack.Extra = extra
	cfg.Platforms = map[gateway.Platform]*gateway.PlatformConfig{gateway.PlatformSlack: slack}
	return cfg
}

func TestSlackIgnoredChannelsEnvBridgeFallback(t *testing.T) {
	t.Setenv("SLACK_IGNORED_CHANNELS", "C_ENV1, C_ENV2")
	ctx := context.Background()
	cfg := configWithSlackExtra(map[string]any{})
	if got := slackIgnoredChannelsFromGatewayConfig(ctx, cfg, nil); !reflect.DeepEqual(got, map[string]bool{"C_ENV1": true, "C_ENV2": true}) {
		t.Fatalf("ignored %v", got)
	}
	if !isSlackIgnoredChannel(ctx, cfg, "C_ENV1", nil) || isSlackIgnoredChannel(ctx, cfg, "C_OTHER", nil) {
		t.Fatal("membership")
	}
}

func TestSlackIgnoredChannelsExtraWinsOverEnv(t *testing.T) {
	t.Setenv("SLACK_IGNORED_CHANNELS", "C_ENV")
	cfg := configWithSlackExtra(map[string]any{"ignored_channels": []any{"C_CFG"}})
	if got := slackIgnoredChannelsFromGatewayConfig(context.Background(), cfg, nil); !reflect.DeepEqual(got, map[string]bool{"C_CFG": true}) {
		t.Fatalf("ignored %v", got)
	}
}

func TestGatewayDedupIdentitySeparatesNoticesFromTheFinalAndFromEachOther(t *testing.T) {
	n0 := map[string]any{"type": "async_delegation", "delegation_id": "d", "task_failure_notice": true, "results": []any{map[string]any{"task_index": 0}}}
	n1 := map[string]any{"type": "async_delegation", "delegation_id": "d", "task_failure_notice": true, "results": []any{map[string]any{"task_index": 1}}}
	final := map[string]any{"type": "async_delegation", "delegation_id": "d", "is_batch": true, "results": []any{}}
	identities := map[completionIdentity]bool{}
	for _, evt := range []map[string]any{n0, n1, final} {
		identity, ok := completionDeliveryIdentity(evt)
		if !ok {
			t.Fatalf("no identity for %v", evt)
		}
		identities[identity] = true
	}
	if len(identities) != 3 {
		t.Fatalf("identities %v", identities)
	}
}

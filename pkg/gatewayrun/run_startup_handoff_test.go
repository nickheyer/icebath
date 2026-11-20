package gatewayrun

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
)

// Adapter whose handoff thread creation answers a fixed thread id
type handoffTestAdapter struct {
	*gateway.BasePlatformAdapter
	thread string
}

func (a *handoffTestAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *handoffTestAdapter) Disconnect(context.Context) error            { return nil }
func (a *handoffTestAdapter) Send(context.Context, string, string, string, map[string]any) (gateway.SendResult, error) {
	return gateway.SendResult{Success: true}, nil
}
func (a *handoffTestAdapter) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"id": chatID}, nil
}
func (a *handoffTestAdapter) CreateHandoffThread(context.Context, string, string) (string, bool, error) {
	return a.thread, true, nil
}

// Key the handoff builds for a thread destination, the Discord thread keyed on its own id
func handoffThreadKey(t *testing.T, platform gateway.Platform, homeChatID, threadID string) string {
	t.Helper()
	chatID := homeChatID
	if platform == gateway.PlatformDiscord && threadID != "" {
		chatID = threadID
	}
	user, name := "system:handoff", "Handoff"
	key, err := gateway.BuildSessionKey(context.Background(), &gateway.SessionSource{Platform: platform, ChatID: chatID, ChatType: "thread", UserID: &user, UserName: &name, ThreadID: &threadID}, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// Real handoff destination and key for a configured home channel served by a thread creating adapter
func runHandoffDestination(t *testing.T, platform gateway.Platform, channelID, threadID string, scopeID *string) (*handoffDestination, string) {
	t.Helper()
	ctx := context.Background()
	cfg := gateway.DefaultGatewayConfig(ctx)
	pc := gateway.NewPlatformConfig()
	pc.Enabled, pc.Token = true, "test"
	home := gateway.NewHomeChannel(platform, channelID, "home")
	home.ScopeID = scopeID
	pc.HomeChannel = home
	cfg.Platforms = map[gateway.Platform]*gateway.PlatformConfig{platform: pc}
	adapter := &handoffTestAdapter{thread: threadID}
	adapter.BasePlatformAdapter = gateway.NewBasePlatformAdapter(pc, platform, adapter)
	r := allocGatewayRunner()
	r.config = cfg
	r.adapters = pydict.New[gateway.Platform, gateway.PlatformAdapter]()
	r.adapters.Set(platform, adapter)
	dest, err := r.handoffResolveDestination(ctx, map[string]any{"id": "cli-session", "title": "work", "handoff_platform": string(platform)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	key, err := r.handoffSessionKey(ctx, dest, nil)
	if err != nil {
		t.Fatal(err)
	}
	return dest, key
}

// Key the Slack adapter builds for a thread reply in a channel
func organicSlackReplyKey(t *testing.T, channelID, threadTS, teamID, chatType string) string {
	t.Helper()
	user := "U123456"
	key, err := gateway.BuildSessionKey(context.Background(), &gateway.SessionSource{Platform: gateway.PlatformSlack, ChatID: channelID, ChatType: chatType, UserID: &user, ThreadID: &threadTS, ScopeID: &teamID}, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestDiscordHandoffKeyMatchesOrganicInThreadKey(t *testing.T) {
	parentID, threadID, userID := "1523581766923845724", "1523590238595846166", "171164909650968576"
	organic, err := gateway.BuildSessionKey(context.Background(), &gateway.SessionSource{Platform: gateway.PlatformDiscord, ChatID: threadID, ChatType: "thread", UserID: &userID, ThreadID: &threadID, ParentChatID: &parentID}, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	handoff := handoffThreadKey(t, gateway.PlatformDiscord, parentID, threadID)
	if handoff != organic || handoff != "agent:main:discord:thread:"+threadID+":"+threadID {
		t.Fatalf("handoff %q organic %q", handoff, organic)
	}
}

func TestDiscordHandoffKeyDoesNotUseParentChannel(t *testing.T) {
	parentID, threadID := "1523581766923845724", "1523590238595846166"
	if handoffThreadKey(t, gateway.PlatformDiscord, parentID, threadID) == "agent:main:discord:thread:"+parentID+":"+threadID {
		t.Fatal("handoff regressed to keying on the parent channel")
	}
}

func TestSlackDmHandoffKeyMatchesTheThreadReplyKey(t *testing.T) {
	team := "T0C2HL96FH6"
	dest, handoff := runHandoffDestination(t, gateway.PlatformSlack, "D0C1HFBMQAX", "1789474088.089709", &team)
	if handoff != organicSlackReplyKey(t, "D0C1HFBMQAX", "1789474088.089709", team, "dm") || dest.source.ChatID != "D0C1HFBMQAX" {
		t.Fatalf("handoff %q chat %q", handoff, dest.source.ChatID)
	}
}

func TestSlackChannelHandoffKeyMatchesTheThreadReplyKey(t *testing.T) {
	team := "T0C2HL96FH6"
	dest, handoff := runHandoffDestination(t, gateway.PlatformSlack, "C0CHANNEL01", "1789474088.089709", &team)
	if handoff != organicSlackReplyKey(t, "C0CHANNEL01", "1789474088.089709", team, "group") || dest.source.ChatID != "C0CHANNEL01" {
		t.Fatalf("handoff %q chat %q", handoff, dest.source.ChatID)
	}
}

func TestTelegramForumHandoffKeyMatchesTheTopicReplyKey(t *testing.T) {
	_, handoff := runHandoffDestination(t, gateway.PlatformTelegram, "-1001234567", "77", nil)
	user, thread := "42", "77"
	want, err := gateway.BuildSessionKey(context.Background(), &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "-1001234567", ChatType: "group", UserID: &user, ThreadID: &thread}, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if handoff != want || handoff != "agent:main:telegram:group:-1001234567:77" || strings.Contains(handoff, "thread") {
		t.Fatalf("handoff %q want %q", handoff, want)
	}
}

package yuanbao

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Adapter built from an extra without the test defaults, or the construction error
func buildAdapter(ctx context.Context, extra map[string]any, homeChannel *gateway.HomeChannel) (*YuanbaoAdapter, error) {
	config := gateway.NewPlatformConfig()
	config.Extra, config.HomeChannel = extra, homeChannel
	return NewYuanbaoAdapter(ctx, config)
}

func TestConstructorReadsCredentialsEndpointsAndHomeState(t *testing.T) {
	isolatedHome(t)
	ctx := context.Background()
	adapter, err := buildAdapter(ctx, map[string]any{"app_id": "  key  ", "app_secret": " sec ", "api_domain": "https://api.example.com//",
		"route_env": " pre ", "dm_policy": " Allowlist ", "dm_allow_from": "u1, ,u2"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if adapter.appKey != "key" || adapter.appSecret != "sec" || adapter.apiDomain != "https://api.example.com" || adapter.wsURL != defaultWSGatewayURL ||
		adapter.routeEnv != "pre" || adapter.botIDValue() != nil || adapter.MediaResolveConcurrency != defaultResolveConcurrency ||
		adapter.autoSethomeDoneValue() || !adapter.EnforcesOwnAccessPolicy() || !adapter.Traits.SplitsLongMessages || adapter.Name() != "Yuanbao" {
		t.Fatalf("adapter %+v", adapter)
	}
	policy := adapter.accessPolicy
	if policy.DMPolicy != "allowlist" || policy.GroupPolicy != "pairing" || !reflect.DeepEqual(policy.AllowFromSnapshot(), map[string]bool{"u1": true, "u2": true}) {
		t.Fatalf("policy %q %q %v", policy.DMPolicy, policy.GroupPolicy, policy.AllowFromSnapshot())
	}
	for extraKey, want := range map[string]string{"app_id": "'int' object has no attribute 'strip'", "api_domain": "'int' object has no attribute 'rstrip'",
		"group_allow_from": "'int' object has no attribute 'split'"} {
		if _, err := buildAdapter(ctx, map[string]any{extraKey: int64(42)}, nil); err == nil || err.Error() != want {
			t.Fatalf("%s of the wrong type: %v", extraKey, err)
		}
	}
	for chatID, done := range map[string]bool{"direct:alice": true, "group:grp-1": false} {
		adapter, err := buildAdapter(ctx, nil, gateway.NewHomeChannel(gateway.PlatformYuanbao, chatID, "Home"))
		if err != nil || adapter.autoSethomeDoneValue() != done {
			t.Fatalf("home %s done %v err %v", chatID, adapter.autoSethomeDoneValue(), err)
		}
	}
	t.Setenv("YUANBAO_HOME_CHANNEL", "direct:env-user")
	t.Setenv("YUANBAO_GROUP_POLICY", "OPEN")
	if adapter, err := buildAdapter(ctx, nil, nil); err != nil || !adapter.autoSethomeDoneValue() || adapter.accessPolicy.GroupPolicy != "open" {
		t.Fatalf("env settings ignored %v", err)
	}
}

func TestScopedConstructionReadsAuthzFromTheSecretScope(t *testing.T) {
	isolatedHome(t)
	t.Setenv("YUANBAO_DM_POLICY", "pairing")
	t.Setenv("YUANBAO_DM_ALLOW_FROM", "default-user")
	scoped := withMultiplexScope(t, map[string]string{"YUANBAO_DM_POLICY": "allowlist", "YUANBAO_DM_ALLOW_FROM": "scoped-user"})
	adapter, err := buildAdapter(scoped, nil, nil)
	if err != nil || adapter.accessPolicy.DMPolicy != "allowlist" || !reflect.DeepEqual(adapter.accessPolicy.AllowFromSnapshot(), map[string]bool{"scoped-user": true}) {
		t.Fatalf("scoped authz %v", err)
	}
	t.Setenv("YUANBAO_DM_POLICY", "allowlist")
	miss := withMultiplexScope(t, map[string]string{"SOMETHING_ELSE": "x"})
	adapter, err = buildAdapter(miss, nil, nil)
	if err != nil || adapter.accessPolicy.DMPolicy != "pairing" || len(adapter.accessPolicy.AllowFromSnapshot()) != 0 {
		t.Fatalf("a scoped miss borrowed the default profile authz %v", err)
	}
}

func TestActiveAdapterIsTrackedPerProfileHome(t *testing.T) {
	first, second := makeAdapter(t, nil), makeAdapter(t, nil)
	ctx := context.Background()
	scoped := home.WithOverride(ctx, t.TempDir())
	SetActive(ctx, first)
	SetActive(scoped, second)
	t.Cleanup(func() {
		clearActive(first)
		clearActive(second)
	})
	if GetActive(ctx) != first || GetActive(scoped) != second || GetActive(home.WithOverride(ctx, t.TempDir())) != nil {
		t.Fatal("each home must see its own adapter")
	}
	SetActive(scoped, nil)
	if GetActive(scoped) != nil || GetActive(ctx) != first {
		t.Fatal("clearing a profile slot must leave the unscoped adapter")
	}
	SetActive(scoped, first)
	if err := first.Disconnect(ctx); err != nil || GetActive(ctx) != nil || GetActive(scoped) != nil {
		t.Fatalf("a disconnected adapter stayed registered %v", err)
	}
}

func TestTurnCleanupClearsOnlyTheRecallTrackingItStillOwns(t *testing.T) {
	const sessionKey = "yuanbao:group:G:user:U"
	cases := []struct{ name, tracked, eventID, during, wantID, wantText string }{
		{"finished turn", "m1", "m1", "", "", ""},
		{"event without an id", "", "", "m2", "m2", "late text"},
		{"overwritten entry", "m1", "m1", "m2", "m2", "late text"},
	}
	for _, c := range cases {
		adapter := makeAdapter(t, nil)
		if c.tracked != "" {
			adapter.recordProcessing(sessionKey, c.tracked, "first text")
		}
		notifiersDuringTurn := -1
		swap(t, &baseProcessMessageBackground, func(_ context.Context, a *YuanbaoAdapter, _ *gateway.MessageEvent, _ string) error {
			a.outbound.slowNotifier.mu.Lock()
			notifiersDuringTurn = len(a.outbound.slowNotifier.tasks)
			a.outbound.slowNotifier.mu.Unlock()
			if c.during != "" {
				a.recordProcessing(sessionKey, c.during, "late text")
			}
			return nil
		})
		event := gateway.NewMessageEvent("x")
		event.Source = &gateway.SessionSource{Platform: gateway.PlatformYuanbao, ChatID: "chat-1"}
		if c.eventID != "" {
			event.MessageID = &c.eventID
		}
		if err := adapter.ProcessMessageBackground(context.Background(), event, sessionKey); err != nil {
			t.Fatal(err)
		}
		var want [][2]string
		if c.wantID != "" {
			want = [][2]string{{sessionKey, c.wantID}}
		}
		if got := adapter.processingSnapshot(); len(got) != len(want) || (len(want) == 1 && got[0] != want[0]) || adapter.processingText(sessionKey) != c.wantText {
			t.Fatalf("%s: tracking %v text %q", c.name, got, adapter.processingText(sessionKey))
		}
		adapter.outbound.slowNotifier.mu.Lock()
		notifiersAfterTurn := len(adapter.outbound.slowNotifier.tasks)
		adapter.outbound.slowNotifier.mu.Unlock()
		if notifiersDuringTurn != 1 || notifiersAfterTurn != 0 {
			t.Fatalf("%s: slow notifier ran %d during and %d after the turn", c.name, notifiersDuringTurn, notifiersAfterTurn)
		}
	}
}

func TestMentionsResolveAgainstTheFreshMemberCacheOnly(t *testing.T) {
	adapter := makeAdapter(t, nil)
	members := []any{pyjson.ObjectOf("nickname", "Bob", "user_id", "u1"), pyjson.ObjectOf("nick_name", "carol", "user_id", "")}
	adapter.memberCache["g-stale"] = memberCacheEntry{wallClock() - 301, members}
	adapter.memberCache["g-fresh"] = memberCacheEntry{wallClock() - 10, members}
	render := func(text, group string) string {
		body, err := adapter.outbound.sender.buildMsgBodyWithMentions(text, group)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := pyjson.Dumps(body, pyjson.EnsureASCII(false))
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	plain := `[{"msg_type": "TIMTextElem", "msg_content": {"text": "hi @bob"}}]`
	if got := render("hi @bob", "g-stale"); got != plain {
		t.Fatalf("stale cache body %s", got)
	}
	if _, kept := adapter.memberCache["g-stale"]; kept || len(adapter.memberCache) != 1 {
		t.Fatal("an expired entry must be evicted on read")
	}
	want := `[{"msg_type": "TIMTextElem", "msg_content": {"text": "hi"}}, ` +
		`{"msg_type": "TIMCustomElem", "msg_content": {"data": "{\"elem_type\": 1002, \"text\": \"@Bob\", \"user_id\": \"u1\"}"}}, ` +
		`{"msg_type": "TIMTextElem", "msg_content": {"text": "and"}}, {"msg_type": "TIMTextElem", "msg_content": {"text": "@carol"}}, ` +
		`{"msg_type": "TIMTextElem", "msg_content": {"text": "你好 mail@bob"}}]`
	if got := render("hi @bob and @carol 你好 mail@bob", "g-fresh"); got != want {
		t.Fatalf("fresh cache body %s", got)
	}
	if _, kept := adapter.memberCache["g-fresh"]; !kept {
		t.Fatal("a fresh entry must survive the read")
	}
	adapter.memberCache["g-odd"] = memberCacheEntry{wallClock(), []any{pyjson.ObjectOf("nickname", int64(7), "user_id", "u9")}}
	if _, err := adapter.outbound.sender.buildMsgBodyWithMentions("hi", "g-odd"); err == nil || err.Error() != "'int' object has no attribute 'lower'" {
		t.Fatalf("a nickname that is not text: %v", err)
	}
}

func TestChatLockEvictionPrefersUnlockedEntries(t *testing.T) {
	sender := makeAdapter(t, nil).outbound.sender
	held := sender.getChatLock("chat_0")
	if err := held.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer held.release()
	for i := 1; i < chatDictMaxSize; i++ {
		sender.getChatLock(fmt.Sprintf("chat_%d", i))
	}
	fresh := sender.getChatLock("new_chat")
	if fresh == nil || sender.chatLocks.Len() != chatDictMaxSize || !sender.chatLocks.Has("chat_0") || sender.chatLocks.Has("chat_1") || !sender.chatLocks.Has("new_chat") {
		t.Fatalf("eviction kept %d locks", sender.chatLocks.Len())
	}
	if again := sender.getChatLock("chat_2"); again == nil || sender.chatLocks.Keys()[chatDictMaxSize-1] != "chat_2" {
		t.Fatal("a reused lock must move to the recent end")
	}
}

func TestSendDMHonoursTheDMPolicyAndCutsLongText(t *testing.T) {
	adapter, server := connectAdapter(t, nil)
	ctx := context.Background()
	adapter.accessPolicy = makePolicy(t, "allowlist", []string{"u1"}, "pairing", nil)
	if denied, err := adapter.SendDM(ctx, "u2", "hi", ""); err != nil || denied.Success || denied.Error != "DM access denied for this user" ||
		len(server.sent("send_c2c_message")) != 0 {
		t.Fatalf("denied DM %+v %v", denied, err)
	}
	sent, err := adapter.SendDM(ctx, " u1 ", strings.Repeat("好", dmMaxChars+5), "grp-9")
	if err != nil || !sent.Success {
		t.Fatalf("allowed DM %+v %v", sent, err)
	}
	var delivered strings.Builder
	frames := server.sent("send_c2c_message")
	for _, frame := range frames {
		delivered.WriteString(firstText(t, frame, 5))
	}
	fields, err := parseDict(frames[0].Vals["data"].([]byte))
	if err != nil {
		t.Fatal(err)
	}
	text := delivered.String()
	if len(frames) != 3 || pystr.Len(firstText(t, frames[0], 5)) != maxTextChunk || !strings.HasPrefix(text, strings.Repeat("好", dmMaxChars)) ||
		strings.Count(text, "好") != dmMaxChars || !strings.HasSuffix(text, i18n.T(ctx, "platform.shared.truncated_suffix", "", nil)) ||
		fields.GetString(6) != "grp-9" || fields.GetString(2) != " u1 " {
		t.Fatalf("long DM went out as %d frames ending %q", len(frames), pystr.Slice(text, pystr.Len(text)-20, pystr.Len(text)))
	}
}

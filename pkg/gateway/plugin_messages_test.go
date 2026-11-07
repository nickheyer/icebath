package gateway

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
)

type msgSource struct {
	platform Platform
	chat     string
}

func (s msgSource) SourcePlatform() Platform { return s.platform }
func (s msgSource) SourceChatID() string     { return s.chat }

type msgAdapter struct{ calls [][]string }

func (a *msgAdapter) Send(_ context.Context, chatID, content, _ string, metadata map[string]any) (PluginSendResult, error) {
	if metadata["_interim_send"] != true {
		panic("sealed as the turn's answer")
	}
	a.calls = append(a.calls, []string{"send", chatID, content})
	return PluginSendResult{Success: true, MessageID: "m" + string(rune('0'+len(a.calls)))}, nil
}

type editingAdapter struct{ msgAdapter }

func (a *editingAdapter) EditMessage(_ context.Context, chatID, messageID, content string, _ bool) (PluginSendResult, error) {
	a.calls = append(a.calls, []string{"edit", chatID, messageID, content})
	return PluginSendResult{Success: true, MessageID: messageID}, nil
}

type raisingEditAdapter struct{ msgAdapter }

func (a *raisingEditAdapter) EditMessage(context.Context, string, string, string, bool) (PluginSendResult, error) {
	return PluginSendResult{}, errors.New("malformed edit response")
}

type failedEditAdapter struct{ msgAdapter }

func (a *failedEditAdapter) EditMessage(context.Context, string, string, string, bool) (PluginSendResult, error) {
	return PluginSendResult{Success: false}, nil
}

type msgRunner struct {
	stored  map[string]PluginMessageSource
	adapter PluginMessageAdapter
	owned   bool
	live    PluginLiveSession
}

func (r *msgRunner) PluginStoredSource(key string) (PluginMessageSource, bool) {
	s, ok := r.stored[key]
	return s, ok
}
func (r *msgRunner) SessionKeyForSource(s PluginMessageSource) string {
	return "agent:main:" + string(s.SourcePlatform()) + ":dm:" + s.SourceChatID()
}
func (r *msgRunner) PluginLocalSession(PluginMessageSource, string, string, string) PluginLiveSession {
	return r.live
}
func (r *msgRunner) PluginMessagingSessionOwned(string, string, string) bool { return r.owned }
func (r *msgRunner) DeliveryAdapterFor(PluginMessageSource) PluginMessageAdapter {
	return r.adapter
}
func (r *msgRunner) ThreadMetadataForSource(PluginMessageSource) (map[string]any, error) {
	return nil, errors.New("no thread")
}
func (r *msgRunner) LoopRunning() bool                      { return true }
func (r *msgRunner) OnGatewayLoop(ctx context.Context) bool { return ctx.Value(liveSourceKey{}) != nil }

func sentCalls(a PluginMessageAdapter) [][]string {
	switch x := a.(type) {
	case *msgAdapter:
		return x.calls
	case *editingAdapter:
		return x.calls
	case *raisingEditAdapter:
		return x.calls
	case *failedEditAdapter:
		return x.calls
	}
	return nil
}

func TestPostThenEditTargetsTheCallingSession(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		adapter PluginMessageAdapter
		edited  bool
	}{{&editingAdapter{}, true}, {&msgAdapter{}, false}, {&raisingEditAdapter{}, false}, {&failedEditAdapter{}, false}} {
		src := msgSource{PlatformTelegram, "42"}
		runner := &msgRunner{stored: map[string]PluginMessageSource{"agent:main:telegram:dm:42": src}, adapter: c.adapter, owned: true}
		target, refusal, err := ResolvePluginMessageTarget(ctx, runner, "agent:main:telegram:dm:42", nil, "", "progress")
		if err != nil || refusal != nil {
			t.Fatalf("%v %v", refusal, err)
		}
		posted := PostPluginMessage(ctx, target, "step 1/2", "progress")
		if posted["ok"] != true || posted["message_id"] != "m1" {
			t.Fatalf("%v", posted)
		}
		result := EditPluginMessage(ctx, target, posted["message_id"], "step 2/2", "progress")
		if result["ok"] != true || result["edited"] != c.edited {
			t.Fatalf("%T %v", c.adapter, result)
		}
		want := [][]string{{"send", "42", "step 1/2"}, {"send", "42", "step 2/2"}}
		if c.edited {
			want[1] = []string{"edit", "42", "m1", "step 2/2"}
		} else if result["fallback"] != true || result["message_id"] != "m2" {
			t.Fatalf("%v", result)
		}
		if !reflect.DeepEqual(sentCalls(c.adapter), want) {
			t.Fatalf("%T %v", c.adapter, sentCalls(c.adapter))
		}
	}
}

func TestNoBoundSessionIsRefusedWithoutSending(t *testing.T) {
	adapter := &editingAdapter{}
	runner := &msgRunner{stored: map[string]PluginMessageSource{}, adapter: adapter, owned: true}
	target, refusal, err := ResolvePluginMessageTarget(context.Background(), runner, "", nil, "", "progress")
	if target != nil || err != nil || refusal["error"] != "no_conversation" || refusal["ok"] != false {
		t.Fatalf("%v %v %v", target, refusal, err)
	}
	if len(adapter.calls) != 0 {
		t.Fatalf("%v", adapter.calls)
	}
}

type msgLive struct{ events []map[string]any }

func (l *msgLive) PublishEvent(chatID string, payload map[string]any, eventType string) bool {
	l.events = append(l.events, map[string]any{"chat": chatID, "payload": payload, "type": eventType})
	return true
}

func TestPluginMessageRouteLocalSessionAndRefusals(t *testing.T) {
	ctx := context.Background()
	route := msgSource{PlatformLocal, "tui-1"}
	live := &msgLive{}
	runner := &msgRunner{stored: map[string]PluginMessageSource{}, live: live}
	bound := WithLiveSource(ctx, route)
	if BoundRoute(bound) != route || BoundRoute(ctx) != nil {
		t.Fatal("bound route")
	}
	target, refusal, err := ResolvePluginMessageTarget(ctx, runner, "agent:main:local:dm:tui-1", BoundRoute(bound), "", "p")
	if err != nil || refusal != nil {
		t.Fatalf("%v %v", refusal, err)
	}
	pt := target.(*PostTarget)
	if pt.Platform != "local" || !strings.HasPrefix(pt.NoticeKey, "plugin-message:p:") || len(pt.NoticeKey) != len("plugin-message:p:")+12 {
		t.Fatalf("%+v", pt)
	}
	edited := EditPluginMessage(ctx, target, nil, "x", "p")
	if edited["ok"] != true || edited["edited"] != true || edited["message_id"] != pt.NoticeKey || live.events[0]["type"] != "notification.show" {
		t.Fatalf("%v %v", edited, live.events)
	}
	runner.live = nil
	if _, refusal, _ := ResolvePluginMessageTarget(ctx, runner, "agent:main:local:dm:tui-1", route, "", "p"); refusal["detail"] != "the local session is not live in this plugin's profile" {
		t.Fatalf("%v", refusal)
	}
	tg := msgSource{PlatformTelegram, "9"}
	runner.stored["k"] = tg
	if _, refusal, _ := ResolvePluginMessageTarget(ctx, runner, "k", nil, "", "p"); refusal["detail"] != "the session belongs to another profile" {
		t.Fatalf("%v", refusal)
	}
	runner.owned = true
	if _, refusal, _ := ResolvePluginMessageTarget(ctx, runner, "k", nil, "", "p"); refusal["error"] != "adapter_unavailable" ||
		refusal["detail"] != "no connected telegram adapter answers this chat" {
		t.Fatalf("%v", refusal)
	}
	if _, refusal, _ := ResolvePluginMessageTarget(ctx, runner, "zz", nil, "", "p"); refusal["detail"] != "session 'zz' is not known to this gateway" {
		t.Fatalf("%v", refusal)
	}
}

type limitAdapter struct{ msgAdapter }

func (limitAdapter) MaxMessageLengthForChat(string) (int, error) { return 5, nil }
func (limitAdapter) MessageLenFnForChat(string) (func(string) int, error) {
	return func(s string) int { return len([]rune(s)) }, nil
}

func TestPluginTextIsClampedToTheChatLimit(t *testing.T) {
	if got := fitPluginText(&limitAdapter{}, "c", "abcdefgh"); got != "abcd…" {
		t.Fatalf("%q", got)
	}
	if got := fitPluginText(&msgAdapter{}, "c", "short"); got != "short" {
		t.Fatalf("%q", got)
	}
}

type msgPolicy struct{}

func (msgPolicy) PolicyPlatform() string { return "tui" }
func (msgPolicy) PolicyCwd() string      { return "/work" }

type actionRunner struct{ msgRunner }

func (actionRunner) ParsePlatform(v string) (string, error) { return v, nil }

func TestGatewayServicesInstallAndRunnerRef(t *testing.T) {
	var s plugins.Services
	InstallPluginServices(&s)
	if s.GatewayLoop == nil || s.BoundRoute == nil || s.ResolvePluginMessageTarget == nil || s.PostPluginMessage == nil ||
		s.EditPluginMessage == nil || s.ActiveSessionPolicy == nil || s.GatewayRunnerRef == nil {
		t.Fatalf("%+v", s)
	}
	ctx := context.Background()
	if _, _, ok := s.ActiveSessionPolicy(ctx); ok {
		t.Fatal("policy without a turn")
	}
	if p, cwd, ok := s.ActiveSessionPolicy(WithActivePolicy(ctx, msgPolicy{})); !ok || p != "tui" || cwd != "/work" {
		t.Fatal(p, cwd, ok)
	}
	if r, _ := s.GatewayLoop(ctx); r != nil || s.GatewayRunnerRef() != nil {
		t.Fatal("runner without a gateway")
	}
	runner := &actionRunner{}
	SetGatewayRunnerRef(runner)
	defer SetGatewayRunnerRef(nil)
	if r, on := s.GatewayLoop(ctx); r != runner || on {
		t.Fatal(r, on)
	}
	if _, on := s.GatewayLoop(WithLiveSource(ctx, msgSource{})); !on {
		t.Fatal("not on loop")
	}
	if s.GatewayRunnerRef() != runner {
		t.Fatal("platform action runner")
	}
}

func TestGetenvAndEnvValueRules(t *testing.T) {
	t.Setenv("ICEBATH_TEST_GETENV", "v")
	ctx := context.Background()
	if Getenv(ctx, "ICEBATH_TEST_GETENV", "d") != "v" || Getenv(ctx, "ICEBATH_TEST_GETENV_UNSET", "d") != "d" {
		t.Fatal("getenv")
	}
	e := NewPlatformEntry("x", "X", nil, nil)
	rule := EnvValueRule{Check: func(v string) bool { return strings.HasPrefix(v, "U") }, Message: "expected a member id"}
	if err := e.SetKeywords(map[string]any{"env_value_rules": map[string]EnvValueRule{"X_ID": rule}}); err != nil {
		t.Fatal(err)
	}
	if got := e.EnvValueRules["X_ID"]; got.Message != "expected a member id" || !got.Check("U1") || got.Check("1") {
		t.Fatalf("%+v", got)
	}
}

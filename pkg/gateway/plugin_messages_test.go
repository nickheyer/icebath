package gateway

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Telegram adapter on the base pipeline recording sends, refusing any send sealed as the turn's answer
type msgAdapter struct {
	*BasePlatformAdapter
	calls [][]string
}

func (a *msgAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *msgAdapter) Disconnect(context.Context) error            { return nil }
func (a *msgAdapter) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"id": chatID, "type": "dm"}, nil
}

func (a *msgAdapter) Send(_ context.Context, chatID, content, _ string, metadata map[string]any) (SendResult, error) {
	if metadata["_interim_send"] != true {
		panic("sealed as the turn's answer")
	}
	a.calls = append(a.calls, []string{"send", chatID, content})
	return SendResult{Success: true, MessageID: "m" + string(rune('0'+len(a.calls)))}, nil
}

func (a *msgAdapter) recorded() [][]string { return a.calls }

type editingAdapter struct{ *msgAdapter }

func (a *editingAdapter) EditMessage(_ context.Context, chatID, messageID, content string, _ bool) (SendResult, error) {
	a.calls = append(a.calls, []string{"edit", chatID, messageID, content})
	return SendResult{Success: true, MessageID: messageID}, nil
}

type raisingEditAdapter struct{ *msgAdapter }

func (a *raisingEditAdapter) EditMessage(context.Context, string, string, string, bool) (SendResult, error) {
	return SendResult{}, errors.New("malformed edit response")
}

type failedEditAdapter struct{ *msgAdapter }

func (a *failedEditAdapter) EditMessage(context.Context, string, string, string, bool) (SendResult, error) {
	return SendResult{Success: false}, nil
}

// Enabled telegram base whose self is the outer adapter, as a BasePlatformAdapter subclass builds it
func msgBase(self PlatformAdapter) *BasePlatformAdapter {
	cfg := NewPlatformConfig()
	cfg.Enabled = true
	return NewBasePlatformAdapter(cfg, PlatformTelegram, self)
}

func newMsgAdapter() *msgAdapter {
	a := &msgAdapter{}
	a.BasePlatformAdapter = msgBase(a)
	return a
}

func newEditingAdapter() *editingAdapter {
	a := &editingAdapter{&msgAdapter{}}
	a.BasePlatformAdapter = msgBase(a)
	return a
}

func newRaisingEditAdapter() *raisingEditAdapter {
	a := &raisingEditAdapter{&msgAdapter{}}
	a.BasePlatformAdapter = msgBase(a)
	return a
}

func newFailedEditAdapter() *failedEditAdapter {
	a := &failedEditAdapter{&msgAdapter{}}
	a.BasePlatformAdapter = msgBase(a)
	return a
}

// Runner double reading a real session store, every other runner attribute fixed by the case itself
type msgRunner struct {
	store   *SessionStore
	adapter PlatformAdapter
	owned   bool
	live    *LiveSession
	loop    EventLoop
}

func (r *msgRunner) SessionStore() *SessionStore { return r.store }
func (r *msgRunner) RestoredSource(_ context.Context, entry *SessionEntry) (*SessionSource, error) {
	return entry.Origin, nil
}
func (r *msgRunner) SessionKeyForSource(ctx context.Context, s *SessionSource) (string, error) {
	return r.store.GenerateSessionKey(ctx, s)
}
func (r *msgRunner) PluginLocalSession(context.Context, *SessionSource, string, string, string) (*LiveSession, error) {
	return r.live, nil
}
func (r *msgRunner) PluginMessagingSessionOwned(context.Context, string, string, string) bool {
	return r.owned
}
func (r *msgRunner) DeliveryAdapterFor(context.Context, *SessionSource) (PlatformAdapter, error) {
	return r.adapter, nil
}
func (r *msgRunner) ThreadMetadataForSource(*SessionSource, *string) map[string]any { return nil }
func (r *msgRunner) Running() bool                                                  { return true }
func (r *msgRunner) GatewayLoop() EventLoop                                         { return r.loop }
func (r *msgRunner) OnGatewayLoop(ctx context.Context) bool {
	return ctx.Value(liveSourceKey{}) != nil
}

// Session store under a fresh home holding one session per source, with the keys it gave them
func msgStore(t *testing.T, sources ...*SessionSource) (*SessionStore, []string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	store := NewSessionStore(ctx, filepath.Join(t.TempDir(), "sessions"), DefaultGatewayConfig(ctx), nil)
	t.Cleanup(store.CloseAllDBHandles)
	var keys []string
	for _, source := range sources {
		entry, err := store.GetOrCreateSession(ctx, source, false, true)
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, entry.SessionKey)
	}
	return store, keys
}

func telegramDM(chatID string) *SessionSource {
	return &SessionSource{Platform: PlatformTelegram, ChatID: chatID, ChatType: "dm", UserID: strPtr(chatID), UserName: strPtr("u")}
}

func TestPostThenEditTargetsTheCallingSession(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		adapter interface {
			PlatformAdapter
			recorded() [][]string
		}
		edited bool
	}{{newEditingAdapter(), true}, {newMsgAdapter(), false}, {newRaisingEditAdapter(), false}, {newFailedEditAdapter(), false}} {
		store, keys := msgStore(t, telegramDM("42"))
		runner := &msgRunner{store: store, adapter: c.adapter, owned: true}
		target, refusal, err := ResolvePluginMessageTarget(ctx, runner, keys[0], nil, "", "progress")
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
		if !reflect.DeepEqual(c.adapter.recorded(), want) {
			t.Fatalf("%T %v", c.adapter, c.adapter.recorded())
		}
	}
}

func TestNoBoundSessionIsRefusedWithoutSending(t *testing.T) {
	adapter := newEditingAdapter()
	store, _ := msgStore(t, telegramDM("42"))
	runner := &msgRunner{store: store, adapter: adapter, owned: true}
	target, refusal, err := ResolvePluginMessageTarget(context.Background(), runner, "", nil, "", "progress")
	if target != nil || err != nil || refusal["error"] != "no_conversation" || refusal["ok"] != false {
		t.Fatalf("%v %v %v", target, refusal, err)
	}
	if len(adapter.calls) != 0 {
		t.Fatalf("%v", adapter.calls)
	}
}

func TestPluginMessageRouteLocalSessionAndRefusals(t *testing.T) {
	ctx := context.Background()
	route := &SessionSource{Platform: PlatformLocal, ChatID: "tui-1", ChatType: "dm"}
	store, keys := msgStore(t, telegramDM("9"))
	localKey, err := store.GenerateSessionKey(ctx, route)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLiveSession(route, localKey)
	var frames []*pyjson.Object
	live.EventStream.Observers[&SessionEventObserver{Observe: func(frame *pyjson.Object) { frames = append(frames, frame) }}] = true
	runner := &msgRunner{store: store, live: live}
	bound := WithLiveSource(ctx, route)
	if BoundRoute(bound) != any(route) || BoundRoute(ctx) != nil {
		t.Fatal("bound route")
	}
	target, refusal, err := ResolvePluginMessageTarget(ctx, runner, localKey, BoundRoute(bound), "", "p")
	if err != nil || refusal != nil {
		t.Fatalf("%v %v", refusal, err)
	}
	pt := target.(*PostTarget)
	if pt.Platform != "local" || !strings.HasPrefix(pt.NoticeKey, "plugin-message:p:") || len(pt.NoticeKey) != len("plugin-message:p:")+12 {
		t.Fatalf("%+v", pt)
	}
	edited := EditPluginMessage(ctx, target, nil, "x", "p")
	if edited["ok"] != true || edited["edited"] != true || edited["message_id"] != pt.NoticeKey || edited["delivered"] != true || len(frames) != 1 {
		t.Fatalf("%v %v", edited, frames)
	}
	params, _ := frames[0].Get("params")
	eventType, _ := params.(*pyjson.Object).Get("type")
	payload, _ := params.(*pyjson.Object).Get("payload")
	if eventType != "notification.show" || !reflect.DeepEqual(payload.(*pyjson.Object).Keys, []string{"text", "level", "kind", "key", "id"}) {
		t.Fatalf("%v %v", eventType, payload)
	}
	runner.live = nil
	if _, refusal, _ := ResolvePluginMessageTarget(ctx, runner, localKey, route, "", "p"); refusal["detail"] != "the local session is not live in this plugin's profile" {
		t.Fatalf("%v", refusal)
	}
	if _, refusal, _ := ResolvePluginMessageTarget(ctx, runner, keys[0], nil, "", "p"); refusal["detail"] != "the session belongs to another profile" {
		t.Fatalf("%v", refusal)
	}
	runner.owned = true
	if _, refusal, _ := ResolvePluginMessageTarget(ctx, runner, keys[0], nil, "", "p"); refusal["error"] != "adapter_unavailable" ||
		refusal["detail"] != "no connected telegram adapter answers this chat" {
		t.Fatalf("%v", refusal)
	}
	if _, refusal, _ := ResolvePluginMessageTarget(ctx, runner, "zz", nil, "", "p"); refusal["detail"] != "session 'zz' is not known to this gateway" {
		t.Fatalf("%v", refusal)
	}
}

type limitAdapter struct{ *msgAdapter }

func (*limitAdapter) MaxMessageLengthForChat(string) (int, error) { return 5, nil }
func (*limitAdapter) MessageLenFnForChat(string) (func(string) int, error) {
	return func(s string) int { return len([]rune(s)) }, nil
}

func TestPluginTextIsClampedToTheChatLimit(t *testing.T) {
	limited := &limitAdapter{&msgAdapter{}}
	limited.BasePlatformAdapter = msgBase(limited)
	if got := fitPluginText(limited, "c", "abcdefgh"); got != "abcd…" {
		t.Fatalf("%q", got)
	}
	if got := fitPluginText(newMsgAdapter(), "c", "short"); got != "short" {
		t.Fatalf("%q", got)
	}
}

type msgPolicy struct{}

func (msgPolicy) PolicyPlatform() string { return "tui" }
func (msgPolicy) PolicyCwd() string      { return "/work" }

type actionRunner struct{ msgRunner }

func (actionRunner) ParsePlatform(_ context.Context, v string) (string, error) { return v, nil }

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
	if r, _ := s.GatewayLoop(ctx); r != nil {
		t.Fatal("runner without a loop", r)
	}
	loop := NewCallbackLoop()
	runner.loop = loop
	if r, on := s.GatewayLoop(ctx); r != runner || on {
		t.Fatal(r, on)
	}
	if _, on := s.GatewayLoop(WithLiveSource(ctx, &SessionSource{})); !on {
		t.Fatal("not on loop")
	}
	loop.Close()
	if r, _ := s.GatewayLoop(ctx); r != nil {
		t.Fatal("runner with a closed loop", r)
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

package gatewayrun

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/fakellm"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Registers a hook callback on the active home's plugin manager the way a loaded plugin does, discovery skipped
func registerGatewayHook(ctx context.Context, hookName string, fn plugins.HookFunc) {
	manager := plugins.GetPluginManager(ctx)
	manager.MarkDiscoveredForTests()
	plugins.NewContext(&plugins.Manifest{Name: "gateway-hook-test"}, manager).RegisterHook(hookName, fn, false)
}

// Whatsapp runner with every user authorized and a real session store, the _runner of the cases
func admissionRunner(t *testing.T, server *fakellm.Server) *GatewayRunner {
	t.Helper()
	installTurnFakes(t, server, "")
	for _, k := range []string{"WHATSAPP_ALLOWED_USERS", "GATEWAY_ALLOWED_USERS", "GATEWAY_ALLOW_ALL_USERS"} {
		t.Setenv(k, "")
	}
	t.Setenv("WHATSAPP_ALLOWED_USERS", "*")
	t.Setenv("GATEWAY_ALLOW_ALL_USERS", "true")
	home.ResetKeyCache()
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	ctx := context.Background()
	adapter := &cleanupCaptureAdapter{}
	adapter.BasePlatformAdapter = gateway.NewBasePlatformAdapter(gateway.NewPlatformConfig(), gateway.PlatformWhatsApp, adapter)
	r := makeTurnRunner(t, adapter)
	r.config = gateway.DefaultGatewayConfig(ctx)
	r.config.Platforms[gateway.PlatformWhatsApp] = &gateway.PlatformConfig{Enabled: true}
	r.sessionStore = gateway.NewSessionStore(ctx, filepath.Join(home.Dir(ctx), "sessions"), r.config, nil)
	t.Cleanup(r.sessionStore.CloseAllDBHandles)
	r.asyncSessionStore = gateway.NewAsyncSessionStore(r.sessionStore)
	r.sessions = map[string]*SessionState{}
	r.agentCache, r.agentCacheLock = pydict.New[string, agentCacheEntry](), &sync.Mutex{}
	return r
}

// Whatsapp dm event of the admission cases
func admissionEvent(text string) *gateway.MessageEvent {
	user, name := "15551234567@s.whatsapp.net", "tester"
	event := gateway.NewMessageEvent(text)
	mid := "m1"
	event.MessageID = &mid
	event.Source = &gateway.SessionSource{Platform: gateway.PlatformWhatsApp, UserID: &user, ChatID: user, UserName: &name, ChatType: "dm"}
	return event
}

// test_post_gateway_admission.py test_handled_consumes_the_turn_and_its_reply_is_delivered
func TestHandledConsumesTheTurnAndItsReplyIsDelivered(t *testing.T) {
	server, err := fakellm.New(nil, fakellm.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	r := admissionRunner(t, server)
	ctx := context.Background()
	var mu sync.Mutex
	var seen []map[string]any
	registerGatewayHook(ctx, postAdmissionHook, func(_ context.Context, kwargs map[string]any) (any, error) {
		mu.Lock()
		seen = append(seen, kwargs)
		mu.Unlock()
		return map[string]any{"action": "handled", "reply": "Queued - I'll follow up."}, nil
	})
	for _, text := range []string{"queue my report", "another"} {
		result, err := r.handleMessage(ctx, admissionEvent(text))
		if err != nil {
			t.Fatal(err)
		}
		if result != "Queued - I'll follow up." {
			t.Fatalf("%s result = %#v", text, result)
		}
	}
	if n := len(server.MainRequests()); n != 0 {
		t.Fatalf("agent turns = %d", n)
	}
	mu.Lock()
	first := seen[0]
	mu.Unlock()
	if first["text"] != "queue my report" || first["message_id"] != "m1" || first["platform"] != "whatsapp" {
		t.Fatalf("payload = %#v", first)
	}
	source, _ := first["source"].(*pyjson.Object)
	var chatID any
	if source != nil {
		chatID, _ = source.Get("chat_id")
	}
	if chatID != "15551234567@s.whatsapp.net" || first["session_key"] == "" || first["session_key"] == nil {
		t.Fatalf("payload routing = %#v", first)
	}
	if _, has := first["gateway"]; has {
		t.Fatalf("payload carries the runner: %#v", first)
	}
	if _, has := first["session_store"]; has {
		t.Fatalf("payload carries the store: %#v", first)
	}
}

// test_post_gateway_admission.py test_a_raising_consumer_fails_open_and_the_message_reaches_the_agent
func TestARaisingConsumerFailsOpenAndTheMessageReachesTheAgent(t *testing.T) {
	server, err := fakellm.New([]any{fakellm.Text{Text: "agent-ok"}, fakellm.Text{Text: "agent-ok"}}, fakellm.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	r := admissionRunner(t, server)
	ctx := context.Background()
	registerGatewayHook(ctx, postAdmissionHook, func(context.Context, map[string]any) (any, error) {
		return nil, errors.New("plugin bug")
	})
	for _, text := range []string{"queue my report", "second"} {
		result, err := r.handleMessage(ctx, admissionEvent(text))
		if err != nil {
			t.Fatal(err)
		}
		if result != "agent-ok" {
			t.Fatalf("%s result = %#v", text, result)
		}
	}
	if n := len(server.MainRequests()); n != 2 {
		t.Fatalf("agent turns = %d", n)
	}
	key, err := r.sessionKeyForSource(ctx, admissionEvent("x").Source)
	if err != nil {
		t.Fatal(err)
	}
	if a := r.cachedAgentFor(key, true); a != nil {
		t.Cleanup(func() { r.cleanupAgentResources(context.Background(), a) })
	}
}

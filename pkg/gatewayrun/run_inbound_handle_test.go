package gatewayrun

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/fakellm"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
)

// Telegram dm source of the dispatch cases
func dispatchSource() *gateway.SessionSource {
	user, name := "u1", "tester"
	return &gateway.SessionSource{Platform: gateway.PlatformTelegram, UserID: &user, ChatID: "c1", UserName: &name, ChatType: "dm"}
}

// Dispatch event from the dm source, internal when the case marks it so
func dispatchEvent(text string, internal bool) *gateway.MessageEvent {
	event := gateway.NewMessageEvent(text)
	mid := "m1"
	event.Source, event.MessageID, event.Internal = dispatchSource(), &mid, internal
	return event
}

// Runner serving one telegram adapter with a real session store, every user authorized, the _make_runner of the cases
func makeDispatchRunner(t *testing.T, server *fakellm.Server) (*GatewayRunner, *cleanupCaptureAdapter) {
	t.Helper()
	installTurnFakes(t, server, "")
	for _, k := range []string{"TELEGRAM_ALLOWED_USERS", "GATEWAY_ALLOWED_USERS", "TELEGRAM_ALLOW_ALL_USERS"} {
		t.Setenv(k, "")
	}
	t.Setenv("GATEWAY_ALLOW_ALL_USERS", "true")
	home.ResetKeyCache()
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	ctx := context.Background()
	adapter := newCleanupCaptureAdapter()
	r := makeTurnRunner(t, adapter)
	r.config = gateway.DefaultGatewayConfig(ctx)
	r.config.Platforms[gateway.PlatformTelegram] = &gateway.PlatformConfig{Enabled: true, Token: "***"}
	r.sessionStore = gateway.NewSessionStore(ctx, filepath.Join(home.Dir(ctx), "sessions"), r.config, nil)
	t.Cleanup(r.sessionStore.CloseAllDBHandles)
	r.asyncSessionStore = gateway.NewAsyncSessionStore(r.sessionStore)
	r.sessions = map[string]*SessionState{}
	r.agentCache, r.agentCacheLock = pydict.New[string, agentCacheEntry](), &sync.Mutex{}
	return r, adapter
}

// test_gateway_command_dispatch_minimal.py test_idle_queue_sends_payload_as_next_turn
func TestIdleQueueSendsPayloadAsNextTurn(t *testing.T) {
	for _, commandText := range []string{"/queue do this next", "/q do this next"} {
		t.Run(commandText, func(t *testing.T) {
			server, err := fakellm.New([]any{fakellm.Text{Text: "queued turn done"}}, fakellm.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer server.Stop()
			r, _ := makeDispatchRunner(t, server)
			ctx := context.Background()
			key, err := r.sessionKeyForSource(ctx, dispatchSource())
			if err != nil {
				t.Fatal(err)
			}
			result, err := r.handleMessage(ctx, dispatchEvent(commandText, true))
			if err != nil {
				t.Fatal(err)
			}
			if text, ok := result.(string); !ok || text != "queued turn done" {
				t.Fatalf("result = %#v", result)
			}
			requests := server.MainRequests()
			if len(requests) != 1 {
				t.Fatalf("main requests = %d", len(requests))
			}
			messages, _ := requests[0]["messages"].([]any)
			last, _ := messages[len(messages)-1].(map[string]any)
			if last["role"] != "user" || !strings.Contains(fakellmContentText(last["content"]), "do this next") ||
				strings.Contains(fakellmContentText(last["content"]), "/q") {
				t.Fatalf("turn user message = %#v", last)
			}
			if a := r.cachedAgentFor(key, true); a == nil {
				t.Fatalf("no agent cached under %s", key)
			} else {
				t.Cleanup(func() { r.cleanupAgentResources(context.Background(), a) })
			}
			if generation := r.sessionState(key).persistent.runGeneration; generation != 1 {
				t.Fatalf("run generation = %d", generation)
			}
			if r.runningAgents().Len() != 0 {
				t.Fatalf("running agents = %v", r.runningAgents().Keys())
			}
		})
	}
}

// Text of a chat message content, its text parts joined when the content is a part list
func fakellmContentText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, part := range v {
			if m, ok := part.(map[string]any); ok {
				if text, ok := m["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// test_status_command.py test_tasks_alias_routes_to_agents_command
func TestTasksAliasRoutesToAgentsCommand(t *testing.T) {
	server, err := fakellm.New(nil, fakellm.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	r, _ := makeDispatchRunner(t, server)
	result, err := r.handleMessage(context.Background(), dispatchEvent("/tasks", false))
	if err != nil {
		t.Fatal(err)
	}
	text, _ := result.(string)
	if !strings.Contains(text, "Active Agents & Tasks") {
		t.Fatalf("result = %#v", result)
	}
	if len(server.MainRequests()) != 0 {
		t.Fatalf("a slash command reached the model: %d requests", len(server.MainRequests()))
	}
}

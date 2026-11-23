package gatewayrun

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/fakellm"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/skills"
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
	installTurnFakes(t, server, "gateway:\n  allow_all_users: true\n")
	for _, k := range []string{"GATEWAY_ALLOWED_USERS", "TELEGRAM_ALLOW_ALL_USERS"} {
		t.Setenv(k, "")
	}
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
	result, err := r.handleMessage(ctx, dispatchEvent("/queue do this next", true))
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
		strings.Contains(fakellmContentText(last["content"]), "/queue") {
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

// The agents command answers from the runner and never starts a model turn
func TestAgentsCommandAnswersWithoutReachingTheModel(t *testing.T) {
	server, err := fakellm.New(nil, fakellm.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	r, _ := makeDispatchRunner(t, server)
	result, err := r.handleMessage(context.Background(), dispatchEvent("/agents", false))
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

// Plugin command bound to a handler that loads the named on disk skill as its turn, recording each turn's session
func installBoundSkillCommand(t *testing.T, command, skill string) *[]string {
	t.Helper()
	dir := filepath.Join(home.Dir(context.Background()), "skills", skill)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + skill + "\ndescription: bound fixture\n---\n\nBody marker of " + skill + " for session ${ICEBATH_SESSION_ID}\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var sessions []string
	prevResolve, prevRun, prevCommand, prevKnown := gateway.ResolveBoundCommand, gateway.RunBoundCommand, ResolveCommand, IsGatewayKnownCommand
	gateway.ResolveBoundCommand = func(name string) (gateway.RegistryCommand, bool) {
		if name != command {
			return prevResolve(name)
		}
		return gateway.RegistryCommand{Name: command, BusyPolicy: "reject"}, true
	}
	gateway.RunBoundCommand = func(ctx context.Context, name, arg string, host any) error {
		if name != command {
			return prevRun(ctx, name, arg, host)
		}
		submitter := host.(interface {
			SubmitUserTurn(context.Context, string) error
			TurnSession() (string, string)
		})
		taskID, platform := submitter.TurnSession()
		sessions = append(sessions, taskID+"|"+platform)
		msg, ok, err := skills.NewScope(home.Dir(ctx), nil).BuildSkillInvocationMessage(ctx, "/"+skill, strings.Replace(arg, "teach", "init", 1), taskID, "")
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("skill " + skill + " is not available")
		}
		return submitter.SubmitUserTurn(ctx, msg)
	}
	ResolveCommand = func(name string) (string, bool) {
		if name == command {
			return command, true
		}
		return prevCommand(name)
	}
	IsGatewayKnownCommand = func(name string) bool { return name == command || prevKnown(name) }
	t.Cleanup(func() {
		gateway.ResolveBoundCommand, gateway.RunBoundCommand, ResolveCommand, IsGatewayKnownCommand = prevResolve, prevRun, prevCommand, prevKnown
	})
	return &sessions
}

func TestIdleBoundPluginCommandRunsTheTurnItsHandlerBuilds(t *testing.T) {
	server, err := fakellm.New([]any{fakellm.Text{Text: "design audit done"}}, fakellm.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	r, _ := makeDispatchRunner(t, server)
	ctx := context.Background()
	sessions := installBoundSkillCommand(t, "boundfix", "bound-design")
	key, err := r.sessionKeyForSource(ctx, dispatchSource())
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.handleMessage(ctx, dispatchEvent("/boundfix teach the checkout flow", false))
	if err != nil {
		t.Fatal(err)
	}
	if text, ok := result.(string); !ok || text != "design audit done" {
		t.Fatalf("result = %#v", result)
	}
	if len(*sessions) != 1 || (*sessions)[0] != key+"|telegram" {
		t.Fatalf("turn sessions = %v, want %s|telegram", *sessions, key)
	}
	requests := server.MainRequests()
	if len(requests) != 1 {
		t.Fatalf("main requests = %d", len(requests))
	}
	messages, _ := requests[0]["messages"].([]any)
	last, _ := messages[len(messages)-1].(map[string]any)
	content := fakellmContentText(last["content"])
	for _, want := range []string{`[IMPORTANT: The user has invoked the "bound-design" skill`, "Body marker of bound-design for session " + key, "init the checkout flow"} {
		if last["role"] != "user" || !strings.Contains(content, want) {
			t.Fatalf("turn user message lacks %q: %#v", want, last)
		}
	}
	if strings.Contains(content, "/boundfix") {
		t.Fatalf("the raw command reached the model: %q", content)
	}
	if a := r.cachedAgentFor(key, true); a != nil {
		t.Cleanup(func() { r.cleanupAgentResources(context.Background(), a) })
	}
}

func TestIdleBoundPluginCommandFailureIsRepliedAndNeverReachesTheModel(t *testing.T) {
	server, err := fakellm.New([]any{fakellm.Text{Text: "unexpected"}}, fakellm.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	r, _ := makeDispatchRunner(t, server)
	installBoundSkillCommand(t, "boundfix", "bound-design")
	if err := os.RemoveAll(filepath.Join(home.Dir(context.Background()), "skills", "bound-design")); err != nil {
		t.Fatal(err)
	}
	result, err := r.handleMessage(context.Background(), dispatchEvent("/boundfix audit", false))
	if err != nil {
		t.Fatal(err)
	}
	if text, ok := result.(string); !ok || text != "Plugin command error: skill bound-design is not available" {
		t.Fatalf("result = %#v", result)
	}
	if n := len(server.MainRequests()); n != 0 {
		t.Fatalf("main requests = %d", n)
	}
}

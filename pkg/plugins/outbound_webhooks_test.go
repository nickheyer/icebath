package plugins

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Every POST and GET the capturing server saw, with the status and location it answers
type capturedWebhook struct {
	Path, Method string
	Headers      http.Header
	Body         []byte
}

type webhookServer struct {
	*httptest.Server
	mu       sync.Mutex
	captured []capturedWebhook
	status   int
	location string
}

func (s *webhookServer) records() []capturedWebhook {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]capturedWebhook(nil), s.captured...)
}

func newWebhookServer(t *testing.T) *webhookServer {
	t.Helper()
	s := &webhookServer{status: 200}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.captured = append(s.captured, capturedWebhook{Path: r.URL.Path, Method: r.Method, Headers: r.Header.Clone(), Body: body})
		status, location := s.status, s.location
		s.mu.Unlock()
		if location != "" {
			w.Header().Set("Location", location)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *webhookServer) url(path string) string { return s.URL + path }

func webhookCfg(entries ...map[string]any) map[string]any {
	list := make([]any, len(entries))
	for i, e := range entries {
		list[i] = e
	}
	return map[string]any{"hooks": map[string]any{"outbound": list}}
}

// Fresh registration state and a process secret service reading the environment, the autouse fixture
func webhookTestEnv(t *testing.T) context.Context {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	ResetPluginManagersForTests()
	stripOutboundCallbacks(context.Background())
	ResetOutboundWebhooksForTests()
	previous := installedServices()
	InstallServices(&Services{GetSecret: func(_ context.Context, name string) (string, bool, error) {
		v, ok := os.LookupEnv(name)
		return v, ok, nil
	}})
	t.Cleanup(func() {
		stripOutboundCallbacks(context.Background())
		ResetOutboundWebhooksForTests()
		InstallServices(previous)
		ResetPluginManagersForTests()
	})
	return context.Background()
}

// Removes outbound webhook callbacks from the shared manager so one test's target never fires in another
func stripOutboundCallbacks(ctx context.Context) {
	m := GetPluginManager(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	for event, entries := range m.hooks {
		kept := entries[:0]
		for _, e := range entries {
			if !strings.HasPrefix(e.name, "outbound_webhook[") {
				kept = append(kept, e)
			}
		}
		m.hooks[event] = kept
	}
}

func iterTargets(t *testing.T, cfg any) []*OutboundWebhookTarget {
	t.Helper()
	targets, err := IterConfiguredOutboundWebhooks(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return targets
}

func TestParseConfigMissingBlockIsEmpty(t *testing.T) {
	webhookTestEnv(t)
	for _, cfg := range []any{map[string]any{}, nil, map[string]any{"hooks": map[string]any{}}, map[string]any{"hooks": "not-a-dict"}} {
		if got := iterTargets(t, cfg); len(got) != 0 {
			t.Fatalf("%v -> %v", cfg, got)
		}
	}
}

func TestParseConfigNonListOutboundIsEmpty(t *testing.T) {
	webhookTestEnv(t)
	if got := iterTargets(t, map[string]any{"hooks": map[string]any{"outbound": map[string]any{"url": "https://x"}}}); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestParseConfigValidEntryParses(t *testing.T) {
	webhookTestEnv(t)
	targets := iterTargets(t, webhookCfg(map[string]any{"url": "https://example.com/hook", "events": []any{"on_session_end"}, "name": "ci", "timeout": 5}))
	if len(targets) != 1 {
		t.Fatal(targets)
	}
	tg := targets[0]
	if tg.URL != "https://example.com/hook" || len(tg.Events) != 1 || tg.Events[0] != "on_session_end" || tg.Name != "ci" || tg.Timeout != 5 {
		t.Fatalf("%+v", tg)
	}
}

func TestParseConfigMissingUrlSkipped(t *testing.T) {
	webhookTestEnv(t)
	if got := iterTargets(t, webhookCfg(map[string]any{"events": []any{"on_session_end"}})); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestParseConfigNonHttpUrlSkipped(t *testing.T) {
	webhookTestEnv(t)
	for _, url := range []string{"ftp://example.com", "file:///etc/passwd"} {
		if got := iterTargets(t, webhookCfg(map[string]any{"url": url, "events": []any{"on_session_end"}})); len(got) != 0 {
			t.Fatal(url)
		}
	}
}

func TestParseConfigUnknownEventsFilteredKnownKept(t *testing.T) {
	webhookTestEnv(t)
	targets := iterTargets(t, webhookCfg(map[string]any{"url": "https://example.com/hook", "events": []any{"on_session_end", "not_a_real_event"}}))
	if len(targets) != 1 || len(targets[0].Events) != 1 || targets[0].Events[0] != "on_session_end" {
		t.Fatal(targets)
	}
}

func TestParseConfigAllUnknownEventsSkipsEntry(t *testing.T) {
	webhookTestEnv(t)
	if got := iterTargets(t, webhookCfg(map[string]any{"url": "https://example.com", "events": []any{"bogus"}})); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestParseConfigEmptyEventsSkipsEntry(t *testing.T) {
	webhookTestEnv(t)
	if got := iterTargets(t, webhookCfg(map[string]any{"url": "https://example.com", "events": []any{}})); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestParseConfigTimeoutClamped(t *testing.T) {
	webhookTestEnv(t)
	targets := iterTargets(t, webhookCfg(map[string]any{"url": "https://example.com", "events": []any{"on_session_end"}, "timeout": 9999}))
	if targets[0].Timeout != OutboundWebhookMaxTimeoutSeconds {
		t.Fatal(targets[0].Timeout)
	}
}

func TestParseConfigBadTimeoutFallsBackToDefault(t *testing.T) {
	webhookTestEnv(t)
	targets := iterTargets(t, webhookCfg(map[string]any{"url": "https://example.com", "events": []any{"on_session_end"}, "timeout": "soon"}))
	if targets[0].Timeout != OutboundWebhookDefaultTimeoutSeconds {
		t.Fatal(targets[0].Timeout)
	}
}

func TestParseConfigMatcherDroppedForNonToolEvents(t *testing.T) {
	webhookTestEnv(t)
	targets := iterTargets(t, webhookCfg(map[string]any{"url": "https://example.com", "events": []any{"on_session_end"}, "matcher": "terminal"}))
	if targets[0].Matcher != "" {
		t.Fatal(targets[0].Matcher)
	}
}

func TestParseConfigMatcherKeptForToolEvents(t *testing.T) {
	webhookTestEnv(t)
	targets := iterTargets(t, webhookCfg(map[string]any{"url": "https://example.com", "events": []any{"post_tool_call"}, "matcher": "terminal|delegate_task"}))
	if targets[0].Matcher != "terminal|delegate_task" {
		t.Fatal(targets[0].Matcher)
	}
}

func TestParseConfigSecretEnvWinsOverLiteral(t *testing.T) {
	webhookTestEnv(t)
	t.Setenv("MY_HOOK_SECRET", "from-env")
	targets := iterTargets(t, webhookCfg(map[string]any{"url": "https://example.com", "events": []any{"on_session_end"}, "secret_env": "MY_HOOK_SECRET", "secret": "literal"}))
	if targets[0].Secret != "from-env" {
		t.Fatal(targets[0].Secret)
	}
}

func TestParseConfigUnsetSecretEnvMeansUnsigned(t *testing.T) {
	webhookTestEnv(t)
	os.Unsetenv("MISSING_SECRET_VAR")
	targets := iterTargets(t, webhookCfg(map[string]any{"url": "https://example.com", "events": []any{"on_session_end"}, "secret_env": "MISSING_SECRET_VAR"}))
	if targets[0].Secret != "" {
		t.Fatal(targets[0].Secret)
	}
}

func matcherTarget(matcher string) *OutboundWebhookTarget {
	return NewOutboundWebhookTarget("https://example.com", []string{"post_tool_call"}, "", "", matcher, OutboundWebhookDefaultTimeoutSeconds)
}

func TestMatcherNoMatcherMatchesEverything(t *testing.T) {
	if !matcherTarget("").MatchesTool("terminal") || !matcherTarget("").MatchesTool(nil) {
		t.Fatal("no matcher matches everything")
	}
}

func TestMatcherRegexFullmatch(t *testing.T) {
	tg := matcherTarget("terminal|delegate_task")
	if !tg.MatchesTool("terminal") || !tg.MatchesTool("delegate_task") || tg.MatchesTool("terminal_extra") || tg.MatchesTool(nil) {
		t.Fatal("regex must fullmatch")
	}
}

func TestMatcherInvalidRegexFallsBackToEquality(t *testing.T) {
	tg := matcherTarget("terminal(")
	if !tg.MatchesTool("terminal(") || tg.MatchesTool("terminal") {
		t.Fatal("an invalid regex is literal equality")
	}
}

func decodePayload(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestPayloadTopLevelShapeMatchesShellHooksWire(t *testing.T) {
	ctx := webhookTestEnv(t)
	body, err := serializeOutboundWebhookPayload(ctx, "post_tool_call", map[string]any{"tool_name": "terminal", "args": map[string]any{"command": "ls"},
		"session_id": "sess_1", "status": "ok", "duration_ms": 42}, "did_1234")
	if err != nil {
		t.Fatal(err)
	}
	payload := decodePayload(t, body)
	extra, _ := payload["extra"].(map[string]any)
	if payload["hook_event_name"] != "post_tool_call" || payload["tool_name"] != "terminal" || payload["session_id"] != "sess_1" ||
		fmt.Sprint(payload["tool_input"]) != fmt.Sprint(map[string]any{"command": "ls"}) || extra["status"] != "ok" || extra["duration_ms"] != float64(42) ||
		payload["delivery_id"] != "did_1234" || !strings.HasSuffix(payload["timestamp"].(string), "Z") {
		t.Fatalf("%v", payload)
	}
}

func TestPayloadProfileFieldReflectsBoundProfileHome(t *testing.T) {
	ctx := webhookTestEnv(t)
	root := home.Dir(ctx)
	profileHome := filepath.Join(root, "profiles", "b")
	if err := os.MkdirAll(profileHome, 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := serializeOutboundWebhookPayload(home.WithOverride(ctx, profileHome), "on_session_end", map[string]any{}, "did_1")
	if err != nil {
		t.Fatal(err)
	}
	if decodePayload(t, body)["profile"] != "b" {
		t.Fatalf("%s", body)
	}
	body, err = serializeOutboundWebhookPayload(ctx, "on_session_end", map[string]any{}, "did_2")
	if err != nil {
		t.Fatal(err)
	}
	if decodePayload(t, body)["profile"] != "default" {
		t.Fatalf("%s", body)
	}
}

func TestPayloadKanbanTaskIdIsTheOwnedTaskOnly(t *testing.T) {
	ctx := webhookTestEnv(t)
	t.Setenv("ICEBATH_KANBAN_TASK", "t_owner")
	os.Unsetenv("ICEBATH_DELEGATED_CHILD_CONTEXT")
	body, err := serializeOutboundWebhookPayload(ctx, "post_tool_call", map[string]any{}, "did_1")
	if err != nil {
		t.Fatal(err)
	}
	if decodePayload(t, body)["extra"].(map[string]any)["kanban_task_id"] != "t_owner" {
		t.Fatalf("%s", body)
	}
	body, err = serializeOutboundWebhookPayload(tools.WithDelegatedChildContext(ctx, true), "post_tool_call", map[string]any{}, "did_2")
	if err != nil {
		t.Fatal(err)
	}
	if decodePayload(t, body)["extra"].(map[string]any)["kanban_task_id"] != "" {
		t.Fatalf("%s", body)
	}
}

func TestPayloadUnserialisableValuesStringified(t *testing.T) {
	ctx := webhookTestEnv(t)
	body, err := serializeOutboundWebhookPayload(ctx, "on_session_end", map[string]any{"weird": struct{ X int }{1}}, "did_1")
	if err != nil {
		t.Fatal(err)
	}
	if _, isStr := decodePayload(t, body)["extra"].(map[string]any)["weird"].(string); !isStr {
		t.Fatalf("%s", body)
	}
}

func registerTargets(t *testing.T, ctx context.Context, cfg map[string]any) []*OutboundWebhookTarget {
	t.Helper()
	registered, err := RegisterOutboundWebhooksFromConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return registered
}

func TestRegistrationIsIdempotent(t *testing.T) {
	ctx := webhookTestEnv(t)
	cfg := webhookCfg(map[string]any{"url": "https://example.com/hook", "events": []any{"on_session_end"}})
	if first := registerTargets(t, ctx, cfg); len(first) != 1 {
		t.Fatal(first)
	}
	if second := registerTargets(t, ctx, cfg); len(second) != 0 {
		t.Fatal(second)
	}
}

func TestRegistrationSafeModeSkipsRegistration(t *testing.T) {
	ctx := webhookTestEnv(t)
	t.Setenv("ICEBATH_SAFE_MODE", "1")
	if got := registerTargets(t, ctx, webhookCfg(map[string]any{"url": "https://example.com/hook", "events": []any{"on_session_end"}})); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestRegistrationCallbacksNeverReturnDirectives(t *testing.T) {
	ctx := webhookTestEnv(t)
	server := newWebhookServer(t)
	registerTargets(t, ctx, webhookCfg(map[string]any{"url": server.url("/hook"), "events": []any{"pre_tool_call"}}))
	results := GetPluginManager(ctx).InvokeHook(ctx, "pre_tool_call", map[string]any{"tool_name": "terminal", "args": map[string]any{"command": "ls"}})
	if !FlushOutboundWebhooks(5 * time.Second) {
		t.Fatal("queue must drain")
	}
	if len(results) != 0 || len(server.records()) != 1 {
		t.Fatalf("results %v captured %d", results, len(server.records()))
	}
}

func TestForceReloadRestoresWebhookAndFiresOnce(t *testing.T) {
	ctx := webhookTestEnv(t)
	server := newWebhookServer(t)
	cfg := webhookCfg(map[string]any{"url": server.url("/hook"), "events": []any{"on_session_end"}})
	text, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home.Dir(ctx), "config.yaml"), text, 0o600); err != nil {
		t.Fatal(err)
	}
	mgr := GetPluginManager(ctx)
	registerTargets(t, ctx, cfg)
	if len(mgr.hooks["on_session_end"]) != 1 {
		t.Fatal(mgr.hooks["on_session_end"])
	}
	mgr.Unload(nil)
	if len(mgr.hooks["on_session_end"]) != 0 {
		t.Fatal("unload wipes the config owned callbacks")
	}
	if err := ReRegisterOutboundWebhooks(ctx); err != nil {
		t.Fatal(err)
	}
	if len(mgr.hooks["on_session_end"]) != 1 {
		t.Fatal("re-registration must rewire the webhook once")
	}
	GetPluginManager(ctx).InvokeHook(ctx, "on_session_end", map[string]any{"session_id": "s1"})
	if !FlushOutboundWebhooks(5*time.Second) || len(server.records()) != 1 {
		t.Fatalf("captured %d", len(server.records()))
	}
}

func TestDeliveryWithHmacSignature(t *testing.T) {
	ctx := webhookTestEnv(t)
	server := newWebhookServer(t)
	secret := "s3cret"
	if got := registerTargets(t, ctx, webhookCfg(map[string]any{"url": server.url("/hook"), "events": []any{"on_session_end"}, "secret": secret, "name": "e2e"})); len(got) != 1 {
		t.Fatal(got)
	}
	GetPluginManager(ctx).InvokeHook(ctx, "on_session_end", map[string]any{"session_id": "sess_e2e", "completed": true, "interrupted": false, "model": "test-model", "platform": "cli"})
	if !FlushOutboundWebhooks(5 * time.Second) {
		t.Fatal("queue must drain")
	}
	records := server.records()
	if len(records) != 1 {
		t.Fatal(len(records))
	}
	req := records[0]
	payload := decodePayload(t, req.Body)
	extra := payload["extra"].(map[string]any)
	if payload["hook_event_name"] != "on_session_end" || payload["session_id"] != "sess_e2e" || extra["completed"] != true || extra["model"] != "test-model" {
		t.Fatalf("%v", payload)
	}
	if req.Headers.Get("X-Hermes-Event") != "on_session_end" || req.Headers.Get("X-Hermes-Delivery") == "" {
		t.Fatal(req.Headers)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(req.Body)
	if req.Headers.Get("X-Hermes-Signature-256") != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatal(req.Headers.Get("X-Hermes-Signature-256"))
	}
}

func TestDeliveryUnsignedDeliveryHasNoSignatureHeader(t *testing.T) {
	ctx := webhookTestEnv(t)
	server := newWebhookServer(t)
	registerTargets(t, ctx, webhookCfg(map[string]any{"url": server.url("/hook"), "events": []any{"on_session_end"}}))
	GetPluginManager(ctx).InvokeHook(ctx, "on_session_end", map[string]any{"session_id": "s"})
	if !FlushOutboundWebhooks(5 * time.Second) {
		t.Fatal("queue must drain")
	}
	records := server.records()
	if len(records) != 1 || records[0].Headers.Get("X-Hermes-Signature-256") != "" {
		t.Fatal(records)
	}
}

func TestDeliveryMatcherFiltersToolEvents(t *testing.T) {
	ctx := webhookTestEnv(t)
	server := newWebhookServer(t)
	registerTargets(t, ctx, webhookCfg(map[string]any{"url": server.url("/hook"), "events": []any{"post_tool_call"}, "matcher": "terminal"}))
	manager := GetPluginManager(ctx)
	manager.InvokeHook(ctx, "post_tool_call", map[string]any{"tool_name": "web_search", "args": map[string]any{}, "status": "ok"})
	manager.InvokeHook(ctx, "post_tool_call", map[string]any{"tool_name": "terminal", "args": map[string]any{}, "status": "ok"})
	if !FlushOutboundWebhooks(5 * time.Second) {
		t.Fatal("queue must drain")
	}
	records := server.records()
	if len(records) != 1 || decodePayload(t, records[0].Body)["tool_name"] != "terminal" {
		t.Fatal(records)
	}
}

func deliveryTo(server *webhookServer, id string) *outboundDelivery {
	target := NewOutboundWebhookTarget(server.url("/hook"), []string{"on_session_end"}, "", "", "", OutboundWebhookDefaultTimeoutSeconds)
	return buildOutboundDelivery("on_session_end", target, []byte("{}"), id)
}

func TestDelivery4xxNotRetried(t *testing.T) {
	server := newWebhookServer(t)
	server.status = 400
	deliverOutboundWebhook(deliveryTo(server, "did_4xx"))
	if len(server.records()) != 1 {
		t.Fatal(len(server.records()))
	}
}

func TestDelivery5xxRetriedOnce(t *testing.T) {
	server := newWebhookServer(t)
	server.status = 500
	deliverOutboundWebhook(deliveryTo(server, "did_5xx"))
	if len(server.records()) != OutboundWebhookMaxDeliveryAttempts {
		t.Fatal(len(server.records()))
	}
}

func TestDeliveryRedirectNotFollowed(t *testing.T) {
	server := newWebhookServer(t)
	server.status = 302
	server.location = server.url("/redirected")
	deliverOutboundWebhook(deliveryTo(server, "did_3xx"))
	records := server.records()
	if len(records) != 1 || records[0].Method != "POST" || records[0].Path != "/hook" {
		t.Fatal(records)
	}
}

func TestDeliveryIdMatchesHeaderAndBody(t *testing.T) {
	ctx := webhookTestEnv(t)
	server := newWebhookServer(t)
	registerTargets(t, ctx, webhookCfg(map[string]any{"url": server.url("/hook"), "events": []any{"on_session_end"}, "secret": "s"}))
	GetPluginManager(ctx).InvokeHook(ctx, "on_session_end", map[string]any{"session_id": "s1"})
	if !FlushOutboundWebhooks(5 * time.Second) {
		t.Fatal("queue must drain")
	}
	req := server.records()[0]
	if decodePayload(t, req.Body)["delivery_id"] != req.Headers.Get("X-Hermes-Delivery") {
		t.Fatal("delivery id must match the header")
	}
}

func TestDeliveryConnectionErrorDoesNotRaise(t *testing.T) {
	target := NewOutboundWebhookTarget("http://127.0.0.1:9/unreachable", []string{"on_session_end"}, "", "", "", 1)
	deliverOutboundWebhook(buildOutboundDelivery("on_session_end", target, []byte("{}"), "did_conn"))
}

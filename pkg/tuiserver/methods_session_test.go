package tuiserver

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Isolated home with synthetic agent builds and a fresh launch store, dropping every session the case created
func sessionCaseScope(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_ISO_CERTIFY_SYNTH_TURN", "1")
	before := map[string]bool{}
	for _, live := range tuigateway.LiveSessions() {
		before[live.SID] = true
	}
	t.Cleanup(func() {
		for _, live := range tuigateway.LiveSessions() {
			if !before[live.SID] {
				tuigateway.SetSessionRecord(live.SID, nil)
			}
		}
		sessionsLock.Lock()
		clear(idempotencyKeys)
		sessionsLock.Unlock()
	})
	return h
}

// Runtime ids of the live sessions a case added
func liveSessionIDs() []string {
	var out []string
	for _, live := range tuigateway.LiveSessions() {
		out = append(out, live.SID)
	}
	return out
}

// Messages of every record logged while the case runs
type logCapture struct {
	mu   sync.Mutex
	msgs []string
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, r.Message)
	return nil
}
func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

// Logged messages containing needle, cleared on take
func (c *logCapture) take(needle string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, msg := range c.msgs {
		if strings.Contains(msg, needle) {
			out = append(out, msg)
		}
	}
	c.msgs = nil
	return out
}

// Routes the default logger into a capture for the case
func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	capture := &logCapture{}
	saved := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(saved) })
	return capture
}

func rpc(t *testing.T, method string, params map[string]any) map[string]any {
	t.Helper()
	return handleRequest(context.Background(), map[string]any{"id": "r1", "method": method, "params": params})
}

// test_tui_gateway_server_branch_idempotency.py test_session_branch_idempotency_key_dedupes_retry
func TestSessionBranchIdempotencyKeyDedupesRetry(t *testing.T) {
	sessionCaseScope(t)
	db := reaperTestDB(t)
	useSharedDB(t, db)
	if err := db.CreateSession(state.NewSession{ID: "parent-key", Source: "tui"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetSessionTitle(context.Background(), "parent-key", "parent"); err != nil {
		t.Fatal(err)
	}
	before := map[string]bool{}
	for _, sid := range liveSessionIDs() {
		before[sid] = true
	}
	cwd := t.TempDir()
	parent := tuigateway.NewSession(map[string]any{"session_key": "parent-key", "history": []any{map[string]any{"role": "user", "content": "hi"}},
		"history_lock": &sync.Mutex{}, "running": false, "cols": 80, "profile_home": nil, "source": "tui",
		"agent": NewSyntheticHeavyAgent("parent-key", "test-model"), "created_at": 1.0, "last_active": 1.0, "cwd": cwd})
	tuigateway.SetSessionRecord("parent", parent)
	params := map[string]any{"session_id": "parent", "name": "forked", "idempotency_key": "branch-live-retry-1"}
	first := rpc(t, "session.branch", params)
	result, isResult := first["result"].(map[string]any)
	if !isResult {
		t.Fatalf("first %v", first)
	}
	if result["title"] != "forked" || result["parent"] != "parent-key" {
		t.Fatalf("first %v", result)
	}
	second := rpc(t, "session.branch", params)
	again, isResult := second["result"].(map[string]any)
	if !isResult {
		t.Fatalf("second %v", second)
	}
	if again["session_id"] != result["session_id"] || again["stored_session_id"] != result["stored_session_id"] ||
		again["title"] != "forked" || again["parent"] != "parent-key" {
		t.Fatalf("second %v first %v", again, result)
	}
	var children []string
	for _, sid := range liveSessionIDs() {
		if sid != "parent" && !before[sid] {
			children = append(children, sid)
		}
	}
	if len(children) != 1 {
		t.Fatalf("children %v", children)
	}
}

// test_branch_system_prompt.py test_persist_branch_copies_the_parent_system_prompt
func TestPersistBranchCopiesTheParentSystemPrompt(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	tmp := t.TempDir()
	db, err := state.NewSessionDB(ctx, filepath.Join(tmp, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "parent", Source: "desktop", Model: "test-model"}); err != nil {
		t.Fatal(err)
	}
	prompt := "PARENT PROMPT\n\nWorkspace snapshot: session start"
	if err := db.UpdateSystemPrompt(ctx, "parent", &prompt); err != nil {
		t.Fatal(err)
	}
	if err := persistBranch(ctx, db, "child", "parent", "Branch", []map[string]any{{"role": "user", "content": "hello"}}, "desktop", &tmp,
		"default", "test-model", nil, false, "user", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "bare", Source: "desktop", Model: "test-model"}); err != nil {
		t.Fatal(err)
	}
	if err := persistBranch(ctx, db, "bare-child", "bare", "Branch 2", []map[string]any{{"role": "user", "content": "hi"}}, "desktop", &tmp,
		"default", "test-model", nil, false, "user", nil); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	db, err = state.NewSessionDB(ctx, filepath.Join(tmp, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	child, err := db.GetSession(ctx, "child")
	if err != nil || child.GetOr("system_prompt", nil) != prompt {
		t.Fatalf("child %v %v", child, err)
	}
	bare, err := db.GetSession(ctx, "bare-child")
	if err != nil || bare.GetOr("system_prompt", "missing") != nil {
		t.Fatalf("bare %v %v", bare, err)
	}
}

// test_seeded_branch_title_routing.py test_seeded_desktop_branch_title_is_derived_until_the_user_renames
func TestSeededDesktopBranchTitleIsDerivedUntilTheUserRenames(t *testing.T) {
	sessionCaseScope(t)
	ctx := context.Background()
	tmp := t.TempDir()
	project := filepath.Join(tmp, "example-project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := state.NewSessionDB(ctx, filepath.Join(tmp, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	useSharedDB(t, db)
	history := []any{map[string]any{"role": "user", "content": "Synthetic parent input"},
		map[string]any{"role": "assistant", "content": "Synthetic parent response"}}
	if err := db.CreateSession(state.NewSession{ID: "parent", Source: "desktop", Cwd: project}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetAutoTitleContext(ctx, "parent", "Generated parent title", "llm"); err != nil {
		t.Fatal(err)
	}
	response := lookupMethod("session.create")(ctx, "create", map[string]any{"source": "desktop", "cwd": project,
		"parent_session_id": "parent", "messages": history})
	if _, failed := response["error"]; failed {
		t.Fatalf("create %v", response)
	}
	child := strOr(response["result"].(map[string]any)["stored_session_id"])
	if source, _, err := db.GetSessionTitleSource(ctx, child); err != nil || source != state.TitleSourceDerived {
		t.Fatalf("title source %q %v", source, err)
	}
	if count, err := db.MessageCount(ctx, child); err != nil || count != int64(len(history)) {
		t.Fatalf("message count %d %v", count, err)
	}
	if _, err := db.SetSessionTitle(ctx, child, "User chosen title"); err != nil {
		t.Fatal(err)
	}
	if source, _, err := db.GetSessionTitleSource(ctx, child); err != nil || source != state.TitleSourceUser {
		t.Fatalf("renamed title source %q %v", source, err)
	}
}

// Profile home with an anthropic default model for the guard cases
func guardScope(t *testing.T, defaultModel string) {
	t.Helper()
	h := sessionCaseScope(t)
	useSharedDB(t, nil)
	config := "model:\n  default: " + defaultModel + "\n  provider: anthropic\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Create response of the guard fixture, cols defaulted to eighty
func guardCreate(params map[string]any) map[string]any {
	withCols := map[string]any{"cols": 80}
	for k, v := range params {
		withCols[k] = v
	}
	return lookupMethod("session.create")(context.Background(), "r1", withCols)
}

// test_session_create_model_provider_guard.py test_session_create_rejects_incoherent_model_provider_pair_before_any_state
func TestSessionCreateRejectsIncoherentModelProviderPairBeforeAnyState(t *testing.T) {
	for _, params := range []map[string]any{
		{"model": "gpt-5.5", "provider": "anthropic"},
		{"model": "gpt-5.5"},
		{"model": "deepseek/deepseek-v4-flash-0731", "provider": "openai-codex"},
	} {
		t.Run(strOr(params["model"])+"/"+strOr(params["provider"]), func(t *testing.T) {
			guardScope(t, "claude-opus-5")
			before := len(tuigateway.LiveSessions())
			response := guardCreate(params)
			errObj, _ := response["error"].(map[string]any)
			data, _ := errObj["data"].(map[string]any)
			message := strOr(errObj["message"])
			if errObj["code"] != -32602 || !strings.Contains(message, strOr(params["model"])) ||
				!strings.Contains(message, strOr(data["provider"])) || data["model"] != params["model"] {
				t.Fatalf("response %v", response)
			}
			suggestions, _ := data["suggestions"].([]any)
			if len(suggestions) < 1 || len(suggestions) > 5 {
				t.Fatalf("suggestions %v", suggestions)
			}
			for _, s := range suggestions {
				if !strings.Contains(message, strOr(s)) {
					t.Fatalf("suggestion %v not in %q", s, message)
				}
			}
			if len(tuigateway.LiveSessions()) != before {
				t.Fatalf("a session was minted")
			}
		})
	}
}

// test_session_create_model_provider_guard.py test_session_create_keeps_coherent_unlisted_and_custom_pairs
func TestSessionCreateKeepsCoherentUnlistedAndCustomPairs(t *testing.T) {
	for _, params := range []map[string]any{
		{"model": "claude-opus-5", "provider": "anthropic"},
		{"model": "claude-opus-5-20261001", "provider": "anthropic"},
		{"model": "gpt-5.5", "provider": "custom:local"},
		{"model": "gpt-5.5", "provider": "openrouter"},
	} {
		t.Run(strOr(params["model"])+"/"+strOr(params["provider"]), func(t *testing.T) {
			guardScope(t, "claude-opus-5")
			response := guardCreate(params)
			result, isResult := response["result"].(map[string]any)
			if !isResult {
				t.Fatalf("response %v", response)
			}
			override, _ := tuigateway.SessionRecord(strOr(result["session_id"])).Get("model_override").(map[string]any)
			if len(override) != 2 || override["model"] != params["model"] || override["provider"] != params["provider"] {
				t.Fatalf("override %v", override)
			}
		})
	}
}

// test_session_create_model_provider_guard.py test_session_create_logs_when_a_client_override_beats_the_profile_default
func TestSessionCreateLogsWhenAClientOverrideBeatsTheProfileDefault(t *testing.T) {
	guardScope(t, "deepseek-v4-flash")
	capture := captureLogs(t)
	response := guardCreate(map[string]any{"model": "gpt-5.5", "provider": "openrouter"})
	pinned := capture.take("client override")
	guardCreate(map[string]any{})
	unpinned := capture.take("client override")
	if len(pinned) != 1 {
		t.Fatalf("pinned %v", pinned)
	}
	key := strOr(response["result"].(map[string]any)["stored_session_id"])
	for _, want := range []string{key, "gpt-5.5", "openrouter", "deepseek-v4-flash"} {
		if !strings.Contains(pinned[0], want) {
			t.Fatalf("%q missing %q", pinned[0], want)
		}
	}
	if len(unpinned) != 0 {
		t.Fatalf("unpinned %v", unpinned)
	}
}

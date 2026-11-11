package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Session database in a fresh isolated home with the given cli rows created
func resumeTestDB(t *testing.T, rows ...state.NewSession) *state.SessionDB {
	t.Helper()
	h := goalHooksHome(t)
	db, err := state.NewSessionDB(context.Background(), filepath.Join(h, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, row := range rows {
		if row.Source == "" {
			row.Source = "cli"
		}
		if err := db.CreateSession(row); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// CLI the way resume paths see it, on the ambient openrouter route
func restoreStub() *CLI {
	c := bareCLI()
	c.model, c.provider, c.requestedProvider = "ambient-model", "openrouter", "openrouter"
	c.baseURL, c.apiKey = "https://openrouter.ai/api/v1", "ambient-key"
	c.console = console()
	return c
}

// Session row with a model and an optional model config encoded as JSON text
func restoreRow(model any, modelConfig map[string]any) map[string]any {
	var cfg any
	if modelConfig != nil {
		raw, _ := json.Marshal(modelConfig)
		cfg = string(raw)
	}
	return map[string]any{"model": model, "model_config": cfg}
}

func TestRestoreSessionModelRestoresModelAndProvider(t *testing.T) {
	resumeTestDB(t)
	recordPrints(t)
	c := restoreStub()
	c.restoreSessionModel(restoreRow("glm-4.7", map[string]any{"gateway_runtime": map[string]any{"provider": "custom:feather", "base_url": "https://f/v1"}}), false)
	if c.model != "glm-4.7" || c.provider != "custom:feather" || c.requestedProvider != "custom:feather" || c.baseURL != "https://f/v1" {
		t.Fatalf("route %q %q %q %q", c.model, c.provider, c.requestedProvider, c.baseURL)
	}
	if c.explicitAPIKey != "" || c.explicitBaseURL != "https://f/v1" {
		t.Fatalf("explicit %q %q", c.explicitAPIKey, c.explicitBaseURL)
	}
}

func TestRestoreSessionModelExplicitCliFlagWins(t *testing.T) {
	c := restoreStub()
	c.model, c.explicitModelOverride = "cli-flag-model", true
	c.restoreSessionModel(restoreRow("glm-4.7", nil), false)
	if c.model != "cli-flag-model" || c.provider != "openrouter" {
		t.Fatalf("route %q %q", c.model, c.provider)
	}
}

func TestRestoreSessionModelNoStoredModelIsNoop(t *testing.T) {
	c := restoreStub()
	c.restoreSessionModel(restoreRow(nil, nil), false)
	if c.model != "ambient-model" {
		t.Fatalf("model %q", c.model)
	}
}

func TestRestoreSessionModelMatchingStateIsSilentNoop(t *testing.T) {
	rec := recordPrints(t)
	c := restoreStub()
	c.model, c.provider, c.requestedProvider = "glm-4.7", "custom:feather", "custom:feather"
	c.restoreSessionModel(restoreRow("glm-4.7", map[string]any{"gateway_runtime": map[string]any{"provider": "custom:feather"}}), false)
	if out := strings.Join(rec.plain(), "\n"); out != "" {
		t.Fatalf("printed %q", out)
	}
}

// Stored model config of a session row decoded from its JSON text
func storedModelConfig(t *testing.T, db *state.SessionDB, sessionID string) (*pyjson.Object, *pyjson.Object) {
	t.Helper()
	meta, err := db.GetSession(context.Background(), sessionID)
	if err != nil || meta == nil {
		t.Fatalf("session %v %v", meta, err)
	}
	raw, _ := meta.Get("model_config")
	decoded, err := pyjson.Loads(raw.(string))
	if err != nil {
		t.Fatal(err)
	}
	return meta, decoded.(*pyjson.Object)
}

func TestPersistModelSwitchWritesModelAndBothRouteShapes(t *testing.T) {
	db := resumeTestDB(t, state.NewSession{ID: "s1", Model: "m0"})
	c := restoreStub()
	c.sessionDB, c.sessionID = db, "s1"
	c.persistModelSwitchToSession(&provider.ModelSwitchResult{NewModel: "deepseek-v4-flash-free", TargetProvider: "custom:opencode-zen", BaseURL: "https://oz/v1"})
	meta, cfg := storedModelConfig(t, db, "s1")
	if model, _ := meta.Get("model"); model != "deepseek-v4-flash-free" {
		t.Fatalf("model %v", model)
	}
	gw, _ := cfg.Get("gateway_runtime")
	nested := gw.(*pyjson.Object)
	if p, _ := nested.Get("provider"); p != "custom:opencode-zen" {
		t.Fatalf("nested %v", pyjson.Plain(nested))
	}
	if p, _ := cfg.Get("provider"); p != "custom:opencode-zen" {
		t.Fatalf("top %v", pyjson.Plain(cfg))
	}
	if u, _ := cfg.Get("base_url"); u != "https://oz/v1" {
		t.Fatalf("top %v", pyjson.Plain(cfg))
	}
	if mode, ok := nested.Get("api_mode"); !ok || mode != nil {
		t.Fatalf("nested api_mode %v %v", mode, ok)
	}
	if _, ok := cfg.Get("api_mode"); ok {
		t.Fatalf("top api_mode kept %v", pyjson.Plain(cfg))
	}
}

func TestPersistModelSwitchClearsStaleRouteKeys(t *testing.T) {
	db := resumeTestDB(t, state.NewSession{ID: "stale1", Model: "m0"})
	c := restoreStub()
	c.sessionDB, c.sessionID = db, "stale1"
	c.persistModelSwitchToSession(&provider.ModelSwitchResult{NewModel: "claude-x", TargetProvider: "custom:feather", BaseURL: "https://feather/v1", APIMode: "anthropic_messages"})
	c.persistModelSwitchToSession(&provider.ModelSwitchResult{NewModel: "gpt-5.4", TargetProvider: "openrouter", BaseURL: "https://openrouter.ai/api/v1"})
	meta, cfg := storedModelConfig(t, db, "stale1")
	if p, _ := cfg.Get("provider"); p != "openrouter" {
		t.Fatalf("provider %v", p)
	}
	if _, ok := cfg.Get("api_mode"); ok {
		t.Fatalf("stale api_mode kept %v", pyjson.Plain(cfg))
	}
	gw, _ := cfg.Get("gateway_runtime")
	nested := gw.(*pyjson.Object)
	if p, _ := nested.Get("provider"); p != "openrouter" {
		t.Fatalf("nested %v", pyjson.Plain(nested))
	}
	if mode, _ := nested.Get("api_mode"); mode != nil {
		t.Fatalf("nested api_mode %v", mode)
	}
	runtime := state.SessionGatewayRuntime(meta)
	if p, _ := runtime.Get("provider"); p != "openrouter" {
		t.Fatalf("runtime %v", pyjson.Plain(runtime))
	}
	if _, ok := runtime.Get("api_mode"); ok {
		t.Fatalf("runtime api_mode %v", pyjson.Plain(runtime))
	}
}

func TestShowRecentSessionsIncludesIndexesAndResumeHint(t *testing.T) {
	db := resumeTestDB(t, state.NewSession{ID: "sess_001"}, state.NewSession{ID: "sess_002"})
	for id, title := range map[string]string{"sess_001": "Research", "sess_002": "Coding"} {
		if _, err := db.SetSessionTitle(context.Background(), id, title); err != nil {
			t.Fatal(err)
		}
	}
	rec := recordPrints(t)
	c := bareCLI()
	c.sessionDB, c.sessionID = db, "current_session"
	if !c.showRecentSessions("resume", 10) {
		t.Fatal("nothing shown")
	}
	out := strings.Join(rec.plain(), "\n")
	if !strings.Contains(out, "Coding") || !strings.Contains(out, "Research") {
		t.Fatalf("output %q", out)
	}
}

func TestHandleResumeByIndexOutOfRange(t *testing.T) {
	db := resumeTestDB(t, state.NewSession{ID: "sess_002"})
	if _, err := db.SetSessionTitle(context.Background(), "sess_002", "Coding"); err != nil {
		t.Fatal(err)
	}
	rec := recordPrints(t)
	c := bareCLI()
	c.sessionDB, c.sessionID = db, "current_session"
	c.handleResumeCommand("/resume 9")
	if strings.Join(rec.plain(), "\n") == "" || c.sessionID != "current_session" {
		t.Fatalf("printed %q session %q", strings.Join(rec.plain(), "\n"), c.sessionID)
	}
}

func TestBareResumeArmsPendingSelection(t *testing.T) {
	db := resumeTestDB(t, state.NewSession{ID: "sess_a"}, state.NewSession{ID: "sess_b"})
	for id, title := range map[string]string{"sess_a": "Alpha", "sess_b": "Beta"} {
		if _, err := db.SetSessionTitle(context.Background(), id, title); err != nil {
			t.Fatal(err)
		}
	}
	recordPrints(t)
	c := bareCLI()
	c.sessionDB, c.sessionID = db, "current_session"
	c.handleResumeCommand("/resume")
	if len(c.pendingResumeSessions) != 2 {
		t.Fatalf("pending %v", c.pendingResumeSessions)
	}
}

// Database holding the Coding and Research sessions, Research created first with one exchange
func twoSessionDB(t *testing.T, researchCwd string) *state.SessionDB {
	t.Helper()
	db := resumeTestDB(t, state.NewSession{ID: "sess_001", Cwd: researchCwd})
	if err := db.CreateSession(state.NewSession{ID: "sess_002", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for id, title := range map[string]string{"sess_001": "Research", "sess_002": "Coding"} {
		if _, err := db.SetSessionTitle(ctx, id, title); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []map[string]any{{"role": "user", "content": "hello"}, {"role": "assistant", "content": "hi"}} {
		if _, err := db.AppendMessage(ctx, "sess_001", m, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.AppendMessage(ctx, "sess_002", map[string]any{"role": "user", "content": "build"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	return db
}

// CLI on the current session of a resume test with the minimal recap display
func resumeCLI(db *state.SessionDB) *CLI {
	c := bareCLI()
	c.sessionDB, c.sessionID, c.resumeDisplay = db, "current_session", "minimal"
	c.console = console()
	return c
}

func TestHandleResumeByIndexSwitchesToNumberedSession(t *testing.T) {
	db := twoSessionDB(t, "")
	rec := recordPrints(t)
	c := resumeCLI(db)
	c.handleResumeCommand("/resume 2")
	if c.sessionID != "sess_001" || !strings.Contains(strings.Join(rec.plain(), "\n"), "Research") {
		t.Fatalf("session %q printed %q", c.sessionID, rec.plain())
	}
}

func TestHandleResumeRestoresRecordedCwd(t *testing.T) {
	t.Chdir(t.TempDir())
	recorded := t.TempDir()
	db := twoSessionDB(t, recorded)
	recordPrints(t)
	t.Setenv("TERMINAL_CWD", "")
	c := resumeCLI(db)
	c.handleResumeCommand("/resume Research")
	if os.Getenv("TERMINAL_CWD") != recorded {
		t.Fatalf("TERMINAL_CWD %q", os.Getenv("TERMINAL_CWD"))
	}
	if wd, _ := os.Getwd(); wd != recorded {
		t.Fatalf("cwd %q", wd)
	}
}

func TestSessionsCommandRestoresRecordedCwd(t *testing.T) {
	t.Chdir(t.TempDir())
	recorded := t.TempDir()
	db := twoSessionDB(t, recorded)
	recordPrints(t)
	t.Setenv("TERMINAL_CWD", "")
	c := resumeCLI(db)
	c.handleSessionsCommand("/sessions Research")
	if os.Getenv("TERMINAL_CWD") != recorded {
		t.Fatalf("TERMINAL_CWD %q", os.Getenv("TERMINAL_CWD"))
	}
}

func TestPendingNumberResumesSelectedSession(t *testing.T) {
	db := twoSessionDB(t, "")
	recordPrints(t)
	c := resumeCLI(db)
	c.pendingResumeSessions = c.listRecentSessions(10)
	if !c.consumePendingResumeSelection("2") {
		t.Fatal("selection not consumed")
	}
	if c.sessionID != "sess_001" || c.pendingResumeSessions != nil {
		t.Fatalf("session %q pending %v", c.sessionID, c.pendingResumeSessions)
	}
}

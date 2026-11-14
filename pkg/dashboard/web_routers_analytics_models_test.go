package dashboard

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Session store of an isolated home that the model analytics opens as the process store
func modelsAnalyticsDB(t *testing.T) *state.SessionDB {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	db, err := state.NewSessionDB(context.Background(), filepath.Join(h, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Model analytics of the last 30 days as plain maps
func runModelsAnalytics(t *testing.T) map[string]any {
	t.Helper()
	out, err := modelsAnalytics(context.Background(), 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	return pyjson.Plain(out).(map[string]any)
}

// Model cards keyed by model and provider
func cardsByPair(result map[string]any) map[[2]string]map[string]any {
	out := map[[2]string]map[string]any{}
	for _, m := range result["models"].([]any) {
		card := m.(map[string]any)
		out[[2]string{card["model"].(string), card["provider"].(string)}] = card
	}
	return out
}

// Records one main loop API call delta on the route active at call time
func touchUsage(t *testing.T, db *state.SessionDB, sessionID, model, provider string, in, out int64) {
	t.Helper()
	if err := db.UpdateTokenCounts(sessionID, state.TokenCountDelta{InputTokens: in, OutputTokens: out, Model: &model,
		BillingProvider: &provider, APICallCount: 1}); err != nil {
		t.Fatal(err)
	}
}

func execSQL(t *testing.T, db *state.SessionDB, query string, args ...any) {
	t.Helper()
	ctx := context.Background()
	if err := db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, query, args...)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func createSession(t *testing.T, db *state.SessionDB, id, model string) {
	t.Helper()
	if err := db.CreateSession(state.NewSession{ID: id, Source: "cli", Model: model}); err != nil {
		t.Fatal(err)
	}
}

func appendMessage(t *testing.T, db *state.SessionDB, id, role, content string, toolCalls []any) {
	t.Helper()
	fields := map[string]any{"role": role, "content": content}
	if toolCalls != nil {
		fields["tool_calls"] = toolCalls
	}
	if _, err := db.AppendMessage(context.Background(), id, fields, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
}

func recordAux(t *testing.T, db *state.SessionDB, id, task, model, provider string, in, out int64) {
	t.Helper()
	if err := db.RecordAuxiliaryUsage(context.Background(), id, task, state.AuxiliaryUsage{Model: &model, BillingProvider: &provider,
		InputTokens: in, OutputTokens: out}); err != nil {
		t.Fatal(err)
	}
}

func TestSessionSwitchingProvidersYieldsSeparatedRows(t *testing.T) {
	db := modelsAnalyticsDB(t)
	createSession(t, db, "s1", "gpt-5.6-sol")
	touchUsage(t, db, "s1", "gpt-5.6-sol", "openai-codex", 30_000, 1_000)
	touchUsage(t, db, "s1", "anthropic/claude-opus-5", "nous", 31_000, 2_000)
	execSQL(t, db, "UPDATE sessions SET model = ?, billing_provider = ? WHERE id = ?", "anthropic/claude-opus-5", "nous", "s1")

	pairs := cardsByPair(runModelsAnalytics(t))
	first, second := pairs[[2]string{"gpt-5.6-sol", "openai-codex"}], pairs[[2]string{"anthropic/claude-opus-5", "nous"}]
	if first == nil || second == nil {
		t.Fatalf("pairs %v", pairs)
	}
	if !pyrepr.Equal(first["input_tokens"], int64(30_000)) || !pyrepr.Equal(second["input_tokens"], int64(31_000)) ||
		!pyrepr.Equal(first["sessions"], int64(1)) || !pyrepr.Equal(second["sessions"], int64(1)) {
		t.Fatalf("first %v second %v", first, second)
	}
}

func TestPhantomPairNeverAppearsForSwitchedSession(t *testing.T) {
	db := modelsAnalyticsDB(t)
	createSession(t, db, "s1", "gpt-5.6-sol")
	touchUsage(t, db, "s1", "gpt-5.6-sol", "openai-codex", 100, 10)
	touchUsage(t, db, "s1", "deepseek-v4-pro", "deepseek", 200, 20)
	execSQL(t, db, "UPDATE sessions SET model = ?, billing_provider = ? WHERE id = ?", "gpt-5.6-sol", "deepseek", "s1")

	pairs := cardsByPair(runModelsAnalytics(t))
	want := map[[2]string]bool{{"gpt-5.6-sol", "openai-codex"}: true, {"deepseek-v4-pro", "deepseek"}: true}
	got := map[[2]string]bool{}
	for k := range pairs {
		got[k] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pairs %v", got)
	}
}

func TestToolCallsSurviveTheSourceSwitch(t *testing.T) {
	db := modelsAnalyticsDB(t)
	createSession(t, db, "s1", "gpt-5.6-sol")
	touchUsage(t, db, "s1", "gpt-5.6-sol", "openai-codex", 100, 10)
	appendMessage(t, db, "s1", "assistant", "running", []any{map[string]any{"name": "terminal", "arguments": map[string]any{}}})
	appendMessage(t, db, "s1", "tool", "result", nil)
	appendMessage(t, db, "s1", "assistant", "done", []any{map[string]any{"name": "read_file", "arguments": map[string]any{}}})
	appendMessage(t, db, "s1", "tool", "result", nil)

	row := cardsByPair(runModelsAnalytics(t))[[2]string{"gpt-5.6-sol", "openai-codex"}]
	if !pyrepr.Equal(row["tool_calls"], int64(2)) {
		t.Fatalf("row %v", row)
	}
}

func TestFallbackProviderSessionAttributedToActualRoute(t *testing.T) {
	db := modelsAnalyticsDB(t)
	createSession(t, db, "s1", "gpt-5.6-sol")
	touchUsage(t, db, "s1", "gpt-5.6-sol", "openai-codex", 100, 10)
	recordAux(t, db, "s1", "vision", "gemini-3-flash", "gemini", 500, 50)

	pairs := cardsByPair(runModelsAnalytics(t))
	main, aux := pairs[[2]string{"gpt-5.6-sol", "openai-codex"}], pairs[[2]string{"gemini-3-flash", "gemini"}]
	if main == nil || aux == nil || !pyrepr.Equal(main["input_tokens"], int64(100)) || !pyrepr.Equal(aux["input_tokens"], int64(500)) ||
		!pyrepr.Equal(main["sessions"], int64(1)) {
		t.Fatalf("pairs %v", pairs)
	}
}

func TestDBWithoutSessionModelUsageFallsBackToSessions(t *testing.T) {
	db := modelsAnalyticsDB(t)
	createSession(t, db, "s1", "gpt-5.6-sol")
	touchUsage(t, db, "s1", "gpt-5.6-sol", "openai-codex", 100, 10)
	appendMessage(t, db, "s1", "assistant", "running", []any{map[string]any{"name": "terminal", "arguments": map[string]any{}}})
	appendMessage(t, db, "s1", "tool", "result", nil)
	execSQL(t, db, "DROP TABLE session_model_usage")

	row := cardsByPair(runModelsAnalytics(t))[[2]string{"gpt-5.6-sol", "openai-codex"}]
	if !pyrepr.Equal(row["input_tokens"], int64(100)) || !pyrepr.Equal(row["tool_calls"], int64(1)) {
		t.Fatalf("row %v", row)
	}
}

func TestWindowExcludesOldSessions(t *testing.T) {
	db := modelsAnalyticsDB(t)
	createSession(t, db, "s1", "gpt-5.6-sol")
	touchUsage(t, db, "s1", "gpt-5.6-sol", "openai-codex", 100, 10)
	execSQL(t, db, "UPDATE sessions SET started_at = ? WHERE id = ?", float64(time.Now().UnixNano())/1e9-90*86400, "s1")

	if models := runModelsAnalytics(t)["models"].([]any); len(models) != 0 {
		t.Fatalf("models %v", models)
	}
}

// Session on a model and billing provider started now
func providerSession(t *testing.T, db *state.SessionDB, id, model, provider string) {
	createSession(t, db, id, model)
	execSQL(t, db, "UPDATE sessions SET billing_provider = ?, started_at = ? WHERE id = ?", provider, float64(time.Now().UnixNano())/1e9, id)
}

func TestAuxTasksDoNotDuplicateTheModelCard(t *testing.T) {
	db := modelsAnalyticsDB(t)
	providerSession(t, db, "s1", "gpt-5.6-terra", "openai-codex")
	for _, task := range []string{"vision", "compression", "title_generation"} {
		recordAux(t, db, "s1", task, "gpt-5.6-terra", "openai-codex", 100, 10)
	}
	result := runModelsAnalytics(t)
	models := result["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("models %v", models)
	}
	card := models[0].(map[string]any)
	if card["model"] != "gpt-5.6-terra" || card["provider"] != "openai-codex" || !pyrepr.Equal(card["input_tokens"], int64(300)) ||
		!pyrepr.Equal(card["sessions"], result["totals"].(map[string]any)["total_sessions"]) {
		t.Fatalf("card %v totals %v", card, result["totals"])
	}
}

func TestAuxOnlyModelStillGetsItsOwnRow(t *testing.T) {
	db := modelsAnalyticsDB(t)
	providerSession(t, db, "s1", "gpt-5.6-terra", "openai-codex")
	recordAux(t, db, "s1", "vision", "gemini-3-flash", "gemini", 500, 50)
	pairs := cardsByPair(runModelsAnalytics(t))
	if pairs[[2]string{"gemini-3-flash", "gemini"}] == nil || pairs[[2]string{"gpt-5.6-terra", "openai-codex"}] == nil {
		t.Fatalf("pairs %v", pairs)
	}
}

func TestDistinctModelsCountsAuxOnlyModels(t *testing.T) {
	db := modelsAnalyticsDB(t)
	providerSession(t, db, "s1", "gpt-5.6-terra", "openai-codex")
	recordAux(t, db, "s1", "title_generation", "gpt-5.4-mini", "openai-codex", 50, 5)
	result := runModelsAnalytics(t)
	names := map[string]bool{}
	for _, m := range result["models"].([]any) {
		names[m.(map[string]any)["model"].(string)] = true
	}
	if !reflect.DeepEqual(names, map[string]bool{"gpt-5.6-terra": true, "gpt-5.4-mini": true}) ||
		!pyrepr.Equal(result["totals"].(map[string]any)["distinct_models"], int64(len(names))) {
		t.Fatalf("names %v totals %v", names, result["totals"])
	}
}

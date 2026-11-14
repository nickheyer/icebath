package agent

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Parent agent carrying the request inputs a review fork must inherit
func reviewParentAgent() *AIAgent {
	return &AIAgent{
		model: "test-model", platform: "test", provider: "openai", sessionID: "sess-123", quietMode: true,
		memoryEnabled: true, memoryNudgeInterval: 5, skillNudgeInterval: 5,
		systemPromptState: systemPromptState{cachedSystemPrompt: "PARENT-SYSTEM-PROMPT-BYTES — must be inherited verbatim " +
			"for prefix-cache parity"},
		ephemeralSystemPrompt: "WebUI session context:\n- Pinned per-request gateway context",
		sessionStart:          time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		enabledToolsets:       []string{"memory", "skills", "terminal"},
		disabledToolsets:      []string{"spotify", "feishu_doc"},
		reasoningConfig:       map[string]any{"enabled": true, "effort": "medium"},
		prefillMessages:       []any{map[string]any{"role": "user", "content": "prefill turn"}},
		providersAllowed:      []any{"anthropic"},
		providerSort:          "throughput",
		requestOverrides:      map[string]any{},
		backgroundReviewState: backgroundReviewState{reviewPrompts: map[string]string{
			"_MEMORY_REVIEW_PROMPT": "review memory", "_SKILL_REVIEW_PROMPT": "review skills", "_COMBINED_REVIEW_PROMPT": "review both",
		}},
	}
}

// Captured fork constructor keywords and the forks it built
type forkRecorder struct {
	opts  []AIAgentOptions
	forks []*AIAgent
}

// Replaces the fork constructor with a recorder building bare agents, then calls init on each
func recordForks(t *testing.T, init func(*AIAgent)) *forkRecorder {
	t.Helper()
	rec := &forkRecorder{}
	prev := newCacheParityForkAgent
	t.Cleanup(func() { newCacheParityForkAgent = prev })
	newCacheParityForkAgent = func(_ context.Context, o AIAgentOptions) (*AIAgent, error) {
		fork := &AIAgent{ephemeralSystemPrompt: o.EphemeralSystemPrompt, validToolNames: map[string]bool{}}
		if init != nil {
			init(fork)
		}
		rec.opts, rec.forks = append(rec.opts, o), append(rec.forks, fork)
		return fork, nil
	}
	return rec
}

// Replaces the review runtime resolver with a fixed runtime
func replaceReviewRuntime(t *testing.T, rt reviewRuntime) {
	t.Helper()
	prev := resolveReviewRuntime
	t.Cleanup(func() { resolveReviewRuntime = prev })
	resolveReviewRuntime = func(context.Context, *AIAgent, map[string]any) reviewRuntime { return rt }
}

func routedAuxRuntime() reviewRuntime {
	return reviewRuntime{Provider: "openrouter", Model: "aux-cheap-model", APIKey: "test-key", RequestOverrides: map[string]any{},
		Args: []string{}, Routed: true}
}

// Collects the warnings a parent emits through its status callback
func captureStatusWarnings(a *AIAgent) *[]string {
	var out []string
	a.statusCallback = func(kind string, msg any) {
		if s, ok := msg.(string); ok {
			out = append(out, s)
		}
	}
	return &out
}

func toolNames(tools []any) []string {
	var names []string
	for _, tool := range tools {
		names = append(names, tool.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	return names
}

func TestReviewForkInheritedToolsSurviveCompactionRefresh(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := reviewParentAgent()
	for _, name := range []string{"terminal_command", "read_file", "memory", "fact_store", "fact_feedback"} {
		a.tools = append(a.tools, map[string]any{"type": "function", "function": map[string]any{"name": name}})
	}
	recordForks(t, nil)
	fork, _, routed, err := buildCacheParityFork(context.Background(), a, map[string]any{}, 5, "")
	if err != nil || routed {
		t.Fatalf("routed=%v err=%v", routed, err)
	}
	if !reflect.DeepEqual(fork.tools, a.tools) {
		t.Fatalf("fork tools %v", fork.tools)
	}
	if reflect.ValueOf(fork.tools[0]).Pointer() == reflect.ValueOf(a.tools[0]).Pointer() {
		t.Fatal("fork tools alias the parent's entries")
	}
	if added := mcp.RefreshAgentMCPTools(context.Background(), fork, mcp.RefreshOptions{ContentAware: true}); len(added) != 0 {
		t.Fatalf("refresh added %v", added)
	}
	want := []string{"terminal_command", "read_file", "memory", "fact_store", "fact_feedback"}
	if got := toolNames(fork.tools); !reflect.DeepEqual(got, want) {
		t.Fatalf("tools after refresh %v", got)
	}
	wantNames := map[string]bool{}
	for _, n := range want {
		wantNames[n] = true
	}
	if !reflect.DeepEqual(fork.validToolNames, wantNames) {
		t.Fatalf("valid tool names %v", fork.validToolNames)
	}
}

func TestUnroutedReviewForkInheritsEmptyToolSurface(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := reviewParentAgent()
	a.tools = []any{}
	recordForks(t, func(fork *AIAgent) {
		fork.tools = []any{map[string]any{"type": "function", "function": map[string]any{"name": "newly_available"}}}
		fork.validToolNames = map[string]bool{"newly_available": true}
	})
	fork, _, routed, err := buildCacheParityFork(context.Background(), a, map[string]any{}, 5, "")
	if err != nil || routed {
		t.Fatalf("routed=%v err=%v", routed, err)
	}
	if fork.tools == nil || len(fork.tools) != 0 || len(fork.validToolNames) != 0 {
		t.Fatalf("fork tools %v names %v", fork.tools, fork.validToolNames)
	}
	if added := mcp.RefreshAgentMCPTools(context.Background(), fork, mcp.RefreshOptions{ContentAware: true}); len(added) != 0 {
		t.Fatalf("refresh added %v", added)
	}
	if len(fork.tools) != 0 || len(fork.validToolNames) != 0 {
		t.Fatalf("refresh changed the frozen surface %v %v", fork.tools, fork.validToolNames)
	}
}

func TestSameModelReviewSurfacesIgnoredReasoningEffortOnce(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := reviewParentAgent()
	warnings := captureStatusWarnings(a)
	rec := recordForks(t, nil)
	cfg := map[string]any{"reasoning_effort": "low"}
	if _, _, routed, err := buildCacheParityFork(context.Background(), a, cfg, 5, ""); err != nil || routed {
		t.Fatalf("routed=%v err=%v", routed, err)
	}
	if len(*warnings) != 1 || !strings.Contains((*warnings)[0], "reasoning_effort") {
		t.Fatalf("expected exactly one notice, got %v", *warnings)
	}
	if !reflect.DeepEqual(rec.opts[0].ReasoningConfig, a.reasoningConfig) {
		t.Fatalf("fork reasoning config %v", rec.opts[0].ReasoningConfig)
	}
	if _, _, _, err := buildCacheParityFork(context.Background(), a, cfg, 5, ""); err != nil {
		t.Fatal(err)
	}
	if len(*warnings) != 1 {
		t.Fatalf("notice repeated per fork: %v", *warnings)
	}
}

func TestReviewEffortNoticeOnlyForSameModelReviewForks(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	recordForks(t, nil)
	warns := func(cfg map[string]any, origin string) []string {
		a := reviewParentAgent()
		warnings := captureStatusWarnings(a)
		if _, _, _, err := buildCacheParityFork(context.Background(), a, cfg, 5, origin); err != nil {
			t.Fatal(err)
		}
		return *warnings
	}
	if w := warns(map[string]any{"reasoning_effort": ""}, ""); len(w) != 0 {
		t.Fatalf("empty effort warned %v", w)
	}
	if w := warns(map[string]any{}, ""); len(w) != 0 {
		t.Fatalf("unset effort warned %v", w)
	}
	if w := warns(map[string]any{"reasoning_effort": "low"}, "side_question"); len(w) != 0 {
		t.Fatalf("side question fork warned %v", w)
	}
	replaceReviewRuntime(t, routedAuxRuntime())
	if w := warns(map[string]any{"reasoning_effort": "low"}, ""); len(w) != 0 {
		t.Fatalf("routed fork warned %v", w)
	}
}

func openReviewDB(t *testing.T) (*state.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := state.NewSessionDB(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func TestSameModelForkInheritsParentCacheScopeGatewayKey(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	db, _ := openReviewDB(t)
	a := reviewParentAgent()
	a.gatewaySessionKey, a.sessionDB = "gw-key-1", db
	if err := db.CreateSession(state.NewSession{ID: "sess-123", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	recordForks(t, nil)
	fork, _, routed, err := buildCacheParityFork(context.Background(), a, map[string]any{}, 5, "")
	if err != nil || routed {
		t.Fatalf("routed=%v err=%v", routed, err)
	}
	btw, _, _, err := buildCacheParityFork(context.Background(), a, map[string]any{}, 5, "side_question")
	if err != nil {
		t.Fatal(err)
	}
	if fork.promptCacheForkTag != "review" || btw.promptCacheForkTag != "" {
		t.Fatalf("tags %q %q", fork.promptCacheForkTag, btw.promptCacheForkTag)
	}
	parentScope := provider.ResolvePromptCacheScope(a.cacheScopeAgent())
	if !strings.HasPrefix(parentScope, "gwk_") {
		t.Fatalf("parent scope %q", parentScope)
	}
	if fork.inheritedCacheScope != parentScope || provider.DeclaredConversationScope(fork.cacheScopeAgent()) != parentScope ||
		provider.ResolvePromptCacheScope(fork.cacheScopeAgent()) != parentScope {
		t.Fatalf("fork scope %q diverged from %q", fork.inheritedCacheScope, parentScope)
	}
}

func TestSameModelForkInheritsParentCacheScopeRotatedLineage(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	db, _ := openReviewDB(t)
	a := reviewParentAgent()
	a.sessionDB = db
	if err := db.CreateSession(state.NewSession{ID: "root-sid", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := db.EndSession("root-sid", "compression"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "sess-123", Source: "test", ParentSessionID: "root-sid"}); err != nil {
		t.Fatal(err)
	}
	recordForks(t, nil)
	fork, _, routed, err := buildCacheParityFork(context.Background(), a, map[string]any{}, 5, "")
	if err != nil || routed {
		t.Fatalf("routed=%v err=%v", routed, err)
	}
	if got := provider.ResolvePromptCacheScope(a.cacheScopeAgent()); got != "root-sid" {
		t.Fatalf("parent scope %q", got)
	}
	if fork.inheritedCacheScope != "root-sid" || provider.ResolvePromptCacheScope(fork.cacheScopeAgent()) != "root-sid" {
		t.Fatalf("fork scope %q", fork.inheritedCacheScope)
	}
	if provider.DeclaredConversationScope(fork.cacheScopeAgent()) != "" || provider.DeclaredConversationScope(a.cacheScopeAgent()) != "" {
		t.Fatal("a physical lineage root was declared as a gateway scope")
	}
	if fork.conversationRootID() != "root-sid" || a.conversationRootID() != "root-sid" {
		t.Fatalf("roots %q %q", fork.conversationRootID(), a.conversationRootID())
	}
}

func TestRoutedForkDoesNotInheritCacheScope(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := reviewParentAgent()
	a.gatewaySessionKey = "gw-key-1"
	recordForks(t, nil)
	replaceReviewRuntime(t, reviewRuntime{Routed: true, Model: "other-model"})
	fork, _, routed, err := buildCacheParityFork(context.Background(), a, map[string]any{}, 5, "")
	if err != nil || !routed {
		t.Fatalf("routed=%v err=%v", routed, err)
	}
	if fork.inheritedCacheScope != "" {
		t.Fatalf("routed fork inherited scope %q", fork.inheritedCacheScope)
	}
}

func reviewUsage(overrides map[string]any) map[string]any {
	base := map[string]any{"model": "test-model", "provider": "test-provider", "base_url": "https://example.invalid/v1",
		"input_tokens": 12000, "output_tokens": 2400, "cache_read_tokens": 190000, "cache_write_tokens": 0,
		"reasoning_tokens": 0, "api_calls": 5, "estimated_cost_usd": 0.05}
	for k, v := range overrides {
		base[k] = v
	}
	return base
}

// Rows of session_model_usage for one session ordered by task
func usageRows(t *testing.T, path, sessionID string) []map[string]any {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	rows, err := raw.Query("SELECT task, model, billing_provider, input_tokens, output_tokens, cache_read_tokens, api_call_count, "+
		"estimated_cost_usd FROM session_model_usage WHERE session_id = ? ORDER BY task", sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var task, model, billing string
		var in, outTok, cacheRead, calls int64
		var cost float64
		if err := rows.Scan(&task, &model, &billing, &in, &outTok, &cacheRead, &calls, &cost); err != nil {
			t.Fatal(err)
		}
		out = append(out, map[string]any{"task": task, "model": model, "billing_provider": billing, "input_tokens": in,
			"output_tokens": outTok, "cache_read_tokens": cacheRead, "api_call_count": calls, "estimated_cost_usd": cost})
	}
	return out
}

func usageParent(t *testing.T, sessionID string) (*AIAgent, string) {
	db, path := openReviewDB(t)
	if err := db.CreateSession(state.NewSession{ID: "sess-parent", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	return &AIAgent{sessionDB: db, sessionID: sessionID}, path
}

func TestRecordsForkUsageAgainstParentSession(t *testing.T) {
	parent, path := usageParent(t, "sess-parent")
	recordReviewUsageToParent(context.Background(), parent, reviewUsage(nil), "", "")
	rows := usageRows(t, path, "sess-parent")
	want := []map[string]any{{"task": "background_review", "model": "test-model", "billing_provider": "test-provider",
		"input_tokens": int64(12000), "output_tokens": int64(2400), "cache_read_tokens": int64(190000), "api_call_count": int64(5),
		"estimated_cost_usd": 0.05}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows %v", rows)
	}
}

func TestAccumulatesRepeatedForksSameModel(t *testing.T) {
	parent, path := usageParent(t, "sess-parent")
	recordReviewUsageToParent(context.Background(), parent, reviewUsage(map[string]any{"api_calls": 5}), "", "")
	recordReviewUsageToParent(context.Background(), parent, reviewUsage(map[string]any{"api_calls": 7}), "", "")
	rows := usageRows(t, path, "sess-parent")
	if len(rows) != 1 || rows[0]["input_tokens"] != int64(24000) || rows[0]["api_call_count"] != int64(12) {
		t.Fatalf("rows %v", rows)
	}
}

func TestNoopWhenForkMadeNoCalls(t *testing.T) {
	parent, path := usageParent(t, "sess-parent")
	recordReviewUsageToParent(context.Background(), parent, reviewUsage(map[string]any{"input_tokens": 0, "output_tokens": 0,
		"cache_read_tokens": 0, "cache_write_tokens": 0, "reasoning_tokens": 0, "api_calls": 0}), "", "")
	if rows := usageRows(t, path, "sess-parent"); len(rows) != 0 {
		t.Fatalf("rows %v", rows)
	}
}

func TestNoopWhenParentHasNoSessionID(t *testing.T) {
	parent, path := usageParent(t, "")
	recordReviewUsageToParent(context.Background(), parent, reviewUsage(nil), "", "")
	if rows := usageRows(t, path, "sess-parent"); len(rows) != 0 {
		t.Fatalf("rows %v", rows)
	}
}

func TestSurvivesAccountingFailure(t *testing.T) {
	db, _ := openReviewDB(t)
	db.Close()
	recordReviewUsageToParent(context.Background(), &AIAgent{sessionDB: db, sessionID: "sess-parent"}, reviewUsage(nil), "", "")
}

func TestClassifyReviewResult(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	cases := []struct {
		actions []string
		want    string
	}{
		{nil, "none"},
		{[]string{"Memory updated"}, "memory"},
		{[]string{"Skill 'x' patched"}, "skill"},
		{[]string{"Memory updated", "Skill 'x' created"}, "skill+memory"},
		{[]string{"Skipped: no skill worth saving"}, "none"},
		{[]string{"📝 Skill 'deploy' patched: \"a\" → \"b\""}, "skill"},
		{[]string{"User profile ➕ prefers terse"}, "memory"},
	}
	for _, c := range cases {
		if got := classifyReviewResult(ctx, c.actions); got != c.want {
			t.Fatalf("classify %q = %q, want %q", c.actions, got, c.want)
		}
	}
}

func TestEnabledConfigFailureLogsWarning(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "home-is-a-file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", blocker)
	records := captureLogger(t, "agent.background_review", slog.LevelWarn)
	if enabled, _ := loadBackgroundReviewSettings(context.Background()); !enabled {
		t.Fatal("a config read failure disabled automatic review")
	}
	if len(records()) == 0 {
		t.Fatal("no warning logged for the config read failure")
	}
}

package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

type titlerCall struct {
	sessionID, userMessage string
	options                autoTitleOptions
}

func patchAutoTitler(t *testing.T) *[]titlerCall {
	calls := &[]titlerCall{}
	prev := maybeAutoTitleFn
	maybeAutoTitleFn = func(_ context.Context, _ any, sessionID, userMessage string, o autoTitleOptions) (any, bool) {
		*calls = append(*calls, titlerCall{sessionID, userMessage, o})
		return nil, false
	}
	t.Cleanup(func() { maybeAutoTitleFn = prev })
	return calls
}

// Only what the turn start titling step reads off an agent, over a real session row
func titlingAgent(t *testing.T, platform string) (*AIAgent, *state.SessionDB) {
	t.Helper()
	db := restoreRealDB(t, "sess-1", "cli")
	h := caseHome(t)
	a := &AIAgent{platform: platform, sessionID: "sess-1", model: "test/model", provider: "openrouter", apiKey: "sk-x", apiMode: "chat_completions",
		sessionDB: db, sessionDBCreated: true, env: &tools.Env{Home: h}}
	a.setBaseURL("https://openrouter.ai/api/v1")
	return a, db
}

func titleTurn(t *testing.T, platform string, rows []map[string]any) (*[]titlerCall, *state.SessionDB) {
	t.Helper()
	calls := patchAutoTitler(t)
	a, db := titlingAgent(t, platform)
	maybeTitleSessionAtTurnStart(context.Background(), a, rowsToMessages(rows), nil)
	return calls, db
}

func storedTitle(t *testing.T, db *state.SessionDB) (string, string) {
	t.Helper()
	title, _, err := db.GetSessionTitle(t.Context(), "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	source, _, err := db.GetSessionTitleSource(t.Context(), "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	return title, source
}

func TestPrologueTitlesTheSurfacesAPersonReads(t *testing.T) {
	for _, platform := range []string{"cli", "telegram", "desktop", "acp", ""} {
		if calls, _ := titleTurn(t, platform, []map[string]any{{"role": "user", "content": "Fix the login button"}}); len(*calls) != 1 {
			t.Fatalf("platform %q titler calls %d", platform, len(*calls))
		}
	}
}

func TestPrologueDoesNotTitleMachineDrivenRuns(t *testing.T) {
	for _, platform := range []string{"cron", "CRON", "subagent"} {
		if calls, _ := titleTurn(t, platform, []map[string]any{{"role": "user", "content": "Fix the login button"}}); len(*calls) != 0 {
			t.Fatalf("platform %q titler calls %d", platform, len(*calls))
		}
	}
}

func TestPrologueNamesASubagentRunAfterItsGoalWithoutAModelCall(t *testing.T) {
	calls, db := titleTurn(t, "subagent", []map[string]any{{"role": "user", "content": "Audit the billing module\n\nContext: ..."}})
	title, source := storedTitle(t, db)
	if len(*calls) != 0 || title != "Subagent: Audit the billing module" || source != "derived" {
		t.Fatalf("titler calls %d title %q source %q", len(*calls), title, source)
	}
}

func TestPrologueLeavesCronRunsUntitled(t *testing.T) {
	_, db := titleTurn(t, "cron", []map[string]any{{"role": "user", "content": "Run the job"}})
	if title, _ := storedTitle(t, db); title != "" {
		t.Fatalf("cron session titled %q", title)
	}
}

func TestPrologueForwardsTheSubmitTitlePreviewToTheTitler(t *testing.T) {
	calls, _ := titleTurn(t, "desktop", []map[string]any{{"role": "user", "content": strings.Repeat("x", 5000),
		"display_metadata": map[string]any{"title_preview": "Pasted 5000 chars"}}})
	if len(*calls) != 1 || (*calls)[0].options.TitlePreview == nil || *(*calls)[0].options.TitlePreview != "Pasted 5000 chars" {
		t.Fatalf("titler calls %+v", *calls)
	}
}

func detachedForkAgent(t *testing.T, persistDisabled bool) *AIAgent {
	a, _ := restoreAgent(t, nil, "system prompt")
	a.sessionID, a.model, a.platform, a.persistDisabled = "shared-session", "test/model", "cli", persistDisabled
	return a
}

func detachedPreLLMContext(a *AIAgent) string {
	messages := rowsToMessages([]map[string]any{{"role": "user", "content": "hello"}})
	return collectPreLLMCallContext(context.Background(), a, "task-1", "turn-1", "hello", messages, nil, nil)
}

func detachedOutputHooks(a *AIAgent) (string, bool) {
	messages := rowsToMessages([]map[string]any{{"role": "user", "content": "hello"}})
	response, transformed, _ := applyOutputHooks(context.Background(), a, "response", "cli", "task-1", "turn-1", "hello", messages)
	return response, transformed
}

func TestPersistDisabledForkSkipsSessionAndTurnLifecycleHooks(t *testing.T) {
	a := detachedForkAgent(t, true)
	var lifecycle []string
	patchPluginHooks(t, func(hook string, _ map[string]any) []any { lifecycle = append(lifecycle, hook); return nil })
	restoreNow(t, a, []map[string]any{})
	contextText := detachedPreLLMContext(a)
	var output []string
	patchPluginHooks(t, func(hook string, _ map[string]any) []any {
		output = append(output, hook)
		if hook == "transform_llm_output" {
			return []any{"transformed"}
		}
		return nil
	})
	response, transformed := detachedOutputHooks(a)
	if len(lifecycle) != 0 || contextText != "" || !reflect.DeepEqual(output, []string{"transform_llm_output"}) || response != "transformed" || !transformed {
		t.Fatalf("lifecycle %v context %q output %v response %q transformed %v", lifecycle, contextText, output, response, transformed)
	}
}

func TestPersistedAgentStillFiresSessionAndTurnLifecycleHooks(t *testing.T) {
	a := detachedForkAgent(t, false)
	var lifecycle []string
	patchPluginHooks(t, func(hook string, _ map[string]any) []any {
		lifecycle = append(lifecycle, hook)
		if hook == "pre_llm_call" {
			return []any{map[string]any{"context": "plugin context"}}
		}
		return nil
	})
	restoreNow(t, a, []map[string]any{})
	contextText := detachedPreLLMContext(a)
	var output []string
	patchPluginHooks(t, func(hook string, _ map[string]any) []any { output = append(output, hook); return nil })
	detachedOutputHooks(a)
	if !reflect.DeepEqual(lifecycle, []string{"on_session_start", "pre_llm_call"}) || contextText != "plugin context" ||
		!reflect.DeepEqual(output, []string{"transform_llm_output", "post_llm_call"}) {
		t.Fatalf("lifecycle %v context %q output %v", lifecycle, contextText, output)
	}
}

package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

func outputSchemaAddress() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"city": map[string]any{"type": "string"}, "zip": map[string]any{"type": "string"}},
		"required":   []any{"city"},
	}
}

// Runs the real single child path over a canned child the way the hermes _run helper does
func runSchemaChild(t *testing.T, schema any, responses ...string) (*pyjson.Object, *cannedConversation) {
	t.Helper()
	env := delegationTestEnv(t, 0, false)
	child, canned := cannedDelegateChild(t, env, responses...)
	child.delegateOutputSchema = schema
	parent := &AIAgent{sessionID: "parent-session", env: env}
	return runSingleChild(context.Background(), 0, "produce the address", child, parent, "", nil, nil), canned
}

func entryHas(e *pyjson.Object, key string) bool {
	_, ok := e.Get(key)
	return ok
}

func TestRunSingleChildSchemaValidationValidFirstTryNoRetry(t *testing.T) {
	entry, canned := runSchemaChild(t, outputSchemaAddress(), `{"city": "Berlin"}`)
	valid, _ := entry.Get("schema_valid")
	if entryText(entry, "status") != "completed" || valid != true || entryHas(entry, "schema_errors") || len(canned.calls) != 1 {
		t.Fatalf("entry %v calls %d", entry, len(canned.calls))
	}
}

func TestRunSingleChildSchemaValidationInvalidThenRetryThenValid(t *testing.T) {
	entry, canned := runSchemaChild(t, outputSchemaAddress(), "not json at all", `{"city": "Oslo"}`)
	valid, _ := entry.Get("schema_valid")
	retries, _ := entry.Get("schema_retries")
	if valid != true || retries != 1 || len(canned.calls) != 2 {
		t.Fatalf("entry %v calls %d", entry, len(canned.calls))
	}
	retry, _ := canned.calls[1].(string)
	if !strings.Contains(retry, "rejected") && !strings.Contains(retry, "JSON") {
		t.Fatalf("retry turn %q", retry)
	}
	summary, err := pyjson.Loads(entryText(entry, "summary"))
	if err != nil {
		t.Fatal(err)
	}
	if city, _ := summary.(*pyjson.Object).Get("city"); city != "Oslo" {
		t.Fatalf("summary %v", summary)
	}
}

func TestRunSingleChildSchemaValidationInvalidTwiceSurfacesErrorsAndStops(t *testing.T) {
	entry, canned := runSchemaChild(t, outputSchemaAddress(), "nope", "still nope")
	valid, _ := entry.Get("schema_valid")
	retries, _ := entry.Get("schema_retries")
	errs, _ := entry.Get("schema_errors")
	if valid != false || len(errs.([]any)) == 0 || retries != 1 || len(canned.calls) != 2 {
		t.Fatalf("entry %v calls %d", entry, len(canned.calls))
	}
}

func TestRunSingleChildSchemaValidationRetryExceptionDegradesToInvalid(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	child, canned := cannedDelegateChild(t, env, "nope")
	child.delegateOutputSchema = outputSchemaAddress()
	prev := conversationLoopRun
	conversationLoopRun = func(ctx context.Context, a *AIAgent, userMessage any, o RunConversationOptions, apiMode string) (map[string]any, error) {
		if a == child && len(canned.calls) > 0 {
			return nil, errors.New("child died on retry")
		}
		return prev(ctx, a, userMessage, o, apiMode)
	}
	t.Cleanup(func() { conversationLoopRun = prev })
	parent := &AIAgent{sessionID: "parent-session", env: env}
	entry := runSingleChild(context.Background(), 0, "produce the address", child, parent, "", nil, nil)
	valid, _ := entry.Get("schema_valid")
	errs, _ := entry.Get("schema_errors")
	if valid != false || errs == nil || len(errs.([]any)) == 0 {
		t.Fatalf("entry %v", entry)
	}
}

func TestRunSingleChildSchemaValidationNoSchemaKeepsLegacyResultShape(t *testing.T) {
	entry, canned := runSchemaChild(t, nil, `{"city": "Berlin"}`)
	if entryHas(entry, "schema_valid") || entryHas(entry, "schema_errors") || entryHas(entry, "schema_retries") || len(canned.calls) != 1 {
		t.Fatalf("entry %v calls %d", entry, len(canned.calls))
	}
}

func TestRunSingleChildSchemaValidationFailedChildSkipsValidation(t *testing.T) {
	entry, canned := runSchemaChild(t, outputSchemaAddress(), "")
	valid, _ := entry.Get("schema_valid")
	if entryText(entry, "status") != "failed" || len(canned.calls) != 1 || valid != false {
		t.Fatalf("entry %v calls %d", entry, len(canned.calls))
	}
}

func TestRunSingleChildSchemaValidationSchemaFailureReturnsRawTextWithSchemaValidFalse(t *testing.T) {
	prose := "Findings:\n```json\n{\"town\": \"Oslo\"}\n```\nlet me know."
	entry, canned := runSchemaChild(t, outputSchemaAddress(), prose, prose)
	valid, _ := entry.Get("schema_valid")
	retries, _ := entry.Get("schema_retries")
	errs, _ := entry.Get("schema_errors")
	if entryText(entry, "status") != "completed" || entryText(entry, "summary") != prose || valid != false {
		t.Fatalf("entry %v", entry)
	}
	if errs == nil || len(errs.([]any)) == 0 || retries != 1 || !strings.Contains(entryText(entry, "schema_note"), "UNVALIDATED") {
		t.Fatalf("entry %v", entry)
	}
	if entryHas(entry, "error") || len(canned.calls) != 2 {
		t.Fatalf("entry %v calls %d", entry, len(canned.calls))
	}
}

func TestRunSingleChildSchemaValidationProseWrappedArrayAnswerValidates(t *testing.T) {
	text := "Verdicts below.\n```json\n[{\"n\": 1}, {\"n\": 2}]\n```\n"
	entry, canned := runSchemaChild(t, map[string]any{"type": "array", "items": map[string]any{"type": "object"}}, text)
	valid, _ := entry.Get("schema_valid")
	if valid != true || len(canned.calls) != 1 {
		t.Fatalf("entry %v calls %d", entry, len(canned.calls))
	}
}

func TestRunSingleChildSchemaValidationRetryTurnRunsInDelegatedChildContext(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	t.Setenv("ICEBATH_KANBAN_TASK", "t_parent")
	var seen []bool
	prev := runChildConversationFn
	runChildConversationFn = func(ctx context.Context, child *AIAgent, _ any, _ RunConversationOptions) (map[string]any, error) {
		seen = append(seen, tools.IsDelegatedChildContext(ctx))
		return map[string]any{"final_response": `{"city": "Berlin"}`, "completed": true, "api_calls": 1, "messages": []any{}}, nil
	}
	t.Cleanup(func() { runChildConversationFn = prev })
	child := &AIAgent{sessionID: "sess-child", model: "test-model", env: env, delegateDepth: 1}
	child.delegateOutputSchema = outputSchemaAddress()
	validateChildOutputSchema(context.Background(), child, map[string]any{"final_response": "not json", "api_calls": 1}, 0, "child-0", nil)
	if len(seen) != 1 || !seen[0] {
		t.Fatalf("seen %v", seen)
	}
	if tools.IsDelegatedChildContext(context.Background()) {
		t.Fatal("delegated child context leaked past the retry turn")
	}
}

// Dispatch fixtures of test_delegate_output_schema.py TestDelegateTaskDispatch, credentials resolving to None
func dispatchSchemaParent(t *testing.T) (*tools.Env, *AIAgent) {
	t.Helper()
	env := delegationTestEnv(t, 0, false)
	prev := resolveDelegationCredentialsFn
	resolveDelegationCredentialsFn = func(context.Context, map[string]any, *AIAgent) (map[string]any, error) {
		return map[string]any{"provider": nil, "model": nil, "base_url": nil, "api_key": nil, "api_mode": nil}, nil
	}
	t.Cleanup(func() { resolveDelegationCredentialsFn = prev })
	return env, &AIAgent{sessionID: "parent-session", env: env}
}

func dispatchPayload(t *testing.T, out string, err error) *pyjson.Object {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	payload, perr := pyjson.Loads(out)
	if perr != nil {
		t.Fatalf("payload %q %v", out, perr)
	}
	return payload.(*pyjson.Object)
}

func TestNonDictOutputSchemaRejected(t *testing.T) {
	_, parent := dispatchSchemaParent(t)
	out, err := delegateTask(context.Background(), parent, map[string]any{"tasks": []any{
		map[string]any{"goal": "Summarize the release notes for module A", "output_schema": "not-a-dict"},
		map[string]any{"goal": "Summarize the release notes for module B"},
	}})
	payload := dispatchPayload(t, out, err)
	if msg := entryText(payload, "error"); msg == "" || !strings.Contains(msg, "output_schema") {
		t.Fatalf("payload %v", payload)
	}
}

func TestInvalidJsonSchemaRejectedAtDispatch(t *testing.T) {
	_, parent := dispatchSchemaParent(t)
	out, err := delegateTask(context.Background(), parent, map[string]any{"tasks": []any{
		map[string]any{"goal": "Summarize the release notes for module A", "output_schema": map[string]any{"type": 42}},
		map[string]any{"goal": "Summarize the release notes for module B"},
	}})
	payload := dispatchPayload(t, out, err)
	if msg := entryText(payload, "error"); msg == "" || !strings.Contains(msg, "output_schema") {
		t.Fatalf("payload %v", payload)
	}
}

func TestChildReceivesContractAndSchemaAttr(t *testing.T) {
	env, parent := dispatchSchemaParent(t)
	var captured childBuildSpec
	prevBuild := buildChildPreservingParentToolsFn
	buildChildPreservingParentToolsFn = func(_ context.Context, spec childBuildSpec) (*AIAgent, error) {
		captured = spec
		child, _ := cannedDelegateChild(t, env, `{"city": "Rio"}`)
		return child, nil
	}
	t.Cleanup(func() { buildChildPreservingParentToolsFn = prevBuild })
	out, err := delegateTask(context.Background(), parent, map[string]any{
		"goal": "produce the address", "context": "base context", "output_schema": outputSchemaAddress(),
	})
	payload := dispatchPayload(t, out, err)
	contextText, _ := captured.Context.(string)
	if !strings.Contains(contextText, "OUTPUT CONTRACT") {
		t.Fatalf("child context %q", contextText)
	}
	rawResults, _ := payload.Get("results")
	results, _ := rawResults.([]any)
	if len(results) == 0 {
		t.Fatalf("payload %v", payload)
	}
	if valid, _ := results[0].(*pyjson.Object).Get("schema_valid"); valid != true {
		t.Fatalf("payload %v", payload)
	}
}

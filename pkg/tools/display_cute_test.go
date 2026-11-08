package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Fresh home with the English catalog and an unlimited preview cap
func cuteTestHome(t *testing.T) context.Context {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	i18n.ResetLanguageCache()
	config.ResetCache()
	SetToolPreviewMaxLen(0)
	t.Cleanup(func() { SetToolPreviewMaxLen(0); i18n.ResetLanguageCache(); config.ResetCache() })
	return context.Background()
}

// Ordered dict built from alternating keys and values
func displayObj(kv ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

// json.dumps text of an ordered dict built from alternating keys and values
func displayDumps(t *testing.T, kv ...any) string {
	t.Helper()
	out, err := pyjson.Dumps(displayObj(kv...))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func cuteLine(ctx context.Context, name string, args map[string]any, duration float64, result any) string {
	return GetCuteToolMessage(ctx, nil, name, args, duration, result)
}

func TestTrimErrorShortMessageUnchanged(t *testing.T) {
	ctx := cuteTestHome(t)
	if got := trimError(ctx, "nope"); got != "nope" {
		t.Fatalf("got %q", got)
	}
}

func TestTrimErrorLongMessageTruncatedToCap(t *testing.T) {
	ctx := cuteTestHome(t)
	trimmed := trimError(ctx, strings.Repeat("x", 200))
	if pystr.Len(trimmed) > errorSuffixMaxLen || !strings.HasSuffix(trimmed, "...") {
		t.Fatalf("got %q", trimmed)
	}
}

func TestTrimErrorFileNotFoundPathCollapsedToFilename(t *testing.T) {
	ctx := cuteTestHome(t)
	if got := trimError(ctx, "File not found: /home/teknium/.icebath/icebath-agent/very/deep/path/foo.py"); got != "File not found: foo.py" {
		t.Fatalf("got %q", got)
	}
}

func TestDetectToolFailureTerminalSuccessReturnsNoSuffix(t *testing.T) {
	ctx := cuteTestHome(t)
	if failed, suffix := DetectToolFailure(ctx, "terminal", displayDumps(t, "output", "ok\n", "exit_code", 0)); failed || suffix != "" {
		t.Fatalf("got %v %q", failed, suffix)
	}
}

func TestDetectToolFailureTerminalNonzeroExitWithErrorShowsMessage(t *testing.T) {
	ctx := cuteTestHome(t)
	result := displayDumps(t, "output", "", "exit_code", 127, "error", "ls: cannot access 'foo': No such file or directory")
	failed, suffix := DetectToolFailure(ctx, "terminal", result)
	if !failed || !strings.Contains(suffix, "cannot access") || !strings.HasPrefix(suffix, " [") || !strings.HasSuffix(suffix, "]") {
		t.Fatalf("got %v %q", failed, suffix)
	}
}

func TestDetectToolFailureTerminalDegradedBackendShowsFullReasonAndRetryHint(t *testing.T) {
	ctx := cuteTestHome(t)
	result := displayDumps(t, "output", "", "exit_code", -1, "status", "degraded",
		"reason", "Docker daemon is not reachable at unix:///var/run/docker.sock",
		"retry_hint", "Start Docker, or run `icebath setup terminal` to switch to Local, then retry",
		"error", "Terminal backend degraded: Docker daemon is not reachable at unix:///var/run/docker.sock")
	failed, suffix := DetectToolFailure(ctx, "terminal", result)
	if !failed || !strings.Contains(suffix, "unix:///var/run/docker.sock") || !strings.Contains(suffix, "icebath setup terminal") ||
		strings.Contains(suffix, "Terminal backend degraded:") {
		t.Fatalf("got %v %q", failed, suffix)
	}
}

func TestDetectToolFailureTerminalNonzeroDictResultIsAFailure(t *testing.T) {
	ctx := cuteTestHome(t)
	if failed, suffix := DetectToolFailure(ctx, "terminal", map[string]any{"exit_code": 2}); !failed || suffix != " [exit 2]" {
		t.Fatalf("got %v %q", failed, suffix)
	}
}

func TestDetectToolFailureTerminalMultimodalEnvelopeDictIsStillASuccess(t *testing.T) {
	ctx := cuteTestHome(t)
	envelope := map[string]any{"_multimodal": true, "content": []any{map[string]any{"type": "text", "text": "screenshot taken"}}}
	if failed, suffix := DetectToolFailure(ctx, "browser_exec", envelope); failed || suffix != "" {
		t.Fatalf("got %v %q", failed, suffix)
	}
}

func TestDetectToolFailureTerminalDegradedBackendWithoutHintShowsReasonAlone(t *testing.T) {
	ctx := cuteTestHome(t)
	result := displayDumps(t, "output", "", "exit_code", -1, "status", "degraded",
		"reason", "SSH connection to bob@host timed out", "retry_hint", "",
		"error", "Terminal backend degraded: SSH connection to bob@host timed out")
	if _, suffix := DetectToolFailure(ctx, "terminal", result); suffix != " [SSH connection to bob@host timed out]" {
		t.Fatalf("got %q", suffix)
	}
}

func TestDetectToolFailureMemoryMemoryFullReturnsFullSuffix(t *testing.T) {
	ctx := cuteTestHome(t)
	result := displayDumps(t, "success", false, "error", "would exceed the limit")
	if failed, suffix := DetectToolFailure(ctx, "memory", result); !failed || suffix != " [full]" {
		t.Fatalf("got %v %q", failed, suffix)
	}
}

func TestDetectToolFailureMemoryMemoryOtherErrorReturnsSpecificMessage(t *testing.T) {
	ctx := cuteTestHome(t)
	result := displayDumps(t, "success", false, "error", "invalid action: zap")
	if failed, suffix := DetectToolFailure(ctx, "memory", result); !failed || !strings.Contains(suffix, "invalid action") {
		t.Fatalf("got %v %q", failed, suffix)
	}
}

func TestDetectToolFailureStructuredReadFileErrorSurfaced(t *testing.T) {
	ctx := cuteTestHome(t)
	result := displayDumps(t, "path", "/nope/missing.py", "success", false, "error", "File not found: /nope/missing.py")
	if failed, suffix := DetectToolFailure(ctx, "read_file", result); !failed || suffix != " [File not found: missing.py]" {
		t.Fatalf("got %v %q", failed, suffix)
	}
}

func TestDetectToolFailureStructuredSuccessfulResultNotFlagged(t *testing.T) {
	ctx := cuteTestHome(t)
	if failed, suffix := DetectToolFailure(ctx, "web_search", displayDumps(t, "success", true, "data", "hello")); failed || suffix != "" {
		t.Fatalf("got %v %q", failed, suffix)
	}
}

func TestDetectToolFailureStructuredEmptyErrorAndFailedValuesAreNotFailures(t *testing.T) {
	ctx := cuteTestHome(t)
	indented, err := pyjson.Dumps(displayObj("results", []any{displayObj("url", "https://example.com/", "content", "hi", "error", nil)}), pyjson.Indent(2))
	if err != nil {
		t.Fatal(err)
	}
	ok := []string{
		indented,
		displayDumps(t, "done", 3, "failed", 0),
		displayDumps(t, "items", []any{}, "errors", []any{}, "error", ""),
		displayDumps(t, "results", []any{displayObj("url", "https://example.com/", "content", strings.Repeat("x", 428), "error", nil)}),
	}
	for _, result := range ok {
		if failed, suffix := DetectToolFailure(ctx, "web_extract", result); failed || suffix != "" {
			t.Fatalf("%s detected %v %q", result, failed, suffix)
		}
		r := result
		if failed, suffix := ClassifyToolFailure("web_extract", &r); failed || suffix != "" {
			t.Fatalf("%s classified %v %q", result, failed, suffix)
		}
	}
}

func TestDetectToolFailureStructuredRealErrorAndFailedMarkersStillFlag(t *testing.T) {
	ctx := cuteTestHome(t)
	bad := []string{
		displayDumps(t, "results", []any{displayObj("url", "https://x.test/", "content", "", "error", "HTTP 403")}),
		displayDumps(t, "status", "failed"),
		"Error: connection refused",
	}
	for _, result := range bad {
		if failed, _ := DetectToolFailure(ctx, "web_extract", result); !failed {
			t.Fatalf("%s not detected", result)
		}
		r := result
		if failed, _ := ClassifyToolFailure("web_extract", &r); !failed {
			t.Fatalf("%s not classified", result)
		}
	}
}

func TestDetectToolFailureStructuredJsonObjectIsJudgedByTopLevelShape(t *testing.T) {
	ctx := cuteTestHome(t)
	ok := []string{
		displayDumps(t, "success", true, "method", "Runtime.evaluate",
			"result", displayObj("result", displayObj("type", "object", "value", displayObj("error", []any{"Vul een geldig telefoonnummer in."})))),
		displayDumps(t, "success", true, "rows", []any{displayObj("ok", false, "error", "matched 0 elements")}),
		displayDumps(t, "results", []any{displayObj("task_index", 0, "error", "interrupted"), displayObj("task_index", 1, "summary", "done", "error", nil)}),
		displayDumps(t, "status", "timeout", "command", "sleep 60", "output", "", "process_running", true),
	}
	bad := []string{
		displayDumps(t, "error", "CDP call timed out after 30.0s", "method", "Runtime.evaluate"),
		displayDumps(t, "results", []any{displayObj("task_index", 0, "status", "interrupted", "error", "child did not finish"),
			displayObj("task_index", 1, "status", "interrupted", "error", "child did not finish")}),
		displayDumps(t, "status", "error", "stdout", "", "traceback", "boom"),
		"Error executing tool 'web_search': boom",
	}
	for _, result := range ok {
		if failed, suffix := DetectToolFailure(ctx, "process", result); failed || suffix != "" {
			t.Fatalf("%s detected %v %q", result, failed, suffix)
		}
		r := result
		if failed, suffix := ClassifyToolFailure("process", &r); failed || suffix != "" {
			t.Fatalf("%s classified %v %q", result, failed, suffix)
		}
	}
	for _, result := range bad {
		if failed, _ := DetectToolFailure(ctx, "delegate_task", result); !failed {
			t.Fatalf("%s not detected", result)
		}
		r := result
		if failed, _ := ClassifyToolFailure("delegate_task", &r); !failed {
			t.Fatalf("%s not classified", result)
		}
	}
}

func TestMemoryFullSuffix(t *testing.T) {
	ctx := cuteTestHome(t)
	fail := displayDumps(t, "success", false, "error", "would exceed the limit")
	if line := cuteLine(ctx, "memory", map[string]any{"action": "add", "target": "memory", "content": "x"}, 0.05, fail); !strings.Contains(line, "[full]") {
		t.Fatalf("line %q", line)
	}
}

func TestGetCuteToolMessageFailureSuffixSuccessHasNoSuffix(t *testing.T) {
	ctx := cuteTestHome(t)
	line := cuteLine(ctx, "web_search", map[string]any{"query": "hi"}, 0.2, displayDumps(t, "success", true, "data", "hi"))
	_, after, found := strings.Cut(line, "0.2s")
	if !found || strings.Contains(after, "[") {
		t.Fatalf("line %q", line)
	}
}

// Fake todo_list tool return value with the given total and completed counts
func planResult(t *testing.T, total, completed int) string {
	return displayDumps(t, "todos", []any{}, "summary", displayObj("total", total, "pending", total-completed, "in_progress", 0,
		"completed", completed, "cancelled", 0))
}

func TestCreateWithResultZeroDone(t *testing.T) {
	ctx := cuteTestHome(t)
	args := map[string]any{"todos": []any{
		map[string]any{"id": "a", "content": "x", "status": "pending"},
		map[string]any{"id": "b", "content": "y", "status": "pending"},
	}}
	if msg := cuteLine(ctx, "todo_list", args, 0.3, planResult(t, 2, 0)); !strings.Contains(msg, "2 task(s)") || strings.Contains(msg, "/") {
		t.Fatalf("msg %q", msg)
	}
}

func TestUpdateHalfway(t *testing.T) {
	ctx := cuteTestHome(t)
	args := map[string]any{"todos": []any{map[string]any{"id": "b", "status": "in_progress"}}, "merge": true}
	if msg := cuteLine(ctx, "todo_list", args, 0.7, planResult(t, 4, 2)); !strings.Contains(msg, "2/4") || !strings.Contains(msg, "✓") {
		t.Fatalf("msg %q", msg)
	}
}

func TestUpdateTotalNotInSummary(t *testing.T) {
	ctx := cuteTestHome(t)
	args := map[string]any{"todos": []any{map[string]any{"id": "a", "status": "completed"}}, "merge": true}
	msg := cuteLine(ctx, "todo_list", args, 0.3, displayDumps(t, "summary", displayObj("completed", 2)))
	if !strings.Contains(msg, "update 1 task(s)") || strings.Contains(msg, "✓") {
		t.Fatalf("msg %q", msg)
	}
}

func TestWebExtractDisplayWebExtractWithDictHrefField(t *testing.T) {
	ctx := cuteTestHome(t)
	args := map[string]any{"urls": []any{map[string]any{"href": "http://test.org/page", "title": "Test", "snippet": "..."}}}
	if msg := cuteLine(ctx, "web_extract", args, 0.3, nil); !strings.Contains(msg, "test.org") {
		t.Fatalf("msg %q", msg)
	}
}

func TestWebExtractDisplayWebExtractWithMixedTypes(t *testing.T) {
	ctx := cuteTestHome(t)
	args := map[string]any{"urls": []any{"https://direct.com/page", map[string]any{"url": "https://dict.com/page", "title": "Dict URL"}}}
	if msg := cuteLine(ctx, "web_extract", args, 0.4, nil); !strings.Contains(msg, "direct.com") {
		t.Fatalf("msg %q", msg)
	}
}

func TestCuteToolMessageFallsBackWhenRendererRaises(t *testing.T) {
	ctx := cuteTestHome(t)
	prev := renderCuteToolMessage
	renderCuteToolMessage = func(context.Context, *Env, string, map[string]any, float64, any) string {
		panic(errors.New("cosmetic failure"))
	}
	t.Cleanup(func() { renderCuteToolMessage = prev })
	if result := cuteLine(ctx, "web_extract", map[string]any{"urls": []any{}}, 0.25, nil); result == "" {
		t.Fatal("empty fallback line")
	}
}

func TestBuildToolPreviewMemoryPatchPreviewShowsTheSpanBeingEdited(t *testing.T) {
	ctx := cuteTestHome(t)
	args := map[string]any{"action": "patch", "target": "memory", "old_text": "port 8080", "content": "port 9090"}
	if got := BuildToolPreview(nil, "memory", args, nil); got != `~memory: "port 8080"` {
		t.Fatalf("preview %q", got)
	}
	if line := cuteLine(ctx, "memory", args, 0.1, nil); !strings.Contains(line, `~memory: "port 8080"`) {
		t.Fatalf("line %q", line)
	}
}

func TestBuildToolPreviewMemoryOperationsBatchPreviewsItsFirstOpAndTheRestCount(t *testing.T) {
	ctx := cuteTestHome(t)
	one := map[string]any{"target": "user", "operations": []any{map[string]any{"action": "add", "content": "Name: Alice"}}}
	if got := BuildToolPreview(nil, "memory", one, nil); got != `+user: "Name: Alice"` {
		t.Fatalf("preview %q", got)
	}
	if line := cuteLine(ctx, "memory", one, 0.1, nil); !strings.Contains(line, `+user: "Name: Alice"`) {
		t.Fatalf("line %q", line)
	}
	three := map[string]any{"target": "memory", "operations": []any{
		map[string]any{"action": "remove", "old_text": "port 8080"}, map[string]any{"action": "add", "content": "a"},
		map[string]any{"action": "patch", "old_text": "b", "content": "c"}}}
	if got := BuildToolPreview(nil, "memory", three, nil); got != `-memory: "port 8080" (2 more)` {
		t.Fatalf("preview %q", got)
	}
	if line := cuteLine(ctx, "memory", three, 0.1, nil); !strings.Contains(line, `-memory: "port 8080" (2 more)`) {
		t.Fatalf("line %q", line)
	}
}

func TestCuteToolMessagePreviewLengthSearchFilesPreviewUsesPositiveConfiguredLimitNotDefault(t *testing.T) {
	ctx := cuteTestHome(t)
	SetToolPreviewMaxLen(80)
	pattern := "function.formatToolCall.context.preview.compactPreview.maxLength.truncate"
	if line := cuteLine(ctx, "search_files", map[string]any{"pattern": pattern}, 0.1, nil); !strings.Contains(line, pattern) || strings.Contains(line, "...") {
		t.Fatalf("line %q", line)
	}
}

func TestCuteToolMessagePreviewLengthBrowserTypeCuteMessageRedactsApiKey(t *testing.T) {
	ctx := cuteTestHome(t)
	secret := "sk-proj-ABCD1234567890EFGH"
	line := cuteLine(ctx, "browser_type", map[string]any{"ref": "@password", "text": secret}, 0.1, `{"success": true, "typed": "sk-pro...EFGH"}`)
	if strings.Contains(line, secret) || !strings.Contains(line, "sk-pro") {
		t.Fatalf("line %q", line)
	}
}

func TestCuteToolMessagePreviewLengthBrowserTypeCuteMessageKeepsNormalText(t *testing.T) {
	ctx := cuteTestHome(t)
	text := "hello world"
	if line := cuteLine(ctx, "browser_type", map[string]any{"ref": "@search", "text": text}, 0.1, `{"success": true, "typed": "hello world"}`); !strings.Contains(line, text) {
		t.Fatalf("line %q", line)
	}
}

func TestStepLabelsProgressLineShowsLabel(t *testing.T) {
	ctx := cuteTestHome(t)
	code := "# Searching Amazon for paper towels\nnew_tab('https://amazon.com')\nwait_for_load()"
	if line := cuteLine(ctx, "browser_exec", map[string]any{"code": code}, 1.2, nil); !strings.Contains(line, "Searching Amazon for paper towels") || strings.Contains(line, "new_tab") {
		t.Fatalf("line %q", line)
	}
}

func TestCliCompletionLinePrefersUserSummaryOverBlockedText(t *testing.T) {
	ctx := cuteTestHome(t)
	result := displayDumps(t, "output", "", "exit_code", -1, "status", "blocked",
		"error", "BLOCKED: User denied this command. The user has NOT consented ...",
		"user_summary", "You denied this command — it did not run.")
	failed, suffix := DetectToolFailure(ctx, "terminal", result)
	if !failed || !strings.Contains(suffix, "You denied this command") || strings.Contains(suffix, "BLOCKED") {
		t.Fatalf("got %v %q", failed, suffix)
	}
}

func TestHarnessRefusalsAreNotToolFailuresOnEitherClassifier(t *testing.T) {
	ctx := cuteTestHome(t)
	env, ws := newFileToolTestEnv(t)
	target := filepath.Join(ws, "responses.ts")
	if err := os.WriteFile(target, []byte(strings.Repeat("export const x = 1;\n", 30)), 0o644); err != nil {
		t.Fatal(err)
	}
	dedupTask, readTask, searchTask := t.Name()+"-dedup", t.Name()+"-read", t.Name()+"-search"
	t.Cleanup(func() {
		readTrackerLock.Lock()
		defer readTrackerLock.Unlock()
		delete(readTracker, dedupTask)
		delete(readTracker, readTask)
		delete(readTracker, searchTask)
	})
	readTrackerLock.Lock()
	task := taskData(dedupTask)
	readTrackerLock.Unlock()
	var dedupBlock, consecutiveBlock, searchBlock string
	for range 3 {
		dedupBlock = dedupStubOrBlock(task, dedupKey{path: target, offset: 1, limit: 999}, target)
	}
	for range 4 {
		consecutiveBlock = readFileTool(context.Background(), env, target, 1, 5, readTask)
	}
	for range 4 {
		searchBlock = searchTool(context.Background(), env, ftrSearchArgs("const x", ws, 0, 50), searchTask)
	}
	for _, refusal := range []string{dedupBlock, consecutiveBlock, searchBlock} {
		if msg, _ := ftDecode(t, refusal)["error"].(string); !strings.HasPrefix(msg, "BLOCKED") {
			t.Fatalf("not a refusal %s", refusal)
		}
		r := refusal
		if failed, suffix := ClassifyToolFailure("read_file", &r); failed || suffix != "" {
			t.Fatalf("%s classified %v %q", refusal, failed, suffix)
		}
		if failed, suffix := DetectToolFailure(ctx, "read_file", refusal); failed || suffix != "" {
			t.Fatalf("%s detected %v %q", refusal, failed, suffix)
		}
	}
}

func TestARealToolErrorIsStillAFailure(t *testing.T) {
	ctx := cuteTestHome(t)
	real := `{"error": "ENOENT: no such file"}`
	if failed, _ := ClassifyToolFailure("read_file", &real); !failed {
		t.Fatal("real error not classified")
	}
	if failed, _ := DetectToolFailure(ctx, "read_file", real); !failed {
		t.Fatal("real error not detected")
	}
	prose := `{"error": "x", "guardrail_refusal": "yes"}`
	if failed, _ := ClassifyToolFailure("read_file", &prose); !failed {
		t.Fatal("truthy prose marker honoured")
	}
}

func TestDisplayIntegrationToolMessageUsesSkinPrefix(t *testing.T) {
	ctx := cuteTestHome(t)
	prev := config.GetActiveSkinName(ctx)
	config.SetActiveSkin(ctx, "ares")
	t.Cleanup(func() { config.SetActiveSkin(ctx, prev) })
	msg := cuteLine(ctx, "terminal", map[string]any{"command": "ls"}, 0.5, nil)
	if !strings.HasPrefix(msg, "╎") || strings.Contains(msg, "┊") {
		t.Fatalf("msg %q", msg)
	}
}

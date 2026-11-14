package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	liveTestBearer = "sk-ant-api03-RRRRRRRRRRRRRRRRRRRRRRRR"
	liveTestEnvKey = "sk-proj-LLLLLLLLLLLLLLLLLLLLLLLL"
	liveTestAWS    = "wJalrXUtnFEMIK7MDENGbPxRfiCY"
)

func liveWriter(t *testing.T, delegationID, goal string, taskContext any) *liveTranscriptWriter {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	w := newLiveTranscriptWriter(context.Background(), delegationID, 0, goal, taskContext, "")
	if w.Path() == "" {
		t.Fatal("writer has no path")
	}
	return w
}

func liveBody(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func liveManifest(t *testing.T, path string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(liveBody(t, path)), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func mustContain(t *testing.T, body string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Fatalf("missing %q in %q", w, body)
		}
	}
}

func mustNotContain(t *testing.T, body string, bans ...string) {
	t.Helper()
	for _, b := range bans {
		if strings.Contains(body, b) {
			t.Fatalf("leaked %q in %q", b, body)
		}
	}
}

func TestWriterPrecreatesFileWithHeader(t *testing.T) {
	w := liveWriter(t, "deleg_test1", "do the thing", "some ctx")
	mustContain(t, liveBody(t, w.Path()), "Icebath subagent live transcript", "delegation: deleg_test1", "goal: do the thing",
		"kickoff", "some ctx")
	if filepath.Base(w.Path()) != "task-0.log" || filepath.Base(filepath.Dir(w.Path())) != "deleg_test1" ||
		filepath.Dir(filepath.Dir(w.Path())) != liveTranscriptRoot(context.Background(), "") {
		t.Fatalf("path %q", w.Path())
	}
}

func TestStreamDeltasBufferAndFlushAsOneLine(t *testing.T) {
	w := liveWriter(t, "deleg_stream", "g", nil)
	w.addStreamDelta("Hello ")
	w.addStreamDelta("world, ")
	w.addStreamDelta("streaming.")
	mustNotContain(t, liveBody(t, w.Path()), "Hello world")
	w.flushStream()
	mustContain(t, liveBody(t, w.Path()), "Hello world, streaming.")
	w.addStreamDelta("more text")
	w.toolStart("read_file", "foo.py")
	body := liveBody(t, w.Path())
	if strings.Index(body, "more text") >= strings.Index(body, "-> read_file") {
		t.Fatalf("stream not flushed first %q", body)
	}
}

func TestObserveMapsChildCallbackEventsToLines(t *testing.T) {
	w := liveWriter(t, "deleg_observe", "g", nil)
	w.observe("subagent.start", nil, "kick off the goal", nil, nil)
	w.observe("_thinking", "first line of thinking", nil, nil, nil)
	w.observe("reasoning.available", "_thinking", "deep reasoning text", nil, nil)
	w.observe("tool.started", "terminal", "ls /tmp", map[string]any{"command": "ls /tmp"}, nil)
	w.observe("tool.completed", "terminal", nil, nil, map[string]any{"duration": 0.5, "is_error": false, "result": "ok output"})
	w.observe("subagent.text", nil, "final reply ", nil, nil)
	w.observe("subagent.text", nil, "streamed in parts", nil, nil)
	w.observe("subagent.complete", nil, "short", nil, map[string]any{"status": "completed", "duration_seconds": 3.2, "summary": "did the thing"})
	mustContain(t, liveBody(t, w.Path()), "kick off the goal", "first line of thinking", "deep reasoning text", "-> terminal(ls /tmp)",
		"terminal ok 0.5s: ok output", "final reply streamed in parts", "status=completed", "did the thing")
}

func liveEntry(kv ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

func TestFinalizeRecordsBudgetExhaustionAndErrors(t *testing.T) {
	w := liveWriter(t, "deleg_final", "g", nil)
	if err := w.Finalize(liveEntry("status", "failed", "exit_reason", "max_iterations", "error", "Subagent did not produce a response.")); err != nil {
		t.Fatal(err)
	}
	mustContain(t, liveBody(t, w.Path()), "end status=failed", "exit_reason=max_iterations", "iteration budget exhausted",
		"did not produce a response")
}

func TestWrapProgressCallbackTeesAndPreservesInner(t *testing.T) {
	w := liveWriter(t, "deleg_wrap", "g", nil)
	var seen [][2]string
	var innerFlushed []bool
	inner := &flushingProgressCallback{
		call: toolProgressFn(func(event, name, preview string, args map[string]any, extra map[string]any) {
			seen = append(seen, [2]string{event, name})
		}),
		flush: func() { innerFlushed = append(innerFlushed, true) },
	}
	cb := wrapProgressCallback(inner, w)
	call, ok := progressFn(cb).(toolProgressFn)
	if !ok {
		t.Fatalf("wrapped type %T", progressFn(cb))
	}
	call("tool.started", "terminal", "echo hi", nil, nil)
	call("_thinking", "pondering", "", nil, nil)
	if !slices.Equal(seen, [][2]string{{"tool.started", "terminal"}, {"_thinking", "pondering"}}) {
		t.Fatalf("seen %v", seen)
	}
	mustContain(t, liveBody(t, w.Path()), "-> terminal(echo hi)", "pondering")
	flushProgressCallback(cb)
	if len(innerFlushed) != 1 {
		t.Fatalf("inner flushed %v", innerFlushed)
	}
}

// Writer whose observe fails the way a broken disk would
type failingLiveWriter struct{ *liveTranscriptWriter }

func (failingLiveWriter) observe(any, any, any, any, map[string]any) { panic("disk on fire") }

func TestWrapProgressCallbackWriterFailureDoesNotBlockInner(t *testing.T) {
	w := failingLiveWriter{liveWriter(t, "deleg_wfail", "g", nil)}
	seen := 0
	cb := wrapProgressCallback(toolProgressFn(func(string, string, string, map[string]any, map[string]any) { seen++ }), w)
	progressFn(cb).(toolProgressFn)("tool.started", "terminal", "x", nil, nil)
	if seen != 1 {
		t.Fatalf("inner calls %d", seen)
	}
}

func TestToolArgsAreRedactedBeforeHittingDisk(t *testing.T) {
	w := liveWriter(t, "deleg_redact_args", "g", nil)
	w.observe("tool.started", "terminal", `curl -H "Authorization: Bearer `+liveTestBearer+`" https://api.internal`, nil, nil)
	body := liveBody(t, w.Path())
	mustNotContain(t, body, liveTestBearer)
	mustContain(t, body, "terminal")
}

func TestToolResultsAreRedactedBeforeHittingDisk(t *testing.T) {
	w := liveWriter(t, "deleg_redact_result", "g", nil)
	w.observe("tool.completed", "terminal", nil, nil, map[string]any{
		"result": "OPENAI_API_KEY=" + liveTestEnvKey + "\nAWS_SECRET_ACCESS_KEY=" + liveTestAWS, "duration": 0.4,
	})
	body := liveBody(t, w.Path())
	mustNotContain(t, body, liveTestEnvKey, liveTestAWS)
	mustContain(t, body, "OPENAI_API_KEY")
}

func TestStreamedAssistantTextIsRedacted(t *testing.T) {
	w := liveWriter(t, "deleg_redact_stream", "g", nil)
	w.observe("subagent.text", nil, "the key is "+liveTestEnvKey, nil, nil)
	w.flushStream()
	mustNotContain(t, liveBody(t, w.Path()), liveTestEnvKey)
}

func TestGoalHeaderIsRedacted(t *testing.T) {
	w := liveWriter(t, "deleg_redact_goal", "deploy using "+liveTestBearer, nil)
	body := liveBody(t, w.Path())
	mustNotContain(t, body, liveTestBearer)
	mustContain(t, body, "deploy using")
}

func TestManifestGoalIsRedacted(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	id, _, _ := createLiveTranscripts(ctx, []map[string]any{{"goal": "deploy using " + liveTestBearer}}, nil, nil, nil, "")
	m := liveManifest(t, filepath.Join(liveTranscriptRoot(ctx, ""), id, "manifest.json"))
	goal := m["tasks"].([]any)[0].(map[string]any)["goal"].(string)
	mustNotContain(t, goal, liveTestBearer)
	mustContain(t, goal, "deploy using")
}

func TestExplicitHomePinsTranscriptsAcrossRawThreadBoundary(t *testing.T) {
	tmp := t.TempDir()
	processHome, sessionHome := filepath.Join(tmp, "process-default"), filepath.Join(tmp, "session-profile")
	for _, d := range []string{processHome, sessionHome} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", processHome)
	sessionCtx := home.WithOverride(context.Background(), sessionHome)
	if home.Dir(sessionCtx) != sessionHome {
		t.Fatal("override not applied")
	}
	done := make(chan []string)
	go func() {
		_, _, paths := createLiveTranscripts(context.Background(), []map[string]any{{"goal": "profile-pinned repro"}}, nil, nil, nil, sessionHome)
		done <- paths
	}()
	paths := <-done
	expectedRoot := filepath.Join(sessionHome, "cache", "delegation", "live")
	if len(paths) == 0 {
		t.Fatal("no transcripts created")
	}
	for _, p := range paths {
		if !strings.HasPrefix(p, expectedRoot+string(filepath.Separator)) {
			t.Fatalf("path %q outside %q", p, expectedRoot)
		}
	}
	if m := liveManifest(t, filepath.Join(filepath.Dir(paths[0]), "manifest.json")); m["task_count"] != 1.0 {
		t.Fatalf("manifest %v", m)
	}
}

func TestManifestUpdateUsesSameExplicitHome(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "other-profile"))
	own := filepath.Join(tmp, "own-profile")
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatal(err)
	}
	id, _, paths := createLiveTranscripts(context.Background(), []map[string]any{{"goal": "pin me"}}, nil, nil, nil, own)
	updateManifestStatuses(id, []*pyjson.Object{liveEntry("task_index", 0, "status", "completed")}, own, true)
	m := liveManifest(t, filepath.Join(filepath.Dir(paths[0]), "manifest.json"))
	if m["tasks"].([]any)[0].(map[string]any)["status"] != "completed" {
		t.Fatalf("manifest %v", m)
	}
}

func TestManifestRecordsInheritedChildModel(t *testing.T) {
	tmp := t.TempDir()
	id, _, paths := createLiveTranscripts(context.Background(), []map[string]any{{"goal": "a"}, {"goal": "b"}}, nil, nil, nil, tmp)
	updateManifestStatuses(id, []*pyjson.Object{
		liveEntry("task_index", 0, "status", "completed", "model", "parent/model"),
		liveEntry("task_index", 1, "status", "completed", "model", "parent/model"),
	}, tmp, true)
	m := liveManifest(t, filepath.Join(filepath.Dir(paths[0]), "manifest.json"))
	if m["model"] != "parent/model" {
		t.Fatalf("model %v", m["model"])
	}
	for _, task := range m["tasks"].([]any) {
		if task.(map[string]any)["model"] != "parent/model" {
			t.Fatalf("task %v", task)
		}
	}
}

func pinnedTrace(recs []loggerRecord) bool {
	for _, r := range recs {
		if strings.Contains(r.msg, "home pinning skipped") {
			return true
		}
	}
	return false
}

func TestParentWithoutSessionDBWarnsAndSkipsPin(t *testing.T) {
	recs := captureLogger(t, "tools.delegate_tool", slog.LevelWarn)
	if got := parentLiveHome(&AIAgent{}); got != "" {
		t.Fatalf("home %q", got)
	}
	if !pinnedTrace(recs()) {
		t.Fatal("no warning trace")
	}
}

func openPinDB(t *testing.T, dir string) *state.SessionDB {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := state.NewSessionDB(context.Background(), filepath.Join(dir, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestParentSessionDBPinsHome(t *testing.T) {
	prof := filepath.Join(t.TempDir(), "prof")
	if got := parentLiveHome(&AIAgent{sessionDB: openPinDB(t, prof)}); got != prof {
		t.Fatalf("home %q", got)
	}
}

func TestMockParentDBPathIsNotPinned(t *testing.T) {
	recs := captureLogger(t, "tools.delegate_tool", slog.LevelDebug)
	if got := parentLiveHome(&AIAgent{sessionDB: &state.SessionDB{}}); got != "" {
		t.Fatalf("home %q", got)
	}
	if !pinnedTrace(recs()) {
		t.Fatal("no debug trace")
	}
}

func TestParentDBPathAcceptsStrPaths(t *testing.T) {
	prof := filepath.Join(t.TempDir(), "str-prof")
	if got := parentLiveHome(&AIAgent{sessionDB: openPinDB(t, prof)}); got != prof {
		t.Fatalf("home %q", got)
	}
}

func TestPruneSweepsThePinnedRootNotAmbient(t *testing.T) {
	tmp := t.TempDir()
	ambient, pinned := filepath.Join(tmp, "ambient"), filepath.Join(tmp, "pinned")
	t.Setenv("ICEBATH_HOME", ambient)
	stale := time.Now().Add(-time.Duration(liveRetentionDays+1) * 24 * time.Hour)
	stalePinned := filepath.Join(pinned, "cache", "delegation", "live", "old-deleg")
	staleAmbient := filepath.Join(ambient, "cache", "delegation", "live", "other-profile-deleg")
	for _, d := range []string{stalePinned, staleAmbient} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(d, stale, stale); err != nil {
			t.Fatal(err)
		}
	}
	createLiveTranscripts(context.Background(), []map[string]any{{"goal": "sweep my own root"}}, nil, nil, nil, pinned)
	if _, err := os.Stat(stalePinned); !os.IsNotExist(err) {
		t.Fatal("pinned stale dir survived")
	}
	if _, err := os.Stat(staleAmbient); err != nil {
		t.Fatal("ambient stale dir removed")
	}
}

func TestManifestIncludesModelAndProvider(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	id, _, _ := createLiveTranscripts(ctx, []map[string]any{{"goal": "task 1"}, {"goal": "task 2"}}, nil, "openrouter/gpt-4o", "openrouter", "")
	m := liveManifest(t, filepath.Join(liveTranscriptRoot(ctx, ""), id, "manifest.json"))
	if m["model"] != "openrouter/gpt-4o" || m["provider"] != "openrouter" || len(m["tasks"].([]any)) != 2 {
		t.Fatalf("manifest %v", m)
	}
}

func TestNoFileInTheDispatchDirectoryCarriesTheRawKey(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	id, _, _ := createLiveTranscripts(ctx, []map[string]any{{"goal": "deploy using " + liveTestBearer}, {"goal": "second task"}}, nil, nil, nil, "")
	dir := filepath.Join(liveTranscriptRoot(ctx, ""), id)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
		mustNotContain(t, liveBody(t, filepath.Join(dir, e.Name())), liveTestBearer)
	}
	if !slices.Contains(names, "manifest.json") || !slices.ContainsFunc(names, func(n string) bool { return strings.HasSuffix(n, ".log") }) {
		t.Fatalf("names %v", names)
	}
}

func TestThinkingTextIsRedacted(t *testing.T) {
	w := liveWriter(t, "deleg_redact_think", "g", nil)
	w.observe("_thinking", "I should use "+liveTestEnvKey+" here", nil, nil, nil)
	mustNotContain(t, liveBody(t, w.Path()), liveTestEnvKey)
}

func TestRedactionCoversEveryHelperViaTheEventChokepoint(t *testing.T) {
	w := liveWriter(t, "deleg_redact_all", "g", nil)
	w.assistantText("a " + liveTestEnvKey)
	w.thinking("b " + liveTestEnvKey)
	w.toolStart("terminal", "c "+liveTestEnvKey)
	w.toolResult("terminal", "d "+liveTestEnvKey, nil, false)
	w.marker("e " + liveTestEnvKey)
	if err := w.Finalize(liveEntry("status", "error", "error", "f "+liveTestEnvKey)); err != nil {
		t.Fatal(err)
	}
	mustNotContain(t, liveBody(t, w.Path()), liveTestEnvKey)
}

func TestBenignTranscriptContentIsUntouched(t *testing.T) {
	w := liveWriter(t, "deleg_redact_benign", "refactor the parser", nil)
	w.observe("tool.started", "read_file", "src/parser.py", nil, nil)
	w.observe("tool.completed", "read_file", nil, nil, map[string]any{"result": "def parse(x): ...", "duration": 1.5})
	mustContain(t, liveBody(t, w.Path()), "src/parser.py", "def parse(x)", "refactor the parser")
}

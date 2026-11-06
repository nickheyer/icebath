package provider

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

var codexStubHooks = []string{"tool_progress_callback", "_fire_stream_delta", "_fire_reasoning_delta", "_emit_interim_assistant_message"}

func codexItemNote(method string, item map[string]any) map[string]any {
	return map[string]any{"method": method, "params": map[string]any{"item": item}}
}

func TestCodexItemHelpersForTheBridge(t *testing.T) {
	if codexItemToToolName(map[string]any{"type": "dynamicToolCall", "tool": "web_search"}) != "web_search" {
		t.Fatal("dynamic tool name")
	}
	for _, tool := range []string{"web_search", "browser_navigate"} {
		if got := codexItemToToolName(map[string]any{"type": "mcpToolCall", "server": CodexToolsMCPServerName, "tool": tool}); got != tool {
			t.Fatalf("icebath-tools bare name %q", got)
		}
	}
	args := codexItemToArgs(map[string]any{"type": "fileChange", "changes": []any{map[string]any{"path": "/a.py", "kind": map[string]any{"type": "add"}},
		map[string]any{"path": "/b.py", "kind": map[string]any{"type": "update"}}, "not-a-dict"}})
	if !pyrepr.Equal(pyjson.Plain(args), map[string]any{"changes": []any{map[string]any{"kind": "add", "path": "/a.py"}, map[string]any{"kind": "update", "path": "/b.py"}}}) {
		t.Fatalf("file change args %v", args)
	}
	if args := codexItemToArgs(map[string]any{"type": "dynamicToolCall", "arguments": []any{"a", "b"}}); !pyrepr.Equal(pyjson.Plain(args), map[string]any{"arguments": []any{"a", "b"}}) {
		t.Fatalf("wrapped args %v", args)
	}
	if p, _ := codexItemToPreview(map[string]any{"type": "commandExecution", "command": "echo " + strings.Repeat("x", 500)}).(string); p == "" || len([]rune(p)) > 120 {
		t.Fatalf("preview %q", p)
	}
	var changes []any
	for i := range 5 {
		changes = append(changes, map[string]any{"path": fmt.Sprintf("/p%d.py", i), "kind": map[string]any{"type": "update"}})
	}
	if p, _ := codexItemToPreview(map[string]any{"type": "fileChange", "changes": changes}).(string); !strings.Contains(p, "/p0.py") || !strings.Contains(p, "/p2.py") || !strings.Contains(p, "+2 more") {
		t.Fatalf("file preview %q", p)
	}
	if r, isErr := codexItemCompletionPayload(map[string]any{"type": "commandExecution", "exitCode": 0, "aggregatedOutput": "hello\nworld\n"}); r != "hello\nworld\n" || isErr {
		t.Fatalf("command payload %q %v", r, isErr)
	}
	if r, isErr := codexItemCompletionPayload(map[string]any{"type": "mcpToolCall", "error": map[string]any{"message": "nope"}}); !strings.Contains(r, "[error]") || !isErr {
		t.Fatalf("mcp error payload %q %v", r, isErr)
	}
}

func TestCodexEventBridgeDispatch(t *testing.T) {
	a := newFakeCodexAgent(codexStubHooks...)
	bridge := makeCodexAppServerEventBridge(a)
	bridge(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"delta": "hello "}})
	bridge(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"delta": "world"}})
	if got := a.callsOf("_fire_stream_delta"); !reflect.DeepEqual(got, [][]any{{"hello "}, {"world"}}) {
		t.Fatalf("stream deltas %v", got)
	}
	bridge(map[string]any{"method": "item/reasoning/delta", "params": map[string]any{"delta": "thinking..."}})
	if got := a.callsOf("_fire_reasoning_delta"); !reflect.DeepEqual(got, [][]any{{"thinking..."}}) || len(a.callsOf("_fire_stream_delta")) != 2 {
		t.Fatalf("reasoning %v", got)
	}

	a = newFakeCodexAgent(codexStubHooks...)
	bridge = makeCodexAppServerEventBridge(a)
	bridge(codexItemNote("item/started", map[string]any{"type": "commandExecution", "id": "exec-1", "command": "ls /tmp", "cwd": "/tmp"}))
	started := a.callsOf("tool_progress_callback")
	if len(started) != 1 || started[0][0] != "tool.started" || started[0][1] != "exec_command" || !strings.Contains(pyrepr.Str(started[0][2]), "ls /tmp") ||
		!pyrepr.Equal(pyjson.Plain(started[0][3]), map[string]any{"command": "ls /tmp", "cwd": "/tmp"}) {
		t.Fatalf("started %v", started)
	}

	a = newFakeCodexAgent(codexStubHooks...)
	bridge = makeCodexAppServerEventBridge(a)
	bridge(codexItemNote("item/started", map[string]any{"type": "commandExecution", "id": "exec-2", "command": "echo hi", "cwd": "/tmp"}))
	bridge(codexItemNote("item/completed", map[string]any{"type": "commandExecution", "id": "exec-2", "exitCode": 0, "aggregatedOutput": "hi\n", "durationMs": 42}))
	calls := a.callsOf("tool_progress_callback")
	extra, _ := calls[1][4].(*CodexToolProgressExtra)
	if len(calls) != 2 || calls[1][0] != "tool.completed" || calls[1][1] != "exec_command" || calls[1][2] != nil || calls[1][3] != nil ||
		extra == nil || math.Abs(*extra.Duration-0.042) > 1e-9 || extra.IsError || *extra.Result != "hi\n" {
		t.Fatalf("completed %v %+v", calls, extra)
	}

	a = newFakeCodexAgent(codexStubHooks...)
	bridge = makeCodexAppServerEventBridge(a)
	bridge(codexItemNote("item/started", map[string]any{"type": "webSearch", "id": "ws-1", "query": "icebath agent docs"}))
	bridge(codexItemNote("item/completed", map[string]any{"type": "webSearch", "id": "ws-1", "query": "icebath agent docs"}))
	calls = a.callsOf("tool_progress_callback")
	if len(calls) != 2 || calls[0][0] != "tool.started" || calls[1][0] != "tool.completed" || calls[0][1] != "web_search" || calls[0][2] != "icebath agent docs" ||
		!pyrepr.Equal(pyjson.Plain(calls[0][3]), map[string]any{"query": "icebath agent docs"}) {
		t.Fatalf("web search %v", calls)
	}
}

func TestCodexEventBridgeInterimAndRobustness(t *testing.T) {
	a := newFakeCodexAgent(codexStubHooks...)
	a.streamedText = "partial"
	makeCodexAppServerEventBridge(a)(codexItemNote("item/completed", map[string]any{"type": "agentMessage", "id": "am-1", "text": "I'll check the config first."}))
	if got := a.callsOf("_emit_interim_assistant_message"); !reflect.DeepEqual(got, [][]any{{map[string]any{"role": "assistant", "content": "I'll check the config first."}}}) || a.streamedText != "" {
		t.Fatalf("interim %v %q", got, a.streamedText)
	}
	a = newFakeCodexAgent(codexStubHooks...)
	a.hideCommentary = true
	bridge := makeCodexAppServerEventBridge(a)
	bridge(codexItemNote("item/completed", map[string]any{"type": "agentMessage", "id": "am-5", "text": "I'll check config."}))
	bridge(codexItemNote("item/started", map[string]any{"type": "commandExecution", "id": "cmd-1", "command": "ls"}))
	if len(a.callsOf("_emit_interim_assistant_message")) != 0 || len(a.callsOf("tool_progress_callback")) != 1 {
		t.Fatalf("commentary off %v", a.calls)
	}
	a = newFakeCodexAgent(codexStubHooks...)
	bridge = makeCodexAppServerEventBridge(a)
	bridge(map[string]any{"method": "item/started"})
	bridge(map[string]any{"method": "item/completed"})
	if len(a.callsOf("tool_progress_callback")) != 0 {
		t.Fatal("missing params fired a callback")
	}
	a = newFakeCodexAgent(codexStubHooks...)
	a.panics = map[string]bool{"tool_progress_callback": true, "_fire_stream_delta": true, "_emit_interim_assistant_message": true}
	bridge = makeCodexAppServerEventBridge(a)
	bridge(codexItemNote("item/started", map[string]any{"type": "commandExecution", "id": "exec-x", "command": "ls"}))
	bridge(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"delta": "x"}})
	bridge(codexItemNote("item/completed", map[string]any{"type": "agentMessage", "id": "am-x", "text": "hi"}))
}

func TestCodexRealProgressTouchesActivityAndIdleNotesDoNot(t *testing.T) {
	var notes []map[string]any
	for _, m := range []string{"item/agentMessage/delta", "item/reasoning/delta", "item/reasoning/summaryDelta", "item/reasoning/textDelta",
		"item/reasoning/summaryTextDelta", "item/commandExecution/outputDelta", "item/fileChange/outputDelta"} {
		notes = append(notes, map[string]any{"method": m, "params": map[string]any{"delta": "progress"}})
	}
	for _, m := range []string{"item/started", "item/completed"} {
		for _, kind := range []string{"agentMessage", "reasoning", "commandExecution", "mcpToolCall", "dynamicToolCall"} {
			notes = append(notes, codexItemNote(m, map[string]any{"id": "i1", "type": kind, "text": "progress"}))
		}
	}
	for _, note := range notes {
		a := newFakeCodexAgent("_touch_activity")
		a.hideCommentary = true
		bridge := makeCodexAppServerEventBridge(a)
		bridge(note)
		if got := a.callsOf("_touch_activity"); len(got) != 1 || got[0][0] != "codex app-server: "+note["method"].(string) {
			t.Fatalf("progress %v touched %v", note, got)
		}
		for _, idle := range []map[string]any{{"method": "item/agentMessage/delta", "params": map[string]any{"delta": ""}},
			{"method": "thread/tokenUsage/updated", "params": map[string]any{}}, {"method": "unknown/keepalive", "params": map[string]any{}}, {"method": "item/started", "params": map[string]any{}}} {
			bridge(idle)
		}
		if len(a.callsOf("_touch_activity")) != 1 {
			t.Fatalf("idle notes touched the activity clock %v", a.callsOf("_touch_activity"))
		}
	}
}

type fakeCodexDB struct {
	values  map[string]any
	patches []map[string]any
	queued  []map[string]any
	getErr  error
	failQ   bool
}

func (d *fakeCodexDB) GetModelConfigValue(id, key string) (any, bool, error) {
	v, ok := d.values[key]
	return v, ok, d.getErr
}
func (d *fakeCodexDB) PatchModelConfig(id string, patch map[string]any) error {
	d.patches = append(d.patches, patch)
	return nil
}
func (d *fakeCodexDB) QueueTokenCounts(sessionID, source string, counts map[string]any) error {
	if d.failQ {
		return errors.New("db closed")
	}
	counts["_source"] = source
	d.queued = append(d.queued, counts)
	return nil
}

type fakeCodexCompressor struct {
	awaiting                  bool
	updates                   []map[string]any
	contextLength, count      int
	rough, lastPrompt, lastCo int
	recorded, usageLess       int
}

func (c *fakeCodexCompressor) AwaitingRealUsageAfterCompression() bool { return c.awaiting }
func (c *fakeCodexCompressor) SetAwaitingRealUsageAfterCompression(on bool) {
	c.awaiting = on
}
func (c *fakeCodexCompressor) UpdateFromResponse(u map[string]any) { c.updates = append(c.updates, u) }
func (c *fakeCodexCompressor) SetContextLength(n int)              { c.contextLength = n }
func (c *fakeCodexCompressor) CompressionCount() int               { return c.count }
func (c *fakeCodexCompressor) SetCompressionCount(n int)           { c.count = n }
func (c *fakeCodexCompressor) SetLastCompressionRoughTokens(n int) { c.rough = n }
func (c *fakeCodexCompressor) SetLastUsage(p, co int)              { c.lastPrompt, c.lastCo = p, co }
func (c *fakeCodexCompressor) RecordCompletedCompaction(bool)      { c.recorded++ }
func (c *fakeCodexCompressor) NoteUsageLessResponse()              { c.usageLess++ }

func TestCodexUsageCoercionAndFailureDetails(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want int
	}{{true, 0}, {int64(-5), 0}, {int64(7), 7}, {3.9, 3}, {"12", 12}, {" 4 ", 4}, {"x", 0}, {nil, 0}, {[]any{}, 0}} {
		if got := codexCoerceUsageInt(tc.in); got != tc.want {
			t.Fatalf("%v -> %d want %d", tc.in, got, tc.want)
		}
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("NaN must raise")
			}
		}()
		codexCoerceUsageInt(math.NaN())
	}()
	inner := &StreamRequestError{Err: errors.New("connect refused"), Request: &FailedStreamRequest{URL: "https://x", Content: []byte("héllo")}}
	n, chain := codexRequestFailureDetails(fmt.Errorf("outer: %w", inner))
	if n == nil || *n != 6 || strings.Count(chain, " <- ") != 2 {
		t.Fatalf("details %v %q", n, chain)
	}
}

func TestCodexRecordUsageAndCompaction(t *testing.T) {
	db := &fakeCodexDB{}
	comp := &fakeCodexCompressor{awaiting: true}
	a := newFakeCodexAgent()
	a.sessionID, a.db, a.compressor, a.model, a.provider = "s1", db, comp, "gpt-5.5", "openai-codex"
	if got := recordCodexAppServerUsage(a, &CodexTurnResult{}, nil); len(got) != 0 || a.apiCalls != 1 || len(comp.updates) != 1 || comp.usageLess != 1 ||
		len(db.queued) != 1 || db.queued[0]["billing_mode"] != "subscription_included" || db.queued[0]["api_call_count"] != 1 {
		t.Fatalf("usage less %v %+v %v", got, comp, db.queued)
	}
	usage := pyjson.NewObject()
	for k, v := range map[string]any{"inputTokens": 1000, "cachedInputTokens": 400, "outputTokens": 50, "reasoningOutputTokens": 7, "totalTokens": 1050} {
		usage.Set(k, v)
	}
	messages := []map[string]any{{"role": "user", "content": "hi"}}
	got := recordCodexAppServerUsage(a, &CodexTurnResult{TokenUsageLast: usage, ModelContextWindow: 272000}, messages)
	if got["prompt_tokens"] != 1000 || got["input_tokens"] != 600 || got["cache_read_tokens"] != 400 || got["total_tokens"] != 1050 || got["last_prompt_tokens"] != 1000 ||
		a.usage["prompt_tokens"] != 1000 || a.usage["reasoning_tokens"] != 7 || comp.contextLength != 272000 || a.anchorSet != 1 || len(db.queued) != 2 {
		t.Fatalf("usage %v %v %+v", got, a.usage, comp)
	}
	turn := &CodexTurnResult{Compacted: true, ThreadID: "th", TurnID: "tu"}
	a.hooks["event_callback"] = true
	if !recordCodexAppServerCompaction(a, turn, 0, false) || comp.count != 1 || comp.recorded != 1 || comp.lastPrompt != -1 || !comp.awaiting || a.anchor != nil ||
		len(a.callsOf("emit_compaction_status")) != 1 || len(a.callsOf("publish_provider_native")) != 1 {
		t.Fatalf("compaction %+v %v", comp, a.calls)
	}
	ev := a.callsOf("event_callback")[0]
	if ev[0] != "session:compress" || !reflect.DeepEqual(ev[1], map[string]any{"platform": "", "session_id": "s1", "old_session_id": "", "in_place": false,
		"compression_count": 1, "runtime": "codex_app_server", "thread_id": "th", "turn_id": "tu"}) {
		t.Fatalf("event %v", ev)
	}
	if recordCodexAppServerCompaction(a, &CodexTurnResult{}, 0, false) {
		t.Fatal("uncompacted turn recorded")
	}
	if !recordCodexAppServerCompaction(a, &CodexTurnResult{}, 900, true) || comp.rough != 900 || len(a.callsOf("publish_provider_native")) != 1 {
		t.Fatalf("forced %+v", comp)
	}
	db.failQ = true
	recordCodexAppServerUsage(a, &CodexTurnResult{}, nil)
}

func TestCodexInterruptPromptAndThreadBinding(t *testing.T) {
	a := newFakeCodexAgent()
	a.interruptRequested, a.interruptMessage = true, "stop"
	if hit, msg := consumeUserInterrupt(a, false); hit || msg != nil || !a.interruptRequested {
		t.Fatal("inactive consume")
	}
	if hit, msg := consumeUserInterrupt(a, true); !hit || msg != "stop" || a.interruptRequested {
		t.Fatal("active consume")
	}
	a.cached, a.ephemeral = "base", "extra"
	if got := codexDeveloperInstructions(a); got != "base\n\nextra" {
		t.Fatalf("instructions %q", got)
	}
	a.cached = ""
	if got := codexDeveloperInstructions(a); got != "extra" {
		t.Fatalf("ephemeral only %q", got)
	}
	if id, err := storedCodexThreadID(a); id != "" || err != nil {
		t.Fatal("no db")
	}
	db := &fakeCodexDB{values: map[string]any{codexThreadIDKey: "th-9"}}
	a.db, a.sessionID = db, "s"
	if id, _ := storedCodexThreadID(a); id != "th-9" {
		t.Fatalf("stored %q", id)
	}
	storeCodexThreadID(a, "th-10")
	storeCodexThreadID(a, "")
	if !reflect.DeepEqual(db.patches, []map[string]any{{codexThreadIDKey: "th-10"}, {codexThreadIDKey: nil}}) {
		t.Fatalf("patches %v", db.patches)
	}
	res := codexTurnResultOf(true, "why", nil, 1, false, "e", "f", map[string]any{"codex_thread_id": "t"})
	if res["partial"] != true || res["interrupt_message"] != "why" || res["codex_thread_id"] != "t" {
		t.Fatalf("turn result %v", res)
	}
	if _, has := codexTurnResultOf(false, nil, nil, 0, true, nil, nil, nil)["interrupt_message"]; has {
		t.Fatal("empty interrupt message kept")
	}
}

func TestCodexStartThreadDropsUnresumableBinding(t *testing.T) {
	f := &fakeCodexClient{}
	f.handler = func(method string, params any) (any, error) {
		if method == "thread/resume" {
			return nil, &CodexAppServerError{Code: -32600, Message: "no rollout found for thread id th-old"}
		}
		return map[string]any{"thread": map[string]any{"id": "thread-new"}}, nil
	}
	db := &fakeCodexDB{}
	a := newFakeCodexAgent()
	a.db, a.sessionID = db, "s"
	a.session = newFakeSession(f, CodexAppServerSessionOptions{ResumeThreadID: "th-old"})
	id, err := startCodexThread(a)
	if err != nil || id != "thread-new" || !reflect.DeepEqual(db.patches, []map[string]any{{codexThreadIDKey: nil}}) ||
		!reflect.DeepEqual(a.callsOf("emit_diagnostic_status"), [][]any{{codexThreadResumeNotice}}) {
		t.Fatalf("restart %q %v %v %v", id, err, db.patches, a.calls)
	}
}

func TestCodexFinishTurnBookkeeping(t *testing.T) {
	a := newFakeCodexAgent("_sync_external_memory_for_turn", "_spawn_background_review")
	a.skillNudgeInterval, a.itersSinceSkill, a.validTools = 3, 1, map[string]bool{"skill_manage": true}
	msgs := []map[string]any{{"role": "user", "content": "q"}}
	finishCodexTurn(a, &CodexTurnResult{ToolIterations: 2, FinalText: "done"}, msgs, "q", false, "done")
	if a.itersSinceSkill != 0 || len(a.callsOf("_sync_external_memory_for_turn")) != 1 || !reflect.DeepEqual(a.callsOf("_spawn_background_review"), [][]any{{1, false, true}}) || a.apiCalls != 1 {
		t.Fatalf("finish %v iters %d", a.calls, a.itersSinceSkill)
	}
	b := newFakeCodexAgent("_sync_external_memory_for_turn", "_spawn_background_review")
	finishCodexTurn(b, &CodexTurnResult{FinalText: "x", Interrupted: true}, msgs, "q", true, "x")
	if len(b.calls) != 0 {
		t.Fatalf("interrupted turn ran hooks %v", b.calls)
	}
}

func TestCodexTokenCountDeltaMapsQueuedCounts(t *testing.T) {
	d := CodexTokenCountDelta("cli", map[string]any{"model": "gpt-5.5", "billing_provider": "openai-codex", "billing_base_url": "", "api_call_count": 1,
		"input_tokens": 600, "output_tokens": 50, "cache_read_tokens": 400, "cache_write_tokens": 0, "reasoning_tokens": 7,
		"estimated_cost_usd": 0.25, "cost_status": "estimated", "cost_source": "catalog", "billing_mode": nil})
	if d.InputTokens != 600 || d.CacheReadTokens != 400 || d.ReasoningTokens != 7 || *d.Model != "gpt-5.5" || *d.BillingBaseURL != "" || d.BillingMode != nil ||
		*d.EstimatedCostUSD != 0.25 || *d.CostStatus != "estimated" || d.APICallCount != 1 || *d.Source != "cli" {
		t.Fatalf("delta %+v", d)
	}
	if d := CodexTokenCountDelta("cron", map[string]any{"billing_mode": "subscription_included", "api_call_count": 1}); *d.BillingMode != "subscription_included" || d.EstimatedCostUSD != nil || d.Model != nil {
		t.Fatalf("usage less delta %+v", d)
	}
}

package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const codexCaseThread, codexCaseTurn = "thread-fake-001", "turn-fake-001"

// Agent over a fake client whose sessions spawn through the swapped factory
func codexCaseAgent(t *testing.T, f *fakeCodexClient, hooks ...string) *fakeCodexAgent {
	t.Helper()
	withCodexClientFactory(t, f)
	a := newFakeCodexAgent(hooks...)
	a.sessionID, a.platform, a.cwd = "sess-codex", "cli", "/tmp"
	a.env = &tools.Env{Home: t.TempDir()}
	a.requestOverrides = map[string]any{}
	return a
}

// Queues one exec tool call and the final agent message of a turn
func codexCaseToolTurn(f *fakeCodexClient, final string) {
	f.note("item/completed", map[string]any{"threadId": codexCaseThread, "turnId": codexCaseTurn, "item": map[string]any{
		"type": "commandExecution", "id": "exec_1", "command": "true", "aggregatedOutput": "ok", "exitCode": 0}})
	agentDone(f, codexCaseThread, codexCaseTurn, final)
}

func codexCaseRun(t *testing.T, a *fakeCodexAgent, user string, messages []map[string]any) map[string]any {
	t.Helper()
	result, err := RunCodexAppServerTurn(a, user, user, messages, "task-1", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func codexCaseRoles(result map[string]any) []any {
	var roles []any
	for _, m := range result["messages"].([]map[string]any) {
		roles = append(roles, m["role"])
	}
	return roles
}

// Client whose turn/start asks the agent's live session to interrupt, as a user stop mid turn
func codexInterruptingClient(a **fakeCodexAgent) *fakeCodexClient {
	return &fakeCodexClient{handler: func(method string, _ any) (any, error) {
		if method == "turn/start" {
			(*a).session.RequestInterrupt()
			return map[string]any{"turn": map[string]any{"id": codexCaseTurn}}, nil
		}
		return map[string]any{"thread": map[string]any{"id": codexCaseThread}}, nil
	}}
}

func TestCodexRuntimeLiveEvents(t *testing.T) {
	t.Run("test_stable_ids_match_history_projector", func(t *testing.T) {
		a := newFakeCodexAgent("_fire_stream_delta", "_fire_reasoning_delta", "tool_progress_callback", "tool_start_callback", "tool_complete_callback")
		bridge := makeCodexAppServerEventBridge(a)
		bridge(codexItemNote("item/started", map[string]any{"type": "mcpToolCall", "id": "m1", "server": "filesystem", "tool": "read", "arguments": map[string]any{"path": "a.py"}}))
		start := a.callsOf("tool_start_callback")
		if len(start) != 1 || start[0][0] != codexDeterministicCallID("mcp__filesystem__read", "m1") || start[0][1] != "mcp.filesystem.read" ||
			!pyrepr.Equal(pyjson.Plain(start[0][2]), map[string]any{"path": "a.py"}) {
			t.Fatalf("mcp start %v", start)
		}
		bridge(codexItemNote("item/started", map[string]any{"type": "fileChange", "id": "p1", "changes": []any{map[string]any{"kind": map[string]any{"type": "add"}, "path": "a.py"}}}))
		start = a.callsOf("tool_start_callback")
		if len(start) != 2 || start[1][0] != codexDeterministicCallID("apply_patch", "p1") || start[1][1] != "apply_patch" ||
			!pyrepr.Equal(pyjson.Plain(start[1][2]), map[string]any{"changes": []any{map[string]any{"kind": "add", "path": "a.py"}}}) {
			t.Fatalf("patch start %v", start)
		}
	})
	t.Run("test_failed_command_result_and_error_flag_are_preserved", func(t *testing.T) {
		a := newFakeCodexAgent("tool_progress_callback", "tool_start_callback", "tool_complete_callback")
		item := map[string]any{"type": "commandExecution", "id": "failed", "command": "false", "aggregatedOutput": "boom", "exitCode": 2}
		makeCodexAppServerEventBridge(a)(codexItemNote("item/completed", item))
		result, isError := codexItemCompletionPayload(item)
		if result != "[exit 2]\nboom" || !isError {
			t.Fatalf("payload %q %v", result, isError)
		}
		progress, complete := a.callsOf("tool_progress_callback"), a.callsOf("tool_complete_callback")
		if extra, _ := progress[0][4].(*CodexToolProgressExtra); extra == nil || !extra.IsError || complete[0][3] != "[exit 2]\nboom" {
			t.Fatalf("progress %v complete %v", progress, complete)
		}
	})
	t.Run("tui_gateway test_codex_bridge_emits_one_authoritative_tui_tool_lifecycle bridge half", func(t *testing.T) {
		a := newFakeCodexAgent("tool_progress_callback", "tool_start_callback", "tool_complete_callback")
		bridge := makeCodexAppServerEventBridge(a)
		started := map[string]any{"type": "commandExecution", "id": "tool-1", "command": "pwd", "cwd": "/tmp"}
		bridge(codexItemNote("item/started", started))
		bridge(codexItemNote("item/completed", map[string]any{"type": "commandExecution", "id": "tool-1", "command": "pwd", "cwd": "/tmp", "aggregatedOutput": "/tmp\n", "exitCode": 0}))
		start, complete := a.callsOf("tool_start_callback"), a.callsOf("tool_complete_callback")
		if len(start) != 1 || len(complete) != 1 || start[0][0] != "codex_exec_tool-1" || complete[0][0] != "codex_exec_tool-1" {
			t.Fatalf("lifecycle start %v complete %v", start, complete)
		}
	})
}

func TestCodexAppServerPersist(t *testing.T) {
	t.Run("test_codex_success_flushes_and_reports_persisted", func(t *testing.T) {
		f := &fakeCodexClient{}
		agentDone(f, codexCaseThread, codexCaseTurn, "CODEX_ASSISTANT")
		a := codexCaseAgent(t, f)
		result := codexCaseRun(t, a, "hello", []map[string]any{{"role": "user", "content": "hello"}})
		msgs := result["messages"].([]map[string]any)
		if _, isFloat := msgs[len(msgs)-1]["timestamp"].(float64); result["completed"] != true || !isFloat || result["agent_persisted"] != true {
			t.Fatalf("result %v", result)
		}
	})
	t.Run("test_codex_user_interrupt_is_reported_and_cleared", func(t *testing.T) {
		var a *fakeCodexAgent
		a = codexCaseAgent(t, codexInterruptingClient(&a))
		a.interruptRequested, a.interruptMessage = true, "new correction"
		result := codexCaseRun(t, a, "hello", []map[string]any{{"role": "user", "content": "hello"}})
		if result["interrupted"] != true || result["interrupt_message"] != "new correction" || len(a.callsOf("clear_interrupt")) != 1 || a.interruptRequested {
			t.Fatalf("result %v clears %v", result, a.callsOf("clear_interrupt"))
		}
	})
	t.Run("test_codex_turn_persists_each_message_exactly_once runtime half", func(t *testing.T) {
		f := &fakeCodexClient{}
		agentDone(f, codexCaseThread, codexCaseTurn, "CODEX_ASSISTANT")
		a := codexCaseAgent(t, f)
		a.db, a.dbCreated = &fakeCodexDB{values: map[string]any{}}, true
		var flushed []map[string]any
		a.flush = func(m []map[string]any) (bool, error) { flushed = slices.Clone(m); return true, nil }
		result := codexCaseRun(t, a, "USER_TURN", []map[string]any{{"role": "user", "content": "USER_TURN"}})
		var users, assistants int
		for _, m := range flushed {
			switch m["content"] {
			case "USER_TURN":
				users++
			case "CODEX_ASSISTANT":
				assistants++
				if _, ok := m["timestamp"].(float64); !ok {
					t.Fatalf("assistant row timestamp %#v", m["timestamp"])
				}
			}
		}
		if result["agent_persisted"] != true || users != 1 || assistants != 1 || len(a.callsOf("flush")) != 1 {
			t.Fatalf("flushed %v", flushed)
		}
	})
}

type fallbackRecordingCompressor struct {
	fakeCodexCompressor
	usedFallback []bool
}

func (c *fallbackRecordingCompressor) RecordCompletedCompaction(used bool) {
	c.usedFallback = append(c.usedFallback, used)
}

func TestCodexNativeBoundaryClearsStaleFallbackStreak(t *testing.T) {
	compressor := &fallbackRecordingCompressor{}
	a := newFakeCodexAgent()
	a.compressor = compressor
	if !recordCodexAppServerCompaction(a, &CodexTurnResult{ThreadID: "thread-1", TurnID: "normal-turn-1", Compacted: true}, 0, false) {
		t.Fatal("native compaction not recorded")
	}
	if !slices.Equal(compressor.usedFallback, []bool{false}) || compressor.count != 1 {
		t.Fatalf("recorded %v count %d", compressor.usedFallback, compressor.count)
	}
}

func TestCodexAppServerIntegrationRuntime(t *testing.T) {
	t.Run("test_run_conversation_returns_codex_shape", func(t *testing.T) {
		f := &fakeCodexClient{}
		codexCaseToolTurn(f, "echo: hello there")
		result := codexCaseRun(t, codexCaseAgent(t, f), "hello there", []map[string]any{{"role": "user", "content": "hello there"}})
		if result["final_response"] != "echo: hello there" || result["completed"] != true || result["partial"] != false || result["error"] != nil ||
			result["api_calls"] != 1 || result["codex_thread_id"] != codexCaseThread || result["codex_turn_id"] != codexCaseTurn {
			t.Fatalf("result %v", result)
		}
	})
	t.Run("test_projected_messages_are_spliced test_user_message_not_duplicated and memory sync", func(t *testing.T) {
		f := &fakeCodexClient{}
		codexCaseToolTurn(f, "echo: hi")
		a := codexCaseAgent(t, f, "_sync_external_memory_for_turn")
		result := codexCaseRun(t, a, "hi", []map[string]any{{"role": "user", "content": "hi"}})
		if roles := codexCaseRoles(result); !slices.Equal(roles, []any{"user", "assistant", "tool", "assistant"}) {
			t.Fatalf("roles %v", roles)
		}
		if sync := a.callsOf("_sync_external_memory_for_turn"); len(sync) != 1 || sync[0][1] != "echo: hi" {
			t.Fatalf("memory sync %v", sync)
		}
	})
	t.Run("test_nudge_counters_tick and background review thresholds", func(t *testing.T) {
		f := &fakeCodexClient{}
		codexCaseToolTurn(f, "done")
		a := codexCaseAgent(t, f, "_spawn_background_review")
		a.skillNudgeInterval, a.validTools["skill_manage"] = 10, true
		codexCaseRun(t, a, "hi", nil)
		if a.itersSinceSkill != 1 || len(a.callsOf("_spawn_background_review")) != 0 {
			t.Fatalf("below threshold iters %d review %v", a.itersSinceSkill, a.callsOf("_spawn_background_review"))
		}
		f2 := &fakeCodexClient{}
		codexCaseToolTurn(f2, "done")
		b := codexCaseAgent(t, f2, "_spawn_background_review")
		b.skillNudgeInterval, b.itersSinceSkill, b.validTools["skill_manage"] = 2, 1, true
		codexCaseRun(t, b, "hi", nil)
		if review := b.callsOf("_spawn_background_review"); len(review) != 1 || review[0][1] != false || review[0][2] != true || b.itersSinceSkill != 0 {
			t.Fatalf("above threshold review %v iters %d", review, b.itersSinceSkill)
		}
	})
	t.Run("test_gateway_terminal_cwd_seeds_codex_thread_cwd and test_configured_codex_binary_seeds_app_server_session", func(t *testing.T) {
		f := &fakeCodexClient{}
		agentDone(f, codexCaseThread, codexCaseTurn, "ok")
		a := codexCaseAgent(t, f)
		workspace := t.TempDir()
		t.Setenv("TERMINAL_CWD", workspace)
		a.cwd = ""
		os.WriteFile(filepath.Join(a.env.Home, "config.yaml"), []byte("model:\n  codex_bin: /opt/codex-app/codex\n"), 0o644)
		codexCaseRun(t, a, "hi", nil)
		if a.session.cwd != workspace || a.session.codexBin != "/opt/codex-app/codex" {
			t.Fatalf("session cwd %q bin %q", a.session.cwd, a.session.codexBin)
		}
	})
	approvalRouting := func(t *testing.T, mode any, _ func(a *fakeCodexAgent)) CodexServerRequestRouting {
		f := &fakeCodexClient{}
		a := codexCaseAgent(t, f)
		a.env.Config = config.Tree{"approvals": map[string]any{"mode": mode}}
		if err := ensureCodexSession(a, nil); err != nil {
			t.Fatal(err)
		}
		return a.session.routing
	}
	t.Run("approval modes", func(t *testing.T) {
		for name, cfg := range map[string]any{"test_approvals_mode_off_auto_approves_codex_server_requests": "off",
			"test_yaml_boolean_false_approval_mode_also_auto_approves": false} {
			if r := approvalRouting(t, cfg, nil); !r.AutoApproveExec || !r.AutoApproveApplyPatch {
				t.Fatalf("%s routing %+v", name, r)
			}
		}
		if r := approvalRouting(t, "manual", nil); r.AutoApproveExec || r.AutoApproveApplyPatch {
			t.Fatalf("test_manual_approvals_keep_codex_server_requests_fail_closed routing %+v", r)
		}
		restore := tools.SwapYoloModeFrozen(true)
		r := approvalRouting(t, "manual", nil)
		restore()
		if !r.AutoApproveExec || !r.AutoApproveApplyPatch {
			t.Fatalf("test_frozen_yolo_env_auto_approves_codex_server_requests routing %+v", r)
		}
		t.Setenv("ICEBATH_SESSION_KEY", "codex-yolo-session")
		tools.EnableSessionYolo("codex-yolo-session")
		t.Cleanup(func() { tools.DisableSessionYolo("codex-yolo-session") })
		if r := approvalRouting(t, "manual", nil); !r.AutoApproveExec || !r.AutoApproveApplyPatch {
			t.Fatalf("test_session_yolo_auto_approves_codex_server_requests routing %+v", r)
		}
	})
	t.Run("TestErrorHandling and TestSessionRetirementOnRunAgent", func(t *testing.T) {
		f := &fakeCodexClient{handler: func(method string, _ any) (any, error) {
			if method == "turn/start" {
				return nil, errors.New("codex crashed")
			}
			return map[string]any{"thread": map[string]any{"id": codexCaseThread}}, nil
		}}
		a := codexCaseAgent(t, f)
		result := codexCaseRun(t, a, "hi", []map[string]any{{"role": "user", "content": "hi"}})
		if result["partial"] != true || result["completed"] != false || !strings.Contains(pyrepr.Str(result["error"]), "codex crashed") || a.session != nil {
			t.Fatalf("test_session_exception_returns_partial_with_error and test_exception_path_also_drops_session %v", result)
		}
		var stopped *fakeCodexAgent
		stopped = codexCaseAgent(t, codexInterruptingClient(&stopped))
		result = codexCaseRun(t, stopped, "hi", nil)
		if result["partial"] != true || result["completed"] != false {
			t.Fatalf("test_interrupted_turn_marked_partial %v", result)
		}
		f3 := &fakeCodexClient{}
		agentDone(f3, codexCaseThread, codexCaseTurn, "fine")
		kept := codexCaseAgent(t, f3)
		codexCaseRun(t, kept, "hi", nil)
		if kept.session == nil {
			t.Fatal("test_normal_turn_keeps_session dropped the session")
		}
		f4 := &fakeCodexClient{handler: func(method string, _ any) (any, error) {
			if method == "turn/start" {
				return nil, codexTransportLost("codex app-server stdout closed")
			}
			return map[string]any{"thread": map[string]any{"id": codexCaseThread}}, nil
		}}
		retired := codexCaseAgent(t, f4)
		codexCaseRun(t, retired, "hi", nil)
		if retired.session != nil {
			t.Fatal("test_should_retire_drops_session kept the session")
		}
	})
	t.Run("TestCodexToolProgressBridge", func(t *testing.T) {
		a := newFakeCodexAgent("tool_progress_callback")
		bridge := makeCodexAppServerEventBridge(a)
		bridge(codexItemNote("item/started", map[string]any{"type": "mcpToolCall", "id": "m", "server": "srv", "tool": "t", "arguments": map[string]any{}}))
		bridge(codexItemNote("item/started", map[string]any{"type": "dynamicToolCall", "id": "d", "tool": "web_search", "arguments": map[string]any{"q": "x"}}))
		bridge(codexItemNote("item/started", map[string]any{"type": "agentMessage", "id": "a", "text": "hi"}))
		bridge(map[string]any{"method": "thread/started", "params": map[string]any{}})
		calls := a.callsOf("tool_progress_callback")
		if len(calls) != 2 || calls[0][1] != "mcp.srv.t" || calls[1][1] != "web_search" {
			t.Fatalf("test_mapper_mcp_and_dynamic_tool_calls and test_bridge_ignores_non_tool_items_and_other_methods %v", calls)
		}
		f := &fakeCodexClient{}
		codexCaseToolTurn(f, "done")
		b := codexCaseAgent(t, f, "tool_progress_callback")
		codexCaseRun(t, b, "hi", nil)
		if progress := b.callsOf("tool_progress_callback"); len(progress) == 0 || progress[0][1] != "exec_command" {
			t.Fatalf("test_session_wired_with_on_event_that_fires_tool_progress %v", progress)
		}
	})
}

func TestCodexRuntimeCloseClearsTheSessionEvenWhenCloseFails(t *testing.T) {
	f := &fakeCodexClient{}
	a := codexCaseAgent(t, f)
	a.session = newFakeSession(f, CodexAppServerSessionOptions{})
	if _, err := a.session.EnsureStarted(); err != nil {
		t.Fatal(err)
	}
	closeCodexSession(a)
	closeCodexSession(a)
	if a.session != nil || !f.closed {
		t.Fatalf("session %v closed %v", a.session, f.closed)
	}
}

func TestCodexRebuiltAgentResumesTheStoredThreadAndAnUnresumableOneFailsClosed(t *testing.T) {
	db := &fakeCodexDB{values: map[string]any{}}
	counter := 0
	var dead string
	newClient := func() *fakeCodexClient {
		f := &fakeCodexClient{}
		f.handler = func(method string, params any) (any, error) {
			switch method {
			case "thread/resume":
				id := pyrepr.Str(cxGet(params, "threadId"))
				if id == dead {
					return nil, &CodexAppServerError{Code: -32600, Message: "no rollout found for thread id " + id}
				}
				return map[string]any{"thread": map[string]any{"id": id}}, nil
			case "thread/start":
				counter++
				return map[string]any{"thread": map[string]any{"id": "thread-" + pyrepr.Str(counter)}}, nil
			}
			return map[string]any{"turn": map[string]any{"id": "turn-1"}}, nil
		}
		return f
	}
	turn := func(f *fakeCodexClient, user string) *fakeCodexAgent {
		thread := "thread-" + pyrepr.Str(counter+1)
		if v, ok := db.values[codexThreadIDKey]; ok && dead == "" {
			thread = pyrepr.Str(v)
		}
		agentDone(f, thread, "turn-1", "echo "+user)
		a := codexCaseAgent(t, f)
		a.sessionID, a.db, a.dbCreated = "sess-codex-restart", db, true
		if result := codexCaseRun(t, a, user, nil); result["completed"] != true {
			t.Fatalf("turn %q %v", user, result)
		}
		for _, p := range db.patches {
			for k, v := range p {
				db.values[k] = v
			}
		}
		return a
	}
	turn(newClient(), "Remember the word amber.")
	if db.values[codexThreadIDKey] != "thread-1" {
		t.Fatalf("binding %v", db.values)
	}
	second := newClient()
	b := turn(second, "Which word?")
	if got := codexStartParams(second, "thread/resume", "threadId"); !slices.Equal(got, []any{"thread-1"}) || len(codexStartParams(second, "thread/start", "cwd")) != 0 || len(b.callsOf("emit_diagnostic_status")) != 0 {
		t.Fatalf("resume %v starts %v notices %v", got, codexStartParams(second, "thread/start", "cwd"), b.callsOf("emit_diagnostic_status"))
	}
	dead = "thread-1"
	third := newClient()
	c := turn(third, "And now?")
	if len(codexStartParams(third, "thread/resume", "threadId")) != 1 || len(codexStartParams(third, "thread/start", "cwd")) != 1 || db.values[codexThreadIDKey] != "thread-2" {
		t.Fatalf("fail closed binding %v", db.values)
	}
	if notices := c.callsOf("emit_diagnostic_status"); len(notices) != 1 || notices[0][0] != "Codex thread could not be resumed; starting a new one." {
		t.Fatalf("notices %v", notices)
	}
}

type codexCaseAsker struct{}

func (codexCaseAsker) Ask(_ context.Context, _ []tools.Question) (tools.FormResult, error) {
	return tools.FormResult{}, nil
}

func TestCodexRuntimeGivesNoApprovalCallbackWhenNobodyCanAnswer(t *testing.T) {
	f := &fakeCodexClient{}
	interactive := codexCaseAgent(t, f)
	interactive.env.Ask = codexCaseAsker{}
	if err := ensureCodexSession(interactive, nil); err != nil || interactive.session.approvalCallback == nil {
		t.Fatalf("interactive callback missing %v", err)
	}
	t.Setenv("ICEBATH_SINGLE_QUERY_SESSION", "1")
	single := codexCaseAgent(t, f)
	single.env.Ask = codexCaseAsker{}
	if err := ensureCodexSession(single, nil); err != nil || single.session.approvalCallback != nil {
		t.Fatalf("single query session got a callback %v", err)
	}
}

var codexSeedHistory = []map[string]any{
	{"role": "system", "content": "SOUL: you are Icebath"},
	{"role": "user", "content": "my dog is called Shadow"},
	{"role": "assistant", "content": "Noted: Shadow.", "tool_calls": []any{map[string]any{"function": map[string]any{"name": "memory"}}}},
	{"role": "tool", "content": "saved"},
	{"role": "user", "content": "what is my dog called?"},
}

func TestCodexRuntimeHistorySeed(t *testing.T) {
	t.Run("test_fresh_thread_is_seeded_with_prior_turns_but_not_the_current_one", func(t *testing.T) {
		f := &fakeCodexClient{}
		a := codexCaseAgent(t, f)
		a.cached = "SOUL: you are Icebath"
		if err := ensureCodexSession(a, codexSeedHistory); err != nil {
			t.Fatal(err)
		}
		if _, err := a.session.EnsureStarted(); err != nil {
			t.Fatal(err)
		}
		starts := codexStartParams(f, "thread/start", "developerInstructions")
		instructions, _ := starts[0].(string)
		if len(starts) != 1 || !strings.HasPrefix(instructions, "SOUL: you are Icebath") || !strings.Contains(instructions, "my dog is called Shadow") ||
			!strings.Contains(instructions, "Noted: Shadow.") || !strings.Contains(instructions, "called tools: memory") || !strings.Contains(instructions, "saved") ||
			strings.Contains(instructions, "what is my dog called?") || strings.Count(instructions, "SOUL: you are Icebath") != 1 || *a.sessionPrompt != "SOUL: you are Icebath" {
			t.Fatalf("instructions %q prompt %v", instructions, a.sessionPrompt)
		}
		if err := ensureCodexSession(a, append(slices.Clone(codexSeedHistory), map[string]any{"role": "assistant", "content": "Shadow"})); err != nil {
			t.Fatal(err)
		}
		if len(codexStartParams(f, "thread/start", "developerInstructions")) != 1 {
			t.Fatal("thread restarted")
		}
	})
	t.Run("test_resumed_thread_gets_no_seed_but_a_failed_resume_fallback_does", func(t *testing.T) {
		f := &fakeCodexClient{handler: func(method string, params any) (any, error) {
			return map[string]any{"thread": map[string]any{"id": cxOr(cxGet(params, "threadId"), "fresh")}}, nil
		}}
		a := codexCaseAgent(t, f)
		a.cached, a.sessionID, a.db = "SOUL: you are Icebath", "s1", &fakeCodexDB{values: map[string]any{codexThreadIDKey: "stored-thread"}}
		if err := ensureCodexSession(a, codexSeedHistory); err != nil {
			t.Fatal(err)
		}
		if _, err := startCodexThread(a); err != nil {
			t.Fatal(err)
		}
		resumes := codexStartParams(f, "thread/resume", "developerInstructions")
		if len(resumes) != 1 || strings.Contains(pyrepr.Str(resumes[0]), "my dog is called Shadow") {
			t.Fatalf("resume %v", resumes)
		}
		failing := &fakeCodexClient{handler: func(method string, params any) (any, error) {
			if method == "thread/resume" {
				return nil, &CodexAppServerError{Code: -32602, Message: "unknown thread"}
			}
			return map[string]any{"thread": map[string]any{"id": "fresh"}}, nil
		}}
		b := codexCaseAgent(t, failing)
		b.cached, b.sessionID, b.db = "SOUL: you are Icebath", "s1", &fakeCodexDB{values: map[string]any{codexThreadIDKey: "stored-thread"}}
		if err := ensureCodexSession(b, codexSeedHistory); err != nil {
			t.Fatal(err)
		}
		if id, err := startCodexThread(b); err != nil || id != "fresh" {
			t.Fatalf("fallback %q %v", id, err)
		}
		starts := codexStartParams(failing, "thread/start", "developerInstructions")
		if len(starts) != 1 || !strings.Contains(pyrepr.Str(starts[0]), "my dog is called Shadow") {
			t.Fatalf("fresh start %v", starts)
		}
	})
}

func TestCodexNamedCustomProviderSelectsCodexModelProvider(t *testing.T) {
	threadStart := func(t *testing.T, provider, requested, model string) map[string]any {
		f := &fakeCodexClient{}
		a := codexCaseAgent(t, f)
		a.key, a.provider, a.requestedProvider, a.model = "sk-secret", provider, requested, model
		os.WriteFile(filepath.Join(a.env.Home, "config.yaml"), []byte("providers:\n  my-gateway:\n    api: https://gateway.example.com/v1\n    api_key: sk-secret\n"), 0o644)
		if err := ensureCodexSession(a, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := a.session.EnsureStarted(); err != nil {
			t.Fatal(err)
		}
		for _, r := range f.requests {
			if r.method == "thread/start" {
				return r.params.(map[string]any)
			}
		}
		t.Fatal("no thread/start")
		return nil
	}
	base := func(extra ...string) map[string]any {
		out := map[string]any{"cwd": "/tmp", "personality": "none"}
		for i := 0; i < len(extra); i += 2 {
			out[extra[i]] = extra[i+1]
		}
		return out
	}
	named := threadStart(t, "custom", "custom:my-gateway", "gpt-5.4")
	if !pyrepr.Equal(named, base("modelProvider", "my-gateway", "model", "gpt-5.4")) || strings.Contains(pyrepr.Repr(named), "sk-secret") {
		t.Fatalf("named %v", named)
	}
	if got := threadStart(t, "openai-codex", "openai-codex", "gpt-5.4"); !pyrepr.Equal(got, base("model", "gpt-5.4")) {
		t.Fatalf("openai-codex %v", got)
	}
	if got := threadStart(t, "custom", "custom", "gpt-5.4"); !pyrepr.Equal(got, base("model", "gpt-5.4")) {
		t.Fatalf("bare custom %v", got)
	}
	if got := threadStart(t, "openai-codex", "openai-codex", "gpt-5.6-sol-900k"); got["model"] != "gpt-5.6-sol" {
		t.Fatalf("900k alias %v", got)
	}
	if got := threadStart(t, "custom", "openai", "openai/gpt-5.4"); got["model"] != "gpt-5.4" {
		t.Fatalf("api key rung %v", got)
	}
}

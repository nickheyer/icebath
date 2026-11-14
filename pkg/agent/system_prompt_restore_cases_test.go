package agent

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Session store double answering get_session from a fixed row and recording prompt writes over a real database
type restoreStore struct {
	SessionStore
	row       *pyjson.Object
	getCalls  int
	updates   [][2]string
	updateErr error
}

func (s *restoreStore) GetSession(ctx context.Context, sessionID string) (*pyjson.Object, error) {
	s.getCalls++
	return s.row, nil
}

func (s *restoreStore) UpdateSystemPrompt(ctx context.Context, sessionID string, systemPrompt *string) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	s.updates = append(s.updates, [2]string{sessionID, *systemPrompt})
	return nil
}

func restoreRealDB(t *testing.T, sessionID, source string) *state.SessionDB {
	t.Helper()
	caseHome(t)
	db, err := state.NewSessionDB(t.Context(), filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateSession(state.NewSession{ID: sessionID, Source: source}); err != nil {
		t.Fatal(err)
	}
	return db
}

func restoreRowStore(t *testing.T, row *pyjson.Object) *restoreStore {
	return &restoreStore{SessionStore: restoreRealDB(t, "test-session-id", "cli"), row: row}
}

type restoreBuilds struct{ calls []*string }

// Agent double of the restore cases with the prompt builder replaced by a fixed prompt and its calls counted
func restoreAgent(t *testing.T, store SessionStore, prebuilt string) (*AIAgent, *restoreBuilds) {
	t.Helper()
	h := caseHome(t)
	builds := &restoreBuilds{}
	prev := loopBuildSystemPrompt
	loopBuildSystemPrompt = func(_ context.Context, _ *AIAgent, systemMessage *string) (string, error) {
		builds.calls = append(builds.calls, systemMessage)
		return prebuilt, nil
	}
	t.Cleanup(func() { loopBuildSystemPrompt = prev })
	a := &AIAgent{sessionID: "test-session-id", model: "test-model", provider: "openrouter", platform: "cli", sessionDB: store,
		env: &tools.Env{Home: h, Cwd: h}, botModeProtocol: true, passSessionID: true, quietMode: true, sessionPersistLock: &pythread.RLock{}}
	return a, builds
}

func restoreNow(t *testing.T, a *AIAgent, history []map[string]any) {
	t.Helper()
	if err := restoreOrBuildSystemPrompt(tools.WithToolEnv(context.Background(), a.env), a, nil, history); err != nil {
		t.Fatal(err)
	}
}

func hiHistory() []map[string]any { return []map[string]any{{"role": "user", "content": "hi"}} }

func storedSurfacePrompt(platform string) string {
	return "SYSTEM PROMPT BODY\n\nConversation started: Monday, January 05, 2026\nModel: test-model\nProvider: openrouter\nPlatform: " + platform
}

func announcedSurface(platform string) []map[string]any {
	return []map[string]any{
		{"role": "user", "content": "hi", "api_content": "hi\n\n" + surfaceSwitchNotePrefix + platform + surfaceNameEnd + " superseded]"},
		{"role": "assistant", "content": "hello"},
	}
}

func restoreToolDef(name string) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{"name": name, "parameters": map[string]any{}}}
}

type surfaceRestore struct {
	stored, current string
	history         []map[string]any
	toolNames       any
	tools           []any
}

func (r surfaceRestore) run(t *testing.T) (*AIAgent, *restoreBuilds) {
	t.Helper()
	row := pyjson.ObjectOf("system_prompt", storedSurfacePrompt(r.stored))
	if r.toolNames != nil {
		row.Set("tool_names", r.toolNames)
	}
	a, builds := restoreAgent(t, restoreRowStore(t, row), "BUILT_PROMPT")
	a.platform, a.tools = r.current, r.tools
	history := r.history
	if history == nil {
		history = hiHistory()
	}
	restoreNow(t, a, history)
	return a, builds
}

func patchToolPin(t *testing.T, restore func(ctx context.Context, agent mcp.AgentToolSnapshot, saved any) bool, persist func(ctx context.Context, agent mcp.AgentToolSnapshot)) {
	prevRestore, prevPersist := loopRestoreAgentToolPrefix, loopPersistAgentToolNames
	if restore != nil {
		loopRestoreAgentToolPrefix = restore
	}
	if persist != nil {
		loopPersistAgentToolNames = persist
	}
	t.Cleanup(func() { loopRestoreAgentToolPrefix, loopPersistAgentToolNames = prevRestore, prevPersist })
}

func TestSurfaceSwitch(t *testing.T) {
	tuiNote := surfaceSwitchNotePrefix + "tui" + surfaceNameEnd
	desktopNote := surfaceSwitchNotePrefix + "desktop" + surfaceNameEnd
	t.Run("test_switch_reuses_the_stored_prompt", func(t *testing.T) {
		a, builds := surfaceRestore{stored: "desktop", current: "tui"}.run(t)
		if a.cachedSystemPrompt != storedSurfacePrompt("desktop") || len(builds.calls) != 0 {
			t.Fatalf("prompt %q builds %d", a.cachedSystemPrompt, len(builds.calls))
		}
	})
	t.Run("test_switch_stages_the_new_surface_guidance", func(t *testing.T) {
		a, _ := surfaceRestore{stored: "desktop", current: "tui"}.run(t)
		if !strings.HasPrefix(a.surfaceSwitchNote, tuiNote) || !strings.Contains(a.surfaceSwitchNote, "terminal UI (TUI)") {
			t.Fatalf("note %q", a.surfaceSwitchNote)
		}
	})
	t.Run("test_same_surface_stages_nothing", func(t *testing.T) {
		if a, _ := (surfaceRestore{stored: "cli", current: "cli"}).run(t); a.surfaceSwitchNote != "" {
			t.Fatalf("note %q", a.surfaceSwitchNote)
		}
	})
	t.Run("test_stored_prompt_platform_ignores_runtime_hint_decoys", func(t *testing.T) {
		stored := "SYSTEM PROMPT BODY\n\nConversation started: Monday, January 05, 2026\nModel: test-model\nProvider: openrouter\nPlatform: desktop\n\n" +
			RuntimeEnvironmentHeading + "\n\nHost: Example\nPlatform: tui\n\n\n" + RuntimeEnvironmentEnd
		if got := IdentityLineValue(stored, "Platform"); got != "desktop" {
			t.Fatalf("identity platform %q", got)
		}
		a, _ := restoreAgent(t, restoreRowStore(t, pyjson.ObjectOf("system_prompt", stored)), "BUILT_PROMPT")
		a.platform = "tui"
		restoreNow(t, a, hiHistory())
		if !strings.HasPrefix(a.surfaceSwitchNote, tuiNote) {
			t.Fatalf("note %q", a.surfaceSwitchNote)
		}
	})
	t.Run("test_not_restaged_once_the_transcript_carries_it", func(t *testing.T) {
		if a, _ := (surfaceRestore{stored: "desktop", current: "tui", history: announcedSurface("tui")}).run(t); a.surfaceSwitchNote != "" {
			t.Fatalf("note %q", a.surfaceSwitchNote)
		}
	})
	t.Run("test_returning_to_the_prompts_own_surface_is_announced", func(t *testing.T) {
		a, _ := surfaceRestore{stored: "desktop", current: "desktop", history: announcedSurface("tui")}.run(t)
		if !strings.HasPrefix(a.surfaceSwitchNote, desktopNote) || !strings.Contains(a.surfaceSwitchNote, "the interface section in the system prompt above") {
			t.Fatalf("note %q", a.surfaceSwitchNote)
		}
	})
	t.Run("test_rebuild_also_retires_a_stale_note", func(t *testing.T) {
		a, builds := restoreAgent(t, restoreRowStore(t, pyjson.ObjectOf("system_prompt", storedSurfacePrompt("desktop"))), storedSurfacePrompt("desktop"))
		a.model, a.platform = "other-model", "desktop"
		restoreNow(t, a, announcedSurface("tui"))
		if len(builds.calls) != 1 || !strings.HasPrefix(a.surfaceSwitchNote, desktopNote) {
			t.Fatalf("builds %d note %q", len(builds.calls), a.surfaceSwitchNote)
		}
	})
	t.Run("test_tool_prefix_stays_pinned_on_the_turn_that_announces_a_switch", func(t *testing.T) {
		pins, persists := 0, 0
		patchToolPin(t, func(context.Context, mcp.AgentToolSnapshot, any) bool { pins++; return false },
			func(context.Context, mcp.AgentToolSnapshot) { persists++ })
		surfaceRestore{stored: "desktop", current: "tui", toolNames: `["desktop_ui_tool"]`}.run(t)
		if pins != 1 || persists != 0 {
			t.Fatalf("pin restores %d persists %d", pins, persists)
		}
	})
	t.Run("test_the_note_names_the_tools_the_pin_carried_forward", func(t *testing.T) {
		patchToolPin(t, func(_ context.Context, agent mcp.AgentToolSnapshot, _ any) bool {
			agent.(*AIAgent).tools = []any{restoreToolDef("read_file"), restoreToolDef("focus_pane")}
			return true
		}, nil)
		a, _ := surfaceRestore{stored: "desktop", current: "tui", toolNames: `["focus_pane"]`, tools: []any{restoreToolDef("read_file")}}.run(t)
		_, after, found := strings.Cut(a.surfaceSwitchNote, "were not loaded for this interface")
		if !found || !strings.Contains(a.surfaceSwitchNote, "focus_pane") || strings.Contains(after, "read_file") {
			t.Fatalf("note %q", a.surfaceSwitchNote)
		}
	})
	t.Run("test_note_rides_the_user_message_channel_once", func(t *testing.T) {
		a, _ := surfaceRestore{stored: "desktop", current: "tui"}.run(t)
		staged := a.surfaceSwitchNote
		if got := mergeGatewayNotes(a, rowsToMessages(hiHistory()), 0, ""); staged == "" || got != staged {
			t.Fatalf("merged %q staged %q", got, staged)
		}
		if consumeSurfaceSwitchNote(a) != "" {
			t.Fatal("the note is consumed once")
		}
	})
}

func TestStoredPromptReuse(t *testing.T) {
	t.Run("test_present_row_is_reused_verbatim", func(t *testing.T) {
		warnings := captureLogger(t, "agent.conversation_loop", slog.LevelWarn)
		stored := "Stored prompt from turn 1 — byte-identical reuse"
		store := restoreRowStore(t, pyjson.ObjectOf("system_prompt", stored))
		a, builds := restoreAgent(t, store, "BUILT_PROMPT")
		restoreNow(t, a, hiHistory())
		if a.cachedSystemPrompt != stored || len(builds.calls) != 0 || len(store.updates) != 0 || len(warnings()) != 0 {
			t.Fatalf("prompt %q builds %d updates %v warnings %v", a.cachedSystemPrompt, len(builds.calls), store.updates, warnings())
		}
	})
	t.Run("test_present_row_with_stale_runtime_identity_rebuilds", func(t *testing.T) {
		infos := captureLogger(t, "agent.conversation_loop", slog.LevelInfo)
		head := "You are Icebath Agent.\n\nConversation started: Tuesday, June 16, 2026\nSession ID: test-session-id\n"
		store := restoreRowStore(t, pyjson.ObjectOf("system_prompt", head+"Model: anthropic/claude-opus-4.8-fast\nProvider: openrouter"))
		a, builds := restoreAgent(t, store, head+"Model: openai/gpt-5.5\nProvider: openrouter")
		a.model = "openai/gpt-5.5"
		restoreNow(t, a, hiHistory())
		if !strings.HasSuffix(a.cachedSystemPrompt, "Model: openai/gpt-5.5\nProvider: openrouter") || len(builds.calls) != 1 || builds.calls[0] != nil {
			t.Fatalf("prompt %q builds %v", a.cachedSystemPrompt, builds.calls)
		}
		if !reflect.DeepEqual(store.updates, [][2]string{{a.sessionID, a.cachedSystemPrompt}}) {
			t.Fatalf("updates %v", store.updates)
		}
		found := false
		for _, r := range infos() {
			found = found || strings.Contains(r.msg, "stale runtime identity")
		}
		if !found {
			t.Fatalf("stale identity log missing from %v", infos())
		}
	})
	t.Run("test_rebuilding_an_existing_sessions_prompt_keeps_its_pinned_tools", func(t *testing.T) {
		toolDef := func(name string) map[string]any {
			return map[string]any{"type": "function", "function": map[string]any{"name": name, "description": name + " v1", "parameters": map[string]any{}}}
		}
		pinned := []any{toolDef("read_file"), toolDef("skill_manage"), toolDef("terminal")}
		db := restoreRealDB(t, "test-session-id", "tui")
		old := "Model: old-model\nProvider: openrouter"
		if err := db.UpdateSystemPrompt(t.Context(), "test-session-id", &old); err != nil {
			t.Fatal(err)
		}
		if err := db.UpdateSessionToolNames("test-session-id", map[string]any{"version": mcp.ToolPinVersion(), "tools": pinned}); err != nil {
			t.Fatal(err)
		}
		a, builds := restoreAgent(t, db, "BUILT_PROMPT")
		a.botModeProtocol = false
		a.tools = []any{toolDef("read_file"), toolDef("terminal")}
		restoreNow(t, a, hiHistory())
		if len(builds.calls) != 1 || !reflect.DeepEqual(a.tools, pinned) || !a.validToolNames["skill_manage"] {
			t.Fatalf("builds %d tools %v valid %v", len(builds.calls), mcp.AgentToolNames(a), a.validToolNames)
		}
		if got := restoreStoredPin(t, db); !reflect.DeepEqual(got, pinned) {
			t.Fatalf("stored pin %v", got)
		}
	})
	t.Run("test_a_swept_pin_row_is_re_pinned_on_the_next_turn", func(t *testing.T) {
		toolDefs := []any{map[string]any{"type": "function", "function": map[string]any{"name": "read_file", "description": "", "parameters": map[string]any{}}}}
		db := restoreRealDB(t, "test-session-id", "tui")
		built := "BUILT_PROMPT"
		if err := db.UpdateSystemPrompt(t.Context(), "test-session-id", &built); err != nil {
			t.Fatal(err)
		}
		if err := db.UpdateSessionToolNames("test-session-id", nil); err != nil {
			t.Fatal(err)
		}
		if err := db.ExecuteWrite(t.Context(), func(conn state.SQLiteConn) error {
			_, err := conn.ExecContext(t.Context(), "UPDATE sessions SET tool_names = ? WHERE id = 'test-session-id'", strings.Repeat("ab", 32))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		a, _ := restoreAgent(t, db, "REBUILT")
		a.tools = append([]any{}, toolDefs...)
		restoreNow(t, a, hiHistory())
		if a.cachedSystemPrompt != "BUILT_PROMPT" {
			t.Fatalf("prompt %q", a.cachedSystemPrompt)
		}
		if got := restoreStoredPin(t, db); !reflect.DeepEqual(got, toolDefs) {
			t.Fatalf("stored pin %v", got)
		}
	})
}

// Tools of the pin the session row resolves to
func restoreStoredPin(t *testing.T, db *state.SessionDB) []any {
	t.Helper()
	row, err := db.GetSession(t.Context(), "test-session-id")
	if err != nil || row == nil {
		t.Fatalf("session row %v %v", row, err)
	}
	raw, _ := row.Get("tool_names")
	text, _ := raw.(string)
	decoded, err := pyjson.Loads(text)
	if err != nil {
		t.Fatalf("tool_names %q: %v", text, err)
	}
	pin, _ := pyjson.Plain(decoded).(map[string]any)
	list, _ := pin["tools"].([]any)
	return list
}

func TestLegitimateFreshBuild(t *testing.T) {
	t.Run("test_no_history_skips_db_and_builds_fresh", func(t *testing.T) {
		warnings := captureLogger(t, "agent.conversation_loop", slog.LevelWarn)
		store := restoreRowStore(t, nil)
		a, builds := restoreAgent(t, store, "BUILT_PROMPT")
		restoreNow(t, a, nil)
		if store.getCalls != 0 || len(builds.calls) != 1 || builds.calls[0] != nil || a.cachedSystemPrompt != "BUILT_PROMPT" {
			t.Fatalf("gets %d builds %v prompt %q", store.getCalls, builds.calls, a.cachedSystemPrompt)
		}
		if !reflect.DeepEqual(store.updates, [][2]string{{a.sessionID, "BUILT_PROMPT"}}) || len(warnings()) != 0 {
			t.Fatalf("updates %v warnings %v", store.updates, warnings())
		}
	})
	t.Run("test_no_db_skips_persistence", func(t *testing.T) {
		a, builds := restoreAgent(t, nil, "BUILT_PROMPT")
		restoreNow(t, a, nil)
		if len(builds.calls) != 1 || a.cachedSystemPrompt != "BUILT_PROMPT" {
			t.Fatalf("builds %d prompt %q", len(builds.calls), a.cachedSystemPrompt)
		}
	})
}

func TestSilentFailureWarnings(t *testing.T) {
	t.Run("test_db_write_failure_warns_loudly", func(t *testing.T) {
		warnings := captureLogger(t, "agent.conversation_loop", slog.LevelWarn)
		store := restoreRowStore(t, nil)
		store.updateErr = errors.New("database is locked")
		a, builds := restoreAgent(t, store, "BUILT_PROMPT")
		restoreNow(t, a, nil)
		if len(builds.calls) != 1 || a.cachedSystemPrompt != "BUILT_PROMPT" {
			t.Fatalf("builds %d prompt %q", len(builds.calls), a.cachedSystemPrompt)
		}
		found := false
		for _, r := range warnings() {
			found = found || (strings.Contains(r.msg, "update_system_prompt failed") && strings.Contains(r.msg, "database is locked"))
		}
		if !found {
			t.Fatalf("write failure warning missing from %v", warnings())
		}
	})
}

func patchPartsBuilder(t *testing.T, fn func() (SystemPromptParts, error)) *int {
	calls := 0
	prev := reconstructPartsBuilder
	reconstructPartsBuilder = func(context.Context, *AIAgent, *string) (SystemPromptParts, error) {
		calls++
		return fn()
	}
	t.Cleanup(func() { reconstructPartsBuilder = prev })
	return &calls
}

func TestStaticPrefixReconstructionOnRestore(t *testing.T) {
	restoreCaching := func(t *testing.T, stored string, parts func() (SystemPromptParts, error)) *AIAgent {
		a, _ := restoreAgent(t, restoreRowStore(t, pyjson.ObjectOf("system_prompt", stored)), "BUILT_PROMPT")
		a.usePromptCaching = true
		patchPartsBuilder(t, parts)
		restoreNow(t, a, hiHistory())
		return a
	}
	t.Run("test_restore_reconstructs_static_prefix_when_it_matches", func(t *testing.T) {
		stable := "STATIC IDENTITY AND GUIDANCE"
		stored := stable + "\n\nper-session context\n\nvolatile tail"
		a := restoreCaching(t, stored, func() (SystemPromptParts, error) { return SystemPromptParts{Stable: stable}, nil })
		if a.cachedSystemPrompt != stored || a.cachedSystemPromptStatic != stable {
			t.Fatalf("prompt %q static %q", a.cachedSystemPrompt, a.cachedSystemPromptStatic)
		}
	})
	t.Run("test_restore_leaves_static_unset_on_prefix_mismatch", func(t *testing.T) {
		stored := "OLD STATIC HEAD\n\nper-session context"
		a := restoreCaching(t, stored, func() (SystemPromptParts, error) { return SystemPromptParts{Stable: "NEW STATIC HEAD"}, nil })
		if a.cachedSystemPrompt != stored || a.cachedSystemPromptStatic != "" {
			t.Fatalf("prompt %q static %q", a.cachedSystemPrompt, a.cachedSystemPromptStatic)
		}
	})
	t.Run("test_restore_survives_parts_builder_exception", func(t *testing.T) {
		stored := "Stored prompt — must survive"
		a := restoreCaching(t, stored, func() (SystemPromptParts, error) { return SystemPromptParts{}, errors.New("boom") })
		if a.cachedSystemPrompt != stored || a.cachedSystemPromptStatic != "" {
			t.Fatalf("prompt %q static %q", a.cachedSystemPrompt, a.cachedSystemPromptStatic)
		}
	})
}

func TestStaticPrefixRebuildMemoization(t *testing.T) {
	memoAgent := func(t *testing.T, stored string) *AIAgent {
		a, _ := restoreAgent(t, nil, "BUILT_PROMPT")
		a.usePromptCaching, a.cachedSystemPrompt = true, stored
		return a
	}
	ctx := context.Background()
	mismatch := func() (SystemPromptParts, error) { return SystemPromptParts{Stable: "MISMATCH"}, nil }
	t.Run("test_failed_rebuild_is_memoized_per_stored_prompt", func(t *testing.T) {
		a := memoAgent(t, "STORED PROMPT\n\ntail")
		calls := patchPartsBuilder(t, mismatch)
		for range 3 {
			ReconstructStaticPrefix(ctx, a, nil, "restore")
		}
		if *calls != 1 || a.cachedSystemPromptStatic != "" {
			t.Fatalf("builder calls %d static %q", *calls, a.cachedSystemPromptStatic)
		}
	})
	t.Run("test_changed_stored_prompt_retries_once", func(t *testing.T) {
		a := memoAgent(t, "OLD STORED")
		calls := patchPartsBuilder(t, mismatch)
		ReconstructStaticPrefix(ctx, a, nil, "restore")
		a.cachedSystemPrompt = "NEW STORED"
		ReconstructStaticPrefix(ctx, a, nil, "restore")
		ReconstructStaticPrefix(ctx, a, nil, "restore")
		if *calls != 2 {
			t.Fatalf("builder calls %d", *calls)
		}
	})
	t.Run("test_success_clears_failure_memo_and_early_returns", func(t *testing.T) {
		stable := "STATIC HEAD"
		a := memoAgent(t, stable+"\n\nvolatile")
		calls := patchPartsBuilder(t, func() (SystemPromptParts, error) { return SystemPromptParts{Stable: stable}, nil })
		ReconstructStaticPrefix(ctx, a, nil, "restore")
		ReconstructStaticPrefix(ctx, a, nil, "restore")
		if *calls != 1 || a.cachedSystemPromptStatic != stable || a.staticRebuildFailedFor != "" {
			t.Fatalf("builder calls %d static %q memo %q", *calls, a.cachedSystemPromptStatic, a.staticRebuildFailedFor)
		}
	})
}

func TestPerResponseSessionWritePath(t *testing.T) {
	t.Run("test_warning_is_a_first_turn_artifact_not_a_lost_write", func(t *testing.T) {
		sessionID := "gc_run_room42_default_Worker_9a7e3b1c05d24e6fb83a1c7d9e0f2a4b"
		db := restoreRealDB(t, sessionID, "studio")
		if _, err := db.AppendMessage(t.Context(), sessionID, map[string]any{"role": "user", "content": "hi"}, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
		nullWarned := func(records []loggerRecord) bool {
			for _, r := range records {
				if strings.Contains(r.msg, "is null") {
					return true
				}
			}
			return false
		}
		warnings := captureLogger(t, "agent.conversation_loop", slog.LevelWarn)
		first, _ := restoreAgent(t, db, "GROUP_PROMPT")
		first.sessionID = sessionID
		restoreNow(t, first, hiHistory())
		firstWarnings := warnings()
		if !nullWarned(firstWarnings) {
			t.Fatalf("first turn warnings %v", firstWarnings)
		}
		second, builds := restoreAgent(t, db, "GROUP_PROMPT")
		second.sessionID = sessionID
		restoreNow(t, second, hiHistory())
		if second.cachedSystemPrompt != "GROUP_PROMPT" || len(builds.calls) != 0 || nullWarned(warnings()[len(firstWarnings):]) {
			t.Fatalf("second turn prompt %q builds %d warnings %v", second.cachedSystemPrompt, len(builds.calls), warnings()[len(firstWarnings):])
		}
	})
}

func TestNullStoredPromptDoesNotTakeTheStaleProbePath(t *testing.T) {
	probes := 0
	prev := loopStoredPromptCapabilityStale
	loopStoredPromptCapabilityStale = func(string, string) bool { probes++; return false }
	t.Cleanup(func() { loopStoredPromptCapabilityStale = prev })
	bare := &AIAgent{botModeProtocol: true, sessionTitleHint: "Bot Chat", sessionID: "test-session-id"}
	if botChatPromptStale(context.Background(), bare, "") || probes != 0 {
		t.Fatalf("an absent stored prompt ran the probe %d times", probes)
	}
	db := restoreRealDB(t, "test-session-id", "tui")
	row, err := db.GetSession(t.Context(), "test-session-id")
	if err != nil || row == nil {
		t.Fatalf("row %v %v", row, err)
	}
	if prompt, _ := row.Get("system_prompt"); prompt != nil {
		t.Fatalf("a fresh row holds no prompt, got %v", prompt)
	}
	a, builds := restoreAgent(t, db, "BUILT")
	restoreNow(t, a, hiHistory())
	if probes != 0 || len(builds.calls) != 1 {
		t.Fatalf("probes %d builds %d", probes, len(builds.calls))
	}
}

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Clears every heal counter, notice and notified mark for the rest of the test
func resetHealLog(t *testing.T) {
	t.Helper()
	clearHeal := func() {
		emptyHealLogLock.Lock()
		defer emptyHealLogLock.Unlock()
		clear(emptyHealLogState)
		emptyHealLogOrder = nil
		clear(emptyHealUserNotified)
		clear(emptyHealPendingNotice)
	}
	clearHeal()
	t.Cleanup(clearHeal)
}

// Temp home whose config sets the heal escalation threshold
func healThresholdHome(t *testing.T, threshold string) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if threshold == "" {
		return
	}
	body := "agent:\n  sanitizer_heal_escalation_threshold: " + threshold + "\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func poisonedRows() []map[string]any {
	return []map[string]any{{"role": "user", "content": "hi"}, {"role": "assistant", "content": ""}, {"role": "user", "content": "next"}}
}

func healN(sessionID string, n int) {
	for range n {
		repairEmptyNonFinalMessages(context.Background(), sessionID, poisonedRows())
	}
}

func TestFillEmptyNonFinalWirePayload(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Run("test_fills_empty_non_final_assistant", func(t *testing.T) {
		msg := map[string]any{"role": "assistant", "content": ""}
		if !fillEmptyNonFinalWirePayload(msg, false) || msg["content"] != InterruptedPlaceholder {
			t.Fatalf("msg %v", msg)
		}
	})
	t.Run("test_fills_empty_non_final_user", func(t *testing.T) {
		msg := map[string]any{"role": "user", "content": nil}
		if !fillEmptyNonFinalWirePayload(msg, false) || msg["content"] != InterruptedPlaceholder {
			t.Fatalf("msg %v", msg)
		}
	})
	t.Run("test_skips_final_turn", func(t *testing.T) {
		msg := map[string]any{"role": "assistant", "content": ""}
		if fillEmptyNonFinalWirePayload(msg, true) || msg["content"] != "" {
			t.Fatalf("msg %v", msg)
		}
	})
	t.Run("test_skips_tool_call_turn", func(t *testing.T) {
		msg := map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "c1", "function": map[string]any{"name": "x", "arguments": "{}"}}}}
		if fillEmptyNonFinalWirePayload(msg, false) || msg["content"] != "" {
			t.Fatalf("msg %v", msg)
		}
	})
	t.Run("test_skips_codex_commentary_carrier", func(t *testing.T) {
		msg := map[string]any{"role": "assistant", "content": "", "codex_message_items": []any{map[string]any{"type": "text", "text": "hi"}}}
		if fillEmptyNonFinalWirePayload(msg, false) || msg["content"] != "" {
			t.Fatalf("msg %v", msg)
		}
	})
	t.Run("test_skips_already_populated_content", func(t *testing.T) {
		msg := map[string]any{"role": "assistant", "content": "hello"}
		if fillEmptyNonFinalWirePayload(msg, false) || msg["content"] != "hello" {
			t.Fatalf("msg %v", msg)
		}
	})
}

func countLevels(records []loggerRecord) (warnings, errs int) {
	for _, r := range records {
		switch r.level {
		case slog.LevelWarn:
			warnings++
		case slog.LevelError:
			errs++
		}
	}
	return warnings, errs
}

func TestHealLogEscalation(t *testing.T) {
	t.Run("test_warning_then_one_error_then_silence", func(t *testing.T) {
		resetHealLog(t)
		healThresholdHome(t, "3")
		records := captureLogger(t, "run_agent", slog.LevelWarn)
		durable := poisonedRows()
		for range 5 {
			rows := make([]map[string]any, len(durable))
			for i, m := range durable {
				rows[i] = shallowDict(m)
			}
			out := repairEmptyNonFinalMessages(context.Background(), "sess-heal", rows)
			if out[1]["content"] != InterruptedPlaceholder || durable[1]["content"] != "" {
				t.Fatalf("out %v durable %v", out, durable)
			}
		}
		if w, e := countLevels(records()); w != 2 || e != 1 {
			t.Fatalf("warnings %d errors %d", w, e)
		}
	})
	t.Run("test_sessions_do_not_share_heal_counters", func(t *testing.T) {
		resetHealLog(t)
		healThresholdHome(t, "3")
		records := captureLogger(t, "run_agent", slog.LevelWarn)
		healN("sess-a", 1)
		healN("sess-b", 1)
		if w, e := countLevels(records()); w != 2 || e != 0 {
			t.Fatalf("warnings %d errors %d", w, e)
		}
	})
	t.Run("test_owner_still_heals_wire_copy_only", func(t *testing.T) {
		resetHealLog(t)
		healThresholdHome(t, "")
		durable := poisonedRows()
		out := repairEmptyNonFinalMessages(context.Background(), "", durable)
		if out[1]["content"] != InterruptedPlaceholder || durable[1]["content"] != "" || &out[0] == &durable[0] {
			t.Fatalf("out %v durable %v", out, durable)
		}
	})
}

func TestOneTimeUserNotice(t *testing.T) {
	t.Run("test_notice_queued_once_at_threshold", func(t *testing.T) {
		resetHealLog(t)
		healThresholdHome(t, "3")
		healN("sess-notice", 2)
		if consumePendingSanitizerHealNotice("sess-notice") != "" {
			t.Fatal("notice before the threshold")
		}
		healN("sess-notice", 1)
		if consumePendingSanitizerHealNotice("sess-notice") == "" {
			t.Fatal("no notice at the threshold")
		}
		if consumePendingSanitizerHealNotice("sess-notice") != "" {
			t.Fatal("notice delivered twice")
		}
	})
	t.Run("test_notice_never_rearms_in_new_window", func(t *testing.T) {
		resetHealLog(t)
		healThresholdHome(t, "2")
		healN("sess-rearm", 2)
		if consumePendingSanitizerHealNotice("sess-rearm") == "" {
			t.Fatal("no notice at the threshold")
		}
		emptyHealLogLock.Lock()
		emptyHealLogState["sess-rearm"].windowStart -= emptyHealWindowS + 1
		emptyHealLogLock.Unlock()
		healN("sess-rearm", 2)
		if consumePendingSanitizerHealNotice("sess-rearm") != "" {
			t.Fatal("notice re-armed in a new window")
		}
	})
	t.Run("test_notice_scoped_to_session", func(t *testing.T) {
		resetHealLog(t)
		healThresholdHome(t, "2")
		healN("sess-x", 2)
		if consumePendingSanitizerHealNotice("sess-y") != "" {
			t.Fatal("another session stole the notice")
		}
		if consumePendingSanitizerHealNotice("sess-x") == "" {
			t.Fatal("notice lost")
		}
	})
	t.Run("test_threshold_zero_disables_escalation", func(t *testing.T) {
		resetHealLog(t)
		healThresholdHome(t, "0")
		records := captureLogger(t, "run_agent", slog.LevelWarn)
		healN("sess-off", 6)
		if consumePendingSanitizerHealNotice("sess-off") != "" {
			t.Fatal("notice with escalation disabled")
		}
		if _, e := countLevels(records()); e != 0 {
			t.Fatalf("errors %d", e)
		}
	})
	t.Run("test_threshold_read_from_config", func(t *testing.T) {
		healThresholdHome(t, "7")
		if got := healEscalationThreshold(context.Background()); got != 7 {
			t.Fatalf("threshold %d", got)
		}
	})
	t.Run("test_threshold_defaults_when_config_unreadable", func(t *testing.T) {
		h := t.TempDir()
		t.Setenv("ICEBATH_HOME", h)
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("agent: [unclosed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := healEscalationThreshold(context.Background()); got != emptyHealEscalateAfter {
			t.Fatalf("threshold %d", got)
		}
	})
}

func TestHealStatsSurface(t *testing.T) {
	t.Run("test_counters_visible_and_escalation_flagged", func(t *testing.T) {
		resetHealLog(t)
		healThresholdHome(t, "3")
		healN("sess-stats", 4)
		st := getSanitizerHealStats()["sess-stats"]
		if st["heal_events"] != 4 || st["messages_healed"] != 4 || st["escalated"] != true {
			t.Fatalf("stats %v", st)
		}
	})
}

// Real agent on a loopback provider that answers ok and records every request's messages
func healLoopAgent(t *testing.T, status func(kind, text string)) (*AIAgent, context.Context, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var last []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "test-model", "object": "model"}}})
			return
		}
		var body struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		last = body.Messages
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl-1", "object": "chat.completion", "model": "test-model",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}}})
	}))
	t.Cleanup(srv.Close)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	env := &tools.Env{Home: h}
	ctx := tools.WithToolEnv(context.Background(), env)
	o := AIAgentOptions{Model: "test-model", APIKey: "test-key-1234567890", BaseURL: srv.URL + "/v1", QuietMode: true,
		SkipContextFiles: true, SkipMemory: true, Cwd: h, Env: env}
	if status != nil {
		o.StatusCallback = status
	}
	a, err := NewAIAgent(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	a.DisableStreaming()
	a.cachedSystemPrompt = "You are helpful."
	a.usePromptCaching = false
	a.compressionEnabled = false
	a.saveTrajectories = false
	return a, ctx, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

func TestProjectionStopsReheal(t *testing.T) {
	t.Run("test_unmarked_empty_assistant_is_filled_before_sanitizer", func(t *testing.T) {
		resetHealLog(t)
		a, ctx, wire := healLoopAgent(t, nil)
		history := []map[string]any{
			{"role": "user", "content": "start"},
			{"role": "assistant", "content": ""},
			{"role": "user", "content": "continue", "finish_reason": "stop"},
			{"role": "assistant", "content": "earlier reply", "finish_reason": "stop"},
		}
		if _, err := a.RunConversation(ctx, "next question", RunConversationOptions{ConversationHistory: history}); err != nil {
			t.Fatal(err)
		}
		sent := wire()
		if len(sent) == 0 {
			t.Fatal("no request reached the provider")
		}
		if stats := getSanitizerHealStats(); len(stats) != 0 {
			t.Fatalf("sanitizer still healed an empty non-final row %v", stats)
		}
		var assistants []map[string]any
		for _, m := range sent {
			if m["role"] == "assistant" {
				assistants = append(assistants, m)
			}
		}
		if len(assistants) == 0 || assistants[0]["content"] != InterruptedPlaceholder {
			t.Fatalf("wire assistants %v", assistants)
		}
		if _, ok := history[1]["api_content"]; history[1]["content"] != "" || ok {
			t.Fatalf("durable history mutated %v", history[1])
		}
	})
	t.Run("test_pending_notice_delivered_out_of_band_not_in_context", func(t *testing.T) {
		resetHealLog(t)
		var warned []string
		a, ctx, wire := healLoopAgent(t, func(kind, text string) {
			if kind == "warn" {
				warned = append(warned, text)
			}
		})
		key := orDash(a.sessionID)
		emptyHealLogLock.Lock()
		emptyHealUserNotified[key] = true
		emptyHealPendingNotice[key] = "⚠️ Your session transcript required repeated repair — run /debug share or `icebath doctor`."
		emptyHealLogLock.Unlock()
		history := []map[string]any{{"role": "user", "content": "start"}, {"role": "assistant", "content": "earlier", "finish_reason": "stop"}}
		if _, err := a.RunConversation(ctx, "next", RunConversationOptions{ConversationHistory: history}); err != nil {
			t.Fatal(err)
		}
		if len(warned) == 0 || !strings.Contains(warned[0], "repeated repair") {
			t.Fatalf("warned %v", warned)
		}
		emptyHealLogLock.Lock()
		pending := len(emptyHealPendingNotice)
		emptyHealLogLock.Unlock()
		if pending != 0 {
			t.Fatal("notice not drained")
		}
		for _, m := range append(wire(), history...) {
			if strings.Contains(fmt.Sprint(m["content"]), "repeated repair") {
				t.Fatalf("notice reached the context %v", m)
			}
		}
	})
}

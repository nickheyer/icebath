package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/relayhost/relaytest"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	atofSession = "compaction-atof-session"
	atofSecret  = "TOPSECRET_TRANSCRIPT_TEXT"
	atofLLMName = "openai.chat_completions"
)

// Summary the compressor's generator returns, abort failing it with abort_on_summary_failure set
type atofSummary struct {
	text  *string
	abort bool
}

func atofTranscript() []map[string]any {
	filler := strings.TrimSpace(strings.Repeat("context ", 200))
	msgs := []map[string]any{{"role": "system", "content": "system prompt"}}
	for idx := range 10 {
		msgs = append(msgs, map[string]any{"role": "user", "content": fmt.Sprintf("user message %d %s %s", idx, atofSecret, filler)})
		msgs = append(msgs, map[string]any{"role": "assistant", "content": fmt.Sprintf("assistant reply %d %s %s", idx, atofSecret, filler)})
	}
	return msgs
}

func atofLLM(t *testing.T, ctx context.Context, messages []map[string]any, requestID string) {
	t.Helper()
	session := atofSession
	request := map[string]any{"model": "test/main-model", "messages": toAnySlice(messages)}
	_, err := provider.RelayExecute(ctx, request, func(context.Context, map[string]any) (any, error) {
		return map[string]any{"id": "r", "object": "chat.completion", "created": int64(0), "model": "test/main-model",
			"choices": []any{map[string]any{"index": int64(0), "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "ok"}}}}, nil
	}, provider.RelayExecuteOptions{Name: atofLLMName, ModelName: "test/main-model", SessionID: &session,
		Metadata: map[string]any{"api_mode": "chat_completions", "api_request_id": requestID}})
	if err != nil {
		t.Fatal(err)
	}
}

func toAnySlice(messages []map[string]any) []any {
	out := make([]any, len(messages))
	for i, m := range messages {
		out[i] = m
	}
	return out
}

func atofPluginsTOML(t *testing.T, dir, atofDir string) string {
	t.Helper()
	quoted, _ := json.Marshal(atofDir)
	path := filepath.Join(dir, "plugins.toml")
	body := "version = 1\n\n[[components]]\nkind = \"observability\"\nenabled = true\n\n[components.config]\nversion = 4\n\n" +
		"[components.config.atof]\nenabled = true\n\n[[components.config.atof.sinks]]\ntype = \"file\"\noutput_directory = " +
		string(quoted) + "\nfilename = \"events.jsonl\"\nmode = \"overwrite\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// One Relay turn of two calls, a compression attempt and a last call, returning ATOF events and request size
func atofRunTurnWithCompaction(t *testing.T, summary atofSummary) ([]map[string]any, int) {
	t.Helper()
	tmp := t.TempDir()
	icebathHome, atofDir := filepath.Join(tmp, "icebath-home"), filepath.Join(tmp, "atof")
	for _, d := range []string{icebathHome, atofDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", icebathHome)
	t.Setenv(tools.RelayPluginsConfigEnv, atofPluginsTOML(t, tmp, atofDir))
	relaytest.Use(t)
	tools.ResetRelayRuntimeForTests()

	window := 100_000
	compressor := newCompressorWith(t, window, func(o *ContextCompressorOptions) {
		o.Model, o.Provider, o.ThresholdPercent, o.ConfigContextLength = "test/main-model", "test-provider", 0.50, &window
		o.AbortOnSummaryFailure = summary.abort
	})
	compressor.SetTailTokenBudget(10)
	a := spAgent(t)
	a.contextCompressor, a.sessionID, a.platform, a.model, a.provider = compressor, atofSession, "cli", "test/main-model", "test-provider"
	a.compressionFeasibilityChecked, a.compressionInPlace = true, true
	transcript := atofTranscript()
	ctx := pyctx.With(context.Background(), pyctx.New())
	profileKey := tools.CurrentRelayProfileKey(ctx)
	var lastRequest []map[string]any
	func() {
		defer tools.ResetRelayRuntimeForTests()
		lease := tools.RelayCoordinator.AcquireConversation(ctx, tools.RelayConversation{ProfileKey: profileKey, SessionID: atofSession, Platform: "cli"})
		turn, err := tools.RelayCoordinator.BeginTurn(ctx, lease, "turn-1", "task-1", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			tools.RelayCoordinator.EndTurn(ctx, turn, "success")
			tools.RelayCoordinator.FinalizeConversation(ctx, profileKey, atofSession)
		}()
		atofLLM(t, ctx, transcript[:len(transcript)-2], "r1")
		atofLLM(t, ctx, transcript, "r2")
		prev := compressorGenerateSummary
		compressorGenerateSummary = func(*ContextCompressor, context.Context, []map[string]any, *string, string, bool) (*string, error) {
			return summary.text, nil
		}
		system, approx := "system prompt", 80_000
		compressed, _, err := compressContext(ctx, a, transcript, &system, compressContextOptions{ApproxTokens: &approx})
		compressorGenerateSummary = prev
		if err != nil {
			t.Fatal(err)
		}
		lastRequest = append(append([]map[string]any{}, compressed...), map[string]any{"role": "user", "content": "next question"})
		atofLLM(t, ctx, lastRequest, "r3")
	}()
	raw, err := os.ReadFile(filepath.Join(atofDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatal(err)
		}
		events = append(events, ev)
	}
	return events, len(lastRequest)
}

func atofLLMStartMessageCounts(events []map[string]any) []int {
	var counts []int
	for _, ev := range events {
		if ev["name"] == atofLLMName && ev["scope_category"] == "start" {
			data, _ := ev["data"].(map[string]any)
			body := data
			if content, ok := data["content"].(map[string]any); ok && content != nil {
				body = content
			}
			msgs, _ := body["messages"].([]any)
			counts = append(counts, len(msgs))
		}
	}
	return counts
}

func atofCompactionMarks(events []map[string]any) []map[string]any {
	var out []map[string]any
	for _, ev := range events {
		name, _ := ev["name"].(string)
		if ev["kind"] == "mark" && strings.HasPrefix(name, "compaction") {
			out = append(out, ev)
		}
	}
	return out
}

func TestCommittedCompactionMarkResetsFreshnessForTheNextLLMStart(t *testing.T) {
	sanitized := "SANITIZED SUMMARY"
	for _, tc := range []struct {
		summary *string
		method  string
	}{{&sanitized, "llm_summary"}, {nil, "deterministic_fallback"}} {
		t.Run(tc.method, func(t *testing.T) {
			events, lastRequestLen := atofRunTurnWithCompaction(t, atofSummary{text: tc.summary})
			marks := atofCompactionMarks(events)
			if len(marks) != 1 {
				t.Fatalf("compaction marks %v", marks)
			}
			mark := marks[0]
			if mark["name"] != "compaction" {
				t.Fatalf("mark name %v", mark["name"])
			}
			if schema, _ := json.Marshal(mark["data_schema"]); string(schema) != `{"name":"icebath.compaction","version":"1"}` {
				t.Fatalf("data_schema %s", schema)
			}
			data, _ := mark["data"].(map[string]any)
			if data["kind"] != "summarize" || data["outcome"] != "committed" || data["method"] != tc.method {
				t.Fatalf("mark data %v", data)
			}
			var turnStarts []map[string]any
			for _, ev := range events {
				if ev["name"] == tools.RelayTurnScope && ev["scope_category"] == "start" {
					turnStarts = append(turnStarts, ev)
				}
			}
			if len(turnStarts) != 1 || mark["parent_uuid"] != turnStarts[0]["uuid"] {
				t.Fatalf("mark parent %v, turn starts %v", mark["parent_uuid"], turnStarts)
			}
			raw, _ := json.Marshal(mark)
			if strings.Contains(string(raw), atofSecret) || strings.Contains(string(raw), "SANITIZED SUMMARY") {
				t.Fatalf("mark leaks transcript text %s", raw)
			}
			counts := atofLLMStartMessageCounts(events)
			if len(counts) != 3 {
				t.Fatalf("llm starts %v", counts)
			}
			if counts[1] >= len(atofTranscript()) {
				t.Fatalf("second start %d not projected below %d", counts[1], len(atofTranscript()))
			}
			if counts[2] != lastRequestLen {
				t.Fatalf("start after compaction %d, want full history %d", counts[2], lastRequestLen)
			}
		})
	}
}

func TestFailedAttemptIsAnAttemptMarkAndLeavesFreshnessAlone(t *testing.T) {
	events, lastRequestLen := atofRunTurnWithCompaction(t, atofSummary{abort: true})
	marks := atofCompactionMarks(events)
	if len(marks) != 1 || marks[0]["name"] != "compaction.attempt" {
		t.Fatalf("compaction marks %v", marks)
	}
	data, _ := marks[0]["data"].(map[string]any)
	if data["outcome"] != "failed" || data["failure_class"] != "summary_generation_aborted" {
		t.Fatalf("attempt mark data %v", data)
	}
	counts := atofLLMStartMessageCounts(events)
	if len(counts) == 0 || counts[len(counts)-1] >= lastRequestLen {
		t.Fatalf("llm starts %v, last request %d", counts, lastRequestLen)
	}
}

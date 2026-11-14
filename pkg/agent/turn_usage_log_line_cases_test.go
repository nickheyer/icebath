package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func usageLineAgent(t *testing.T) (*AIAgent, context.Context) {
	t.Helper()
	h := caseHome(t)
	env := &tools.Env{Home: h, Cwd: h}
	ctx := tools.WithToolEnv(context.Background(), env)
	a, err := NewAIAgent(ctx, AIAgentOptions{APIKey: "k", BaseURL: "https://inference-api.nousresearch.com/v1", Provider: "nous", APIMode: "chat_completions",
		Model: "anthropic/claude-fable-5.1", SessionID: "t", Platform: "cli", QuietMode: true, SkipContextFiles: true, SkipMemory: true,
		EnabledToolsets: []string{"file"}, Cwd: h, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	a.saveTrajectories = false
	return a, ctx
}

func usageOf(read, write, prompt int) map[string]any {
	return map[string]any{"prompt_tokens": prompt, "completion_tokens": 7, "total_tokens": prompt + 7,
		"prompt_tokens_details": map[string]any{"cached_tokens": read, "cache_write_tokens": write}, "completion_tokens_details": nil}
}

func usageLine(t *testing.T, a *AIAgent, ctx context.Context, response map[string]any) string {
	t.Helper()
	records := captureLogger(t, "agent.conversation_loop", slog.LevelInfo)
	a.recordResponseUsage(ctx, response, rowsToMessages([]map[string]any{{"role": "user", "content": "hi"}}), 1, 0.2, 0, 3)
	for _, r := range records() {
		if strings.HasPrefix(r.msg, "API call #") {
			return r.msg
		}
	}
	t.Fatalf("no API call line in %v", records())
	return ""
}

type usageCapturingEngine struct {
	ContextEngine
	seen map[string]any
}

func (e *usageCapturingEngine) UpdateFromResponse(usage map[string]any) {
	for k, v := range usage {
		e.seen[k] = v
	}
}

func TestTurnUsageLogLine(t *testing.T) {
	t.Run("test_write_id_and_upstream_are_on_the_line", func(t *testing.T) {
		a, ctx := usageLineAgent(t)
		line := usageLine(t, a, ctx, map[string]any{"usage": usageOf(34_283, 28_604, 62_889), "id": "gen-1788636728-qMa1SbYZcwjrvUzJuF1g",
			"provider": "Anthropic", "model": "anthropic/claude-fable-5.1"})
		for _, want := range []string{" cache=34283/62889 (55%)", " write=28604", " id=gen-1788636728-qMa1SbYZcwjrvUzJuF1g", " upstream=Anthropic"} {
			if !strings.Contains(line, want) {
				t.Fatalf("%q missing from %q", want, line)
			}
		}
		if !strings.HasPrefix(line, "API call #1: model=anthropic/claude-fable-5.1 provider=nous in=62889 out=7 total=62896 latency=0.2s") {
			t.Fatalf("line prefix changed %q", line)
		}
	})
	t.Run("test_fields_are_omitted_when_absent", func(t *testing.T) {
		a, ctx := usageLineAgent(t)
		line := usageLine(t, a, ctx, map[string]any{"usage": usageOf(0, 0, 100), "id": nil, "model": "anthropic/claude-fable-5.1"})
		if strings.Contains(line, "write=") || strings.Contains(line, "id=") || strings.Contains(line, "upstream=") {
			t.Fatalf("absent fields on the line %q", line)
		}
	})
	t.Run("test_forensics_parser_reads_the_new_fields", func(t *testing.T) {
		log := filepath.Join(t.TempDir(), "agent.log")
		text := "2026-09-06 19:32:08,549 INFO [s1] agent.conversation_loop: API call #3: model=anthropic/claude-fable-5.1 provider=nous " +
			"in=62889 out=7 total=62896 latency=0.2s cache=34283/62889 (55%) write=28604 id=gen-1788636728-qMa1 upstream=Claude Platform on AWS\n" +
			"2026-09-06 19:32:09,549 INFO [s1] agent.conversation_loop: API call #4: model=anthropic/claude-fable-5.1 provider=nous " +
			"in=91079 out=7 total=91086 latency=0.2s cache=62887/91079 (69%)\n"
		if err := os.WriteFile(log, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		calls := ParseAPICallLogs([]string{log, filepath.Join(t.TempDir(), "absent.log")}, map[string]bool{"s1": true})
		if len(calls) != 2 || calls[0]["n"] != 3 || calls[1]["n"] != 4 {
			t.Fatalf("calls %v", calls)
		}
		if calls[0]["write"] != 28604 || calls[0]["id"] != "gen-1788636728-qMa1" || calls[0]["upstream"] != "Claude Platform on AWS" {
			t.Fatalf("first call %v", calls[0])
		}
		if _, ok := calls[1]["write"]; ok {
			t.Fatalf("second call %v", calls[1])
		}
		if _, ok := calls[1]["id"]; ok {
			t.Fatalf("second call %v", calls[1])
		}
		want := map[string]any{"ts": "2026-09-06 19:32:09,549", "sid": "s1", "n": 4, "model": "anthropic/claude-fable-5.1", "inp": 91079, "out": 7, "lat": 0.2, "hit": 62887}
		if !reflect.DeepEqual(calls[1], want) {
			t.Fatalf("second call %v", calls[1])
		}
		if got := ParseAPICallLogs([]string{log}, map[string]bool{"other": true}); len(got) != 0 {
			t.Fatalf("foreign session calls %v", got)
		}
	})
	t.Run("test_long_decode_context_buckets_use_the_first_hop", func(t *testing.T) {
		a, ctx := usageLineAgent(t)
		usage := usageOf(32_840, 0, 32_968)
		usage["initial_prompt_tokens"], usage["initial_cached_tokens"] = 100, 40
		engine := &usageCapturingEngine{ContextEngine: a.contextCompressor, seen: map[string]any{}}
		a.contextCompressor = engine
		line := usageLine(t, a, ctx, map[string]any{"usage": usage, "id": nil, "model": "anthropic/claude-fable-5.1"})
		if !strings.Contains(line, " in=32968 ") {
			t.Fatalf("the line keeps the billed sum, got %q", line)
		}
		if engine.seen["prompt_tokens"] != 100 || engine.seen["input_tokens"] != 60 || engine.seen["cache_read_tokens"] != 40 {
			t.Fatalf("context buckets %v", engine.seen)
		}
	})
}

func TestInvalidResponseStampsReasonFromEmbeddedProviderCode(t *testing.T) {
	caseHome(t)
	a := bareSteerAgent()
	a.provider, a.model, a.maxIterations, a.suppressStatusOutput = "openrouter", "gpt-5-turbo", 30, true
	a.setBaseURL("https://openrouter.ai/api/v1")
	response := sdkInvalidResponse{Error: sdkResponseError{Code: 429, Metadata: map[string]any{"provider_name": "Acme"}}, Choices: []any{}}
	s := &LoopState{Response: response, Retry: &TurnRetryState{}, RetryCount: 2, MaxRetries: 3, APICallCount: 1, APIRequestID: "r",
		APIDuration: 0.4, EffectiveTaskID: "t", TurnID: "turn"}
	verdict, err := retryInvalidResponse(context.Background(), a, s, []string{"no choices"})
	if err != nil {
		t.Fatal(err)
	}
	final, _ := verdict.Result["final_response"].(string)
	if verdict.Action != "return" || verdict.Result["failure_reason"] != "rate_limit" || !strings.Contains(final, "Acme") {
		t.Fatalf("action %q result %v", verdict.Action, verdict.Result)
	}
}

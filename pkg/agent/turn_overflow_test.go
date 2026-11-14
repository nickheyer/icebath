package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Recovery over a gpt-5 agent with three of three compression attempts spent
func newOverflowRecovery(t *testing.T) *overflowRecovery {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := &AIAgent{model: "gpt-5"}
	s := &LoopState{EffectiveTaskID: "t", APICallCount: 2, MaxCompressionAttempts: 3, ApproxTokens: 200_000, CompressionAttempts: 3, Retry: &TurnRetryState{}}
	return &overflowRecovery{ctx: context.Background(), a: a, s: s, retry: s.Retry}
}

func TestOverflowExhaustionIsNonRetryableContextOverflowWithSlashCommands(t *testing.T) {
	v, err := newOverflowRecovery(t).countAttempt(false)
	if err != nil || v == nil {
		t.Fatalf("no terminal verdict: %v", err)
	}
	result := v.Result
	if result["failed"] != true || result["compression_exhausted"] != true {
		t.Fatalf("result %v", result)
	}
	if result["failure_reason"] != "context_overflow" || result["failure_retryable"] != false {
		t.Fatalf("verdict %v %v", result["failure_reason"], result["failure_retryable"])
	}
	text, _ := result["final_response"].(string)
	if !strings.Contains(text, "/new") || !strings.Contains(text, "/compress") {
		t.Fatalf("copy %q", text)
	}
	if code := buildErrorSurfaceFromResult(result, "", "")["code"]; code != "context_overflow" {
		t.Fatalf("surface code %v", code)
	}
}

// Drives context length recovery with a request the rough estimator prices at requestTokens against a window
func contextRejection(t *testing.T, requestTokens, window int, errorMsg, baseURL string) (PhaseVerdict, int) {
	t.Helper()
	r := newOverflowRecovery(t)
	r.s.CompressionAttempts = 0
	r.a.provider, r.a.contextCompressor = "lmstudio", newStubEngine(window, 0.5)
	r.a.setBaseURL(baseURL)
	compressed := 0
	estimates := []int{requestTokens, 10}
	savedCompress, savedRequest, savedMessages, savedSleep := overflowCompressContext, overflowEstimateRequest, overflowEstimateMessages, recoverySleep
	t.Cleanup(func() {
		overflowCompressContext, overflowEstimateRequest, overflowEstimateMessages, recoverySleep = savedCompress, savedRequest, savedMessages, savedSleep
	})
	overflowCompressContext = func(*AIAgent, context.Context, []map[string]any, *string, compressContextOptions) ([]map[string]any, string, error) {
		compressed++
		return []map[string]any{{"role": "user", "content": "x"}}, "", nil
	}
	overflowEstimateRequest = func(context.Context, []map[string]any, string, []map[string]any) int { return requestTokens }
	overflowEstimateMessages = func(context.Context, []*state.Message) int {
		n := estimates[0]
		estimates = estimates[1:]
		return n
	}
	recoverySleep = func(time.Duration) {}
	v, err := recoverContextLength(r, errorMsg)
	if err != nil {
		t.Fatal(err)
	}
	return v, compressed
}

func TestContextRejectionFarBelowTheWindowIsNotBlamedOnTheConversation(t *testing.T) {
	v, compressed := contextRejection(t, 3_000, 65_536, "HTTP 500: Context size has been exceeded.", "http://127.0.0.1:1234/v1")
	result := v.Result
	if v.Action != "return" || compressed != 0 {
		t.Fatalf("action %q compressed %d", v.Action, compressed)
	}
	if result["failed"] != true || pyTruthy(result["compression_exhausted"]) {
		t.Fatalf("result %v", result)
	}
	if result["failure_reason"] != "server_error" || result["failure_retryable"] != true {
		t.Fatalf("verdict %v %v", result["failure_reason"], result["failure_retryable"])
	}
	text, _ := result["final_response"].(string)
	if strings.Contains(text, "grown too long") || strings.Contains(text, "/new") {
		t.Fatalf("blamed the conversation: %q", text)
	}
	for _, want := range []string{"3,000", "65,536", "background", "/retry"} {
		if !strings.Contains(text, want) {
			t.Fatalf("copy lacks %q: %q", want, text)
		}
	}
}

func TestContextRejectionNearTheWindowStillCompresses(t *testing.T) {
	if v, compressed := contextRejection(t, 60_000, 65_536, "HTTP 500: Context size has been exceeded.", "http://127.0.0.1:1234/v1"); compressed == 0 || v.Action != "break" {
		t.Fatalf("near window: action %q compressed %d", v.Action, compressed)
	}
	if v, compressed := contextRejection(t, 3_000, 65_536, "prompt is too long: 70000 tokens > 65536 maximum", "http://127.0.0.1:1234/v1"); compressed == 0 || v.Action != "break" {
		t.Fatalf("server quoted count: action %q compressed %d", v.Action, compressed)
	}
}

func TestHostedContextRejectionFarBelowTheKnownWindowCompresses(t *testing.T) {
	v, compressed := contextRejection(t, 47_000, 1_000_000, "This model's maximum context length was exceeded. Please reduce the length of the messages.", "https://api.anthropic.com")
	if compressed == 0 || v.Action != "break" {
		t.Fatalf("hosted: action %q compressed %d", v.Action, compressed)
	}
}

func TestEndpointRejectionHintReachesAnyRoute(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Cleanup(provider.SnapshotProviderRegistryForTests())
	provider.RegisterProvider(provider.Profile{Name: "overflow-hint-acme", BaseURL: "https://api.acme.example/v1", Hostnames: []string{"acme.example"},
		RequestRejectionHintHook: func(statusCode int, baseURL string) []string {
			if statusCode == 413 && strings.Contains(baseURL, "acme.example") {
				return []string{"capped"}
			}
			return nil
		}})
	status := 413
	if got := requestRejectionHint(context.Background(), &status, "https://api.acme.example/v1"); !reflect.DeepEqual(got, []string{"capped"}) {
		t.Fatalf("413 hint %v", got)
	}
	status = 400
	if got := requestRejectionHint(context.Background(), &status, "https://api.acme.example/v1"); len(got) != 0 {
		t.Fatalf("400 hint %v", got)
	}
}

func TestProviderConfirmedWritersSurviveRestart(t *testing.T) {
	for _, baseURL := range []string{"", "https://bedrock-runtime.us-east-1.amazonaws.com"} {
		for _, writer := range []string{"overflow", "usage"} {
			t.Run(writer, func(t *testing.T) {
				t.Setenv("ICEBATH_HOME", t.TempDir())
				t.Setenv("AWS_REGION", "us-east-1")
				ctx := context.Background()
				model := "global.xai.grok-4.6"
				o := DefaultContextCompressorOptions(model)
				o.BaseURL, o.Provider, o.QuietMode = baseURL, "bedrock", true
				compressor := NewContextCompressor(ctx, o)
				compressor.SetContextLength(500_000)
				a := &AIAgent{model: model, provider: "bedrock", apiMode: "bedrock", contextCompressor: compressor, quietMode: true}
				a.setBaseURL(baseURL)
				if writer == "overflow" {
					if got, ok := adoptProviderContextLimit(ctx, a, "maximum context length is 96000 tokens", 500_000); !ok || got != 96_000 {
						t.Fatalf("adopted %d %v", got, ok)
					}
					if !compressor.contextProbed || !compressor.contextProbePersistable {
						t.Fatal("provider limit not flagged as probed")
					}
				} else {
					compressor.SetContextLength(96_000)
					compressor.contextProbed, compressor.contextProbePersistable = true, true
					response := map[string]any{"usage": map[string]any{"input_tokens": 100, "output_tokens": 5}}
					a.recordResponseUsage(ctx, response, []*state.Message{{Role: "user", Content: "hi"}}, 1, 0.1, 0, 3)
					if compressor.contextProbed || compressor.contextProbePersistable {
						t.Fatal("usage record must clear the probe flags")
					}
				}
				cacheURL := baseURL
				if cacheURL == "" {
					cacheURL = "bedrock://"
				}
				if got, ok := provider.GetCachedContextLength(ctx, model, cacheURL, true); !ok || got != 96_000 {
					t.Fatalf("cached %d %v", got, ok)
				}
				if got := provider.GetModelContextLength(ctx, provider.ContextLengthRequest{Model: model, BaseURL: baseURL, Provider: "bedrock"}); got != 96_000 {
					t.Fatalf("resolved after restart %d", got)
				}
			})
		}
	}
}

func TestRetryBufferClassificationSurvivesTerminalFlush(t *testing.T) {
	for _, policy := range []*bool{nil, new(false), new(true)} {
		for _, shape := range []string{"message_count", "token_count", "payload_bytes"} {
			home := t.TempDir()
			t.Setenv("ICEBATH_HOME", home)
			t.Setenv("ICEBATH_MANAGED_DIR", filepath.Join(home, "managed"))
			cfg := "{}"
			if policy != nil {
				cfg = "display:\n  suppress_warning_notifications: " + map[bool]string{true: "true", false: "false"}[*policy] + "\n"
			}
			if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			suppressed := policy != nil && *policy
			var printed []string
			a := &AIAgent{model: "fixture", platform: "cli", logPrefix: ""}
			a.printFn = func(s string) { printed = append(printed, s) }
			a.statusCallback = func(kind, text string) {}
			rows := []map[string]any{{"role": "user", "content": strings.Repeat("a", 4000)}, {"role": "assistant", "content": strings.Repeat("b", 4000)}}
			compressedRows := []map[string]any{rows[0]}
			if shape != "message_count" {
				compressedRows = []map[string]any{{"role": "user", "content": strings.Repeat("a", 10)}, {"role": "assistant", "content": strings.Repeat("b", 10)}}
			}
			saved, savedSleep := overflowCompressContext, recoverySleep
			t.Cleanup(func() { overflowCompressContext, recoverySleep = saved, savedSleep })
			overflowCompressContext = func(*AIAgent, context.Context, []map[string]any, *string, compressContextOptions) ([]map[string]any, string, error) {
				return compressedRows, "system", nil
			}
			recoverySleep = func(time.Duration) {}
			system := "system"
			s := &LoopState{Messages: rowsToMessages(rows), APIMessages: anyRows(rows), SystemMessage: &system, ActiveSystemPrompt: "system",
				ConversationHistory: []*state.Message{}, ApproxTokens: 2000, EffectiveTaskID: "offline", APICallCount: 1, MaxCompressionAttempts: 3, Retry: &TurnRetryState{}}
			r := &overflowRecovery{ctx: context.Background(), a: a, s: s, retry: s.Retry}
			if shape == "payload_bytes" {
				if _, err := recoverPayloadTooLarge(r); err != nil {
					t.Fatal(err)
				}
				if !s.Retry.RestartWithCompressedMessages {
					t.Fatalf("%v %s: restart not armed", policy, shape)
				}
			} else {
				_, shrank, _, err := r.compressScoredByTokens(2000, false)
				if err != nil || !shrank {
					t.Fatalf("%v %s: shrank=%v err=%v", policy, shape, shrank, err)
				}
			}
			if len(a.retryStatusBuffer) == 0 {
				t.Fatalf("%v %s: nothing buffered", policy, shape)
			}
			for _, line := range a.retryStatusBuffer {
				if _, ok := line.message.(config.DiagnosticText); !ok {
					t.Fatalf("%v %s: buffered %T", policy, shape, line.message)
				}
			}
			if a.liveSessionMessages() != nil {
				t.Fatalf("%v %s: persisted before the terminal failure", policy, shape)
			}
			v, err := r.failTurn("Requested task failed.", overflowFailure{notices: []string{"diagnostic terminal detail"}, log: "offline terminal failure"})
			if err != nil {
				t.Fatal(err)
			}
			if v.Result["failed"] != true || v.Result["final_response"] != "Requested task failed." {
				t.Fatalf("%v %s: result %v", policy, shape, v.Result)
			}
			if len(a.liveSessionMessages()) != len(s.Messages) || a.liveSessionMessages() == nil {
				t.Fatalf("%v %s: terminal failure not persisted", policy, shape)
			}
			if len(a.retryStatusBuffer) != 0 {
				t.Fatalf("%v %s: buffer not flushed", policy, shape)
			}
			if (len(printed) > 0) == suppressed {
				t.Fatalf("%v %s: printed %q", policy, shape, printed)
			}
		}
	}
}

// Output cap compression errors fall back to the max tokens retry while a keyboard interrupt ends the turn
func TestOutputCapCompressionErrorRetriesButInterruptPropagates(t *testing.T) {
	for _, tc := range []struct {
		err       error
		interrupt bool
	}{{errors.New("summary model down"), false}, {tools.ErrKeyboardInterrupt, true}} {
		r := newOverflowRecovery(t)
		r.s.CompressionAttempts = 0
		r.a.contextCompressor = newStubEngine(100_000, 0.5)
		saved, savedRequest := overflowCompressContext, overflowEstimateRequest
		overflowCompressContext = func(*AIAgent, context.Context, []map[string]any, *string, compressContextOptions) ([]map[string]any, string, error) {
			return nil, "", tc.err
		}
		overflowEstimateRequest = func(context.Context, []map[string]any, string, []map[string]any) int { return 1000 }
		v, err := clampOutputCap(r, 4096, 100_000)
		overflowCompressContext, overflowEstimateRequest = saved, savedRequest
		if tc.interrupt {
			if !errors.Is(err, tools.ErrKeyboardInterrupt) {
				t.Fatalf("interrupt swallowed: %v %v", v, err)
			}
			continue
		}
		if err != nil || v.Action != "break" || !r.retry.RestartWithCompressedMessages || r.a.ephemeralMaxOutputTokens != 4032 {
			t.Fatalf("verdict %v err %v restart %v cap %d", v, err, r.retry.RestartWithCompressedMessages, r.a.ephemeralMaxOutputTokens)
		}
	}
}

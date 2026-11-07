package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

type sdkResponseError struct {
	Code     any            `json:"code"`
	Message  any            `json:"message"`
	Metadata map[string]any `json:"metadata"`
}

type sdkInvalidResponse struct {
	Error   any    `json:"error"`
	Model   string `json:"model"`
	Choices []any  `json:"choices"`
}

func TestCodexFinishReasonRoutesOutputCapAndFilterStops(t *testing.T) {
	cases := []struct {
		response any
		want     string
	}{
		{map[string]any{"status": " Incomplete ", "incomplete_details": map[string]any{"reason": "max_output_tokens"}}, "incomplete"},
		{map[string]any{"status": "incomplete", "incomplete_details": map[string]any{"reason": "LENGTH"}}, "incomplete"},
		{map[string]any{"status": "incomplete", "incomplete_details": map[string]any{"reason": "content_filter"}}, "content_filter"},
		{map[string]any{"status": "incomplete"}, "stop"},
		{map[string]any{"status": "completed", "incomplete_details": map[string]any{"reason": "max_output_tokens"}}, "stop"},
		{nil, "stop"},
	}
	for _, c := range cases {
		if got := codexFinishReason(c.response); got != c.want {
			t.Fatalf("%v: got %q want %q", c.response, got, c.want)
		}
	}
}

func TestFailureHintForCodesAndResponseTimes(t *testing.T) {
	code := func(n int64) *int64 { return &n }
	cases := []struct {
		code     *int64
		duration float64
		want     string
	}{
		{code(524), 100.4, "upstream provider timed out (Cloudflare 524, 100s)"},
		{code(504), 30, "upstream gateway timeout (504, 30s)"},
		{code(429), 1, "rate limited by upstream provider (429)"},
		{code(502), 2, "upstream server error (502, 2s)"},
		{code(529), 2, "upstream provider overloaded (529)"},
		{code(418), 3, "upstream error (code 418, 3s)"},
		{nil, 0.4, "fast response (0.4s) — likely rate limited"},
		{nil, 75, "slow response (75s) — likely upstream timeout"},
		{nil, 20, "response time 20.0s"},
	}
	for _, c := range cases {
		if got := failureHintFor(c.code, c.duration); got != c.want {
			t.Fatalf("got %q want %q", got, c.want)
		}
	}
}

func TestDescribeInvalidResponseReadsTheEmbeddedProviderError(t *testing.T) {
	a := &AIAgent{}
	typed := sdkInvalidResponse{Error: sdkResponseError{Code: 429, Metadata: map[string]any{"provider_name": "Acme"}}, Choices: []any{}}
	msg, name, hint := describeInvalidResponse(a, typed, 0.4)
	if name != "Acme" || hint != "rate limited by upstream provider (429)" || !strings.Contains(msg, "429") {
		t.Fatalf("typed error: %q %q %q", msg, name, hint)
	}
	raw := map[string]any{"error": map[string]any{"code": "503", "message": "busy", "metadata": map[string]any{"provider_name": "Acme"}}, "model": "m1"}
	msg, name, hint = describeInvalidResponse(a, raw, 2)
	if msg != "busy" || name != "model=m1" || hint != "upstream provider overloaded (503)" {
		t.Fatalf("dict error: %q %q %q", msg, name, hint)
	}
	msg, name, hint = describeInvalidResponse(a, nil, 30)
	if msg != "Unknown" || name != "Unknown" || hint != "response time 30.0s" {
		t.Fatalf("no response: %q %q %q", msg, name, hint)
	}
}

func TestValidateResponseShapeReadsCodexStatusAndChatChoices(t *testing.T) {
	codex := &AIAgent{apiMode: "codex_responses", provider: "openai-codex"}
	failed := map[string]any{"status": "failed", "output": []any{}, "error": map[string]any{"code": "usage_limit_reached", "message": "You've hit your usage limit."}}
	if invalid, details, err := validateResponseShape(codex, failed); err != nil || !invalid || details[0] != "response.status=failed: You've hit your usage limit." {
		t.Fatalf("failed status: %v %v %v", invalid, details, err)
	}
	if invalid, details, _ := validateResponseShape(codex, map[string]any{"status": "cancelled", "output": []any{}}); !invalid || details[0] != "response.status=cancelled: Responses API returned status 'cancelled'" {
		t.Fatalf("cancelled status: %v", details)
	}
	if invalid, _, _ := validateResponseShape(codex, map[string]any{"status": "completed", "output": []any{}, "output_text": " recovered "}); invalid {
		t.Fatal("output_text must defer to normalization")
	}
	if invalid, details, _ := validateResponseShape(codex, map[string]any{"status": "completed", "output": []any{}}); !invalid || details[0] != "response.output is empty" {
		t.Fatalf("empty output: %v", details)
	}
	chat := &AIAgent{apiMode: "chat_completions"}
	for _, c := range []struct {
		response map[string]any
		want     string
	}{
		{map[string]any{"id": "x"}, "response has no 'choices' attribute"},
		{map[string]any{"choices": nil}, "response.choices is None"},
		{map[string]any{"choices": []any{}}, "response.choices is empty"},
	} {
		if invalid, details, _ := validateResponseShape(chat, c.response); !invalid || details[0] != c.want {
			t.Fatalf("%v: %v", c.response, details)
		}
	}
	if invalid, details, _ := validateResponseShape(chat, nil); !invalid || details[0] != "response is None" {
		t.Fatalf("nil response: %v", details)
	}
}

func TestClassifyCodexSoftFailureReadsTheQuotaBody(t *testing.T) {
	a := &AIAgent{apiMode: "codex_responses", provider: "openai-codex", model: "gpt-5.1-codex", baseURL: "https://chatgpt.com/backend-api/codex"}
	response := map[string]any{"status": "failed", "error": sdkResponseError{Code: "usage_limit_reached", Message: "You've hit your usage limit. Try again at 3:00 PM."}}
	classified, errCtx := classifyCodexSoftFailure(a, response)
	if classified == nil || (classified.Reason != provider.ReasonRateLimit && classified.Reason != provider.ReasonBilling) {
		t.Fatalf("quota soft failure must reach the pool: %+v", classified)
	}
	if errCtx["reason"] != "usage_limit_reached" {
		t.Fatalf("error context: %v", errCtx)
	}
	if c, ctx := classifyCodexSoftFailure(a, map[string]any{"status": "completed"}); c != nil || len(ctx) != 0 {
		t.Fatal("a completed response is no soft failure")
	}
	if c, _ := classifyCodexSoftFailure(&AIAgent{apiMode: "chat_completions"}, response); c != nil {
		t.Fatal("only the Codex wire reports soft failures")
	}
}

func TestInterruptibleBackoffSleepHonoursRedirectAndAbort(t *testing.T) {
	saved := recoverySleep
	recoverySleep = func(time.Duration) {}
	t.Cleanup(func() { recoverySleep = saved })

	a := newFixtureAgent(t, AIAgentOptions{BaseURL: "https://openrouter.ai/api/v1", Model: "test/model"})
	a.interruptRequested, a.pendingRedirect = true, "use the other file"
	retry := &TurnRetryState{}
	msgs := []*state.Message{{Role: "user", Content: "go"}}
	result, err := interruptibleBackoffSleep(context.Background(), a, 30, retry, &msgs, nil, 2, "abort", "stopped", "retry backoff (1/3)")
	if err != nil || result != nil || !retry.RestartWithRedirectedMessages || a.pendingRedirect != "use the other file" {
		t.Fatalf("redirect: %v %v %+v %q", result, err, retry, a.pendingRedirect)
	}

	a.interruptRequested, a.pendingRedirect = true, ""
	msgs = []*state.Message{{Role: "user", Content: "go"}, {Role: "tool", Content: "out", ToolCallID: "c1"}}
	result, err = interruptibleBackoffSleep(context.Background(), a, 30, &TurnRetryState{}, &msgs, nil, 2, "abort", "Operation interrupted during retry.", "retry backoff (1/3)")
	if err != nil || result["interrupted"] != true || result["final_response"] != "Operation interrupted during retry." || result["api_calls"] != 2 {
		t.Fatalf("abort: %v %v", result, err)
	}
	if last := msgs[len(msgs)-1]; last.Role != "assistant" || last.Content != "Operation interrupted during retry." || a.Interrupted() {
		t.Fatalf("abort must close the tool sequence and clear the interrupt: %+v", last)
	}
}

func TestCheckAPIResponseFoldsAValidReplyAndLeavesTheRetryLoop(t *testing.T) {
	a := newFixtureAgent(t, AIAgentOptions{BaseURL: "https://openrouter.ai/api/v1", Model: "test/model"})
	s := &LoopState{
		Response: map[string]any{"model": "test/model", "choices": []any{map[string]any{
			"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "hello"},
		}}},
		Retry: &TurnRetryState{HasRetried429: true}, APIStartTime: recoveryNow(), APICallCount: 1, MaxRetries: 3,
		FinishReason: "stop", TruncatedResponseParts: []TruncatedPart{},
	}
	verdict, err := checkAPIResponse(context.Background(), a, s)
	if err != nil || verdict.Action != "break" {
		t.Fatalf("verdict %+v %v", verdict, err)
	}
	if s.FinishReason != "stop" || s.Retry.HasRetried429 || !a.turnReceivedProviderResponse {
		t.Fatalf("state after a valid reply: %q %+v %v", s.FinishReason, s.Retry, a.turnReceivedProviderResponse)
	}
}

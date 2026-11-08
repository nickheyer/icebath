package agent

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Provider HTTP error carrying a status, message and response headers
type ladderTestErr struct {
	*provider.APIError
	headers http.Header
}

func (e *ladderTestErr) Unwrap() error                { return e.APIError }
func (e *ladderTestErr) ResponseHeaders() http.Header { return e.headers }

func ladderErr(status int, message string, headers map[string]string) *ladderTestErr {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &ladderTestErr{APIError: &provider.APIError{Provider: "custom", Model: "m", StatusCode: status, Message: message}, headers: h}
}

// Fixture agent recording the status and wait surfaces, with the ladder wait answering sleepOutcome
type ladderAgent struct {
	*AIAgent
	statuses, waits []string
	sleepOutcome    map[string]any
}

func newLadderAgent(t *testing.T, fallback []map[string]any, cycles int, platform string) *ladderAgent {
	t.Helper()
	la := &ladderAgent{}
	la.AIAgent = newFixtureAgent(t, AIAgentOptions{Provider: "custom", BaseURL: "http://127.0.0.1:9/v1", Model: "m", Platform: platform, FallbackModel: fallback})
	la.autoRecoveryCycles = cycles
	la.statusCallback = func(kind, text string) { la.statuses = append(la.statuses, text) }
	la.thinkingCallback = func(text string) { la.waits = append(la.waits, text) }
	savedSleep, savedNonretryable, savedExhausted := ladderBackoffSleep, settleNonretryableResult, settleExhaustedResult
	t.Cleanup(func() {
		ladderBackoffSleep, settleNonretryableResult, settleExhaustedResult = savedSleep, savedNonretryable, savedExhausted
	})
	ladderBackoffSleep = func(context.Context, *AIAgent, float64, *TurnRetryState, *[]*state.Message, []*state.Message, int, string, string, string) (map[string]any, error) {
		return la.sleepOutcome, nil
	}
	settleNonretryableResult = func(context.Context, *AIAgent, *LoopState, *apiErrorTurn, string) (map[string]any, error) {
		return map[string]any{"failed": true, "nonretryable": true}, nil
	}
	settleExhaustedResult = func(context.Context, *AIAgent, *LoopState, *apiErrorTurn, int, string) (map[string]any, error) {
		return map[string]any{"failed": true}, nil
	}
	return la
}

// Settles an exhausted error the way the retry loop hands it over, three of three retries spent
func settleLadder(t *testing.T, la *ladderAgent, apiErr *ladderTestErr, retry *TurnRetryState) (PhaseVerdict, *LoopState) {
	t.Helper()
	if retry == nil {
		retry = &TurnRetryState{PrimaryRecoveryAttempted: true}
	}
	cl := provider.ClassifyError(provider.ClassifyInput{Err: apiErr, Status: apiErr.StatusCode, Headers: apiErr.headers, Provider: la.provider, Model: "m"})
	status := apiErr.StatusCode
	s := &LoopState{Retry: retry, RetryCount: 3, MaxRetries: 3, APICallCount: 1, CurrentTurnUserIdx: -1}
	e := &apiErrorTurn{apiError: apiErr, classified: cl, statusCode: &status, errorContext: map[string]any{},
		errorMsg: strings.ToLower(apiErr.Error()), providerName: la.provider, baseURL: "http://x", model: "m"}
	v, err := settleUnrecoveredError(context.Background(), la.AIAgent, s, e)
	if err != nil {
		t.Fatal(err)
	}
	return v, s
}

func TestTransientExhaustionWithoutFallbackEntersLadderAndRetries(t *testing.T) {
	for _, c := range []struct {
		status  int
		message string
	}{{503, "Service temporarily unavailable"}, {529, "Overloaded"}} {
		la := newLadderAgent(t, nil, 5, "cli")
		v, s := settleLadder(t, la, ladderErr(c.status, c.message, nil), nil)
		if v.Action != "continue" || s.RetryCount != 0 || s.Retry.AutoRecoveryCyclesUsed != 1 {
			t.Fatalf("%d: verdict %q retry_count %d cycles %d", c.status, v.Action, s.RetryCount, s.Retry.AutoRecoveryCyclesUsed)
		}
		if len(la.statuses) != 1 || len(la.waits) != 1 || la.statuses[0] != la.waits[0] {
			t.Fatalf("%d: statuses %q waits %q", c.status, la.statuses, la.waits)
		}
		if !strings.Contains(la.statuses[0], "(cycle 1/5)") {
			t.Fatalf("%d: line %q", c.status, la.statuses[0])
		}
	}
}

func TestLadderIsBoundedAndYieldsToFallbackAndShunsNonretryable(t *testing.T) {
	spentAgent := newLadderAgent(t, nil, 5, "cli")
	v, _ := settleLadder(t, spentAgent, ladderErr(503, "down", nil), &TurnRetryState{PrimaryRecoveryAttempted: true, AutoRecoveryCyclesUsed: 5})
	if v.Action != "return" || len(v.Result) != 1 || v.Result["failed"] != true {
		t.Fatalf("spent ladder: %q %v", v.Action, v.Result)
	}
	gaveUp := false
	for _, line := range spentAgent.statuses {
		gaveUp = gaveUp || strings.Contains(line, "gave up after 5 cycles")
	}
	if !gaveUp {
		t.Fatalf("no give up line in %q", spentAgent.statuses)
	}

	fb := newLadderAgent(t, []map[string]any{{"provider": "custom", "model": "fallback-m", "base_url": "http://127.0.0.1:9/fallback/v1", "api_key": "fb-key"}}, 5, "cli")
	v, s := settleLadder(t, fb, ladderErr(503, "down", nil), nil)
	if v.Action != "break" || !fb.fallbackActivated || fb.model != "fallback-m" || s.Retry.AutoRecoveryCyclesUsed != 0 {
		t.Fatalf("fallback first: %q activated=%v model=%q cycles=%d", v.Action, fb.fallbackActivated, fb.model, s.Retry.AutoRecoveryCyclesUsed)
	}

	auth := newLadderAgent(t, nil, 5, "cli")
	v, s = settleLadder(t, auth, ladderErr(401, "Incorrect API key provided", nil), nil)
	if v.Result["nonretryable"] != true || s.Retry.AutoRecoveryCyclesUsed != 0 {
		t.Fatalf("auth: %v cycles=%d", v.Result, s.Retry.AutoRecoveryCyclesUsed)
	}
	streamed := newLadderAgent(t, nil, 5, "cli")
	streamed.streamedAssistantTextParts = []string{"Here is the first half of the answer"}
	v, s = settleLadder(t, streamed, ladderErr(503, "down", nil), nil)
	if v.Action != "return" || s.Retry.AutoRecoveryCyclesUsed != 0 {
		t.Fatalf("streamed: %q cycles=%d", v.Action, s.Retry.AutoRecoveryCyclesUsed)
	}

	off := newLadderAgent(t, nil, 0, "cli")
	v, s = settleLadder(t, off, ladderErr(503, "down", nil), nil)
	if v.Action != "return" || s.Retry.AutoRecoveryCyclesUsed != 0 || len(off.statuses) != 0 {
		t.Fatalf("disabled: %q cycles=%d statuses=%q", v.Action, s.Retry.AutoRecoveryCyclesUsed, off.statuses)
	}
}

func TestLadderScheduleHonoursRetryAfterAndPlatformStopHint(t *testing.T) {
	type call struct {
		attempt   int
		base, max time.Duration
	}
	var calls []call
	saved := ladderJitteredBackoff
	t.Cleanup(func() { ladderJitteredBackoff = saved })
	ladderJitteredBackoff = func(attempt int, opts provider.BackoffOptions) time.Duration {
		calls = append(calls, call{attempt, opts.BaseDelay, opts.MaxDelay})
		return min(opts.BaseDelay*time.Duration(1<<(attempt-1)), opts.MaxDelay)
	}

	plain := ladderErr(503, "down", nil)
	var waits []float64
	for _, c := range []int{1, 2, 3, 4, 5} {
		waits = append(waits, ladderWaitSeconds(c, plain))
	}
	if want := []float64{15, 30, 60, 60, 60}; len(waits) != 5 || waits[0] != want[0] || waits[1] != want[1] || waits[2] != want[2] || waits[3] != want[3] || waits[4] != want[4] {
		t.Fatalf("schedule %v", waits)
	}
	if calls[0] != (call{1, 15 * time.Second, 60 * time.Second}) {
		t.Fatalf("first call %v", calls[0])
	}
	for header, want := range map[string]float64{"4": 4, "90": 90, "900": 120} {
		if got := ladderWaitSeconds(1, ladderErr(529, "Overloaded", map[string]string{"Retry-After": header})); got != want {
			t.Fatalf("Retry-After %s: got %v want %v", header, got, want)
		}
	}

	tg := newLadderAgent(t, nil, 5, "telegram")
	settleLadder(t, tg, plain, nil)
	if len(tg.statuses) == 0 || !strings.Contains(tg.statuses[0], "send /stop to cancel") {
		t.Fatalf("telegram: %q", tg.statuses)
	}
	cron := newLadderAgent(t, nil, 5, "cron")
	settleLadder(t, cron, plain, nil)
	if len(cron.statuses) == 0 || !strings.HasSuffix(cron.statuses[0], "(cycle 1/5)") {
		t.Fatalf("cron: %q", cron.statuses)
	}
}

func TestInterruptDuringLadderWaitReturnsInterruptedResult(t *testing.T) {
	la := newLadderAgent(t, nil, 5, "cli")
	la.sleepOutcome = map[string]any{"interrupted": true, "completed": false}
	v, s := settleLadder(t, la, ladderErr(503, "down", nil), nil)
	if v.Action != "return" || v.Result["interrupted"] != true {
		t.Fatalf("verdict %q %v", v.Action, v.Result)
	}
	if s.Retry.AutoRecoveryCyclesUsed != 1 {
		t.Fatalf("cycles %d", s.Retry.AutoRecoveryCyclesUsed)
	}
}

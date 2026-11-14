package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

type refusedResponseError struct {
	error
	headers http.Header
}

func (e refusedResponseError) ResponseHeaders() http.Header { return e.headers }

func TestBlockedResponseCapture(t *testing.T) {
	t.Run("test_handle_api_error_captures_only_a_nous_window_refusal", blockedCaptureRows)
}

func blockedCaptureRows(t *testing.T) {
	for _, tc := range []struct {
		provider string
		status   int
		headers  string
		captured bool
	}{
		{"nous", 402, "window", true}, {"nous", 403, "window", true}, {"openrouter", 402, "window", false},
		{"nous", 402, "grant", false}, {"nous", 500, "window", false},
	} {
		t.Run(fmt.Sprintf("%s %d %s", tc.provider, tc.status, tc.headers), func(t *testing.T) {
			caseHome(t)
			t.Cleanup(provider.SwapCreditsWarmForTests(func(context.Context) {}))
			a := newFixtureAgent(t, AIAgentOptions{Provider: "openai-compat", BaseURL: "http://127.0.0.1:11434/v1", Model: "glm", APIKey: "test-key-1234567890", EnabledToolsets: []string{}})
			t.Cleanup(func() { a.Close() })
			a.provider, a.baseURL, a.credits = tc.provider, "https://inference-api.nousresearch.com/v1", nil
			gauges, _ := json.Marshal(refusalGauges(10_000, 900, 0, 0))
			h := http.Header{}
			for k, v := range map[string]string{
				"x-nous-credits-version": "1", "x-nous-credits-remaining-micros": "0", "x-nous-credits-remaining-usd": "0.00",
				"x-nous-credits-subscription-micros": "0", "x-nous-credits-subscription-usd": "0.00", "x-nous-credits-rollover-micros": "0",
				"x-nous-credits-purchased-micros": "0", "x-nous-credits-purchased-usd": "0.00", "x-nous-credits-paid-access": "true",
				"x-nous-credits-as-of-ms": fmt.Sprint(refusalNowMs), "x-nous-credits-denominator-kind": "usage_window",
				"x-nous-credits-window-gauges": string(gauges),
			} {
				h.Set(k, v)
			}
			if tc.headers == "grant" {
				h.Set("x-nous-credits-denominator-kind", "subscription_cap")
			}
			apiErr := errors.Join(&provider.APIError{Provider: tc.provider, Model: "test/model", StatusCode: tc.status, Message: "refused", Err: tools.ErrPoolShutdown},
				refusedResponseError{errors.New("refused"), h})
			s := &LoopState{Retry: &TurnRetryState{}, MaxRetries: 1, MaxCompressionAttempts: 1, APICallCount: 1, APIRequestID: "r", EffectiveTaskID: "t", TurnID: "u"}
			verdict, err := handleAPIError(context.Background(), a, s, apiErr)
			if err != nil || verdict.Action != "return" {
				t.Fatalf("verdict %+v err %v", verdict, err)
			}
			st := a.creditsCapture().Credits()
			if (st != nil) != tc.captured {
				t.Fatalf("captured state %+v want captured %v", st, tc.captured)
			}
			if st != nil && (!st.IsUsageWindow() || st.Unmetered || st.SpendSource != "") {
				t.Fatalf("capture not marked refused %+v", st)
			}
		})
	}
}

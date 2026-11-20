package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Settles one unrecovered error on an agent whose fallback chain holds one entry that activates
func settleCase(t *testing.T, err error, prov, model, baseURL string, status int, retry *TurnRetryState) (*AIAgent, PhaseVerdict) {
	t.Helper()
	caseHome(t)
	savedSleep := recoverySleep
	recoverySleep = func(time.Duration) {}
	t.Cleanup(func() { recoverySleep = savedSleep })
	a := newFixtureAgent(t, AIAgentOptions{Provider: "openai-compat", BaseURL: baseURL, Model: model, APIKey: "test-key-1234567890", EnabledToolsets: []string{}})
	t.Cleanup(func() { a.Close() })
	a.provider, a.persistDisabled, a.credentialPool = prov, true, nil
	a.fallbackChain, a.fallbackIndex = []map[string]any{{"provider": "openrouter", "model": "gpt-4o-mini", "api_key": "sk-or-x"}}, 0
	e := &apiErrorTurn{apiError: err, classified: provider.ClassifyError(provider.ClassifyInput{Err: err, Status: status, Provider: prov, Model: model}),
		errorContext: map[string]any{}, errorMsg: strings.ToLower(err.Error()), providerName: prov, baseURL: baseURL, model: model}
	if status != 0 {
		e.statusCode = &status
	}
	if retry == nil {
		retry = &TurnRetryState{}
	}
	s := &LoopState{APIMessages: []any{}, ApproxTokens: 10, MaxRetries: 3, APICallCount: 1, Retry: retry}
	verdict, verr := settleUnrecoveredError(context.Background(), a, s, e)
	if verr != nil {
		t.Fatal(verr)
	}
	return a, verdict
}

func TestClientErrorSettlementSkipsFallbackWhenClassifierSaysSo(t *testing.T) {
	err := &provider.APIError{Provider: "ollama", Model: "glm", StatusCode: 400, Message: "invalid tool call arguments",
		Body: `{"error": {"message": "invalid tool call arguments", "type": "invalid_request_error"}}`}
	a, verdict := settleCase(t, err, "ollama", "glm", "http://127.0.0.1:11434/v1", 400, nil)
	if verdict.Action != "return" || verdict.Result["failure_reason"] != string(provider.ReasonFormatError) {
		t.Fatalf("verdict %+v", verdict)
	}
	if a.fallbackIndex != 0 {
		t.Fatalf("fallback walked to %d", a.fallbackIndex)
	}
}

func TestFallbackFreeVerdictWinsOverLocalValueerrorShape(t *testing.T) {
	a, verdict := settleCase(t, &provider.MoAPresetNotFoundError{Preset: "old"}, "moa", "glm", "http://127.0.0.1:11434/v1", 0, nil)
	if verdict.Action != "return" || a.fallbackIndex != 0 {
		t.Fatalf("missing preset verdict %+v fallback index %d", verdict, a.fallbackIndex)
	}
	a, verdict = settleCase(t, &provider.LocalValidationError{Message: "some local bug"}, "openai", "glm", "http://127.0.0.1:11434/v1", 0, nil)
	if verdict.Action != "break" || a.fallbackIndex != 1 {
		t.Fatalf("local error verdict %+v fallback index %d", verdict, a.fallbackIndex)
	}
}

func TestSpentDisableDropFallsBackInsteadOfReplayingTheRequest(t *testing.T) {
	message := "reasoning_effort 'none' unsupported; use minimal|low|medium|high|xhigh"
	newErr := func() error {
		return &provider.APIError{Provider: "custom", Model: "chat-only", StatusCode: 400,
			Message: "Error code: 400 - {'error': {'message': \"" + message + "\"}}",
			Body:    `{"error": {"message": "` + message + `", "type": "invalid_request_error"}}`}
	}
	a, verdict := settleCase(t, newErr(), "custom", "chat-only", "http://relay.example/v1", 400, &TurnRetryState{ReasoningMandatoryRetryAttempted: true})
	if verdict.Action != "break" || a.fallbackIndex != 1 {
		t.Fatalf("spent drop verdict %+v fallback index %d", verdict, a.fallbackIndex)
	}
	a, verdict = settleCase(t, newErr(), "custom", "chat-only", "http://relay.example/v1", 400, &TurnRetryState{})
	if verdict.Action != "fallthrough" || a.fallbackIndex != 0 {
		t.Fatalf("control verdict %+v fallback index %d", verdict, a.fallbackIndex)
	}
}

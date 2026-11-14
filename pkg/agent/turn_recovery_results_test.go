package agent

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

func classifiedFor(err error, status int, providerName, model string) *provider.Classified {
	return provider.ClassifyError(provider.ClassifyInput{Err: err, Status: status, Provider: providerName, Model: model, ContextLength: 200000})
}

func TestNonretryable401ResultClassifiesAsAuthForTheUi(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a, _ := quietAgent()
	a.provider, a.model, a.persistDisabled = "nous", "m", true
	err := &statusHeaderErr{msg: "HTTP 401: User not found.", status: 401}
	cl := classifiedFor(err, 401, "nous", "m")
	status := 401
	e := &apiErrorTurn{apiError: err, classified: cl, statusCode: &status, errorContext: map[string]any{},
		providerName: "nous", baseURL: "https://inference-api.nousresearch.com/v1", model: "m"}
	s := &LoopState{APIMessages: []any{}, APICallCount: 1, ApproxTokens: 10, Retry: &TurnRetryState{}}
	result, rerr := nonretryableClientErrorResult(t.Context(), a, s, e, "")
	if rerr != nil {
		t.Fatal(rerr)
	}
	if result["failure_reason"] != string(cl.Reason) || result["failure_retryable"] != false || cl.Retryable {
		t.Fatalf("verdict %v %v", result["failure_reason"], result["failure_retryable"])
	}
	surface := buildErrorSurfaceFromResult(result, "nous", "m")
	if surface["layer"] != LayerAuth || surface["retryable"] != false || surface["auth_kind"] != "oauth" {
		t.Fatalf("surface %v", surface)
	}
}

const deliveredPartial = "Here is the first half of the report, already shown to the user."

func TestTerminalErrorKeepsPartialAndCollapsesThisTurnsTrail(t *testing.T) {
	for _, tc := range []struct {
		status  int
		message string
	}{
		{401, "HTTP 401: invalid api key"},
	} {
		t.Run(strings.SplitN(tc.message, ":", 2)[0], func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			a, _ := quietAgent()
			a.provider, a.model, a.baseURL, a.persistDisabled = "openrouter", "m", "https://openrouter.ai/api/v1", true
			messages := rowsOf(
				map[string]any{"role": "user", "content": "earlier turn"},
				map[string]any{"role": "assistant", "content": "stale", "_length_continuation_fragment": true},
				map[string]any{"role": "user", "content": "write a long report"},
				map[string]any{"role": "assistant", "content": deliveredPartial, "_length_continuation_fragment": true},
				map[string]any{"role": "user", "content": "continue where you left off", "_length_continuation_nudge": true},
			)
			err := &statusHeaderErr{msg: tc.message, status: tc.status}
			status := tc.status
			e := &apiErrorTurn{apiError: err, classified: classifiedFor(err, tc.status, "openrouter", "m"), statusCode: &status,
				errorContext: map[string]any{}, errorMsg: strings.ToLower(tc.message), isRateLimited: tc.status == 429,
				providerName: "openrouter", baseURL: "https://openrouter.ai/api/v1", model: "m"}
			s := &LoopState{Messages: messages, APIMessages: []any{}, ApproxTokens: 10, RetryCount: 3, MaxRetries: 3,
				APICallCount: 3, CurrentTurnUserIdx: 2, Retry: &TurnRetryState{PrimaryRecoveryAttempted: true}}
			verdict, verr := settleUnrecoveredError(t.Context(), a, s, e)
			if verr != nil || verdict.Action != "return" {
				t.Fatalf("verdict %v err %v", verdict.Action, verr)
			}
			result := verdict.Result
			final, _ := result["final_response"].(string)
			if result["partial"] != true || result["failed"] != true || !strings.Contains(final, deliveredPartial) || strings.Contains(final, "stale") {
				t.Fatalf("result %v", result)
			}
			if strings.TrimSpace(final) == strings.TrimSpace(result["error"].(string)) {
				t.Fatal("final response equals the error")
			}
			var roles []string
			for _, m := range s.Messages {
				roles = append(roles, m.Role)
			}
			if strings.Join(roles, ",") != "user,assistant,user,assistant" || s.Messages[1].Content != "stale" {
				t.Fatalf("roles %v", roles)
			}
			if d := state.MessageToDict(s.Messages[3]); d["content"] != deliveredPartial || d["finish_reason"] != "error" {
				t.Fatalf("delivered row %v", d)
			}
		})
	}
}

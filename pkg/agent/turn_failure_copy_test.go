package agent

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func handoffResult(override map[string]any) map[string]any {
	out := map[string]any{
		"final_response":   "checkpoint: two of three steps done",
		"completed":        false,
		"failed":           false,
		"interrupted":      false,
		"turn_exit_reason": "max_iterations_reached(3/3)",
	}
	maps.Copy(out, override)
	return out
}

func TestOnlyANonFailedCappedTurnWithASummaryIsAHandoff(t *testing.T) {
	cases := []struct {
		override map[string]any
		expected bool
	}{
		{map[string]any{}, true},
		{map[string]any{"failed": true}, false},
		{map[string]any{"interrupted": true}, false},
		{map[string]any{"completed": true}, false},
		{map[string]any{"turn_exit_reason": "budget_exhausted"}, false},
		{map[string]any{"turn_exit_reason": "error_near_max_iterations(3/3)"}, false},
		{map[string]any{"final_response": "   "}, false},
	}
	for _, c := range cases {
		if got := IsMaxIterationHandoff(handoffResult(c.override)); got != c.expected {
			t.Fatalf("override %v: got %v want %v", c.override, got, c.expected)
		}
	}
	if IsMaxIterationHandoff("max_iterations_reached(3/3)") {
		t.Fatal("a non dict result is never a handoff")
	}
	if IsMaxIterationHandoff(handoffResult(map[string]any{"completed": nil})) {
		t.Fatal("completed must be exactly False")
	}
}

func TestFailedTurnNoticeNeverClaimsNotProcessedAfterATool(t *testing.T) {
	if got := FailedTurnNoticeFor(nil); got != FailedTurnNotice {
		t.Fatalf("empty turn: %q", got)
	}
	plain := []map[string]any{{"role": "user", "content": "hi"}, {"role": "assistant", "content": "x", "tool_calls": []any{}}}
	if got := FailedTurnNoticeFor(plain); got != FailedTurnNotice {
		t.Fatalf("no tool ran: %q", got)
	}
	called := []any{map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1"}}}}
	if got := FailedTurnNoticeFor(called); got != PartialFailedTurnNotice {
		t.Fatalf("tool call: %q", got)
	}
	tool := []any{"junk", map[string]any{"role": "tool", "content": "ok"}}
	if got := FailedTurnNoticeFor(tool); got != PartialFailedTurnNotice {
		t.Fatalf("tool row: %q", got)
	}
}

func TestUntypedFailedTurnDisplayKindMatchesExactNotices(t *testing.T) {
	for _, notice := range []string{FailedTurnNotice, PartialFailedTurnNotice, AutomationInterruptedNotice} {
		if kind, ok := UntypedFailedTurnDisplayKind("assistant", "  "+notice+"\n"); !ok || kind != FailedTurnDisplayKind {
			t.Fatalf("notice %q not typed", notice)
		}
	}
	if _, ok := UntypedFailedTurnDisplayKind("assistant", "Quote: "+FailedTurnNotice); ok {
		t.Fatal("a reply quoting the notice stays a reply")
	}
	if _, ok := UntypedFailedTurnDisplayKind("user", FailedTurnNotice); ok {
		t.Fatal("only assistant rows are boundaries")
	}
}

func TestExitReasonFailurePrefixTable(t *testing.T) {
	got, ok := ExitReasonFailure("local_processing_error(KeyError)")
	if !ok || got != (ExitFailure{Reason: "loop_error", Retryable: false, FailsTurn: false}) {
		t.Fatalf("local processing: %+v %v", got, ok)
	}
	got, ok = ExitReasonFailure("all_retries_exhausted_no_response")
	if !ok || got != (ExitFailure{Reason: "server_error", Retryable: true, FailsTurn: true}) {
		t.Fatalf("exhausted: %+v", got)
	}
	if _, ok := ExitReasonFailure(nil); ok {
		t.Fatal("None carries no verdict")
	}
	if _, ok := ExitReasonFailure("max_iterations_reached(3/3)"); ok {
		t.Fatal("the iteration cap is not a failure")
	}
}

type fakeResponse struct{ Error any }
type fakeAPIError struct{ Code any }

func TestInvalidResponseFailureReasonReadsEmbeddedCode(t *testing.T) {
	cases := []struct {
		response any
		want     string
	}{
		{nil, "invalid_response"},
		{fakeResponse{}, "invalid_response"},
		{fakeResponse{Error: fakeAPIError{Code: 429}}, "rate_limit"},
		{fakeResponse{Error: map[string]any{"code": "503"}}, "overloaded"},
		{fakeResponse{Error: map[string]any{"code": 524.0}}, "timeout"},
		{fakeResponse{Error: map[string]any{"code": "abc"}}, "invalid_response"},
		{fakeResponse{Error: map[string]any{"code": 418}}, "invalid_response"},
		{map[string]any{"error": map[string]any{"code": 429}}, "rate_limit"},
	}
	for _, c := range cases {
		if got := InvalidResponseFailureReason(c.response); got != c.want {
			t.Fatalf("%#v: got %q want %q", c.response, got, c.want)
		}
	}
}

func TestStampFailureSetsVerdictInPlace(t *testing.T) {
	result := map[string]any{"final_response": "x"}
	if out := StampFailure(result, "loop_error", true); out["failure_reason"] != "loop_error" || result["failure_retryable"] != true {
		t.Fatalf("stamp: %v", result)
	}
}

func TestFailureCauseGlossSubstitutesTheAsker(t *testing.T) {
	if got, ok := tools.FailureCauseGloss("model_not_found", "the job", "the job's"); !ok || got != "the model the job uses was not found at the AI model service" {
		t.Fatalf("model_not_found: %q", got)
	}
	if got, _ := tools.FailureCauseGloss(provider.ReasonContextOverflow, "it", "its"); got != "its request grew too large for the model" {
		t.Fatalf("context overflow: %q", got)
	}
	if _, ok := tools.FailureCauseGloss(nil, "it", "its"); ok {
		t.Fatal("None has no gloss")
	}
	if _, ok := tools.FailureCauseGloss("format_error", "it", "its"); ok {
		t.Fatal("format_error is not provider shaped")
	}
}

func TestSiteCopyDefaultsMissingFieldsAndFormatsNumbers(t *testing.T) {
	ctx := context.Background()
	got, err := tools.SiteCopy(ctx, "server_context_rejection", map[string]any{"tokens": 12345, "window": 131072, "model": "m"})
	if err != nil || !strings.Contains(got, "about 12,345 tokens — well under the 131,072-token window Icebath knows for m") {
		t.Fatalf("server rejection: %q %v", got, err)
	}
	got, err = tools.SiteCopy(ctx, "loop_error", nil)
	if err != nil || !strings.HasSuffix(got, "\n\nDetails: ") {
		t.Fatalf("loop error: %q %v", got, err)
	}
	got, _ = tools.SiteCopy(ctx, "interpreter_shutdown", map[string]any{"resume": " with /resume abc"})
	if !strings.Contains(got, "reopen it with /resume abc and send") {
		t.Fatalf("interpreter shutdown: %q", got)
	}
	if _, err := tools.SiteCopy(ctx, "nope", nil); err == nil || err.Error() != "'nope'" {
		t.Fatalf("unknown code: %v", err)
	}
	if _, err := tools.SiteCopy(ctx, "server_context_rejection", nil); err == nil {
		t.Fatal("a thousands separator over the empty default raises ValueError")
	}
}

func TestExhaustedCopyNamesResetWindowAndLifetimeCap(t *testing.T) {
	got := ExhaustedCopy("rate_limit", "OpenRouter", 3, "429", nil, nil)
	want := "OpenRouter rate-limited every one of 3 attempts — it looks temporarily unavailable. Wait a minute and send /retry, or switch models with /model. To avoid this in future, add a backup provider with `icebath fallback add`.\n\nProvider said: 429"
	if got != want {
		t.Fatalf("plain: %q", got)
	}
	reset := 30960.0
	if got := ExhaustedCopy("rate_limit", "Nous Portal", 2, "s", &reset, nil); !strings.Contains(got, "its usage limit resets in ~9h. Send /retry after that") {
		t.Fatalf("reset: %q", got)
	}
	short := 90.0
	if got := ExhaustedCopy("rate_limit", "X", 1, "s", &short, nil); !strings.HasPrefix(got, "X is rate-limiting requests right now — its usage limit resets in ~2 min.") {
		t.Fatalf("first attempt: %q", got)
	}
	if got := ExhaustedCopy("weird", "X", 2, "s", &short, nil); !strings.HasPrefix(got, "X didn't answer after 2 attempts — it looks temporarily unavailable.") {
		t.Fatalf("default lead: %q", got)
	}
	cut := &provider.StreamLifetimeExceededError{Elapsed: 1801, Cap: 1800}
	if got := ExhaustedCopy("timeout", "X", 3, "s", nil, cut); !strings.HasPrefix(got, "The response was still streaming when agent.stream_max_lifetime (1800s) cut it, on all 3 attempts.") {
		t.Fatalf("lifetime: %q", got)
	}
}

func TestLimitResetCopyNamesLocalTimeAndWait(t *testing.T) {
	now := 1_700_000_000.0
	resets := now + 2*3600 + 5*60 + 1
	clock := time.Unix(int64(resets), 0).Local().Format("15:04")
	if got := LimitResetCopy(resets, &now); got != "Limit resets at "+clock+" (in 2h 06m)." {
		t.Fatalf("hours: %q", got)
	}
	soon := now + 61
	if got := LimitResetCopy(soon, &now); !strings.HasSuffix(got, "(in 2m).") {
		t.Fatalf("minutes: %q", got)
	}
	past := now - 1
	if got := LimitResetCopy(past, &now); got != "" {
		t.Fatalf("passed: %q", got)
	}
	prevNow := failureCopyNow
	failureCopyNow = func() float64 { return now }
	t.Cleanup(func() { failureCopyNow = prevNow })
	if got := LimitResetCopy(now+30, nil); !strings.HasSuffix(got, "(in 1m).") {
		t.Fatalf("default clock: %q", got)
	}
}

func TestNonretryableCopyPicksTemplateByVerdict(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	got, err := NonretryableCopy(ctx, &provider.Classified{Reason: provider.ReasonModelNotFound}, "openrouter", "claude-x", "404", "anthropic/claude-x")
	if err != nil || !strings.Contains(got, "Model 'claude-x' isn't available on ") || !strings.Contains(got, "did you mean 'anthropic/claude-x'?") || !strings.HasSuffix(got, "\n\nProvider said: 404") {
		t.Fatalf("model not found: %q %v", got, err)
	}
	got, _ = NonretryableCopy(ctx, &provider.Classified{Reason: provider.ReasonAuth}, "openrouter", nil, "401", "")
	if !strings.Contains(got, "rejected your API key") {
		t.Fatalf("api key auth: %q", got)
	}
	got, _ = NonretryableCopy(ctx, &provider.Classified{Reason: provider.ReasonAuthPermanent}, "nous", nil, "401", "")
	if !strings.Contains(got, "Sign in again: `icebath portal`.") {
		t.Fatalf("oauth auth: %q", got)
	}
	got, _ = NonretryableCopy(ctx, &provider.Classified{Reason: provider.ReasonUnknown}, "openrouter", nil, "400", "")
	if !strings.Contains(got, "rejected the request and retrying won't help") || !strings.Contains(got, "/logs/agent.log`") {
		t.Fatalf("default: %q", got)
	}
	if got := ContentPolicyCopy("OpenRouter", "blocked"); got != "OpenRouter's safety filter refused this request, so the model didn't answer. "+provider.ContentPolicyNextSteps+"\n\nProvider said: blocked" {
		t.Fatalf("content policy: %q", got)
	}
}

type launchRefusal struct {
	Field  string
	Detail string
	Data   map[string]any
}

func (launchRefusal) Error() string { return "launch_credentials_unavailable" }

func TestLaunchCredentialsCopyNamesTheField(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_LANGUAGE", "en")
	ctx := context.Background()
	got := LaunchCredentialsCopy(ctx, launchRefusal{Field: "api_key"})
	if !strings.Contains(got, " (api_key: unavailable)") || !strings.HasSuffix(got, "\n\nDetails: launch_credentials_unavailable (api_key: unavailable)") {
		t.Fatalf("field: %q", got)
	}
	got = LaunchCredentialsCopy(ctx, launchRefusal{Data: map[string]any{"field": "base_url", "detail": "rotated"}})
	if !strings.HasSuffix(got, "Details: launch_credentials_unavailable (base_url: rotated)") {
		t.Fatalf("data: %q", got)
	}
	if got := LaunchCredentialsCopy(ctx, errors.New("x")); strings.Contains(got, "Details:") {
		t.Fatalf("no field: %q", got)
	}
}

func TestShortDetailFirstLineCapped(t *testing.T) {
	if got := ShortDetail(errors.New("  boom\nsecond"), 200); got != "boom" {
		t.Fatalf("first line: %q", got)
	}
	if got := ShortDetail(errors.New(strings.Repeat("é", 10)), 5); got != "éééé…" {
		t.Fatalf("cap: %q", got)
	}
	if got := ShortDetail(state.PyObject{}, 200); got == "" {
		t.Fatal("an empty text falls back to the type name")
	}
}

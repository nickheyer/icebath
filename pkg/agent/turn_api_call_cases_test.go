package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Runs the plain interrupt handler over an agent that streamed the given partial text
func interruptWithPartial(t *testing.T, streamed string) ([]*state.Message, APIInterruptVerdict) {
	t.Helper()
	caseHome(t)
	a := bareSteerAgent()
	a.quietMode = true
	a.setCurrentStreamedAssistantText(streamed)
	messages := rowsToMessages([]map[string]any{{"role": "user", "content": "start"}})
	verdict, err := a.handleAPIInterrupt(&TurnRetryState{}, nil, &messages, nil, apiCallWallNow(), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	return messages, verdict
}

func TestRepetitionDominatedPartialIsNotKeptAsTheInterruptedRow(t *testing.T) {
	messages, verdict := interruptWithPartial(t, strings.Repeat("I. ", 1941))
	last := messages[len(messages)-1]
	if last.Role != "assistant" || last.Content != "" || last.DisplayKind != "hidden" || last.APIContent != InterruptedPlaceholder {
		t.Fatalf("interrupted row %+v", last)
	}
	final, _ := verdict.FinalResponse.(string)
	if final != provider.RepetitionLoopInterrupted || strings.Contains(final, "I. I. I.") {
		t.Fatalf("final %q", final)
	}
}

func TestDistinctBatchRowsAreNotMistakenForALoop(t *testing.T) {
	var lines []string
	for i := range 12 {
		lines = append(lines, fmt.Sprintf("INSERT INTO users (id, name, email, created_at) VALUES (%d, 'user%d', 'user%d@example.com', NOW());", i, i, i))
	}
	rows := strings.Join(lines, "\n")
	messages, verdict := interruptWithPartial(t, rows)
	last := messages[len(messages)-1]
	if last.Role != "assistant" || last.Content != rows || verdict.FinalResponse != rows {
		t.Fatalf("interrupted row %+v final %v", last, verdict.FinalResponse)
	}
}

func TestOrdinaryPartialIsKeptAsTheInterruptedRow(t *testing.T) {
	messages, verdict := interruptWithPartial(t, "Visible draft.")
	last := messages[len(messages)-1]
	if last.Role != "assistant" || last.Content != "Visible draft." || !reflect.DeepEqual(last.DisplayMetadata, map[string]any{"interrupted": true}) ||
		verdict.FinalResponse != "Visible draft." {
		t.Fatalf("interrupted row %+v final %v", last, verdict.FinalResponse)
	}
}

func guardJWT(t *testing.T, tier string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"account_tier": tier})
	if err != nil {
		t.Fatal(err)
	}
	seg := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return seg([]byte(`{"alg":"none"}`)) + "." + seg(raw) + ".sig"
}

func TestNousRateLimitGuardPhase(t *testing.T) {
	h := caseHome(t)
	ctx := home.WithOverride(context.Background(), h)
	guardAgent := func(apiKey string) *AIAgent {
		a := bareSteerAgent()
		a.provider, a.apiKey, a.quietMode = "nous", apiKey, true
		a.setBaseURL("https://welcome-api.nousresearch.com/v1")
		return a
	}
	state0 := func() *LoopState {
		return &LoopState{ActiveSystemPrompt: "system", Retry: &TurnRetryState{}, RetryCount: 2, CompressionAttempts: 1}
	}
	guest := guardAgent(guardJWT(t, "anonymous"))
	if v := nousRateLimitGuard(ctx, guest, state0()); v.Action != "fallthrough" {
		t.Fatalf("no recorded limit must fall through, got %q", v.Action)
	}
	header := http.Header{}
	header.Set("retry-after", "600")
	if err := (&provider.NousRateGuard{Home: h, Anonymous: true}).Record(header, time.Time{}, 300*time.Second); err != nil {
		t.Fatal(err)
	}
	statuses := captureStatuses(guest)
	verdict := nousRateLimitGuard(ctx, guest, state0())
	expected := strings.ReplaceAll(provider.FreeTierRateLimitChat, "{reset}", provider.FriendlyWait(600))
	final, _ := verdict.Result["final_response"].(string)
	if verdict.Action != "return" || !strings.Contains(final, expected) || !strings.Contains(expected, "/login") || strings.Contains(expected, "Nous Portal") {
		t.Fatalf("action %q final %q expected %q", verdict.Action, final, expected)
	}
	if !reflect.DeepEqual(*statuses, []string{"⏳ " + expected}) {
		t.Fatalf("statuses %q", *statuses)
	}
	if free, _ := verdict.Result["free_tier"].(map[string]any); free["kind"] != "rate_limited" || verdict.Result["failed"] != true ||
		verdict.Result["failure_reason"] != string(provider.ReasonRateLimit) {
		t.Fatalf("result %v", verdict.Result)
	}
	named := guardAgent(guardJWT(t, "free"))
	if v := nousRateLimitGuard(ctx, named, state0()); v.Action != "fallthrough" {
		t.Fatalf("a signed in credential does not inherit the anonymous cooldown, got %q", v.Action)
	}
	if err := (&provider.NousRateGuard{Home: h}).Record(http.Header{"Retry-After": []string{"300"}}, time.Time{}, 300*time.Second); err != nil {
		t.Fatal(err)
	}
	blocked := nousRateLimitGuard(ctx, named, state0())
	if _, hasFree := blocked.Result["free_tier"]; blocked.Action != "return" || hasFree || !strings.Contains(fmt.Sprint(blocked.Result["final_response"]), "Your Nous account has hit its rate limit") {
		t.Fatalf("named guard %q result %v", blocked.Action, blocked.Result)
	}
}

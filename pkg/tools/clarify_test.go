package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Asker answering from a canned result
type cannedAsker struct {
	seen   []Question
	result FormResult
	err    error
}

func (a *cannedAsker) Ask(ctx context.Context, qs []Question) (FormResult, error) {
	a.seen = qs
	return a.result, a.err
}

func TestClarifyNormalizationErrors(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, "questions must be a non-empty array"},
		{[]any{}, "questions must be a non-empty array"},
		{[]any{1, 2, 3, 4, 5, 6}, "questions supports at most 5 items."},
		{[]any{"x"}, "questions[0] must be an object."},
		{[]any{map[string]any{"question": " "}}, "questions[0].question must be non-empty text."},
		{[]any{map[string]any{"question": "q", "choices": "a"}}, "questions[0].choices must be a list of strings."},
		{[]any{map[string]any{"question": "q", "choices": []any{"a", 2}}}, "questions[0].choices must be a list of strings."},
		{[]any{map[string]any{"question": "q", "choices": []any{strings.Repeat("x", 8001)}}}, "is 8001 characters; the limit is 8000"},
		{[]any{map[string]any{"question": "q", "choices": []any{" ", ""}}}, "has 2 entries but all are blank"},
	}
	for _, c := range cases {
		if _, got := clarifyNormalizeQuestions(c.in); !strings.Contains(got, c.want) {
			t.Errorf("%v -> %q want %q", c.in, got, c.want)
		}
	}
	normalized, errText := clarifyNormalizeQuestions([]any{map[string]any{"question": " Which? ", "choices": []any{"a", " b ", "c", "d", "e"}, "multi_select": true}, map[string]any{"question": "Open"}})
	if errText != "" || len(normalized) != 2 {
		t.Fatal(errText)
	}
	if normalized[0].QID != "q0" || normalized[0].Question != "Which?" || len(normalized[0].Choices) != 4 || normalized[0].Choices[0] != "a (Recommended)" || !normalized[0].MultiSelect {
		t.Fatalf("%+v", normalized[0])
	}
	if normalized[1].Choices != nil || normalized[1].MultiSelect {
		t.Fatalf("%+v", normalized[1])
	}
}

func TestMarkAndStripRecommended(t *testing.T) {
	if got := MarkRecommended([]string{"only"}); got[0] != "only" {
		t.Fatal(got)
	}
	marked := MarkRecommended([]string{"a", "b"})
	if marked[0] != "a (Recommended)" || MarkRecommended(marked)[0] != "a (Recommended)" {
		t.Fatal(marked)
	}
	if StripRecommended(" a (recommended) ") != "a" || StripRecommended("plain") != "plain" {
		t.Fatal("strip")
	}
}

func TestClarifyToolRoundTripMapsDisplayCopyBack(t *testing.T) {
	env := &Env{Home: t.TempDir(), Config: config.Defaults(), Values: &sync.Map{}}
	if r := clarifyTool(context.Background(), env, []any{map[string]any{"question": "q"}}); !strings.Contains(r, clarifyUnavailable) {
		t.Fatal(r)
	}
	asker := &cannedAsker{result: FormResult{Outcome: "submitted", Notice: "", Answers: []Answer{
		{Status: "answered", Text: "sk-***"}, {Status: "answered", Selected: []string{"y", "z"}}, {Status: "skipped"}}}}
	env.Ask = asker
	questions := []any{
		map[string]any{"question": "Key?", "choices": []any{"sk-abcdef1234567890ABCDEF", "other"}},
		map[string]any{"question": "Multi", "choices": []any{"x", "y", "z"}, "multi_select": true},
		map[string]any{"question": "Skip me"},
		map[string]any{"question": "Unanswered"},
	}
	out := mustJSON(t, clarifyTool(context.Background(), env, questions))
	responses := out["responses"].([]any)
	if out["outcome"] != "submitted" || len(responses) != 4 {
		t.Fatal(out)
	}
	first := responses[0].(map[string]any)
	if first["status"] != "answered" {
		t.Fatal(first)
	}
	if asker.seen[0].Choices[0] == "sk-abcdef1234567890ABCDEF (Recommended)" {
		t.Fatal("secret shown unredacted")
	}
	second := responses[1].(map[string]any)
	if got, _ := json.Marshal(second["user_response"]); string(got) != `["y","z"]` {
		t.Fatal(second)
	}
	if responses[2].(map[string]any)["status"] != "skipped" || responses[3].(map[string]any)["status"] != "unanswered" {
		t.Fatal(responses)
	}
	asker.err = errors.New("boom")
	if r := clarifyTool(context.Background(), env, questions); !strings.Contains(r, "Failed to get user input: boom") {
		t.Fatal(r)
	}
}

func TestClarifyResultOutcomeAndNotice(t *testing.T) {
	normalized, _ := clarifyNormalizeQuestions([]any{map[string]any{"question": "q", "choices": []any{"a", "b"}}})
	r := mustJSON(t, clarifyResult(normalized, ClarifyReply{Answers: map[string]any{"q0": "a (Recommended)"}, Outcome: "timed_out", Notice: "left"}, nil))
	if r["notice"] != "left" || r["outcome"] != "timed_out" || r["responses"].([]any)[0].(map[string]any)["user_response"] != "a" {
		t.Fatal(r)
	}
}

func TestSettleClarifyBatch(t *testing.T) {
	locked := map[string]any{"q0": "a"}
	if got := SettleClarifyBatch(locked, nil, "timed_out"); got["outcome"] != "timed_out" || got["answers"].(map[string]any)["q0"] != "a" {
		t.Fatal(got)
	}
	if got := SettleClarifyBatch(locked, nil, "weird"); got["outcome"] != "cancelled" {
		t.Fatal(got)
	}
	frame := map[string]any{"answers": map[string]any{"q1": "b"}, "outcome": "bogus", "extra": 1}
	got := SettleClarifyBatch(locked, frame, "")
	if got["outcome"] != "submitted" || got["extra"] != 1 || got["answers"].(map[string]any)["q0"] != "a" || got["answers"].(map[string]any)["q1"] != "b" {
		t.Fatal(got)
	}
	if got := SettleClarifyBatch(locked, map[string]any{}, ""); got["outcome"] != "cancelled" {
		t.Fatal(got)
	}
}

func TestGatewayClarifyTextCoercion(t *testing.T) {
	entry := RegisterGatewayClarify("c1", "s1", "Pick", []string{"staging (Recommended)", "prod", "two words"}, false)
	defer ClearClarifySession("s1")
	if v, ok, _ := clarifyCoerceTextDetailed(entry, "2"); !ok || v != "prod" {
		t.Fatal(v)
	}
	if v, ok, _ := clarifyCoerceTextDetailed(entry, "STAGING"); !ok || v != "staging (Recommended)" {
		t.Fatal(v)
	}
	if _, ok, reason := clarifyCoerceTextDetailed(entry, "9"); ok || reason != "invalid_selection" {
		t.Fatal(reason)
	}
	if _, ok, reason := clarifyCoerceTextDetailed(entry, "tell me more"); ok || reason != "prose" {
		t.Fatal(reason)
	}
	if v, ok, _ := clarifyCoerceTextDetailed(entry, "Skip"); !ok || v != ClarifySkipped {
		t.Fatal(v)
	}
	MarkClarifyAwaitingText("c1")
	if v, ok, _ := clarifyCoerceTextDetailed(entry, "tell me more"); !ok || v != "tell me more" {
		t.Fatal(v)
	}
	multi := RegisterGatewayClarify("c2", "s2", "Pick", []string{"a", "b", "c"}, true)
	defer ClearClarifySession("s2")
	if v, ok, _ := clarifyCoerceTextDetailed(multi, "1 3"); !ok || v != `["a","c"]` {
		t.Fatal(v)
	}
	if v, ok, _ := clarifyCoerceTextDetailed(multi, "b, a, b"); !ok || v != `["b","a"]` {
		t.Fatal(v)
	}
	if _, ok, reason := clarifyCoerceTextDetailed(multi, "1,9"); ok || reason != "invalid_selection" {
		t.Fatal(reason)
	}
	open := RegisterGatewayClarify("c3", "s3", "Say", nil, false)
	defer ClearClarifySession("s3")
	if v, ok, _ := clarifyCoerceTextDetailed(open, "  anything  "); !ok || v != "anything" {
		t.Fatal(v)
	}
}

func TestGatewayClarifyWaitResolveAndClear(t *testing.T) {
	env := &Env{Home: t.TempDir(), Values: &sync.Map{}}
	RegisterGatewayClarify("w1", "sess", "Q", []string{"a", "b"}, false)
	if AttemptClarifyTextResponse("sess", "prose here") != ClarifyTextRejectedProse {
		t.Fatal("prose must be rejected")
	}
	if AttemptClarifyTextResponse("sess", "7") != ClarifyTextRejectedSelection {
		t.Fatal("bad selection must keep the prompt armed")
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		if !ResolveClarifyTextResponse("sess", "2") {
			t.Error("resolve failed")
		}
	}()
	got := WaitForGatewayClarify(context.Background(), env, "w1", 5*time.Second)
	if got == nil || *got != "b" {
		t.Fatalf("%v", got)
	}
	if ResolveGatewayClarify("w1", "x") || GetPendingClarify("w1") != nil || HasPendingClarify("sess") {
		t.Fatal("entry must be retired")
	}
	if WaitForGatewayClarify(context.Background(), env, "missing", time.Second) != nil {
		t.Fatal("unknown id")
	}
	RegisterGatewayClarify("w2", "sess", "Q", nil, false)
	if got := WaitForGatewayClarify(context.Background(), env, "w2", 30*time.Millisecond); got != nil {
		t.Fatal("timeout must give nil")
	}
	RegisterGatewayClarify("w3", "sess", "Q", nil, false)
	RegisterGatewayClarify("w4", "sess", "Q", nil, false)
	if ClearClarifySession("sess") != 2 || HasPendingClarify("sess") {
		t.Fatal("clear")
	}
	if AttemptClarifyTextResponse("sess", "x") != ClarifyTextNoPending {
		t.Fatal("no pending")
	}
}

func TestResolveClarifyTimeout(t *testing.T) {
	if ResolveClarifyTimeout(config.Tree{}) != 3600 {
		t.Fatal("default")
	}
	if ResolveClarifyTimeout(config.Tree{"clarify": map[string]any{"timeout": "0"}}) != 0 {
		t.Fatal("legacy string zero")
	}
	if ResolveClarifyTimeout(config.Tree{"agent": map[string]any{"clarify_timeout": 12.9}}) != 12 {
		t.Fatal("agent float")
	}
	if ResolveClarifyTimeout(config.Tree{"agent": map[string]any{"clarify_timeout": "soon"}}) != 3600 {
		t.Fatal("non numeric")
	}
	if ResolveClarifyTimeout(config.Tree{"clarify": map[string]any{"timeout": nil}, "agent": map[string]any{"clarify_timeout": -5}}) != -5 {
		t.Fatal("null legacy falls through")
	}
}

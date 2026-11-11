package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type callbackAsker struct {
	answer string
	err    error
	seen   []ApprovalCallbackOptions
	text   [][2]string
}

func (a *callbackAsker) Ask(ctx context.Context, qs []Question) (FormResult, error) {
	return FormResult{}, errors.New("the form path must not run for an approval callback")
}

func (a *callbackAsker) ApprovalCallback(ctx context.Context, command, description string, o ApprovalCallbackOptions) (string, error) {
	a.seen = append(a.seen, o)
	a.text = append(a.text, [2]string{command, description})
	return a.answer, a.err
}

func TestApprovalCallbackAnswerPassesThroughVerbatim(t *testing.T) {
	env := approvalTestEnv(t, "prompt", nil, nil)
	for _, answer := range []string{"once", "session", "always", "deny", "timeout"} {
		asker := &callbackAsker{answer: answer}
		env.Ask = asker
		got := promptDangerousApproval(context.Background(), env, promptOptions{command: "rm -rf /tmp/x", description: "recursive delete", allowPermanent: true, allowSession: true})
		if got.value != answer {
			t.Fatalf("%q -> %q", answer, got.value)
		}
		if len(asker.seen) != 1 || !asker.seen[0].AllowPermanent || !asker.seen[0].AllowSession || asker.seen[0].SmartDenied {
			t.Fatalf("options %+v", asker.seen)
		}
		if asker.text[0] != [2]string{"rm -rf /tmp/x", "recursive delete"} {
			t.Fatalf("arguments %v", asker.text[0])
		}
	}
	asker := &callbackAsker{answer: "once"}
	env.Ask = asker
	promptDangerousApproval(context.Background(), env, promptOptions{command: "x", description: "d", allowSession: false, smartDenied: true, title: "Save to memory?"})
	if o := asker.seen[0]; o.AllowPermanent || o.AllowSession || !o.SmartDenied || o.Title != "Save to memory?" {
		t.Fatalf("options %+v", o)
	}
}

func TestApprovalCallbackFailureIsUnanswered(t *testing.T) {
	env := approvalTestEnv(t, "prompt", nil, nil)
	env.Ask = &callbackAsker{err: errors.New("boom")}
	got := promptDangerousApproval(context.Background(), env, promptOptions{command: "x", description: "d", allowSession: true})
	if got.value != "cancelled" || !strings.Contains(got.cause, "the approval callback failed") {
		t.Fatalf("got %+v", got)
	}
}

func TestCallbackTimeoutAnswerIsNotDeny(t *testing.T) {
	env := approvalTestEnv(t, "prompt", nil, nil)
	env.Ask = &scriptedAsker{answer: "timeout"}
	got := promptDangerousApproval(context.Background(), env, promptOptions{command: "x", description: "d", allowPermanent: true, allowSession: true})
	if got.value != "timeout" {
		t.Fatalf("callback timeout answer became %+v", got)
	}
	env.Ask = &scriptedAsker{answer: "timeout", terminal: true}
	got = promptDangerousApproval(context.Background(), env, promptOptions{command: "x", description: "d", allowPermanent: true, allowSession: true})
	if got.value != "deny" {
		t.Fatalf("a typed word at the terminal menu became %+v", got)
	}
}

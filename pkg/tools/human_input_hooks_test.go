package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

const (
	humanHookPassword = "hunter2-do-not-leak"
	humanHookToken    = "ghp_" + "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"
	humanHookCommand  = "sudo env GITHUB_TOKEN=" + humanHookToken + " apt-get install -y jq"
)

var humanHookNames = []string{"on_human_input_request", "on_human_input_resolved"}

type humanHookCall struct {
	name    string
	payload map[string]any
}

type humanHookCapture struct {
	mu    sync.Mutex
	calls []humanHookCall
}

func (c *humanHookCapture) snapshot() []humanHookCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]humanHookCall{}, c.calls...)
}

type hookAsker func(ctx context.Context, questions []Question) (FormResult, error)

func (f hookAsker) Ask(ctx context.Context, questions []Question) (FormResult, error) {
	return f(ctx, questions)
}

// Captures the two human input hooks the way the hermes fixture patches invoke_hook
func captureHumanInputHooks(t *testing.T) *humanHookCapture {
	t.Helper()
	capture := &humanHookCapture{}
	saved := ApprovalHookInvoker
	ApprovalHookInvoker = func(name string, payload map[string]any) error {
		for _, hook := range humanHookNames {
			if name == hook {
				capture.mu.Lock()
				capture.calls = append(capture.calls, humanHookCall{name: name, payload: payload})
				capture.mu.Unlock()
			}
		}
		return nil
	}
	t.Cleanup(func() { ApprovalHookInvoker = saved })
	ResetCachedSudoPasswords()
	return capture
}

func singleHumanPair(t *testing.T, capture *humanHookCapture, kind string) (map[string]any, map[string]any) {
	t.Helper()
	calls := capture.snapshot()
	if len(calls) != 2 || calls[0].name != humanHookNames[0] || calls[1].name != humanHookNames[1] {
		t.Fatalf("calls %v", calls)
	}
	request, resolved := calls[0].payload, calls[1].payload
	if request["kind"] != kind || resolved["kind"] != kind {
		t.Fatalf("kinds %v %v", request["kind"], resolved["kind"])
	}
	if id, _ := request["request_id"].(string); id == "" || resolved["request_id"] != id {
		t.Fatalf("request ids %v %v", request["request_id"], resolved["request_id"])
	}
	if _, present := request["outcome"]; present {
		t.Fatal("request carries an outcome")
	}
	for _, key := range []string{"session_id", "session_key", "platform", "prompt"} {
		value, present := request[key]
		if !present || resolved[key] != value {
			t.Fatalf("%s request %v resolved %v", key, value, resolved[key])
		}
	}
	return request, resolved
}

func assertNoSecret(t *testing.T, capture *humanHookCapture, secrets ...string) {
	t.Helper()
	blob := fmt.Sprintf("%v", capture.snapshot())
	for _, secret := range secrets {
		if strings.Contains(blob, secret) {
			t.Fatalf("payloads carry %q: %s", secret, blob)
		}
	}
}

func answerText(text string) hookAsker {
	return func(context.Context, []Question) (FormResult, error) {
		return FormResult{Outcome: "submitted", Answers: []Answer{{Status: "answered", Text: text}}}, nil
	}
}

func TestSudoCallbackPromptFiresPairWithoutPassword(t *testing.T) {
	capture := captureHumanInputHooks(t)
	env := &Env{Ask: answerText(humanHookPassword)}
	if got := promptForSudoPassword(context.Background(), env, humanHookCommand, 45*time.Second); got != humanHookPassword {
		t.Fatalf("password %q", got)
	}
	request, resolved := singleHumanPair(t, capture, "sudo")
	if resolved["outcome"] != "provided" || !strings.Contains(request["prompt"].(string), "apt-get install") {
		t.Fatalf("request %v resolved %v", request, resolved)
	}
	assertNoSecret(t, capture, humanHookPassword, humanHookToken)
}

func TestSudoEmptyAnswerIsSkipped(t *testing.T) {
	capture := captureHumanInputHooks(t)
	env := &Env{Ask: answerText("")}
	if got := promptForSudoPassword(context.Background(), env, "sudo true", 45*time.Second); got != "" {
		t.Fatalf("password %q", got)
	}
	if _, resolved := singleHumanPair(t, capture, "sudo"); resolved["outcome"] != "skipped" {
		t.Fatalf("resolved %v", resolved)
	}
}

func TestSudoTtyFallbackTimeoutResolvesAsTimeout(t *testing.T) {
	capture := captureHumanInputHooks(t)
	hold := make(chan struct{})
	saved := readHiddenLineFn
	readHiddenLineFn = func() string {
		<-hold
		return ""
	}
	t.Cleanup(func() {
		close(hold)
		readHiddenLineFn = saved
	})
	if got := promptForSudoPassword(context.Background(), &Env{}, "sudo true", 0); got != "" {
		t.Fatalf("password %q", got)
	}
	if _, resolved := singleHumanPair(t, capture, "sudo"); resolved["outcome"] != "timeout" {
		t.Fatalf("resolved %v", resolved)
	}
}

var clarifyHookQuestions = []any{map[string]any{"question": "Which database?", "choices": []any{"postgres", "sqlite"}}}

func TestClarifyCallbackOutcomeIsReportedWithoutAnswers(t *testing.T) {
	for _, outcome := range []string{"submitted", "timed_out", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			capture := captureHumanInputHooks(t)
			env := &Env{Ask: hookAsker(func(context.Context, []Question) (FormResult, error) {
				return FormResult{Outcome: outcome, Answers: []Answer{{Status: "answered", Text: humanHookPassword}}}, nil
			})}
			clarifyTool(context.Background(), env, clarifyHookQuestions)
			request, resolved := singleHumanPair(t, capture, "clarify")
			if resolved["outcome"] != outcome || !strings.Contains(request["prompt"].(string), "Which database?") {
				t.Fatalf("request %v resolved %v", request, resolved)
			}
			assertNoSecret(t, capture, humanHookPassword)
		})
	}
}

func TestClarifyCallbackFailureResolvesAsError(t *testing.T) {
	capture := captureHumanInputHooks(t)
	env := &Env{Ask: hookAsker(func(context.Context, []Question) (FormResult, error) {
		return FormResult{}, errors.New("ui gone")
	})}
	clarifyTool(context.Background(), env, clarifyHookQuestions)
	if _, resolved := singleHumanPair(t, capture, "clarify"); resolved["outcome"] != "error" {
		t.Fatalf("resolved %v", resolved)
	}
}

func TestApprovalCliCallbackPromptRedactsCommand(t *testing.T) {
	capture := captureHumanInputHooks(t)
	once := hookAsker(func(context.Context, []Question) (FormResult, error) {
		return FormResult{Outcome: "submitted", Answers: []Answer{{Status: "answered", Selected: []string{"once"}}}}, nil
	})
	choice := promptDangerousApproval(context.Background(), &Env{}, promptOptions{command: humanHookCommand, description: "privileged install", approvalCallback: once})
	if choice.value != "once" {
		t.Fatalf("choice %v", choice)
	}
	request, resolved := singleHumanPair(t, capture, "approval")
	if resolved["outcome"] != "once" || !strings.Contains(request["prompt"].(string), "apt-get install") {
		t.Fatalf("request %v resolved %v", request, resolved)
	}
	assertNoSecret(t, capture, humanHookToken)
}

func TestApprovalGatewayDecisionFiresPair(t *testing.T) {
	capture := captureHumanInputHooks(t)
	sessionKey := "human-input-gateway"
	env := &Env{Config: config.Tree{"approvals": map[string]any{"timeout": 5}}}
	notify := func(map[string]any) error {
		time.AfterFunc(50*time.Millisecond, func() { ResolveGatewayApproval(sessionKey, "session", false, "", "") })
		return nil
	}
	approvalData := map[string]any{"command": humanHookCommand, "description": "privileged install", "pattern_key": "sudo", "pattern_keys": []any{"sudo"}}
	decision := awaitGatewayDecision(context.Background(), env, sessionKey, notify, approvalData, "gateway")
	if decision.Choice != "session" {
		t.Fatalf("decision %+v", decision)
	}
	request, resolved := singleHumanPair(t, capture, "approval")
	if request["session_key"] != sessionKey || resolved["outcome"] != "session" {
		t.Fatalf("request %v resolved %v", request, resolved)
	}
	assertNoSecret(t, capture, humanHookToken)
}

func TestApprovalGatewayNotifyFailureStillResolves(t *testing.T) {
	capture := captureHumanInputHooks(t)
	notify := func(map[string]any) error { return errors.New("adapter offline") }
	approvalData := map[string]any{"command": "rm -rf build", "description": "d", "pattern_key": "rm", "pattern_keys": []any{"rm"}}
	decision := awaitGatewayDecision(context.Background(), &Env{}, "human-input-notify-fail", notify, approvalData, "gateway")
	if !decision.NotifyFailed {
		t.Fatalf("decision %+v", decision)
	}
	if _, resolved := singleHumanPair(t, capture, "approval"); resolved["outcome"] != "notify_failed" {
		t.Fatalf("resolved %v", resolved)
	}
}

func TestApprovalPluginTransportFiresPair(t *testing.T) {
	capture := captureHumanInputHooks(t)
	approvalTransportResolver.RLock()
	previous := approvalTransportResolver.fn
	approvalTransportResolver.RUnlock()
	RegisterApprovalTransportResolver(func(env *Env, name string) (*RegisteredApprovalTransport, error) {
		if name != "phone" {
			return nil, nil
		}
		present := func(ctx context.Context, req ApprovalRequest) (ApprovalTransportDecision, error) {
			return req.Respond("deny"), nil
		}
		return &RegisteredApprovalTransport{Name: "phone", Present: present, PluginID: "phone-fixture"}, nil
	})
	t.Cleanup(func() { RegisterApprovalTransportResolver(previous) })
	env := &Env{Config: config.Tree{"security": map[string]any{"approval": map[string]any{"transport": "phone"}}}}
	attempt := presentWithSelectedTransport(context.Background(), env, transportRequest{
		command: humanHookCommand, description: "d", patternKey: "sudo", patternKeys: []string{"sudo"},
		sessionKey: "human-input-transport", surface: "cli", allowSession: true, allowPermanent: true})
	if attempt.choice != "deny" {
		t.Fatalf("attempt %+v", attempt)
	}
	request, resolved := singleHumanPair(t, capture, "approval")
	if request["session_key"] != "human-input-transport" || resolved["outcome"] != "deny" {
		t.Fatalf("request %v resolved %v", request, resolved)
	}
	assertNoSecret(t, capture, humanHookToken)
}

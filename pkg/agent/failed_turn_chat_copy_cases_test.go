package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Openrouter agent with status output suppressed and thirty iterations of budget
func chatCopyAgent(t *testing.T) *AIAgent {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	return &AIAgent{model: "gpt-5-turbo", provider: "openrouter", maxIterations: 30, suppressStatusOutput: true}
}

func TestOuterLoopErrorCopyHasNoApologyAndRoutesToGatewayLayer(t *testing.T) {
	a := chatCopyAgent(t)
	a.setBaseURL("https://openrouter.ai/api/v1")
	var traced error
	func() {
		defer func() { traced = RecoveredOuterLoopError(recover()) }()
		panic(errors.New("expected str, got list"))
	}()
	messages := []*state.Message{}
	v, err := a.handleOuterLoopError(context.Background(), traced, 7, 2, &messages, nil, "unknown", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.Action != "break" || !v.Failed {
		t.Fatalf("verdict %+v", v)
	}
	text, _ := v.FinalResponse.(string)
	if !strings.HasSuffix(strings.TrimRight(text, " \t\n"), "expected str, got list") {
		t.Fatalf("raw detail must come last: %q", text)
	}
	exitFailure, ok := ExitReasonFailure(v.TurnExitReason)
	if !ok || !exitFailure.FailsTurn {
		t.Fatalf("exit failure %+v %v for %v", exitFailure, ok, v.TurnExitReason)
	}
	surface := buildErrorSurfaceFromResult(map[string]any{"failed": true, "error": text, "failure_reason": exitFailure.Reason,
		"failure_retryable": exitFailure.Retryable}, "", "")
	if surface["layer"] != LayerGateway || surface["code"] != "loop_error" {
		t.Fatalf("surface %v", surface)
	}
}

func TestInterpreterShutdownCopySubstitutesTheRealSessionId(t *testing.T) {
	a := chatCopyAgent(t)
	a.sessionID = "20260914_abc"
	messages := []*state.Message{}
	v, err := a.handleOuterLoopError(context.Background(), &summaryRuntimeError{msg: "cannot schedule new futures after interpreter shutdown"}, 0, 1, &messages, nil, "unknown", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := v.FinalResponse.(string)
	if !strings.Contains(text, "icebath --resume 20260914_abc") || strings.Contains(text, "<session-id>") {
		t.Fatalf("final response %q", text)
	}
}

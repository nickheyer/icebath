package acp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

type fakeFuture struct {
	result    any
	err       error
	cancelled int
}

func (f *fakeFuture) Result(time.Duration) (any, error) { return f.result, f.err }
func (f *fakeFuture) Cancel() bool                      { f.cancelled++; return true }

type fakeLoop struct {
	future    *fakeFuture
	scheduled int
	refuse    error
}

func (l *fakeLoop) RunCoroutineThreadsafe(fn func(ctx context.Context) (any, error)) (Future, error) {
	if l.refuse != nil {
		return nil, l.refuse
	}
	l.scheduled++
	_, _ = fn(context.Background())
	return l.future, nil
}

type permCall struct {
	sessionID string
	toolCall  *pyacp.ToolCallProgress
	options   []pyacp.PermissionOption
}

func recordingRequest(calls *[]permCall) RequestPermissionFunc {
	return func(ctx context.Context, sessionID string, tc *pyacp.ToolCallProgress, options []pyacp.PermissionOption) (*pyacp.RequestPermissionResponse, error) {
		*calls = append(*calls, permCall{sessionID, tc, options})
		return nil, nil
	}
}

func invokeCallback(t *testing.T, outcome pyacp.PermissionOutcome, allowPermanent, allowSession, smartDenied bool) (string, permCall, *fakeLoop) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var calls []permCall
	loop := &fakeLoop{future: &fakeFuture{result: &pyacp.RequestPermissionResponse{Outcome: outcome}}}
	timeout := 60.0
	cb := MakeApprovalCallback(recordingRequest(&calls), loop, "s1", &timeout, nil)
	result := cb(context.Background(), "rm -rf /", "dangerous command", allowPermanent, allowSession, smartDenied)
	if len(calls) != 1 {
		t.Fatalf("calls %d", len(calls))
	}
	return result, calls[0], loop
}

func optionIDs(options []pyacp.PermissionOption) []string {
	var ids []string
	for _, o := range options {
		ids = append(ids, o.OptionID)
	}
	return ids
}

func TestApprovalBridgeBridgeSchedulesRequestOnTheGivenLoop(t *testing.T) {
	result, call, loop := invokeCallback(t, &pyacp.AllowedOutcome{OptionID: "allow_once"}, true, true, false)
	tc := call.toolCall
	if result != "once" || loop.scheduled != 1 || call.sessionID != "s1" {
		t.Fatalf("result %q", result)
	}
	if *tc.Kind != "execute" || *tc.Status != "pending" || !strings.Contains(*tc.Title, "dangerous command") || !strings.Contains(*tc.Title, "rm -rf /") {
		t.Fatalf("tool call %#v", tc)
	}
	text := contentText(t, tc.Content[0])
	if !strings.Contains(text, "$ rm -rf /") || !strings.Contains(text, "dangerous command") {
		t.Fatalf("text %q", text)
	}
	if !pyjson.Equal(tc.RawInput, obj("command", "rm -rf /", "description", "dangerous command")) {
		t.Fatalf("raw %v", tc.RawInput)
	}
	if !reflect.DeepEqual(optionIDs(call.options), []string{"allow_once", "allow_session", "allow_always", "deny", "deny_always"}) {
		t.Fatalf("options %v", optionIDs(call.options))
	}
}

func TestApprovalBridgeSessionLessGateOffersOnlyOnceAndDeny(t *testing.T) {
	_, call, _ := invokeCallback(t, &pyacp.AllowedOutcome{OptionID: "allow_once"}, false, false, false)
	if !reflect.DeepEqual(optionIDs(call.options), []string{"allow_once", "deny"}) {
		t.Fatalf("options %v", optionIDs(call.options))
	}
}

func TestApprovalBridgeToolCallIdsAreUnique(t *testing.T) {
	_, first, _ := invokeCallback(t, &pyacp.AllowedOutcome{OptionID: "allow_once"}, true, true, false)
	_, second, _ := invokeCallback(t, &pyacp.AllowedOutcome{OptionID: "allow_once"}, true, true, false)
	if first.toolCall.ToolCallID == second.toolCall.ToolCallID {
		t.Fatal("same id")
	}
}

func TestApprovalBridgeTimeoutReturnsTimeoutAndCancelsFuture(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var calls []permCall
	loop := &fakeLoop{future: &fakeFuture{err: ErrFutureTimeout}}
	timeout := 0.01
	result := MakeApprovalCallback(recordingRequest(&calls), loop, "s1", &timeout, nil)(context.Background(), "rm -rf /", "dangerous command", true, true, false)
	if result != "timeout" || loop.scheduled != 1 || loop.future.cancelled != 1 {
		t.Fatalf("result %q cancelled %d", result, loop.future.cancelled)
	}
}

func TestSchedulerFailureClosesPermissionCoroutine(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var calls []permCall
	loop := &fakeLoop{refuse: errors.New("scheduler down")}
	timeout := 0.01
	result := MakeApprovalCallback(recordingRequest(&calls), loop, "s1", &timeout, nil)(context.Background(), "rm -rf /", "dangerous", true, true, false)
	if result != "deny" || len(calls) != 0 {
		t.Fatalf("result %q calls %d", result, len(calls))
	}
}

func runTerminalStatus(t *testing.T, outcome pyacp.PermissionOutcome, call func(RequestPermissionFunc, Loop, SendUpdateFunc)) (*pyacp.ToolCallProgress, []*pyacp.ToolCallProgress) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var calls []permCall
	var sent []*pyacp.ToolCallProgress
	loop := &fakeLoop{future: &fakeFuture{result: &pyacp.RequestPermissionResponse{Outcome: outcome}}}
	call(recordingRequest(&calls), loop, func(u pyacp.SessionUpdate) { sent = append(sent, u.(*pyacp.ToolCallProgress)) })
	return calls[0].toolCall, sent
}

func TestPermissionRequestToolCallReachesATerminalStatusCommandPermissionRequestIsClosedPerOutcome(t *testing.T) {
	for _, c := range []struct {
		outcome pyacp.PermissionOutcome
		status  string
	}{{&pyacp.AllowedOutcome{OptionID: "allow_once"}, "completed"}, {&pyacp.DeniedOutcome{}, "failed"}} {
		requested, sent := runTerminalStatus(t, c.outcome, func(rp RequestPermissionFunc, l Loop, su SendUpdateFunc) {
			MakeApprovalCallback(rp, l, "s1", nil, su)(context.Background(), "rm -rf /", "dangerous", true, true, false)
		})
		if len(sent) != 1 || sent[0].ToolCallID != requested.ToolCallID || *sent[0].Status != c.status {
			t.Fatalf("sent %#v", sent)
		}
	}
}

func editProposal() *tools.EditProposal {
	return &tools.EditProposal{ToolName: "write_file", Path: "/tmp/x", OldText: pyacp.Ptr(""), NewText: "y", Arguments: map[string]any{}}
}

func TestPermissionRequestToolCallReachesATerminalStatusDeniedEditApprovalRequestIsClosedAsFailed(t *testing.T) {
	requested, sent := runTerminalStatus(t, &pyacp.DeniedOutcome{}, func(rp RequestPermissionFunc, l Loop, su SendUpdateFunc) {
		_, _ = MakeACPEditApprovalRequester(rp, l, "s1", nil, nil, su)(context.Background(), nil, editProposal())
	})
	if len(sent) != 1 || sent[0].ToolCallID != requested.ToolCallID || *sent[0].Status != "failed" {
		t.Fatalf("sent %#v", sent)
	}
}

func TestPermissionRequestToolCallReachesATerminalStatusAllowedEditApprovalRequestIsClosedAsCompletedOnce(t *testing.T) {
	var decisions []bool
	requested, sent := runTerminalStatus(t, &pyacp.AllowedOutcome{OptionID: "allow_once"}, func(rp RequestPermissionFunc, l Loop, su SendUpdateFunc) {
		ok, _ := MakeACPEditApprovalRequester(rp, l, "s1", nil, nil, su)(context.Background(), nil, editProposal())
		decisions = append(decisions, ok)
	})
	if !reflect.DeepEqual(decisions, []bool{true}) || len(sent) != 1 || sent[0].ToolCallID != requested.ToolCallID || *sent[0].Status != "completed" {
		t.Fatalf("decisions %v sent %#v", decisions, sent)
	}
}

func TestDefaultPermissionTimeoutFollowsApprovalsConfig(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("approvals:\n  timeout: 900\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ResolvePermissionTimeout(context.Background(), nil); got != 900.0 {
		t.Fatalf("got %v", got)
	}
	half := 0.5
	if got := ResolvePermissionTimeout(context.Background(), &half); got != 0.5 {
		t.Fatalf("got %v", got)
	}
}

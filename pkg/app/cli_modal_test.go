package app

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Runs the approval callback off the test goroutine and answers it once the panel opens
func resolveApproval(t *testing.T, c *CLI, command, description string, allowPermanent, allowSession, smartDenied bool, wantChoices []string, answer string) string {
	t.Helper()
	results := make(chan string, 1)
	go func() {
		result, err := c.approvalCallback(context.Background(), command, description, allowPermanent, allowSession, smartDenied)
		if err != nil {
			t.Error(err)
		}
		results <- result
	}()
	waitFor(t, "approval panel", func() bool { return c.approvalModal() != nil })
	state := c.approvalModal()
	if wantChoices != nil && strings.Join(state.Choices, ",") != strings.Join(wantChoices, ",") {
		t.Fatalf("choices %v", state.Choices)
	}
	state.response <- answer
	select {
	case result := <-results:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("the approval callback never returned")
	}
	return ""
}

func TestCliApprovalUiSmartDeniedCallbackOffersOnlyOnceAndDeny(t *testing.T) {
	c, _ := newModalTestCLI(t, nil)
	captureStreams(t)
	if got := resolveApproval(t, c, "rm -rf /tmp/example", "recursive delete", false, true, true, []string{"once", "deny"}, "deny"); got != "deny" {
		t.Fatalf("got %q", got)
	}
}

func TestCliApprovalUiSessionLessGateOffersOnlyOnceAndDeny(t *testing.T) {
	c, _ := newModalTestCLI(t, nil)
	captureStreams(t)
	if got := resolveApproval(t, c, "<write to AGENTS.md>", "protected agent-instruction file", false, false, false, []string{"once", "deny"}, "once"); got != "once" {
		t.Fatalf("got %q", got)
	}
}

func TestCliApprovalUiSudoPromptRestoresExistingDraftAfterResponse(t *testing.T) {
	c, app := newModalTestCLI(t, nil)
	captureStreams(t)
	app.SetBuffer("draft command", 5)
	results := make(chan string, 1)
	go func() { results <- c.sudoPasswordCallback(context.Background()) }()
	waitFor(t, "sudo panel", func() bool { return c.sudoModal() != nil })
	if app.BufferText() != "" {
		t.Fatalf("draft still in the buffer: %q", app.BufferText())
	}
	app.SetBuffer("secret", len("secret"))
	c.sudoModal().response <- "secret"
	if got := <-results; got != "secret" {
		t.Fatalf("got %q", got)
	}
	if app.BufferText() != "draft command" || app.BufferCursor() != 5 {
		t.Fatalf("buffer %q cursor %d", app.BufferText(), app.BufferCursor())
	}
}

func TestCliApprovalUiHandleApprovalSelectionViewExpandsInPlace(t *testing.T) {
	c, _ := newModalTestCLI(t, nil)
	c.approvalState = &approvalModalState{
		Command:     "sudo dd if=/tmp/in of=/usr/share/keyrings/githubcli-archive-keyring.gpg bs=4M status=progress",
		Description: "disk copy", Choices: []string{"once", "session", "always", "deny", "view"}, Selected: 4, response: make(chan string, 1)}
	c.handleApprovalSelection()
	state := c.approvalModal()
	if state == nil || !state.ShowFull || strings.Join(state.Choices, ",") != "once,session,always,deny" || state.Selected != 3 || len(state.response) != 0 {
		t.Fatalf("state %+v", state)
	}
}

// A CLI whose paint gates are both set adversarially, a pending resize and a recent throttled paint
func newRealPaintCLI(t *testing.T) (*CLI, *fakeTUIApp) {
	t.Helper()
	c, app := newModalTestCLI(t, nil)
	c.resizeRecoveryPending = true
	now := monotonic()
	c.lastInvalidate = &now
	return c, app
}

func TestModalPaintNowPaintNowBypassesThrottleAndResizeGuard(t *testing.T) {
	c, app := newRealPaintCLI(t)
	_ = c.invalidate()
	if app.invalidations() != 0 {
		t.Fatal("a throttled invalidate painted under both gates")
	}
	c.paintNow()
	if app.invalidations() == 0 {
		t.Fatal("paint_now did not paint")
	}
}

func TestModalPaintNowSecretResponseTeardownPaints(t *testing.T) {
	c, app := newRealPaintCLI(t)
	c.secretState = &secretModalState{response: make(chan string, 1)}
	c.secretDeadline = 0
	c.submitSecretResponse("hunter2")
	if c.secretModal() != nil || app.invalidations() == 0 {
		t.Fatalf("state %v paints %d", c.secretModal(), app.invalidations())
	}
}

func TestPersistPromptSummaryApprovalResolutionPrintsSummaryLine(t *testing.T) {
	c, _ := newModalTestCLI(t, nil)
	out, _ := captureStreams(t)
	if got := resolveApproval(t, c, "rm -rf /tmp/scratch", "danger", true, true, false, nil, "session"); got != "session" {
		t.Fatalf("got %q", got)
	}
	summary := out.String()
	for _, want := range []string{"Approval", "rm -rf /tmp/scratch", "allowed for session"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary %q lacks %q", summary, want)
		}
	}
}

func TestPersistPromptSummaryPersistPromptsFalseSuppressesSummary(t *testing.T) {
	c, _ := newModalTestCLI(t, map[string]any{"display": map[string]any{"persist_prompts": false}})
	out, _ := captureStreams(t)
	if got := resolveApproval(t, c, "rm -rf /tmp/scratch", "danger", true, true, false, nil, "once"); got != "once" {
		t.Fatalf("got %q", got)
	}
	if strings.Contains(out.String(), "Approval") {
		t.Fatalf("summary printed with persist_prompts off: %q", out.String())
	}
}

func TestClearOverlaysForInterruptClearsAllFourOverlaysAndUnblocksQueues(t *testing.T) {
	c, _ := newModalTestCLI(t, nil)
	approvalQ, clarifyQ, sudoQ, secretQ := make(chan string, 1), make(chan map[string]any, 1), make(chan string, 1), make(chan string, 1)
	c.approvalState = &approvalModalState{response: approvalQ}
	c.clarifyState = &clarifyModalState{response: clarifyQ}
	c.clarifyFreetext = true
	c.sudoState = &sudoModalState{response: sudoQ}
	c.sudoDeadline = 99999.0
	c.secretState = &secretModalState{VarName: "X", response: secretQ}
	c.clearActiveOverlaysForInterrupt()
	if c.approvalModal() != nil || c.clarifyState != nil || c.clarifyFreetext || c.sudoModal() != nil || c.sudoDeadline != 0 || c.secretModal() != nil {
		t.Fatal("an overlay survived the interrupt")
	}
	if <-approvalQ != "deny" || <-clarifyQ != nil || <-sudoQ != "" || <-secretQ != "" {
		t.Fatal("a blocked prompt did not receive its terminal value")
	}
}

func TestClearOverlaysForInterruptDeadQueueDoesNotBlockClearingOthers(t *testing.T) {
	c, _ := newModalTestCLI(t, nil)
	clarifyQ := make(chan map[string]any, 1)
	c.approvalState = &approvalModalState{response: nil}
	c.clarifyState = &clarifyModalState{response: clarifyQ}
	c.clearActiveOverlaysForInterrupt()
	if c.approvalModal() != nil || c.clarifyState != nil {
		t.Fatal("the dead queue blocked the clearing")
	}
	if <-clarifyQ != nil {
		t.Fatal("clarify must receive nil")
	}
}

func TestClearOverlaysForInterruptInterruptUnblocksThreadBlockedOnApproval(t *testing.T) {
	c, _ := newModalTestCLI(t, nil)
	approvalQ := make(chan string, 1)
	c.approvalState = &approvalModalState{response: approvalQ}
	results := make(chan string, 1)
	go func() {
		select {
		case v := <-approvalQ:
			results <- v
		case <-time.After(2 * time.Second):
			results <- "<timeout>"
		}
	}()
	time.Sleep(50 * time.Millisecond)
	c.clearActiveOverlaysForInterrupt()
	if got := <-results; got != "deny" {
		t.Fatalf("worker got %q", got)
	}
}

package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestLateVerdictReachesTheModelBeforeTheTurnEnds(t *testing.T) {
	t.Setenv("ICEBATH_VERIFY_ON_STOP", "0")
	token := tools.LSPDeferredTrackPending("S-end")
	go func() {
		time.Sleep(50 * time.Millisecond)
		tools.LSPDeferredPost("S-end", "/repo/c.py", "c.py:1:5 error MOCK001 name 'bad' is not defined", nil)
		tools.LSPDeferredSettled("S-end", token)
	}()
	a := &AIAgent{sessionID: "S-end", currentTaskID: "t-S-end"}
	s := &LoopState{Messages: []*state.Message{{Role: "user", Content: "edit c.py"}}}
	verdict := applyStopGates(context.Background(), a, map[string]any{"role": "assistant", "content": "done"}, "done", s)
	if !verdict.continueTurn {
		t.Fatal("the turn ended with the edit's verdict still undelivered")
	}
	last := s.Messages[len(s.Messages)-1]
	if text, _ := last.Content.(string); last.Role != "user" || !strings.Contains(text, "MOCK001") {
		t.Fatalf("last %+v", last)
	}
	if verdict.pendingVerificationResponse != "done" || a.lspTurnEndNotes != 1 {
		t.Fatalf("verdict %+v notes %d", verdict, a.lspTurnEndNotes)
	}
}

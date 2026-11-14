package tuiserver

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/codeexec"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

func TestFinalizeSessionUsesAgentSessionIdFinalizeTargetsAgentSessionIdNotStaleKey(t *testing.T) {
	sessionCaseScope(t)
	db := reapLedger(t)
	if err := db.CreateSession(state.NewSession{ID: "parent-session", Source: "tui", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := db.EndSession("parent-session", "compression"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "continuation-session", Source: "tui", Model: "test", ParentSessionID: "parent-session"}); err != nil {
		t.Fatal(err)
	}
	a := testAgent(t, agent.AIAgentOptions{Model: "test", Provider: "custom", APIMode: "chat_completions", SessionID: "continuation-session", Platform: "tui"})
	session := tuigateway.NewSession(map[string]any{"agent": a, "session_key": "parent-session", "history": []any{map[string]any{"role": "user", "content": "hello"}},
		"history_lock": &sync.Mutex{}, "history_version": 0, "running": false, "attached_images": []any{}, "image_counter": 0, "cols": 80,
		"slash_worker": nil, "show_reasoning": false, "tool_progress_mode": "all", "pending_title": nil})
	finalizeSession(context.Background(), session, "tui_close")
	row, err := db.GetSession(context.Background(), "continuation-session")
	if err != nil || row.GetOr("ended_at", nil) == nil || row.GetOr("end_reason", nil) != "tui_close" {
		t.Fatalf("continuation %v %v", row, err)
	}
}

const kernelSessionKey = "20260907_120000_abcdef"

// Seeds a registered kernel and session yolo for the kernel key, a stored row of the source when set
func liveKernel(t *testing.T, source string) (registered, stopped func() bool) {
	t.Helper()
	sessionCaseScope(t)
	db := reapLedger(t)
	if source != "" {
		if err := db.CreateSession(state.NewSession{ID: kernelSessionKey, Source: source}); err != nil {
			t.Fatal(err)
		}
	}
	registered, stopped, remove := codeexec.RegisterKernelForTests(kernelSessionKey, t.TempDir())
	t.Cleanup(remove)
	tools.EnableSessionYolo(kernelSessionKey)
	t.Cleanup(func() { tools.ClearSession(kernelSessionKey) })
	return registered, stopped
}

// Session record of the kernel cases
func kernelSession() *tuigateway.Session {
	return tuigateway.NewSession(map[string]any{"active_session_lease": nil, "agent": nil, "history": []any{}, "history_lock": &sync.Mutex{},
		"profile_home": nil, "session_key": kernelSessionKey, "slash_worker": nil, "source": "desktop"})
}

func TestTuiOwnedFinalizeDisposesTheSessionsKernels(t *testing.T) {
	for _, tc := range []struct{ source, endReason string }{{"desktop", "tui_close"}, {"desktop", "ws_orphan_reap"}, {"", "idle_timeout"}} {
		t.Run(fmt.Sprint(tc.source, "-", tc.endReason), func(t *testing.T) {
			registered, stopped := liveKernel(t, tc.source)
			finalizeSession(context.Background(), kernelSession(), tc.endReason)
			if registered() || !stopped() || tools.IsSessionYoloEnabled(kernelSessionKey) {
				t.Fatalf("registered %v stopped %v yolo %v", registered(), stopped(), tools.IsSessionYoloEnabled(kernelSessionKey))
			}
		})
	}
}

func TestViewerFinalizeKeepsAGatewayOwnedSessionsKernels(t *testing.T) {
	registered, stopped := liveKernel(t, "telegram")
	finalizeSession(context.Background(), kernelSession(), "tui_shutdown")
	if !registered() || stopped() || !tools.IsSessionYoloEnabled(kernelSessionKey) {
		t.Fatalf("registered %v stopped %v yolo %v", registered(), stopped(), tools.IsSessionYoloEnabled(kernelSessionKey))
	}
}

package tuiserver

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/fakellm"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

const closingSID = "closing-sid"

// Session record of the closing cases with the overrides
func closingSession(extra map[string]any) *tuigateway.Session {
	fields := map[string]any{"agent": nil, "session_key": "closing-session-key", "history": []any{}, "history_lock": &sync.Mutex{},
		"history_version": int64(0), "running": false, "attached_images": []any{}, "image_counter": 0, "cols": 80, "slash_worker": nil,
		"show_reasoning": false, "tool_progress_mode": "all"}
	for key, value := range extra {
		fields[key] = value
	}
	return tuigateway.NewSession(fields)
}

// Home with no settle grace before a close finalizes, every lease of the case released after
func closingTurnEnv(t *testing.T) (string, *[]*tuigateway.Session) {
	t.Helper()
	h := sessionCaseScope(t)
	useSharedDB(t, reaperTestDB(t))
	swapVar(t, &turnSettleBeforeCloseSeconds, 0.0)
	sessions := &[]*tuigateway.Session{}
	t.Cleanup(func() {
		for _, session := range *sessions {
			releaseActiveSessionSlot(session)
		}
		tuigateway.SetSessionRecord(closingSID, nil)
	})
	return h, sessions
}

// Real agent of the closing key on a scripted provider whose requests count the turns run
func closingBuiltAgent(t *testing.T) (*agent.AIAgent, *fakellm.Server) {
	t.Helper()
	srv, err := fakellm.New([]any{fakellm.Text{Text: "done"}}, fakellm.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	return testAgent(t, agent.AIAgentOptions{Model: fakellm.ModelID, Provider: "custom", BaseURL: srv.BaseURL(), APIMode: "chat_completions",
		SessionID: "closing-session-key", Platform: "tui"}), srv
}

func closingRPC(method string, params map[string]any) map[string]any {
	merged := map[string]any{"session_id": closingSID}
	for key, value := range params {
		merged[key] = value
	}
	return handleRequest(context.Background(), map[string]any{"id": method, "method": method, "params": merged})
}

// Session ids holding a registry lease of the home
func leaseIDs(t *testing.T, h string) []any {
	t.Helper()
	var ids []any
	for _, entry := range registryEntries(t, h) {
		ids = append(ids, entry["session_id"])
	}
	return ids
}

// Refusal a new runtime of the same chat gets, a successful claim released again
func reopenRefusal(ctx context.Context) *activesessions.Refusal {
	reopened := closingSession(nil)
	refusal := ensureActiveSessionSlot(ctx, "reopened-sid", reopened)
	releaseActiveSessionSlot(reopened)
	return refusal
}

// Waits until the turn thread of the session ended and the session stopped running
func joinTurn(t *testing.T, session *tuigateway.Session) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		thread, _ := session.Get("_run_thread").(*memory.ContextThread)
		if (thread == nil || !thread.Alive()) && !truthy(session.Get("running")) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("turn thread still alive")
		}
	}
}

func TestCloseWhileFirstPromptWaitsForBuildLeavesNoLease(t *testing.T) {
	h, sessions := closingTurnEnv(t)
	buildDone := &pythread.Event{}
	session := closingSession(map[string]any{"agent_ready": buildDone, "agent_build_started": true})
	*sessions = append(*sessions, session)
	tuigateway.SetSessionRecord(closingSID, session)
	submitted := closingRPC("prompt.submit", map[string]any{"text": "hello"})
	if resultOf(t, submitted)["status"] != "streaming" {
		t.Fatalf("submitted %v", submitted)
	}
	if ids := leaseIDs(t, h); len(ids) != 1 || ids[0] != "closing-session-key" {
		t.Fatalf("leases %v", ids)
	}
	if resultOf(t, closingRPC("session.close", nil))["closed"] != true {
		t.Fatal("close refused")
	}
	if ids := leaseIDs(t, h); len(ids) != 0 {
		t.Fatalf("leases after close %v", ids)
	}
	built, srv := closingBuiltAgent(t)
	session.Set("agent", built)
	buildDone.Set()
	joinTurn(t, session)
	if ids := leaseIDs(t, h); len(ids) != 0 || reopenRefusal(context.Background()) != nil || truthy(session.Get("running")) || len(srv.MainRequests()) != 0 {
		t.Fatalf("leases %v running %v turns %d", ids, session.Get("running"), len(srv.MainRequests()))
	}
}

func TestCloseLandingWhileTurnClaimsLeaseLeavesNoLease(t *testing.T) {
	h, sessions := closingTurnEnv(t)
	ready := &pythread.Event{}
	ready.Set()
	built, srv := closingBuiltAgent(t)
	session := closingSession(map[string]any{"agent": built, "agent_ready": ready})
	*sessions = append(*sessions, session)
	tuigateway.SetSessionRecord(closingSID, session)
	claim := ensureActiveSessionSlot
	atClaim := map[string]any{}
	var once sync.Once
	swapVar(t, &ensureActiveSessionSlot, func(ctx context.Context, sid string, claiming *tuigateway.Session) *activesessions.Refusal {
		stack := make([]byte, 1<<16)
		if strings.Contains(string(stack[:runtime.Stack(stack, false)]), "admitPromptTurn") && !truthy(claiming.Get("_closing")) {
			once.Do(func() {
				atClaim["closed"] = resultOf(t, closingRPC("session.close", nil))["closed"]
				atClaim["leases"] = len(leaseIDs(t, h))
			})
		}
		return claim(ctx, sid, claiming)
	})
	submitted := closingRPC("prompt.submit", map[string]any{"text": "hello"})
	if resultOf(t, submitted)["status"] != "streaming" {
		t.Fatalf("submitted %v", submitted)
	}
	joinTurn(t, session)
	ensureActiveSessionSlot = claim
	if atClaim["closed"] != true || atClaim["leases"] != 0 {
		t.Fatalf("at claim %v", atClaim)
	}
	if ids := leaseIDs(t, h); len(ids) != 0 || reopenRefusal(context.Background()) != nil || truthy(session.Get("running")) || len(srv.MainRequests()) != 0 {
		t.Fatalf("leases %v running %v turns %d", ids, session.Get("running"), len(srv.MainRequests()))
	}
}

func TestClosingRefusalKeepsALeaseTheSessionAlreadyHeld(t *testing.T) {
	h, sessions := closingTurnEnv(t)
	ctx := context.Background()
	session := closingSession(nil)
	*sessions = append(*sessions, session)
	if refusal := ensureActiveSessionSlot(ctx, closingSID, session); refusal != nil {
		t.Fatalf("claim %v", refusal)
	}
	held := session.Get("active_session_lease")
	session.Set("_closing", true)
	if _, _, admitted := admitPromptTurn(ctx, closingSID, session, "hello", nil, nil, "", nil, true); admitted {
		t.Fatal("closing session admitted a turn")
	}
	if session.Get("active_session_lease") != held {
		t.Fatal("held lease released")
	}
	if ids := leaseIDs(t, h); len(ids) != 1 || ids[0] != "closing-session-key" {
		t.Fatalf("leases %v", ids)
	}
}

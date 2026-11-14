package tuiserver

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Already set agent ready event of a built session
func readyEvent() *pythread.Event {
	ready := &pythread.Event{}
	ready.Set()
	return ready
}

// Transport whose socket already closed
type closedPeer struct{}

func (*closedPeer) Write(*pyjson.Object) bool { return false }
func (*closedPeer) Close()                    {}
func (*closedPeer) Closed() bool              { return true }

func TestEnforceSessionCapEvictsOldestDetachedOnly(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeFile(t, filepath.Join(h, "config.yaml"), "max_live_sessions: 2\n")
	useSharedDB(t, ledgerHandle(t, filepath.Join(h, "state.db")))
	detached := tuigateway.DetachedWSTransport()
	record := func(fields map[string]any) *tuigateway.Session {
		fields["agent_ready"] = readyEvent()
		return tuigateway.NewSession(fields)
	}
	swapSessions(t, map[string]*tuigateway.Session{
		"old_detached":     record(map[string]any{"transport": detached, "last_active": 100.0}),
		"new_detached":     record(map[string]any{"transport": detached, "last_active": 300.0}),
		"running_detached": record(map[string]any{"transport": detached, "last_active": 50.0, "running": true}),
		"focused_live":     record(map[string]any{"transport": &orphanRacePeer{id: 20}, "last_active": 200.0}),
	})
	enforceSessionCap(context.Background())
	for sid, kept := range map[string]bool{"old_detached": false, "new_detached": false, "running_detached": true, "focused_live": true} {
		if (tuigateway.SessionRecord(sid) != nil) != kept {
			t.Fatalf("%s kept=%v", sid, !kept)
		}
	}
}

func TestIdleReaperRearmsMissingWsOrphanTimer(t *testing.T) {
	for _, closedTransport := range []bool{false, true} {
		t.Run(map[bool]string{false: "detached", true: "closed"}[closedTransport], func(t *testing.T) {
			ctx := context.Background()
			h := filepath.Join(t.TempDir(), ".icebath")
			if err := os.MkdirAll(h, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ICEBATH_HOME", h)
			useSharedDB(t, ledgerHandle(t, filepath.Join(h, "state.db")))
			sid, siblingSID := "detached-without-reaper", "live-sibling"
			orphanLease, refusal, err := activesessions.TryAcquire(ctx, sid, "desktop", map[string]any{}, nil, h, true)
			if err != nil || orphanLease == nil || refusal != nil {
				t.Fatalf("orphan lease %v %v %v", orphanLease, refusal, err)
			}
			siblingLease, refusal, err := activesessions.TryAcquire(ctx, siblingSID, "desktop", map[string]any{}, nil, h, true)
			if err != nil || siblingLease == nil || refusal != nil {
				t.Fatalf("sibling lease %v %v %v", siblingLease, refusal, err)
			}
			t.Cleanup(func() { _ = siblingLease.Release() })
			record := func(key string, lease *activesessions.Lease, transport tuigateway.Transport) *tuigateway.Session {
				now := nowSeconds()
				return tuigateway.NewSession(map[string]any{"active_session_lease": lease, "created_at": now, "history": []any{},
					"last_active": now, "session_key": key, "source": "tui", "transport": transport})
			}
			var dead tuigateway.Transport = tuigateway.DetachedWSTransport()
			if closedTransport {
				dead = &closedPeer{}
			}
			swapSessions(t, map[string]*tuigateway.Session{sid: record(sid, orphanLease, dead), siblingSID: record(siblingSID, siblingLease, &orphanRacePeer{id: 21})})
			t.Cleanup(func() { cancelWSOrphanReap(sid) })
			prevGrace, prevTTL := wsOrphanReapGraceS, sessionTTLS
			wsOrphanReapGraceS, sessionTTLS = func() float64 { return 0.05 }, 3600
			t.Cleanup(func() { wsOrphanReapGraceS, sessionTTLS = prevGrace, prevTTL })
			reapIdleSessions(ctx)
			deadline := time.Now().Add(2 * time.Second)
			for (tuigateway.SessionRecord(sid) != nil || !orphanLease.Released()) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if tuigateway.SessionRecord(sid) != nil || !orphanLease.Released() {
				t.Fatal("detached lane kept its record or lease")
			}
			if tuigateway.SessionRecord(siblingSID) == nil {
				t.Fatal("live sibling was reaped")
			}
			assertRegistrySessions := func() {
				t.Helper()
				entries := registryEntries(t, h)
				if len(entries) != 1 || entries[0]["session_id"] != siblingSID {
					t.Fatalf("registry %v", entries)
				}
			}
			assertRegistrySessions()
			spawnLeaseHolder(t, h, sid).finish(t)
			assertRegistrySessions()
		})
	}
}

func TestSyncSessionKeyAfterCompressReanchorsActiveSessionLease(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	rotated := testAgent(t, agent.AIAgentOptions{})
	rotated.SetSessionID("session-new")
	lease, refusal, err := activesessions.TryAcquire(ctx, "session-old", "tui", map[string]any{"max_concurrent_sessions": 1},
		map[string]any{"live_session_id": "ui-1"}, "", false)
	if err != nil || refusal != nil || lease == nil {
		t.Fatalf("lease %v %v %v", lease, refusal, err)
	}
	t.Cleanup(func() { _ = lease.Release() })
	session := tuigateway.NewSession(map[string]any{"active_session_lease": lease,
		"agent": rotated, "session_key": "session-old"})
	syncSessionKeyAfterCompress(ctx, "ui-1", session, true, false)
	if session.Get("session_key") != "session-new" || lease.SessionID() != "session-new" {
		t.Fatalf("session key %v lease %v", session.Get("session_key"), lease.SessionID())
	}
	entries := registryEntries(t, "")
	if len(entries) != 1 || entries[0]["session_id"] != "session-new" {
		t.Fatalf("registry %v", entries)
	}
}

func TestMakeAgentAcceptsListSystemPrompt(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeFile(t, filepath.Join(h, "config.yaml"), "model:\n  default: test/model\n  provider: custom\n  base_url: http://127.0.0.1:9/v1\n  api_key: test\n"+
		"agent:\n  system_prompt:\n    - one\n    - two\n")
	useSharedDB(t, ledgerHandle(t, filepath.Join(h, "state.db")))
	built, err := makeAgent(context.Background(), "sid", "session-key", makeAgentOptions{sessionID: "session-key"})
	if err != nil {
		t.Fatal(err)
	}
	a, isAgent := built.(*agent.AIAgent)
	if !isAgent {
		t.Fatalf("built %T %v", built, built)
	}
	if a.EphemeralSystemPrompt() != "one\ntwo" {
		t.Fatalf("prompt %q", a.EphemeralSystemPrompt())
	}
}

func TestConfigRoundtrip(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if err := saveCfg(ctx, map[string]any{"model": "test/model"}); err != nil {
		t.Fatal(err)
	}
	if got := loadCfg(ctx)["model"]; got != "test/model" {
		t.Fatalf("model %v", got)
	}
}

func TestCliExecBlocked(t *testing.T) {
	for _, argv := range [][]string{{}, {"setup"}, {"gateway"}, {"sessions", "browse"}, {"config", "edit"}} {
		if cliExecBlocked(argv) == "" {
			t.Fatalf("%v not blocked", argv)
		}
	}
}

// session.create through the real dispatcher under a fresh home with synthetic agent builds
func createCall(t *testing.T, rid string, params map[string]any) map[string]any {
	t.Helper()
	return handleRequest(context.Background(), map[string]any{"id": rid, "method": "session.create", "params": maps.Clone(params)})
}

// Created session id of a successful create
func createdSID(t *testing.T, resp map[string]any) string {
	t.Helper()
	if _, failed := resp["error"]; failed {
		t.Fatalf("create failed %v", resp)
	}
	sid, _ := resultOf(t, resp)["session_id"].(string)
	if sid == "" {
		t.Fatalf("no session id in %v", resp)
	}
	return sid
}

// Isolated home whose live registry starts empty
func idempotencyScope(t *testing.T) {
	t.Helper()
	sessionCaseScope(t)
	swapSessions(t, map[string]*tuigateway.Session{})
}

func TestSessionCreateIdempotencyKeyDedupesRetry(t *testing.T) {
	idempotencyScope(t)
	params := map[string]any{"cols": 96, "source": "desktop", "messages": []any{map[string]any{"role": "user", "content": "branch me"}},
		"parent_session_id": "parent-1", "idempotency_key": "branch-retry-abc"}
	first := createdSID(t, createCall(t, "c1", params))
	if n := len(tuigateway.LiveSessions()); n != 1 {
		t.Fatalf("%d live sessions", n)
	}
	if second := createdSID(t, createCall(t, "c2", params)); second != first {
		t.Fatalf("retry created %s, first %s", second, first)
	}
	if n := len(tuigateway.LiveSessions()); n != 1 {
		t.Fatalf("%d live sessions after retry", n)
	}
}

func TestSessionCreateNoIdempotencyKeyCreatesDistinctSessions(t *testing.T) {
	idempotencyScope(t)
	params := map[string]any{"cols": 96, "source": "desktop"}
	first, second := createdSID(t, createCall(t, "c1", params)), createdSID(t, createCall(t, "c2", params))
	if first == second || len(tuigateway.LiveSessions()) != 2 {
		t.Fatalf("sessions %s %s live %d", first, second, len(tuigateway.LiveSessions()))
	}
}

func TestSessionCreateIdempotencyKeyExpiresWithSession(t *testing.T) {
	idempotencyScope(t)
	params := map[string]any{"cols": 96, "source": "desktop", "idempotency_key": "branch-retry-xyz"}
	first := createdSID(t, createCall(t, "c1", params))
	tuigateway.SetSessionRecord(first, nil)
	second := createdSID(t, createCall(t, "c2", params))
	if second == first || len(tuigateway.LiveSessions()) != 1 {
		t.Fatalf("second %s first %s live %d", second, first, len(tuigateway.LiveSessions()))
	}
}

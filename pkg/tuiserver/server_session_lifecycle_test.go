package tuiserver

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Level and text of every record logged while the case runs
type levelCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *levelCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *levelCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
	return nil
}
func (c *levelCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *levelCapture) WithGroup(string) slog.Handler      { return c }

// Whether a record at or above level holds every needle
func (c *levelCapture) has(level slog.Level, needles ...string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		if r.Level < level {
			continue
		}
		matched := true
		for _, needle := range needles {
			matched = matched && strings.Contains(r.Message, needle)
		}
		if matched {
			return true
		}
	}
	return false
}

func captureLevels(t *testing.T) *levelCapture {
	t.Helper()
	capture := &levelCapture{}
	saved := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(saved) })
	return capture
}

// Transport that holds the first resume loading frame until released, the pause inside the history read
type heldLoadingTransport struct {
	started, release chan struct{}
	once             sync.Once
}

func (h *heldLoadingTransport) Write(obj *pyjson.Object) bool {
	frame, _ := pyjson.Plain(obj).(map[string]any)
	params, _ := frame["params"].(map[string]any)
	payload, _ := params["payload"].(map[string]any)
	if params["type"] == "session.resume_progress" && payload["status"] == "loading" {
		h.once.Do(func() {
			close(h.started)
			<-h.release
		})
	}
	return true
}
func (h *heldLoadingTransport) Close() {}

func TestSessionSlotIsClaimedOnFirstTurnNotOnCreate(t *testing.T) {
	resumeDB(t)
	h := os.Getenv("ICEBATH_HOME")
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("model:\n  default: test-model\nmax_concurrent_sessions: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	swapSessions(t, map[string]*tuigateway.Session{})
	ctx := context.Background()
	sid := createdSID(t, createCall(t, "r1", map[string]any{"cols": 80}))
	other := createdSID(t, createCall(t, "r2", map[string]any{"cols": 80}))
	if entries := registryEntries(t, h); len(entries) != 0 {
		t.Fatalf("create claimed %v", entries)
	}
	for range 2 {
		if refusal := ensureActiveSessionSlot(ctx, sid, tuigateway.SessionRecord(sid)); refusal != nil {
			t.Fatalf("first turn refused %v", refusal)
		}
	}
	if entries := registryEntries(t, h); len(entries) != 1 {
		t.Fatalf("entries %v", entries)
	}
	if refusal := ensureActiveSessionSlot(ctx, other, tuigateway.SessionRecord(other)); refusal == nil {
		t.Fatal("second session claimed past the cap")
	}
	closed := handleRequest(ctx, map[string]any{"id": "r3", "method": "session.close", "params": map[string]any{"session_id": sid}})
	if resultOf(t, closed)["closed"] != true {
		t.Fatalf("close %v", closed)
	}
	if entries := registryEntries(t, h); len(entries) != 0 {
		t.Fatalf("close kept %v", entries)
	}
	if refusal := ensureActiveSessionSlot(ctx, other, tuigateway.SessionRecord(other)); refusal != nil {
		t.Fatalf("freed slot refused %v", refusal)
	}
}

func TestPromptSubmitUnknownSessionLogsWarning(t *testing.T) {
	sessionCaseScope(t)
	swapSessions(t, map[string]*tuigateway.Session{})
	logs := captureLevels(t)
	resp := handleRequest(context.Background(), map[string]any{"id": "r1", "method": "prompt.submit", "params": map[string]any{"session_id": "gone-sid", "text": "hello"}})
	if resp["id"] != "r1" || resumeErrCode(resp) != 4001 {
		t.Fatalf("resp %v", resp)
	}
	if !logs.has(slog.LevelWarn, "gone-sid", "prompt.submit") {
		t.Fatal("no warning names the runtime id and method")
	}
}

func TestSessionResumeDeferredHistoryCloseCancelsBuild(t *testing.T) {
	db := resumeDB(t)
	for _, key := range []string{"ICEBATH_GATEWAY_SESSION", "ICEBATH_EXEC_ASK", "ICEBATH_INTERACTIVE"} {
		t.Setenv(key, "")
	}
	storeTranscript(t, db, "cancel-session", "", map[string]any{"role": "user", "content": "late"})
	swapSessions(t, map[string]*tuigateway.Session{})
	held := &heldLoadingTransport{started: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-held.release:
		default:
			close(held.release)
		}
	}()
	ctx := tuigateway.BindTransport(context.Background(), held)
	resp := handleRequest(ctx, map[string]any{"id": "r1", "method": "session.resume", "params": map[string]any{"session_id": "cancel-session", "defer_history": true}})
	sid, _ := resultOf(t, resp)["session_id"].(string)
	session := tuigateway.SessionRecord(sid)
	select {
	case <-held.started:
	case <-time.After(time.Second):
		t.Fatal("history load never started")
	}
	if !closeSessionByID(context.Background(), sid, "tui_close", nil) {
		t.Fatal("close refused")
	}
	ready, _ := session.Get("resume_history_ready").(*pythread.Event)
	if ready == nil || !ready.IsSet() || session.Get("resume_history_error") != "session resume cancelled" {
		t.Fatalf("ready %v error %v", ready, session.Get("resume_history_error"))
	}
	close(held.release)
	time.Sleep(50 * time.Millisecond)
	built, _ := session.Get("agent_ready").(*pythread.Event)
	if (built != nil && built.IsSet()) || session.Get("agent") != nil {
		t.Fatal("agent build started after close")
	}
	if tuigateway.SessionRecord(sid) != nil {
		t.Fatal("closed session still registered")
	}
}

// Waits until a goroutine whose stack names fn is parked on a lock, the interleave point of a race case
func waitBlockedIn(t *testing.T, fn string) {
	t.Helper()
	buf := make([]byte, 1<<20)
	var seen []string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		seen = nil
		for _, stack := range strings.Split(string(buf[:runtime.Stack(buf, true)]), "\n\n") {
			if !strings.Contains(stack, fn) {
				continue
			}
			header, _, _ := strings.Cut(stack, "\n")
			if strings.Contains(header, "[sync.Mutex.Lock") || strings.Contains(header, "[sync.Cond.Wait") || strings.Contains(header, "[sync.RWMutex") {
				return
			}
			seen = append(seen, stack)
		}
	}
	t.Fatalf("no goroutine parked in %s %v", fn, seen)
}

// Transport that holds the message start frame of a turn until released
type heldStartTransport struct {
	started, release chan struct{}
	once             sync.Once
}

func (h *heldStartTransport) Write(obj *pyjson.Object) bool {
	frame, _ := pyjson.Plain(obj).(map[string]any)
	if params, _ := frame["params"].(map[string]any); params["type"] == "message.start" {
		h.once.Do(func() {
			close(h.started)
			<-h.release
		})
	}
	return true
}
func (h *heldStartTransport) Close() {}

func TestRunPromptSubmitRejectsWorkerWhenCloseWinsPublication(t *testing.T) {
	hz := newTurnHarness(t, []any{fakellm.Text{Text: "must not run"}}, "")
	held := &heldStartTransport{started: make(chan struct{}), release: make(chan struct{})}
	hz.session.Set("transport", held)
	hz.session.Set("running", true)
	result := make(chan bool, 1)
	go func() { result <- runPromptSubmit(hz.ctx, "rid", hz.sid, hz.session, "turn", promptSubmitOptions{}) }()
	select {
	case <-held.started:
	case <-time.After(10 * time.Second):
		close(held.release)
		t.Fatal("message.start never published")
	}
	popped := popSessionByID(context.Background(), hz.sid)
	close(held.release)
	if popped != hz.session {
		t.Fatalf("popped %v", popped)
	}
	select {
	case got := <-result:
		if got {
			t.Fatal("worker dispatched after the close won")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("dispatch never returned")
	}
	if hz.session.Get("running") != false || len(hz.srv.MainRequests()) != 0 {
		t.Fatalf("running %v turns %d", hz.session.Get("running"), len(hz.srv.MainRequests()))
	}
}

func TestPromptSubmitRowIDAcceptsFullLineageOrdinal(t *testing.T) {
	db, _ := submitCaseDB(t, "session-key")
	seedRowsWithIDs(t, db, "session-key", [3]any{int64(501), "user", "post-compress A"}, [3]any{int64(502), "assistant", "reply A"},
		[3]any{int64(503), "user", "post-compress B"}, [3]any{int64(504), "assistant", "reply B"})
	tip := func() []any {
		return []any{map[string]any{"_row_id": int64(501), "role": "user", "content": "post-compress A"},
			map[string]any{"_row_id": int64(502), "role": "assistant", "content": "reply A"},
			map[string]any{"_row_id": int64(503), "role": "user", "content": "post-compress B"},
			map[string]any{"_row_id": int64(504), "role": "assistant", "content": "reply B"}}
	}
	prefix := func() []any {
		return []any{map[string]any{"role": "user", "content": "pre-compress 1"}, map[string]any{"role": "assistant", "content": "pre reply 1"},
			map[string]any{"role": "user", "content": state.SummaryPrefix + "\n" + state.HistoricalTaskHeading + "\nold task\n\n" + state.SummaryEndMarker},
			map[string]any{"role": "user", "content": "pre-compress 2"}, map[string]any{"role": "assistant", "content": "pre reply 2"}}
	}
	session := parkedSubmitSession(t, "lineage-row-sid", map[string]any{"history": tip(), "display_history_prefix": prefix()})
	resp := submitRPC("lineage-row-sid", map[string]any{"text": "edited post B", "truncate_before_row_id": 503,
		"truncate_before_user_ordinal": 3, "confirm_truncate": true})
	if resp["error"] != nil {
		t.Fatalf("got error %v", resp["error"])
	}
	unlock := lockHistory(session)
	history := session.Get("history")
	unlock()
	kept, _ := history.([]any)
	for _, row := range kept {
		delete(row.(map[string]any), "message_uid")
	}
	if !pyjson.Equal(kept, tip()[:2]) {
		t.Fatalf("history %v", history)
	}
	releaseActiveSessionSlot(session)
	tuigateway.SetSessionRecord("lineage-row-sid", nil)
	stale := parkedSubmitSession(t, "lineage-row-sid-2", map[string]any{"history": tip(), "display_history_prefix": prefix()})
	t.Cleanup(func() { releaseActiveSessionSlot(stale) })
	refused := rpcErrorOf(t, submitRPC("lineage-row-sid-2", map[string]any{"text": "edited post B", "truncate_before_row_id": 503,
		"truncate_before_user_ordinal": 2, "confirm_truncate": true}))
	if refused["code"] != 4030 || truthy(stale.Get("running")) {
		t.Fatalf("refused %v running %v", refused, stale.Get("running"))
	}
}

// Launch home with an mlperf profile and a clean hosting latch
func mlperfProfile(t *testing.T) (string, string) {
	t.Helper()
	launch := sessionCaseScope(t)
	isolateProfileHosting(t)
	profile := filepath.Join(launch, "profiles", "mlperf")
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{launch, profile} {
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("model:\n  default: test-model\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return launch, profile
}

func TestSessionCreateReportsRequestedProfileName(t *testing.T) {
	_, profile := mlperfProfile(t)
	useSharedDB(t, reaperTestDB(t))
	swapSessions(t, map[string]*tuigateway.Session{})
	resp := createCall(t, "r1", map[string]any{"profile": "mlperf", "cols": 80})
	result := resultOf(t, resp)
	info, _ := result["info"].(map[string]any)
	if info["profile_name"] != "mlperf" {
		t.Fatalf("info %v", info)
	}
	sid, _ := result["session_id"].(string)
	if got := tuigateway.SessionRecord(sid).Get("profile_home"); got != profile {
		t.Fatalf("profile_home %v", got)
	}
}

func TestTeardownEndsSessionInProfileDB(t *testing.T) {
	_, profile := mlperfProfile(t)
	launchDB := reapLedger(t, "ml-sess")
	profileDB, err := state.NewSessionDB(context.Background(), filepath.Join(profile, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := profileDB.CreateSession(state.NewSession{ID: "ml-sess", Source: "tui"}); err != nil {
		t.Fatal(err)
	}
	profileDB.Close()
	session := tuigateway.NewSession(map[string]any{"session_key": "ml-sess", "profile_home": profile, "agent": nil, "history": []any{}, "source": "tui"})
	teardownSession(context.Background(), session, "closed")
	if reason := storedEndReason(t, launchDB, "ml-sess"); reason != "" {
		t.Fatalf("launch store ended %q", reason)
	}
	reopened, err := state.NewSessionDB(context.Background(), filepath.Join(profile, "state.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reason := storedEndReason(t, reopened, "ml-sess"); reason != "closed" {
		t.Fatalf("profile store end reason %q", reason)
	}
}

func TestCloseSessionsForTransportSkipsSessionReboundBeforeClaim(t *testing.T) {
	for _, closeOnDisconnect := range []bool{true, false} {
		t.Run(fmt.Sprint(closeOnDisconnect), func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			oldTransport, newTransport := &liveWritePeer{id: 1}, &liveWritePeer{id: 2}
			session := tuigateway.NewSession(map[string]any{"transport": oldTransport, "close_on_disconnect": closeOnDisconnect})
			swapSessions(t, map[string]*tuigateway.Session{"rebound": session})
			sessionResumeLock.Lock()
			type counts struct{ reaped, detached int }
			done := make(chan counts, 1)
			go func() {
				reaped, detached := closeSessionsForTransport(context.Background(), oldTransport, "ws_disconnect")
				done <- counts{reaped, detached}
			}()
			waitBlockedIn(t, "closeSessionsForTransport")
			session.Set("transport", newTransport)
			sessionResumeLock.Unlock()
			got := <-done
			if got.reaped != 0 || got.detached != 0 {
				t.Fatalf("counts %v", got)
			}
			if tuigateway.SessionRecord("rebound") != session || session.Get("transport") != newTransport || truthy(session.Get("_finalized")) {
				t.Fatal("rebound session was torn down or detached")
			}
			if pendingReap("rebound") != nil {
				t.Fatal("rebound session got an orphan reap")
			}
		})
	}
}

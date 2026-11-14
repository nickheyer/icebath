package tuiserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

type bareBusyAgent struct{}

type steeringBusyAgent struct {
	mu   sync.Mutex
	seen []string
}

func (a *steeringBusyAgent) Steer(text string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = append(a.seen, text)
	return true
}

type redirectingBusyAgent struct {
	mu   sync.Mutex
	seen []string
}

func (a *redirectingBusyAgent) Redirect(text string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = append(a.seen, text)
	return true
}

func (a *redirectingBusyAgent) SupportsActiveTurnRedirect() bool { return true }

// Running session record of the hermes fixture, holding the given agent and extra fields
func busySession(agentObj any, extra map[string]any) *tuigateway.Session {
	if agentObj == nil {
		agentObj = &bareBusyAgent{}
	}
	fields := map[string]any{"agent": agentObj, "session_key": "session-key", "history": []any{}, "history_lock": &sync.Mutex{},
		"history_version": 0, "running": true, "transport": nil, "attached_images": []string{}}
	for k, v := range extra {
		fields[k] = v
	}
	return tuigateway.NewSession(fields)
}

// Isolated home whose display config picks the busy mode and whose store holds a compression lock when asked
func busyHome(t *testing.T, mode string, compressing bool) string {
	t.Helper()
	h := sessionCaseScope(t)
	if mode != "" {
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("display:\n  busy_input_mode: "+mode+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if compressing {
		holdCompressionLock(t, h, "session-key", "compressor-1")
	}
	return h
}

// Takes the compression lock of a session in the store of a profile home
func holdCompressionLock(t *testing.T, profileHome, sessionID, holder string) {
	t.Helper()
	db, err := state.Acquire(context.Background(), filepath.Join(profileHome, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.ReleaseOrClose(db)
	if !db.TryAcquireCompressionLock(sessionID, holder, 600) {
		t.Fatal("compression lock not acquired")
	}
}

func TestCompressionInFlightTrueWhenLockHeld(t *testing.T) {
	h := busyHome(t, "", true)
	session := busySession(nil, map[string]any{"profile_home": h})
	if !sessionCompressionInFlight(context.Background(), session) {
		t.Fatal("held compression lock not seen")
	}
}

func TestCompressionInFlightFalseWhenLockFree(t *testing.T) {
	h := busyHome(t, "", false)
	if sessionCompressionInFlight(context.Background(), busySession(nil, map[string]any{"profile_home": h})) {
		t.Fatal("free lock reported in flight")
	}
}

func TestCompressionInFlightFalseWhenDbErrors(t *testing.T) {
	h := busyHome(t, "", false)
	gone := filepath.Join(h, "db-gone")
	if err := os.WriteFile(gone, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if sessionCompressionInFlight(context.Background(), busySession(nil, map[string]any{"profile_home": gone})) {
		t.Fatal("failed check did not fail open")
	}
}

// Result member of a busy submit response
func busyResult(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	result, isMap := resp["result"].(map[string]any)
	if !isMap {
		t.Fatalf("no result in %v", resp)
	}
	return result
}

// Text of the queued prompt envelope of a session
func queuedPromptText(session *tuigateway.Session) any {
	q, _ := session.Get("queued_prompt").(map[string]any)
	return q["text"]
}

func TestBusyInterruptModeQueuesWhileCompressing(t *testing.T) {
	h := busyHome(t, "interrupt", true)
	a := &redirectingBusyAgent{}
	session := busySession(a, map[string]any{"profile_home": h})
	resp := handleBusySubmit(context.Background(), "r1", "sid", session, "follow-up", "ws-1", false, nil, "")
	if got := busyResult(t, resp)["status"]; got != "queued" {
		t.Fatalf("status %v", got)
	}
	if len(a.seen) != 0 {
		t.Fatalf("redirected %v", a.seen)
	}
	if got := queuedPromptText(session); got != "follow-up" {
		t.Fatalf("queued %v", got)
	}
}

func TestBusySteerModeQueuesWhileCompressing(t *testing.T) {
	h := busyHome(t, "steer", true)
	a := &steeringBusyAgent{}
	session := busySession(a, map[string]any{"profile_home": h})
	resp := handleBusySubmit(context.Background(), "r1", "sid", session, "follow-up", "ws-1", false, nil, "")
	if got := busyResult(t, resp)["status"]; got != "queued" {
		t.Fatalf("status %v", got)
	}
	if len(a.seen) != 0 {
		t.Fatalf("steered %v", a.seen)
	}
	if got := queuedPromptText(session); got != "follow-up" {
		t.Fatalf("queued %v", got)
	}
}

func TestBusyInterruptModeStillRedirectsWithoutCompression(t *testing.T) {
	h := busyHome(t, "interrupt", false)
	a := &redirectingBusyAgent{}
	session := busySession(a, map[string]any{"profile_home": h})
	resp := handleBusySubmit(context.Background(), "r1", "sid", session, "correction", "ws-1", false, nil, "")
	if got := busyResult(t, resp)["status"]; got != "redirected" {
		t.Fatalf("status %v", got)
	}
	if !reflect.DeepEqual(a.seen, []string{"correction"}) {
		t.Fatalf("redirects %v", a.seen)
	}
}

func TestBusyQueuedDrainForcesQueueRegardless(t *testing.T) {
	h := busyHome(t, "interrupt", false)
	session := busySession(nil, map[string]any{"profile_home": h})
	session.Set("queued_prompt", map[string]any{"text": "earlier", "transport": "ws-1"})
	resp := handleBusySubmit(context.Background(), "r1", "sid", session, "next", "ws-1", true, nil, "")
	if got := busyResult(t, resp)["status"]; got != "queued" {
		t.Fatalf("status %v", got)
	}
}

// Sends one correction RPC for a registered compressing session through the request handler
func busyCorrectionRPC(t *testing.T, method string, agentObj any) (map[string]any, *tuigateway.Session) {
	t.Helper()
	h := busyHome(t, "", true)
	session := busySession(agentObj, map[string]any{"profile_home": h})
	tuigateway.SetSessionRecord("sid", session)
	defer tuigateway.SetSessionRecord("sid", nil)
	return handleRequest(context.Background(), map[string]any{"id": "r1", "method": method,
		"params": map[string]any{"session_id": "sid", "text": "follow-up"}}), session
}

func TestSessionSteerQueuesWhileCompressing(t *testing.T) {
	a := &steeringBusyAgent{}
	resp, session := busyCorrectionRPC(t, "session.steer", a)
	if got := busyResult(t, resp)["status"]; got != "queued" {
		t.Fatalf("status %v", got)
	}
	if len(a.seen) != 0 {
		t.Fatalf("steered %v", a.seen)
	}
	if got := queuedPromptText(session); got != "follow-up" {
		t.Fatalf("queued %v", got)
	}
}

func TestSessionRedirectQueuesWhileCompressing(t *testing.T) {
	a := &redirectingBusyAgent{}
	resp, _ := busyCorrectionRPC(t, "session.redirect", a)
	if got := busyResult(t, resp)["status"]; got != "queued" {
		t.Fatalf("status %v", got)
	}
	if len(a.seen) != 0 {
		t.Fatalf("redirected %v", a.seen)
	}
}

// Payloads of the session.info events a recording transport saw, in order
func sessionInfoPayloads(rec *recordingTransport) []map[string]any {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var infos []map[string]any
	for _, frame := range rec.frames {
		params, _ := frame["params"].(map[string]any)
		if params["type"] == "session.info" {
			payload, _ := params["payload"].(map[string]any)
			infos = append(infos, payload)
		}
	}
	return infos
}

func TestSubmitDuringManualCompressIsQueuedAndReplyPersists(t *testing.T) {
	sessionCaseScope(t)
	ctx := context.Background()
	gate, entered := make(chan struct{}), make(chan struct{})
	prevCompress := compressNow
	compressNow = func(_ context.Context, _ *agent.AIAgent, msgs []map[string]any, request agent.CompressRequest, _ agent.CompressNowOptions) (agent.CompressResult, error) {
		close(entered)
		select {
		case <-gate:
		case <-time.After(5 * time.Second):
		}
		after := append([]map[string]any{{"role": "user", "content": "[summary]"}}, msgs[len(msgs)-2:]...)
		return agent.CompressResult{Status: "compressed", BeforeMessages: msgs, AfterMessages: after, Request: request}, nil
	}
	t.Cleanup(func() { compressNow = prevCompress })
	prevRun := runPromptSubmit
	runPromptSubmit = func(_ context.Context, _ any, _ string, session *tuigateway.Session, text any, _ promptSubmitOptions) bool {
		unlock := lockHistory(session)
		hist := append([]any{}, historyList(session.Get("history"))...)
		ver := intOr(session.Get("history_version"))
		unlock()
		messages := append(historyAsMessages(hist), map[string]any{"role": "user", "content": text},
			map[string]any{"role": "assistant", "content": "REPLY"})
		commitTurnHistory(session, messages, hist, ver)
		session.Set("running", false)
		return true
	}
	t.Cleanup(func() { runPromptSubmit = prevRun })
	a := testAgent(t, agent.AIAgentOptions{SessionID: "session-key"})
	var history []any
	for i, role := range []string{"user", "assistant", "user", "assistant", "user", "assistant"} {
		history = append(history, map[string]any{"role": role, "content": "m" + string(rune('0'+i))})
	}
	rec := &recordingTransport{}
	session := busySession(a, map[string]any{"running": false, "history": history, "transport": rec})
	tuigateway.SetSessionRecord("sid", session)
	t.Cleanup(func() { tuigateway.SetSessionRecord("sid", nil) })

	done := make(chan map[string]any, 1)
	go func() { done <- lookupMethod("session.compress")(ctx, "r1", map[string]any{"session_id": "sid"}) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("compaction never entered")
	}
	interruptSessionTurn(ctx, "sid", session, "", false)
	if session.Get("running") != true {
		t.Fatal("stop mid compaction released the busy claim")
	}
	second, _ := lookupMethod("session.compress")(ctx, "r3", map[string]any{"session_id": "sid"})["error"].(map[string]any)
	if second["code"] != 4009 {
		t.Fatalf("second compress %v", second)
	}
	if msg, _ := second["message"].(string); !strings.Contains(msg, "/compress") || strings.Contains(msg, "Stop") {
		t.Fatalf("second compress message %q", msg)
	}
	refused, _ := lookupMethod("tools.configure")(ctx, "r4", map[string]any{"session_id": "sid"})["error"].(map[string]any)
	if refused["code"] != 4009 {
		t.Fatalf("tools.configure %v", refused)
	}
	if msg, _ := refused["message"].(string); !strings.Contains(msg, "/compress") || strings.Contains(msg, "Stop") {
		t.Fatalf("tools.configure message %q", msg)
	}
	var busy *CompressionBusy
	if _, err := compressLiveWithFeedback(ctx, "sid", session, a, "", true); !errors.As(err, &busy) {
		t.Fatalf("slash compress while busy %v", err)
	}
	if got, err := compressLiveWithFeedback(ctx, "sid", session, a, "--aggressive", true); err != nil || got != agent.AggressiveUnsupported {
		t.Fatalf("aggressive %q %v", got, err)
	}
	resp := handleBusySubmit(ctx, "r2", "sid", session, "question", "ws-1", false, nil, "")
	close(gate)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("compaction rpc never returned")
	}

	if got := busyResult(t, resp)["status"]; got != "queued" {
		t.Fatalf("status %v", got)
	}
	if a.IsInterrupted() {
		t.Fatal("compaction was interrupted")
	}
	hist := historyList(session.Get("history"))
	if last, _ := hist[len(hist)-1].(map[string]any); last["content"] != "REPLY" {
		t.Fatalf("last row %v", hist[len(hist)-1])
	}
	if first, _ := hist[0].(map[string]any); first["content"] != "[summary]" {
		t.Fatalf("first row %v", hist[0])
	}
	if session.Get("running") != false {
		t.Fatal("session still running")
	}
	infos := sessionInfoPayloads(rec)
	if len(infos) == 0 || infos[0]["running"] != true || infos[len(infos)-1]["running"] != false {
		t.Fatalf("session.info edges %v", infos)
	}
}

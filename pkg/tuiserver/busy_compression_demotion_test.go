package tuiserver

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuihost"
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

// Running session record of the test fixture, holding the given agent and extra fields
func busySession(agentObj any, extra map[string]any) *tuihost.Session {
	if agentObj == nil {
		agentObj = &bareBusyAgent{}
	}
	fields := map[string]any{"agent": agentObj, "session_key": "session-key", "history": []any{}, "history_lock": &sync.Mutex{},
		"history_version": 0, "running": true, "transport": nil, "attached_images": []string{}}
	for k, v := range extra {
		fields[k] = v
	}
	return tuihost.NewSession(fields)
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
func queuedPromptText(session *tuihost.Session) any {
	q, _ := session.Get("queued_prompt").(map[string]any)
	return q["text"]
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

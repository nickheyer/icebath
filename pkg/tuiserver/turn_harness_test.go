package tuiserver

import (
	"context"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// A live session whose real agent talks to a scripted loopback provider, every frame recorded
type turnHarness struct {
	srv      *fakellm.Server
	ctx      context.Context
	sid, key string
	session  *tuihost.Session
	recorder *frameRecorder
	home     string
}

// Submits one prompt and waits until its turn completed and the session settled
func (hz *turnHarness) turn(t *testing.T, text string) {
	t.Helper()
	before := len(hz.events("message.complete"))
	resp := handleRequest(hz.ctx, map[string]any{"id": "turn", "method": "prompt.submit", "params": map[string]any{"session_id": hz.sid, "text": text}})
	result, _ := resp["result"].(map[string]any)
	if result["status"] != "streaming" {
		t.Fatalf("prompt.submit answered %v", resp)
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if len(hz.events("message.complete")) > before && !truthy(hz.session.Get("running")) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("turn never settled, frames %v", hz.types())
}

// Type and payload of every event frame of the harness session in wire order
func (hz *turnHarness) frames() [][2]any {
	hz.recorder.mu.Lock()
	defer hz.recorder.mu.Unlock()
	var out [][2]any
	for _, frame := range hz.recorder.frames {
		params, isObj := frame.GetOr("params", nil).(*pyjson.Object)
		if !isObj || params.GetOr("session_id", nil) != hz.sid {
			continue
		}
		payload, _ := pyjson.Plain(params.GetOr("payload", nil)).(map[string]any)
		if payload == nil {
			payload = map[string]any{}
		}
		out = append(out, [2]any{params.GetOr("type", nil), payload})
	}
	return out
}

// Payloads of the harness session events of one type
func (hz *turnHarness) events(kind string) []map[string]any {
	var out []map[string]any
	for _, frame := range hz.frames() {
		if frame[0] == kind {
			out = append(out, frame[1].(map[string]any))
		}
	}
	return out
}

// Event types of the harness session in wire order
func (hz *turnHarness) types() []any {
	var out []any
	for _, frame := range hz.frames() {
		out = append(out, frame[0])
	}
	return out
}

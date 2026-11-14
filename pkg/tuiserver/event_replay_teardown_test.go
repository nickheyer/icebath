package tuiserver

import (
	"context"
	"errors"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Event frame of one session as write_json stamps it
func replayFrame(sid string) *pyjson.Object {
	return pyjson.ObjectOf("jsonrpc", "2.0", "method", "event",
		"params", pyjson.ObjectOf("type", "message.delta", "session_id", sid, "payload", pyjson.NewObject()))
}

// Clean replay state for one case and a finalizer standing in for the full teardown
func swapTeardown(t *testing.T, fn func(context.Context, *tuigateway.Session, string)) {
	t.Helper()
	tuigateway.ResetReplayState()
	prev := teardownSessionFn
	teardownSessionFn = fn
	t.Cleanup(func() {
		teardownSessionFn = prev
		tuigateway.ResetReplayState()
	})
}

func TestSessionTeardownReleasesTheReplayRing(t *testing.T) {
	swapTeardown(t, func(context.Context, *tuigateway.Session, string) {})
	for range 3 {
		tuigateway.StampEvent(replayFrame("closed-sid"))
	}
	tuigateway.StampEvent(replayFrame("live-sid"))
	if !teardownPoppedSession(context.Background(), tuigateway.NewSession(map[string]any{"_sid": "closed-sid"}), "tui_shutdown") {
		t.Fatal("teardown reported no session")
	}
	if got := tuigateway.EventsSince("closed-sid", 0); len(got) != 0 {
		t.Fatalf("closed ring still holds %v", got)
	}
	if !tuigateway.IsTruncated("closed-sid", 0) {
		t.Fatal("a client at seq 0 must refetch the closed session")
	}
	if got := tuigateway.EventsSince("live-sid", 0); len(got) != 1 {
		t.Fatalf("live ring holds %v", got)
	}
}

func TestARaisingTeardownStillReleasesTheReplayRing(t *testing.T) {
	boom := errors.New("finalize failed")
	swapTeardown(t, func(context.Context, *tuigateway.Session, string) { panic(boom) })
	tuigateway.StampEvent(replayFrame("failed-sid"))
	func() {
		defer func() {
			if failure := recover(); failure != boom {
				t.Fatalf("teardown raised %v", failure)
			}
		}()
		teardownPoppedSession(context.Background(), tuigateway.NewSession(map[string]any{"_sid": "failed-sid"}), "tui_shutdown")
	}()
	if got := tuigateway.EventsSince("failed-sid", 0); len(got) != 0 {
		t.Fatalf("failed ring still holds %v", got)
	}
}

package tuiserver

import (
	"context"
	"errors"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Event frame of one session as write_json stamps it
func replayFrame(sid string) *pyjson.Object {
	return pyjson.ObjectOf("jsonrpc", "2.0", "method", "event",
		"params", pyjson.ObjectOf("type", "message.delta", "session_id", sid, "payload", pyjson.NewObject()))
}

// Clean replay state for one case and a finalizer standing in for the full teardown
func swapTeardown(t *testing.T, fn func(context.Context, *tuihost.Session, string)) {
	t.Helper()
	tuihost.ResetReplayState()
	prev := teardownSessionFn
	teardownSessionFn = fn
	t.Cleanup(func() {
		teardownSessionFn = prev
		tuihost.ResetReplayState()
	})
}

func TestSessionTeardownReleasesTheReplayRing(t *testing.T) {
	swapTeardown(t, func(context.Context, *tuihost.Session, string) {})
	for range 3 {
		tuihost.StampEvent(replayFrame("closed-sid"))
	}
	tuihost.StampEvent(replayFrame("live-sid"))
	if !teardownPoppedSession(context.Background(), tuihost.NewSession(map[string]any{"_sid": "closed-sid"}), "tui_shutdown") {
		t.Fatal("teardown reported no session")
	}
	if got := tuihost.EventsSince("closed-sid", 0); len(got) != 0 {
		t.Fatalf("closed ring still holds %v", got)
	}
	if !tuihost.IsTruncated("closed-sid", 0) {
		t.Fatal("a client at seq 0 must refetch the closed session")
	}
	if got := tuihost.EventsSince("live-sid", 0); len(got) != 1 {
		t.Fatalf("live ring holds %v", got)
	}
}

func TestARaisingTeardownStillReleasesTheReplayRing(t *testing.T) {
	boom := errors.New("finalize failed")
	swapTeardown(t, func(context.Context, *tuihost.Session, string) { panic(boom) })
	tuihost.StampEvent(replayFrame("failed-sid"))
	func() {
		defer func() {
			if failure := recover(); failure != boom {
				t.Fatalf("teardown raised %v", failure)
			}
		}()
		teardownPoppedSession(context.Background(), tuihost.NewSession(map[string]any{"_sid": "failed-sid"}), "tui_shutdown")
	}()
	if got := tuihost.EventsSince("failed-sid", 0); len(got) != 0 {
		t.Fatalf("failed ring still holds %v", got)
	}
}

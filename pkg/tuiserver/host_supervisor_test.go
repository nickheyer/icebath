package tuiserver

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Supervisor that never spawns a child and records every frame it would send
func testSupervisor(t *testing.T) (*HostSupervisor, *[]*pyjson.Object) {
	t.Helper()
	sha, h := "unknown", t.TempDir()
	sup, err := NewHostSupervisor(context.Background(), HostSupervisorOptions{RegistryPath: h + "/registry.json",
		Argv: []string{"true"}, Cwd: h, ExpectedBuildSHA: &sha, ExpectedHermesHome: &h})
	if err != nil {
		t.Fatal(err)
	}
	sent := &[]*pyjson.Object{}
	sup.sendFrameFn = func(frame *pyjson.Object) error { *sent = append(*sent, frame); return nil }
	sup.startFn = func() error { return nil }
	return sup, sent
}

func lateHandlerIDs(sup *HostSupervisor) []string {
	sup.lock.Lock()
	defer sup.lock.Unlock()
	ids := []string{}
	for rid := range sup.lateControlHandlers {
		ids = append(ids, rid)
	}
	sort.Strings(ids)
	return ids
}

func TestControlTimeoutRegistersOneShotLateAckHandler(t *testing.T) {
	sup, sent := testSupervisor(t)
	fired := []*pyjson.Object{}
	_, err := sup.Control("sid", "session.compress", pyjson.ObjectOf("command", "/compress"), true, 50*time.Millisecond,
		func(frame *pyjson.Object) { fired = append(fired, frame) })
	if !errors.Is(err, ErrHostReplyTimeout) {
		t.Fatalf("err %v", err)
	}
	requestID := strOr((*sent)[0].GetOr("request_id", nil))
	if _, pending := sup.pendingControls[requestID]; pending {
		t.Fatal("waiter left registered")
	}
	if ids := lateHandlerIDs(sup); !reflect.DeepEqual(ids, []string{requestID}) {
		t.Fatalf("late handlers %v", ids)
	}
	late := pyjson.ObjectOf("type", "control.ack", "request_id", requestID, "result", pyjson.ObjectOf("status", "compressed"))
	sup.handleHostFrame(late)
	sup.handleHostFrame(late)
	if len(fired) != 1 || fired[0] != late {
		t.Fatalf("fired %d", len(fired))
	}
	if len(lateHandlerIDs(sup)) != 0 {
		t.Fatal("handler kept after firing")
	}
}

func TestControlTimeoutWithoutHandlerStillDropsLateAck(t *testing.T) {
	sup, sent := testSupervisor(t)
	if _, err := sup.Control("sid", "session.compress", nil, true, 50*time.Millisecond, nil); !errors.Is(err, ErrHostReplyTimeout) {
		t.Fatalf("err %v", err)
	}
	if len(lateHandlerIDs(sup)) != 0 {
		t.Fatal("handler registered")
	}
	sup.handleHostFrame(pyjson.ObjectOf("type", "control.ack", "request_id", (*sent)[0].GetOr("request_id", nil)))
}

func TestLateControlErrorAndBareErrorFramesFireHandler(t *testing.T) {
	sup, sent := testSupervisor(t)
	fired := []string{}
	for range 2 {
		if _, err := sup.Control("sid", "session.compress", nil, true, 10*time.Millisecond,
			func(frame *pyjson.Object) { fired = append(fired, strOr(frame.GetOr("request_id", nil))) }); !errors.Is(err, ErrHostReplyTimeout) {
			t.Fatalf("err %v", err)
		}
	}
	ridA, ridB := strOr((*sent)[0].GetOr("request_id", nil)), strOr((*sent)[1].GetOr("request_id", nil))
	sup.handleHostFrame(pyjson.ObjectOf("type", "control.error", "request_id", ridA, "message", "boom"))
	sup.handleHostFrame(pyjson.ObjectOf("type", "error", "request_id", ridB, "message", "bad frame"))
	if !reflect.DeepEqual(fired, []string{ridA, ridB}) {
		t.Fatalf("fired %v", fired)
	}
}

func TestLateAckHandlersAreBoundedByTTLAndCap(t *testing.T) {
	swapVar(t, &lateControlMax, 3)
	sup, _ := testSupervisor(t)
	for _, rid := range []string{"r0", "r1", "r2", "r3", "r4"} {
		sup.registerLateControlHandler(rid, func(*pyjson.Object) {})
	}
	if ids := lateHandlerIDs(sup); !reflect.DeepEqual(ids, []string{"r2", "r3", "r4"}) {
		t.Fatalf("handlers %v", ids)
	}
	swapVar(t, &lateControlTTLSecs, 0)
	time.Sleep(10 * time.Millisecond)
	sup.registerLateControlHandler("fresh", func(*pyjson.Object) {})
	if ids := lateHandlerIDs(sup); !reflect.DeepEqual(ids, []string{"fresh"}) {
		t.Fatalf("handlers %v", ids)
	}
}

func TestHostCrashFailsOutstandingLateAckHandlers(t *testing.T) {
	sup, sent := testSupervisor(t)
	fired := []*pyjson.Object{}
	if _, err := sup.Control("sid", "session.compress", nil, true, 10*time.Millisecond,
		func(frame *pyjson.Object) { fired = append(fired, frame) }); !errors.Is(err, ErrHostReplyTimeout) {
		t.Fatalf("err %v", err)
	}
	sup.failPendingTurns("crash", "compute host exited with code 1")
	if len(fired) != 1 || fired[0].GetOr("type", nil) != "control.error" || fired[0].GetOr("request_id", nil) != (*sent)[0].GetOr("request_id", nil) {
		t.Fatalf("fired %v", fired)
	}
	if len(lateHandlerIDs(sup)) != 0 {
		t.Fatal("handlers kept after crash")
	}
}

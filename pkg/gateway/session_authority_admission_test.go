package gateway

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Reason of a runtime store refusal, empty for any other error
func refusalReason(err error) string {
	var refusal *state.RuntimeStoreError
	if errors.As(err, &refusal) {
		return refusal.Reason
	}
	return ""
}

func TestQueuedInputsPastTheCapAreRefusedQueueFullAndRetriesReplay(t *testing.T) {
	ctx, db, a := newTestAuthority(t, "")
	holdDrains(t)
	for n := range state.AdmissionQueueMax {
		if _, err := submitText(ctx, a, fmt.Sprintf("r%d", n)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := submitText(ctx, a, "overflow"); refusalReason(err) != "queue_full" {
		t.Fatalf("overflow: %v", err)
	}
	rows, err := db.ListSessionAdmissions(ctx, "s", true)
	if err != nil || len(rows) != state.AdmissionQueueMax {
		t.Fatalf("pending rows %d %v", len(rows), err)
	}
	replay, err := submitText(ctx, a, "r0")
	if err != nil || replay.Status != "queued" {
		t.Fatalf("replay %v %v", replay, err)
	}
	if err := db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "UPDATE session_admissions SET status='unknown' WHERE request_id='r0'")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	refill, err := submitText(ctx, a, "refill")
	if err != nil || refill.Status != "queued" {
		t.Fatalf("refill %v %v", refill, err)
	}
	for _, requestID := range []string{"paused-overflow", "paused-overflow-2"} {
		if _, err := submitText(ctx, a, requestID); refusalReason(err) != "queue_full" {
			t.Fatalf("%s: %v", requestID, err)
		}
	}
	rows, err = db.ListSessionAdmissions(ctx, "s", true)
	if err != nil || len(rows) != state.AdmissionQueueMax+1 {
		t.Fatalf("pending rows %d %v", len(rows), err)
	}
}

func TestAttachmentCapIsPerAdmissionNotPerFile(t *testing.T) {
	ctx, db, a := newTestAuthority(t, "")
	holdDrains(t)
	if err := os.WriteFile(filepath.Join(home.Dir(ctx), "config.yaml"), []byte("gateway:\n  max_inbound_media_bytes: 3072\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := []any{stagedAttachment(t, ctx, "part0.png", 2*1024), stagedAttachment(t, ctx, "part1.png", 2*1024)}
	if _, err := submitAttachments(ctx, a, "too-big", files); refusalReason(err) != "invalid_params" {
		t.Fatalf("too big: %v", err)
	}
	rows, err := db.ListSessionAdmissions(ctx, "s", false)
	if err != nil || len(rows) != 0 {
		t.Fatalf("rows %v %v", rows, err)
	}
	receipt, err := submitAttachments(ctx, a, "fits", files[:1])
	if err != nil || receipt.Status != "queued" {
		t.Fatalf("fits %v %v", receipt, err)
	}
}

func TestPhysicalHoldersAndUncertainStatDenyCollection(t *testing.T) {
	names := map[string]string{"same": "shared.png", "case": "SHARED.png", "hardlink": "linked.png", "distinct": "other.png", "missing-holder": "missing.png"}
	for _, alias := range []string{"same", "case", "hardlink", "distinct", "missing-holder"} {
		t.Run(alias, func(t *testing.T) {
			ctx, db, a := newTestAuthority(t, "")
			holdDrains(t)
			name := names[alias]
			first := stagedAttachment(t, ctx, "shared.png", 2048)
			second := stagedAttachment(t, ctx, name, 2048)
			receipt, err := submitAttachments(ctx, a, "first", []any{first})
			if err != nil {
				t.Fatal(err)
			}
			firstRef := firstAttachmentRef(t, ctx, db, receipt.AdmissionID)
			firstPath := rowString(firstRef, "path")
			if alias == "hardlink" {
				if err := os.Link(firstPath, filepath.Join(filepath.Dir(firstPath), name)); err != nil {
					t.Fatal(err)
				}
			}
			other, err := submitAttachments(ctx, a, "other", []any{second})
			if err != nil {
				t.Fatal(err)
			}
			otherRef := firstAttachmentRef(t, ctx, db, other.AdmissionID)
			otherPath := rowString(otherRef, "path")
			firstInfo, err := os.Stat(firstPath)
			if err != nil {
				t.Fatal(err)
			}
			otherInfo, err := os.Stat(otherPath)
			if err != nil {
				t.Fatal(err)
			}
			sameFile := os.SameFile(firstInfo, otherInfo)
			if alias == "missing-holder" {
				if err := os.Remove(otherPath); err != nil {
					t.Fatal(err)
				}
			}
			started, _, err := db.ClaimSessionInput(ctx, state.ClaimInput{Epoch: a.Epoch, SessionID: "s"})
			if err != nil || started == nil {
				t.Fatalf("claim %v %v", started, err)
			}
			if _, err := db.SettleSessionInput(ctx, state.SettleInput{Epoch: a.Epoch, AdmissionID: receipt.AdmissionID,
				Generation: *rowInt(started, "generation"), Outcome: "completed"}); err != nil {
				t.Fatal(err)
			}
			removed, err := releaseAdmissionMediaCount(ctx, db, receipt.AdmissionID)
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if sameFile || alias == "missing-holder" {
				want = 0
			}
			if removed != want {
				t.Fatalf("released %d files, want %d", removed, want)
			}
			if alias == "missing-holder" {
				if !onDisk(firstPath) {
					t.Fatal("bytes whose holder is uncertain were collected")
				}
				return
			}
			restored, err := restoreNativeMedia(ctx, []any{otherRef})
			if err != nil {
				t.Fatal(err)
			}
			if info, err := os.Stat(restored[0]); err != nil || info.Size() != 2048 {
				t.Fatalf("restored holder %v %v", info, err)
			}
		})
	}
}

func TestCancelQueuedSettlesNativeWaiterAndPublishesTerminalCompletion(t *testing.T) {
	ctx, _, a := newTestAuthority(t, "")
	holdDrains(t)
	var frames []*pyjson.Object
	receipt, err := submitText(ctx, a, "queued-then-cancelled")
	if err != nil {
		t.Fatal(err)
	}
	waiter := newAdmissionWaiter()
	a.nativeWaiters[receipt.AdmissionID] = true
	a.waiters[receipt.AdmissionID] = waiter
	a.Sessions["s"].EventStream.Observers[&SessionEventObserver{Observe: func(frame *pyjson.Object) { frames = append(frames, frame) }}] = true
	completions := func() []*pyjson.Object {
		var out []*pyjson.Object
		for _, frame := range frames {
			params, _ := frame.Get("params")
			if kind, _ := params.(*pyjson.Object).Get("type"); kind == "message.complete" {
				out = append(out, params.(*pyjson.Object))
			}
		}
		return out
	}
	cancelled, err := a.CancelQueued(ctx, testActor, testRef, receipt.AdmissionID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != "terminal" || cancelled.Outcome == nil || *cancelled.Outcome != "cancelled" {
		t.Fatalf("cancelled receipt %+v", cancelled)
	}
	if !waiter.isDone() || a.nativeWaiters[receipt.AdmissionID] {
		t.Fatal("native waiter was not settled")
	}
	if _, waiting := a.waiters[receipt.AdmissionID]; waiting {
		t.Fatal("waiter stayed registered")
	}
	terminal := completions()
	if len(terminal) != 1 {
		t.Fatalf("%d completions", len(terminal))
	}
	if id, _ := terminal[0].Get("admission_id"); id != receipt.AdmissionID {
		t.Fatalf("completion stamped %v", id)
	}
	payload, _ := terminal[0].Get("payload")
	if outcome, _ := payload.(*pyjson.Object).Get("outcome"); outcome != "cancelled" {
		t.Fatalf("completion outcome %v", outcome)
	}
	if id, _ := payload.(*pyjson.Object).Get("admission_id"); id != receipt.AdmissionID {
		t.Fatalf("completion payload names %v", id)
	}
	if _, err := a.CancelQueued(ctx, testActor, testRef, receipt.AdmissionID); err != nil {
		t.Fatal(err)
	}
	if len(completions()) != 1 {
		t.Fatalf("%d completions after a repeated cancel", len(completions()))
	}
}

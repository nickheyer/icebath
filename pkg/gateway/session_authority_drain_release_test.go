package gateway

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestRetainedBytesAreReleasedAfterCancelAndSettlement(t *testing.T) {
	ctx, db, a := newTestAuthority(t, "")
	holdDrains(t)
	shared := stagedAttachment(t, ctx, "shared.png", 64)
	first, err := submitAttachments(ctx, a, "first", []any{shared})
	if err != nil {
		t.Fatal(err)
	}
	second, err := submitAttachments(ctx, a, "second", []any{shared})
	if err != nil {
		t.Fatal(err)
	}
	retained := rowString(firstAttachmentRef(t, ctx, db, first.AdmissionID), "path")
	root, err := nativeInputsRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(filepath.Dir(retained)) != root || !onDisk(retained) {
		t.Fatalf("retained %s under %s", retained, root)
	}
	if _, err := a.CancelQueued(ctx, testActor, testRef, first.AdmissionID); err != nil {
		t.Fatal(err)
	}
	if !onDisk(retained) {
		t.Fatal("cancelling one referent dropped the bytes the other still needs")
	}
	survived := false
	replaceExecution(t, func(context.Context, *SessionAuthority, SessionRef, *pyjson.Object) (any, error) {
		survived = onDisk(retained)
		return "done", nil
	})
	if err := drainSessionFIFO(ctx, a, testRef); err != nil {
		t.Fatal(err)
	}
	if !survived {
		t.Fatal("bytes must survive until the turn has settled")
	}
	settled := admissionByID(t, ctx, db, second.AdmissionID)
	if rowString(settled, "status") != "terminal" || rowString(settled, "outcome") != "completed" {
		t.Fatalf("settled %v", settled)
	}
	if onDisk(retained) || onDisk(filepath.Dir(retained)) {
		t.Fatal("the last referent settled and its bytes stayed")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("media root %v %v", entries, err)
	}
}

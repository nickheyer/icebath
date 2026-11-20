package state

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Store with session s, a boot epoch and one claimed admission, returning the epoch, admission id and generation
func claimedWorkerStore(t *testing.T) (*SessionDB, int64, string, int64) {
	t.Helper()
	ctx := context.Background()
	db := runtimeStore(t, filepath.Join(t.TempDir(), "state.db"))
	epoch, err := db.BeginRuntimeEpoch(ctx, "boot")
	if err != nil {
		t.Fatal(err)
	}
	admitted := admitQueued(t, db, epoch, "human", "s", "input", admissionText("work"))
	claim, _, err := db.ClaimSessionInput(ctx, ClaimInput{Epoch: epoch, SessionID: "s"})
	if err != nil || claim == nil {
		t.Fatalf("claim %v %v", claim, err)
	}
	generation, _ := claim.Get("generation")
	return db, epoch, admitted, generation.(int64)
}

func TestRegisterWorkerExecutionReplaysOnlyAnIdenticalRegistration(t *testing.T) {
	ctx := context.Background()
	db, epoch, _, generation := claimedWorkerStore(t)
	defer db.Close()
	reg := WorkerRegistration{Epoch: epoch, ExecutionID: "compute", SessionID: "s", Generation: generation, Kind: "compute", AdoptionSecret: "private"}
	first, err := db.RegisterWorkerExecution(ctx, reg)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := first.Get("adoption_digest"); has || objStr(first, "status") != "registered" {
		t.Fatalf("public row %v", first)
	}
	again, err := db.RegisterWorkerExecution(ctx, reg)
	if err != nil || !pyjson.Equal(again, first) {
		t.Fatalf("replay %v %v", again, err)
	}
	changed := reg
	changed.AdoptionSecret = "other"
	if _, err := db.RegisterWorkerExecution(ctx, changed); refusalReason(t, err) != "admission_conflict" {
		t.Fatal(err)
	}
	second := reg
	second.ExecutionID = "second"
	if _, err := db.RegisterWorkerExecution(ctx, second); refusalReason(t, err) != "stale_generation" {
		t.Fatalf("second live worker %v", err)
	}
	bad := reg
	bad.Kind = "daemon"
	if _, err := db.RegisterWorkerExecution(ctx, bad); refusalReason(t, err) != "invalid_params" {
		t.Fatal(err)
	}
	stale := reg
	stale.ExecutionID, stale.Generation = "stale", generation+1
	if _, err := db.RegisterWorkerExecution(ctx, stale); refusalReason(t, err) != "stale_generation" {
		t.Fatal(err)
	}
}

func TestAdoptWorkerExecutionPreservesTheClaimUnderTheNewEpoch(t *testing.T) {
	for _, recoverFirst := range []bool{false, true} {
		ctx := context.Background()
		db, epoch, admitted, generation := claimedWorkerStore(t)
		reg := WorkerRegistration{Epoch: epoch, ExecutionID: "compute", SessionID: "s", Generation: generation, Kind: "compute", AdoptionSecret: "private"}
		if _, err := db.RegisterWorkerExecution(ctx, reg); err != nil {
			t.Fatal(err)
		}
		newEpoch, _ := db.BeginRuntimeEpoch(ctx, "replacement")
		if recoverFirst {
			if _, err := db.RecoverSessionInputs(ctx, newEpoch); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.AdoptWorkerExecution(ctx, newEpoch, "compute", "s", generation, "wrong"); refusalReason(t, err) != "permission_denied" {
			t.Fatal(err)
		}
		adopted, err := db.AdoptWorkerExecution(ctx, newEpoch, "compute", "s", generation, "private")
		if err != nil || objStr(adopted, "status") != "running" {
			t.Fatalf("adopt %v %v", adopted, err)
		}
		if _, err := db.RecoverSessionInputs(ctx, newEpoch); err != nil {
			t.Fatal(err)
		}
		restored, _ := db.GetSessionAdmission(ctx, admitted)
		owner, _ := restored.Get("owner_epoch")
		gen, _ := restored.Get("generation")
		if objStr(restored, "status") != "started" || owner != newEpoch || gen != generation {
			t.Fatalf("restored %v", restored)
		}
		if claim, _, err := db.ClaimSessionInput(ctx, ClaimInput{Epoch: newEpoch, SessionID: "s"}); err != nil || claim != nil {
			t.Fatalf("re-execution claim %v %v", claim, err)
		}
		db.Close()
	}
}

func TestAdoptWorkerExecutionRefusesRevokedAndLateAdmissionWorkers(t *testing.T) {
	ctx := context.Background()
	db, epoch, admitted, generation := claimedWorkerStore(t)
	defer db.Close()
	reserved := WorkerRegistration{Epoch: epoch, ExecutionID: "admission-worker:one", SessionID: "s", Generation: generation, Kind: "compute", AdoptionSecret: "private"}
	if _, err := db.RegisterWorkerExecution(ctx, reserved); err != nil {
		t.Fatal(err)
	}
	newEpoch, _ := db.BeginRuntimeEpoch(ctx, "replacement")
	if _, err := db.AdoptWorkerExecution(ctx, newEpoch, reserved.ExecutionID, "s", generation, "private"); refusalReason(t, err) != "stale_epoch" {
		t.Fatalf("late handshake %v", err)
	}
	if _, err := db.RecoverSessionInputs(ctx, newEpoch); err != nil {
		t.Fatal(err)
	}
	result, err := db.ResolveUnknownSessionInput(ctx, newEpoch, admitted, generation, nil, nil)
	if err != nil || objStr(result, "status") != "terminal" || objStr(result, "outcome") != "interrupted" {
		t.Fatalf("resolve %v %v", result, err)
	}
	if _, err := db.AdoptWorkerExecution(ctx, newEpoch, reserved.ExecutionID, "s", generation, "private"); refusalReason(t, err) != "stale_generation" {
		t.Fatalf("revoked worker %v", err)
	}
	if _, err := db.AdoptWorkerExecution(ctx, newEpoch, "missing", "s", generation, "private"); refusalReason(t, err) != "not_found" {
		t.Fatal(err)
	}
}

package agent

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestWorkerConstructorRestoresPersistedCompressorGuards(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	tmp := t.TempDir()
	db := openOwnerDB(t, filepath.Join(tmp, "state.db"))
	defer db.Close()
	if err := db.CreateSession(state.NewSession{ID: "owned", Source: "cli", ModelConfig: map[string]any{"_proactive_prune_rearm_tokens": 1234}}); err != nil {
		t.Fatal(err)
	}
	deadline := float64(time.Now().UnixNano())/1e9 + 3600
	reason := "fixture-cooldown"
	if db.SetCompressionFallbackStreak("owned", 4) != nil || db.SetCompressionIneffectiveCount("owned", 3) != nil || db.SetCompressionRecoveryDeadline("owned", deadline) != nil {
		t.Fatal("seeding the guards failed")
	}
	db.RecordCompressionFailureCooldown("owned", deadline, &reason)
	epoch, err := db.BeginRuntimeEpoch(ctx, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	scope := pyjson.ObjectOf("epoch", epoch, "execution_id", "worker", "session_id", "owned", "generation", int64(0))
	if _, err := db.RegisterWorkerExecution(ctx, state.WorkerRegistration{Epoch: epoch, ExecutionID: "worker", SessionID: "owned", Kind: "compute", AdoptionSecret: "test-secret"}); err != nil {
		t.Fatal(err)
	}
	store := mustStore(t, dbWorkerCall(db), scope, filepath.Join(tmp, "outbox"), 0)
	defer store.Close()
	window := 100000
	o := DefaultContextCompressorOptions("fixture")
	o.ConfigContextLength, o.QuietMode = &window, true
	compressor := NewContextCompressor(ctx, o)
	compressor.bindSessionState(store, "owned")
	if compressor.fallbackCompressionStreak != 4 || compressor.ineffectiveCompressionCount != 3 ||
		compressor.antiThrashRecoveryDeadline != deadline || compressor.proactivePruneRearmTokens != 1234 {
		t.Fatalf("streak %d ineffective %d deadline %v runway %d", compressor.fallbackCompressionStreak, compressor.ineffectiveCompressionCount,
			compressor.antiThrashRecoveryDeadline, compressor.proactivePruneRearmTokens)
	}
	storeRow, err := store.GetCompressionFailureCooldownRow("owned")
	if err != nil {
		t.Fatal(err)
	}
	dbRow, err := db.GetCompressionFailureCooldownRow("owned")
	if err != nil || !reflect.DeepEqual(storeRow, dbRow) {
		t.Fatalf("store row %+v db row %+v %v", storeRow, dbRow, err)
	}
	cooldown, err := store.GetCompressionFailureCooldown("owned")
	if err != nil {
		t.Fatal(err)
	}
	if until, _ := cooldown.Get("cooldown_until"); until != deadline {
		t.Fatalf("cooldown_until %v want %v", until, deadline)
	}
	if _, err := db.SetSessionTitle(ctx, "owned", "retained-title"); err != nil {
		t.Fatal(err)
	}
	storeTitle, storeOK, err := store.GetSessionTitle(ctx, "owned")
	if err != nil {
		t.Fatal(err)
	}
	dbTitle, dbOK, _ := db.GetSessionTitle(ctx, "owned")
	if storeTitle != dbTitle || storeOK != dbOK || dbTitle != "retained-title" {
		t.Fatalf("store title %q %v db title %q %v", storeTitle, storeOK, dbTitle, dbOK)
	}
}

package state

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestWorkerContextPreservesAuthorityIdentityAndPromptReceipts(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	if err := db.CreateSession(NewSession{ID: "owned", Source: "subagent", Model: "requested", ModelConfig: map[string]any{"_delegate_from": "parent"},
		SystemPrompt: "prefix", ProfileName: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(NewSession{ID: "foreign", Source: "cli", SystemPrompt: "private"}); err != nil {
		t.Fatal(err)
	}
	epoch, _ := db.BeginRuntimeEpoch(ctx, "fixture")
	scope := WorkerReceipt{Epoch: epoch, ExecutionID: "worker", SessionID: "owned", Generation: 0}
	if _, err := db.RegisterWorkerExecution(ctx, WorkerRegistration{Epoch: epoch, ExecutionID: "worker", SessionID: "owned", Generation: 0, Kind: "child", AdoptionSecret: "test-secret"}); err != nil {
		t.Fatal(err)
	}
	store := newWorkerStore(t, db, scope)
	owned, err := store.getSession("owned")
	if err != nil || !pyjson.Equal(rowValue(owned, "system_prompt"), "prefix") {
		t.Fatalf("owned %v %v", owned, err)
	}
	var denied *workerPersistenceError
	if _, err := store.getSession("foreign"); err == nil || err.Error() != "permission_denied" {
		t.Fatalf("foreign read %v", err)
	} else {
		denied = err.(*workerPersistenceError)
	}
	if denied == nil {
		t.Fatal("facade refusal expected")
	}
	if err := store.updateSystemPrompt("owned", "stable-prefix"); err != nil {
		t.Fatal(err)
	}
	if err := store.patchSessionModelConfig("owned", map[string]any{"_usage_anchor": map[string]any{"tokens": 23}}); err != nil {
		t.Fatal(err)
	}
	anchor, err := store.getSessionModelConfigValue("owned", "_usage_anchor")
	if err != nil || !pyjson.Equal(anchor, pyjson.ObjectOf("tokens", 23)) {
		t.Fatalf("anchor %v %v", anchor, err)
	}
	if delegate, err := db.GetSessionModelConfigValue(ctx, "owned", "_delegate_from", nil); err != nil || delegate != "parent" {
		t.Fatalf("delegate marker %v %v", delegate, err)
	}
	if !pyjson.Equal(rowValue(mustSession(t, db, "foreign"), "system_prompt"), "private") {
		t.Fatal("foreign prompt changed")
	}
	original := store.rpc
	store.rpc = lostAckRPC(original, "lost-ack")
	if err := store.updateSystemPrompt("owned", "durable-prefix"); err == nil || err.Error() != "lost-ack" {
		t.Fatalf("lost ack %v", err)
	}
	store.rpc = original
	if results, err := store.retryPending(); err != nil || len(results) != 1 {
		t.Fatalf("retry %v %v", results, err)
	}
	if !pyjson.Equal(rowValue(mustSession(t, db, "owned"), "system_prompt"), "durable-prefix") {
		t.Fatal("durable prompt missing")
	}
	if r := receiptRefusal(t, db, store.rawReceipt("session.sidecars", map[string]any{"patch": map[string]any{"_delegate_from": "foreign"}})); r != "invalid_params" {
		t.Fatal(r)
	}
}

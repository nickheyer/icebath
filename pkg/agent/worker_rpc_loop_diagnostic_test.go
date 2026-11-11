package agent

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

const loopSecret = "arg-value-never-logged"

func guardWarnings(records []loggerRecord) []string {
	var out []string
	for _, r := range records {
		if r.level == slog.LevelWarn {
			out = append(out, r.msg)
		}
	}
	return out
}

func persistFromCallback(ctx context.Context, rpc *WorkerRPC) error {
	_, err := rpc.Call(ctx, "worker.persist", pyjson.ObjectOf("payload", loopSecret))
	return err
}

func onLoopPersist(ctx context.Context, rpc *WorkerRPC) error {
	return persistFromCallback(pyctx.WithRunningLoop(ctx), rpc)
}

func TestOnLoopRpcLogsCallerFramesWithoutArgumentValues(t *testing.T) {
	records := captureLogger(t, "agent.runtime_session_store", slog.LevelWarn)
	rpc := NewWorkerRPC(t.TempDir())
	if err := onLoopPersist(context.Background(), rpc); persistenceReason(err) != "synchronous_rpc_on_event_loop" {
		t.Fatalf("on loop call %v", err)
	}
	warnings := guardWarnings(records())
	if len(warnings) != 1 {
		t.Fatalf("warnings %v", warnings)
	}
	message := warnings[0]
	if !strings.Contains(message, "synchronous_rpc_on_event_loop") || !strings.Contains(message, "worker.persist") {
		t.Fatal(message)
	}
	if !strings.Contains(message, "pkg/agent:persistFromCallback:") || !strings.Contains(message, "pkg/agent:onLoopPersist:") {
		t.Fatal(message)
	}
	if strings.Contains(message, loopSecret) {
		t.Fatal(message)
	}
}

func bestEffortUsage(ctx context.Context, store *RuntimeSessionStore) {
	model := loopSecret
	_ = store.RecordAuxiliaryUsage(ctx, "s", "approval", state.AuxiliaryUsage{InputTokens: 7, Model: &model})
}

func TestOnLoopStoreWriteNamesTheCallerNotTheStore(t *testing.T) {
	records := captureLogger(t, "agent.runtime_session_store", slog.LevelWarn)
	tmp := t.TempDir()
	rpc := NewWorkerRPC(tmp)
	store := mustStore(t, rpc.Call, pyjson.ObjectOf("session_id", "s"), filepath.Join(tmp, "outbox"), 0)
	defer store.closeOwner()
	bestEffortUsage(pyctx.WithRunningLoop(context.Background()), store)
	warnings := guardWarnings(records())
	if len(warnings) != 1 {
		t.Fatalf("warnings %v", warnings)
	}
	_, caller, _ := strings.Cut(warnings[0], "caller: ")
	if !strings.HasPrefix(caller, "github.com/nickheyer/icebath/pkg/agent:bestEffortUsage:") {
		t.Fatal(caller)
	}
	if strings.Contains(caller, "RuntimeSessionStore") || strings.Contains(caller, "WorkerRPC") || strings.Contains(warnings[0], loopSecret) {
		t.Fatal(warnings[0])
	}
	if f := store.Failure(); f == nil || *f != "synchronous_rpc_on_event_loop" {
		t.Fatalf("failure %v", f)
	}
}

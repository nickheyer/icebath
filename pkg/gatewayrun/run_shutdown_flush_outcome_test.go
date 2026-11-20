package gatewayrun

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestShutdownFlushSnapshotsOnlyWhenTheFlushRaises(t *testing.T) {
	a := quietAgent(t)
	messages := []*state.Message{{Role: "user", Content: "lost msg"}}
	if err := a.PersistSession(&messages, nil); err != nil {
		t.Fatal(err)
	}
	spool := filepath.Join(os.Getenv("ICEBATH_HOME"), "pending_messages")
	snapshots := func() []string {
		files, _ := filepath.Glob(filepath.Join(spool, "*.json"))
		return files
	}
	real := shutdownTranscriptFlush
	t.Cleanup(func() { shutdownTranscriptFlush = real })
	for _, outcome := range []state.FlushOutcome{state.FlushDisabled, state.FlushFailed, state.FlushPersisted} {
		shutdownTranscriptFlush = func(*agent.AIAgent) state.FlushOutcome { return outcome }
		flushAgentTranscriptAtShutdown(context.Background(), a)
		if files := snapshots(); len(files) != 0 {
			t.Fatalf("returned outcome %v wrote a recovery snapshot %v", outcome, files)
		}
	}
	shutdownTranscriptFlush = func(*agent.AIAgent) state.FlushOutcome {
		panic(errors.New("database disk image is malformed"))
	}
	flushAgentTranscriptAtShutdown(context.Background(), a)
	files := snapshots()
	if len(files) != 1 {
		t.Fatalf("raised flush snapshots %v", files)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	if data["issue"] != "#72680" || data["session_id"] != a.SessionID() || data["count"] != float64(1) {
		t.Fatalf("snapshot %v", data)
	}
}

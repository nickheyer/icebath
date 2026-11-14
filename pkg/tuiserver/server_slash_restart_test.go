package tuiserver

import (
	"context"
	"testing"
)

// Slash worker double recording its close
type closingWorker struct{ closed bool }

func (w *closingWorker) Close() { w.closed = true }

func TestRestartSlashWorkerWithoutSessionKeyClearsTheWorker(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	old := &closingWorker{}
	session := liveSession(t, "sid-restart-no-key", map[string]any{"slash_worker": old, "agent": nil})
	restartSlashWorker(context.Background(), "sid-restart-no-key", session)
	if !old.closed {
		t.Fatal("old worker was not closed")
	}
	if worker, has := session.Lookup("slash_worker"); !has || worker != nil {
		t.Fatalf("slash_worker = %v, want a cleared slot", worker)
	}
}

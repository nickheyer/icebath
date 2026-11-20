package tools

import "testing"

func TestOneshotCompletionWaitSecondsDefaultsToTenMinutes(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	env := &Env{Home: h, Cwd: h}
	if got := OneshotCompletionWaitSeconds(env); got != 600 {
		t.Fatalf("got %v", got)
	}
}

func TestInsertedSessionsReleaseTheGoalBarrierOnAWatchMatch(t *testing.T) {
	r := NewProcessRegistry()
	running := &ProcessSession{ID: "proc_live", Command: "watcher.sh", WatchPatterns: []string{"READY"}}
	r.InsertRunning(running)
	r.InsertFinished(&ProcessSession{ID: "proc_gone", Command: "done.sh", Exited: true})
	if !r.IsSessionWaiting("proc_live") || r.IsSessionWaiting("proc_gone") || r.IsSessionWaiting("proc_missing") {
		t.Fatal("inserted sessions read wrong before a match")
	}
	r.CheckWatchPatterns(running, "booting\nREADY now\n")
	if r.IsSessionWaiting("proc_live") || running.Exited {
		t.Fatal("a watch match did not release the barrier of a live session")
	}
}

func TestSwapAsyncRecordsInstallsAndRestoresTheDelegationTable(t *testing.T) {
	restore := SwapAsyncRecords([]string{"a", "b"}, map[string]map[string]any{
		"a": {"status": "running", "parent_session_id": "root"},
		"b": {"status": "completed", "parent_session_id": "root"},
	})
	if LiveDelegationBatches("root") != 1 {
		t.Fatalf("live batches %d", LiveDelegationBatches("root"))
	}
	restore()
	if LiveDelegationBatches("root") != 0 {
		t.Fatal("the swapped records survived the restore")
	}
}

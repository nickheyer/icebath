package cron

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
)

// Runs a manual job through the owner execution whose session holds the rows, the end reason it booked
func runBookedJob(t *testing.T, rows []map[string]any, breakProbe bool) string {
	t.Helper()
	s, _ := newTestStore(t)
	resetDeliver(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: strPtr("Do the thing"), Schedule: "every 1h", Name: strPtr("Verification"), Model: "m1"})
	fake := &fakeCronAgent{result: map[string]any{"completed": true, "failed": false, "final_response": "done", "turn_exit_reason": ""}}
	ctx, execID := ownerRun(t, s, job, fake)
	db, err := state.Acquire(context.Background(), home.StatePath(s.home))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.ReleaseOrClose(db) })
	if err := db.CreateSession(state.NewSession{ID: "cron_owner_session", Source: "cron"}); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if _, err := db.AppendMessage(context.Background(), "cron_owner_session", row, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	if breakProbe {
		if err := db.ExecuteWrite(context.Background(), func(conn state.SQLiteConn) error {
			_, err := conn.ExecContext(context.Background(), "ALTER TABLE messages RENAME TO messages_unreadable")
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	out, err := s.runJob(ctx, job, runJobOptions{ExecutionID: &execID})
	if err != nil || !out.Success || out.FinalResponse != "done" {
		t.Fatalf("outcome %+v err %v", out, err)
	}
	row, err := db.GetSession(context.Background(), "cron_owner_session")
	if err != nil || row == nil {
		t.Fatalf("session row %v %v", row, err)
	}
	if ended, _ := row.Get("ended_at"); ended == nil {
		t.Fatal("the cron session was never ended")
	}
	reason, _ := row.Get("end_reason")
	text, _ := reason.(string)
	return text
}

func TestRunWithoutFinalAssistantMessageBooksIncomplete(t *testing.T) {
	reason := runBookedJob(t, []map[string]any{{"role": "user", "content": "Do the thing"}}, false)
	if reason != "cron_incomplete_no_output" {
		t.Fatalf("end reason %q", reason)
	}
}

func TestRunWithFinalAssistantReplyBooksComplete(t *testing.T) {
	rows := []map[string]any{{"role": "user", "content": "Do the thing"}, {"role": "assistant", "content": "done"}}
	if reason := runBookedJob(t, rows, false); reason != "cron_complete" {
		t.Fatalf("end reason %q", reason)
	}
}

func TestClassificationProbeFailureKeepsHistoricalReason(t *testing.T) {
	rows := []map[string]any{{"role": "user", "content": "Do the thing"}}
	if reason := runBookedJob(t, rows, true); reason != "cron_complete" {
		t.Fatalf("end reason %q", reason)
	}
}

package tools

import (
	"context"
	"reflect"
	"testing"
)

func TestProcessResultsKeepTheKeyOrderHermesInserts(t *testing.T) {
	env, _ := cpProcEnv(t)
	r := NewProcessRegistry()
	proc := cpSleepProc(t)
	start := safeHostStartTime(proc.Process.Pid)
	if start == nil {
		t.Fatal("start time unreadable")
	}
	s := cpDetach(r, proc.Process.Pid, "proc_order", start, true)
	s.OutputBuffer = "line one\nline two\n"
	ctx := context.Background()
	check := func(name string, got []string, want ...string) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s keys %v, want %v", name, got, want)
		}
	}
	check("poll", r.pollDict(ctx, env, s.ID).Keys, "session_id", "command", "status", "pid", "uptime_seconds", "output_preview", "detached", "note")
	check("log", r.readLogDict(env, s.ID, nil, 200).Keys, "session_id", "command", "status", "output", "total_lines", "showing")
	one := 1
	check("wait timeout", r.waitDict(ctx, env, s.ID, &one).Keys, "status", "command", "output", "process_running", "timeout_note")
	zero := 0
	check("wait bad timeout", r.waitDict(ctx, env, s.ID, &zero).Keys, "status", "error")
	listed := r.listSessionDicts(ctx, env, "", "", false)
	if len(listed) != 1 {
		t.Fatalf("listed %d sessions", len(listed))
	}
	check("list entry", listed[0].Keys, "session_id", "command", "cwd", "pid", "owner_task_id", "started_at", "uptime_seconds", "status", "output_preview", "notify_on_complete", "detached")
	check("write on a recovered process", r.writeStdinDict(env, s.ID, "x").Keys, "status", "error")
	check("kill", r.killDict(ctx, env, s.ID, "", true).Keys, "status", "session_id", "completion_reason", "termination_source", "output")
	check("poll after kill", r.pollDict(ctx, env, s.ID).Keys, "session_id", "command", "status", "pid", "uptime_seconds", "output_preview", "exit_code", "completion_reason", "termination_source", "detached", "note")
	check("kill again", r.killDict(ctx, env, s.ID, "", true).Keys, "status", "command", "exit_code", "completion_reason", "termination_source", "output")
	check("missing", r.pollDict(ctx, env, "nope").Keys, "status", "error")
	listed = r.listSessionDicts(ctx, env, "", "", false)
	if len(listed) != 1 {
		t.Fatalf("listed %d sessions after the kill", len(listed))
	}
	check("list exited entry", listed[0].Keys, "session_id", "command", "cwd", "pid", "owner_task_id", "started_at", "uptime_seconds", "status", "output_preview", "notify_on_complete", "exit_code", "exited_at", "completion_reason", "detached")
}

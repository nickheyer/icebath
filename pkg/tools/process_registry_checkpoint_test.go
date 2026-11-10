package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Child half of the readopt tests, a process that only sleeps
func TestProcessCheckpointSleepHelper(t *testing.T) {
	if os.Getenv("PROC_CP_SLEEP") != "1" {
		t.Skip("helper process only")
	}
	time.Sleep(60 * time.Second)
}

// Starts a sleeping child and kills it at cleanup
func cpSleepProc(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessCheckpointSleepHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "PROC_CP_SLEEP=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	return cmd
}

// Reports whether the child is still running without reaping it
func cpStillRunning(cmd *exec.Cmd) bool {
	return isHostPidAlive(cmd.Process.Pid)
}

// Registry env with the crash checkpoint redirected into a temp home
func cpProcEnv(t *testing.T) (*Env, string) {
	t.Helper()
	home := cpTempDir(t)
	checkpoint := filepath.Join(home, "processes.json")
	cpSwap(t, &checkpointPathFn, func(*Env) string { return checkpoint })
	return &Env{Home: home}, checkpoint
}

// Detached host session for a live child with the given start time baseline
func cpDetach(r *ProcessRegistry, pid int, sid string, start *int64, notify bool) *ProcessSession {
	s := NewProcessSession(sid, "sleep-for-readopt")
	s.TaskID = "t1"
	s.StartedAt = nowSeconds()
	s.Pid = pid
	s.PidScope = "host"
	s.Detached = true
	s.HostStartTime = start
	s.NotifyOnComplete = notify
	s.finishInit()
	r.mu.Lock()
	r.insertRunning(s)
	r.mu.Unlock()
	return s
}

// Drains every queued completion event
func cpCompletions(r *ProcessRegistry) []map[string]any {
	found := []map[string]any{}
	for !r.CompletionQueue.Empty() {
		evt, ok := r.CompletionQueue.GetNowait()
		if !ok {
			break
		}
		found = append(found, evt)
	}
	return found
}

// Start time probe failing for every pid during the test
func cpFailStartProbe(t *testing.T) {
	t.Helper()
	cpSwap(t, &safeHostStartTimeSeam, func(int) *int64 { return nil })
}

func TestProcessCheckpointLiveMatchingPidStaysRunningWhenLaterStartProbeFails(t *testing.T) {
	env, _ := cpProcEnv(t)
	r := NewProcessRegistry()
	proc := cpSleepProc(t)
	start := safeHostStartTime(proc.Process.Pid)
	if start == nil {
		t.Fatal("start time unreadable")
	}
	s := cpDetach(r, proc.Process.Pid, "proc_live_probe", start, true)
	cpFailStartProbe(t)
	polled := r.Poll(context.Background(), env, s.ID)
	if !cpStillRunning(proc) || polled["status"] != "running" || s.Exited || len(cpCompletions(r)) != 0 {
		t.Fatalf("live child reported exited: %v", polled)
	}
	if _, has := polled["exit_code"]; has {
		t.Fatal("exit code invented")
	}
	r.mu.Lock()
	_, running := r.running[s.ID]
	r.mu.Unlock()
	if !running {
		t.Fatal("session left the running table")
	}
}

func TestProcessCheckpointReusedPidIsClosedWithoutKillOrCompletion(t *testing.T) {
	env, _ := cpProcEnv(t)
	r := NewProcessRegistry()
	proc := cpSleepProc(t)
	real := safeHostStartTime(proc.Process.Pid)
	if real == nil {
		t.Fatal("start time unreadable")
	}
	wrong := *real + 1
	s := cpDetach(r, proc.Process.Pid, "proc_reused", &wrong, true)
	polled := r.Poll(context.Background(), env, s.ID)
	if !cpStillRunning(proc) {
		t.Fatal("reused pid was killed")
	}
	r.mu.Lock()
	_, running := r.running[s.ID]
	_, finished := r.finished[s.ID]
	r.mu.Unlock()
	if !s.Exited || s.ExitCode != nil || running || !finished || len(cpCompletions(r)) != 0 {
		t.Fatalf("reused entry not closed cleanly: exited=%v code=%v running=%v finished=%v", s.Exited, s.ExitCode, running, finished)
	}
	if polled["status"] == "running" || polled["exit_code"] != nil {
		t.Fatalf("poll wrong: %v", polled)
	}
}

func TestProcessCheckpointKillTerminatesReadoptedPidWhenStartProbeFails(t *testing.T) {
	env, _ := cpProcEnv(t)
	r := NewProcessRegistry()
	proc := cpSleepProc(t)
	start := safeHostStartTime(proc.Process.Pid)
	if start == nil {
		t.Fatal("start time unreadable")
	}
	s := cpDetach(r, proc.Process.Pid, "proc_kill_probe", start, true)
	cpFailStartProbe(t)
	result := r.KillProcess(context.Background(), env, s.ID, "", false)
	if result["status"] != "killed" {
		t.Fatalf("kill did not reach the child: %v", result)
	}
	done := make(chan error, 1)
	go func() { done <- proc.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("child survived the kill")
	}
	if !s.Exited {
		t.Fatal("session not marked exited")
	}
}

func TestProcessCheckpointGonePidIsPrunedWithoutAFalseCompletion(t *testing.T) {
	env, _ := cpProcEnv(t)
	r := NewProcessRegistry()
	start := int64(1)
	s := cpDetach(r, 1<<31-1, "proc_gone", &start, true)
	s.Command = "already-dead"
	listed := r.ListSessions(context.Background(), env, "", "", false)
	polled := r.Poll(context.Background(), env, s.ID)
	r.mu.Lock()
	_, running := r.running[s.ID]
	r.mu.Unlock()
	if running {
		t.Fatal("gone pid still running")
	}
	for _, row := range listed {
		if row["session_id"] == s.ID {
			t.Fatal("gone pid listed")
		}
	}
	if polled["status"] != "not_found" || len(cpCompletions(r)) != 0 {
		t.Fatalf("gone pid wrong: %v", polled)
	}
}

func TestProcessCheckpointRecoverReattachesLivePidWhenStartProbeFails(t *testing.T) {
	env, checkpoint := cpProcEnv(t)
	r := NewProcessRegistry()
	proc := cpSleepProc(t)
	start := safeHostStartTime(proc.Process.Pid)
	if start == nil {
		t.Fatal("start time unreadable")
	}
	cpWriteEntries(t, checkpoint, []map[string]any{{
		"session_id": "proc_recover_live", "command": "sleep-for-readopt", "pid": proc.Process.Pid,
		"pid_scope": "host", "host_start_time": *start, "task_id": "t1", "notify_on_complete": true,
	}})
	cpFailStartProbe(t)
	recovered, _ := r.RecoverFromCheckpoint(context.Background(), env)
	polled := r.Poll(context.Background(), env, "proc_recover_live")
	if recovered != 1 || !cpStillRunning(proc) || polled["status"] != "running" || len(cpCompletions(r)) != 0 {
		t.Fatalf("live recovery wrong: recovered=%d poll=%v", recovered, polled)
	}
	if _, has := polled["exit_code"]; has {
		t.Fatal("exit code invented")
	}
}

func TestProcessCheckpointRecoverDoesNotKillAReusedPid(t *testing.T) {
	env, checkpoint := cpProcEnv(t)
	r := NewProcessRegistry()
	proc := cpSleepProc(t)
	real := safeHostStartTime(proc.Process.Pid)
	if real == nil {
		t.Fatal("start time unreadable")
	}
	cpWriteEntries(t, checkpoint, []map[string]any{{
		"session_id": "proc_recover_reused", "command": "sleep-for-readopt", "pid": proc.Process.Pid,
		"pid_scope": "host", "host_start_time": *real + 1, "task_id": "t1", "notify_on_complete": true,
		"systemd_unit": "icebath-worker-proc_recover_reused.scope",
	}})
	var stopped []string
	cpSwap(t, &stopSystemdUnit, func(unit string) bool {
		stopped = append(stopped, unit)
		return true
	})
	if recovered, err := r.RecoverFromCheckpoint(context.Background(), env); recovered != 0 || err != nil {
		t.Fatalf("reused pid adopted: %d", recovered)
	}
	if !cpStillRunning(proc) || r.Get(env, "proc_recover_reused") != nil || len(stopped) != 0 || len(cpCompletions(r)) != 0 {
		t.Fatalf("reused pid touched: stopped=%v", stopped)
	}
}

// Writes checkpoint entries the way a previous interpreter left them
func cpWriteEntries(t *testing.T, path string, entries []map[string]any) {
	t.Helper()
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProcessCheckpointOwnerWriteKeepsALiveWorkerHandoff(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("linux and macos shape")
	}
	env, checkpoint := cpProcEnv(t)
	logs := processOutputDir(env)
	os.MkdirAll(logs, 0o755)
	worker := cpSleepProc(t)
	owner := cpSleepProc(t)
	workerStart := safeHostStartTime(worker.Process.Pid)
	handoff := map[string]any{
		"session_id": "proc_worker", "command": "dev-server", "pid": worker.Process.Pid, "pid_scope": "host",
		"host_start_time": workerStart, "started_at": nowSeconds(), "output_log": filepath.Join(logs, "proc_worker.log"),
		"notify_on_complete": true,
	}
	os.WriteFile(filepath.Join(logs, "proc_worker.log"), []byte("serving\n"), 0o644)
	gone := map[string]any{}
	for k, v := range handoff {
		gone[k] = v
	}
	gone["session_id"] = "proc_gone"
	gone["pid"] = 1<<22 + 7
	gone["output_log"] = filepath.Join(logs, "proc_gone.log")
	os.WriteFile(filepath.Join(logs, "proc_gone.log"), nil, 0o644)
	cpWriteEntries(t, checkpoint, []map[string]any{handoff, gone})

	ownerReg := NewProcessRegistry()
	s := NewProcessSession("proc_owner", "owner-job")
	s.TaskID = "t"
	s.StartedAt = nowSeconds()
	s.Pid = owner.Process.Pid
	s.PidScope = "host"
	s.HostStartTime = safeHostStartTime(owner.Process.Pid)
	s.finishInit()
	ownerReg.mu.Lock()
	ownerReg.insertRunning(s)
	ownerReg.mu.Unlock()
	ownerReg.writeCheckpoint(env, nil)

	value, ok := readCheckpointValue(checkpoint)
	if !ok {
		t.Fatal("checkpoint unreadable")
	}
	ids := map[string]bool{}
	for _, e := range value.([]any) {
		ids[asStr(e.(*pyjson.Object).Vals["session_id"])] = true
	}
	if len(ids) != 2 || !ids["proc_owner"] || !ids["proc_worker"] {
		t.Fatalf("handoff dropped or dead entry kept: %v", ids)
	}

	successor := NewProcessRegistry()
	successor.mu.Lock()
	successor.insertRunning(s)
	successor.mu.Unlock()
	if recovered, err := successor.RecoverFromCheckpoint(context.Background(), env); recovered != 1 || err != nil {
		t.Fatalf("worker handoff not adopted: %d", recovered)
	}
	adopted := successor.Get(env, "proc_worker")
	if adopted == nil {
		t.Fatal("adopted worker not visible")
	}
	worker.Process.Kill()
	worker.Wait()
	if isHostPidAlive(worker.Process.Pid) {
		t.Fatal("worker survived")
	}
	select {
	case <-adopted.readerDone:
	case <-time.After(15 * time.Second):
		t.Fatal("log reader of the adopted worker never finished")
	}
}

func TestCheckpointRewriteKeepsForeignEntryOrderAndNumberTypes(t *testing.T) {
	env, checkpoint := cpProcEnv(t)
	logs := processOutputDir(env)
	os.MkdirAll(logs, 0o755)
	log := filepath.Join(logs, "proc_w.log")
	os.WriteFile(log, []byte("done\n"), 0o644)
	os.WriteFile(exitPathOf(log), []byte("0"), 0o644)
	quoted, err := pyjson.Dumps(log, pyjson.EnsureASCII(false))
	if err != nil {
		t.Fatal(err)
	}
	onDisk := `[{"zeta": 1, "session_id": "proc_w", "ratio": 1.0, "big": 1e2, "neg": -0.0, "command": "a<b>&c é", "pid": 7, "output_log": ` + quoted + `, "nested": {"y": 1, "x": [1.0, 2]}}, 5, "text"]`
	if err := os.WriteFile(checkpoint, []byte(onDisk), 0o644); err != nil {
		t.Fatal(err)
	}
	NewProcessRegistry().writeCheckpoint(env, nil)
	got, err := os.ReadFile(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	want := "[\n  {\n    \"zeta\": 1,\n    \"session_id\": \"proc_w\",\n    \"ratio\": 1.0,\n    \"big\": 100.0,\n    \"neg\": -0.0,\n    \"command\": \"a<b>&c é\",\n    \"pid\": 7,\n    \"output_log\": " + quoted + ",\n    \"nested\": {\n      \"y\": 1,\n      \"x\": [\n        1.0,\n        2\n      ]\n    }\n  }\n]"
	if string(got) != want {
		t.Fatalf("checkpoint rewrite\n%s\nwant\n%s", got, want)
	}
}

func TestMalformedCheckpointFailsAsHermesDoes(t *testing.T) {
	cases := []struct{ text, recoverErr, writeErr string }{
		{`{"a":1}`, "AttributeError: 'str' object has no attribute 'get'", ""},
		{`"abc"`, "AttributeError: 'str' object has no attribute 'get'", ""},
		{`5`, "TypeError: 'int' object is not iterable", "TypeError: 'int' object is not iterable"},
		{`1.5`, "TypeError: 'float' object is not iterable", "TypeError: 'float' object is not iterable"},
		{`true`, "TypeError: 'bool' object is not iterable", "TypeError: 'bool' object is not iterable"},
		{`null`, "TypeError: 'NoneType' object is not iterable", "TypeError: 'NoneType' object is not iterable"},
		{`[1]`, "AttributeError: 'int' object has no attribute 'get'", ""},
		{`[null]`, "AttributeError: 'NoneType' object has no attribute 'get'", ""},
		{`["s"]`, "AttributeError: 'str' object has no attribute 'get'", ""},
		{`[[1]]`, "AttributeError: 'list' object has no attribute 'get'", ""},
		{`[true]`, "AttributeError: 'bool' object has no attribute 'get'", ""},
		{`[1.5]`, "AttributeError: 'float' object has no attribute 'get'", ""},
		{`{}`, "", ""},
		{`""`, "", ""},
		{`[]`, "", ""},
		{`[{"pid": 0}, 7]`, "AttributeError: 'int' object has no attribute 'get'", ""},
		{`[{"session_id": [1]}]`, "", "TypeError: cannot use 'list' as a set element (unhashable type: 'list')"},
		{`[{"session_id": {"k": 1}}]`, "", "TypeError: cannot use 'dict' as a set element (unhashable type: 'dict')"},
	}
	describe := func(err error) string {
		if err == nil {
			return ""
		}
		return pyrepr.TypeName(err) + ": " + err.Error()
	}
	for _, c := range cases {
		env, checkpoint := cpProcEnv(t)
		if err := os.WriteFile(checkpoint, []byte(c.text), 0o644); err != nil {
			t.Fatal(err)
		}
		r := NewProcessRegistry()
		_, werr := r.foreignHandoffs(env, checkpoint, map[string]bool{"a": true})
		if got := describe(werr); got != c.writeErr {
			t.Errorf("%s foreign handoffs %q, want %q", c.text, got, c.writeErr)
		}
		r.writeCheckpoint(env, nil)
		after, _ := os.ReadFile(checkpoint)
		want := "[]"
		if c.writeErr != "" {
			want = c.text
		}
		if string(after) != want {
			t.Errorf("%s checkpoint after the write %q, want %q", c.text, after, want)
		}
		if err := os.WriteFile(checkpoint, []byte(c.text), 0o644); err != nil {
			t.Fatal(err)
		}
		recovered, rerr := NewProcessRegistry().RecoverFromCheckpoint(context.Background(), env)
		if got := describe(rerr); got != c.recoverErr || recovered != 0 {
			t.Errorf("%s recover %d %q, want %q", c.text, recovered, got, c.recoverErr)
		}
	}
}

package cron

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Worker double whose wait and poll answer fixed exit codes like a mocked Popen
type fakeWorker struct{ code int }

func (f fakeWorker) Wait(time.Duration) (*int, bool) { c := f.code; return &c, true }
func (f fakeWorker) Poll() *int                      { c := f.code; return &c }

// Writes a handoff payload for job-1 exec-1 owned by the profile directory under the temp root
func writeTestPayload(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "payload.json")
	body, _ := json.Marshal(map[string]any{"job": map[string]any{"id": "job-1", "execution_id": "exec-1"}, "profile_home": filepath.Join(dir, "profile")})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Replaces the adopt and run seams for one test, recording the home each one observed
func stubWorkerRun(t *testing.T, adopted map[string]any, ran *[]*Job, homes *[]string) {
	t.Helper()
	prevAdopt, prevRun := adoptClaimedExecution, runOneJobSeam
	t.Cleanup(func() { adoptClaimedExecution, runOneJobSeam = prevAdopt, prevRun })
	adoptClaimedExecution = func(ctx context.Context, s *Store, id string) (map[string]any, error) {
		*homes = append(*homes, home.Dir(ctx))
		if adopted == nil {
			return nil, nil
		}
		return map[string]any{"id": id, "status": adopted["status"]}, nil
	}
	runOneJobSeam = func(ctx context.Context, s *Store, job *Job, _ runOneJobOptions) (bool, error) {
		*ran = append(*ran, job)
		*homes = append(*homes, home.Dir(ctx))
		return true, nil
	}
}

func TestExternalWorkerAdoptsExecutionAndRunsPayloadOnce(t *testing.T) {
	dir := t.TempDir()
	payload := writeTestPayload(t, dir)
	ack := filepath.Join(dir, "exec-1.ready")
	capture := filepath.Join(dir, "exec-1.stderr")
	if err := os.WriteFile(capture, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var ran []*Job
	var homes []string
	stubWorkerRun(t, map[string]any{"status": "running"}, &ran, &homes)
	if !runExternalWorkerPayload(context.Background(), payload, ack) {
		t.Fatal("payload run returned false")
	}
	if len(ran) != 1 || ran[0].ID != "job-1" || ran[0].ExecutionID == nil || *ran[0].ExecutionID != "exec-1" {
		t.Fatalf("ran %+v", ran)
	}
	want := filepath.Join(dir, "profile")
	if len(homes) != 2 || homes[0] != want || homes[1] != want {
		t.Fatalf("homes %v want %s", homes, want)
	}
	if _, err := os.Stat(ack); err != nil {
		t.Fatalf("ack missing %v", err)
	}
	for _, gone := range []string{payload, capture} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s still exists", gone)
		}
	}
}

func TestExternalWorkerAckIsNeverObservableHalfWritten(t *testing.T) {
	dir := t.TempDir()
	payload := writeTestPayload(t, dir)
	ack := filepath.Join(dir, "exec-1.ready")
	var ran []*Job
	var homes []string
	stubWorkerRun(t, map[string]any{"status": "running"}, &ran, &homes)
	if !runExternalWorkerPayload(context.Background(), payload, ack) {
		t.Fatal("payload run returned false")
	}
	raw, err := os.ReadFile(ack)
	var body map[string]any
	if err != nil || json.Unmarshal(raw, &body) != nil || body["execution_id"] != "exec-1" || body["pid"] != float64(os.Getpid()) {
		t.Fatalf("ack %s %v", raw, err)
	}
	var names []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "exec-1") {
			names = append(names, e.Name())
		}
	}
	if len(names) != 1 || names[0] != "exec-1.ready" {
		t.Fatalf("left behind %v", names)
	}
	if err := publishWorkerAck(ack, "exec-2"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ack+".tmp"+strconv.Itoa(os.Getpid()), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := publishWorkerAck(ack, "exec-3"); err == nil {
		t.Fatal("publishing over a live temp sibling succeeded")
	}
	if raw, _ := os.ReadFile(ack); !strings.Contains(string(raw), "exec-2") {
		t.Fatalf("a failed publish replaced the ack %s", raw)
	}
}

func TestExternalWorkerRefusesToRunWithoutDurableOwnership(t *testing.T) {
	dir := t.TempDir()
	payload := writeTestPayload(t, dir)
	ack := filepath.Join(dir, "ready.json")
	var ran []*Job
	var homes []string
	stubWorkerRun(t, nil, &ran, &homes)
	if runExternalWorkerPayload(context.Background(), payload, ack) {
		t.Fatal("refused payload returned true")
	}
	if len(ran) != 0 {
		t.Fatalf("ran %v", ran)
	}
	if _, err := os.Stat(ack); !os.IsNotExist(err) {
		t.Fatal("ack published without ownership")
	}
}

func TestLaunchExternalWorkerStaysInProcessOutsideManagedGateway(t *testing.T) {
	s, _ := newTestStore(t)
	var calls [][]string
	prev := restartSafeChildArgv
	t.Cleanup(func() { restartSafeChildArgv = prev })
	restartSafeChildArgv = func(command []string, unitSuffix string, require, outlives bool) (tools.GatewayChildDispatch, error) {
		calls = append(calls, append(command, unitSuffix))
		return tools.GatewayChildDispatch{Mode: "in_process", Argv: command}, nil
	}
	handed, err := s.launchExternalCronWorker(context.Background(), &Job{ID: "job-1", ExecutionID: strPtr("exec-1")})
	if err != nil || handed {
		t.Fatalf("handed %v err %v", handed, err)
	}
	if len(calls) != 1 || calls[0][1] != ExternalWorkerEntry || calls[0][len(calls[0])-1] != "cron-job-1-exec-exec-1" {
		t.Fatalf("calls %v", calls)
	}
	if _, err := os.Stat(filepath.Join(s.home, "cron", "external-workers")); !os.IsNotExist(err) {
		t.Fatal("in process launch prepared a handoff")
	}
}

func TestWorkerDyingBeforeAckNamesItsStderrCause(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	execution, err := openExecutionsLedger(s).Create(ctx, "job-1", "direct", nil)
	if err != nil {
		t.Fatal(err)
	}
	executionID := execution["id"].(string)
	prev := restartSafeChildArgv
	t.Cleanup(func() { restartSafeChildArgv = prev })
	restartSafeChildArgv = func([]string, string, bool, bool) (tools.GatewayChildDispatch, error) {
		return tools.GatewayChildDispatch{Mode: "direct", Argv: []string{"/bin/sh", "-c", "echo \"ModuleNotFoundError: No module named 'cron_module_that_does_not_exist'\" >&2; exit 1"}}, nil
	}
	_, err = s.launchExternalCronWorker(ctx, &Job{ID: "job-1", ExecutionID: &executionID, Prompt: "work"})
	if err == nil || !strings.Contains(err.Error(), "exit 1") || !strings.Contains(err.Error(), "No module named 'cron_module_that_does_not_exist'") {
		t.Fatalf("err %v", err)
	}
	var postErr *externalWorkerPostHandoffError
	if errors.As(err, &postErr) {
		t.Fatal("a pre ack death is a dispatch failure, not a post handoff failure")
	}
	left, _ := filepath.Glob(filepath.Join(s.home, "cron", "external-workers", executionID+".*"))
	sort.Strings(left)
	if len(left) != 0 {
		t.Fatalf("left behind %v", left)
	}
}

func TestExternalWorkerExitRechecksExactExecutionBeforeFailure(t *testing.T) {
	s, _ := newTestStore(t)
	statuses := []string{"running", "completed"}
	calls := 0
	prev := getExecution
	t.Cleanup(func() { getExecution = prev })
	getExecution = func(context.Context, *Store, string) (map[string]any, error) {
		st := statuses[calls]
		calls++
		return map[string]any{"id": "exec-1", "status": st}, nil
	}
	ok, err := s.waitForExternalCronWorker(context.Background(), fakeWorker{0}, "exec-1", nil, nil, nil)
	if err != nil || !ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	if calls != 2 {
		t.Fatalf("calls %d", calls)
	}
}

func TestExternalWorkerCrashRecoversUncertainAttempt(t *testing.T) {
	s, _ := newTestStore(t)
	statuses := []string{"running", "unknown"}
	calls, recovered := 0, 0
	prevGet, prevRecover := getExecution, recoverInterruptedExecutions
	t.Cleanup(func() { getExecution, recoverInterruptedExecutions = prevGet, prevRecover })
	getExecution = func(context.Context, *Store, string) (map[string]any, error) {
		st := statuses[calls]
		calls++
		return map[string]any{"id": "exec-1", "status": st}, nil
	}
	recoverInterruptedExecutions = func(context.Context, *Store) (int, error) { recovered++; return 1, nil }
	_, err := s.waitForExternalCronWorker(context.Background(), fakeWorker{9}, "exec-1", nil, nil, nil)
	var postErr *externalWorkerPostHandoffError
	if !errors.As(err, &postErr) || !strings.Contains(err.Error(), "status 9") {
		t.Fatalf("err %v", err)
	}
	if recovered != 1 || calls != 2 {
		t.Fatalf("recovered %d calls %d", recovered, calls)
	}
}

func TestTerminalEarlyReturnReapsARealWorkerProcess(t *testing.T) {
	s, _ := newTestStore(t)
	prev := getExecution
	t.Cleanup(func() { getExecution = prev })
	getExecution = func(context.Context, *Store, string) (map[string]any, error) {
		return map[string]any{"id": "exec-1", "status": "completed"}, nil
	}
	capture := filepath.Join(t.TempDir(), "w.stderr")
	proc, _, err := spawnExternalWorker([]string{"/bin/sh", "-c", "sleep 1.3"}, "", map[string]string{"PATH": os.Getenv("PATH")}, capture)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := s.waitForExternalCronWorkerBody(context.Background(), proc, "exec-1", nil)
	if err != nil || !ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	if code, exited := proc.Wait(8 * time.Second); !exited || *code != 0 {
		t.Fatalf("worker not reaped %v %v", code, exited)
	}
}

func TestLaunchExternalWorkerHandsAScriptJobToARealWorkerThatAcknowledges(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	scripts := filepath.Join(s.home, "scripts")
	if err := os.MkdirAll(scripts, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scripts, "w.sh"), []byte("#!/bin/sh\necho handed off\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	job, err := s.createJob(CreateJobOptions{Schedule: "every 5m", Script: "w.sh", NoAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := openExecutionsLedger(s).Create(ctx, job.ID, "direct", nil)
	if err != nil {
		t.Fatal(err)
	}
	executionID := execution["id"].(string)
	job.ExecutionID = &executionID
	var suffixes []string
	prev := restartSafeChildArgv
	t.Cleanup(func() { restartSafeChildArgv = prev })
	restartSafeChildArgv = func(command []string, unitSuffix string, _, _ bool) (tools.GatewayChildDispatch, error) {
		suffixes = append(suffixes, unitSuffix)
		return tools.GatewayChildDispatch{Mode: "direct", Argv: command}, nil
	}
	key := s.inflightKey(job.ID, "")
	t.Cleanup(func() { inflight.Lock(); delete(inflight.workerPIDs, key); inflight.Unlock() })
	handed, err := s.launchExternalCronWorker(ctx, job)
	if err != nil || !handed {
		t.Fatalf("handed %v err %v", handed, err)
	}
	if len(suffixes) != 1 || suffixes[0] != "cron-"+job.ID+"-exec-"+executionID {
		t.Fatalf("suffixes %v", suffixes)
	}
	row, err := openExecutionsLedger(s).Get(ctx, executionID)
	if err != nil || row == nil || row["status"] != "completed" {
		t.Fatalf("row %v err %v", row, err)
	}
	inflight.Lock()
	pid, waiting := inflight.workerPIDs[key], inflightGuard.restartSafeWaiters[key]
	inflight.Unlock()
	if pid <= 0 || pid == os.Getpid() || waiting {
		t.Fatalf("worker pid %d waiter still registered %v", pid, waiting)
	}
	if left, _ := filepath.Glob(filepath.Join(s.home, "cron", "external-workers", executionID+".*")); len(left) != 0 {
		t.Fatalf("left behind %v", left)
	}
}

package cron

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Model every tests/cron case runs with, the autouse fixture of its conftest
func pinCronTestModel(t *testing.T) {
	t.Setenv("ICEBATH_MODEL", "test-cron-default-model")
}

// Runner double over a real session store that drains when its flag is set
type drainingRunner struct {
	progressStampRunner
	draining *atomic.Bool
}

func (r drainingRunner) Draining() bool { return r.draining.Load() }

// Real session authority over the test home that never schedules, gateway config model {} and no cli toolsets
func recoveryOwner(t *testing.T, s *Store) (*gateway.SessionAuthority, *state.SessionDB, context.Context) {
	t.Helper()
	authority, db, ctx, _ := drainableOwner(t, s)
	prev := gateway.SwapSessionSchedule(func(context.Context, *gateway.SessionAuthority, gateway.SessionRef) error { return nil })
	t.Cleanup(func() { gateway.SwapSessionSchedule(prev) })
	return authority, db, ctx
}

// Real session authority over the test home with its runner draining flag
func drainableOwner(t *testing.T, s *Store) (*gateway.SessionAuthority, *state.SessionDB, context.Context, *atomic.Bool) {
	t.Helper()
	ctx := home.WithOverride(context.Background(), s.Home())
	writeTestFile(t, filepath.Join(s.Home(), "config.yaml"), "model: {}\nplatform_toolsets:\n  cli: []\n")
	db, err := state.NewSessionDB(ctx, filepath.Join(s.Home(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	epoch, err := db.BeginRuntimeEpoch(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	cfg := gateway.DefaultGatewayConfig(ctx)
	draining := &atomic.Bool{}
	runner := drainingRunner{progressStampRunner{store: gateway.NewSessionStore(ctx, filepath.Join(s.Home(), "sessions"), cfg, nil),
		adapters: pydict.New[gateway.Platform, gateway.PlatformAdapter]()}, draining}
	return gateway.NewSessionAuthority(runner, s.Home(), "test", db, epoch), db, ctx, draining
}

// Binds the authority as the cron owner of its home until the returned release runs
func bindRecoveryOwner(ctx context.Context, authority *gateway.SessionAuthority) func() {
	loop, stop := context.WithCancel(ctx)
	gateway.BindCronOwner(loop, authority)
	return func() {
		gateway.UnbindCronOwner(authority)
		stop()
	}
}

// Submits one fire through the owner and returns the session its admission opened
func ownerSubmit(t *testing.T, ctx context.Context, authority *gateway.SessionAuthority, params map[string]any) string {
	t.Helper()
	receipt, err := gateway.CronOwnerOperation(ctx, authority, "submit", params)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return receipt["session_id"].(string)
}

// Claims the session head at the authority epoch
func ownerClaim(t *testing.T, ctx context.Context, db *state.SessionDB, authority *gateway.SessionAuthority, sessionID string) *pyjson.Object {
	t.Helper()
	row, _, err := db.ClaimSessionInput(ctx, state.ClaimInput{Epoch: authority.Epoch, SessionID: sessionID})
	if err != nil || row == nil {
		t.Fatalf("claim = %v, %v", row, err)
	}
	return row
}

// Claims the session head and settles it completed with the canonical cron result
func ownerSettleAnswer(t *testing.T, ctx context.Context, db *state.SessionDB, authority *gateway.SessionAuthority, sessionID string) {
	t.Helper()
	row := ownerClaim(t, ctx, db, authority, sessionID)
	admission, _ := row.Get("admission_id")
	generation, _ := row.Get("generation")
	gen, ok := generation.(int64)
	if !ok {
		t.Fatalf("generation = %#v", generation)
	}
	result := pyjsonCronResult(true, "document", "answer")
	if _, err := db.SettleSessionInput(ctx, state.SettleInput{Epoch: authority.Epoch, AdmissionID: admission.(string), Generation: gen,
		Outcome: "completed", Result: result}); err != nil {
		t.Fatal(err)
	}
}

// Settled admission result carrying a canonical cron result with no error
func pyjsonCronResult(success bool, output, answer string) *pyjson.Object {
	return pyjson.ObjectOf("result", pyjson.ObjectOf("cron_result", []any{success, output, answer, nil}), "usage", pyjson.ObjectOf())
}

func writeFireJournal(t *testing.T, s *Store, jobID, fire string) string {
	t.Helper()
	journal := s.journalPath(jobID, fire)
	writeTestFile(t, journal, `{"params": {"job_id": "`+jobID+`", "request_id": "`+fire+`", "extra_prompt": null}, "receipt": null}`)
	return journal
}

func TestLostAckReconcilesFrozenDeliveryOnce(t *testing.T) {
	s, _ := newTestStore(t)
	pinCronTestModel(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("original"), Schedule: "every 1h", Deliver: prompt("telegram:123")})
	authority, db, ctx := recoveryOwner(t, s)
	q := openDeliveryQueue(s)
	params := map[string]any{"job_id": job.ID, "request_id": "fire", "extra_prompt": nil}
	ownerSettleAnswer(t, ctx, db, authority, ownerSubmit(t, ctx, authority, params))
	writeFireJournal(t, s, job.ID, "fire")
	release := bindRecoveryOwner(ctx, authority)
	defer release()
	mustReconcile(t, s)
	if status, err := q.GetStatus(ctx, "fire"); err != nil || status == nil || status["status"] != "pending" {
		t.Fatalf("delivery status = %v, %v", status, err)
	}
	saved := mustGet(t, s, job.ID)
	if deref(saved.LastStatus) != "delivery_queued" {
		t.Fatalf("last_status = %q", deref(saved.LastStatus))
	}
	completed := saved.Repeat.Completed
	writeFireJournal(t, s, job.ID, "fire")
	mustReconcile(t, s)
	if got := mustGet(t, s, job.ID).Repeat.Completed; got != completed {
		t.Fatalf("completed = %d, want %d", got, completed)
	}
	if claimed, err := q.ClaimNext(ctx); err != nil || claimed == nil || claimed["content"] != "answer" {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	if claimed, err := q.ClaimNext(ctx); err != nil || claimed != nil {
		t.Fatalf("second claim = %v, %v", claimed, err)
	}
	unknown := map[string]any{"job_id": job.ID, "request_id": "unknown", "extra_prompt": nil}
	ownerClaim(t, ctx, db, authority, ownerSubmit(t, ctx, authority, unknown))
	epoch, err := db.BeginRuntimeEpoch(ctx, "restart")
	if err != nil {
		t.Fatal(err)
	}
	authority.Epoch = epoch
	if _, err := db.RecoverSessionInputs(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	unknownJournal := writeFireJournal(t, s, job.ID, "unknown")
	mustReconcile(t, s)
	if !journalExists(unknownJournal) {
		t.Fatal("unknown journal retired")
	}
	if state := mustGet(t, s, job.ID).State; state != "paused" {
		t.Fatalf("state = %s", state)
	}
	if claimed, err := q.ClaimNext(ctx); err != nil || claimed != nil {
		t.Fatalf("claim after unknown = %v, %v", claimed, err)
	}
}

func TestRecoveredReceiptSettlesTheDepartedFirersExecutionRow(t *testing.T) {
	s, _ := newTestStore(t)
	pinCronTestModel(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("original"), Schedule: "every 1h", Deliver: prompt("local")})
	authority, db, ctx := recoveryOwner(t, s)
	cmd := ledgerChildCommand(s.Home(), "", "create-running-direct", job.ID)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("firer: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	fire := lines[len(lines)-1]
	if row := executionRow(t, s, fire); row["status"] != "running" {
		t.Fatalf("status = %v", row["status"])
	}
	params := map[string]any{"job_id": job.ID, "request_id": fire, "extra_prompt": nil}
	ownerSettleAnswer(t, ctx, db, authority, ownerSubmit(t, ctx, authority, params))
	journal := writeFireJournal(t, s, job.ID, fire)
	release := bindRecoveryOwner(ctx, authority)
	mustReconcile(t, s)
	release()
	if journalExists(journal) {
		t.Fatal("journal kept")
	}
	settled := executionRow(t, s, fire)
	if settled["status"] != "completed" || settled["delivery_outcome"] != "suppressed" {
		t.Fatalf("settled = %v %v", settled["status"], settled["delivery_outcome"])
	}
	if n, err := openExecutionsLedger(s).RecoverInterrupted(ctx); err != nil || n != 0 {
		t.Fatalf("recovered = %d, %v", n, err)
	}
	if row := executionRow(t, s, fire); row["status"] != "completed" {
		t.Fatalf("status after sweep = %v", row["status"])
	}
}

// Job whose workdir is removed after creation, the definite pre-admission refusal of the owner
func removedWorkdirJob(t *testing.T, s *Store) *Job {
	t.Helper()
	workdir := filepath.Join(t.TempDir(), "work")
	if err := os.Mkdir(workdir, 0o700); err != nil {
		t.Fatal(err)
	}
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("original"), Schedule: "every 1h", Deliver: prompt("local"), Workdir: workdir})
	if err := os.Remove(workdir); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestOwnerRefusedFireIsBookedFailedAndRetiresItsJournal(t *testing.T) {
	s, _ := newTestStore(t)
	pinCronTestModel(t)
	job := removedWorkdirJob(t, s)
	authority, _, ctx := recoveryOwner(t, s)
	release := bindRecoveryOwner(ctx, authority)
	out, err := s.runCanonicalJob(ctx, mustGet(t, s, job.ID), nil, nil, strPtr("fire"))
	if err != nil || out.Success || out.Error == nil || !strings.Contains(*out.Error, "invalid_params") {
		t.Fatalf("out %+v err %v", out, err)
	}
	if !journalExists(s.journalPath(job.ID, "fire")) {
		t.Fatal("refusal journal missing before bookkeeping")
	}
	mustReconcile(t, s)
	if journalExists(s.journalPath(job.ID, "fire")) {
		t.Fatal("refusal journal kept")
	}
	release()
	if mustGet(t, s, job.ID).State == "paused" {
		t.Fatal("job paused")
	}
}

func TestAdmissionJournalSurvivesPowerLoss(t *testing.T) {
	s, _ := newTestStore(t)
	pinCronTestModel(t)
	var mu sync.Mutex
	var synced []string
	real := pyfs.FsyncDirectory
	pyfs.FsyncDirectory = func(path string) {
		mu.Lock()
		synced = append(synced, path)
		mu.Unlock()
		real(path)
	}
	t.Cleanup(func() { pyfs.FsyncDirectory = real })
	job := removedWorkdirJob(t, s)
	authority, _, ctx := recoveryOwner(t, s)
	release := bindRecoveryOwner(ctx, authority)
	_, _ = s.runCanonicalJob(ctx, mustGet(t, s, job.ID), nil, nil, strPtr("fire"))
	release()
	mu.Lock()
	defer mu.Unlock()
	if dir := filepath.Dir(s.journalPath(job.ID, "fire")); !slices.Contains(synced, dir) {
		t.Fatalf("synced %v, want %s", synced, dir)
	}
}

func TestFailedManualRunExitsNonzero(t *testing.T) {
	s, _ := newTestStore(t)
	prev := cronAPIDoor
	t.Cleanup(func() { cronAPIDoor = prev })
	cronAPIDoor = func(*Store, context.Context, *cronjobArgs) *pyjson.Object {
		return pyjson.ObjectOf("success", true, "job", pyjson.ObjectOf("executed", true, "execution_success", false))
	}
	if rc := s.jobAction(context.Background(), io.Discard, "run", "job", "Triggered"); rc != 1 {
		t.Fatalf("rc = %d", rc)
	}
}

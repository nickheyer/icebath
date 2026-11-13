package cron

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Swaps run_job for a canned outcome and restores it when the test ends
func patchRunJob(t *testing.T, out jobRunOutcome, err error) *[]string {
	t.Helper()
	calls := &[]string{}
	prev := runJobSeam
	runJobSeam = func(ctx context.Context, s *Store, job *Job, o runJobOptions) (jobRunOutcome, error) {
		*calls = append(*calls, job.ID)
		return out, err
	}
	t.Cleanup(func() { runJobSeam = prev })
	return calls
}

// Clears the delivery double between tests
func resetDeliver(t *testing.T) {
	t.Helper()
	testDeliver.Lock()
	testDeliver.sent, testDeliver.err, testDeliver.targets = nil, nil, nil
	testDeliver.Unlock()
	t.Cleanup(func() {
		testDeliver.Lock()
		testDeliver.sent = nil
		testDeliver.Unlock()
	})
}

func runOneJobFor(t *testing.T, s *Store, name, deliver string) (*Job, bool) {
	t.Helper()
	got, _, ok := runOneJobRun(t, s, name, deliver)
	return got, ok
}

// Runs a fresh job and returns the stored record beside the in-memory job that carries the execution id
func runOneJobRun(t *testing.T, s *Store, name, deliver string) (*Job, *Job, bool) {
	t.Helper()
	job := mustCreate(t, s, CreateJobOptions{Prompt: strPtr("report"), Schedule: "every 1h", Name: strPtr(name), Deliver: strPtr(deliver)})
	ok, err := runOneJob(context.Background(), s, job, runOneJobOptions{})
	if err != nil {
		t.Fatalf("runOneJob: %v", err)
	}
	got, err := s.getJob(context.Background(), job.ID)
	if err != nil || got == nil {
		t.Fatalf("getJob: %v", err)
	}
	return got, job, ok
}

func queuedContent(t *testing.T, s *Store, job *Job) string {
	t.Helper()
	id := deref(job.ExecutionID)
	row, err := openDeliveryQueue(s).GetStatus(context.Background(), id)
	if err != nil || row == nil {
		t.Fatalf("queued row for %q: %v %v", id, row, err)
	}
	content, _ := row["content"].(string)
	return content
}

func TestRunOneJobSuccessSequence(t *testing.T) {
	s, _ := newTestStore(t)
	resetDeliver(t)
	calls := patchRunJob(t, jobRunOutcome{true, "out", "final response", nil}, nil)
	job, ran, ok := runOneJobRun(t, s, "t", "telegram")
	if !ok || len(*calls) != 1 {
		t.Fatalf("processed=%v calls=%v", ok, *calls)
	}
	if deref(job.LastStatus) != "delivery_queued" {
		t.Fatalf("last_status = %q", deref(job.LastStatus))
	}
	if got := queuedContent(t, s, ran); got != "final response" {
		t.Fatalf("queued content = %q", got)
	}
	entries, _ := os.ReadDir(s.outputDir + "/" + job.ID)
	if len(entries) != 1 {
		t.Fatalf("saved outputs = %d", len(entries))
	}
}

func TestRunOneJobAgentDeclaredFailureIsDeliveredVerbatimWithFailureBookkeeping(t *testing.T) {
	s, _ := newTestStore(t)
	resetDeliver(t)
	evidence := "The export subagent timed out after 30 minutes waiting on the database."
	patchRunJob(t, jobRunOutcome{true, "out", "[CRON_FAILURE]\n" + evidence, nil}, nil)
	job, ran, ok := runOneJobRun(t, s, "nightly export", "telegram")
	if !ok || deref(job.LastStatus) != "error" {
		t.Fatalf("processed=%v last_status=%q", ok, deref(job.LastStatus))
	}
	content := queuedContent(t, s, ran)
	if !strings.Contains(content, strings.TrimRight(evidence, ".")) || strings.Contains(content, "model service") {
		t.Fatalf("delivered %q", content)
	}
}

func TestRunOneJobMarkerMentionedInReportStaysSuccessful(t *testing.T) {
	s, _ := newTestStore(t)
	resetDeliver(t)
	patchRunJob(t, jobRunOutcome{true, "out", "The child documentation says [CRON_FAILURE], but this run recovered.", nil}, nil)
	job, _ := runOneJobFor(t, s, "delegate", "telegram")
	if deref(job.LastStatus) == "error" {
		t.Fatalf("quoted marker failed the run: %q", deref(job.LastError))
	}
}

func TestRunOneJobExceptionDeliversFailureAlert(t *testing.T) {
	s, _ := newTestStore(t)
	resetDeliver(t)
	patchRunJob(t, jobRunOutcome{}, &cronFailureText{text: "Gemini HTTP 503 (UNAVAILABLE)"})
	job, ok := runOneJobFor(t, s, "j3", "telegram")
	if ok {
		t.Fatal("an escaped exception reports processed")
	}
	testDeliver.Lock()
	sent := append([]string(nil), testDeliver.sent...)
	testDeliver.Unlock()
	if len(sent) != 1 {
		t.Fatalf("deliveries = %v", sent)
	}
	if deref(job.LastStatus) != "error" || !strings.Contains(deref(job.LastError), "503") {
		t.Fatalf("job = %q %q", deref(job.LastStatus), deref(job.LastError))
	}
	rows, err := openExecutionsLedger(s).List(context.Background(), &job.ID, 5, nil)
	if err != nil || len(rows) != 1 || rows[0]["status"] != "failed" {
		t.Fatalf("ledger rows = %v %v", rows, err)
	}
}

func TestRunOneJobExceptionRecordsFailureAlertDeliveryError(t *testing.T) {
	s, _ := newTestStore(t)
	resetDeliver(t)
	testDeliver.Lock()
	testDeliver.err = strPtr("telegram down")
	testDeliver.Unlock()
	t.Cleanup(func() { testDeliver.Lock(); testDeliver.err = nil; testDeliver.Unlock() })
	patchRunJob(t, jobRunOutcome{}, &cronFailureText{text: "boom"})
	job, _ := runOneJobFor(t, s, "j4", "telegram")
	if deref(job.LastDeliveryError) != "telegram down" {
		t.Fatalf("last_delivery_error = %q", deref(job.LastDeliveryError))
	}
}

type fakeCronAgent struct {
	mu          sync.Mutex
	spec        AgentOptions
	result      map[string]any
	idle        float64
	block       chan struct{}
	interrupted []string
	closed      int
}

func (f *fakeCronAgent) RunConversation(ctx context.Context, prompt, taskID string) (map[string]any, error) {
	f.mu.Lock()
	b := f.block
	f.mu.Unlock()
	if b != nil {
		<-b
	}
	return f.result, nil
}
func (f *fakeCronAgent) GetActivitySummary() map[string]any {
	return map[string]any{"seconds_since_activity": f.idle, "last_activity_desc": "tool call"}
}
func (f *fakeCronAgent) Interrupt(message string) {}
func (f *fakeCronAgent) HardInterrupt(reason, toolReason string) {
	f.mu.Lock()
	f.interrupted = append(f.interrupted, reason)
	if f.block != nil {
		close(f.block)
		f.block = nil
	}
	f.mu.Unlock()
}
func (f *fakeCronAgent) SessionID() string           { return f.spec.SessionID }
func (f *fakeCronAgent) SessionAPICalls() int        { return 1 }
func (f *fakeCronAgent) SetEndSessionOnClose(v bool) {}
func (f *fakeCronAgent) Close() error {
	f.mu.Lock()
	f.closed++
	f.mu.Unlock()
	return nil
}

// Binds the job as the canonical owner execution so run_job runs the agent in process
func ownerRun(t *testing.T, s *Store, job *Job, fake *fakeCronAgent) (context.Context, string) {
	savedResolve := preflightResolveRuntime
	preflightResolveRuntime = func(context.Context, string, string, string, string, string) (*provider.Runtime, error) {
		return &provider.Runtime{}, nil
	}
	t.Cleanup(func() { preflightResolveRuntime = savedResolve })
	t.Helper()
	db, err := state.Acquire(context.Background(), home.StatePath(s.home))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.ReleaseOrClose(db) })
	execID := "exec-" + job.ID
	ctx := gateway.BindCronExecution(context.Background(), gateway.CronOwnerExecution{
		DB: db, DBPath: home.StatePath(s.home), SessionID: "cron_owner_session", JobID: job.ID, ExecutionID: execID,
	})
	installFakeAgent(t, func(ctx context.Context, spec AgentOptions) (CronAgent, error) {
		fake.spec = spec
		return fake, nil
	})
	prevResolve := resolveRuntimeProviderSeam
	t.Cleanup(func() { resolveRuntimeProviderSeam = prevResolve })
	resolveRuntimeProviderSeam = func(context.Context, string, string, string, string, string) (*provider.Runtime, error) {
		return &provider.Runtime{}, nil
	}
	return ctx, execID
}

func TestRunJobAgentPathFramesTheRunDocument(t *testing.T) {
	s, _ := newTestStore(t)
	resetDeliver(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: strPtr("say hi"), Schedule: "every 1h", Name: strPtr("greeter"), Model: "m1"})
	fake := &fakeCronAgent{result: map[string]any{"final_response": "hello", "completed": true}}
	ctx, execID := ownerRun(t, s, job, fake)
	out, err := s.runJob(ctx, job, runJobOptions{ExecutionID: &execID})
	if err != nil || !out.Success || out.FinalResponse != "hello" {
		t.Fatalf("outcome %+v err %v", out, err)
	}
	if !strings.Contains(out.Output, promptFrame) || !strings.Contains(out.Output, responseHeading+"hello\n") {
		t.Fatalf("run document %q", out.Output)
	}
	if fake.spec.Platform != "cron" || fake.spec.SessionID != "cron_owner_session" || !fake.spec.SkipContextFiles || fake.spec.Model != "m1" {
		t.Fatalf("spec %+v", fake.spec)
	}
	if fake.closed != 1 {
		t.Fatalf("agent closed %d times", fake.closed)
	}
}

func TestRunJobRepairsMediaPathFromAgentRows(t *testing.T) {
	s, _ := newTestStore(t)
	resetDeliver(t)
	captureName := "computer_use_0123456789abcdef0123456789abcdef.png"
	canonical := `C:\Users\Alice\AppData\Local\icebath\cache\images\` + captureName
	toolResult := `{"screenshot_path": "` + strings.ReplaceAll(canonical, `\`, `\\`) + `"}`
	job := mustCreate(t, s, CreateJobOptions{Prompt: strPtr("capture"), Schedule: "every 1h", Name: strPtr("shooter"), Model: "m1"})
	fake := &fakeCronAgent{result: map[string]any{
		"final_response": "Here is the screenshot.\nMEDIA:/Users/Alice/AppData/Local/icebath/cache/images/" + captureName,
		"completed":      true,
		"messages": []map[string]any{
			{"role": "assistant", "tool_calls": []any{map[string]any{"id": "capture", "function": map[string]any{"name": "computer_use"}}}},
			{"role": "tool", "tool_call_id": "capture", "content": toolResult},
		},
	}}
	ctx, execID := ownerRun(t, s, job, fake)
	out, err := s.runJob(ctx, job, runJobOptions{ExecutionID: &execID})
	if want := "Here is the screenshot.\nMEDIA:" + canonical; err != nil || !out.Success || out.FinalResponse != want {
		t.Fatalf("outcome %+v err %v", out, err)
	}
}

func TestRunJobInactivityTimeoutInterruptsTheAgent(t *testing.T) {
	s, _ := newTestStore(t)
	resetDeliver(t)
	t.Setenv("ICEBATH_CRON_TIMEOUT", "0.05")
	prevPoll := watchdogPollSeconds
	watchdogPollSeconds = 0.01
	t.Cleanup(func() { watchdogPollSeconds = prevPoll })
	job := mustCreate(t, s, CreateJobOptions{Prompt: strPtr("work"), Schedule: "every 1h", Name: strPtr("slow"), Model: "m1"})
	fake := &fakeCronAgent{idle: 100, block: make(chan struct{}), result: map[string]any{"final_response": "late"}}
	ctx, execID := ownerRun(t, s, job, fake)
	out, err := s.runJob(ctx, job, runJobOptions{ExecutionID: &execID})
	if err != nil || out.Success || out.Error == nil || !strings.Contains(*out.Error, "TimeoutError: Cron job 'slow' idle for 100s") {
		t.Fatalf("outcome %+v err %v", out, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.interrupted) != 1 || fake.interrupted[0] != "Cron job timed out (inactivity)" {
		t.Fatalf("interrupts %v", fake.interrupted)
	}
}

func TestRunCronCleanupWithTimeoutAbandonsAWedgedCleanup(t *testing.T) {
	s, _ := newTestStore(t)
	release := make(chan struct{})
	defer close(release)
	bound := 0.05
	start := time.Now()
	if s.runCronCleanupWithTimeout("j", "agent resource teardown", &bound, func() error { <-release; return nil }) {
		t.Fatal("a wedged cleanup reported success")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cleanup bound not enforced")
	}
	zero := 0.0
	if !s.runCronCleanupWithTimeout("j", "inline", &zero, func() error { return nil }) {
		t.Fatal("inline cleanup failed")
	}
}

func TestClassifyDeliveryOutcome(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"failed", classifyDeliveryOutcome(strPtr("x"), true, false, "telegram", false, true, nil, false), "failed"},
		{"queued", classifyDeliveryOutcome(nil, true, false, "telegram", false, true, map[string]any{"a": 1}, false), "queued"},
		{"suppressed", classifyDeliveryOutcome(nil, true, false, "telegram", false, true, nil, true), "suppressed"},
		{"not configured", classifyDeliveryOutcome(nil, true, true, "origin", false, true, nil, false), "not_configured"},
		{"delivered", classifyDeliveryOutcome(nil, true, false, "telegram", false, true, nil, false), "delivered"},
		{"acked", classifyDeliveryOutcome(nil, false, false, "local", true, false, nil, false), "suppressed_acked"},
		{"local", classifyDeliveryOutcome(nil, true, false, "local", false, true, nil, false), "suppressed"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %q want %q", c.name, c.got, c.want)
		}
	}
}

func TestFailEmptyResponseAndNewlines(t *testing.T) {
	d := &runDelivery{success: true}
	failEmptyResponse(d, "  \n")
	if d.success || deref(d.err) != emptyResponseError {
		t.Fatalf("blank answer kept healthy: %+v", d)
	}
	if got := normalizeNewlines("a\r\nb\rc\n"); got != "a\nb\nc\n" {
		t.Fatalf("normalized %q", got)
	}
	if got := externalWorkerExitedReason(nil); !strings.Contains(got, "status None after adopting") {
		t.Fatalf("reason %q", got)
	}
}

func TestInactivityWatchdogLoopStopsOnDoneAndFiresOnIdle(t *testing.T) {
	stop := make(chan struct{})
	if inactivityWatchdogLoop(func() (float64, error) { return 0, nil }, 10, time.Millisecond, chanStop(stop), func() bool { return true }, nil) {
		t.Fatal("a finished run timed out")
	}
	if !inactivityWatchdogLoop(func() (float64, error) { return 50, nil }, 10, time.Millisecond, chanStop(stop), func() bool { return false }, nil) {
		t.Fatal("idle past the limit did not fire")
	}
	close(stop)
	if inactivityWatchdogLoop(func() (float64, error) { return 50, nil }, 10, time.Hour, chanStop(stop), func() bool { return false }, nil) {
		t.Fatal("a stopped watchdog fired")
	}
}

// A run scope binds a blank stateless session with the workdir, marks the cron session on enter and clears it all on exit
func TestCronRunScopeBindsAStatelessCronSessionAndClearsItOnExit(t *testing.T) {
	s, _ := newTestStore(t)
	t.Setenv("ICEBATH_CRON_SESSION", "")
	os.Unsetenv("ICEBATH_CRON_SESSION")
	workdir := t.TempDir()
	deliver := "telegram:12345"
	job := &Job{ID: "j1", Workdir: &workdir, Deliver: &deliver}
	execID := "exec-1"
	scope := s.newCronRunScope(job, "j1", &execID)
	env := scope.env
	if scope.taskID != "cron:j1:exec-1" {
		t.Fatalf("task id %q", scope.taskID)
	}
	if tools.SessionAsyncDeliverySupported(env) {
		t.Fatal("cron scope left async delivery supported")
	}
	if got := tools.ScopedSessionCwd(env); got != workdir {
		t.Fatalf("logical cwd %q", got)
	}
	for _, name := range append([]string{"ICEBATH_SESSION_PLATFORM", "ICEBATH_SESSION_CHAT_ID", "ICEBATH_SESSION_KEY"}, cronDeliveryVars...) {
		if v, had := tools.BoundSessionVar(env, name); !had || v != "" {
			t.Fatalf("%s bound %q %v, want blank", name, v, had)
		}
	}
	if _, had := tools.BoundSessionVar(env, "ICEBATH_CRON_SESSION"); had {
		t.Fatal("cron session bound before enter")
	}
	prevTargets := resolveDeliveryTargets
	resolveDeliveryTargets = func(context.Context, *Store, *Job, bool) []Target {
		return []Target{{Platform: "telegram", ChatID: "12345", ThreadID: "7"}}
	}
	t.Cleanup(func() { resolveDeliveryTargets = prevTargets })
	scope.enter(context.Background())
	if got := tools.GetSessionEnv(env, "ICEBATH_CRON_SESSION", ""); got != "1" {
		t.Fatalf("cron session %q inside the run", got)
	}
	if err := s.reloadDotenvAndPublishDeliveryTarget(context.Background(), job, scope); err != nil {
		t.Fatal(err)
	}
	if p, c := tools.GetSessionEnv(env, "ICEBATH_CRON_AUTO_DELIVER_PLATFORM", ""), tools.GetSessionEnv(env, "ICEBATH_CRON_AUTO_DELIVER_CHAT_ID", ""); p != "telegram" || c != "12345" || tools.GetSessionEnv(env, "ICEBATH_CRON_AUTO_DELIVER_THREAD_ID", "") != "7" {
		t.Fatalf("published target %q %q", p, c)
	}
	scope.exit()
	if _, had := tools.BoundSessionVar(env, "ICEBATH_CRON_SESSION"); had {
		t.Fatal("cron session still bound after exit")
	}
	for _, name := range append([]string{"ICEBATH_SESSION_PLATFORM"}, cronDeliveryVars...) {
		if v, had := tools.BoundSessionVar(env, name); !had || v != "" {
			t.Fatalf("%s after exit %q %v, want blank", name, v, had)
		}
	}
	if !tools.SessionAsyncDeliverySupported(env) {
		t.Fatal("exit left the async delivery declaration in place")
	}
	if got := tools.ScopedSessionCwd(env); got != "" {
		t.Fatalf("logical cwd %q after exit", got)
	}
	if _, ok := tools.GetSessionCwd(env, scope.taskID); ok {
		t.Fatal("task cwd kept after exit")
	}
}

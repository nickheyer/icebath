package cron

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Runner double the owner authority reads, never draining
type ownerTestRunner struct{}

func (ownerTestRunner) SessionRuntimeDescriptor() map[string]any { return nil }
func (ownerTestRunner) Draining() bool                           { return false }
func (ownerTestRunner) Running() bool                            { return true }
func (ownerTestRunner) SessionStore() *gateway.SessionStore      { return nil }
func (ownerTestRunner) AdapterForSource(context.Context, *gateway.SessionSource) gateway.PlatformAdapter {
	return nil
}
func (ownerTestRunner) MultiplexProfiles() bool { return false }

// Authority over a fresh store holding live cron session s owned by the cron principal, with its claimed fire
func claimedCronFire(t *testing.T, s *Store, requestID string) (*gateway.SessionAuthority, *pyjson.Object) {
	t.Helper()
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(s.Home(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.CreateSession(state.NewSession{ID: "s", Source: "cron"}); err != nil {
		t.Fatal(err)
	}
	epoch, err := db.BeginRuntimeEpoch(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	authority := gateway.NewSessionAuthority(ownerTestRunner{}, "p", "test", db, epoch)
	owner := state.CronPrincipalID
	authority.Sessions["s"] = gateway.NewLiveSession(&gateway.SessionSource{UserID: &owner}, "route")
	if _, err := db.AdmitSessionInput(ctx, state.SessionInput{Epoch: epoch, PrincipalID: owner, SessionID: "s",
		RequestID: requestID, Payload: pyjson.ObjectOf("text", "")}); err != nil {
		t.Fatal(err)
	}
	row, _, err := db.ClaimSessionInput(ctx, state.ClaimInput{Epoch: epoch, SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	return authority, row
}

// Frozen cron policy of a fire
func cronFirePolicy(t *testing.T, requestID string, job *pyjson.Object) *gateway.LocalSessionPolicy {
	t.Helper()
	text, err := pyjson.Dumps(pyjson.ObjectOf("extra_prompt", nil, "request_id", requestID, "cron_job", job))
	if err != nil {
		t.Fatal(err)
	}
	return &gateway.LocalSessionPolicy{RequestJSON: text}
}

func TestCronCancelBetweenClaimAndExecutionRegistrationStopsTheJob(t *testing.T) {
	s, _ := newTestStore(t)
	authority, row := claimedCronFire(t, s, "cron:job:fire")
	seen := map[string]bool{}
	prev := ownerRunJob
	ownerRunJob = func(_ *Store, _ context.Context, _ *Job, o runJobOptions) (jobRunOutcome, error) {
		seen["cancelled_at_start"] = o.Cancel.IsSet()
		reason := "cancelled"
		return jobRunOutcome{Success: false, Error: &reason}, nil
	}
	t.Cleanup(func() { ownerRunJob = prev })
	actor := &gateway.Principal{Subject: "cron-owner", ProfileID: "p",
		Capabilities: map[string]bool{"session:create": true, "session:submit": true, "session:control": true}, TransportID: "ticker"}
	ctx := home.WithOverride(context.Background(), s.Home())
	admissionID := rowField(row, "admission_id")
	got, err := gateway.CronRPC(ctx, ownerTestConnection{authority, actor}, "cancel",
		map[string]any{"session_id": "s", "admission_id": admissionID})
	if err != nil || len(got) != 1 || got["ok"] != true {
		t.Fatalf("cancel = %v, %v", got, err)
	}
	_, err = gateway.CronExecute(ctx, authority, gateway.SessionRef{ProfileID: "p", SessionID: "s"}, row,
		cronFirePolicy(t, "cron:job:fire", pyjson.ObjectOf("id", "job")))
	var named interface{ PyTypeName() string }
	if err == nil || !errors.As(err, &named) || named.PyTypeName() != "RuntimeError" {
		t.Fatalf("execute error = %v, want RuntimeError", err)
	}
	if len(seen) != 1 || !seen["cancelled_at_start"] {
		t.Fatalf("seen = %v", seen)
	}
	if n := authority.CronCancellations(); n != 0 {
		t.Fatalf("cron cancellations = %d", n)
	}
}

func TestOwnerExecutionBindsTheSchedulerCronIdentityInsideRunJob(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := home.WithOverride(context.Background(), s.Home())
	scheduled := "2026-10-08T09:00:00"
	execution, err := openExecutionsLedger(s).Create(ctx, "job", "builtin", &scheduled)
	if err != nil {
		t.Fatal(err)
	}
	executionID := execution["id"].(string)
	record, err := openExecutionsLedger(s).MarkRunning(ctx, executionID)
	if err != nil {
		t.Fatal(err)
	}
	requestID := "cron:job:" + executionID
	authority, row := claimedCronFire(t, s, requestID)
	var seenID string
	var identity *CronExecution
	prev := ownerRunJob
	ownerRunJob = func(_ *Store, runCtx context.Context, _ *Job, o runJobOptions) (jobRunOutcome, error) {
		seenID = *o.ExecutionID
		identity = CurrentCronExecution(runCtx)
		return jobRunOutcome{Success: true, Output: "doc", FinalResponse: "answer"}, nil
	}
	t.Cleanup(func() { ownerRunJob = prev })
	answer, err := gateway.CronExecute(ctx, authority, gateway.SessionRef{ProfileID: "p", SessionID: "s"}, row,
		cronFirePolicy(t, requestID, pyjson.ObjectOf("id", "job", "name", "Job")))
	if err != nil || answer != "answer" {
		t.Fatalf("execute = %q, %v", answer, err)
	}
	if seenID != executionID || seenID == rowField(row, "admission_id") {
		t.Fatalf("execution id = %q, admission %q", seenID, rowField(row, "admission_id"))
	}
	if identity == nil || identity.JobID != "job" || identity.JobName != "Job" || identity.ExecutionID != executionID {
		t.Fatalf("identity = %+v", identity)
	}
	if identity.Source != "builtin" || !pyjson.Equal(optAny(identity.ScheduledInstant), record["scheduled_instant"]) ||
		!pyjson.Equal(optAny(identity.StartedAt), record["started_at"]) {
		t.Fatalf("identity = %+v, record %v", identity, record)
	}
	if CurrentCronExecution(ctx) != nil {
		t.Fatal("identity leaked outside run_job")
	}
}

// Gateway connection double carrying an authority and its actor
type ownerTestConnection struct {
	authority *gateway.SessionAuthority
	actor     *gateway.Principal
}

func (c ownerTestConnection) CronAuthority() *gateway.SessionAuthority { return c.authority }
func (c ownerTestConnection) CronActor() *gateway.Principal            { return c.actor }

// String column of an admission row
func rowField(row *pyjson.Object, key string) string {
	v, _ := row.Get(key)
	s, _ := v.(string)
	return s
}

// Python value of an optional string
func optAny(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

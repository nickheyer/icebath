package cron

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
)

const quotaMsg = "Codex provider quota exhausted (429); retry after 123518s. Credentials are still valid."

type causedError struct {
	msg   string
	cause error
}

func (e *causedError) Error() string { return e.msg }
func (e *causedError) Unwrap() error { return e.cause }

func quotaError() *provider.AuthError {
	return &provider.AuthError{Message: quotaMsg, Provider: "openai-codex", Code: provider.CodexRateLimitedCode}
}

// Stored jobs.json row of one job as decoded JSON
func rawStoredRecord(t *testing.T, s *Store, id string) map[string]any {
	t.Helper()
	for _, row := range rawStoredRows(t, s) {
		if row["id"] == id {
			return row
		}
	}
	t.Fatalf("job %s not stored", id)
	return nil
}

func mutableClockStore(t *testing.T, cur *time.Time, zone *time.Location) *Store {
	t.Helper()
	s := newClockStore(t, nil, zone)
	s.clk.nowFn = func() time.Time { return *cur }
	return s
}

func TestHoldSecondsOnlyFromRateLimitedAuthErrorInCauseChain(t *testing.T) {
	if got, ok := quotaHoldSecondsFromFailure(&causedError{msg: quotaMsg, cause: quotaError()}); !ok || got != 123518 {
		t.Fatalf("wrapped = %v %v", got, ok)
	}
	for _, err := range []error{
		errors.New(quotaMsg),
		errors.New("HTTP 429: retry after 60s"),
		&provider.AuthError{Message: quotaMsg, Provider: "openai-codex", Code: "expired", ReloginRequired: true},
	} {
		if got, ok := quotaHoldSecondsFromFailure(err); ok {
			t.Fatalf("%v parked %v", err, got)
		}
	}
	structured := &provider.AuthError{Message: "quota", Code: provider.CodexRateLimitedCode, RetryAfter: 900 * time.Second, HasRetryAfter: true}
	if got, ok := quotaHoldSecondsFromFailure(structured); !ok || got != 900 {
		t.Fatalf("structured = %v %v", got, ok)
	}
}

func TestWeeklyCronRetriesWhenQuotaRecoversBeforeNextOccurrence(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 1, 0, 0, time.UTC)
	cur := now
	s := mutableClockStore(t, &cur, time.UTC)
	naturalNext := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("weekly digest"), Schedule: "0 12 * * 5"})
	if at, _ := ParseISO(*job.NextRunAt); !at.Time.Equal(naturalNext) {
		t.Fatalf("next_run_at = %s", *job.NextRunAt)
	}
	ok, err := s.markJobRun(context.Background(), job.ID, false, MarkOptions{Error: prompt(quotaMsg), QuotaHoldSeconds: 20 * 60 * 60, RecoverConsumedFire: true})
	if err != nil || !ok {
		t.Fatalf("markJobRun = %v %v", ok, err)
	}
	held := mustGet(t, s, job.ID)
	retryAt, _ := ParseISO(*held.NextRunAt)
	if want := now.Add(20*time.Hour + QuotaHoldSlackSeconds*time.Second); !retryAt.Time.Equal(want) || !retryAt.Time.Before(naturalNext) {
		t.Fatalf("parked at %s, want %s", *held.NextRunAt, FormatISO(want))
	}
	if !quotaHoldIsRecoveryFire(s, held, *held.NextRunAt) {
		t.Fatal("parked recovery fire not recognised")
	}
	edited := *held
	edited.Schedule = &Schedule{Kind: "cron", Expr: "0 9 * * *"}
	if quotaHoldIsRecoveryFire(s, &edited, *edited.NextRunAt) {
		t.Fatal("edited expression inherited the recovery exception")
	}
	cur = retryAt.Time.Add(time.Second)
	if !dueIDs(t, s)[job.ID] {
		t.Fatal("recovery fire not due")
	}
}

func TestRecoveryFireSkipsDenseSchedulesAndNeverReParks(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 1, 0, 0, time.UTC)
	s := newClockStore(t, &now, time.UTC)
	natural := FormatISO(time.Date(2026, 9, 18, 13, 0, 0, 0, time.UTC))
	dense := &Job{Schedule: &Schedule{Kind: "cron", Expr: "0 * * * *"}, NextRunAt: strPtr(natural)}
	if parked, err := quotaHoldPlanHold(s, dense, 57*60-QuotaHoldSlackSeconds, true); err != nil || parked {
		t.Fatalf("dense parked = %v %v", parked, err)
	}
	if *dense.NextRunAt != natural || dense.QuotaHoldUntil != nil {
		t.Fatalf("dense = %s %v", *dense.NextRunAt, dense.QuotaHoldUntil)
	}
	natural = FormatISO(now.Add(7 * 24 * time.Hour))
	heldAgain := &Job{
		Schedule:          &Schedule{Kind: "cron", Expr: "0 12 * * 5"},
		NextRunAt:         strPtr(natural),
		QuotaHoldUntil:    strPtr(FormatISO(now)),
		QuotaHoldCronExpr: strPtr("0 12 * * 5"),
	}
	if parked, err := quotaHoldPlanHold(s, heldAgain, 20*60*60, true); err != nil || parked {
		t.Fatalf("held again parked = %v %v", parked, err)
	}
	if *heldAgain.NextRunAt != natural || heldAgain.QuotaHoldUntil != nil {
		t.Fatalf("held again = %s %v", *heldAgain.NextRunAt, heldAgain.QuotaHoldUntil)
	}
}

func TestQuotaHoldParksPastWindowSurvivesStaleRearmAndClearsOnModelReach(t *testing.T) {
	t.Setenv("ICEBATH_MODEL", "test-cron-default-model")
	s := newClockStore(t, nil, time.UTC)
	resetDeliver(t)
	registerHomePlatforms(t, "slack")
	writePinConfig(t, s, "platforms:\n  slack:\n    enabled: true\n    token: xoxb-test\n")
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("portfolio triage"), Schedule: "every 30m", Deliver: prompt("slack:alerts")})
	now := time.Now().UTC()
	raiseQuota := func(context.Context, string, string, string, string, string) (*provider.Runtime, error) {
		return nil, quotaError()
	}
	prevRun, prevPreflight := resolveRuntimeProviderSeam, preflightResolveRuntime
	resolveRuntimeProviderSeam, preflightResolveRuntime = raiseQuota, raiseQuota
	t.Cleanup(func() { resolveRuntimeProviderSeam, preflightResolveRuntime = prevRun, prevPreflight })
	installFakeAgent(t, func(context.Context, AgentOptions) (CronAgent, error) { return nil, errors.New("model said no") })
	installCronOwner(t)
	ran := mustGet(t, s, job.ID)
	if _, err := runOneJob(context.Background(), s, ran, runOneJobOptions{}); err != nil {
		t.Fatal(err)
	}
	j := mustGet(t, s, job.ID)
	if deref(j.LastStatus) != "error" {
		t.Fatalf("last_status = %v", deref(j.LastStatus))
	}
	if content := queuedContent(t, s, ran); !strings.Contains(content, "The provider's usage window is closed for about") {
		t.Fatalf("delivered alert %q carries no hold notice", content)
	}
	q := openDeliveryQueue(s)
	if first, err := q.ClaimNext(context.Background()); err != nil || first == nil {
		t.Fatalf("queued alert %v %v", first, err)
	}
	if second, err := q.ClaimNext(context.Background()); err != nil || second != nil {
		t.Fatalf("a second alert was queued %v %v", second, err)
	}
	parked, _ := ParseISO(*j.NextRunAt)
	if parked.Time.Sub(now) < 123518*time.Second {
		t.Fatalf("next_run_at %s does not land past the window", *j.NextRunAt)
	}
	if deref(j.QuotaHoldUntil) != *j.NextRunAt {
		t.Fatalf("quota_hold_until = %v", deref(j.QuotaHoldUntil))
	}
	if _, leaked := rawStoredRecord(t, s, job.ID)["_quota_hold_seconds"]; leaked {
		t.Fatal("the transient hold seconds were persisted")
	}
	if _, err := s.updateJob(job.ID, map[string]any{"last_run_at": FormatISO(now.Add(-2 * time.Hour))}); err != nil {
		t.Fatal(err)
	}
	j = mustGet(t, s, job.ID)
	if s.jobIsStaleErrorRecurring(j, j.Schedule, now) {
		t.Fatal("held job read as a stale error record")
	}
	if dueIDs(t, s)[job.ID] {
		t.Fatal("held job is due inside the window")
	}
	if at, _ := ParseISO(*mustGet(t, s, job.ID).NextRunAt); !at.Time.Equal(parked.Time) {
		t.Fatal("due scan moved the parked fire")
	}
	mustMark(t, s, job.ID, false, MarkOptions{Error: prompt("RuntimeError: model said no")})
	j = mustGet(t, s, job.ID)
	if j.QuotaHoldUntil != nil {
		t.Fatal("model reach left the hold marker")
	}
	if at, _ := ParseISO(*j.NextRunAt); at.Time.Sub(now) >= time.Hour {
		t.Fatalf("next_run_at %s not back on cadence", *j.NextRunAt)
	}
	mustMark(t, s, job.ID, false, MarkOptions{Error: prompt(quotaMsg), QuotaHoldSeconds: 123518})
	if mustGet(t, s, job.ID).QuotaHoldUntil == nil {
		t.Fatal("second hold not stamped")
	}
	j, err := s.updateJob(job.ID, map[string]any{"schedule": "every 15m"})
	if err != nil {
		t.Fatal(err)
	}
	if j.QuotaHoldUntil != nil {
		t.Fatal("schedule edit kept the stale hold marker")
	}
	if at, _ := ParseISO(*j.NextRunAt); at.Time.Sub(now) >= time.Hour {
		t.Fatalf("edited next_run_at %s", *j.NextRunAt)
	}
}

func TestQuotaHoldNoticeNamesTheWindow(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 1, 0, 0, time.UTC)
	s := newClockStore(t, &now, time.UTC)
	job := &Job{Schedule: &Schedule{Kind: "interval", Minutes: 30, HasMinutes: true}}
	want := "\nThe provider's usage window is closed for about 2.0h. This job is held through 2026-09-18 14:02 UTC " +
		"and resumes at the first safe opportunity afterwards; no further alerts are sent while the provider is unavailable."
	if got := quotaHoldNotice(s, job, 7200); got != want {
		t.Fatalf("notice = %q", got)
	}
	if got := quotaHoldNotice(s, &Job{Schedule: &Schedule{Kind: "once"}}, 7200); got != "" {
		t.Fatalf("one shot notice = %q", got)
	}
	if got := quotaHoldNotice(s, job, 0); got != "" {
		t.Fatalf("no hold notice = %q", got)
	}
}

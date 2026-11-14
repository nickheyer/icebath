package monitoring

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestExecutionProjectionIsOpaqueBoundedAndContentFree(t *testing.T) {
	outcome := "failed"
	event := ProjectExecutionEvent(map[string]any{
		"id":          "execution-private-id",
		"job_id":      "Payroll for alice@example.com and token top-secret-token",
		"source":      "builtin",
		"status":      "failed",
		"claimed_at":  "2026-07-24T12:00:00+00:00",
		"started_at":  "2026-07-24T12:00:01+00:00",
		"finished_at": "2026-07-24T12:00:03.250000+00:00",
		"error":       "Bearer top-secret-token rejected for alice@example.com",
	}, &outcome).ToDict()
	get := func(k string) any { v, _ := event.Get(k); return v }
	if get("event") != "cron_execution" || get("status") != "failed" || get("delivery_outcome") != "failed" ||
		get("error_class") != "auth_failed" || get("duration_ms") != int64(2250) {
		t.Fatalf("event %v", event.Vals)
	}
	key := get("job_key").(string)
	if !strings.HasPrefix(key, "sha256:") || len(key) != len("sha256:")+24 {
		t.Fatalf("job key %q", key)
	}
	if _, ok := event.Get("job_id"); ok {
		t.Fatal("job_id exported")
	}
	if _, ok := event.Get("error"); ok {
		t.Fatal("error exported")
	}
	raw, _ := pyjson.Dumps(event)
	if strings.Contains(raw, "alice@example.com") || strings.Contains(raw, "top-secret-token") {
		t.Fatalf("content leaked %s", raw)
	}
}

func TestErrorClassificationAvoidsAuthSubstringFalsePositives(t *testing.T) {
	for _, msg := range []string{"oauth refresh failed", "tokenizer crashed", "HTTP 4015"} {
		if got := ClassifyCronError(msg); got != "unknown" {
			t.Errorf("%q classified %q", msg, got)
		}
	}
	if got := ClassifyCronError("HTTP 429 quota"); got != "rate_limited" {
		t.Errorf("rate limit classified %q", got)
	}
}

type fakeSink struct{ calls []string }

func (f *fakeSink) Emit(event any) {
	f.calls = append(f.calls, "emit "+event.(Event).ToDict().Vals["status"].(string))
}

func (f *fakeSink) Flush(timeout float64) {
	f.calls = append(f.calls, "flush "+time.Duration(timeout*float64(time.Second)).String())
	panic(errors.New("collector unavailable"))
}

func TestTerminalExecutionEmissionFlushesAndFailuresAreFailOpen(t *testing.T) {
	sink := &fakeSink{}
	prev := cronEmitter
	cronEmitter = func() eventSink { return sink }
	defer func() { cronEmitter = prev }()
	EmitExecutionState(map[string]any{"job_id": "private", "source": "builtin", "status": "completed"}, nil)
	if strings.Join(sink.calls, ",") != "emit completed,flush 1s" {
		t.Fatalf("calls %v", sink.calls)
	}
}

func TestMonitoringReportsStopsAndMissesAsThemselves(t *testing.T) {
	for _, status := range []string{"cancelled", "missed"} {
		if got := ProjectExecutionEvent(map[string]any{"status": status, "job_id": "j"}, nil).Status; got != status {
			t.Errorf("%s projected %s", status, got)
		}
	}
}

func TestCronHealthSnapshotGroupsFailOpenAndCountOverdue(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	age := 3.5
	neg := -2.0
	SetCronHealthSources(CronHealthSources{
		Now: func() time.Time { return now },
		LoadJobs: func(_ context.Context) ([]CronJobHealth, error) {
			grace := func() int { return 120 }
			return []CronJobHealth{
				{Enabled: true, NextRunAt: "2026-07-24T11:00:00", HasSchedule: true, GraceSeconds: grace},
				{Enabled: true, NextRunAt: "2026-07-24T11:59:00+00:00", HasSchedule: true, GraceSeconds: grace},
				{Enabled: true, NextRunAt: "2026-07-24T10:00:00+00:00", HasSchedule: false, GraceSeconds: grace},
				{Enabled: false, NextRunAt: "2026-07-24T10:00:00+00:00", HasSchedule: true, GraceSeconds: grace},
			}, nil
		},
		TickerHeartbeatAge:     func() (*float64, error) { return &age, nil },
		TickerSuccessAge:       func() (*float64, error) { return &neg, nil },
		CatchUpOccurrenceCount: func() (int, error) { return 0, errors.New("unreadable") },
		RunningJobCount:        func() (int, error) { panic("scheduler gone") },
		DegradedSkippedRuns:    func() ([]int, error) { return []int{2, 3}, nil },
	})
	defer SetCronHealthSources(CronHealthSources{})
	got := map[string]any{}
	var order []string
	for _, m := range BuildCronHealthSnapshot(context.Background()).Metrics {
		got[m.Name] = m.Value
		order = append(order, m.Name)
	}
	want := map[string]any{"hermes.cron.scheduler.heartbeat_age_seconds": 3.5, "hermes.cron.scheduler.last_success_age_seconds": 0.0,
		"hermes.cron.jobs.enabled": 3, "hermes.cron.jobs.overdue": 1, "hermes.cron.store.writable": 0, "hermes.cron.store.skipped_runs": 5}
	if len(got) != len(want) {
		t.Fatalf("metrics %v", order)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v want %v", k, got[k], v)
		}
	}
}

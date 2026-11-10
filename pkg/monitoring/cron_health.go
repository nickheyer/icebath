package monitoring

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	knownStatuses         = map[string]bool{"claimed": true, "running": true, "completed": true, "failed": true, "unknown": true, "cancelled": true, "missed": true}
	knownSources          = map[string]bool{"builtin": true, "direct": true, "external": true}
	knownDeliveryOutcomes = map[string]bool{"queued": true, "delivered": true, "failed": true, "suppressed": true, "suppressed_acked": true, "not_configured": true}
	terminalStatuses      = map[string]bool{"completed": true, "failed": true, "unknown": true, "cancelled": true, "missed": true}
)

// CronHealthSnapshot holds the cron gauges and the always empty event list of one snapshot
type CronHealthSnapshot struct {
	Metrics []GatewayMetric
	Events  []CronExecutionEvent
}

// CronJobHealth is the view of one stored job the overdue gauge reads
type CronJobHealth struct {
	Enabled      bool
	NextRunAt    any
	HasSchedule  bool
	GraceSeconds func() int
}

// CronHealthSources are the cron readers pkg/cron registers in init, each fail open on its own
type CronHealthSources struct {
	Now                    func() time.Time
	LoadJobs               func(ctx context.Context) ([]CronJobHealth, error)
	TickerHeartbeatAge     func() (*float64, error)
	TickerSuccessAge       func() (*float64, error)
	CatchUpOccurrenceCount func() (int, error)
	RunningJobCount        func() (int, error)
	DegradedSkippedRuns    func() ([]int, error)
}

var (
	cronSourcesMu sync.Mutex
	cronSources   CronHealthSources
)

// Installs the cron readers the health snapshot projects
func SetCronHealthSources(s CronHealthSources) {
	cronSourcesMu.Lock()
	defer cronSourcesMu.Unlock()
	cronSources = s
}

// Registered cron readers
func currentCronSources() CronHealthSources {
	cronSourcesMu.Lock()
	defer cronSourcesMu.Unlock()
	return cronSources
}

var authRE = mustPyRe(`\b(?:authentication|authenticated|authenticate|authorization|authorized|authorize|unauthorized|forbidden|bearer|401|403)\b` +
	`|\b(?:access|api|refresh) token\b`)

// Python re pattern compiled with CPython matching semantics
func mustPyRe(pattern string) interface{ MatchString(string) (bool, error) } {
	re, err := pyre.Compile(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

type cronErrorRule struct {
	match func(string) bool
	label string
}

var cronErrorRules = []cronErrorRule{
	{func(s string) bool { ok, _ := authRE.MatchString(s); return ok }, "auth_failed"},
	{containsAny("rate limit", "429", "quota"), "rate_limited"},
	{containsAny("timeout", "timed out"), "timeout"},
	{containsAny("network", "connection", "dns", "socket", "unreachable"), "network_error"},
	{containsAny("dispatch", "executor"), "dispatch_failed"},
	{containsAny("interrupt", "owner exited", "restarted"), "interrupted"},
	{containsAny("empty response"), "empty_response"},
	{containsAny("config", "missing", "invalid"), "invalid_config"},
}

// First matching error class of the lowered error text, auth by word boundary, unknown when none match
func ClassifyCronError(raw any) string {
	if !pyrepr.Truthy(raw) {
		raw = ""
	}
	text := pystr.Lower(pyrepr.Str(raw))
	for _, rule := range cronErrorRules {
		if rule.match(text) {
			return rule.label
		}
	}
	return "unknown"
}

type parsedTime struct {
	t     time.Time
	aware bool
}

// ISO timestamp of a truthy value, absent when empty or unparsable
func parseTime(raw any) (parsedTime, bool) {
	if !pyrepr.Truthy(raw) {
		return parsedTime{}, false
	}
	t, aware, err := pydatetime.ParseISOFormat(pyrepr.Str(raw))
	if err != nil {
		return parsedTime{}, false
	}
	return parsedTime{t, aware}, true
}

// Non negative whole milliseconds from start or claim to finish, absent when a bound is missing or awareness differs
func durationMS(record map[string]any) *int64 {
	start, ok := parseTime(record["started_at"])
	if !ok {
		start, ok = parseTime(record["claimed_at"])
	}
	finish, fok := parseTime(record["finished_at"])
	if !ok || !fok || start.aware != finish.aware {
		return nil
	}
	us := finish.t.Sub(start.t).Microseconds()
	ms := int64(math.Trunc(float64(us) / 1e6 * 1000))
	ms = max(0, ms)
	return &ms
}

// Lowered string of a truthy field or the fallback
func loweredOr(v any, fallback string) string {
	if !pyrepr.Truthy(v) {
		return fallback
	}
	return pystr.Lower(pyrepr.Str(v))
}

// Content free event of a ledger record with opaque job key and bucketed status, source and outcome
func ProjectExecutionEvent(record map[string]any, deliveryOutcome *string) CronExecutionEvent {
	status := loweredOr(record["status"], "unknown")
	source := loweredOr(record["source"], "unknown")
	ev := NewCronExecutionEvent("unknown", safeInstanceID(record["job_id"]))
	if knownStatuses[status] {
		ev.Status = status
	}
	if knownSources[source] || source == "unknown" {
		ev.Source = source
	} else {
		ev.Source = "external"
	}
	ev.DurationMS = durationMS(record)
	if deliveryOutcome != nil {
		if outcome := pystr.Lower(*deliveryOutcome); knownDeliveryOutcomes[outcome] {
			ev.DeliveryOutcome = &outcome
		}
	}
	if status == "failed" || status == "unknown" {
		class := ClassifyCronError(record["error"])
		ev.ErrorClass = &class
	}
	return ev
}

type eventSink interface {
	Emit(event any)
	Flush(timeout float64)
}

var cronEmitter = func() eventSink { return GetEmitter() }

// Emits the lifecycle event of a record, flushing terminal states for up to a second, never failing the caller
func EmitExecutionState(record map[string]any, deliveryOutcome *string) {
	if len(record) == 0 {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("cron execution telemetry emit failed: %v", r))
		}
	}()
	event := ProjectExecutionEvent(record, deliveryOutcome)
	target := cronEmitter()
	target.Emit(event)
	if terminalStatuses[event.Status] {
		target.Flush(1.0)
	}
}

// Enabled job past its scheduled run by more than its grace window
func isOverdue(job CronJobHealth, now time.Time) bool {
	if !job.Enabled {
		return false
	}
	next, ok := parseTime(job.NextRunAt)
	if !ok || !job.HasSchedule {
		return false
	}
	nextRun := next.t
	if !next.aware {
		nextRun = time.Date(nextRun.Year(), nextRun.Month(), nextRun.Day(), nextRun.Hour(), nextRun.Minute(),
			nextRun.Second(), nextRun.Nanosecond(), now.Location())
	}
	return float64(now.Sub(nextRun).Microseconds())/1e6 > float64(job.GraceSeconds())
}

// Enabled and overdue job gauges
func jobMetrics(ctx context.Context, src CronHealthSources, metrics *[]GatewayMetric) error {
	jobs, err := src.LoadJobs(ctx)
	if err != nil {
		return err
	}
	var enabled []CronJobHealth
	for _, j := range jobs {
		if j.Enabled {
			enabled = append(enabled, j)
		}
	}
	*metrics = append(*metrics, GatewayMetric{"hermes.cron.jobs.enabled", len(enabled), map[string]string{}})
	overdue := 0
	for _, j := range enabled {
		if isOverdue(j, src.Now()) {
			overdue++
		}
	}
	*metrics = append(*metrics, GatewayMetric{"hermes.cron.jobs.overdue", overdue, map[string]string{}})
	return nil
}

// Age gauge clamped at zero, omitted when the reader has no value
func freshnessMetric(name string, reader func(CronHealthSources) (*float64, error)) func(context.Context, CronHealthSources, *[]GatewayMetric) error {
	return func(_ context.Context, src CronHealthSources, metrics *[]GatewayMetric) error {
		v, err := reader(src)
		if err != nil {
			return err
		}
		if v != nil {
			*metrics = append(*metrics, GatewayMetric{name, math.Max(0, *v), map[string]string{}})
		}
		return nil
	}
}

// Gauge carrying the reader value as is
func singleMetric(name string, reader func(CronHealthSources) (any, error)) func(context.Context, CronHealthSources, *[]GatewayMetric) error {
	return func(_ context.Context, src CronHealthSources, metrics *[]GatewayMetric) error {
		v, err := reader(src)
		if err != nil {
			return err
		}
		*metrics = append(*metrics, GatewayMetric{name, v, map[string]string{}})
		return nil
	}
}

type metricGroup struct {
	build   func(context.Context, CronHealthSources, *[]GatewayMetric) error
	failure string
}

// Each group fails open on its own so one unavailable source never hides the rest
var metricGroups = []metricGroup{
	{freshnessMetric("hermes.cron.scheduler.heartbeat_age_seconds", func(s CronHealthSources) (*float64, error) { return s.TickerHeartbeatAge() }), "cron freshness metric unavailable"},
	{freshnessMetric("hermes.cron.scheduler.last_success_age_seconds", func(s CronHealthSources) (*float64, error) { return s.TickerSuccessAge() }), "cron freshness metric unavailable"},
	{singleMetric("hermes.cron.scheduler.catch_up_occurrences", func(s CronHealthSources) (any, error) { return s.CatchUpOccurrenceCount() }), "cron catch-up metric unavailable"},
	{jobMetrics, "cron job metrics unavailable"},
	{singleMetric("hermes.cron.jobs.running", func(s CronHealthSources) (any, error) { return s.RunningJobCount() }), "cron running-job metric unavailable"},
	{singleMetric("hermes.cron.store.writable", func(s CronHealthSources) (any, error) {
		runs, err := s.DegradedSkippedRuns()
		if err != nil {
			return nil, err
		}
		if len(runs) == 0 {
			return 1, nil
		}
		return 0, nil
	}), "cron store metric unavailable"},
	{singleMetric("hermes.cron.store.skipped_runs", func(s CronHealthSources) (any, error) {
		runs, err := s.DegradedSkippedRuns()
		total := 0
		for _, r := range runs {
			total += r
		}
		return total, err
	}), "cron store metric unavailable"},
}

// Runs one group, turning an error or panic into a debug log line
func runMetricGroup(ctx context.Context, g metricGroup, src CronHealthSources, metrics *[]GatewayMetric) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("%s: %v", g.failure, r))
		}
	}()
	if err := g.build(ctx, src, metrics); err != nil {
		slog.Debug(fmt.Sprintf("%s: %v", g.failure, err))
	}
}

// Scheduler freshness, job, running and store gauges with no events
func BuildCronHealthSnapshot(ctx context.Context) CronHealthSnapshot {
	src := currentCronSources()
	metrics := []GatewayMetric{}
	for _, g := range metricGroups {
		runMetricGroup(ctx, g, src, &metrics)
	}
	return CronHealthSnapshot{Metrics: metrics, Events: []CronExecutionEvent{}}
}

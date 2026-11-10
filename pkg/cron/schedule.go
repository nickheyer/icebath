package cron

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	minGraceSeconds              = 120
	maxGraceSeconds              = 7200
	lateDispatchToleranceSeconds = 300
	telemetryRecentHistory       = 20
	// StaleCronMatch means the stored instant is an occurrence of the current expression
	StaleCronMatch = "match"
	// StaleCronTimezoneMigration means only the stored offset representation changed
	StaleCronTimezoneMigration = "timezone_migration"
	// StaleCronExprEdit means a hand edit changed the expression under a stored instant
	StaleCronExprEdit = "expr_edit"
)

var (
	durationMultipliers = map[byte]int{'m': 1, 'h': 60, 'd': 1440}
	durationRe          = regexp2.MustCompile(`^(\d*)\s*(m|min|mins|minute|minutes|h|hr|hrs|hour|hours|d|day|days)$`, regexp2.None)
	clockTimeRe         = regexp2.MustCompile(`^(\d{1,2})(?::(\d{2}))?(am|pm)?$`, regexp2.None)
	cronFieldRe         = regexp2.MustCompile(`^[A-Za-z\d\*\-,/]+$`, regexp2.None)
	isoDatePrefixRe     = regexp2.MustCompile(`^\d{4}-\d{2}-\d{2}`, regexp2.None)
	weekdayToCronDow    = map[string]string{
		"sunday": "0", "sun": "0", "monday": "1", "mon": "1",
		"tuesday": "2", "tue": "2", "tues": "2",
		"wednesday": "3", "wed": "3", "weds": "3",
		"thursday": "4", "thu": "4", "thur": "4", "thurs": "4",
		"friday": "5", "fri": "5", "saturday": "6", "sat": "6",
	}
	dayspecToCronDow = map[string]string{
		"day": "*", "daily": "*", "everyday": "*",
		"weekday": "1-5", "weekdays": "1-5", "weekend": "0,6", "weekends": "0,6",
	}
	cadenceMu    sync.Mutex
	cadenceCache = map[string]*float64{}
	telemetryMu  sync.Mutex
	// Counters and bounded histories of the probe visible fire path signals
	persistedErrorRecoveries        int
	persistedErrorRecoveriesRecent  []map[string]any
	timezoneMigrationCatchups       int
	timezoneMigrationCatchupsRecent []map[string]any
	errDurationOverflow             = errors.New("Python int too large to convert to C int")
)

func reMatch(re *regexp2.Regexp, s string) *regexp2.Match {
	m, err := re.FindStringMatch(s)
	if err != nil {
		return nil
	}
	return m
}

// Minutes in a duration such as 30m, 2h, 1d or a bare unit meaning one
func parseDuration(text string) (int, error) {
	s := pystr.Lower(pystr.Strip(text))
	m := reMatch(durationRe, s)
	if m == nil {
		return 0, fmt.Errorf("Invalid duration: '%s'. Use format like '30m', '2h', '1d', "+
			"or a bare unit like 'hour' (defaults to 1).", s)
	}
	value := int64(1)
	if digits := m.GroupByNumber(1).String(); digits != "" {
		n, ok := pyint.Parse(digits)
		if !ok || !n.IsInt64() || n.Int64() > math.MaxInt64/1440 {
			return 0, errDurationOverflow
		}
		value = n.Int64()
	}
	return int(value) * durationMultipliers[m.GroupByNumber(2).String()[0]], nil
}

// Hour and minute of 9am, half past forms, 24h clock times, a bare hour, noon or midnight
func parseClockTime(text string) (int, int, bool) {
	t := strings.ReplaceAll(pystr.Lower(pystr.Strip(text)), " ", "")
	switch t {
	case "":
		return 0, 0, false
	case "noon", "midday":
		return 12, 0, true
	case "midnight":
		return 0, 0, true
	}
	m := reMatch(clockTimeRe, t)
	if m == nil {
		return 0, 0, false
	}
	hourN, _ := pyint.Parse(m.GroupByNumber(1).String())
	hour := int(hourN.Int64())
	minute := 0
	if g := m.GroupByNumber(2).String(); g != "" {
		n, _ := pyint.Parse(g)
		minute = int(n.Int64())
	}
	if meridiem := m.GroupByNumber(3).String(); meridiem != "" {
		if hour < 1 || hour > 12 {
			return 0, 0, false
		}
		hour = hour % 12
		if meridiem == "pm" {
			hour += 12
		}
	}
	if hour > 23 || minute > 59 {
		return 0, 0, false
	}
	return hour, minute, true
}

// Five field cron expression for a day spec plus clock time phrase, false to fall back to intervals
func naturalEveryToCron(rest string) (string, bool) {
	tokens := pystr.SplitWhitespace(strings.ReplaceAll(pystr.Lower(rest), ",", " "), -1)
	if len(tokens) == 0 {
		return "", false
	}
	dow, ok := dayspecToCronDow[tokens[0]]
	idx := 1
	if !ok {
		var days []string
		idx = len(tokens)
		for i, tok := range tokens {
			if tok == "and" {
				continue
			}
			mapped, known := weekdayToCronDow[tok]
			if !known {
				idx = i
				break
			}
			if !containsString(days, mapped) {
				days = append(days, mapped)
			}
		}
		if len(days) == 0 {
			return "", false
		}
		dow = strings.Join(days, ",")
	}
	timeTokens := tokens[idx:]
	if len(timeTokens) > 0 && timeTokens[0] == "at" {
		timeTokens = timeTokens[1:]
	}
	if len(timeTokens) == 0 {
		return "", false
	}
	hour, minute, ok := parseClockTime(strings.Join(timeTokens, " "))
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%d %d * * %s", minute, hour, dow), true
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Validated cron schedule record for expr shown as display
func cronSchedule(expr, display, invalidLabel string) (*Schedule, error) {
	if _, err := ParseCron(expr); err != nil {
		return nil, fmt.Errorf("Invalid %s '%s': %v", invalidLabel, display, err)
	}
	return &Schedule{Kind: "cron", Expr: expr, Display: display}, nil
}

func intervalSchedule(minutes int) *Schedule {
	return &Schedule{Kind: "interval", Minutes: minutes, HasMinutes: true, Display: fmt.Sprintf("every %dm", minutes)}
}

// Schedule record for an interval, natural weekday phrase, cron expression, ISO instant or in duration
func (s *Store) parseSchedule(schedule string) (*Schedule, error) {
	schedule = pystr.Strip(schedule)
	original := schedule
	lower := pystr.Lower(schedule)
	isEvery := strings.HasPrefix(lower, "every ")
	rest := lower
	if isEvery {
		rest = pystr.Strip(schedule[6:])
	}
	if expr, ok := naturalEveryToCron(rest); ok {
		return cronSchedule(expr, original, "schedule")
	}
	if isEvery {
		minutes, err := parseDuration(rest)
		if err != nil {
			return nil, err
		}
		return intervalSchedule(minutes), nil
	}
	parts := pystr.SplitWhitespace(schedule, -1)
	if len(parts) >= 5 {
		all := true
		for _, p := range parts[:5] {
			if reMatch(cronFieldRe, p) == nil {
				all = false
				break
			}
		}
		if all {
			return cronSchedule(schedule, schedule, "cron expression")
		}
	}
	if strings.Contains(schedule, "T") || reMatch(isoDatePrefixRe, schedule) != nil {
		v, err := ParseISO(schedule)
		if err != nil {
			return nil, fmt.Errorf("Invalid timestamp '%s': %v", schedule, err)
		}
		dt := v.Time
		if !v.Aware {
			dt, _ = wallInZone(v.Time, s.clk.zone())
		}
		return &Schedule{Kind: "once", RunAt: FormatISO(dt), Display: "once at " + dt.Format("2006-01-02 15:04")}, nil
	}
	if strings.HasPrefix(lower, "in ") {
		durationStr := pystr.Strip(schedule[3:])
		minutes, err := parseDuration(durationStr)
		if err != nil {
			return nil, fmt.Errorf("Invalid duration '%s' after 'in '. Use e.g. 'in 30m', 'in 2h'.", durationStr)
		}
		now := s.clk.now()
		runAt := now.Add(time.Duration(minutes) * time.Minute).In(now.Location())
		return &Schedule{Kind: "once", RunAt: FormatISO(runAt), Display: "once in " + durationStr}, nil
	}
	if minutes, err := parseDuration(schedule); err == nil {
		return intervalSchedule(minutes), nil
	}
	return nil, fmt.Errorf("Invalid schedule '%s'. Use:\n"+
		"  - Interval: '30m', 'every 30m', 'every 2h' (recurring)\n"+
		"  - One-shot delay: 'in 30m', 'in 2h' (fires once)\n"+
		"  - Weekly/daily: 'every monday 9am', 'weekdays at 9am' (recurring)\n"+
		"  - Cron: '0 9 * * *' (cron expression)\n"+
		"  - Timestamp: '2026-02-03T14:00:00' (one-shot at time)", original)
}

func instantAfter(left, right time.Time) bool { return left.After(right) }

func instantAtOrBefore(left, right time.Time) bool { return !left.After(right) }

func instantBefore(left, right time.Time) bool { return left.Before(right) }

// One shot run_at still inside the grace window and never run, else nil
func (s *Store) recoverableOneshotRunAt(sched *Schedule, now time.Time, lastRunAt *string, zone *time.Location) *string {
	if sched == nil || sched.Kind != "once" || deref(lastRunAt) != "" || sched.RunAt == "" {
		return nil
	}
	runAt := sched.RunAt
	t, ok := s.clk.oneshotInstant(runAt, zone)
	if ok && elapsedSeconds(now, t) <= oneshotGraceSeconds {
		return &runAt
	}
	return nil
}

// Raw grace_minutes value a record carries when it did not decode to a whole number
func rawGraceMinutes(job *Job) any {
	if job == nil || job.Extra == nil {
		return nil
	}
	raw, ok := job.Extra["grace_minutes"]
	if !ok {
		return nil
	}
	v, err := pyjson.LoadsBytes(raw)
	if err != nil {
		return string(raw)
	}
	return v
}

// Catch up window of half the cadence clamped to two minutes and two hours unless overridden per job
func (s *Store) computeGraceSeconds(sched *Schedule, grace *int, raw any) int {
	if override := s.knobs.graceSecondsOverride(grace, raw); override != nil {
		return maxInt(*override, minGraceSeconds)
	}
	period, ok := s.scheduleCadenceSeconds(sched)
	if !ok || period == 0 {
		return minGraceSeconds
	}
	return maxInt(minGraceSeconds, minInt(int(math.Floor(period))/2, maxGraceSeconds))
}

// Grace window of a job from its schedule and override
func (s *Store) jobGraceSeconds(job *Job) int {
	return s.computeGraceSeconds(job.scheduleOrEmpty(), job.GraceMinutes, rawGraceMinutes(job))
}

// on_time within ticker slack, late within the grace window, catch_up beyond it
func classifyDispatchLateness(lateness float64, grace int) string {
	if lateness > float64(grace) {
		return "catch_up"
	}
	if lateness > lateDispatchToleranceSeconds {
		return "late"
	}
	return "on_time"
}

// Recurring job wedged in a persisted error for longer than a cadence plus grace with no live run
func (s *Store) jobIsStaleErrorRecurring(job *Job, sched *Schedule, now time.Time) bool {
	if deref(job.LastStatus) != "error" {
		return false
	}
	if quotaHoldActive(s, job, now) {
		return false
	}
	if s.jobRunningInThisProcess(job.ID) {
		return false
	}
	if claimIsLive(s, job.FireClaim, now, FireClaimTTLSeconds) {
		return false
	}
	if deref(job.LastRunAt) == "" {
		return false
	}
	lastRun, ok := s.clk.parseAware(*job.LastRunAt, nil)
	if !ok {
		return false
	}
	age := elapsedSeconds(now, lastRun)
	if age < 0 {
		return false
	}
	grace := float64(s.computeGraceSeconds(sched, nil, nil))
	cadence, ok := s.scheduleCadenceSeconds(sched)
	if !ok {
		cadence = grace
	}
	return age > cadence+grace
}

// Approximate period of a schedule in seconds, false when unknown, cron gaps cached per expression
func (s *Store) scheduleCadenceSeconds(sched *Schedule) (float64, bool) {
	if sched == nil {
		return 0, false
	}
	switch sched.Kind {
	case "interval":
		if sched.Minutes == 0 {
			return 0, false
		}
		return float64(sched.Minutes) * 60, true
	case "cron":
	default:
		return 0, false
	}
	expr := sched.Expr
	if expr == "" {
		return 0, false
	}
	cadenceMu.Lock()
	cached, hit := cadenceCache[expr]
	cadenceMu.Unlock()
	if hit {
		if cached == nil {
			return 0, false
		}
		return *cached, true
	}
	var result *float64
	if e, err := ParseCron(expr); err == nil {
		now := s.clk.now()
		zone := now.Location()
		if w1, err := e.Next(wallOf(now)); err == nil {
			if w2, err := e.Next(w1); err == nil {
				first, _ := wallInZone(w1, zone)
				second, _ := wallInZone(w2, zone)
				if gap := second.Sub(first).Seconds(); gap > 0 {
					result = &gap
				}
			}
		}
	}
	cadenceMu.Lock()
	if len(cadenceCache) >= 256 {
		cadenceCache = map[string]*float64{}
	}
	cadenceCache[expr] = result
	cadenceMu.Unlock()
	if result == nil {
		return 0, false
	}
	return *result, true
}

// Records entry in the bounded recent list and appends it to a jsonl file in the cron dir
func (s *Store) appendTelemetryRecord(filename string, entry *pyjson.Object, recent *[]map[string]any) {
	m := map[string]any{}
	for _, k := range entry.Keys {
		m[k] = entry.Vals[k]
	}
	*recent = append(*recent, m)
	if len(*recent) > telemetryRecentHistory {
		*recent = (*recent)[len(*recent)-telemetryRecentHistory:]
	}
	path := filepath.Join(s.cronDir, filename)
	if err := ensureCronDir(filepath.Dir(path)); err != nil {
		s.log.Debug(fmt.Sprintf("Could not append %s record: %v", filename, err))
		return
	}
	text, err := pyjson.Dumps(entry)
	if err == nil {
		var f *os.File
		f, err = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
		if err == nil {
			_, err = f.WriteString(text + "\n")
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
	}
	if err != nil {
		s.log.Debug(fmt.Sprintf("Could not append %s record: %v", filename, err))
	}
}

func nameOrID(job *Job) any {
	if job.Name != "" {
		return job.Name
	}
	return job.ID
}

// Counts one stale error re-arm and persists its record
func (s *Store) recordPersistedErrorRecovery(job *Job, previousNextRun string) {
	entry := pyjson.NewObject()
	entry.Set("job_id", job.ID)
	entry.Set("name", nameOrID(job))
	entry.Set("previous_next_run_at", previousNextRun)
	entry.Set("rearmed_at", s.clk.nowISO())
	telemetryMu.Lock()
	defer telemetryMu.Unlock()
	persistedErrorRecoveries++
	s.appendTelemetryRecord("persisted_error_recoveries.jsonl", entry, &persistedErrorRecoveriesRecent)
}

// PersistedErrorRecoveryStats is the probe snapshot of stale error re-arms
func PersistedErrorRecoveryStats() map[string]any {
	telemetryMu.Lock()
	defer telemetryMu.Unlock()
	return map[string]any{"persisted_error_recoveries": persistedErrorRecoveries,
		"recent": append([]map[string]any{}, persistedErrorRecoveriesRecent...)}
}

// Whether an instant is an occurrence of the schedule current expression, anything uncheckable matching
func cronNextRunMatchesExpr(sched *Schedule, next time.Time) bool {
	if sched == nil || sched.Kind != "cron" || sched.Expr == "" {
		return true
	}
	e, err := ParseCron(sched.Expr)
	if err != nil {
		return true
	}
	prev, err := e.Prev(wallOf(next).Add(time.Second))
	if err != nil {
		return true
	}
	return math.Abs(prev.Sub(wallOf(next)).Seconds()) < 1
}

// Why a stored next_run_at misses the cron lattice, an expression edit or an offset migration
func classifyStaleCronNextRun(sched *Schedule, raw, next time.Time) string {
	if cronNextRunMatchesExpr(sched, next) {
		return StaleCronMatch
	}
	if !wallOf(raw).Equal(wallOf(next)) && cronNextRunMatchesExpr(sched, raw) {
		return StaleCronTimezoneMigration
	}
	return StaleCronExprEdit
}

// Counts one offset migration catch up fire and persists its record
func (s *Store) recordTimezoneMigrationCatchup(job *Job, raw, next time.Time) {
	entry := pyjson.NewObject()
	entry.Set("job_id", job.ID)
	entry.Set("name", nameOrID(job))
	var expr any
	if job.Schedule != nil && (job.Schedule.Expr != "" || job.Schedule.Kind == "cron") {
		expr = job.Schedule.Expr
	}
	entry.Set("expr", expr)
	entry.Set("stored_next_run_at", FormatISO(raw))
	entry.Set("normalized_next_run_at", FormatISO(next))
	entry.Set("fired_at", s.clk.nowISO())
	telemetryMu.Lock()
	defer telemetryMu.Unlock()
	timezoneMigrationCatchups++
	s.appendTelemetryRecord("timezone_migration_catchups.jsonl", entry, &timezoneMigrationCatchupsRecent)
}

// TimezoneMigrationCatchupStats is the probe snapshot of offset migration catch up fires
func TimezoneMigrationCatchupStats() map[string]any {
	telemetryMu.Lock()
	defer telemetryMu.Unlock()
	return map[string]any{"timezone_migration_catchups": timezoneMigrationCatchups,
		"recent": append([]map[string]any{}, timezoneMigrationCatchupsRecent...)}
}

// Next run computation every caller reaches, the compute_next_run module attribute tests patch
var computeNextRunFn = (*Store).computeNextRunFor

// Next run instant as ISO text, nil when the schedule has no further runs
func (s *Store) computeNextRun(sched *Schedule, lastRunAt *string, timezone *string) (*string, error) {
	return computeNextRunFn(s, sched, lastRunAt, timezone)
}

// Next run instant of the schedule after lastRunAt or now in the job zone, nil when it has no further runs
func (s *Store) computeNextRunFor(sched *Schedule, lastRunAt *string, timezone *string) (*string, error) {
	now := s.clk.now()
	if sched == nil || (sched.Kind == "" && sched.Raw != "") {
		return nil, nil
	}
	jobZone := s.knobs.zoneForJob(timezone)
	if sched.Kind == "once" {
		return s.recoverableOneshotRunAt(sched, now, lastRunAt, jobZone), nil
	}
	base := now
	if deref(lastRunAt) != "" {
		if t, ok := s.clk.parseAware(*lastRunAt, nil); ok {
			base = t
		}
	}
	switch sched.Kind {
	case "interval":
		if !sched.HasMinutes {
			return nil, nil
		}
		next := FormatISO(base.Add(time.Duration(sched.Minutes) * time.Minute).In(base.Location()))
		return &next, nil
	case "cron":
		if sched.Expr == "" {
			return nil, nil
		}
		e, err := ParseCron(sched.Expr)
		if err != nil {
			return nil, err
		}
		zone := jobZone
		if zone == nil {
			zone = s.clk.configuredZone()
		}
		if zone == nil {
			zone = base.Location()
		}
		nextWall, err := e.Next(wallOf(base.In(zone)))
		if err != nil {
			return nil, err
		}
		for i := 0; i < 2; i++ {
			fold0, fold1 := wallInZone(nextWall, zone)
			for _, candidate := range []time.Time{fold0, fold1} {
				if candidate.After(base) {
					out := FormatISO(candidate)
					return &out, nil
				}
			}
			if nextWall, err = e.Next(nextWall); err != nil {
				return nil, err
			}
		}
		fold0, _ := wallInZone(nextWall, zone)
		out := FormatISO(fold0)
		return &out, nil
	}
	return nil, nil
}

// Value rounded to one decimal the way Python round does on the exact binary value
func roundTenth(v float64) float64 {
	f, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'f', 1, 64), 64)
	return f
}

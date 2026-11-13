package cron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	oneshotRunClaimTTLSeconds     = 1800
	defaultCronInactivityTimeout  = 600.0
	restoreKey                    = "manual_run_restore_at"
	emptyPayloadError             = "Cron job has nothing to run: the prompt is blank and no script or skill(s) are set. Provide a prompt, a script, or at least one skill."
	rearmRecurringError           = "Cannot re-arm recurring jobs: re-arm is one-shot-only; use plain resume or cron run."
	createdPausedReason           = "Created paused; awaiting operator approval."
	maxFireForwardFailureDetail   = 500
	completedOneshotRetentionDays = 7
)

var payloadFields = []string{"prompt", "script", "skill", "skills", "no_agent"}

// ValueError is a job argument or schedule the jobs store rejects, the ValueError of cron.jobs
type ValueError struct{ Msg string }

func (e *ValueError) Error() string { return e.Msg }

// AmbiguousJobReferenceError reports a job name matching more than one job
type AmbiguousJobReferenceError struct {
	Ref     string
	Matches []*Job
}

// Error naming the ambiguous reference and the ids of every job it matched
func newAmbiguousJobReferenceError(ref string, matches []*Job) *AmbiguousJobReferenceError {
	return &AmbiguousJobReferenceError{Ref: ref, Matches: matches}
}

func (e *AmbiguousJobReferenceError) Error() string {
	ids := make([]string, len(e.Matches))
	for i, m := range e.Matches {
		ids[i] = m.ID
	}
	return fmt.Sprintf("Job name '%s' is ambiguous — matches %d jobs: %s. Use the job ID instead.",
		e.Ref, len(e.Matches), strings.Join(ids, ", "))
}

// Hex of a random uuid4 the way uuid.uuid4().hex renders it
func uuid4Hex() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}

func newJobID() string { return uuid4Hex()[:12] }

// Run claim TTL of a one shot from ICEBATH_CRON_TIMEOUT times the headroom, floored at thirty minutes
func (s *Store) oneshotRunClaimTTL(ctx context.Context) float64 {
	raw := pystr.Strip(s.cronEnvSetting(ctx, "ICEBATH_CRON_TIMEOUT", ""))
	timeout := defaultCronInactivityTimeout
	if raw != "" {
		if f, err := pyint.ParseFloat(raw); err == nil {
			timeout = f
		}
	}
	if timeout <= 0 {
		return oneshotRunClaimTTLSeconds
	}
	return max(timeout*ClaimTTLInactivityHeadroom, oneshotRunClaimTTLSeconds)
}

// Whether the scheduler of this process is still running the job in this store
func (s *Store) jobRunningInThisProcess(jobID string) bool {
	return isJobRunning(s, jobID)
}

// Well formed claim younger than the TTL whose owner process is not provably dead
func claimIsLive(s *Store, claim *Claim, now time.Time, ttlSeconds float64) bool {
	if claim == nil || claim.At == "" {
		return false
	}
	at, ok := s.clk.parseAware(claim.At, nil)
	if !ok {
		return false
	}
	age := elapsedSeconds(now, at)
	if age < 0 || age >= ttlSeconds {
		return false
	}
	return !claimOwnerIsDead(claim)
}

// Unique ordered skill names from the legacy single skill or the skills list
func normalizeSkillList(skill any, skills any) []string {
	var raw []any
	switch v := skills.(type) {
	case nil:
		if skill != nil && pyrepr.Truthy(skill) {
			raw = []any{skill}
		}
	case string:
		raw = []any{v}
	case []string:
		for _, x := range v {
			raw = append(raw, x)
		}
	case []any:
		raw = v
	default:
		raw = []any{v}
	}
	out := []string{}
	for _, item := range raw {
		text := ""
		if item != nil && pyrepr.Truthy(item) {
			text = pystr.Strip(pyrepr.Str(item))
		}
		if text != "" && !containsString(out, text) {
			out = append(out, text)
		}
	}
	return out
}

func skillAny(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func skillsAny(list []string) any {
	if list == nil {
		return nil
	}
	return list
}

// Aligns the canonical skills list and the legacy skill field on the record
func applySkillFields(job *Job) *Job {
	skills := normalizeSkillList(skillAny(job.Skill), skillsAny(job.Skills))
	job.Skills = skills
	job.Skill = nil
	if len(skills) > 0 {
		job.Skill = strPtr(skills[0])
	}
	return job
}

// Whether a record names one of the given keys, records built in process carrying every key
func (j *Job) hasKey(key string) bool {
	if j.keyOrder == nil {
		return true
	}
	return containsString(j.keyOrder, key)
}

// Record has nothing runnable while naming at least one payload field
func jobPayloadIsEmpty(job *Job) bool {
	if pystr.Strip(job.Prompt) != "" || pystr.Strip(deref(job.Script)) != "" {
		return false
	}
	if len(normalizeSkillList(skillAny(job.Skill), skillsAny(job.Skills))) > 0 {
		return false
	}
	for _, k := range []string{"prompt", "script", "skill", "skills"} {
		if job.hasKey(k) {
			return true
		}
	}
	return false
}

// Display text of the schedule, the stored display first and a question mark when nothing remains
func scheduleDisplayForJob(job *Job) string {
	if d := pystr.Strip(job.ScheduleDisplay); d != "" {
		return d
	}
	sched := job.Schedule
	if sched == nil {
		return "?"
	}
	if sched.Kind == "" && sched.Raw != "" {
		return sched.Raw
	}
	value := ""
	if raw, ok := sched.Extra["value"]; ok {
		value, _ = decodeText(raw)
	}
	for _, text := range []string{sched.Display, value, sched.Expr, sched.RunAt} {
		if t := pystr.Strip(text); t != "" {
			return t
		}
	}
	return "?"
}

// Read safe copy with skills aligned, a derived name and display, and the effective state
func normalizeJobRecord(job *Job) *Job {
	n := applySkillFields(job.Clone())
	n.LatestExecution, n.LiveExecution = job.LatestExecution, job.LiveExecution
	n.ScheduledInstant = job.ScheduledInstant
	if n.ID == "" && !job.hasKey("id") {
		n.ID = "unknown"
	}
	name := pystr.Strip(n.Name)
	if name == "" {
		source := n.Prompt
		if source == "" && len(n.Skills) > 0 {
			source = n.Skills[0]
		}
		if source == "" {
			source = pystr.Strip(deref(n.Script))
		}
		if source == "" {
			source = n.ID
		}
		if source == "" {
			source = "cron job"
		}
		name = pystr.Strip(firstRunes(source, 50))
		if name == "" {
			name = "cron job"
		}
	}
	n.Name = name
	n.ScheduleDisplay = scheduleDisplayForJob(n)
	n.State = effectiveJobState(n)
	return n
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

// Record carries an operator facing pause signal
func hasPauseMarker(job *Job) bool {
	return pystr.Strip(job.State) == "paused" || deref(job.PausedAt) != ""
}

// Scheduler may fire the job, enabled and free of pause markers
func isJobRunnable(job *Job) bool {
	return job.Enabled && !hasPauseMarker(job)
}

// Operator facing state derived from enabled, terminal states kept as stored
func effectiveJobState(job *Job) string {
	stored := pystr.Strip(job.State)
	if stored == "completed" || stored == "error" {
		return stored
	}
	if !job.Enabled {
		if hasPauseMarker(job) || stored == "paused" {
			return "paused"
		}
		if stored != "" {
			return stored
		}
		return "paused"
	}
	if stored == "paused" || deref(job.PausedAt) != "" {
		return "scheduled"
	}
	if stored != "" {
		return stored
	}
	return "scheduled"
}

// Record is in a terminal scheduler state
func isTerminalJob(job *Job) bool {
	return job.State == "completed" || job.State == "error"
}

// Recurring job parked in state error that still has future occurrences
func isRecoverableErrorJob(job *Job) bool {
	return job.State == "error" && isRecurring(job)
}

// Repeat budget from an integer or forever and once words, nil meaning forever
func normalizeRepeatValue(repeat any) (*int, error) {
	var n float64
	switch v := repeat.(type) {
	case nil:
		return nil, nil
	case string:
		text := pystr.Lower(pystr.Strip(v))
		switch text {
		case "forever", "infinite", "inf", "none", "":
			return nil, nil
		case "once", "one", "1x":
			return intPtr(1), nil
		}
		parsed, ok := pyint.Parse(text)
		if !ok {
			return nil, fmt.Errorf("Invalid repeat value %s: use an integer, 'forever', or 'once'.", pyrepr.Repr(v))
		}
		if parsed.Sign() <= 0 {
			return nil, nil
		}
		return intPtr(int(parsed.Int64())), nil
	case bool:
		if v {
			return intPtr(1), nil
		}
		return nil, nil
	case int:
		n = float64(v)
	case int64:
		n = float64(v)
	case float64:
		n = v
	default:
		return nil, fmt.Errorf("'<=' not supported between instances of '%s' and 'int'", pyrepr.TypeName(repeat))
	}
	if n <= 0 {
		return nil, nil
	}
	return intPtr(int(n)), nil
}

// Runs fn on the first record with the id under the jobs lock, found false when no record matches
func withJob[T any](ctx context.Context, s *Store, jobID string, fn func(tx *jobsTx, jobs []*Job, i int, job *Job) (T, error)) (T, bool, error) {
	var out T
	found := false
	err := s.withJobsLock(home.WithOverride(ctx, s.home), func(tx *jobsTx) error {
		jobs, err := s.loadJobs(home.WithOverride(ctx, s.home), tx)
		if err != nil {
			return err
		}
		for i, job := range jobs {
			if job.ID == jobID {
				found = true
				out, err = fn(tx, jobs, i, job)
				return err
			}
		}
		return nil
	})
	return out, found, err
}

// Retires the record as a terminal completion
func completeJobRecord(job *Job) {
	job.Enabled, job.enabledAbsent = false, false
	job.State = "completed"
	job.NextRunAt = nil
}

// Clears pause markers so the record is runnable again
func activateJobRecord(job *Job) {
	job.Enabled, job.enabledAbsent = true, false
	job.State = "scheduled"
	job.PausedAt = nil
	job.PausedReason = nil
}

// Absolute existing directory for a job workdir, nil when empty
func normalizeWorkdir(workdir any) (*string, error) {
	if workdir == nil {
		return nil, nil
	}
	raw := pystr.Strip(pyrepr.Str(workdir))
	if raw == "" {
		return nil, nil
	}
	expanded := pypath.Expanduser(raw)
	if !filepath.IsAbs(expanded) {
		return nil, fmt.Errorf("Cron workdir must be an absolute path (got %s). "+
			"Cron jobs run detached from any shell cwd, so relative paths are ambiguous.", pyrepr.Repr(raw))
	}
	resolved, err := pypath.Realpath(expanded, pypath.Lax)
	if err != nil {
		resolved = filepath.Clean(expanded)
	}
	st, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("Cron workdir does not exist: %s", resolved)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("Cron workdir is not a directory: %s", resolved)
	}
	return &resolved, nil
}

// Stripped text of a string value, nil for non strings and blanks
func normalizeOptionalText(value any, stripTrailingSlash bool) *string {
	text, ok := value.(string)
	if !ok {
		if p, isPtr := value.(*string); isPtr && p != nil {
			text, ok = *p, true
		}
	}
	if !ok {
		return nil
	}
	text = pystr.Strip(text)
	if stripTrailingSlash {
		text = strings.TrimRight(text, "/")
	}
	if text == "" {
		return nil
	}
	return &text
}

func anyList(value any) ([]any, bool) {
	switch v := value.(type) {
	case []any:
		return v, true
	case []string:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = x
		}
		return out, true
	}
	return nil, false
}

func strippedItems(items []any) []string {
	out := []string{}
	for _, item := range items {
		if t := pystr.Strip(pyrepr.Str(item)); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// Toolset allowlist where an explicit empty list means nothing allowed
func normalizeEnabledToolsets(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	items, ok := anyList(value)
	if !ok {
		return nil, errors.New("enabled_toolsets must be a list of toolset names (or [] for none).")
	}
	return strippedItems(items), nil
}

// Job ids whose latest output feeds this job, nil when nothing usable remains
func normalizeContextFrom(value any) []string {
	if text, ok := value.(string); ok {
		value = []any{text}
	}
	items, ok := anyList(value)
	if !ok {
		return nil
	}
	if out := strippedItems(items); len(out) > 0 {
		return out
	}
	return nil
}

// Failure delivery target text with lists joined by commas
func normalizeFailureDeliver(value any) *string {
	if items, ok := anyList(value); ok {
		joined := strings.Join(strippedItems(items), ",")
		if joined == "" {
			return nil
		}
		return &joined
	}
	return normalizeOptionalText(value, false)
}

// Lowercase reasoning level checked against the shared parser, disabled spellings becoming none
func normalizeReasoningEffort(value any) (*string, error) {
	if value == nil {
		return nil, nil
	}
	if p, ok := value.(*string); ok {
		if p == nil {
			return nil, nil
		}
		value = *p
	}
	text := pystr.Lower(pystr.Strip(pyrepr.Str(value)))
	if text == "" {
		return nil, nil
	}
	if provider.ParseReasoningEffort(text) == nil {
		return nil, fmt.Errorf("Invalid reasoning_effort %s. Valid levels: "+
			"none, minimal, low, medium, high, xhigh, max, ultra "+
			"(empty string clears the override).", pyrepr.Repr(value))
	}
	if text == "false" || text == "disabled" {
		return strPtr("none"), nil
	}
	return &text, nil
}

// Catch up policy of true, false or nil for the default
func normalizeCatchUp(value any) (*bool, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case bool:
		return &v, nil
	case *bool:
		return v, nil
	}
	return nil, errors.New("catch_up must be true, false, or null (use the default).")
}

func oneshotPastGraceError(runAt any) error {
	return &ValueError{fmt.Sprintf("Requested one-shot time %s is more than %ds in the past and cannot be scheduled.",
		pyrepr.Str(runAt), oneshotGraceSeconds)}
}

// Next run of a schedule, rejecting a one shot outside the grace window so no ghost job is stored
func (s *Store) nextRunOrRejectPastOneshot(sched *Schedule, label string, fallbackRunAt any, what string, timezone *string) (*string, error) {
	next, err := s.computeNextRun(sched, nil, timezone)
	if err != nil {
		return nil, err
	}
	if sched.Kind == "once" && next == nil {
		var runAt any = fallbackRunAt
		if sched.RunAt != "" {
			runAt = sched.RunAt
		}
		s.log.Warn(fmt.Sprintf("Rejecting one-shot cron job %s'%s': run_at %s is outside the %ds grace window",
			what, label, pyrepr.Str(runAt), oneshotGraceSeconds))
		return nil, oneshotPastGraceError(runAt)
	}
	return next, nil
}

// CreateJobOptions carries every create_job argument
type CreateJobOptions struct {
	Prompt          *string
	Schedule        string
	Name            *string
	Repeat          any
	Deliver         *string
	Origin          map[string]any
	Skill           *string
	Skills          any
	Model           any
	Provider        any
	BaseURL         any
	Script          any
	ContextFrom     any
	EnabledToolsets any
	Workdir         any
	NoAgent         bool
	AttachToSession any
	MonitorScript   any
	MonitorURL      any
	ReasoningEffort any
	FailureDeliver  any
	CatchUp         any
	Paused          bool
	PausedReason    *string
	Pinned          bool
	Interpreter     any
	Timezone        any
	GraceMinutes    any
}

// Creates and stores a job, validating every knob before anything is written
func (s *Store) createJob(ctx context.Context, o CreateJobOptions) (*Job, error) {
	return s.createJobTx(home.WithOverride(ctx, s.home), nil, o)
}

// Creates a job inside the held jobs lock section tx, taking the lock itself when tx is nil
func (s *Store) createJobTx(ctx context.Context, tx *jobsTx, o CreateJobOptions) (*Job, error) {
	if o.PausedReason != nil && !o.Paused {
		return nil, errors.New("paused_reason requires paused=True.")
	}
	parsed, err := s.parseSchedule(o.Schedule)
	if err != nil {
		return nil, err
	}
	repeat, err := normalizeRepeatValue(o.Repeat)
	if err != nil {
		return nil, err
	}
	if parsed.Kind == "once" && repeat == nil {
		repeat = intPtr(1)
	}
	deliver := "local"
	if o.Deliver != nil {
		deliver = *o.Deliver
	} else if len(o.Origin) > 0 {
		deliver = "origin"
	}
	jobID := newJobID()
	now := s.clk.nowISO()
	model := normalizeOptionalText(o.Model, false)
	prov := normalizeOptionalText(o.Provider, false)
	baseURL := normalizeOptionalText(o.BaseURL, true)
	script := normalizeOptionalText(o.Script, false)
	monitorScript := normalizeOptionalText(o.MonitorScript, false)
	monitorURL := normalizeOptionalText(o.MonitorURL, false)
	toolsets, err := normalizeEnabledToolsets(o.EnabledToolsets)
	if err != nil {
		return nil, err
	}
	workdir, err := normalizeWorkdir(o.Workdir)
	if err != nil {
		return nil, err
	}
	contextFrom := normalizeContextFrom(o.ContextFrom)
	failureDeliver := normalizeFailureDeliver(o.FailureDeliver)
	interpreter := normalizeOptionalText(o.Interpreter, false)
	timezone, err := NormalizeTimezone(o.Timezone)
	if err != nil {
		return nil, err
	}
	grace, err := NormalizeGraceMinutes(o.GraceMinutes)
	if err != nil {
		return nil, err
	}
	catchUp, err := normalizeCatchUp(o.CatchUp)
	if err != nil {
		return nil, err
	}
	skills := normalizeSkillList(skillAny(o.Skill), o.Skills)
	var attach *bool
	if b, ok := o.AttachToSession.(bool); ok {
		attach = &b
	}
	effort, err := normalizeReasoningEffort(o.ReasoningEffort)
	if err != nil {
		return nil, err
	}
	if err := s.validateJobModeInvariants(monitorScript, monitorURL, o.NoAgent, script); err != nil {
		return nil, err
	}
	var tzAny, graceAny any
	if timezone != nil {
		tzAny = *timezone
	}
	if grace != nil {
		graceAny = *grace
	}
	if err := validateScheduleKnobs(parsed, tzAny, graceAny); err != nil {
		return nil, err
	}
	promptText := pystr.Strip(deref(o.Prompt))
	if promptText == "" && script == nil && len(skills) == 0 {
		return nil, errors.New(emptyPayloadError)
	}
	if err := checkGatewayLifecycle(s, promptText, deref(script)); err != nil {
		return nil, err
	}
	label := promptText
	if label == "" && len(skills) > 0 {
		label = skills[0]
	}
	if label == "" && o.NoAgent {
		label = deref(script)
	}
	if label == "" {
		label = "cron job"
	}
	name := deref(o.Name)
	if name == "" {
		name = pystr.Strip(firstRunes(label, 50))
	}
	if o.Pinned && model == nil {
		var err error
		if prov, model, err = mainModelPin(s); err != nil {
			return nil, err
		}
	}
	nextRunAt, err := s.nextRunOrRejectPastOneshot(parsed, name, o.Schedule, "", timezone)
	if err != nil {
		return nil, err
	}
	job := &Job{
		ID: jobID, Name: name, Prompt: promptText, Skills: skills, Model: model, Provider: prov,
		BaseURL: baseURL, Script: script, NoAgent: o.NoAgent, MonitorScript: monitorScript,
		MonitorURL: monitorURL, ContextFrom: contextFrom, Schedule: parsed, ScheduleDisplay: parsed.Display,
		Repeat: &Repeat{Times: repeat}, Enabled: !o.Paused, State: "scheduled", CreatedAt: now,
		NextRunAt: nextRunAt, Deliver: &deliver, Origin: o.Origin, EnabledToolsets: toolsets, Workdir: workdir,
		AttachToSession: attach, ReasoningEffort: effort, FailureDeliver: failureDeliver,
		Interpreter: interpreter, Timezone: timezone, GraceMinutes: grace, CatchUp: catchUp,
	}
	if len(skills) > 0 {
		job.Skill = strPtr(skills[0])
	}
	if o.Paused {
		reason := pystr.Strip(deref(o.PausedReason))
		if reason == "" {
			reason = createdPausedReason
		}
		job.State, job.PausedAt, job.PausedReason, job.NextRunAt = "paused", strPtr(now), &reason, nil
	}
	insert := func(tx *jobsTx) error {
		jobs, err := s.loadJobs(home.WithOverride(ctx, s.home), tx)
		if err != nil {
			return err
		}
		return s.saveJobsLocked(home.WithOverride(ctx, s.home), tx, append(jobs, job), nil, false)
	}
	if tx != nil {
		err = insert(tx)
	} else {
		err = s.withJobsLock(home.WithOverride(ctx, s.home), insert)
	}
	if err != nil {
		return nil, err
	}
	return job.Clone(), nil
}

// Job by exact id in read safe form, nil when absent
func (s *Store) getJob(ctx context.Context, jobID string) (*Job, error) {
	jobs, err := s.loadJobs(home.WithOverride(ctx, s.home), nil)
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		if j.ID == jobID {
			return normalizeJobRecord(j), nil
		}
	}
	return nil, nil
}

// Job of a reference in the cron store of h, the ambient home when h is empty
func ResolveJobRef(ctx context.Context, h string, ref string) (*Job, error) {
	if h == "" {
		h = home.Dir(context.Background())
	}
	return openCronStore(h, time.Now).resolveJobRef(ctx, ref)
}

// Job by id, then by case insensitive name, an ambiguous name failing rather than picking one
func (s *Store) resolveJobRef(ctx context.Context, ref string) (*Job, error) {
	if ref == "" {
		return nil, nil
	}
	jobs, err := s.loadJobs(home.WithOverride(ctx, s.home), nil)
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		if j.ID == ref {
			return normalizeJobRecord(j), nil
		}
	}
	lower := pystr.Lower(ref)
	var matches []*Job
	for _, j := range jobs {
		if pystr.Lower(j.Name) == lower {
			matches = append(matches, j)
		}
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return normalizeJobRecord(matches[0]), nil
	}
	norm := make([]*Job, len(matches))
	for i, m := range matches {
		norm[i] = normalizeJobRecord(m)
	}
	return nil, newAmbiguousJobReferenceError(ref, norm)
}

// Read safe jobs with their latest and live executions, disabled ones only when asked
func (s *Store) listJobs(ctx context.Context, includeDisabled bool) ([]*Job, error) {
	raw, err := s.loadJobs(home.WithOverride(ctx, s.home), nil)
	if err != nil {
		return nil, err
	}
	jobs := []*Job{}
	for _, j := range raw {
		n := normalizeJobRecord(j)
		if includeDisabled || n.Enabled {
			jobs = append(jobs, n)
		}
	}
	ids := make([]string, len(jobs))
	for i, j := range jobs {
		ids[i] = j.ID
	}
	latest, err := latestExecutions(s, ids)
	var live map[string]map[string]any
	if err == nil {
		live, err = liveExecutions(s, ids)
	}
	if err != nil {
		latest, live = nil, nil
	}
	for _, j := range jobs {
		j.LatestExecution, j.LiveExecution = latest[j.ID], live[j.ID]
	}
	return jobs, nil
}

// Terminal jobs cannot be reactivated through an update
func rejectTerminalActivation(job, updated *Job, jobID string) error {
	if !isTerminalJob(job) || isRecoverableErrorJob(job) {
		return nil
	}
	enabledTrue := updated.Enabled && !updated.enabledAbsent
	if (updated.State != "completed" && updated.State != "error") || enabledTrue || updated.NextRunAt != nil {
		name := jobID
		if job.hasKey("name") {
			name = job.Name
		}
		return fmt.Errorf("Cannot activate terminal cron job '%s' through update_job; use cron resume --run-now or --at.", name)
	}
	return nil
}

// Rewrites the per job pin from a pinned update, an explicit model winning
func (s *Store) applyPinUpdate(job *Job, updates map[string]any) error {
	raw, ok := updates["pinned"]
	if !ok {
		return nil
	}
	delete(updates, "pinned")
	if _, has := updates["model"]; has {
		return nil
	}
	if pyrepr.Truthy(raw) {
		if normalizeOptionalText(skillAny(job.Model), false) == nil {
			prov, model, err := mainModelPin(s)
			if err != nil {
				return err
			}
			updates["provider"], updates["model"] = skillAny(prov), skillAny(model)
		}
		return nil
	}
	updates["provider"], updates["model"] = nil, nil
	return nil
}

// Normalizes present update fields like create does, coercing repeat while keeping completed
func normalizeJobUpdates(job *Job, updates map[string]any) error {
	type norm func(any) (any, error)
	text := func(v any) (any, error) { return skillAny(normalizeOptionalText(v, false)), nil }
	normalizers := []struct {
		key string
		fn  norm
	}{
		{"enabled_toolsets", func(v any) (any, error) {
			out, err := normalizeEnabledToolsets(v)
			if out == nil {
				return nil, err
			}
			return out, err
		}},
		{"workdir", func(v any) (any, error) {
			if v == nil || v == "" || v == false {
				return nil, nil
			}
			out, err := normalizeWorkdir(v)
			return skillAny(out), err
		}},
		{"monitor_script", text}, {"monitor_url", text}, {"interpreter", text},
		{"reasoning_effort", func(v any) (any, error) {
			out, err := normalizeReasoningEffort(v)
			return skillAny(out), err
		}},
		{"timezone", func(v any) (any, error) {
			out, err := NormalizeTimezone(v)
			return skillAny(out), err
		}},
		{"grace_minutes", func(v any) (any, error) {
			out, err := NormalizeGraceMinutes(v)
			if out == nil {
				return nil, err
			}
			return *out, err
		}},
		{"catch_up", func(v any) (any, error) {
			out, err := normalizeCatchUp(v)
			if out == nil {
				return nil, err
			}
			return *out, err
		}},
	}
	for _, n := range normalizers {
		if v, ok := updates[n.key]; ok {
			out, err := n.fn(v)
			if err != nil {
				return err
			}
			updates[n.key] = out
		}
	}
	rp, ok := updates["repeat"]
	if !ok {
		return nil
	}
	completed := 0
	if job.Repeat != nil {
		completed = job.Repeat.Completed
	}
	if m, isMap := rp.(map[string]any); isMap {
		times, err := normalizeRepeatValue(m["times"])
		if err != nil {
			return err
		}
		out := map[string]any{}
		for k, v := range m {
			out[k] = v
		}
		out["times"] = intOrNil(times)
		if _, has := out["completed"]; !has {
			out["completed"] = completed
		}
		updates["repeat"] = out
		return nil
	}
	if r, isRepeat := rp.(*Repeat); isRepeat && r != nil {
		updates["repeat"] = map[string]any{"times": intOrNil(r.Times), "completed": r.Completed}
		return nil
	}
	times, err := normalizeRepeatValue(rp)
	if err != nil {
		return err
	}
	updates["repeat"] = map[string]any{"times": intOrNil(times), "completed": completed}
	return nil
}

func intOrNil(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// Schedule value of an update as a record, parsing schedule text
func (s *Store) updateSchedule(value any) (*Schedule, error) {
	switch v := value.(type) {
	case string:
		return s.parseSchedule(v)
	case *Schedule:
		return v, nil
	case Schedule:
		return &v, nil
	}
	raw, err := marshalNoEscape(value)
	if err != nil {
		return nil, err
	}
	var sched Schedule
	if err := json.Unmarshal(raw, &sched); err != nil {
		return nil, err
	}
	return &sched, nil
}

// Re-derives the repeat default when a schedule update flips between one shot and recurring
func (s *Store) rederiveRepeatForScheduleChange(job *Job, updates map[string]any) error {
	value, ok := updates["schedule"]
	if !ok {
		return nil
	}
	if _, has := updates["repeat"]; has {
		return nil
	}
	sched, err := s.updateSchedule(value)
	if err != nil {
		return err
	}
	if _, isText := value.(string); isText {
		updates["schedule"] = sched
	}
	oldKind, newKind := job.Kind(), sched.Kind
	if oldKind == newKind {
		return nil
	}
	repeat := Repeat{}
	if job.Repeat != nil {
		repeat = *job.Repeat
	}
	switch {
	case newKind == "once" && repeat.Times == nil:
		repeat.Times = intPtr(1)
	case newKind != "once" && oldKind == "once" && repeat.Times != nil && *repeat.Times == 1:
		repeat.Times = nil
	default:
		return nil
	}
	updates["repeat"] = map[string]any{"times": intOrNil(repeat.Times), "completed": repeat.Completed}
	return nil
}

// Copy of job with every update key applied the way a dict merge does
func mergeJobFields(job *Job, updates map[string]any) (*Job, error) {
	out := job.Clone()
	out.LatestExecution, out.LiveExecution = job.LatestExecution, job.LiveExecution
	return out, setJobFields(out, updates)
}

// Assigns each field onto the record through its JSON form, unknown keys landing in Extra
func setJobFields(job *Job, fields map[string]any) error {
	if job.Extra == nil {
		job.Extra = map[string]json.RawMessage{}
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := fields[k]
		if k == "skills" {
			if text, ok := v.(string); ok {
				v = []string{text}
			} else if items, ok := anyList(v); ok {
				list := make([]string, 0, len(items))
				for _, item := range items {
					list = append(list, pyrepr.Str(item))
				}
				v = list
			}
		}
		raw, err := marshalNoEscape(v)
		if err != nil {
			return err
		}
		if err := job.setField(k, raw); err != nil {
			return err
		}
		if job.keyOrder != nil && !containsString(job.keyOrder, k) {
			job.keyOrder = append(job.keyOrder, k)
		}
	}
	if len(job.Extra) == 0 {
		job.Extra = nil
	}
	return nil
}

// Parses a schedule update and refreshes its display and, unless paused, its next run
func (s *Store) applyScheduleUpdate(updated *Job, updates map[string]any, jobID string) error {
	sched := updated.scheduleOrEmpty()
	if d, ok := updates["schedule_display"]; ok {
		updated.ScheduleDisplay = pyrepr.Str(d)
	} else if sched.Display != "" || sched.Kind != "" {
		updated.ScheduleDisplay = sched.Display
	}
	if updated.State == "paused" {
		return nil
	}
	label := jobID
	if updated.hasKey("name") {
		label = updated.Name
	}
	next, err := s.nextRunOrRejectPastOneshot(sched, label, sched.RunAt, "update ", updated.Timezone)
	if err != nil {
		return err
	}
	updated.NextRunAt = next
	return nil
}

// Enabled unpaused records never persist without a next run
func (s *Store) fillMissingNextRun(updated *Job) error {
	if !updated.Enabled || updated.State == "paused" || deref(updated.NextRunAt) != "" {
		return nil
	}
	sched := updated.scheduleOrEmpty()
	next, err := s.computeNextRun(sched, nil, updated.Timezone)
	if err != nil {
		return err
	}
	if next == nil && sched.Kind == "once" {
		runAt := sched.RunAt
		if runAt == "" {
			runAt = "unknown"
		}
		return fmt.Errorf("Requested one-shot time %s is in the past (grace window: %ds) and cannot be scheduled.",
			runAt, oneshotGraceSeconds)
	}
	updated.NextRunAt = next
	return nil
}

func hasAnyKey(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

// Updates a job by id, refreshing derived schedule fields, and returns its read safe form
func (s *Store) updateJob(ctx context.Context, jobID string, updates map[string]any) (*Job, error) {
	if _, bad := updates["id"]; bad {
		return nil, &ValueError{Msg: "Cron job field(s) cannot be updated: id"}
	}
	out, _, err := withJob(home.WithOverride(ctx, s.home), s, jobID, func(tx *jobsTx, jobs []*Job, i int, job *Job) (*Job, error) {
		if err := s.rederiveRepeatForScheduleChange(job, updates); err != nil {
			return nil, err
		}
		if err := normalizeJobUpdates(job, updates); err != nil {
			return nil, err
		}
		if err := s.applyPinUpdate(job, updates); err != nil {
			return nil, err
		}
		if v, ok := updates["schedule"]; ok {
			sched, err := s.updateSchedule(v)
			if err != nil {
				return nil, err
			}
			updates["schedule"] = sched
		}
		updated, err := mergeJobFields(job, updates)
		if err != nil {
			return nil, err
		}
		applySkillFields(updated)
		if err := rejectTerminalActivation(job, updated, jobID); err != nil {
			return nil, err
		}
		if hasAnyKey(updates, "monitor_script", "monitor_url", "no_agent", "script") {
			if err := s.validateJobModeInvariants(nonEmpty(updated.MonitorScript), nonEmpty(updated.MonitorURL),
				updated.NoAgent, normalizeOptionalText(skillAny(updated.Script), false)); err != nil {
				return nil, err
			}
		}
		if hasAnyKey(updates, payloadFields...) && jobPayloadIsEmpty(updated) {
			return nil, errors.New(emptyPayloadError)
		}
		if hasAnyKey(updates, "timezone", "grace_minutes") {
			var tz, grace any
			if updated.Timezone != nil {
				tz = *updated.Timezone
			}
			if updated.GraceMinutes != nil {
				grace = *updated.GraceMinutes
			} else if raw := rawGraceMinutes(updated); raw != nil {
				grace = raw
			}
			if err := validateScheduleKnobs(updated.scheduleOrEmpty(), tz, grace); err != nil {
				return nil, err
			}
			pruneInheritedKnobs(updated)
		}
		if _, ok := updates["schedule"]; ok {
			if err := s.applyScheduleUpdate(updated, updates, jobID); err != nil {
				return nil, err
			}
			quotaHoldClearState(updated)
		} else if _, ok := updates["timezone"]; ok && updated.State != "paused" {
			recomputed, err := s.computeNextRun(updated.scheduleOrEmpty(), nil, updated.Timezone)
			if err != nil {
				return nil, err
			}
			if recomputed != nil {
				updated.NextRunAt = recomputed
			}
		}
		if hasAnyKey(updates, "schedule", "next_run_at", "enabled", "state", "timezone") {
			updated.PendingSlot = nil
		}
		if err := s.fillMissingNextRun(updated); err != nil {
			return nil, err
		}
		if err := rejectTerminalActivation(job, updated, jobID); err != nil {
			return nil, err
		}
		jobs[i] = updated
		if err := s.saveJobsLocked(home.WithOverride(ctx, s.home), tx, jobs, nil, false); err != nil {
			return nil, err
		}
		return normalizeJobRecord(updated), nil
	})
	return out, err
}

func nonEmpty(p *string) *string {
	if p == nil || *p == "" {
		return nil
	}
	return p
}

// Pauses a job by id or name without deleting it
func (s *Store) pauseJob(ctx context.Context, ref string, reason *string) (*Job, error) {
	job, err := s.resolveJobRef(home.WithOverride(ctx, s.home), ref)
	if err != nil || job == nil {
		return nil, err
	}
	return s.updateJob(home.WithOverride(ctx, s.home), job.ID, map[string]any{"enabled": false, "state": "paused",
		"paused_at": s.clk.nowISO(), "paused_reason": skillAny(reason)})
}

// Resumes a paused job, keeping an occurrence that elapsed while paused as the due instant
func (s *Store) resumeJob(ctx context.Context, ref string) (*Job, error) {
	job, err := s.resolveJobRef(home.WithOverride(ctx, s.home), ref)
	if err != nil || job == nil {
		return nil, err
	}
	sched := job.scheduleOrEmpty()
	var next *string
	stored := deref(job.NextRunAt)
	storedAt, parsed := time.Time{}, false
	if stored != "" {
		storedAt, parsed = s.clk.parseAware(stored, nil)
	}
	if isRecurring(job) && parsed && instantAtOrBefore(storedAt, s.clk.now()) {
		next = &stored
		s.log.Info(fmt.Sprintf("Job '%s' resumed with occurrence %s that elapsed while paused kept due; the next "+
			"tick fires it (late/catch-up) or logs the skip.", job.Name, stored))
	} else if next, err = s.computeNextRun(sched, nil, job.Timezone); err != nil {
		return nil, err
	}
	if next == nil && sched.Kind == "once" {
		runAt := sched.RunAt
		if runAt == "" {
			runAt = "unknown"
		}
		return nil, &ValueError{fmt.Sprintf("Cannot resume: one-shot time %s is in the past (grace window: %ds) and will never fire.",
			runAt, oneshotGraceSeconds)}
	}
	return s.updateJob(home.WithOverride(ctx, s.home), job.ID, map[string]any{"enabled": true, "state": "scheduled",
		"paused_at": nil, "paused_reason": nil, "next_run_at": skillAny(next)})
}

// Schedules a job for the next tick with an optional one fire prompt addition
func (s *Store) triggerJob(ctx context.Context, ref string, extraPrompt string) (*Job, error) {
	job, err := s.resolveJobRef(home.WithOverride(ctx, s.home), ref)
	if err != nil || job == nil {
		return nil, err
	}
	if isTerminalJob(job) {
		name := job.Name
		return nil, &ValueError{fmt.Sprintf("Cannot run: job '%s' is %s (terminal). Create a new occurrence with "+
			"'icebath cron resume %s --run-now' or '--at <ISO-8601>'.", name, job.State, name)}
	}
	manual := s.clk.nowISO()
	var prompt any
	if extraPrompt != "" {
		prompt = extraPrompt
	}
	return s.updateJob(home.WithOverride(ctx, s.home), job.ID, map[string]any{"enabled": true, "state": "scheduled", "paused_at": nil,
		"paused_reason": nil, "next_run_at": manual, "manual_run_at": manual,
		restoreKey: skillAny(extraFireRestoreTarget(job)), "manual_run_prompt": prompt})
}

// Re-arms a completed one shot as an explicit new occurrence
func (s *Store) rearmOneshot(ctx context.Context, ref string, runAt any) (*Job, error) {
	jobRef, err := s.resolveJobRef(home.WithOverride(ctx, s.home), ref)
	if err != nil || jobRef == nil {
		return nil, err
	}
	if t, ok := runAt.(time.Time); ok {
		runAt = FormatISO(t)
	}
	parsed, err := s.parseSchedule(pyrepr.Str(runAt))
	if err != nil {
		return nil, &ValueError{err.Error()}
	}
	if parsed.Kind != "once" {
		return nil, &ValueError{rearmRecurringError}
	}
	next, err := s.computeNextRun(parsed, nil, jobRef.Timezone)
	if err != nil {
		return nil, err
	}
	if next == nil {
		var shown any = runAt
		if parsed.RunAt != "" {
			shown = parsed.RunAt
		}
		return nil, oneshotPastGraceError(shown)
	}
	ttl := s.oneshotRunClaimTTL(ctx)
	out, _, err := withJob(home.WithOverride(ctx, s.home), s, jobRef.ID, func(tx *jobsTx, jobs []*Job, i int, job *Job) (*Job, error) {
		now := s.clk.now()
		if claimIsLive(s, job.RunClaim, now, ttl) {
			return nil, &ValueError{"Cannot re-arm one-shot over a live run claim."}
		}
		if claimIsLive(s, job.FireClaim, now, FireClaimTTLSeconds) {
			return nil, &ValueError{"Cannot re-arm one-shot over a live fire claim."}
		}
		if job.Kind() != "once" {
			return nil, &ValueError{rearmRecurringError}
		}
		if job.Repeat == nil {
			job.Repeat = &Repeat{}
		}
		job.Repeat.Completed = 0
		job.Schedule, job.ScheduleDisplay = parsed, parsed.Display
		job.RunClaim, job.FireClaim = nil, nil
		activateJobRecord(job)
		job.NextRunAt = next
		if err := s.saveJobsLocked(home.WithOverride(ctx, s.home), tx, jobs, nil, false); err != nil {
			return nil, err
		}
		return normalizeJobRecord(job), nil
	})
	return out, err
}

// Removes a job by id or name with its output, notepad and fence, stopping a live run started elsewhere
func (s *Store) removeJob(ctx context.Context, ref string) (bool, error) {
	job, err := s.resolveJobRef(home.WithOverride(ctx, s.home), ref)
	if err != nil || job == nil {
		return false, err
	}
	id := job.ID
	removed := false
	err = s.withJobsLock(home.WithOverride(ctx, s.home), func(tx *jobsTx) error {
		jobs, err := s.loadJobs(home.WithOverride(ctx, s.home), tx)
		if err != nil {
			return err
		}
		kept := make([]*Job, 0, len(jobs))
		for _, j := range jobs {
			if j.ID != id {
				kept = append(kept, j)
			}
		}
		if len(kept) == len(jobs) {
			return nil
		}
		outDir, err := s.jobOutputDir(id)
		if err != nil {
			return err
		}
		if err := s.saveJobsLocked(home.WithOverride(ctx, s.home), tx, kept, []string{id}, false); err != nil {
			return err
		}
		removed = true
		if marker, _ := ctx.Value(selfRemovalKey{}).(*selfRemovalDelivery); marker != nil && marker.jobID == id {
			marker.removed = true
		} else {
			if _, err := s.cancelJobRun(id, "job removed while running"); err != nil {
				return err
			}
		}
		if _, err := os.Stat(outDir); err == nil {
			if err := os.RemoveAll(outDir); err != nil {
				return err
			}
		}
		if err := clearNotepad(s, id); err != nil {
			s.log.Debug(fmt.Sprintf("Failed to clear notepad for removed job %s: %v", id, err))
		}
		fireFenceMu.Lock()
		delete(fireFenceLocks, s.resolvedCronDir()+"::"+id)
		fireFenceMu.Unlock()
		return nil
	})
	return removed, err
}

// Sets or clears the preflight alert marker and returns its prior value
func (s *Store) setPreflightAlerted(ctx context.Context, jobID string, value bool) (bool, error) {
	prior, _, err := withJob(home.WithOverride(ctx, s.home), s, jobID, func(tx *jobsTx, jobs []*Job, i int, job *Job) (bool, error) {
		prior := job.PreflightAlerted
		job.PreflightAlerted = value
		if prior != value {
			return prior, s.saveJobsLocked(home.WithOverride(ctx, s.home), tx, jobs, nil, false)
		}
		return prior, nil
	})
	return prior, err
}

// Marks the job preflight alerted and reports whether it already was
func (s *Store) markPreflightAlerted(ctx context.Context, jobID string) (bool, error) {
	return s.setPreflightAlerted(home.WithOverride(ctx, s.home), jobID, true)
}

// Clears the preflight alert marker once the config validates again
func (s *Store) clearPreflightAlerted(ctx context.Context, jobID string) error {
	_, err := s.setPreflightAlerted(home.WithOverride(ctx, s.home), jobID, false)
	return err
}

// Records that a scheduled fire could not be handed to the runner
func (s *Store) noteFireForwardFailure(ctx context.Context, jobID, detail string) (bool, error) {
	ok, _, err := withJob(home.WithOverride(ctx, s.home), s, jobID, func(tx *jobsTx, jobs []*Job, i int, job *Job) (bool, error) {
		job.LastFireError = &Stamp{At: s.clk.nowISO(), Detail: firstRunes(detail, maxFireForwardFailureDetail)}
		return true, s.saveJobsLocked(home.WithOverride(ctx, s.home), tx, jobs, nil, false)
	})
	return ok, err
}

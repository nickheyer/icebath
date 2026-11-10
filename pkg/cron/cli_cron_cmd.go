package cron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/tools"
)

// CronArgs is the parsed icebath cron argument namespace, any fields carrying the parser value or nil
type CronArgs struct {
	Command, JobID, IncidentAction, IncidentID, NotepadAction string
	All, ClearSkills, Paused, RunNow                          bool
	Limit                                                     *int
	State, Key, Value, Schedule, Prompt, Skill, PausedReason  *string
	RunAt, Reason                                             *string
	Skills, AddSkills, RemoveSkills                           []string
	NoAgent                                                   *bool
	Name, Deliver, FailureDeliver, Repeat, Script, Workdir    any
	Model, ModelProvider, Pinned, MonitorScript, MonitorURL   any
	Continuity, ReasoningEffort, Interpreter, Timezone        any
	GraceMinutes, CatchUp                                     any
}

func color(text string, code string) string { return cliui.Color(text, code) }

func say(w io.Writer, lines ...string) {
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
}

// Deduped stripped skill names, nil when neither argument was given
func normalizeSkillArgs(single *string, skills []string) []string {
	if skills == nil && single == nil {
		return nil
	}
	items := skills
	if skills == nil {
		items = []string{*single}
	}
	out := []string{}
	for _, item := range items {
		if text := pystr.Strip(item); text != "" && !containsString(out, text) {
			out = append(out, text)
		}
	}
	return out
}

// Scheduler readiness warning at create and list time, silent on an unknown probe result
func (s *Store) warnIfGatewayNotRunning(ctx context.Context, w io.Writer) {
	if alive, known := s.builtinGatewayLiveness(ctx); alive || !known {
		return
	}
	say(w, color(fmt.Sprintf("  ⚠  Scheduler is not ready for profile '%s': no gateway serves it and no fresh ticker heartbeat.", profiles.GetActiveProfileName(ctx)), cliui.Yellow),
		color("     If no gateway is running: icebath gateway install\n                    sudo icebath gateway install --system  # Linux servers\n     Check status:  icebath cron status", cliui.Dim))
}

// One line scheduled versus actual dispatch summary, false when the stamp is malformed
func dispatchDisplay(dispatch any) (string, bool) {
	d, ok := dispatch.(*pyjson.Object)
	if !ok {
		return "", false
	}
	scheduled, actual, kind := field(d, "scheduled_at", nil), field(d, "dispatched_at", nil), field(d, "kind", nil)
	if !pyrepr.Truthy(scheduled) || !pyrepr.Truthy(actual) || !pyrepr.Truthy(kind) {
		return "", false
	}
	lateness := formatLateness(numberOf(field(d, "lateness_seconds", 0)))
	if kind == "on_time" {
		return color(fmt.Sprintf("on time (scheduled %s)", pyrepr.Str(scheduled)), cliui.Dim), true
	}
	label := dispatchKindLabel(kind)
	if label == "" {
		label = "late"
	}
	return color("⚠ "+label+": ", cliui.Yellow) + fmt.Sprintf("scheduled %s, ran %s ", pyrepr.Str(scheduled), pyrepr.Str(actual)) +
		color("("+lateness+" late)", cliui.Yellow), true
}

func numberOf(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	case *big.Int:
		f, _ := new(big.Float).SetInt(n).Float64()
		return f
	case bool:
		if n {
			return 1
		}
	}
	return 0
}

// Object of a ledger row map with its keys sorted
func mapObject(m map[string]any) *pyjson.Object {
	o := pyjson.NewObject()
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		o.Set(k, m[k])
	}
	return o
}

// Boxed cyan section header shared by cron list and cron incidents
func printBanner(w io.Writer, title string) {
	fmt.Fprintln(w)
	rule := strings.Repeat("─", 73)
	for _, line := range []string{"┌" + rule + "┐", "│" + strings.Repeat(" ", 25) + pystr.Ljust(title, 48) + "│", "└" + rule + "┘"} {
		say(w, color(line, cliui.Cyan))
	}
	fmt.Fprintln(w)
}

func unverifiedTargets(v any) string {
	if list, ok := v.([]any); ok {
		parts := make([]string, len(list))
		for i, x := range list {
			parts[i] = pyrepr.Str(x)
		}
		return strings.Join(parts, ", ")
	}
	return pyrepr.Str(v)
}

var stateBadges = map[string][2]string{"paused": {"[paused]", cliui.Yellow}, "completed": {"[completed]", cliui.Blue}}

// Decoded record of a listed job with the latest execution list_jobs attaches
func listedJobObject(job *Job) (*pyjson.Object, error) {
	raw, err := json.Marshal(job)
	if err != nil {
		return nil, err
	}
	v, err := pyjson.Loads(string(raw))
	if err != nil {
		return nil, err
	}
	obj := v.(*pyjson.Object)
	if job.LatestExecution != nil {
		obj.Set("latest_execution", mapObject(job.LatestExecution))
	} else {
		obj.Set("latest_execution", nil)
	}
	return obj, nil
}

func enabledOf(job *pyjson.Object) bool { return pyrepr.Truthy(field(job, "enabled", true)) }

// Prints every scheduled job with its detail rows and warnings
func (s *Store) cronList(ctx context.Context, w io.Writer, showAll bool) error {
	_, jobs, err := s.probeThenListJobs(ctx, w, true, true)
	if err != nil || jobs == nil {
		return err
	}
	if !showAll {
		kept := jobs[:0:0]
		for _, j := range jobs {
			if enabledOf(j.obj) || effectiveJobState(j.job) == "paused" {
				kept = append(kept, j)
			}
		}
		jobs = kept
	}
	active := profiles.GetActiveProfileName(ctx)
	if len(jobs) == 0 {
		say(w, color(fmt.Sprintf("No scheduled jobs in profile '%s'.\nCreate one with 'icebath cron create ...' or the /cron command in chat.", active), cliui.Dim))
		return nil
	}
	printBanner(w, fmt.Sprintf("Scheduled Jobs (profile: %s)", active))
	for _, j := range jobs {
		badge, ok := stateBadges[effectiveJobState(j.job)]
		if !ok {
			badge = [2]string{"[disabled]", cliui.Red}
			if enabledOf(j.obj) {
				badge = [2]string{"[active]", cliui.Green}
			}
		}
		fmt.Fprintf(w, "  %s %s\n", color(pyrepr.Str(field(j.obj, "id", "?")), cliui.Yellow), color(badge[0], badge[1]))
		for _, row := range s.jobRows(j.obj) {
			fmt.Fprintf(w, "    %s%s\n", pystr.Ljust(row[0]+":", 11), row[1])
		}
		for _, line := range jobWarnings(j.obj) {
			fmt.Fprintf(w, "    %s\n", line)
		}
		fmt.Fprintln(w)
	}
	s.warnIfGatewayNotRunning(ctx, w)
	return nil
}

func lastRunDisplay(job *pyjson.Object) string {
	lastStatus := field(job, "last_status", nil)
	switch lastStatus {
	case "ok":
		return color("ok", cliui.Green)
	case "delivery_queued":
		return color("finished; delivery is still in progress", cliui.Yellow)
	case "delivery_failed":
		return color(fmt.Sprintf("ran, but the result was not delivered (%s). %s", shortReason(field(job, "last_delivery_error", nil), 120), deliveryFixHint(job)), cliui.Yellow)
	}
	display := color(fmt.Sprintf("%s: %s", pyrepr.Str(lastStatus), pyrepr.Str(field(job, "last_error", "?"))), cliui.Red)
	streak := 0
	if v := field(job, "failure_streak", nil); pyrepr.Truthy(v) {
		streak = int(numberOf(v))
	}
	if streak >= 2 {
		display += color(fmt.Sprintf("  (%d failures in a row)", streak), cliui.Red)
	}
	return display
}

func objOrEmpty(v any) *pyjson.Object {
	if o, ok := v.(*pyjson.Object); ok && o.Len() > 0 {
		return o
	}
	return pyjson.NewObject()
}

// Label and value detail rows of one job in cron list
func (s *Store) jobRows(job *pyjson.Object) [][2]string {
	repeatInfo := objOrEmpty(field(job, "repeat", nil))
	repeatTimes := field(repeatInfo, "times", nil)
	deliver := field(job, "deliver", nil)
	if !pyrepr.Truthy(deliver) {
		deliver = []any{"local"}
	}
	var skills []string
	if v := field(job, "skills", nil); pyrepr.Truthy(v) {
		for _, x := range v.([]any) {
			skills = append(skills, pyrepr.Str(x))
		}
	} else if sk := field(job, "skill", nil); pyrepr.Truthy(sk) {
		skills = []string{pyrepr.Str(sk)}
	}
	monitorSource := field(job, "monitor_script", nil)
	if !pyrepr.Truthy(monitorSource) {
		monitorSource = field(job, "monitor_url", nil)
	}
	monState := objOrEmpty(field(job, "monitor_state", nil))
	latest := objOrEmpty(field(job, "latest_execution", nil))
	str := func(v any) string {
		if !pyrepr.Truthy(v) {
			return ""
		}
		return pyrepr.Str(v)
	}
	optional := [][2]string{{"Skills", strings.Join(skills, ", ")}, {"Script", str(field(job, "script", nil))}}
	if pyrepr.Truthy(monitorSource) {
		optional = append(optional, [2]string{"Monitor", pyrepr.Str(monitorSource) + " (agent runs only on output change)"}, [2]string{"Changed", str(field(monState, "last_changed_at", nil))})
	}
	if pyrepr.Truthy(field(job, "no_agent", nil)) {
		optional = append(optional, [2]string{"Mode", color("no-agent", cliui.Dim) + " (script stdout delivered directly)"})
	}
	optional = append(optional, [2]string{"Timezone", str(field(job, "timezone", nil))})
	if g := field(job, "grace_minutes", nil); g != nil {
		optional = append(optional, [2]string{"Grace", pyrepr.Str(g) + " min"})
	}
	optional = append(optional, [2]string{"Workdir", str(field(job, "workdir", nil))}, [2]string{"Python", str(field(job, "interpreter", nil))})
	if pyrepr.Truthy(field(job, "last_status", nil)) {
		optional = append(optional, [2]string{"Last run", fmt.Sprintf("%s  %s", pyrepr.Str(field(job, "last_run_at", "?")), lastRunDisplay(job))})
	}
	if d, ok := dispatchDisplay(field(job, "last_dispatch", nil)); ok {
		optional = append(optional, [2]string{"Dispatch", d})
	}
	if latest.Len() > 0 {
		optional = append(optional, [2]string{"Execution", fmt.Sprintf("%s  %s", pyrepr.Str(field(latest, "status", "?")), pyrepr.Str(field(latest, "id", "?")))})
	}
	schedule := field(job, "schedule_display", nil)
	if _, ok := job.Get("schedule_display"); !ok {
		schedule = field(objOrEmpty(field(job, "schedule", nil)), "value", "?")
	}
	repeat := "∞"
	if pyrepr.Truthy(repeatTimes) {
		repeat = fmt.Sprintf("%s/%s", pyrepr.Str(field(repeatInfo, "completed", 0)), pyrepr.Str(repeatTimes))
	}
	deliverText := ""
	if d, ok := deliver.(string); ok {
		deliverText = d
	} else {
		deliverText = unverifiedTargets(deliver)
	}
	label, value := s.nextRunRow(job)
	rows := [][2]string{{"Name", pyrepr.Str(field(job, "name", "(unnamed)"))}, {"Schedule", pyrepr.Str(schedule)}, {"Repeat", repeat}, {label, value}, {"Deliver", deliverText}}
	for _, r := range optional {
		if r[1] != "" {
			rows = append(rows, r)
		}
	}
	return rows
}

// First line of an error blob, whitespace collapsed and capped, or no details
func shortReason(text any, limit int) string {
	t := ""
	if pyrepr.Truthy(text) {
		t = pyrepr.Str(text)
	}
	lines := pystr.Splitlines(pystr.Strip(t), false)
	reason := ""
	if len(lines) > 0 {
		reason = pystr.Strip(collapseWhitespace(lines[0]))
	}
	if rs := []rune(reason); len(rs) > limit {
		return string(rs[:limit-1]) + "…"
	}
	if reason == "" {
		return "no details"
	}
	return reason
}

func deliveryFixHint(job *pyjson.Object) string {
	return fmt.Sprintf("Check the target with `icebath cron status` or change it with `icebath cron edit %s --deliver <target>`.", pyrepr.Str(field(job, "id", "<id>")))
}

func missedFireIssue(job, fireErr *pyjson.Object) string {
	return fmt.Sprintf("missed scheduled fire at %s: %s. The messaging gateway was unreachable. Run `icebath gateway restart`, then `icebath cron run %s` to run it now.",
		pyrepr.Str(field(fireErr, "at", "?")), shortReason(field(fireErr, "detail", nil), 120), pyrepr.Str(field(job, "id", "<id>")))
}

// Delivery and fire warning lines of one job in cron list
func jobWarnings(job *pyjson.Object) []string {
	var lines []string
	if q := field(job, "last_delivery_queued", nil); pyrepr.Truthy(q) {
		lines = append(lines, "Delivery still in progress (the result was handed off but not confirmed yet): "+pyrepr.Str(q))
	}
	if e := field(job, "last_delivery_error", nil); pyrepr.Truthy(e) {
		lines = append(lines, fmt.Sprintf("%s %s. %s", color("⚠ The result was not delivered:", cliui.Yellow), shortReason(e, 120), deliveryFixHint(job)))
	}
	if u := field(job, "last_delivery_unverified", nil); pyrepr.Truthy(u) {
		lines = append(lines, fmt.Sprintf("%s adapter acked %s without message_id/raw_response", color("⚠ Delivery UNVERIFIED:", cliui.Yellow), unverifiedTargets(u)))
	}
	if fe, ok := field(job, "last_fire_error", nil).(*pyjson.Object); ok && pyrepr.Truthy(field(fe, "detail", nil)) {
		lines = append(lines, color("⚠ "+missedFireIssue(job, fe), cliui.Red))
	}
	if lf, ok := field(job, "last_failure", nil).(*pyjson.Object); ok && pyrepr.Truthy(field(lf, "detail", nil)) {
		st := ""
		if v := field(job, "last_status", nil); pyrepr.Truthy(v) {
			st = pyrepr.Str(v)
		}
		recovered := st == "ok" || st == "delivery_queued"
		text := fmt.Sprintf("⚠ Last failure at %s: %s", pyrepr.Str(field(lf, "at", "?")), shortReason(field(lf, "detail", nil), 120))
		code := cliui.Red
		if recovered {
			text += " — recovered since"
			code = cliui.Yellow
		}
		lines = append(lines, color(text, code))
	}
	return lines
}

// Runs due jobs once headless for an external crontab, nonzero when the tick yielded or failed
func (s *Store) cronTick(ctx context.Context, w io.Writer) (int, error) {
	observability.BeginProcess(ctx, "cron")
	_, err := schedulerTick(ctx, s, tickOptions{Verbose: true, Headless: true})
	var yielded *CronTickYielded
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &yielded):
		say(w, color("✗ "+yielded.Error(), cliui.Yellow), "  A fresher gateway process owns the runtime lock and will fire due jobs; this stale process yielded its tick.")
		return 1, nil
	case isOSError(err):
		say(w, color("✗ Cron tick failed: "+err.Error(), cliui.Red), "  Check `icebath cron status` and the gateway log for details.")
		return 1, nil
	}
	return 0, err
}

// Prints the indexed durable execution history
func (s *Store) cronRuns(ctx context.Context, w io.Writer, jobID *string, limit int) error {
	records, err := openExecutionsLedger(s).List(ctx, jobID, limit, nil)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		say(w, "No cron execution attempts recorded.")
		return nil
	}
	get := func(r map[string]any, k string) string {
		if v, ok := r[k]; ok {
			return pyrepr.Str(v)
		}
		return "?"
	}
	for _, r := range records {
		fmt.Fprintf(w, "%s  %s  job=%s  source=%s  %s\n", get(r, "id"), pystr.Ljust(get(r, "status"), 9), get(r, "job_id"), get(r, "source"), get(r, "claimed_at"))
		if pyrepr.Truthy(r["error"]) {
			fmt.Fprintf(w, "    %s\n", pyrepr.Str(r["error"]))
		}
	}
	return nil
}

var incidentStateColors = map[string]string{"detected": cliui.Red, "alerted": cliui.Yellow, "resolved": cliui.Green, "closed": cliui.Dim}

// Runs of Python whitespace replaced by single spaces
func collapseWhitespace(text string) string {
	out, err := whitespaceRunRe.Replace(text, " ", -1, -1)
	if err != nil {
		return text
	}
	return out
}

// Lists or acks durable failure incidents
func (s *Store) cronIncidents(ctx context.Context, w io.Writer, a *CronArgs) (int, error) {
	store := openIncidents(s)
	action := a.IncidentAction
	if action == "" {
		action = "list"
	}
	if action == "ack" {
		if a.IncidentID == "" {
			say(w, color("✗ Incident ID required: icebath cron incidents ack <incident_id>", cliui.Red))
			return 1, nil
		}
		ok, err := store.AckIncident(a.IncidentID)
		if err != nil {
			return 0, err
		}
		if ok {
			say(w, color(fmt.Sprintf("✓ Incident %s acknowledged (closed).", a.IncidentID), cliui.Green))
		} else {
			say(w, color(fmt.Sprintf("Incident %s not found or already closed.", a.IncidentID), cliui.Yellow))
		}
		return 0, nil
	}
	incidents, err := store.ListIncidents(a.State)
	if err != nil {
		return 0, err
	}
	if len(incidents) == 0 {
		say(w, color("No cron failure incidents recorded.", cliui.Dim))
		if a.State != nil && *a.State != "" {
			say(w, color(fmt.Sprintf("  (filtered by state '%s')", *a.State), cliui.Dim))
		}
		return 0, nil
	}
	printBanner(w, "Cron Failure Incidents")
	for _, inc := range incidents {
		code, ok := incidentStateColors[inc.State]
		if !ok {
			code = cliui.Dim
		}
		errText := pystr.Strip(collapseWhitespace(inc.Error))
		if rs := []rune(errText); len(rs) > 160 {
			errText = pystr.Rstrip(string(rs[:157])) + "..."
		}
		output := ""
		if inc.OutputFile != nil {
			output = *inc.OutputFile
		}
		fmt.Fprintf(w, "  %s  %s\n", color(inc.ID, cliui.Yellow), color(inc.State, code))
		for _, row := range [][2]string{{"Job", inc.JobID}, {"Type", inc.FailureType}, {"First seen", inc.FirstSeenAt}, {"Last seen", inc.LastSeenAt}, {"Error", errText}, {"Output", output}} {
			if row[0] != "Output" || row[1] != "" {
				fmt.Fprintf(w, "    %s%s\n", pystr.Ljust(row[0]+":", 12), row[1])
			}
		}
		fmt.Fprintln(w)
	}
	say(w, color(fmt.Sprintf("  %d incident(s)  |  ack one with: icebath cron incidents ack <id>", len(incidents)), cliui.Dim))
	return 0, nil
}

const (
	cronPermissionHint   = "  Hint: jobs.json may be owned by another user (e.g. rewritten by a root `docker exec icebath icebath cron ...`). Fix ownership to match the gateway user, and prefer `docker exec -u <uid>:<gid>`."
	cronFDExhaustionHint = "  Hint: the ticker hit file-descriptor exhaustion (EMFILE). The scheduler now retries with backoff and attempts fd reclamation, but if the leak persists, restart the gateway to recover scheduling."
)

// Probe of the active cron store, nil when it accepts writes
func (s *Store) storeUnwritableReport() map[string]any { return ProbeReport(s.cronDir) }

type listedJob struct {
	job *Job
	obj *pyjson.Object
}

// Store report and jobs after printing the unwritable store warning, nil jobs when the read failed on a reported store
func (s *Store) probeThenListJobs(ctx context.Context, w io.Writer, includeDisabled, brief bool) (map[string]any, []listedJob, error) {
	report := s.storeUnwritableReport()
	if report != nil {
		printStoreUnwritable(w, report, brief)
	}
	jobs, err := s.listJobs(home.WithOverride(ctx, s.home), includeDisabled)
	if err != nil {
		if report == nil || !isOSError(err) {
			return report, nil, err
		}
		return report, nil, nil
	}
	out := make([]listedJob, 0, len(jobs))
	for _, j := range jobs {
		obj, err := listedJobObject(j)
		if err != nil {
			return report, nil, err
		}
		out = append(out, listedJob{job: j, obj: obj})
	}
	return report, out, nil
}

func printStoreUnwritable(w io.Writer, r map[string]any, brief bool) {
	if brief {
		say(w, color(fmt.Sprintf("⚠ Cron store %s is NOT writable (%s) — scheduled jobs are being skipped; %s. See `icebath cron status`.",
			pyrepr.Str(r["store"]), pyrepr.Str(r["error"]), pyrepr.Str(r["fix"])), cliui.Red))
		return
	}
	say(w, color("⚠ Cron store is NOT writable — scheduled jobs are being skipped", cliui.Red),
		color(fmt.Sprintf("  %s: %s (last successful write %s)", pyrepr.Str(r["store"]), pyrepr.Str(r["error"]), pyrepr.Str(r["since"])), cliui.Red),
		color(fmt.Sprintf("  Fix: %s. Each due job then fires once on the next tick.", pyrepr.Str(r["fix"])), cliui.Yellow))
}

// Builtin ticker liveness report for a gateway known to be alive, checking heartbeat and last success
func (s *Store) printTickerHealth(w io.Writer, pids []int, restart string) {
	hbAge, hbOK := s.TickerHeartbeatAge()
	okAge, okOK := s.TickerSuccessAge()
	lastError, hasErr := s.TickerLastError()
	pidLine := ""
	if len(pids) > 0 {
		parts := make([]string, len(pids))
		for i, p := range pids {
			parts[i] = fmt.Sprint(p)
		}
		pidLine = "  PID: " + strings.Join(parts, ", ")
	}
	warn := func(headline string) {
		say(w, color(headline, cliui.Yellow))
		if pidLine != "" {
			say(w, pidLine)
		}
	}
	bootRev, diskRev, skew := "", "", false
	if hasErr {
		bootRev, diskRev, skew = staleCodeYieldLabels(lastError)
	}
	switch {
	case !hbOK:
		warn("⚠ Gateway is running but the cron ticker has not reported a heartbeat.")
		say(w, "  Cron jobs will NOT fire until the ticker writes its first heartbeat.\n  If the gateway just started, wait ~60s and re-run `icebath cron status`.\n  If heartbeat never appears, restart: "+restart)
	case !tickerAgeIsFresh(hbAge, hbOK):
		warn(fmt.Sprintf("⚠ Gateway is running but the cron ticker looks STALLED — no heartbeat for %ds (expected every ~60s).", int(hbAge)))
		say(w, "  Cron jobs may NOT be firing. Restart: "+restart)
	case skew:
		warn("⚠ Gateway is running STALE code — its cron ticker yields every tick and fires NOTHING.")
		say(w, color(fmt.Sprintf("  Booted on %s, checkout is now at %s (the code was updated under the running gateway).", bootRev, diskRev), cliui.Red), "  Restart it onto the new code: "+restart)
	case (okOK && !tickerAgeIsFresh(okAge, okOK)) || (!okOK && hasErr && lastError != ""):
		when := "yet"
		if okOK {
			when = fmt.Sprintf("in %ds", int(okAge))
		}
		warn(fmt.Sprintf("⚠ Gateway and cron ticker are running, but no tick has succeeded %s — ticks may be failing.", when))
		if hasErr && lastError != "" {
			say(w, color("  Last tick error: "+lastError, cliui.Red))
			if strings.Contains(lastError, "Permission denied") {
				say(w, color(cronPermissionHint, cliui.Yellow))
			} else if isFDExhaustionText(lastError) {
				say(w, color(cronFDExhaustionHint, cliui.Yellow))
			}
		}
		say(w, "  Check the gateway log for 'Cron tick error'.")
	default:
		say(w, color("✓ Gateway is running — cron jobs will fire automatically", cliui.Green))
		if pidLine != "" {
			say(w, pidLine)
		}
		say(w, fmt.Sprintf("  Ticker heartbeat: %ds ago", int(hbAge)))
	}
}

func suppressPanic(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

// Prints scheduler health of the active profile and its active jobs summary
func (s *Store) cronStatus(ctx context.Context, w io.Writer) error {
	fmt.Fprintln(w)
	report, active, err := s.probeThenListJobs(ctx, w, false, false)
	if err != nil {
		return err
	}
	if active == nil && report != nil {
		say(w, color("  Jobs could not be read from the unwritable store.", cliui.Red))
	} else if report != nil {
		overdue := 0
		for _, j := range active {
			if secs, ok := s.nextRunOverdueSeconds(field(j.obj, "next_run_at", nil)); ok && secs > 0 {
				overdue++
			}
		}
		say(w, color(fmt.Sprintf("  %d due run(s) not fired", overdue), cliui.Red))
	}
	if report != nil {
		fmt.Fprintln(w)
	}
	if provider := s.activeCronProviderName(); provider != "builtin" {
		say(w, color(fmt.Sprintf("✓ Cron provider: %s — jobs fire via the managed scheduler, not the in-process ticker.", provider), cliui.Green),
			color("  (No ticker heartbeat is expected for an external provider; due jobs are delivered by an authenticated webhook.)", cliui.Dim))
	} else if err := s.printSchedulerHost(ctx, w); err != nil {
		return err
	}
	fmt.Fprintln(w)
	s.printActiveJobsSummary(w, active)
	fmt.Fprintln(w)
	return nil
}

// Which scheduler serves the builtin provider of this profile and how healthy its ticker is
func (s *Store) printSchedulerHost(ctx context.Context, w io.Writer) error {
	active := profiles.GetActiveProfileName(ctx)
	var host *gateway.HostGatewayTopology
	suppressPanic(func() {
		if h, err := gateway.HostGatewayServing(ctx, active); err == nil {
			host = h
		}
	})
	var pids []int
	if host == nil {
		found, err := gateway.FindGatewayPIDs(ctx, s.home)
		if err != nil {
			return err
		}
		pids = found
	}
	viaLock, viaMultiplexer, inProcess := false, false, false
	if host == nil && len(pids) == 0 {
		suppressPanic(func() {
			ok, err := gateway.IsGatewayRuntimeLockActive(gateway.GatewayRuntimeLockPath(s.home))
			if err != nil {
				return
			}
			viaLock = ok
			if viaLock {
				if pid, alive, err := gateway.GetRunningPid("", true); err == nil && alive && pid != 0 {
					pids = []int{pid}
				}
			}
		})
		if !viaLock && gateway.NamedProfileServedByRunningMultiplexer != nil {
			profile, _ := gateway.ProfileNameForHome(s.home)
			viaMultiplexer = gateway.NamedProfileServedByRunningMultiplexer(profile)
		}
		if !viaLock && !viaMultiplexer {
			suppressPanic(func() { inProcess = tickerAgeIsFresh(s.TickerHeartbeatAge()) && s.TickerHeartbeatWriterAlive() })
		}
	}
	switch {
	case host != nil:
		say(w, "  Scheduler host: "+host.Describe())
		hostProfile := "default"
		suppressPanic(func() {
			if label, ok, err := gateway.HostMultiplexerServing(ctx, active); err == nil && ok {
				hostProfile = label
			}
		})
		s.printTickerHealth(w, []int{host.PID}, fmt.Sprintf("icebath --profile %s gateway restart", hostProfile))
	case viaMultiplexer:
		say(w, "  Scheduler host: the host gateway (multiplexing this profile)")
		s.printTickerHealth(w, nil, "icebath --profile default gateway restart")
	case inProcess:
		say(w, fmt.Sprintf("  Scheduler host: an in-process ticker (icebath serve / Desktop backend) ticking profile '%s'", active))
		s.printTickerHealth(w, nil, "restart the icebath Desktop app (or its serve backend)")
	case len(pids) > 0 || viaLock:
		s.printTickerHealth(w, pids, "icebath gateway restart")
	default:
		say(w, color(fmt.Sprintf("✗ No scheduler is serving profile '%s' — its cron jobs will NOT fire", active), cliui.Red))
		if hb, ok := s.TickerHeartbeatAge(); ok && hb > TickerIntervalSeconds*3+20 {
			say(w, color(fmt.Sprintf("  Scheduler last ticked %s ago — jobs that came due since then have not fired.", formatLateness(hb)), cliui.Yellow))
		}
		say(w, "\n  Start the ONE host gateway (it multiplexes every profile, this one included):\n"+
			"    icebath --profile default gateway install   # user service\n"+
			"    sudo icebath --profile default gateway install --system  # Linux servers: boot-time service\n"+
			"    icebath --profile default gateway run       # Or run in foreground")
		if active != "default" && active != "custom" {
			say(w, "\n  It serves this profile automatically. If a per-profile service or gateway\n"+
				"  from an older release is still installed, fold it in (preflight + dry run):\n"+
				"      icebath --profile default gateway migrate --multiplex --dry-run\n"+
				"      icebath --profile default gateway migrate --multiplex\n"+
				"  Check: icebath cron status from this profile should show its ticker heartbeat.\n")
		}
	}
	return nil
}

// Active job count with the earliest next run and the jobs that last fired late
func (s *Store) printActiveJobsSummary(w io.Writer, jobs []listedJob) {
	if len(jobs) == 0 {
		say(w, "  No active jobs")
		return
	}
	type run struct {
		at  time.Time
		raw any
	}
	var runs []run
	for _, j := range jobs {
		raw := field(j.obj, "next_run_at", nil)
		if text, ok := raw.(string); ok {
			if parsed, ok := s.clk.parseAware(text, nil); ok {
				runs = append(runs, run{parsed.UTC(), raw})
			}
		}
	}
	fmt.Fprintf(w, "  %d active job(s)\n", len(jobs))
	if len(runs) > 0 {
		sort.SliceStable(runs, func(a, b int) bool { return runs[a].at.Before(runs[b].at) })
		earliest := runs[0].raw
		if over, ok := s.nextRunOverdueSeconds(earliest); ok && over > overdueGraceSeconds {
			say(w, color(fmt.Sprintf("  ⚠ Next run %s is OVERDUE — passed %s ago but the job has not fired (is the scheduler running?)", pyrepr.Str(earliest), formatLateness(over)), cliui.Yellow))
		} else {
			say(w, "  Next run: "+pyrepr.Str(earliest))
		}
	}
	var late []*pyjson.Object
	for _, j := range jobs {
		if d, ok := field(j.obj, "last_dispatch", nil).(*pyjson.Object); ok {
			if k := field(d, "kind", nil); k == "late" || k == "catch_up" {
				late = append(late, j.obj)
			}
		}
	}
	if len(late) > 0 {
		fmt.Fprintln(w)
		say(w, color(fmt.Sprintf("  ⚠ %d job(s) last fired late:", len(late)), cliui.Yellow))
		for _, j := range late {
			d := field(j, "last_dispatch", nil).(*pyjson.Object)
			lateBy := formatLateness(numberOf(field(d, "lateness_seconds", 0)))
			say(w, fmt.Sprintf("    %s  %s: %s, scheduled %s, ran %s ", pyrepr.Str(field(j, "id", "?")), pyrepr.Str(field(j, "name", "(unnamed)")), dispatchKindLabelOrNone(field(d, "kind", nil)),
				pyrepr.Str(field(d, "scheduled_at", "?")), pyrepr.Str(field(d, "dispatched_at", "?")))+color("("+lateBy+" late)", cliui.Yellow))
		}
	}
}

func dispatchKindLabelOrNone(kind any) string {
	if l := dispatchKindLabel(kind); l != "" {
		return l
	}
	return "None"
}

func (s *Store) scriptsDirForCron() string { return filepath.Join(filepath.Dir(s.cronDir), "scripts") }

// Readable problem with a job script path, empty when the path is fine
func (s *Store) scriptHealthIssue(script string) (string, error) {
	scriptsDir, err := pypath.Resolve(s.scriptsDirForCron())
	if err != nil {
		return "", err
	}
	raw := pypath.Expanduser(script)
	p := raw
	if !filepath.IsAbs(raw) {
		p = filepath.Join(scriptsDir, raw)
	}
	if p, err = pypath.Resolve(p); err != nil {
		return "", err
	}
	if rel, err := filepath.Rel(scriptsDir, p); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Sprintf("script resolves outside %s: %s", scriptsDir, pyrepr.StrRepr(script)), nil
	}
	info, err := os.Stat(p)
	if err != nil {
		return "script not found: " + p, nil
	}
	if !info.Mode().IsRegular() {
		return "script path is not a file: " + p, nil
	}
	return "", nil
}

// Issue text when next_run_at is parked in the past beyond the grace
func (s *Store) nextRunOverdueIssue(nextRun string) string {
	over, ok := s.nextRunOverdueSeconds(nextRun)
	if !ok {
		return "next_run_at is not a valid timestamp: " + pyrepr.StrRepr(nextRun)
	}
	if over <= overdueGraceSeconds {
		return ""
	}
	amount, _ := pyrepr.Format(over/60, ".0f")
	amount += "m"
	if over >= 3600 {
		amount, _ = pyrepr.Format(over/3600, ".1f")
		amount += "h"
	}
	return "next_run_at is " + amount + " overdue — job is not firing (is the scheduler running?)"
}

func strField(job *pyjson.Object, key string) string {
	if v := field(job, key, nil); pyrepr.Truthy(v) {
		return pystr.Strip(pyrepr.Str(v))
	}
	return ""
}

// Read only health findings of one job
func (s *Store) cronDoctorIssuesForJob(job *pyjson.Object) ([]string, error) {
	var issues []string
	if last := pystr.Lower(strField(job, "last_status")); last != "" && last != "ok" && last != "delivery_failed" && last != "delivery_queued" {
		e := "unknown error"
		if v := field(job, "last_error", nil); pyrepr.Truthy(v) {
			e = pyrepr.Str(v)
		}
		issues = append(issues, "last run failed: "+pystr.Strip(e))
	}
	if de := strField(job, "last_delivery_error"); de != "" {
		issues = append(issues, fmt.Sprintf("last run finished but the result was not delivered (%s). %s", shortReason(de, 120), deliveryFixHint(job)))
	}
	if u := field(job, "last_delivery_unverified", nil); pyrepr.Truthy(u) {
		issues = append(issues, "last delivery unverified (adapter acked without evidence): "+unverifiedTargets(u))
	}
	if d, ok := field(job, "last_dispatch", nil).(*pyjson.Object); ok {
		if label := dispatchKindLabel(field(d, "kind", nil)); label != "" {
			issues = append(issues, fmt.Sprintf("last fire was %s (scheduled %s, %s late). This warning clears at the next on-time fire.",
				label, pyrepr.Str(field(d, "scheduled_at", "?")), formatLateness(numberOf(field(d, "lateness_seconds", 0)))))
		}
	}
	if fe, ok := field(job, "last_fire_error", nil).(*pyjson.Object); ok && pyrepr.Truthy(field(fe, "detail", nil)) {
		issues = append(issues, missedFireIssue(job, fe))
	}
	if st := field(job, "state", nil); enabledOf(job) && st != "paused" && st != "completed" {
		issue := "active job has no next_run_at"
		if next := strField(job, "next_run_at"); next != "" {
			issue = s.nextRunOverdueIssue(next)
		}
		if issue != "" {
			issues = append(issues, issue)
		}
	}
	script := strField(job, "script")
	if pyrepr.Truthy(field(job, "no_agent", nil)) && script == "" {
		issues = append(issues, "no-agent job has no script")
	}
	if script != "" {
		issue, err := s.scriptHealthIssue(script)
		if err != nil {
			return nil, err
		}
		if issue != "" {
			issues = append(issues, issue)
		}
	}
	if wd := strField(job, "workdir"); wd != "" {
		if _, err := os.Lstat(pypath.Expanduser(wd)); err != nil {
			if _, err := os.Stat(pypath.Expanduser(wd)); err != nil {
				issues = append(issues, "workdir not found: "+wd)
			}
		}
	}
	if tz := field(job, "timezone", nil); pyrepr.Truthy(tz) {
		name := pyrepr.Str(tz)
		if s.knobs.zoneForJob(&name) == nil {
			issues = append(issues, fmt.Sprintf("timezone %s is not a valid IANA zone; the job falls back to the profile timezone", pyrepr.Repr(tz)))
		}
	}
	if g := field(job, "grace_minutes", nil); g != nil {
		if _, err := NormalizeGraceMinutes(g); err != nil {
			issues = append(issues, fmt.Sprintf("grace_minutes is unusable (%s)", err.Error()))
		} else if field(objOrEmpty(field(job, "schedule", nil)), "kind", nil) != "cron" {
			issues = append(issues, "grace_minutes has no effect on a non-cron schedule")
		}
	}
	return issues, nil
}

// Read only cron health checks with a shell friendly status
func (s *Store) cronDoctor(ctx context.Context, w io.Writer) (int, error) {
	jobs, err := s.listJobs(home.WithOverride(ctx, s.home), false)
	if err != nil {
		return 0, err
	}
	type finding struct {
		job    *pyjson.Object
		issues []string
	}
	var findings []finding
	total := 0
	for _, j := range jobs {
		obj, err := listedJobObject(j)
		if err != nil {
			return 0, err
		}
		issues, err := s.cronDoctorIssuesForJob(obj)
		if err != nil {
			return 0, err
		}
		if len(issues) > 0 {
			findings = append(findings, finding{obj, issues})
			total += len(issues)
		}
	}
	if len(findings) == 0 {
		say(w, color("✓ Cron doctor found no issues", cliui.Green))
		note := "  No active jobs configured."
		if len(jobs) > 0 {
			note = fmt.Sprintf("  Checked %d active job(s).", len(jobs))
		}
		say(w, color(note, cliui.Dim))
		return 0, nil
	}
	say(w, color(fmt.Sprintf("Cron doctor found %d issue(s) across %d job(s):", total, len(findings)), cliui.Yellow))
	fmt.Fprintln(w)
	for _, f := range findings {
		fmt.Fprintf(w, "  %s %s\n", color(pyrepr.Str(field(f.job, "id", "?")), cliui.Yellow), pyrepr.Str(field(f.job, "name", "(unnamed)")))
		for _, issue := range f.issues {
			fmt.Fprintf(w, "    - %s\n", issue)
		}
	}
	fmt.Fprintln(w)
	say(w, color("Review the findings above, then run `icebath cron doctor` again.", cliui.Dim))
	return 1, nil
}

// Shared create and update keyword arguments of cron create and cron edit
func jobAPIKwargs(a *CronArgs, c *cronjobArgs) {
	c.Name, c.Deliver, c.FailureDeliver, c.Repeat, c.Script, c.Workdir = a.Name, a.Deliver, a.FailureDeliver, a.Repeat, a.Script, a.Workdir
	c.Model, c.Provider, c.Pinned, c.MonitorScript, c.MonitorURL = a.Model, a.ModelProvider, a.Pinned, a.MonitorScript, a.MonitorURL
	c.Continuity, c.ReasoningEffort, c.Interpreter, c.Timezone, c.GraceMinutes, c.CatchUp = a.Continuity, a.ReasoningEffort, a.Interpreter, a.Timezone, a.GraceMinutes, a.CatchUp
}

var jobDetailLines = [][2]string{
	{"script", "  Script: {}"},
	{"monitor_script", "  Monitor: {} (agent runs only on output change)"},
	{"monitor_url", "  Monitor: {} (agent runs only on output change)"},
	{"no_agent", "  Mode: no-agent (script stdout delivered directly)"},
	{"continuity", "  Continuity: on (each run sees the previous run's output)"},
	{"timezone", "  Timezone: {} (schedule runs on this zone's wall clock)"},
	{"workdir", "  Workdir: {}"},
	{"interpreter", "  Python: {}"},
}

func printJobDetails(w io.Writer, job *pyjson.Object) {
	for _, d := range jobDetailLines {
		if v := field(job, d[0], nil); pyrepr.Truthy(v) {
			say(w, strings.Replace(d[1], "{}", pyrepr.Str(v), 1))
		}
	}
}

func skillsOrNil(v []string) any {
	if v == nil {
		return nil
	}
	return stringsToAny(v)
}

func joinedStrings(v any) string {
	list, _ := v.([]any)
	parts := make([]string, len(list))
	for i, x := range list {
		parts[i] = pyrepr.Str(x)
	}
	return strings.Join(parts, ", ")
}

// Creates a job through the cronjob tool and prints its summary
func (s *Store) cronCreate(ctx context.Context, w io.Writer, a *CronArgs) int {
	c := &cronjobArgs{Action: "create", Schedule: strOrNil(a.Schedule), Prompt: strOrNil(a.Prompt), Skill: strOrNil(a.Skill), Skills: skillsOrNil(normalizeSkillArgs(a.Skill, a.Skills))}
	if a.NoAgent != nil && *a.NoAgent {
		c.NoAgent = true
	}
	if a.Paused || a.PausedReason != nil {
		c.Paused, c.PausedReason = a.Paused, strOrNil(a.PausedReason)
	}
	jobAPIKwargs(a, c)
	result := s.cronAPI(ctx, c)
	if !pyrepr.Truthy(field(result, "success", nil)) {
		say(w, color("Failed to create job: "+pyrepr.Str(field(result, "error", "unknown error")), cliui.Red))
		return 1
	}
	say(w, color("Created job: "+pyrepr.Str(field(result, "job_id", nil)), cliui.Green))
	say(w, fmt.Sprintf("  Name: %s\n  Schedule: %s", pyrepr.Str(field(result, "name", nil)), pyrepr.Str(field(result, "schedule", nil))))
	if sk := field(result, "skills", nil); pyrepr.Truthy(sk) {
		say(w, "  Skills: "+joinedStrings(sk))
	}
	job, _ := field(result, "job", nil).(*pyjson.Object)
	if job == nil {
		job = pyjson.NewObject()
	}
	printJobDetails(w, job)
	if !enabledOf(job) {
		say(w, "  Created PAUSED — resume to schedule, or explicitly run now.")
	} else {
		say(w, "  Next run: "+pyrepr.Str(field(result, "next_run_at", nil)))
	}
	s.warnIfGatewayNotRunning(ctx, w)
	return 0
}

func strOrNil(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// Updates a job through the cronjob tool, resolving skill replacement, additions and removals
func (s *Store) cronEdit(ctx context.Context, w io.Writer, a *CronArgs) (int, error) {
	job, err := s.resolveJobRef(home.WithOverride(ctx, s.home), a.JobID)
	var amb *AmbiguousJobReferenceError
	if errors.As(err, &amb) {
		say(w, color(amb.Error(), cliui.Red))
		for _, m := range amb.Matches {
			fmt.Fprintf(w, "  %s  (name: %s)\n", m.ID, pyrepr.StrRepr(m.Name))
		}
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	if job == nil {
		say(w, color("Job not found: "+a.JobID, cliui.Red))
		return 1, nil
	}
	existing := append([]string(nil), job.Skills...)
	if len(existing) == 0 && job.Skill != nil && *job.Skill != "" {
		existing = []string{*job.Skill}
	}
	replacement := normalizeSkillArgs(a.Skill, a.Skills)
	add := normalizeSkillArgs(nil, a.AddSkills)
	remove := normalizeSkillArgs(nil, a.RemoveSkills)
	var final any
	switch {
	case a.ClearSkills:
		final = []any{}
	case replacement != nil:
		final = stringsToAny(replacement)
	case len(add) > 0 || len(remove) > 0:
		kept := []string{}
		for _, sk := range existing {
			if !containsString(remove, sk) {
				kept = append(kept, sk)
			}
		}
		for _, sk := range add {
			if !containsString(kept, sk) {
				kept = append(kept, sk)
			}
		}
		final = stringsToAny(kept)
	}
	c := &cronjobArgs{Action: "update", JobID: a.JobID, Schedule: strOrNil(a.Schedule), Prompt: strOrNil(a.Prompt), Skills: final}
	if a.NoAgent != nil {
		c.NoAgent = *a.NoAgent
	}
	jobAPIKwargs(a, c)
	result := s.cronAPI(ctx, c)
	if !pyrepr.Truthy(field(result, "success", nil)) {
		say(w, color("Failed to update job: "+pyrepr.Str(field(result, "error", "unknown error")), cliui.Red))
		return 1, nil
	}
	updated, _ := field(result, "job", nil).(*pyjson.Object)
	say(w, color("Updated job: "+pyrepr.Str(field(updated, "job_id", nil)), cliui.Green))
	say(w, fmt.Sprintf("  Name: %s\n  Schedule: %s", pyrepr.Str(field(updated, "name", nil)), pyrepr.Str(field(updated, "schedule", nil))))
	if sk := field(updated, "skills", nil); pyrepr.Truthy(sk) {
		say(w, "  Skills: "+joinedStrings(sk))
	} else {
		say(w, "  Skills: none")
	}
	printJobDetails(w, updated)
	return 0, nil
}

// Pause, resume, run or remove through the cronjob tool, a run without async session delivery
func (s *Store) jobAction(ctx context.Context, w io.Writer, action, jobID, verb string) int {
	c := &cronjobArgs{Action: action, JobID: jobID}
	if action == "run" {
		c.env = &tools.Env{Values: &sync.Map{}}
		tools.DeclareStatelessChannel(c.env)
	}
	result := s.cronAPI(ctx, c)
	if !pyrepr.Truthy(field(result, "success", nil)) {
		say(w, color(fmt.Sprintf("Failed to %s job: %s", action, pyrepr.Str(field(result, "error", "unknown error"))), cliui.Red))
		return 1
	}
	job, _ := field(result, "job", nil).(*pyjson.Object)
	if job == nil || job.Len() == 0 {
		job, _ = field(result, "removed_job", nil).(*pyjson.Object)
	}
	if job == nil {
		job = pyjson.NewObject()
	}
	say(w, color(fmt.Sprintf("%s job: %s (%s)", verb, pyrepr.Str(field(job, "name", jobID)), jobID), cliui.Green))
	resJob, _ := field(result, "job", nil).(*pyjson.Object)
	if resJob == nil {
		resJob = pyjson.NewObject()
	}
	if (action == "resume" || action == "run") && pyrepr.Truthy(field(resJob, "next_run_at", nil)) {
		say(w, "  Next run: "+pyrepr.Str(field(resJob, "next_run_at", nil)))
	}
	if action == "run" {
		say(w, "  "+runOutcome(resJob))
		if pyrepr.Truthy(field(job, "executed", nil)) && field(job, "execution_success", nil) == false && field(job, "last_status", nil) != "cancelled" &&
			!pyrepr.Truthy(field(job, "delegation_id", nil)) && field(job, "execution_mode", nil) != "background" {
			return 1
		}
	}
	return 0
}

// Resumes a paused job or re-arms a completed one shot at a time or now
func (s *Store) cronResume(ctx context.Context, w io.Writer, a *CronArgs) (int, error) {
	runAt := ""
	if a.RunAt != nil {
		runAt = *a.RunAt
	}
	if runAt != "" && a.RunNow {
		say(w, color("Use exactly one of --at or --run-now.", cliui.Red))
		return 1, nil
	}
	if runAt == "" && !a.RunNow {
		return s.jobAction(ctx, w, "resume", a.JobID, "Resumed"), nil
	}
	when := runAt
	if a.RunNow {
		when = s.clk.nowISO()
	}
	job, err := cliRearmOneshot(s, ctx, a.JobID, when)
	var amb *AmbiguousJobReferenceError
	var bad *ValueError
	if errors.As(err, &amb) || errors.As(err, &bad) {
		say(w, color("Failed to re-arm job: "+err.Error(), cliui.Red))
		return 1, nil
	}
	if err != nil {
		return 1, err
	}
	if job == nil {
		say(w, color("Job not found: "+a.JobID, cliui.Red))
		return 1, nil
	}
	name := job.Name
	next := "None"
	if job.NextRunAt != nil {
		next = *job.NextRunAt
	}
	say(w, color(fmt.Sprintf("Re-armed job: %s (%s)", name, a.JobID), cliui.Green)+"\n  Next run: "+next)
	return 0, nil
}

// Lists, reads, sets or deletes a job notepad entry
func (s *Store) cronNotepad(w io.Writer, a *CronArgs) (int, error) {
	n := openNotepad(s)
	action := a.NotepadAction
	if action == "" {
		action = "list"
	}
	if a.JobID == "" {
		say(w, color("A job ID is required.", cliui.Red))
		return 1, nil
	}
	fail := func(err error) (int, error) {
		var bad *NoteRejectedError
		if !errors.As(err, &bad) {
			return 1, err
		}
		say(w, color("Notepad error: "+err.Error(), cliui.Red))
		return 1, nil
	}
	if action != "set" && action != "get" && action != "delete" {
		notes, err := n.List(a.JobID)
		if err != nil {
			return fail(err)
		}
		if len(notes) == 0 {
			say(w, color(fmt.Sprintf("Notepad for job %s is empty.", a.JobID), cliui.Dim))
		}
		for _, note := range notes {
			say(w, fmt.Sprintf("  %s = %s\n    %s", color(pyrepr.Str(field(note, "key", nil)), cliui.Yellow), pyrepr.Str(field(note, "value", nil)),
				color("updated: "+pyrepr.Str(field(note, "updated_at", nil)), cliui.Dim)))
		}
		return 0, nil
	}
	usage := action + " <key>"
	if action == "set" {
		usage = "set <key> <value>"
	}
	if a.Key == nil || (action == "set" && a.Value == nil) {
		say(w, color("Usage: icebath cron notepad <job_id> "+usage, cliui.Red))
		return 1, nil
	}
	key := *a.Key
	switch action {
	case "set":
		if _, err := n.Set(a.JobID, key, *a.Value); err != nil {
			return fail(err)
		}
		say(w, color(fmt.Sprintf("Set notepad key '%s' for job %s.", key, a.JobID), cliui.Green))
		return 0, nil
	case "get":
		stored, err := n.Get(a.JobID, key)
		if err != nil {
			return fail(err)
		}
		if stored != nil {
			say(w, *stored)
			return 0, nil
		}
	default:
		ok, err := n.Delete(a.JobID, key)
		if err != nil {
			return fail(err)
		}
		if ok {
			say(w, color(fmt.Sprintf("Deleted notepad key '%s' for job %s.", key, a.JobID), cliui.Green))
			return 0, nil
		}
	}
	say(w, color(fmt.Sprintf("No notepad key '%s' for job %s.", key, a.JobID), cliui.Yellow))
	return 1, nil
}

// Stops the live run of a job leaving its schedule untouched
func (s *Store) cronCancel(ctx context.Context, w io.Writer, a *CronArgs) int {
	result := s.cronAPI(ctx, &cronjobArgs{Action: "cancel", JobID: a.JobID, Reason: strOrNil(a.Reason)})
	if !pyrepr.Truthy(field(result, "success", nil)) {
		say(w, color("Failed to cancel run: "+pyrepr.Str(field(result, "error", "unknown error")), cliui.Red))
		return 1
	}
	job, _ := field(result, "job", nil).(*pyjson.Object)
	if job == nil {
		job = pyjson.NewObject()
	}
	label := fmt.Sprintf("%s (%s)", pyrepr.Str(field(job, "name", a.JobID)), a.JobID)
	if !pyrepr.Truthy(field(result, "cancelled", nil)) {
		say(w, color("Nothing to cancel for "+label, cliui.Yellow))
		return 0
	}
	say(w, color("Cancel requested for "+label, cliui.Green), fmt.Sprintf("  Execution: %s (pid %s)", pyrepr.Str(field(result, "execution_id", nil)), pyrepr.Str(field(result, "pid", nil))),
		"  "+pyrepr.Str(field(result, "detail", "")))
	return 0
}

type cronSubcommand func(ctx context.Context, s *Store, w io.Writer, a *CronArgs) (int, error)

func noErr(code int) (int, error) { return code, nil }

var cronSubcommands = map[string]cronSubcommand{
	"list": func(ctx context.Context, s *Store, w io.Writer, a *CronArgs) (int, error) {
		return 0, s.cronList(ctx, w, a.All)
	},
	"status": func(ctx context.Context, s *Store, w io.Writer, a *CronArgs) (int, error) {
		return 0, s.cronStatus(ctx, w)
	},
	"doctor": func(ctx context.Context, s *Store, w io.Writer, a *CronArgs) (int, error) {
		return s.cronDoctor(ctx, w)
	},
	"tick": func(ctx context.Context, s *Store, w io.Writer, a *CronArgs) (int, error) { return s.cronTick(ctx, w) },
	"runs": func(ctx context.Context, s *Store, w io.Writer, a *CronArgs) (int, error) {
		limit := 20
		if a.Limit != nil {
			limit = *a.Limit
		}
		var jobID *string
		if a.JobID != "" {
			jobID = &a.JobID
		}
		return 0, s.cronRuns(ctx, w, jobID, limit)
	},
	"incidents": func(ctx context.Context, s *Store, w io.Writer, a *CronArgs) (int, error) {
		return s.cronIncidents(ctx, w, a)
	},
	"notepad": func(ctx context.Context, s *Store, w io.Writer, a *CronArgs) (int, error) {
		return s.cronNotepad(w, a)
	},
	"create": func(ctx context.Context, s *Store, w io.Writer, a *CronArgs) (int, error) {
		return noErr(s.cronCreate(ctx, w, a))
	},
	"edit": func(ctx context.Context, s *Store, w io.Writer, a *CronArgs) (int, error) {
		return s.cronEdit(ctx, w, a)
	},
	"pause": func(ctx context.Context, s *Store, w io.Writer, a *CronArgs) (int, error) {
		return noErr(s.jobAction(ctx, w, "pause", a.JobID, "Paused"))
	},
	"resume": func(ctx context.Context, s *Store, w io.Writer, a *CronArgs) (int, error) {
		return s.cronResume(ctx, w, a)
	},
	"run": func(ctx context.Context, s *Store, w io.Writer, a *CronArgs) (int, error) {
		return noErr(s.jobAction(ctx, w, "run", a.JobID, "Triggered"))
	},
	"cancel": func(ctx context.Context, s *Store, w io.Writer, a *CronArgs) (int, error) {
		return noErr(s.cronCancel(ctx, w, a))
	},
	"remove": func(ctx context.Context, s *Store, w io.Writer, a *CronArgs) (int, error) {
		return noErr(s.jobAction(ctx, w, "remove", a.JobID, "Removed"))
	},
}

func init() {
	cronSubcommands["history"] = cronSubcommands["runs"]
	cronSubcommands["add"] = cronSubcommands["create"]
	cronSubcommands["rm"] = cronSubcommands["remove"]
	cronSubcommands["delete"] = cronSubcommands["remove"]
}

// UnknownCronCommandError is the usage exit of an unknown cron subcommand, sys.exit(1) in hermes
type UnknownCronCommandError struct{ Command string }

func (e *UnknownCronCommandError) Error() string { return "Unknown cron command: " + e.Command }

// The cron.jobs rearm_oneshot attribute cron_resume calls, patched by tests
var cliRearmOneshot = (*Store).rearmOneshot

// Dispatches one icebath cron subcommand for the home, returning its exit status
func CronCommand(ctx context.Context, h string, a *CronArgs, w io.Writer) (int, error) {
	return openCronStore(h, nil).cronCommand(ctx, a, w)
}

func (s *Store) cronCommand(ctx context.Context, a *CronArgs, w io.Writer) (int, error) {
	sub := a.Command
	if sub == "" {
		sub = "list"
	}
	if handler, ok := cronSubcommands[sub]; ok {
		return handler(ctx, s, w, a)
	}
	say(w, "Unknown cron command: "+a.Command+"\nUsage: icebath cron [list|create|edit|pause|resume|run|cancel|remove|status|runs|doctor|tick]")
	return 1, &UnknownCronCommandError{Command: a.Command}
}

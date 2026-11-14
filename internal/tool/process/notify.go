package process

import (
	"context"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	DisplayProcessComplete    = "process_complete"
	DisplayHeartbeat          = "hidden"
	DisplayDelegationComplete = "async_delegation_complete"
	CategoryResult            = "result"
	CategoryDiagnostic        = "diagnostic"
)

const truncatedSummaryNote = "[TRUNCATED — subagent hit its iteration cap; the summary below " +
	"may be incomplete. Verify before relying on it, or re-dispatch " +
	"the unfinished part.]"

var modelNotFoundPhrases = []string{
	"is not a valid model", "invalid model", "model not found", "model_not_found", "does not exist",
	"no such model", "unknown model", "unsupported model", "no endpoints found that support tool use",
	"is no longer free",
}

var reasonStatus = map[string]string{
	"lost":         "marked lost because the process backend disappeared",
	"failed_start": "failed to start",
}

func doneStatus(status string) bool { return status == "completed" || status == "success" }

func formatAge(seconds float64) string {
	s := int(max(0, seconds))
	if s < 60 {
		return strconv.Itoa(s) + "s"
	}
	m, s := s/60, s%60
	if m < 60 {
		out := strconv.Itoa(m) + "m"
		if s != 0 {
			out += strconv.Itoa(s) + "s"
		}
		return out
	}
	h, m := m/60, m%60
	out := strconv.Itoa(h) + "h"
	if m != 0 {
		out += strconv.Itoa(m) + "m"
	}
	return out
}

func exitText(code *int) string {
	if code == nil {
		return "None"
	}
	return strconv.Itoa(*code)
}

func secondsText(v *float64) string {
	if v == nil {
		return "?"
	}
	return formatSeconds(*v)
}

func localStamp(unix float64) string {
	sec := int64(unix)
	return time.Unix(sec, int64((unix-float64(sec))*1e9)).Local().Format("2006-01-02 15:04:05")
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func (r *Registry) Render(ctx context.Context, ev Event) string {
	typ := ev.Type
	if typ == "" {
		typ = EventCompletion
	}
	switch typ {
	case EventWatchDisabled, EventOverflowTripped, EventOverflowReleased:
		return "[IMPORTANT: " + ev.Message + "]"
	case EventAsyncDelegation:
		return r.renderDelegation(ctx, ev)
	}
	sid, cmd := orUnknown(ev.SessionID), orUnknown(ev.Command)
	attr, attributed := r.attributionLine(ev)
	if ev.HandoffNote != "" {
		attr, attributed = "Handed off to you by a subagent before it finished. Purpose: "+ev.HandoffNote, true
	}
	attribution := ""
	if attributed {
		attribution = attr + "\n"
	}
	switch typ {
	case EventHeartbeat:
		return "[Background process " + sid + " heartbeat #" + strconv.Itoa(ev.Seq) + " — still running after " +
			formatAge(float64(ev.Elapsed)) + " (next in " + strconv.Itoa(ev.Interval) + "s when there " +
			"is new output; you will also be told when it exits).\n" +
			attribution + "Command: " + cmd + "\nOutput since last heartbeat:\n" + HeartbeatOutputText(ev) + "]"
	case EventWatchMatch:
		pattern := ev.Pattern
		if pattern == "" {
			pattern = "?"
		}
		text := "[IMPORTANT: Background process " + sid + " matched watch pattern \"" + pattern + "\".\n" +
			attribution + "Command: " + cmd + "\nMatched output:\n" + ev.Output
		if ev.Suppressed != 0 {
			text += "\n(" + strconv.Itoa(ev.Suppressed) + " earlier matches were suppressed by rate limit)"
		}
		return text + "]"
	}
	signal := ""
	if ev.ExitCode != nil && (*ev.ExitCode == -15 || *ev.ExitCode == 143) {
		signal = ", SIGTERM"
	}
	out := ev.Output
	if attributed && utf8.RuneCountInString(out) > 600 {
		out = "...(output trimmed — subagent-owned process; see the " +
			"delegation's live transcript for full output)\n" + runeTail(out, 600)
	} else if ev.OutputCut != 0 {
		out = "...(first " + strconv.Itoa(ev.OutputCut) + " characters cut — process(action=\"log\", " +
			"session_id=\"" + sid + "\") has the full output)\n" + out
	}
	return "[IMPORTANT: Background process " + sid + " " + completionStatus(ev) + " (exit code " + exitText(ev.ExitCode) +
		signal + ").\n" + attribution + "Command: " + cmd + "\nOutput:\n" + out + "]"
}

func completionStatus(ev Event) string {
	reason := ev.CompletionReason
	if reason == "" {
		reason = reasonExited
	}
	if reason == reasonKilled {
		source := ev.TerminationSource
		if source == "" {
			source = "icebath"
		}
		return "terminated by " + source
	}
	if status, ok := reasonStatus[reason]; ok {
		return status
	}
	if ev.ExitCode != nil && *ev.ExitCode == 0 {
		return "completed normally"
	}
	return "exited"
}

func (r *Registry) attributionLine(ev Event) (string, bool) {
	taskID := ev.spawner()
	if !strings.HasPrefix(taskID, "sa-") {
		return "", false
	}
	info, ok := r.delegation.Attribution(taskID)
	if !ok {
		return "Started by subagent " + taskID + " (delegate_task).", true
	}
	goal := strings.TrimSpace(info.Goal)
	if utf8.RuneCountInString(goal) > 120 {
		goal = runeHead(goal, 117) + "..."
	}
	line := "Started by subagent " + taskID
	if info.DelegationID != "" {
		line += " of delegation " + info.DelegationID
	}
	line += "."
	if goal != "" {
		line += " Task: \"" + goal + "\""
	}
	return line, true
}

func HeartbeatOutputTail(output string, omitted int) (string, int) {
	if n := utf8.RuneCountInString(output); n > heartbeatOutputChars {
		omitted += n - heartbeatOutputChars
		output = runeTail(output, heartbeatOutputChars)
	}
	return output, omitted
}

func JoinHeartbeatOutputs(earlier, later Event) (string, int) {
	if later.Omitted != 0 {
		return HeartbeatOutputTail(later.Output, earlier.Omitted+utf8.RuneCountInString(earlier.Output)+later.Omitted)
	}
	return HeartbeatOutputTail(earlier.Output+later.Output, earlier.Omitted)
}

func HeartbeatOutputText(ev Event) string {
	if ev.Omitted != 0 {
		return "...(" + strconv.Itoa(ev.Omitted) + " earlier characters omitted)\n" + ev.Output
	}
	return ev.Output
}

func shortCommand(command string) string {
	cmd := strings.Join(strings.Fields(command), " ")
	if utf8.RuneCountInString(cmd) > 80 {
		return runeHead(cmd, 77) + "..."
	}
	return cmd
}

func HeartbeatDisplayText(ev Event) string {
	cmd := shortCommand(ev.Command)
	age := formatAge(float64(ev.Elapsed))
	if cmd != "" {
		return "Background Process Output after " + age + ": " + cmd
	}
	return "Background Process Output after " + age
}

func ProcessCompletionDisplayText(events []Event) string {
	if len(events) != 1 {
		return strconv.Itoa(len(events)) + " Background Processes Finished"
	}
	ev := events[0]
	reason := ev.CompletionReason
	if reason == "" {
		reason = reasonExited
	}
	zero := ev.ExitCode != nil && *ev.ExitCode == 0
	_, special := reasonStatus[reason]
	var outcome string
	switch {
	case reason == reasonKilled:
		outcome = "Terminated"
	case reason == reasonLost:
		outcome = "Lost"
	case special:
		outcome = "Failed to Start"
	case zero:
		outcome = "Finished"
	default:
		outcome = "Failed"
	}
	detail := ""
	if reason != reasonKilled && !special && !zero {
		detail = " (exit " + exitText(ev.ExitCode) + ")"
	}
	cmd := shortCommand(ev.Command)
	if cmd != "" {
		return "Background Process " + outcome + detail + ": " + cmd
	}
	return "Background Process " + outcome + detail
}

type Timeline struct {
	Text     string
	Display  string
	Kind     string
	Category string
}

func DelegationTimeline(text string, ev Event) Timeline {
	category := CategoryResult
	if ev.Diagnostic() {
		category = CategoryDiagnostic
	}
	return Timeline{Text: text, Display: AsyncDelegationDisplayText(ev), Kind: DisplayDelegationComplete, Category: category}
}

type Batch []Notification

func GroupNotifications(ns []Notification) []Batch {
	var out []Batch
	var batch Batch
	for _, n := range ns {
		if n.Event.Type == "" || n.Event.Type == EventCompletion {
			batch = append(batch, n)
			continue
		}
		if len(batch) > 0 {
			out = append(out, batch)
			batch = nil
		}
		out = append(out, Batch{n})
	}
	if len(batch) > 0 {
		out = append(out, batch)
	}
	return out
}

func (b Batch) live(r *Registry) Batch {
	var out Batch
	for _, n := range b {
		if !r.IsConsumed(n.Event.SessionID) {
			out = append(out, n)
		}
	}
	return out
}

func (b Batch) Render(r *Registry) (string, bool) {
	live := b.live(r)
	switch len(live) {
	case 0:
		return "", false
	case 1:
		return live[0].Text, true
	}
	parts := []string{"[IMPORTANT: " + strconv.Itoa(len(live)) + " background processes completed. " +
		"Treat these results as one batch and give one consolidated response; " +
		"preserve failures and actionable results.]"}
	for _, n := range live {
		parts = append(parts, n.Text)
	}
	return strings.Join(parts, "\n\n"), true
}

func (b Batch) DisplayText(r *Registry) string {
	var events []Event
	for _, n := range b.live(r) {
		events = append(events, n.Event)
	}
	return ProcessCompletionDisplayText(events)
}

func truncatedResult(truncated bool, exitReason string) bool {
	return truncated || exitReason == "max_iterations"
}

func (r *Registry) delegationNotice(ctx context.Context, texts []string) []string {
	m, err := r.delegation.Model(ctx)
	if err != nil {
		slog.Debug("delegation config unreadable for the model rejection notice", "err", err)
		return nil
	}
	model := strings.ToLower(m.Model)
	if model == "" {
		return nil
	}
	rejected := false
	for _, text := range texts {
		text = strings.ToLower(text)
		if !strings.Contains(text, model) {
			continue
		}
		for _, p := range modelNotFoundPhrases {
			if strings.Contains(text, p) {
				rejected = true
				break
			}
		}
		if rejected {
			break
		}
	}
	if !rejected {
		return nil
	}
	name, provider := m.Model, m.Provider
	if provider == "" {
		provider = "configured provider"
	}
	lines := []string{
		"",
		"⚠ SUBAGENT MODEL REJECTED: the configured Subagent Model " +
			"\"" + name + "\" was rejected by provider \"" + provider + "\" " +
			"(HTTP 400: not a valid model ID).",
		"Every task in this batch failed for this reason before doing any work.",
		"Check Settings → Advanced → Subagent Model (or: icebath config get delegation.model).",
	}
	if !m.FallbackChain {
		lines = append(lines, "No fallback chain is configured, so no failover was attempted.")
	}
	return lines
}

func joinTruthy(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " ")
}

func resultTexts(results []DelegationResult) []string {
	out := make([]string, 0, len(results))
	for _, res := range results {
		out = append(out, joinTruthy(res.Error, res.Summary))
	}
	return out
}

func preamble(d *DelegationEvent, title, intro string, completedAt float64, withGoal bool) []string {
	lines := []string{title, intro, ""}
	if d.DispatchedAt != nil {
		lines = append(lines, "Dispatched: "+localStamp(*d.DispatchedAt)+" ("+formatAge(completedAt-*d.DispatchedAt)+" ago)")
	}
	if withGoal {
		lines = append(lines, "Original goal: "+d.Goal)
	}
	if d.Context != "" {
		lines = append(lines, "Context you provided: "+d.Context)
	}
	if len(d.Toolsets) > 0 {
		lines = append(lines, "Toolsets: "+strings.Join(d.Toolsets, ", "))
	}
	role := d.Role
	if role == "" {
		role = "leaf"
	}
	line := "Role: " + role
	if d.Model != "" {
		line += "   Model: " + d.Model
	}
	return append(lines, line)
}

func recoveryLines(d *DelegationEvent) []string {
	if d.LastKnownStatus == "" {
		return nil
	}
	lines := []string{"Last persisted unit status: " + d.LastKnownStatus + " (before owner exit; not current liveness). " +
		"Unrecorded outcomes remain unknown; inspect evidence before retrying side effects."}
	for _, t := range d.TaskTranscripts {
		lines = append(lines, "Task index "+t.Index+" transcript (may be incomplete): "+t.Path)
		if tail := d.TranscriptTails[t.Index]; tail != "" {
			lines = append(lines, "--- last lines of task "+t.Index+" transcript ---", tail, "--- end ---")
		}
	}
	if d.GitStateHint != "" {
		lines = append(lines, "Owner working tree at recovery: "+d.GitStateHint)
	}
	return lines
}

func goalFor(goals []string, idx int, fallback string) string {
	if idx >= 0 && idx < len(goals) {
		return goals[idx]
	}
	if idx < 0 && -idx <= len(goals) {
		return goals[len(goals)+idx]
	}
	return fallback
}

func (r *Registry) renderDelegation(ctx context.Context, ev Event) string {
	d := ev.Delegation
	if d == nil {
		d = &DelegationEvent{}
	}
	id := d.ID
	if id == "" {
		id = "unknown"
	}
	completedAt := d.CompletedAt
	if completedAt == 0 {
		completedAt = now()
	}
	if d.TaskFailureNotice {
		return taskFailureNotice(d, id)
	}
	if d.IsBatch || d.Results != nil {
		return r.batchDelegation(ctx, d, id, completedAt)
	}
	status := d.Status
	if status == "" {
		status = "completed"
	}
	truncated := truncatedResult(d.Truncated, d.ExitReason)
	lines := preamble(d, "[ASYNC DELEGATION COMPLETE — "+id+"]",
		"A background subagent you dispatched earlier has finished. You may "+
			"have moved on since dispatching it; the full task source is below so "+
			"you can act on the result or re-dispatch if things have changed.",
		completedAt, true)
	lines = append(lines, r.delegationNotice(ctx, []string{joinTruthy(d.Error, d.Summary)})...)
	head := "Status: " + status
	if d.APICalls != nil {
		head += "   API calls: " + strconv.Itoa(*d.APICalls)
	}
	head += "   Duration: " + secondsText(d.DurationSeconds) + "s"
	if truncated {
		head += " [TRUNCATED: hit max_iterations — work may be incomplete]"
	}
	lines = append(lines, head, "--- RESULT ---")
	if doneStatus(status) && d.Summary != "" {
		if truncated {
			lines = append(lines, truncatedSummaryNote)
		}
		lines = append(lines, d.Summary)
		return strings.Join(lines, "\n")
	}
	if status == "interrupted" {
		tail := "."
		if d.Error != "" {
			tail = ": " + d.Error
		}
		lines = append(lines, "The subagent was interrupted before completing"+tail)
	} else {
		line := "The subagent did not complete successfully (status=" + status + ")."
		if d.Error != "" {
			line += "\n" + d.Error
		}
		lines = append(lines, line)
		lines = append(lines, recoveryLines(d)...)
	}
	if d.Summary != "" {
		lines = append(lines, "Partial output:", d.Summary)
	}
	return strings.Join(lines, "\n")
}

func taskFailureNotice(d *DelegationEvent, id string) string {
	var res DelegationResult
	if len(d.Results) > 0 {
		res = d.Results[0]
	}
	n := d.NTasks
	if n == 0 {
		n = len(d.Goals)
	}
	goal := goalFor(d.Goals, res.TaskIndex, res.Goal)
	errText := strings.ReplaceAll(strings.TrimSpace(res.Error), "\n", " ")
	errText = runeHead(errText, 400)
	status := res.Status
	if status == "" {
		status = "?"
	}
	statusLine := "Status: " + status + "   Duration: " + secondsText(res.DurationSeconds) + "s"
	if errText != "" {
		statusLine += "\nError: " + errText
	}
	lines := []string{
		"[ASYNC DELEGATION TASK FAILED — " + id + ", task " + strconv.Itoa(res.TaskIndex+1) + "/" + strconv.Itoa(n) + "]",
		"One subagent in a background fan-out you dispatched has failed while its siblings are still running. " +
			"The batch's consolidated results will still arrive when the last sibling finishes; this is an early " +
			"warning so you can re-dispatch or investigate now instead of then.",
	}
	if goal != "" {
		lines = append(lines, "Task: "+goal)
	}
	lines = append(lines, statusLine)
	if res.LiveTranscript != "" {
		lines = append(lines, "Live transcript: "+res.LiveTranscript)
	}
	return strings.Join(lines, "\n")
}

func (r *Registry) batchDelegation(ctx context.Context, d *DelegationEvent, id string, completedAt float64) string {
	results, goals := d.Results, d.Goals
	n := len(goals)
	if n == 0 {
		n = len(results)
	}
	nUnit := len(results)
	if nUnit == 0 {
		nUnit = len(goals)
	}
	unit := strconv.Itoa(nUnit) + " subagent(s)"
	if d.Group != nil {
		unit = "group '" + *d.Group + "' (" + strconv.Itoa(nUnit) + " subagent(s))"
	}
	lines := preamble(d, "[ASYNC DELEGATION BATCH COMPLETE — "+id+"]",
		"A background fan-out unit you dispatched earlier — "+unit+" — has finished; its consolidated results are "+
			"below. Any other units from the same delegate_task call report separately as they finish. You may have "+
			"moved on since dispatching — act on these or re-dispatch if things have changed. If you are still waiting "+
			"on siblings, end your turn after acting on this one.",
		completedAt, false)
	total := d.TotalDurationSeconds
	if total == nil {
		total = d.DurationSeconds
	}
	lines[len(lines)-1] += "   Total duration: " + secondsText(total) + "s"
	lines = append(lines, recoveryLines(d)...)
	if d.Error != "" && len(results) == 0 {
		lines = append(lines, "--- ERROR ---", "The batch did not complete successfully: "+d.Error)
		return strings.Join(lines, "\n")
	}
	lines = append(lines, r.delegationNotice(ctx, resultTexts(results))...)
	sorted := slices.Clone(results)
	slices.SortStableFunc(sorted, func(a, b DelegationResult) int { return a.TaskIndex - b.TaskIndex })
	for _, res := range sorted {
		truncated := truncatedResult(res.Truncated, res.ExitReason)
		status := res.Status
		if status == "" {
			status = "?"
		}
		goal := goalFor(goals, res.TaskIndex, res.Goal)
		icon := "✗"
		if truncated {
			icon = "⚠"
		} else if doneStatus(status) {
			icon = "✓"
		}
		header := "--- " + icon + " TASK " + strconv.Itoa(res.TaskIndex+1) + "/" + strconv.Itoa(n)
		if goal != "" {
			header += ": " + goal
		}
		header += "  (status=" + status
		if res.APICalls != 0 {
			header += ", api_calls=" + strconv.Itoa(res.APICalls)
		}
		if res.DurationSeconds != nil {
			header += ", " + formatSeconds(*res.DurationSeconds) + "s"
		}
		if truncated {
			header += ", TRUNCATED: hit max_iterations — work may be incomplete"
		}
		lines = append(lines, "", header+") ---")
		switch {
		case doneStatus(status) && res.Summary != "":
			if truncated {
				lines = append(lines, truncatedSummaryNote)
			}
			lines = append(lines, res.Summary)
		case res.Summary != "":
			if res.Error != "" {
				lines = append(lines, "("+status+": "+res.Error+")")
			}
			lines = append(lines, "Partial output:", res.Summary)
		default:
			line := "(no summary — status=" + status
			if res.Error != "" {
				line += ": " + res.Error
			}
			lines = append(lines, line+")")
		}
		if res.LiveTranscript != "" {
			lines = append(lines, "Full live transcript (complete tool/assistant trace): "+res.LiveTranscript)
		}
		lines = append(lines, ProcessAccountingLines(res)...)
	}
	return strings.Join(lines, "\n")
}

func ProcessAccountingLines(res DelegationResult) []string {
	var lines []string
	for _, h := range res.HandedOff {
		lines = append(lines, "Handed off to you: "+h.SessionID+" ("+runeHead(h.Command, 120)+") — "+h.Note+". "+
			"You own it now; its completion notice will arrive here.")
	}
	if len(res.Orphaned) > 0 {
		parts := make([]string, 0, len(res.Orphaned))
		for _, o := range res.Orphaned {
			parts = append(parts, o.SessionID+" `"+runeHead(o.Command, 100)+"` ("+strconv.Itoa(o.RuntimeSeconds)+"s)")
		}
		lines = append(lines, "Child left "+strconv.Itoa(len(res.Orphaned))+" background process(es) running that were TERMINATED with it "+
			"(subagent process notices never reach you): "+strings.Join(parts, "; ")+
			". Re-launch in this session anything you still need.")
	}
	for _, u := range res.Unread {
		lines = append(lines, "Child's process "+u.SessionID+" `"+runeHead(u.Command, 100)+"` finished (exit code "+
			exitText(u.ExitCode)+") but the child never read its result; output tail:\n"+u.OutputTail)
	}
	return lines
}

var statusLabels = map[string]string{
	"failed": "Failed", "error": "Failed", "cancelled": "Cancelled",
	"interrupted": "Interrupted", "timeout": "Timed Out", "stalled": "Stalled",
	"unknown": "Unknown", "rejected": "Failed",
}

func shortTitle(title string) string {
	if utf8.RuneCountInString(title) > 200 {
		return runeHead(title, 197) + "..."
	}
	return title
}

func AsyncDelegationDisplayText(ev Event) string {
	d := ev.Delegation
	if d == nil {
		d = &DelegationEvent{}
	}
	results := d.Results
	if len(results) == 0 {
		results = []DelegationResult{{Status: d.Status, Error: d.Error, Goal: d.Goal, Truncated: d.Truncated, ExitReason: d.ExitReason}}
	}
	labels := make([]string, 0, len(results))
	titles := make([]string, 0, len(results))
	for _, res := range results {
		status := res.Status
		if status == "" {
			status = "completed"
			if res.Error != "" {
				status = "failed"
			}
		}
		var label string
		switch {
		case doneStatus(status) && truncatedResult(res.Truncated, res.ExitReason):
			label = "Incomplete"
		case doneStatus(status):
			label = "Completed"
		default:
			label = statusLabels[status]
			if label == "" {
				label = "Incomplete"
			}
		}
		labels = append(labels, label)
		goal := res.Goal
		if res.TaskIndex >= 0 && res.TaskIndex < len(d.Goals) {
			goal = d.Goals[res.TaskIndex]
		}
		if goal == "" {
			goal = "Background task"
		}
		titles = append(titles, strings.Join(strings.Fields(goal), " "))
	}
	if len(results) == 1 {
		return "Subagent Task " + labels[0] + ": " + shortTitle(titles[0])
	}
	outcome := labels[0]
	for _, l := range labels[1:] {
		if l != labels[0] {
			outcome = "Finished with Issues"
			break
		}
	}
	title := ""
	if d.Group != nil {
		title = strings.Join(strings.Fields(*d.Group), " ")
	}
	if title == "" {
		title = strings.Join(titles, "; ")
	}
	return "Subagent Tasks " + outcome + ": " + shortTitle(title) + " (" + strconv.Itoa(len(results)) + " tasks)"
}

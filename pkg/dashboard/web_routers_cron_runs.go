package dashboard

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	cronOutputFilenameFormat = "%Y-%m-%d_%H-%M-%S"
	cronSessionDocMatchSecs  = 300.0
)

// Labels whose row shows the attempt error as its reason
var cronLabelsWithReason = map[string]bool{"FAILED": true, "CANCELLED": true, "MISSED": true}

// Terminal attempt states that carry a real outcome
var cronOutcomeStates = map[string]bool{"completed": true, "failed": true, "cancelled": true, "missed": true}

// One terminal ledger attempt of a job
type cronAttempt struct {
	id         string
	claimedAt  float64
	finishedAt *float64
	status     string
	error      string
}

// Output doc dir of a job under its profile home, the current home when the profile does not resolve
func cronOutputRunsDir(ctx context.Context, profile, jobID string) string {
	profileHome := home.Dir(ctx)
	if profile != "" {
		if _, h, err := cronProfileHome(ctx, profile); err == nil {
			profileHome = h
		}
	}
	return filepath.Join(profileHome, "cron", "output", jobID)
}

// Epoch seconds of an output filename stem read in the configured zone, local time without one
func cronOutputRunTimestamp(ctx context.Context, path string) (float64, bool) {
	stem := pypath.PathStem(path)
	naive, _, err := pydatetime.Strptime(stem, cronOutputFilenameFormat)
	if err != nil {
		return 0, false
	}
	loc := time.Local
	if tz, ok := config.TimeZone(ctx); ok && tz != nil {
		loc = tz
	}
	t := time.Date(naive.Year(), naive.Month(), naive.Day(), naive.Hour(), naive.Minute(), naive.Second(), naive.Nanosecond(), loc)
	return float64(t.UnixNano()) / 1e9, true
}

// Whitespace collapsed opening of an output doc, cut with an ellipsis past maxChars
func cronOutputRunPreview(path string, maxChars int) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var b strings.Builder
	inSpace := false
	for _, r := range pystr.DecodeUTF8SigReplace(raw) {
		if pystr.IsSpace(r) {
			if !inSpace {
				b.WriteByte(' ')
			}
			inSpace = true
			continue
		}
		inSpace = false
		b.WriteRune(r)
	}
	preview := pystr.Strip(b.String())
	if pystr.Len(preview) <= maxChars {
		return preview
	}
	return pystr.Rstrip(pystr.Slice(preview, 0, maxChars-1)) + "…"
}

// Epoch seconds of a job last_run_at number or ISO text, false when absent or unparsable
func cronJobLastRunTimestamp(job *pyjson.Object) (float64, bool) {
	if job == nil {
		return 0, false
	}
	switch raw := job.GetOr("last_run_at", nil).(type) {
	case bool:
		if raw {
			return 1, true
		}
		return 0, true
	case int, int64, float64:
		f, err := pyint.Float(raw)
		return f, err == nil
	case string:
		return cronISOToEpoch(raw)
	}
	return 0, false
}

// Upper case last status of a job with underscores as spaces, empty when it has none
func cronOutputStatusLabel(job *pyjson.Object) string {
	if job == nil {
		return ""
	}
	status := pystr.Strip(pyrepr.Str(pyrepr.Or(job.GetOr("last_status", nil), "")))
	if status == "" {
		return ""
	}
	return pystr.Upper(strings.ReplaceAll(status, "_", " "))
}

// Session shaped row of a script only run
func cronOutputRunRow(startedAt float64, title string, preview string) *pyjson.Object {
	var previewValue any
	if preview != "" {
		previewValue = preview
	}
	return pyjson.ObjectOf(
		"title", title,
		"preview", previewValue,
		"source", "cron_output",
		"started_at", startedAt,
		"last_active", startedAt,
		"ended_at", startedAt,
		"input_tokens", int64(0),
		"output_tokens", int64(0),
		"message_count", int64(0),
		"tool_call_count", int64(0),
		"model", nil,
		"cwd", nil,
		"archived", false,
		"is_active", false,
	)
}

// Row with an id placed first ahead of the run row fields
func cronRowWithID(id string, row *pyjson.Object) *pyjson.Object {
	out := pyjson.ObjectOf("id", id)
	for _, k := range row.Keys {
		out.Set(k, row.Vals[k])
	}
	return out
}

// Terminal ledger attempts of the job in the current home, newest claim first, none on any failure
func ownerProfileExecutions(ctx context.Context, canonicalJobID string) []cronAttempt {
	rows, err := cron.ListExecutionsIn(ctx, home.Dir(ctx), canonicalJobID, 100)
	if err != nil {
		return []cronAttempt{}
	}
	terminal := []cronAttempt{}
	for _, row := range rows {
		status := pyrepr.Str(pyrepr.Or(row["status"], ""))
		if !cron.TerminalExecutionStates[status] {
			continue
		}
		claimed, ok := cronISOToEpoch(row["claimed_at"])
		if !ok {
			claimed = 0
		}
		var finished *float64
		if f, ok := cronISOToEpoch(row["finished_at"]); ok {
			finished = &f
		}
		terminal = append(terminal, cronAttempt{
			id:         pyrepr.Str(pyrepr.Or(row["id"], "")),
			claimedAt:  claimed,
			finishedAt: finished,
			status:     status,
			error:      pystr.Strip(pyrepr.Str(pyrepr.Or(row["error"], ""))),
		})
	}
	sort.SliceStable(terminal, func(i, j int) bool { return terminal[i].claimedAt > terminal[j].claimedAt })
	return terminal
}

// Whether a doc timestamp falls inside the attempt window, the claim floored to its second
func executionContains(attempt cronAttempt, startedAt, graceSeconds float64) bool {
	if startedAt+0.001 < math.Floor(attempt.claimedAt) {
		return false
	}
	return attempt.finishedAt == nil || startedAt <= *attempt.finishedAt+graceSeconds
}

// Row title of a ledger outcome, the reason shown for failures, cancels and misses
func executionStatusTitle(status, errText, fallback string) string {
	label := pystr.Upper(strings.ReplaceAll(status, "_", " "))
	if label == "UNKNOWN" || label == "" {
		return fallback
	}
	if cronLabelsWithReason[label] && errText != "" {
		return label + " · " + errText
	}
	return label + " · " + fallback
}

// Output doc rows newest first plus the terminal attempts, one metadata row when only the job record shows a run
func listCronOutputRuns(ctx context.Context, job *pyjson.Object, canonicalJobID, profile string, limit int) ([]*pyjson.Object, []cronAttempt) {
	outputDir := cronOutputRunsDir(ctx, profile, canonicalJobID)
	var files []string
	if entries, err := os.ReadDir(outputDir); err == nil {
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			full := filepath.Join(outputDir, e.Name())
			if st, err := os.Stat(full); err == nil && st.Mode().IsRegular() {
				files = append(files, full)
			}
		}
	}
	slices.SortFunc(files, func(a, b string) int { return strings.Compare(filepath.Base(b), filepath.Base(a)) })
	attempts := ownerProfileExecutions(ctx, canonicalJobID)
	docs := []*pyjson.Object{}
	for i, path := range files {
		if i >= limit {
			break
		}
		startedAt, ok := cronOutputRunTimestamp(ctx, path)
		if !ok {
			startedAt = 0
			if st, err := os.Stat(path); err == nil {
				startedAt = float64(st.ModTime().UnixNano()) / 1e9
			}
		}
		preview := cronOutputRunPreview(path, 180)
		title := preview
		if title == "" {
			title = "Script-only run"
		}
		stem := pypath.PathStem(path)
		docs = append(docs, cronRowWithID(fmt.Sprintf("cron_output:%s:%s", canonicalJobID, stem), cronOutputRunRow(startedAt, title, preview)))
	}
	if len(docs) > 0 || slices.ContainsFunc(attempts, func(a cronAttempt) bool { return cronOutcomeStates[a.status] }) {
		return docs, attempts
	}
	latest, ok := cronJobLastRunTimestamp(job)
	if !ok {
		return []*pyjson.Object{}, attempts
	}
	preview := ""
	if job != nil {
		preview = pystr.Strip(pyrepr.Str(pyrepr.Or(job.GetOr("last_error", nil), "")))
	}
	title := cronOutputStatusLabel(job)
	if title == "" {
		title = "Script-only run"
	}
	if preview != "" {
		title = title + " · " + preview
	}
	return []*pyjson.Object{cronRowWithID(fmt.Sprintf("cron_output:%s:latest", canonicalJobID), cronOutputRunRow(latest, title, preview))}, attempts
}

// Float of a row number field, zero for a falsy or non numeric value
func rowFloat(v any) float64 {
	if !pyrepr.Truthy(v) {
		return 0
	}
	f, err := pyint.Float(v)
	if err != nil {
		return 0
	}
	return f
}

// Whether an output doc falls inside the session span widened by the grace on both ends
func docMatchesSession(docTS float64, session *pyjson.Object, graceSeconds float64) bool {
	start := rowFloat(session.GetOr("started_at", nil))
	end := start
	if v := session.GetOr("ended_at", nil); pyrepr.Truthy(v) {
		end = rowFloat(v)
	} else if v := session.GetOr("last_active", nil); pyrepr.Truthy(v) {
		end = rowFloat(v)
	}
	return start-graceSeconds <= docTS && docTS <= end+graceSeconds
}

// Writer sort key of a candidate attempt, closed before the doc then without a spanning session then older claim last
func docWriterLess(docTS float64, a cronAttempt, aSession *pyjson.Object, b cronAttempt, bSession *pyjson.Object) bool {
	key := func(at cronAttempt, s *pyjson.Object) (bool, bool, float64) {
		closed := at.finishedAt != nil && docTS > *at.finishedAt
		unspanned := s == nil || !docMatchesSession(docTS, s, cronSessionDocMatchSecs)
		return closed, unspanned, -at.claimedAt
	}
	a1, a2, a3 := key(a, aSession)
	b1, b2, b3 := key(b, bSession)
	if a1 != b1 {
		return !a1
	}
	if a2 != b2 {
		return !a2
	}
	return a3 < b3
}

// Session rows with a stopped run titled by its cancel reason
func withCancelReasons(sessionRuns []*pyjson.Object, terminal []indexedAttempt) []*pyjson.Object {
	cancelled := map[string]cronAttempt{}
	for _, t := range terminal {
		if t.attempt.status == "cancelled" {
			cancelled[t.attempt.id] = t.attempt
		}
	}
	out := make([]*pyjson.Object, 0, len(sessionRuns))
	for _, s := range sessionRuns {
		execID, isStr := s.GetOr("cron_execution_id", nil).(string)
		if a, ok := cancelled[execID]; isStr && ok {
			c := s.Copy()
			c.Set("title", executionStatusTitle("cancelled", a.error, pyrepr.Str(pyrepr.Or(s.GetOr("title", nil), ""))))
			out = append(out, c)
			continue
		}
		out = append(out, s)
	}
	return out
}

// Ledger attempt with its index in the newest first attempt list
type indexedAttempt struct {
	index   int
	attempt cronAttempt
}

// One row per execution newest first, a session beating the doc it wrote and a ledger row standing in for neither
func reconcileCronRuns(sessionRuns, docs []*pyjson.Object, attempts []cronAttempt, canonicalJobID string, limit int) []*pyjson.Object {
	matches := func(docTS float64, sessions []*pyjson.Object) bool {
		for _, s := range sessions {
			if docMatchesSession(docTS, s, cronSessionDocMatchSecs) {
				return true
			}
		}
		return false
	}
	byExecution := map[string]*pyjson.Object{}
	var legacy []*pyjson.Object
	for _, s := range sessionRuns {
		v := s.GetOr("cron_execution_id", nil)
		if !pyrepr.Truthy(v) {
			legacy = append(legacy, s)
			continue
		}
		if id, ok := v.(string); ok {
			byExecution[id] = s
		}
	}
	var terminal []indexedAttempt
	for i, a := range attempts {
		if cronOutcomeStates[a.status] {
			terminal = append(terminal, indexedAttempt{i, a})
		}
	}
	represented := map[int]bool{}
	for _, t := range terminal {
		if _, ok := byExecution[t.attempt.id]; ok {
			represented[t.index] = true
		}
	}
	paired := map[int]bool{}
	merged := withCancelReasons(sessionRuns, terminal)
	for _, doc := range docs {
		docTS := rowFloat(doc.GetOr("started_at", nil))
		var best *indexedAttempt
		for i := range terminal {
			t := terminal[i]
			if paired[t.index] || !executionContains(t.attempt, docTS, 300.0) {
				continue
			}
			if best == nil || docWriterLess(docTS, t.attempt, byExecution[t.attempt.id], best.attempt, byExecution[best.attempt.id]) {
				best = &terminal[i]
			}
		}
		if best == nil {
			if !matches(docTS, sessionRuns) {
				merged = append(merged, doc)
			}
			continue
		}
		paired[best.index] = true
		represented[best.index] = true
		if _, ok := byExecution[best.attempt.id]; ok || matches(docTS, legacy) {
			continue
		}
		row := doc.Copy()
		row.Set("title", executionStatusTitle(best.attempt.status, best.attempt.error, pyrepr.Str(doc.GetOr("title", nil))))
		merged = append(merged, row)
	}
	for _, t := range terminal {
		if represented[t.index] {
			continue
		}
		when := t.attempt.claimedAt
		if t.attempt.finishedAt != nil && *t.attempt.finishedAt != 0 {
			when = *t.attempt.finishedAt
		}
		if matches(when, legacy) {
			continue
		}
		title := executionStatusTitle(t.attempt.status, t.attempt.error, "Script-only run")
		merged = append(merged, cronRowWithID(fmt.Sprintf("cron_output:%s:exec:%d", canonicalJobID, t.index), cronOutputRunRow(when, title, t.attempt.error)))
	}
	sort.SliceStable(merged, func(i, j int) bool {
		return rowFloat(merged[i].GetOr("started_at", nil)) > rowFloat(merged[j].GetOr("started_at", nil))
	})
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged
}

// Run history of a job newest first, agent sessions reconciled with script only fires in the owner profile scope
func listCronJobRunsSync(ctx context.Context, jobID, profile string, limit int64) (*pyjson.Object, error) {
	selected, err := jobOwnerProfile(ctx, jobID, profile)
	if err != nil {
		return nil, err
	}
	canonical := jobID
	var job *pyjson.Object
	if selected != "" {
		found, err := callCronForProfile(ctx, selected, func(ctx context.Context, h string) (any, error) {
			return cron.GetJob(ctx, h, jobID)
		})
		if err != nil {
			return nil, err
		}
		job, _ = found.(*pyjson.Object)
		if job != nil && pyrepr.Truthy(job.GetOr("id", nil)) {
			canonical = pyrepr.Str(job.GetOr("id", nil))
		}
	}
	limitN := int(max(1, min(limit, 100)))
	scoped := cronOwnerHomeScope(ctx, selected)
	var selectedPtr *string
	if selected != "" {
		selectedPtr = &selected
	}
	db, err := openSessionDBForProfile(scoped, selectedPtr, true)
	if err != nil {
		return nil, err
	}
	sessionRuns, err := db.ListCronJobRunsFrom(scoped, canonical, limitN, 0)
	db.Close()
	if err != nil {
		return nil, err
	}
	now := float64(time.Now().UnixNano()) / 1e9
	inflight := cronLiveInflightExecution(scoped, canonical)
	for _, s := range sessionRuns {
		ended, _ := s.Get("ended_at")
		last, ok := s.Get("last_active")
		if !ok {
			last = s.GetOr("started_at", int64(0))
		}
		lastF, _ := pyint.Float(last)
		s.Set("is_active", ended == nil && now-lastF < 300)
		s.Set("scheduler_owned", cronRunOwnedBy(s, inflight))
		s.Set("archived", pyrepr.Truthy(s.GetOr("archived", nil)))
		if selected != "" {
			s.Set("profile", selected)
		}
	}
	docs, attempts := listCronOutputRuns(scoped, job, canonical, selected, limitN)
	runs := reconcileCronRuns(sessionRuns, docs, attempts, canonical, limitN)
	items := make([]any, len(runs))
	for i, r := range runs {
		items[i] = r
	}
	return pyjson.ObjectOf("runs", items, "limit", int64(limitN)), nil
}

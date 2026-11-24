//go:build !icebath_core && !icebath_noextras

package tuiserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

var hubCronRegistry = &handlerRegistry{}

func init() {
	for _, m := range []struct {
		name string
		body rpcBody
	}{
		{tuigateway.CronJobsList.Name(), cronJobsList},
		{tuigateway.CronJobsCreate.Name(), cronJobsWrite("create")},
		{tuigateway.CronJobsUpdate.Name(), cronJobsWrite("edit")},
		{tuigateway.CronJobsPause.Name(), cronJobsAction("pause")},
		{tuigateway.CronJobsResume.Name(), cronJobsAction("resume")},
		{tuigateway.CronJobsCancel.Name(), cronJobsAction("cancel")},
		{tuigateway.CronJobsRemove.Name(), cronJobsAction("remove")},
		{tuigateway.CronJobsRun.Name(), cronJobsRun},
		{tuigateway.CronJobsRuns.Name(), cronJobsRuns},
		{tuigateway.CronJobsOutput.Name(), cronJobsOutput},
		{tuigateway.CronIncidentsList.Name(), cronIncidentsList},
		{tuigateway.CronIncidentsAck.Name(), cronIncidentsAck},
		{tuigateway.CronReport.Name(), cronReport},
	} {
		scopedRPC(hubCronRegistry, m.name, 5170, rpcShape{}, m.body)
	}
	hubCronRegistry.install()
}

// Refusal of params that do not fit a scheduled jobs method
func cronParamsRefused(rid any, err error) map[string]any { return rpcErr(rid, 4170, err.Error(), nil) }

// Text of an optional string, empty for nil
func optText(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Schedule of a job as it is typed to make the job again
func cronScheduleText(j *cron.Job) string {
	s := j.Schedule
	switch {
	case s == nil:
		return j.ScheduleDisplay
	case s.Kind == "cron":
		return s.Expr
	case s.Kind == "interval" && s.HasMinutes:
		return fmt.Sprintf("every %dm", s.Minutes)
	case s.Kind == "once":
		return s.RunAt
	case s.Raw != "":
		return s.Raw
	}
	return j.ScheduleDisplay
}

// Row of a job for the scheduled jobs screen
func cronJobRow(j *cron.Job) tuigateway.CronJobRow {
	skills := slices.Clone(j.Skills)
	if len(skills) == 0 && j.Skill != nil && *j.Skill != "" {
		skills = []string{*j.Skill}
	}
	row := tuigateway.CronJobRow{ID: j.ID, Name: j.Name, Prompt: j.Prompt, Schedule: j.ScheduleDisplay, ScheduleText: cronScheduleText(j),
		State: cron.EffectiveJobState(j), Enabled: j.Enabled, PausedReason: optText(j.PausedReason), NextRunAt: optText(j.NextRunAt),
		LastRunAt: optText(j.LastRunAt), LastStatus: optText(j.LastStatus), LastError: optText(j.LastError), Running: j.LiveExecution != nil,
		Deliver: optText(j.Deliver), Skills: skills, Model: optText(j.Model), Provider: optText(j.Provider), ReasoningEffort: optText(j.ReasoningEffort),
		Workdir: optText(j.Workdir), Script: optText(j.Script), NoAgent: j.NoAgent, Timezone: optText(j.Timezone)}
	if row.Schedule == "" {
		row.Schedule = row.ScheduleText
	}
	if j.Repeat != nil {
		row.RepeatDone = int64(j.Repeat.Completed)
		if j.Repeat.Times != nil {
			times := int64(*j.Repeat.Times)
			row.RepeatTimes = &times
		}
	}
	return row
}

// Scheduled jobs of the profile, paused and finished ones when asked, and whether the background service starts on its own
func cronJobsList(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	p, err := tuigateway.CronJobsList.Params(params)
	if err != nil {
		return cronParamsRefused(rid, err), nil
	}
	jobs, err := cron.ListJobs(ctx, home.Dir(ctx), p.All)
	if err != nil {
		return nil, err
	}
	out := tuigateway.CronJobsListResult{Jobs: make([]tuigateway.CronJobRow, 0, len(jobs))}
	for _, j := range jobs {
		out.Jobs = append(out.Jobs, cronJobRow(j))
	}
	if out.ServiceInstalled, err = gateway.IsServiceInstalled(ctx); err != nil {
		return nil, err
	}
	return ok(rid, tuigateway.Plain(out)), nil
}

// Runs one icebath cron subcommand in this process the way the CLI does, answering its exit and what it printed without colour
func cronCommand(ctx context.Context, a *cron.CronArgs) (bool, string, error) {
	var buf bytes.Buffer
	code, err := cron.CronCommand(ctx, home.Dir(ctx), a, &buf)
	if err != nil {
		return false, "", err
	}
	return code == 0, strings.TrimSpace(ansi.Strip(buf.String())), nil
}

// Job of the profile by id after a change, nil once it is gone
func cronJobAfter(ctx context.Context, id string) (*tuigateway.CronJobRow, error) {
	if id == "" {
		return nil, nil
	}
	j, err := cron.GetJob(ctx, home.Dir(ctx), id)
	if err != nil || j == nil {
		return nil, err
	}
	jobs, err := cron.ListJobs(ctx, home.Dir(ctx), true)
	if err != nil {
		return nil, err
	}
	for _, listed := range jobs {
		if listed.ID == id {
			row := cronJobRow(listed)
			return &row, nil
		}
	}
	row := cronJobRow(j)
	return &row, nil
}

// Id a create printed, the CLI's own words naming the new job
var createdJobRe = regexp.MustCompile(`Created job: (\S+)`)

// Creates or edits a job through the cron command the CLI runs, the form's fields as its flags
func cronJobsWrite(command string) rpcBody {
	return func(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
		p, err := tuigateway.CronJobsCreate.Params(params)
		if err != nil {
			return cronParamsRefused(rid, err), nil
		}
		if command == "edit" && p.JobID == "" {
			return rpcErr(rid, 4171, "job_id required", nil), nil
		}
		f := p.Fields
		a := &cron.CronArgs{Command: command, JobID: p.JobID, Schedule: f.Schedule, Prompt: f.Prompt}
		optional := func(v *string) any {
			if v == nil {
				return nil
			}
			return *v
		}
		a.Name, a.Deliver, a.Model, a.ModelProvider = optional(f.Name), optional(f.Deliver), optional(f.Model), optional(f.Provider)
		a.ReasoningEffort, a.Workdir = optional(f.ReasoningEffort), optional(f.Workdir)
		if f.Repeat != nil {
			a.Repeat = int(*f.Repeat)
		}
		if f.SetSkills {
			if len(f.Skills) == 0 {
				a.ClearSkills = command == "edit"
			} else {
				a.Skills = f.Skills
			}
		}
		a.Paused = command == "create" && f.Paused
		done, out, err := cronCommand(ctx, a)
		if err != nil {
			return nil, err
		}
		id := p.JobID
		if m := createdJobRe.FindStringSubmatch(out); command == "create" && m != nil {
			id = m[1]
		}
		res := tuigateway.CronJobResult{OK: done, Output: out}
		if done {
			if res.Job, err = cronJobAfter(ctx, id); err != nil {
				return nil, err
			}
		}
		return ok(rid, tuigateway.Plain(res)), nil
	}
}

// Pauses, resumes, cancels the live run of or removes a job through the cron command the CLI runs
func cronJobsAction(command string) rpcBody {
	return func(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
		p, err := tuigateway.CronJobsPause.Params(params)
		if err != nil {
			return cronParamsRefused(rid, err), nil
		}
		done, out, err := cronCommand(ctx, &cron.CronArgs{Command: command, JobID: p.JobID, Reason: p.Reason})
		if err != nil {
			return nil, err
		}
		res := tuigateway.CronJobResult{OK: done, Output: out}
		if done && command != "remove" {
			if res.Job, err = cronJobAfter(ctx, p.JobID); err != nil {
				return nil, err
			}
		}
		return ok(rid, tuigateway.Plain(res)), nil
	}
}

// Starts a run of the job now as icebath cron run does, in the background so the answer comes at once and the outcome lands in the job's runs
func cronJobsRun(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	p, err := tuigateway.CronJobsRun.Params(params)
	if err != nil {
		return cronParamsRefused(rid, err), nil
	}
	job, err := cron.ResolveJobRef(ctx, home.Dir(ctx), p.JobID)
	if err != nil {
		return ok(rid, tuigateway.Plain(tuigateway.CronJobResult{Output: pyrepr.ExceptionMessage(err)})), nil
	}
	if job == nil {
		return rpcErr(rid, 4172, "no scheduled job "+p.JobID, nil), nil
	}
	detached := context.WithoutCancel(ctx)
	go func() {
		done, out, err := cronCommand(detached, &cron.CronArgs{Command: "run", JobID: job.ID})
		switch {
		case err != nil:
			pkgLog().Warn("scheduled job run failed", "job", job.ID, "error", err)
		case !done:
			pkgLog().Warn("scheduled job run failed", "job", job.ID, "output", out)
		}
	}()
	row := cronJobRow(job)
	row.Running = true
	return ok(rid, tuigateway.Plain(tuigateway.CronJobResult{OK: true, Job: &row})), nil
}

// Whether a name is one path part, no folder or parent step in it
func plainPathPart(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name && !strings.ContainsAny(name, `/\`)
}

// Durable runs of a job, or of every job, and the outputs its runs saved, newest first
func cronJobsRuns(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	p, err := tuigateway.CronJobsRuns.Params(params)
	if err != nil {
		return cronParamsRefused(rid, err), nil
	}
	limit := int(p.Limit)
	if limit <= 0 {
		limit = 50
	}
	h := home.Dir(ctx)
	records, err := cron.ListExecutions(ctx, h, p.JobID, limit)
	if err != nil {
		return nil, err
	}
	text := func(r map[string]any, k string) string {
		if v, ok := r[k]; ok && v != nil {
			return pyrepr.Str(v)
		}
		return ""
	}
	out := tuigateway.CronJobRunsResult{Runs: []tuigateway.CronJobRun{}, Outputs: []tuigateway.CronJobOutput{}}
	for _, r := range records {
		out.Runs = append(out.Runs, tuigateway.CronJobRun{ID: text(r, "id"), JobID: text(r, "job_id"), Status: text(r, "status"), Source: text(r, "source"),
			ClaimedAt: text(r, "claimed_at"), StartedAt: text(r, "started_at"), FinishedAt: text(r, "finished_at"), Error: text(r, "error"),
			Delivery: text(r, "delivery_outcome")})
	}
	if plainPathPart(p.JobID) {
		entries, err := os.ReadDir(filepath.Join(h, "cron", "output", p.JobID))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		for _, e := range entries {
			info, err := e.Info()
			if err != nil || e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			out.Outputs = append(out.Outputs, tuigateway.CronJobOutput{Name: e.Name(), At: info.ModTime().Format("2006-01-02 15:04:05"), Size: info.Size()})
		}
		slices.SortFunc(out.Outputs, func(x, y tuigateway.CronJobOutput) int { return strings.Compare(y.Name, x.Name) })
	}
	return ok(rid, tuigateway.Plain(out)), nil
}

// Largest saved output a read returns in full
const cronOutputReadLimit = 256 * 1024

// Text of one saved output of a job, its end kept when it is long
func cronJobsOutput(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	p, err := tuigateway.CronJobsOutput.Params(params)
	if err != nil {
		return cronParamsRefused(rid, err), nil
	}
	if !plainPathPart(p.JobID) || !plainPathPart(p.Name) {
		return rpcErr(rid, 4173, "not an output of a job", nil), nil
	}
	data, err := os.ReadFile(filepath.Join(home.Dir(ctx), "cron", "output", p.JobID, p.Name))
	if errors.Is(err, fs.ErrNotExist) {
		return rpcErr(rid, 4174, "that output is gone", nil), nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > cronOutputReadLimit {
		data = data[len(data)-cronOutputReadLimit:]
	}
	return ok(rid, tuigateway.Plain(tuigateway.CronJobOutputResult{Content: string(data)})), nil
}

// Failure incidents of the scheduled jobs, those in a state when one is named
func cronIncidentsList(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	p, err := tuigateway.CronIncidentsList.Params(params)
	if err != nil {
		return cronParamsRefused(rid, err), nil
	}
	incidents, err := cron.ListIncidents(home.Dir(ctx), p.State)
	if err != nil {
		return nil, err
	}
	out := tuigateway.CronIncidentsResult{Incidents: make([]tuigateway.CronIncident, 0, len(incidents))}
	for _, inc := range incidents {
		out.Incidents = append(out.Incidents, tuigateway.CronIncident{ID: inc.ID, JobID: inc.JobID, State: inc.State, FailureType: inc.FailureType,
			FirstSeenAt: inc.FirstSeenAt, LastSeenAt: inc.LastSeenAt, Error: inc.Error, OutputFile: optText(inc.OutputFile)})
	}
	return ok(rid, tuigateway.Plain(out)), nil
}

// Acknowledges a failure incident through the cron command the CLI runs
func cronIncidentsAck(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	p, err := tuigateway.CronIncidentsAck.Params(params)
	if err != nil {
		return cronParamsRefused(rid, err), nil
	}
	done, out, err := cronCommand(ctx, &cron.CronArgs{Command: "incidents", IncidentAction: "ack", IncidentID: p.ID})
	if err != nil {
		return nil, err
	}
	return ok(rid, tuigateway.Plain(tuigateway.CronReportResult{OK: done, Output: out})), nil
}

// Scheduler doctor or status report, or a job's notepad listed, set or cleared, through the cron command the CLI runs
func cronReport(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	p, err := tuigateway.CronReport.Params(params)
	if err != nil {
		return cronParamsRefused(rid, err), nil
	}
	a := &cron.CronArgs{Command: p.Command}
	switch p.Command {
	case "doctor", "status":
	case "notepad":
		a.JobID, a.NotepadAction, a.Key, a.Value = p.JobID, p.Action, p.Key, p.Value
	default:
		return rpcErr(rid, 4175, "unknown report "+p.Command, nil), nil
	}
	done, out, err := cronCommand(ctx, a)
	if err != nil {
		return nil, err
	}
	return ok(rid, tuigateway.Plain(tuigateway.CronReportResult{OK: done, Output: out})), nil
}

package cron

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Tells the active scheduler provider the job set changed, logging instead of failing
func (s *Store) notifyProviderJobsChanged() {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("on_jobs_changed notify failed: %v", r))
		}
	}()
	if err := resolveCronScheduler(s).OnJobsChanged(); err != nil {
		slog.Debug(fmt.Sprintf("on_jobs_changed notify failed: %v", err))
	}
}

// CronSchedulerRegistrationError reports a saved job whose first external trigger was not registered
type CronSchedulerRegistrationError struct {
	Job   *Job
	Cause error
}

// Python exception class of a provider registration error
func cronExceptionName(err error) string {
	var named interface{ ExceptionName() string }
	if errors.As(err, &named) {
		return named.ExceptionName()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "TimeoutError"
	}
	if pypath.IsOSError(err) {
		return pypath.OSErrorClass(err)
	}
	return "RuntimeError"
}

func (e *CronSchedulerRegistrationError) Error() string {
	return fmt.Sprintf("Cron job '%s' was saved, but its first scheduler registration failed (%s). Do not create a duplicate. Pause/resume or update the job to retry registration.", e.Job.ID, cronExceptionName(e.Cause))
}

func (e *CronSchedulerRegistrationError) Unwrap() error { return e.Cause }

// Chat and CLI wording of the partial failure without the exception class
func (e *CronSchedulerRegistrationError) UserMessage() string {
	label := e.Job.ID
	if e.Job.Name != "" {
		label = e.Job.Name
	}
	return fmt.Sprintf("Saved cron job '%s', but couldn't register it with the external scheduler yet. The job is kept — don't re-create it; pause/resume or edit it (e.g. via /cron) to retry registration.", label)
}

// Public partial failure contract without provider details
func (e *CronSchedulerRegistrationError) ToDict() *pyjson.Object {
	out := pyjson.NewObject()
	out.Set("error", e.Error())
	out.Set("job_id", e.Job.ID)
	out.Set("job_saved", true)
	out.Set("scheduler_registered", false)
	out.Set("retry_create", false)
	return out
}

var createJobParams = []string{"prompt", "schedule", "name", "repeat", "deliver", "origin", "skill", "skills", "model",
	"provider", "base_url", "script", "context_from", "enabled_toolsets", "workdir", "no_agent", "attach_to_session",
	"monitor_script", "monitor_url", "reasoning_effort", "failure_deliver", "catch_up", "paused", "paused_reason",
	"pinned", "interpreter", "timezone", "grace_minutes"}

// Binds a keyword spec to the create_job parameters with Python argument errors
func createJobOptionsFromSpec(spec *pyjson.Object) (CreateJobOptions, error) {
	var o CreateJobOptions
	known := map[string]bool{}
	for _, p := range createJobParams {
		known[p] = true
	}
	for _, k := range spec.Keys {
		if !known[k] {
			return o, fmt.Errorf("TypeError: create_job() got an unexpected keyword argument %s", pyrepr.Repr(k))
		}
	}
	var missing []string
	for _, p := range []string{"prompt", "schedule"} {
		if _, ok := spec.Vals[p]; !ok {
			missing = append(missing, pyrepr.Repr(p))
		}
	}
	switch len(missing) {
	case 1:
		return o, fmt.Errorf("TypeError: create_job() missing 1 required positional argument: %s", missing[0])
	case 2:
		return o, fmt.Errorf("TypeError: create_job() missing 2 required positional arguments: %s", strings.Join(missing, " and "))
	}
	optStr := func(key string) (*string, error) {
		switch v := spec.Vals[key].(type) {
		case nil:
			return nil, nil
		case string:
			return &v, nil
		default:
			return nil, fmt.Errorf("TypeError: create_job() argument %s must be str or None, not %s", pyrepr.Repr(key), pyrepr.TypeName(v))
		}
	}
	var err error
	if o.Prompt, err = optStr("prompt"); err != nil {
		return o, err
	}
	sched, ok := spec.Vals["schedule"].(string)
	if !ok {
		return o, fmt.Errorf("TypeError: create_job() argument 'schedule' must be str, not %s", pyrepr.TypeName(spec.Vals["schedule"]))
	}
	o.Schedule = sched
	for key, dst := range map[string]**string{"name": &o.Name, "deliver": &o.Deliver, "skill": &o.Skill, "paused_reason": &o.PausedReason} {
		if *dst, err = optStr(key); err != nil {
			return o, err
		}
	}
	switch v := spec.Vals["origin"].(type) {
	case nil:
	case map[string]any:
		o.Origin = v
	case *pyjson.Object:
		o.Origin = make(map[string]any, v.Len())
		for _, k := range v.Keys {
			o.Origin[k] = v.Vals[k]
		}
	default:
		return o, fmt.Errorf("TypeError: create_job() argument 'origin' must be dict or None, not %s", pyrepr.TypeName(v))
	}
	o.Repeat, o.Skills, o.Model, o.Provider = spec.Vals["repeat"], spec.Vals["skills"], spec.Vals["model"], spec.Vals["provider"]
	o.BaseURL, o.Script, o.ContextFrom, o.EnabledToolsets = spec.Vals["base_url"], spec.Vals["script"], spec.Vals["context_from"], spec.Vals["enabled_toolsets"]
	o.Workdir, o.AttachToSession, o.MonitorScript, o.MonitorURL = spec.Vals["workdir"], spec.Vals["attach_to_session"], spec.Vals["monitor_script"], spec.Vals["monitor_url"]
	o.ReasoningEffort, o.FailureDeliver, o.CatchUp = spec.Vals["reasoning_effort"], spec.Vals["failure_deliver"], spec.Vals["catch_up"]
	o.Interpreter, o.Timezone, o.GraceMinutes = spec.Vals["interpreter"], spec.Vals["timezone"], spec.Vals["grace_minutes"]
	o.NoAgent, o.Paused, o.Pinned = pyrepr.Truthy(spec.Vals["no_agent"]), pyrepr.Truthy(spec.Vals["paused"]), pyrepr.Truthy(spec.Vals["pinned"])
	return o, nil
}

// Persists one job and registers its first trigger with the active provider
func (s *Store) createJobWithSchedulerRegistration(spec *pyjson.Object) (*Job, error) {
	opts, err := createJobOptionsFromSpec(spec)
	if err != nil {
		return nil, err
	}
	job, err := s.createJob(opts)
	if err != nil {
		return nil, err
	}
	if !job.Enabled {
		return job, nil
	}
	if err := resolveCronScheduler(s).RegisterJob(job); err != nil {
		return nil, &CronSchedulerRegistrationError{Job: job, Cause: err}
	}
	return job, nil
}

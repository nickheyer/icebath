package cron

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Tells the active scheduler provider the job set changed, logging instead of failing
func (s *Store) notifyProviderJobsChanged() {
	defer func() {
		if r := recover(); r != nil {
			pkgLog().Debug(fmt.Sprintf("on_jobs_changed notify failed: %v", r))
		}
	}()
	if err := schedulerOnJobsChanged(schedulerResolver(s)); err != nil {
		pkgLog().Debug(fmt.Sprintf("on_jobs_changed notify failed: %v", err))
	}
}

// CronSchedulerRegistrationError reports a saved job whose first external trigger was not registered
type CronSchedulerRegistrationError struct {
	Job   *Job
	Cause error
}

// Kind label of a provider registration error: the label it declares, else a timeout, system or runtime failure
func cronExceptionName(err error) string {
	var named interface{ ErrorKind() string }
	if errors.As(err, &named) {
		return named.ErrorKind()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "TimeoutError"
	}
	if errorutil.IsSystem(err) {
		return errorutil.Kind(err)
	}
	return "RuntimeError"
}

func (e *CronSchedulerRegistrationError) Error() string {
	return fmt.Sprintf("Cron job '%s' was saved, but its first scheduler registration failed. Do not create a duplicate. Pause/resume or update the job to retry registration.", e.Job.ID)
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
func (e *CronSchedulerRegistrationError) ToDict() *jsonutil.Object {
	out := jsonutil.NewObject()
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
func createJobOptionsFromSpec(spec *jsonutil.Object) (CreateJobOptions, error) {
	var o CreateJobOptions
	known := map[string]bool{}
	for _, p := range createJobParams {
		known[p] = true
	}
	for _, k := range spec.Keys() {
		if !known[k] {
			return o, fmt.Errorf("unknown cron job field %s", k)
		}
	}
	var missing []string
	for _, p := range []string{"prompt", "schedule"} {
		if _, ok := spec.Get(p); !ok {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		return o, fmt.Errorf("a cron job spec needs %s", strings.Join(missing, " and "))
	}
	optStr := func(key string) (*string, error) {
		switch v := spec.Value(key).(type) {
		case nil:
			return nil, nil
		case string:
			return &v, nil
		default:
			return nil, fmt.Errorf("cron job field %s must be text, got %s", key, errchain.Kind(v))
		}
	}
	var err error
	if o.Prompt, err = optStr("prompt"); err != nil {
		return o, err
	}
	sched, ok := spec.Value("schedule").(string)
	if !ok {
		return o, fmt.Errorf("cron job field schedule must be text, got %s", errchain.Kind(spec.Value("schedule")))
	}
	o.Schedule = sched
	for key, dst := range map[string]**string{"name": &o.Name, "deliver": &o.Deliver, "skill": &o.Skill} {
		if *dst, err = optStr(key); err != nil {
			return o, err
		}
	}
	switch v := spec.Value("origin").(type) {
	case nil:
	case map[string]any:
		o.Origin = v
	case *jsonutil.Object:
		o.Origin = make(map[string]any, v.Len())
		for _, k := range v.Keys() {
			o.Origin[k] = v.Value(k)
		}
	default:
		return o, fmt.Errorf("cron job field origin must be a mapping, got %s", errchain.Kind(v))
	}
	o.Repeat, o.Skills, o.Model, o.Provider = spec.Value("repeat"), spec.Value("skills"), spec.Value("model"), spec.Value("provider")
	o.BaseURL, o.Script, o.ContextFrom, o.EnabledToolsets = spec.Value("base_url"), spec.Value("script"), spec.Value("context_from"), spec.Value("enabled_toolsets")
	o.Workdir, o.AttachToSession, o.MonitorScript, o.MonitorURL = spec.Value("workdir"), spec.Value("attach_to_session"), spec.Value("monitor_script"), spec.Value("monitor_url")
	o.ReasoningEffort, o.FailureDeliver, o.CatchUp = spec.Value("reasoning_effort"), spec.Value("failure_deliver"), spec.Value("catch_up")
	o.Interpreter, o.Timezone, o.GraceMinutes = spec.Value("interpreter"), spec.Value("timezone"), spec.Value("grace_minutes")
	o.NoAgent, o.Pinned = anyutil.IsTrue(spec.Value("no_agent")), anyutil.IsTrue(spec.Value("pinned"))
	if v, ok := spec.Get("paused"); ok {
		paused, isBool := v.(bool)
		if !isBool {
			return o, errors.New("paused must be a boolean.")
		}
		o.Paused = paused
	}
	if v := spec.Value("paused_reason"); v != nil {
		reason, isStr := v.(string)
		if !isStr {
			return o, errors.New("paused_reason must be a string.")
		}
		o.PausedReason = &reason
	}
	return o, nil
}

// Persists one job and registers its first trigger with the active provider
func (s *Store) createJobWithSchedulerRegistration(ctx context.Context, spec *jsonutil.Object) (*Job, error) {
	opts, err := createJobOptionsFromSpec(spec)
	if err != nil {
		return nil, err
	}
	job, err := s.createJob(home.WithOverride(ctx, s.home), opts)
	if err != nil {
		return nil, err
	}
	if !job.Enabled {
		return job, nil
	}
	if err := schedulerRegisterJob(schedulerResolver(s), job); err != nil {
		return nil, &CronSchedulerRegistrationError{Job: job, Cause: err}
	}
	return job, nil
}

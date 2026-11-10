package cron

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Provider double of the creation registration contract whose register_job is the given callable
type registeringProvider struct {
	name     string
	register func(job *Job) error
}

func (p *registeringProvider) Name() string                                         { return p.name }
func (p *registeringProvider) Start(context.Context, <-chan struct{}, StartOptions) {}
func (p *registeringProvider) RegisterJob(job *Job) error {
	if p.register != nil {
		return p.register(job)
	}
	return nil
}

func failRegistration(*Job) error { return errors.New("private callback URL and token") }

// Provider counting its on_jobs_changed calls
type spyCronProvider struct {
	minimalProvider
	calls []int
}

func (s *spyCronProvider) OnJobsChanged() error {
	s.calls = append(s.calls, 1)
	return nil
}

// Keyword spec of create_job(prompt="echo hi", schedule="every 5m", name="w")
func notifySpec() *pyjson.Object {
	spec := pyjson.NewObject()
	spec.Set("prompt", "echo hi")
	spec.Set("schedule", "every 5m")
	spec.Set("name", "w")
	return spec
}

func TestNotifyHelperCallsProviderOnJobsChanged(t *testing.T) {
	s, _ := newTestStore(t)
	spy := &spyCronProvider{minimalProvider: minimalProvider{"spy"}}
	patchChronos(t, &schedulerResolver, func(*Store) CronScheduler { return spy })
	s.notifyProviderJobsChanged()
	if !reflect.DeepEqual(spy.calls, []int{1}) {
		t.Fatalf("calls %v", spy.calls)
	}
}

func TestCreateRegistersFirstTriggerWithActiveProvider(t *testing.T) {
	s, _ := newTestStore(t)
	var registered []*Job
	provider := &registeringProvider{name: "stub", register: func(job *Job) error {
		registered = append(registered, job)
		return nil
	}}
	patchChronos(t, &schedulerResolver, func(*Store) CronScheduler { return provider })
	job, err := s.createJobWithSchedulerRegistration(notifySpec())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(registered, []*Job{job}) {
		t.Fatalf("registered %v", registered)
	}
}

func TestCreateFailurePreservesJobAndHidesProviderDetails(t *testing.T) {
	s, _ := newTestStore(t)
	provider := &registeringProvider{name: "failing", register: failRegistration}
	patchChronos(t, &schedulerResolver, func(*Store) CronScheduler { return provider })
	_, err := s.createJobWithSchedulerRegistration(notifySpec())
	var regErr *CronSchedulerRegistrationError
	if !errors.As(err, &regErr) {
		t.Fatalf("error %T %v", err, err)
	}
	stored, err2 := s.getJob(regErr.Job.ID)
	if err2 != nil || stored == nil {
		t.Fatalf("stored %v %v", stored, err2)
	}
	want, _ := regErr.Job.MarshalJSON()
	got, _ := stored.MarshalJSON()
	if string(got) != string(want) {
		t.Fatalf("stored %s\nwant %s", got, want)
	}
	if strings.Contains(err.Error(), "private callback URL and token") {
		t.Fatal(err)
	}
	if msg := regErr.UserMessage(); strings.Contains(msg, "RuntimeError") || !strings.Contains(msg, "'w'") {
		t.Fatal(msg)
	}
}

func TestToolCreateRegistersProviderBeforeReportingSuccess(t *testing.T) {
	s, _ := newTestStore(t)
	var registered []string
	provider := &registeringProvider{name: "recording", register: func(job *Job) error {
		registered = append(registered, job.ID)
		return nil
	}}
	patchChronos(t, &schedulerResolver, func(*Store) CronScheduler { return provider })
	out := dispatchCronjob(t, s, map[string]any{"action": "create", "prompt": "echo hi", "schedule": "every 5m", "name": "w"})
	if out["success"] != true || !reflect.DeepEqual(registered, []string{out["job_id"].(string)}) {
		t.Fatalf("out %v registered %v", out, registered)
	}
}

func TestToolCreateReportsPartialRegistrationFailure(t *testing.T) {
	s, _ := newTestStore(t)
	provider := &registeringProvider{name: "failing", register: failRegistration}
	patchChronos(t, &schedulerResolver, func(*Store) CronScheduler { return provider })
	out := dispatchCronjob(t, s, map[string]any{"action": "create", "prompt": "echo hi", "schedule": "every 5m", "name": "w"})
	if out["success"] != false || out["job_saved"] != true || out["scheduler_registered"] != false || out["retry_create"] != false {
		t.Fatalf("out %v", out)
	}
	if id, _ := out["job_id"].(string); id == "" {
		t.Fatalf("job id %v", out["job_id"])
	}
	if msg, ok := out["error"].(string); !ok || strings.Contains(msg, "private callback URL and token") {
		t.Fatalf("error %v", out["error"])
	}
}

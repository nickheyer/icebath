package cron

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	syncutil "github.com/nickheyer/icebath/pkg/utils/sync"
)

// Job runner the owner executes a fire with, the scheduler run_job attribute tests patch
var ownerRunJob = (*Store).runJob

// Store of the profile home bound on ctx, the owner's scope
func ownerStore(ctx context.Context) *Store {
	return openCronStore(home.Dir(ctx), time.Now)
}

// Ordered JSON object of a job record
func jobObject(job *Job) (*jsonutil.Object, error) {
	raw, err := json.Marshal(job)
	if err != nil {
		return nil, err
	}
	decoded, err := jsonutil.Decode(raw)
	if err != nil {
		return nil, err
	}
	return decoded.(*jsonutil.Object), nil
}

// Job record of an ordered JSON object
func objectJob(obj *jsonutil.Object) (*Job, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	job := &Job{}
	if err := json.Unmarshal(raw, job); err != nil {
		return nil, err
	}
	if id, ok := job.Extra["execution_id"]; ok {
		if err := json.Unmarshal(id, &job.ExecutionID); err != nil {
			return nil, err
		}
		delete(job.Extra, "execution_id")
	}
	return job, nil
}

// Cancel source over the owner's per admission latch, reporting no reason of its own
type ownerLatchCancel struct{ event *syncutil.Event }

func (c *ownerLatchCancel) IsSet() bool    { return c.event.IsSet() }
func (c *ownerLatchCancel) Set()           { c.event.Set() }
func (c *ownerLatchCancel) Reason() string { return "" }

func init() {
	gateway.WebhookExecuteJobForEvent = ExecuteJobForEvent
	gateway.SetCronOwnerHooks(&gateway.CronOwnerHooks{
		GetJob: func(ctx context.Context, jobID string) (*jsonutil.Object, error) {
			job, err := ownerStore(ctx).getJob(ctx, jobID)
			if err != nil || job == nil {
				return nil, err
			}
			return jobObject(job)
		},
		JobModel: func(ctx context.Context, obj *jsonutil.Object, jobID, jobName string) (*string, error) {
			job, err := objectJob(obj)
			if err != nil {
				return nil, err
			}
			cfg, err := ownerStore(ctx).loadCronJobConfig(ctx, job, jobID, jobName)
			if err != nil {
				return nil, err
			}
			return &cfg.model, nil
		},
		GetExecution: func(ctx context.Context, executionID string) (map[string]any, error) {
			return openExecutionsLedger(ownerStore(ctx)).Get(ctx, executionID)
		},
		EnterExecution: func(ctx context.Context, obj *jsonutil.Object, executionID string, record map[string]any) context.Context {
			job, err := objectJob(obj)
			if err != nil {
				panic(err)
			}
			return enterCronExecution(ctx, job, executionID, record)
		},
		RunJob: func(ctx context.Context, obj *jsonutil.Object, executionID string, extraPrompt *string, local *syncutil.Event) (gateway.CronRunResult, error) {
			s := ownerStore(ctx)
			job, err := objectJob(obj)
			if err != nil {
				return gateway.CronRunResult{}, err
			}
			cancel := NewCombinedCancelEvent(s.newCancelRequest(executionID), &ownerLatchCancel{event: local})
			out, err := ownerRunJob(s, ctx, job, runJobOptions{ExtraPrompt: extraPrompt, Cancel: cancel, ExecutionID: &executionID})
			if job.run.modelUnreachable {
				obj.Set("_model_unreachable", true)
			}
			if job.run.quotaHoldSeconds != 0 {
				obj.Set("_quota_hold_seconds", job.run.quotaHoldSeconds)
			}
			if err != nil {
				return gateway.CronRunResult{}, err
			}
			return gateway.CronRunResult{Success: out.Success, Output: out.Output, FinalResponse: out.FinalResponse, Error: out.Error}, nil
		},
		OwnerProgress: func(executionID string) any {
			ownerProgress.Lock()
			defer ownerProgress.Unlock()
			if at, ok := ownerProgress.at[executionID]; ok {
				return at
			}
			return nil
		},
		PopOwnerProgress: func(executionID string) {
			ownerProgress.Lock()
			defer ownerProgress.Unlock()
			delete(ownerProgress.at, executionID)
		},
	})
}

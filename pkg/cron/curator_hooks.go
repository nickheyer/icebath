package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/memory"
)

// Installs the cron skill reference reader, rewriter and locked job updater the curator calls
func init() {
	memory.CuratorHooks.ReferencedSkillNames = func(ctx context.Context, h string) (map[string]bool, error) {
		return openCronStore(h, time.Now).referencedSkillNames(ctx), nil
	}
	memory.CuratorHooks.RewriteSkillRefs = func(ctx context.Context, h string, consolidated *pyjson.Object, pruned []string) (memory.CuratorCronRewriteReport, error) {
		report, err := openCronStore(h, time.Now).rewriteSkillRefs(ctx, consolidated, pruned)
		if err != nil {
			return memory.CuratorCronRewriteReport{}, err
		}
		out := memory.CuratorCronRewriteReport{JobsUpdated: report.JobsUpdated, JobsScanned: report.JobsScanned}
		for _, r := range report.Rewrites {
			out.Rewrites = append(out.Rewrites, memory.CuratorCronRewrite{JobID: r.JobID, JobName: r.JobName, Before: r.Before, After: r.After, Mapped: r.Mapped, Dropped: r.Dropped})
		}
		return out, nil
	}
	memory.CuratorHooks.UpdateJobs = func(ctx context.Context, h string, update func(jobs []any) (bool, error)) error {
		return openCronStore(h, time.Now).updateRawJobs(ctx, update)
	}
}

// Hands the stored jobs as Python dicts to update under the jobs lock and saves them when it reports a change
func (s *Store) updateRawJobs(ctx context.Context, update func(jobs []any) (bool, error)) error {
	return s.withJobsLock(home.WithOverride(ctx, s.home), func(tx *jobsTx) error {
		jobs, err := s.loadJobs(home.WithOverride(ctx, s.home), tx)
		if err != nil {
			return err
		}
		raw := make([]any, 0, len(jobs))
		for _, job := range jobs {
			obj, err := storedJobObject(job)
			if err != nil {
				return err
			}
			raw = append(raw, obj)
		}
		changed, err := update(raw)
		if err != nil || !changed {
			return err
		}
		kept := map[string]bool{}
		next := make([]*Job, 0, len(raw))
		for _, item := range raw {
			text, err := pyjson.Dumps(item, pyjson.EnsureASCII(false))
			if err != nil {
				return err
			}
			var job Job
			if err := json.Unmarshal([]byte(text), &job); err != nil {
				return fmt.Errorf("invalid cron job record: %w", err)
			}
			if obj, ok := item.(*pyjson.Object); ok {
				job.keyOrder = append([]string(nil), obj.Keys...)
				job.onlyStoredKeys = true
			}
			kept[job.ID] = true
			next = append(next, &job)
		}
		var removed []string
		for _, job := range jobs {
			if !kept[job.ID] {
				removed = append(removed, job.ID)
			}
		}
		return s.saveJobsLocked(home.WithOverride(ctx, s.home), tx, next, removed, false)
	})
}

// Record object of a job with only the keys its stored record names, absent keys staying absent
func storedJobObject(job *Job) (any, error) {
	data, err := job.MarshalJSON()
	if err != nil {
		return nil, err
	}
	v, err := pyjson.LoadsBytes(data)
	if err != nil {
		return nil, err
	}
	obj, ok := v.(*pyjson.Object)
	if !ok || job.keyOrder == nil {
		return v, nil
	}
	out := pyjson.NewObject()
	for _, k := range obj.Keys {
		if containsString(job.keyOrder, k) {
			out.Set(k, obj.Vals[k])
		}
	}
	return out, nil
}

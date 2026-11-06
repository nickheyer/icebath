package cron

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/memory"
)

// Installs the cron skill reference reader, rewriter and locked job updater the curator calls
func init() {
	memory.CuratorHooks.ReferencedSkillNames = func(h string) (map[string]bool, error) {
		return openCronStore(h, time.Now).referencedSkillNames(), nil
	}
	memory.CuratorHooks.RewriteSkillRefs = func(h string, consolidated *pyjson.Object, pruned []string) (memory.CuratorCronRewriteReport, error) {
		report, err := openCronStore(h, time.Now).rewriteSkillRefs(consolidated, pruned)
		if err != nil {
			return memory.CuratorCronRewriteReport{}, err
		}
		out := memory.CuratorCronRewriteReport{JobsUpdated: report.JobsUpdated, JobsScanned: report.JobsScanned}
		for _, r := range report.Rewrites {
			out.Rewrites = append(out.Rewrites, memory.CuratorCronRewrite{JobID: r.JobID, JobName: r.JobName, Before: r.Before, After: r.After, Mapped: r.Mapped, Dropped: r.Dropped})
		}
		return out, nil
	}
	memory.CuratorHooks.UpdateJobs = func(h string, update func(jobs []any) (bool, error)) error {
		return openCronStore(h, time.Now).updateRawJobs(update)
	}
}

// Hands the stored jobs as Python dicts to update under the jobs lock and saves them when it reports a change
func (s *Store) updateRawJobs(update func(jobs []any) (bool, error)) error {
	return s.withJobsLock(func(tx *jobsTx) error {
		jobs, err := s.loadJobs(tx)
		if err != nil {
			return err
		}
		raw := make([]any, 0, len(jobs))
		for _, job := range jobs {
			data, err := job.MarshalJSON()
			if err != nil {
				return err
			}
			obj, err := pyjson.LoadsBytes(data)
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
			kept[job.ID] = true
			next = append(next, &job)
		}
		var removed []string
		for _, job := range jobs {
			if !kept[job.ID] {
				removed = append(removed, job.ID)
			}
		}
		return s.saveJobsLocked(tx, next, removed, false)
	})
}

package cron

import (
	"context"
	"fmt"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/skills"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// SkillRefRewrite is one job whose skill references a curator pass rewrote
type SkillRefRewrite struct {
	JobID   string
	JobName string
	Before  []string
	After   []string
	Mapped  *pyjson.Object
	Dropped []string
}

// SkillRefRewriteReport summarizes a curator skill reference rewrite
type SkillRefRewriteReport struct {
	Rewrites    []SkillRefRewrite
	JobsUpdated int
	JobsScanned int
}

// Bare skill name a reference resolves to the way the scheduler resolves it
func (s *Store) canonicalSkillRef(raw string) string {
	value := pystr.Strip(raw)
	if value == "" {
		return ""
	}
	cfg, _ := s.loadConfig()
	if n := skills.NewScope(s.home, cfg).NormalizeSkillLookupName(value); n != "" {
		value = n
	}
	return strings.TrimLeft(pystr.Strip(value), "/")
}

// Skill names referenced by any job including paused ones, empty when the store is unreadable
func (s *Store) referencedSkillNames(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	jobs, err := s.loadJobs(home.WithOverride(ctx, s.home), nil)
	if err != nil {
		s.log.Debug(fmt.Sprintf("referenced_skill_names: failed to load cron jobs: %v", err))
		return out
	}
	for _, job := range jobs {
		for _, name := range normalizeSkillList(skillAny(job.Skill), skillsAny(job.Skills)) {
			if cleaned := s.canonicalSkillRef(name); cleaned != "" {
				out[cleaned] = true
			}
		}
	}
	return out
}

// Maps consolidated skill names onto their umbrella and drops pruned ones in every job, keeping order
func (s *Store) rewriteSkillRefs(ctx context.Context, consolidated *pyjson.Object, pruned []string) (SkillRefRewriteReport, error) {
	if consolidated == nil {
		consolidated = pyjson.NewObject()
	}
	prunedSet := map[string]bool{}
	for _, p := range pruned {
		if _, ok := consolidated.Get(p); !ok {
			prunedSet[p] = true
		}
	}
	report := SkillRefRewriteReport{Rewrites: []SkillRefRewrite{}}
	if len(consolidated.Keys) == 0 && len(prunedSet) == 0 {
		return report, nil
	}
	err := s.withJobsLock(home.WithOverride(ctx, s.home), func(tx *jobsTx) error {
		jobs, err := s.loadJobs(home.WithOverride(ctx, s.home), tx)
		if err != nil {
			return err
		}
		for _, job := range jobs {
			before := normalizeSkillList(skillAny(job.Skill), skillsAny(job.Skills))
			if len(before) == 0 {
				continue
			}
			mapped := pyjson.NewObject()
			dropped := []string{}
			after := []string{}
			for _, name := range before {
				if target, ok := consolidated.Get(name); ok {
					t, _ := target.(string)
					mapped.Set(name, t)
					if t != "" && !containsString(after, t) {
						after = append(after, t)
					}
				} else if prunedSet[name] {
					dropped = append(dropped, name)
				} else if !containsString(after, name) {
					after = append(after, name)
				}
			}
			if len(mapped.Keys) == 0 && len(dropped) == 0 {
				continue
			}
			job.Skills = after
			job.Skill = nil
			if len(after) > 0 {
				job.Skill = strPtr(after[0])
			}
			name := job.Name
			if name == "" {
				name = job.ID
			}
			report.Rewrites = append(report.Rewrites, SkillRefRewrite{JobID: job.ID, JobName: name,
				Before: append([]string{}, before...), After: append([]string{}, after...), Mapped: mapped, Dropped: dropped})
		}
		report.JobsUpdated, report.JobsScanned = len(report.Rewrites), len(jobs)
		if len(report.Rewrites) > 0 {
			if err := s.saveJobsLocked(home.WithOverride(ctx, s.home), tx, jobs, nil, false); err != nil {
				return err
			}
			s.log.Info(fmt.Sprintf("Curator rewrote skill references in %d cron job(s)", len(report.Rewrites)))
		}
		return nil
	})
	return report, err
}

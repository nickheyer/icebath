package worktree

import (
	"context"
	"errors"
	"strings"
	"time"
)

const (
	ToneError   = "error"
	ToneSuccess = "success"
	ToneWarning = "warning"
)

type Message struct {
	Key    string
	Args   map[string]string
	Text   string
	Tone   string
	Indent bool
	Gap    bool
}

type PruneOptions struct {
	DryRun        bool
	OlderThanDays *float64
	TreesOnly     bool
	BranchesOnly  bool
}

type PruneReport struct {
	Repo      string
	DryRun    bool
	Sweep     []*Decision
	Actions   []Message
	Preserved []*TreeRecord
}

type PruneDocument struct {
	Repo      string        `json:"repo"`
	DryRun    bool          `json:"dry_run"`
	Actions   []string      `json:"actions"`
	Preserved []*TreeRecord `json:"preserved"`
	Sweep     []*Decision   `json:"sweep"`
}

func (r *PruneReport) Document(render func(Message) string) PruneDocument {
	actions := make([]string, 0, len(r.Actions))
	for _, a := range r.Actions {
		actions = append(actions, render(a))
	}
	preserved := r.Preserved
	if preserved == nil {
		preserved = []*TreeRecord{}
	}
	sweep := r.Sweep
	if sweep == nil {
		sweep = []*Decision{}
	}
	return PruneDocument{Repo: r.Repo, DryRun: r.DryRun, Actions: actions, Preserved: preserved, Sweep: sweep}
}

type ListReport struct {
	Repo     string                `json:"repo"`
	Trees    []*TreeRecord         `json:"trees"`
	External []*ExternalTreeRecord `json:"external_trees"`
	Branches []*BranchRecord       `json:"branches"`
	Archives []*Archive            `json:"archives"`
}

func textMessages(lines []string) []Message {
	out := make([]Message, 0, len(lines))
	for _, l := range lines {
		out = append(out, Message{Text: l})
	}
	return out
}

func (m *Manager) List(ctx context.Context, repoRoot string, olderThanDays *float64) (*ListReport, error) {
	trees, err := m.AuditWorktrees(ctx, repoRoot, true, olderThanDays)
	if err != nil {
		return nil, err
	}
	external, err := m.AuditExternalTrees(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	branches, err := m.AuditBranches(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	archives, err := m.ListArchives(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	report := &ListReport{Repo: repoRoot, Trees: trees, External: external, Branches: branches, Archives: archives}
	if report.Trees == nil {
		report.Trees = []*TreeRecord{}
	}
	if report.External == nil {
		report.External = []*ExternalTreeRecord{}
	}
	if report.Branches == nil {
		report.Branches = []*BranchRecord{}
	}
	if report.Archives == nil {
		report.Archives = []*Archive{}
	}
	return report, nil
}

func (m *Manager) attendedRecords(records []*TreeRecord, plan []*Decision, settings Settings) ([]*TreeRecord, error) {
	if settings.Classic() {
		return records, nil
	}
	owned := map[string]bool{}
	for _, d := range plan {
		if d.Action != "skip-young" {
			owned[d.Path] = true
		}
	}
	var remaining []*TreeRecord
	for _, r := range records {
		if !owned[r.Path] {
			remaining = append(remaining, r)
		}
	}
	paths := make([]string, 0, len(remaining))
	for _, r := range remaining {
		paths = append(paths, r.Path)
	}
	live, err := m.LivePIDs(paths, nil)
	if err != nil {
		return nil, err
	}
	var out []*TreeRecord
	for _, r := range remaining {
		if len(live[r.Path]) == 0 {
			out = append(out, r)
		}
	}
	return out, nil
}

func preservedRecords(records []*TreeRecord) []*TreeRecord {
	var kept []*TreeRecord
	for _, r := range records {
		if r.Verdict == "keep" && !strings.Contains(r.Reason, "kanban") && !strings.Contains(r.Reason, "in use") {
			kept = append(kept, r)
		}
	}
	return kept
}

func (m *Manager) Prune(ctx context.Context, repoRoot string, o PruneOptions) (*PruneReport, error) {
	report := &PruneReport{Repo: repoRoot, DryRun: o.DryRun}
	var errs []error
	if !o.BranchesOnly {
		settings, err := m.Settings(ctx)
		if err != nil {
			return nil, err
		}
		plan, err := m.PlanSweep(ctx, repoRoot, 24, settings, o.OlderThanDays)
		if err != nil {
			return nil, err
		}
		report.Sweep = plan
		if !o.DryRun && !settings.Classic() {
			_, _, archived, err := m.ExecutePlan(ctx, repoRoot, plan, settings)
			if err != nil {
				return nil, err
			}
			for _, a := range archived {
				if a.Removed {
					report.Actions = append(report.Actions, Message{Key: "cli.worktree.sweep_archived_removed", Args: map[string]string{"ref": a.Ref}})
					continue
				}
				why := a.Why
				if why == "" {
					why = "?"
				}
				report.Actions = append(report.Actions, Message{Key: "cli.worktree.sweep_archived_kept", Args: map[string]string{"ref": a.Ref, "reason": why}})
			}
			expired, err := m.PruneExpiredArchives(ctx, repoRoot, settings.ArchiveRetentionDays, false)
			if err != nil {
				if fatal(ctx, err) {
					return nil, err
				}
				errs = append(errs, err)
			}
			for _, ref := range expired {
				report.Actions = append(report.Actions, Message{Text: "deleted expired archive " + ref})
			}
		}
		regs, err := m.PruneMissingRegistrations(ctx, repoRoot, o.DryRun)
		if err != nil {
			return nil, err
		}
		report.Actions = append(report.Actions, textMessages(regs)...)
		audited, err := m.AuditWorktrees(ctx, repoRoot, false, o.OlderThanDays)
		if err != nil {
			return nil, err
		}
		records, err := m.attendedRecords(audited, plan, settings)
		if err != nil {
			return nil, err
		}
		reclaimed, err := m.ReclaimWorktrees(ctx, repoRoot, o.DryRun, records)
		report.Actions = append(report.Actions, textMessages(reclaimed)...)
		if err != nil {
			return nil, err
		}
		report.Preserved = preservedRecords(records)
	}
	if !o.TreesOnly {
		branches, err := m.AuditBranches(ctx, repoRoot)
		if err != nil {
			return nil, err
		}
		deleted, err := m.ReclaimBranches(ctx, repoRoot, o.DryRun, branches)
		report.Actions = append(report.Actions, textMessages(deleted)...)
		if err != nil {
			return nil, err
		}
	}
	return report, errors.Join(errs...)
}

type SlashPruneReport struct {
	DryRun    bool
	Actions   []string
	Preserved []*TreeRecord
}

func (m *Manager) SlashPrune(ctx context.Context, repoRoot, activePath string, dryRun bool) (*SlashPruneReport, error) {
	audited, err := m.AuditWorktrees(ctx, repoRoot, false, nil)
	if err != nil {
		return nil, err
	}
	var records []*TreeRecord
	for _, r := range audited {
		if activePath == "" || r.Path != activePath {
			records = append(records, r)
		}
	}
	actions, err := m.ReclaimWorktrees(ctx, repoRoot, dryRun, records)
	if err != nil {
		return nil, err
	}
	branches, err := m.AuditBranches(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	deleted, err := m.ReclaimBranches(ctx, repoRoot, dryRun, branches)
	if err != nil {
		return nil, err
	}
	return &SlashPruneReport{DryRun: dryRun, Actions: append(actions, deleted...), Preserved: preservedRecords(records)}, nil
}

func (m *Manager) ListPlain(ctx context.Context, repoRoot string) (string, error) {
	r, err := m.git(ctx, repoRoot, 10*time.Second, nil, "worktree", "list")
	if err != nil {
		return "", err
	}
	if !r.ok() {
		return "", &GitError{Args: r.args, Code: r.code, Stderr: r.errText()}
	}
	return r.out(), nil
}

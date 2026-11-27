package skills

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/strs"
)

// Refusal a locked skill's change gets, the same the file and terminal tools give
func LockRefusal(name string) string {
	return fmt.Sprintf("Skill '%s' is locked by the user, so the agent cannot change it: skill_manage, write_file, patch and terminal all refuse. Don't retry or work around it (no copying it under another name); tell the user what you would change. They can unlock it with `icebath curator unlock %s`.", name, name)
}

// Guards every change runs first: the user's content lock, and for a delete the background review's ownership guard
func (s *Scope) preflight(ctx context.Context, action, name string) Result {
	if refused := s.lockGuard(ctx, name, action); refused != nil {
		return refused
	}
	if action != "delete" {
		return nil
	}
	if dir, ok := s.Find(name); ok {
		return s.reviewDeleteGuard(ctx, name, dir)
	}
	return nil
}

// Refuses any change to a skill the user locked; when the lock can't be read only a background review is refused
func (s *Scope) lockGuard(ctx context.Context, name, action string) Result {
	if action == "create" || name == "" {
		return nil
	}
	candidates := []string{name, filepath.Base(name)}
	if dir, ok := s.Find(name); ok {
		candidates = append(candidates, filepath.Base(dir))
	}
	for _, candidate := range uniqueStrings(candidates...) {
		locked, err := s.IsLocked(candidate)
		if err != nil {
			if reviewing(ctx) == nil {
				return nil
			}
			return failure(fmt.Sprintf("Refusing %s for skill '%s': its lock state could not be verified because the skills/.usage.json sidecar is unreadable. Leave the skill alone and tell the user; they can repair the sidecar or run `icebath curator unlock %s`.", action, name, name), Result{"_locked": true})
		}
		if locked {
			return failure(fmt.Sprintf("Refusing %s: %s", action, LockRefusal(candidate)), Result{"_locked": true})
		}
	}
	return nil
}

// Refuses a background review's delete of anything but a skill the curator manages
func (s *Scope) reviewDeleteGuard(ctx context.Context, name, dir string) Result {
	if reviewing(ctx) == nil {
		return nil
	}
	const refuse = "Refusing background curator delete for"
	if record, err := s.Record(name); err == nil && record.Pinned {
		return failure(fmt.Sprintf("%s pinned skill '%s': pinned skills are off-limits to autonomous maintenance. Ask the user to run `icebath curator unpin %s` if they want it changed.", refuse, name, name), nil)
	}
	if s.ExternalPath(dir) {
		return failure(fmt.Sprintf("%s skill '%s': the skill lives in skills.external_dirs, which are externally owned and read-only to autonomous curation.", refuse, name), nil)
	}
	unverified := failure(fmt.Sprintf("%s skill '%s': agent ownership could not be verified because the provenance record is unavailable or unreadable.", refuse, name), nil)
	if s.HubInstalled(name) {
		return failure(fmt.Sprintf("%s hub-installed skill '%s'.", refuse, name), nil)
	}
	bundled, err := s.Bundled(name)
	if err != nil {
		return unverified
	}
	if bundled {
		return failure(fmt.Sprintf("%s bundled skill '%s'.", refuse, name), nil)
	}
	usage, err := s.Usage()
	if err != nil {
		return unverified
	}
	if record := usage[name]; !record.Managed() {
		detail := "no usage record"
		if record != nil {
			detail = "created_by=" + strings.TrimSpace(record.CreatedBy)
			if record.CreatedBy == "" {
				detail = "created_by=None"
			}
		}
		return failure(fmt.Sprintf("%s skill '%s': the skill is not curator-managed (%s). Only curator-managed skills may be archived autonomously. Run `icebath curator adopt %s` to opt it in.", refuse, name, detail, name), nil)
	}
	return nil
}

// Refuses a background review's change of a file it has not read in this review
func (s *Scope) readGuard(ctx context.Context, name, target, action, label string) Result {
	if reviewing(ctx) == nil || readInReview(ctx, target) {
		return nil
	}
	return failure(fmt.Sprintf("Refusing background curator %s for skill '%s': the current %s content has not been loaded in this review turn. Call skill_view(name) for SKILL.md, or skill_view(name, file_path=...) for a supporting file, then retry the write using the content just returned.", action, name, label), Result{"_read_before_write_required": true})
}

// Refuses a background review's delete that names no skill the deleted one was merged into
func (s *Scope) consolidationGuard(ctx context.Context, name string, absorbed *string) Result {
	if reviewing(ctx) == nil || strings.TrimSpace(strs.Deref(absorbed)) != "" {
		return nil
	}
	return failure(fmt.Sprintf("Refusing background curator delete of skill '%s': the consolidation pass may only archive a skill it has absorbed into an umbrella. Pass absorbed_into=<umbrella> (the umbrella must already exist) to record a verified consolidation. Pruning a skill with no forwarding target is not permitted here — the deterministic inactivity prune handles staleness archival separately. Keeping '%s' active.", name, name), Result{"_fail_closed": true})
}

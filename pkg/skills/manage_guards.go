package skills

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Ordered result object built from alternating keys and values
func resultObj(kv ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

// Failed skill_manage result carrying the message and any extra keys
func refusal(message string, extra ...any) *pyjson.Object {
	return resultObj(append([]any{"success", false, "error", message}, extra...)...)
}

// Registry style error result with success false
func toolErrorObj(message string) *pyjson.Object {
	return resultObj("error", tools.BoundErrorText(message), "success", false)
}

// Registry error a raised Python exception becomes at dispatch
func raisedObj(err error) *pyjson.Object {
	class, msg := "OSError", err.Error()
	var typed interface{ PyTypeName() string }
	var pe *fs.PathError
	switch {
	case errors.As(err, &typed):
		class = typed.PyTypeName()
	case errors.As(err, &pe):
		class, msg = pypath.OSErrorClass(err), pyOSErrorText(err)
	}
	return resultObj("error", tools.BoundErrorText(tools.SanitizeToolError(fmt.Sprintf("Tool execution failed: %s: %s", class, msg))))
}

// Truthiness of a skill record value
func objTruthy(o *pyjson.Object, key string) bool {
	if o == nil {
		return false
	}
	v, _ := o.Get(key)
	return pyrepr.Truthy(v)
}

// Resolved skill path plus every skills root paired with its resolved form
func (s *Scope) resolvedRoots(skillPath string) (string, [][2]string) {
	resolved := resolvePath(skillPath)
	var roots [][2]string
	for _, root := range s.AllSkillsDirs() {
		roots = append(roots, [2]string{root, resolvePath(root)})
	}
	return resolved, roots
}

// Skills root holding the path, the local skills dir when no root matches
func (s *Scope) containingSkillsRoot(skillPath string) string {
	resolved, roots := s.resolvedRoots(skillPath)
	for _, r := range roots {
		if isRelativeTo(resolved, r[1]) {
			return r[0]
		}
	}
	return s.SkillsDir()
}

// Reports a symlink or junction that would let a recursive delete leave the tree
func isPathRedirect(p string) bool {
	st, err := os.Lstat(p)
	if err != nil {
		return false
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return true
	}
	return runtime.GOOS == "windows" && st.Mode()&os.ModeIrregular != 0
}

// Refusal text for a delete target outside every skills root, a root itself, or a link
func (s *Scope) validateDeleteTarget(skillDir string) string {
	if isPathRedirect(skillDir) {
		return fmt.Sprintf("Refusing to delete '%s': the skill directory is a symlink/junction. Remove the link target manually if intended.", skillDir)
	}
	if _, err := filepath.EvalSymlinks(skillDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Sprintf("Refusing to delete '%s': could not resolve path (%s).", skillDir, pyOSErrorText(err))
	}
	resolved, roots := s.resolvedRoots(skillDir)
	for _, r := range roots {
		if resolved == r[1] {
			return fmt.Sprintf("Refusing to delete '%s': resolves to the skills root itself, which would remove every installed skill.", skillDir)
		}
		if isRelativeTo(resolved, r[1]) {
			return ""
		}
	}
	return fmt.Sprintf("Refusing to delete '%s': path does not resolve inside any known skills root.", skillDir)
}

// Pinned flag of the usage record, ok false when the record is unreadable
func (s *Scope) isPinned(name, what string) (bool, bool) {
	rec, err := s.GetRecord(name)
	if err != nil {
		s.logger().Debug(fmt.Sprintf("%s lookup failed for %s", what, name), "error", err)
		return false, false
	}
	return objTruthy(rec, "pinned"), true
}

// Refusal text when the skill is essential or pinned, deletion being the only blocked change
func (s *Scope) pinnedGuard(name string) string {
	if EssentialSkills[name] {
		return fmt.Sprintf("Skill '%s' is essential to Icebath (the agent's own operating manual referenced by the system prompt) and cannot be deleted. Patches and edits are still allowed.", name)
	}
	if pinned, _ := s.isPinned(name, "pinned-guard"); pinned {
		return fmt.Sprintf("Skill '%s' is pinned and cannot be deleted by skill_manage. Ask the user to run `icebath curator unpin %s` if they want to delete it. Patches and edits are allowed on pinned skills; only deletion is blocked.", name, name)
	}
	return ""
}

// Refuses review fork deletes of anything but curator managed agent skills
func (s *Scope) backgroundReviewDeleteGuard(name, skillDir string) *pyjson.Object {
	if !IsBackgroundReview(s.Env) {
		return nil
	}
	refuse := "Refusing background curator delete for"
	if pinned, _ := s.isPinned(name, "pinned skill guard"); pinned {
		return refusal(fmt.Sprintf("%s pinned skill '%s': pinned skills are off-limits to autonomous maintenance. Ask the user to run `icebath curator unpin %s` if they want it changed.", refuse, name, name))
	}
	if s.isExternalSkillPath(skillDir) {
		return refusal(fmt.Sprintf("%s skill '%s': the skill lives in skills.external_dirs, which are externally owned and read-only to autonomous curation.", refuse, name))
	}
	unverified := func(err error) *pyjson.Object {
		s.logger().Warn(fmt.Sprintf("owned skill guard lookup failed for %s", name), "error", err)
		return refusal(fmt.Sprintf("%s skill '%s': agent ownership could not be verified because the provenance record is unavailable or unreadable.", refuse, name))
	}
	if IsProtectedBuiltin(name) {
		return refusal(fmt.Sprintf("%s protected built-in skill '%s'.", refuse, name))
	}
	if s.IsHubInstalled(name) {
		return refusal(fmt.Sprintf("%s hub-installed skill '%s'.", refuse, name))
	}
	bundled, err := s.IsBundled(name)
	if err != nil {
		return unverified(err)
	}
	if bundled {
		return refusal(fmt.Sprintf("%s bundled skill '%s'.", refuse, name))
	}
	usage, err := s.LoadUsage()
	if err != nil {
		return unverified(err)
	}
	rec, _ := usage.Get(name)
	if !isCuratorManagedRecord(rec) {
		detail := "no usage record"
		if obj, ok := rec.(*pyjson.Object); ok {
			createdBy, _ := obj.Get("created_by")
			detail = "created_by=" + pyrepr.Repr(createdBy)
		}
		return refusal(fmt.Sprintf("%s skill '%s': the skill is not curator-managed (%s). Only curator-managed skills may be archived autonomously. Run `icebath curator adopt %s` to opt it in.", refuse, name, detail, name))
	}
	return nil
}

// Requires the review fork to have loaded the exact target before mutating it
func (s *Scope) backgroundReviewReadBeforeWriteGuard(name, target, action, fileLabel string) *pyjson.Object {
	if !IsBackgroundReview(s.Env) || backgroundReviewHasRead(s.Env, target) {
		return nil
	}
	return refusal(fmt.Sprintf("Refusing background curator %s for skill '%s': the current %s content has not been loaded in this review turn. Call skill_view(name) for SKILL.md, or skill_view(name, file_path=...) for a supporting file, then retry the write using the content just returned.", action, name, fileLabel),
		"_read_before_write_required", true)
}

// Refuses every mutation of a user locked skill, failing closed only for autonomous passes
func (s *Scope) lockedGuard(name, action string) *pyjson.Object {
	if action == "create" || name == "" {
		return nil
	}
	candidates := []string{name, filepath.Base(name)}
	if dir, ok := s.FindSkill(name); ok {
		candidates = append(candidates, filepath.Base(dir))
	}
	seen := map[string]bool{}
	locked := ""
	for _, c := range candidates {
		if seen[c] {
			continue
		}
		seen[c] = true
		isLocked, err := s.IsLocked(c)
		if err != nil {
			s.logger().Warn(fmt.Sprintf("locked skill guard lookup failed for %s", name), "error", err)
			if !IsBackgroundReview(s.Env) {
				return nil
			}
			return refusal(fmt.Sprintf("Refusing %s for skill '%s': its lock state could not be verified because the skills/.usage.json sidecar is unreadable. Leave the skill alone and tell the user; they can repair the sidecar or run `icebath curator unlock %s`.", action, name, name), "_locked", true)
		}
		if isLocked {
			locked = c
			break
		}
	}
	if locked != "" {
		return refusal(fmt.Sprintf("Refusing %s: %s", action, tools.SkillLockRefusal(locked)), "_locked", true)
	}
	return nil
}

// Lock guard for every mutating action plus the ownership guard for deletes, run before any handler
func (s *Scope) backgroundReviewPreflight(action, name string) *pyjson.Object {
	if locked := s.lockedGuard(name, action); locked != nil {
		return locked
	}
	if action != "delete" {
		return nil
	}
	if dir, ok := s.FindSkill(name); ok {
		return s.backgroundReviewDeleteGuard(name, dir)
	}
	return nil
}

// Fails closed on review fork deletes that name no umbrella the skill was absorbed into
func (s *Scope) curatorConsolidationDeleteGuard(name string, absorbedInto *string) *pyjson.Object {
	if !IsBackgroundReview(s.Env) || (absorbedInto != nil && pystr.Strip(*absorbedInto) != "") {
		return nil
	}
	return refusal(fmt.Sprintf("Refusing background curator delete of skill '%s': the consolidation pass may only archive a skill it has absorbed into an umbrella. Pass absorbed_into=<umbrella> (the umbrella must already exist) to record a verified consolidation. Pruning a skill with no forwarding target is not permitted here — the deterministic inactivity prune handles staleness archival separately. Keeping '%s' active.", name, name),
		"_fail_closed", true)
}

// Starts a fresh read set for the current review run
func ResetBackgroundReviewReadMarks(env *tools.Env) {
	if env == nil || env.Values == nil {
		return
	}
	env.Values.Store(reviewReadMarksKey, &reviewReadMarks{paths: map[string]bool{}})
}

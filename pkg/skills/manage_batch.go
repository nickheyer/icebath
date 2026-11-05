package skills

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const batchMaxOps = 20

// Actions an atomic batch accepts, delete only ever running alone
var batchOpActions = map[string]bool{"create": true, "patch": true, "write_file": true, "remove_file": true}

const (
	patchNeedsOldString = "old_string is required for 'patch' and must be the EXACT text currently in the file. Read the target file first (read_file on the skill's SKILL.md, or the file named by file_path) and copy the snippet verbatim, then retry 'patch'. Do NOT fall back to action='write_file' — that rewrites the entire file and destroys unrelated content."
	patchNeedsNewString = "new_string is required for 'patch'. Use an empty string to delete matched text."
	patchEitherOr       = "Pass EITHER content (full SKILL.md rewrite) OR old_string/new_string (targeted replacement), not both."
)

// Required argument checks per action, missing meaning falsy or null
var requiredArgs = map[string][]struct {
	arg     string
	isNone  bool
	message string
}{
	"create":      {{"content", false, "content is required for 'create'. Provide the full SKILL.md text (frontmatter + body)."}},
	"edit":        {{"content", false, "content is required for a full rewrite. Provide the full updated SKILL.md text."}},
	"write_file":  {{"file_path", false, "file_path is required for 'write_file'. Example: 'references/api-guide.md'"}, {"file_content", true, "file_content is required for 'write_file'."}},
	"remove_file": {{"file_path", false, "file_path is required for 'remove_file'."}},
}

// Text slot keys mapped to the action that reads them, in upstream order
var textSlotOwner = [][2]string{
	{"content", "create (and a full-rewrite patch)"},
	{"new_string", "a targeted patch (with old_string)"},
	{"file_content", "write_file"},
}

// Text slots each action reads and where misfiled text belongs
var textSlotFor = map[string]struct {
	reads       []string
	destination string
}{
	"create":     {[]string{"content"}, "'content'"},
	"edit":       {[]string{"content"}, "'content'"},
	"patch":      {[]string{"content", "new_string"}, "old_string/new_string (targeted) or 'content' (full rewrite, last resort)"},
	"write_file": {[]string{"file_content"}, "'file_content'"},
}

// Sentence naming text slot keys the op carries that the action never reads
func misplacedTextHint(action string, args map[string]any) string {
	slot, ok := textSlotFor[action]
	if !ok {
		return ""
	}
	var carried []string
	for _, kv := range textSlotOwner {
		reads := false
		for _, r := range slot.reads {
			reads = reads || r == kv[0]
		}
		if v, present := args[kv[0]]; !reads && present && v != nil {
			carried = append(carried, fmt.Sprintf("'%s' (that key is for %s)", kv[0], kv[1]))
		}
	}
	if len(carried) == 0 {
		return ""
	}
	return fmt.Sprintf(" Note: this op carries %s — move that text to %s.", strings.Join(carried, " and "), slot.destination)
}

// Argument shape error of one op, empty when the op is well formed
func opShapeError(action string, args map[string]any) string {
	for _, req := range requiredArgs[action] {
		v := args[req.arg]
		if (req.isNone && v == nil) || (!req.isNone && !pyrepr.Truthy(v)) {
			return req.message + misplacedTextHint(action, args)
		}
	}
	if action == "patch" {
		content, old := pyrepr.Truthy(args["content"]), pyrepr.Truthy(args["old_string"])
		if content && (old || args["new_string"] != nil) {
			return patchEitherOr
		}
		if !old && !content {
			return patchNeedsOldString + misplacedTextHint(action, args)
		}
		if !content && args["new_string"] == nil {
			return patchNeedsNewString
		}
	}
	return ""
}

// Name an op targets, its own truthy name else the default, a TypeError for a non string name
func opName(op map[string]any, defaultName string) (string, error) {
	v := op["name"]
	if !pyrepr.Truthy(v) {
		return defaultName, nil
	}
	name, ok := v.(string)
	if !ok {
		return "", &manageTypeError{msg: fmt.Sprintf("expected str, bytes or os.PathLike object, not %s", pyTypeName(v))}
	}
	return name, nil
}

// Validated op maps and the target names, else the error result
func (s *Scope) validateBatchOps(operations []any, defaultName string) ([]map[string]any, []string, *pyjson.Object, error) {
	fail := func(i int, msg string) ([]map[string]any, []string, *pyjson.Object, error) {
		return nil, nil, toolErrorObj(fmt.Sprintf("operations[%d]%s", i, msg)), nil
	}
	ops := make([]map[string]any, len(operations))
	var names []string
	for i, raw := range operations {
		op, ok := raw.(map[string]any)
		if !ok || !pyrepr.Truthy(op["action"]) {
			return fail(i, " needs an 'action'.")
		}
		ops[i] = op
		act, _ := op["action"].(string)
		if !batchOpActions[act] {
			acts := make([]string, 0, len(batchOpActions))
			for a := range batchOpActions {
				acts = append(acts, a)
			}
			sort.Strings(acts)
			return fail(i, fmt.Sprintf(": unknown action '%s'. Batchable: %s; delete must be sole.", pyStrValue(op["action"]), strings.Join(acts, ", ")))
		}
		nm, err := opName(op, defaultName)
		if err != nil {
			return nil, nil, nil, err
		}
		if nm == "" {
			return fail(i, " needs a 'name' (the skill it targets).")
		}
		if msg := opShapeError(act, op); msg != "" {
			return fail(i, fmt.Sprintf(" (%s on '%s'): %s", act, nm, msg))
		}
		if act == "create" {
			if msg := validateSkillCategory(op["category"]); msg != "" {
				return fail(i, fmt.Sprintf(" (%s on '%s'): %s", act, nm, msg))
			}
		}
		for _, prev := range names {
			if act == "create" && prev == nm {
				return fail(i, fmt.Sprintf(": create for '%s' must precede that skill's other ops.", nm))
			}
		}
		names = append(names, nm)
		if pre := s.backgroundReviewPreflight(act, nm); pre != nil {
			return nil, nil, pre, nil
		}
	}
	touched := map[[2]string]bool{}
	for i, op := range ops {
		act, nm := op["action"].(string), names[i]
		fullRewrite := act == "patch" && pyrepr.Truthy(op["content"])
		fp := ""
		if v := op["file_path"]; pyrepr.Truthy(v) {
			str, ok := v.(string)
			if !ok {
				return nil, nil, nil, &manageTypeError{msg: fmt.Sprintf("'%s' object has no attribute 'strip'", pyTypeName(v))}
			}
			fp = strings.TrimSpace(str)
		}
		target := "SKILL.md"
		if !(act == "create" || fullRewrite || fp == "") {
			target = path.Clean(strings.TrimLeft(fp, "/"))
		}
		key := [2]string{nm, target}
		if (act == "create" || act == "write_file" || act == "remove_file" || fullRewrite) && touched[key] {
			return fail(i, fmt.Sprintf(": %s on '%s' of skill '%s' — an earlier op in this batch already touched that file, and this op would silently discard its work. One destructive op (write_file/remove_file/full rewrite) per file per batch; put it first, or fold the change in. Patch chains are fine.", act, target, nm))
		}
		touched[key] = true
	}
	return ops, names, nil, nil
}

// Pre batch copy of one touched skill
type skillSnapshot struct {
	name          string
	preDir        string
	snap          string
	dirPreExisted bool
}

// Snapshot copy and per op dispatch of a batch, replaced by the batch tests
var batchSteps = struct {
	copyTree func(src, dst string) error
	applyOp  func(s *Scope, ctx context.Context, payload map[string]any, taskID, sessionID string) (*pyjson.Object, error)
}{
	copyTree: func(src, dst string) error { return pyfs.CopyTree(src, dst, false, false, nil) },
}

func init() { batchSteps.applyOp = applyBatchOp }

// Parses one batch op payload and runs it through the single op dispatcher
func applyBatchOp(s *Scope, ctx context.Context, payload map[string]any, taskID, sessionID string) (*pyjson.Object, error) {
	a, err := parseManageArgs(payload, taskID, sessionID)
	if err != nil {
		return nil, err
	}
	return s.skillManage(ctx, a)
}

// Copies every touched skill aside, the error text when a copy fails
func (s *Scope) snapshotSkills(names []string, snapRoot string, createTargets map[string]string) ([]skillSnapshot, string) {
	var out []skillSnapshot
	seen := map[string]bool{}
	for _, nm := range names {
		if seen[nm] {
			continue
		}
		seen[nm] = true
		preDir, found := s.FindSkill(nm)
		snap := ""
		if found && isDir(preDir) {
			snap = filepath.Join(snapRoot, nm)
			if err := batchSteps.copyTree(preDir, snap); err != nil {
				return nil, fmt.Sprintf("Could not snapshot '%s' for atomic batch: %s", nm, pyErrText(err))
			}
		}
		target, ok := createTargets[nm]
		preExisted := !found && ok && isDir(target)
		out = append(out, skillSnapshot{name: nm, preDir: preDir, snap: snap, dirPreExisted: preExisted})
	}
	return out, ""
}

// Python str of an error raised from a filesystem helper
func pyErrText(err error) string {
	if _, ok := err.(*pyfs.CopyTreeError); ok {
		return err.Error()
	}
	return pyOSErrorText(err)
}

// Ignores every rmtree failure the way ignore_errors does
func ignoreRmTree(pyfs.RmTreeOp, string, error) error { return nil }

// Puts one skill back to its snapshot, removing batch created content
func restoreSnapshot(sn skillSnapshot, postDir string, written []string) error {
	postExists := postDir != "" && isDir(postDir)
	if sn.snap == "" {
		if !postExists {
			return nil
		}
		if !sn.dirPreExisted {
			return pyfs.RmTree(postDir, nil)
		}
		for _, rel := range append([]string{"SKILL.md"}, written...) {
			target := filepath.Join(postDir, filepath.FromSlash(rel))
			os.Remove(target)
			for parent := filepath.Dir(target); parent != postDir && isRelativeTo(parent, postDir); parent = filepath.Dir(parent) {
				os.Remove(parent)
			}
		}
		os.Remove(postDir)
		return nil
	}
	if !postExists {
		return batchSteps.copyTree(sn.snap, sn.preDir)
	}
	aside := filepath.Join(filepath.Dir(postDir), filepath.Base(postDir)+".rollback-broken")
	pyfs.RmTree(aside, ignoreRmTree)
	if err := os.Rename(postDir, aside); err != nil {
		return err
	}
	if err := batchSteps.copyTree(sn.snap, sn.preDir); err != nil {
		pyfs.RmTree(sn.preDir, ignoreRmTree)
		os.Rename(aside, sn.preDir)
		return err
	}
	pyfs.RmTree(aside, ignoreRmTree)
	return nil
}

// Applied batch op row
type batchResult struct {
	name, action string
	filePath     any
	advisory     [][2]any
}

// Restores every snapshot, returning the note and whether any restore failed
func (s *Scope) rollbackBatch(snapshots []skillSnapshot, results []batchResult) (string, bool) {
	var notes []string
	for _, sn := range snapshots {
		var written []string
		for _, r := range results {
			fp, _ := r.filePath.(string)
			if r.name == sn.name && r.action == "write_file" && fp != "" {
				written = append(written, path.Clean(strings.TrimLeft(fp, "/")))
			}
		}
		post, _ := s.FindSkill(sn.name)
		if err := restoreSnapshot(sn, post, written); err != nil {
			note := fmt.Sprintf("ROLLBACK FAILED for '%s' (%s)", sn.name, pyErrText(err))
			if sn.snap != "" {
				note += fmt.Sprintf("; snapshot preserved at '%s'", sn.snap)
			}
			notes = append(notes, note)
		}
	}
	if len(notes) == 0 {
		return "all touched skills rolled back", false
	}
	return strings.Join(notes, "; "), true
}

// Applies operations atomically, rolling every touched skill back on any failure
func (s *Scope) skillManageBatch(ctx context.Context, operations any, defaultName, taskID, sessionID string) (*pyjson.Object, error) {
	list, ok := operations.([]any)
	if !ok || len(list) == 0 {
		return toolErrorObj("operations must be a non-empty array."), nil
	}
	if len(list) > batchMaxOps {
		return toolErrorObj(fmt.Sprintf("operations is capped at %d ops per call.", batchMaxOps)), nil
	}
	for _, raw := range list {
		if op, ok := raw.(map[string]any); ok && op["action"] == "delete" {
			if len(list) != 1 {
				return toolErrorObj("delete must be the SOLE op in its call — it doesn't compose with other ops' rollback."), nil
			}
			first, _ := list[0].(map[string]any)
			nm, err := opName(first, defaultName)
			if err != nil {
				return nil, err
			}
			if nm == "" {
				return toolErrorObj("operations[0] (delete) needs a 'name'."), nil
			}
			absorbed, err := optStringArg(first, "absorbed_into")
			if err != nil {
				return nil, err
			}
			return s.skillManage(ctx, manageArgs{action: "delete", name: nm, absorbedInto: absorbed, taskID: taskID, sessionID: sessionID})
		}
	}
	ops, names, failed, err := s.validateBatchOps(list, defaultName)
	if err != nil || failed != nil {
		return failed, err
	}
	if !gateBypassed(ctx) {
		staged := s.runWriteGate(ctx, func() (map[string]any, string) {
			acts := make([]string, len(ops))
			for i, op := range ops {
				acts[i] = op["action"].(string)
			}
			unique := map[string]bool{}
			var sorted []string
			for _, n := range names {
				if !unique[n] {
					unique[n] = true
					sorted = append(sorted, n)
				}
			}
			sort.Strings(sorted)
			gist := fmt.Sprintf("batch(%d ops: %s) on %s", len(ops), strings.Join(acts, ", "), strings.Join(sorted, ", "))
			return map[string]any{"action": "batch", "operations": operations}, gist
		})
		if staged != nil {
			return staged, nil
		}
	}
	lockedCtx, release, err := s.skillMutationLocks(ctx, names)
	if err != nil {
		return nil, err
	}
	defer release()
	snapRoot, err := os.MkdirTemp("", "skill_batch_")
	if err != nil {
		return nil, err
	}
	createTargets := map[string]string{}
	for i, op := range ops {
		if op["action"] == "create" {
			cat, _ := op["category"].(string)
			createTargets[names[i]] = s.ResolveSkillDir(names[i], cat)
		}
	}
	snapshots, snapErr := s.snapshotSkills(names, snapRoot, createTargets)
	if snapErr != "" {
		pyfs.RmTree(snapRoot, ignoreRmTree)
		return toolErrorObj(snapErr), nil
	}
	rollbackFailed := false
	defer func() {
		if rollbackFailed {
			s.logger().Warn(fmt.Sprintf("skill_manage batch rollback failed, snapshots kept at %s", snapRoot))
		} else {
			pyfs.RmTree(snapRoot, ignoreRmTree)
		}
	}()
	bypassCtx := context.WithValue(lockedCtx, skillGateBypassKey{}, true)
	var results []batchResult
	for i, op := range ops {
		payload := make(map[string]any, len(op)+2)
		for k, v := range op {
			payload[k] = v
		}
		payload["name"], payload["operations"] = names[i], nil
		parsed, err := batchSteps.applyOp(s, bypassCtx, payload, taskID, sessionID)
		if err != nil {
			return nil, err
		}
		if !objTruthy(parsed, "success") {
			var note string
			note, rollbackFailed = s.rollbackBatch(snapshots, results)
			errText := "unknown error"
			if v, ok := parsed.Get("error"); ok {
				errText = pyStrValue(v)
			}
			fail := resultObj("success", false,
				"error", fmt.Sprintf("operations[%d] (%s on '%s') failed: %s — batch aborted, %s.", i, op["action"], names[i], errText, note),
				"failed_index", i, "completed_before_failure", i)
			for _, k := range parsed.Keys {
				if k == "success" || k == "error" || parsed.Vals[k] == nil {
					continue
				}
				if _, exists := fail.Get(k); !exists {
					fail.Set(k, parsed.Vals[k])
				}
			}
			return fail, nil
		}
		r := batchResult{name: names[i], action: op["action"].(string), filePath: op["file_path"]}
		for _, k := range []string{"lint_warnings", "lint_hint"} {
			if v, ok := parsed.Get(k); ok && v != nil {
				r.advisory = append(r.advisory, [2]any{k, v})
			}
		}
		results = append(results, r)
	}
	rows := make([]any, 0, len(results))
	for _, r := range results {
		row := resultObj("name", r.name, "action", r.action, "file_path", r.filePath, "success", true)
		for _, kv := range r.advisory {
			row.Set(kv[0].(string), kv[1])
		}
		rows = append(rows, row)
	}
	return resultObj("success", true, "operations_applied", len(results), "results", rows), nil
}

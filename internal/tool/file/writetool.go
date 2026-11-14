package file

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/internal/tool"
)

const (
	rewriteHintMinChars     = 20_000
	rewriteHintMaxChars     = 400_000
	rewriteHintMinUnchanged = 0.80
	patchFailureEscalation  = 3
)

var (
	v4aSingleHeader = regexp.MustCompile(`(?m)^(\*\*\*[` + spaceClass + `]*(Update|Add|Delete)[` + spaceClass + `]+File:[` + spaceClass + `]*)(.+)$`)
	v4aMoveHeader   = regexp.MustCompile(`(?m)^(\*\*\*[` + spaceClass + `]*Move[` + spaceClass + `]+File:[` + spaceClass + `]*)(.+?)[` + spaceClass + `]*->[` + spaceClass + `]*(.+)$`)
)

type patchRequest struct {
	mode         string
	path         string
	oldString    string
	newString    string
	hasOld       bool
	hasNew       bool
	replaceAll   bool
	patch        string
	crossProfile bool
}

func rewritePatchPaths(patch string, resolved, entry map[string]string) string {
	pick := func(raw string, useEntry bool) string {
		raw = trimSpace(raw)
		table := resolved
		if useEntry {
			table = entry
		}
		if mapped := table[raw]; mapped != "" {
			return mapped
		}
		return raw
	}
	patch = v4aSingleHeader.ReplaceAllStringFunc(patch, func(header string) string {
		groups := v4aSingleHeader.FindStringSubmatch(header)
		return groups[1] + pick(groups[3], groups[2] == "Delete")
	})
	return v4aMoveHeader.ReplaceAllStringFunc(patch, func(header string) string {
		groups := v4aMoveHeader.FindStringSubmatch(header)
		return groups[1] + pick(groups[2], true) + " -> " + pick(groups[3], true)
	})
}

func collectPatchPaths(patch string) (all, content, entry []string, failure string) {
	type header struct {
		raw        string
		writesText bool
	}
	var headers []header
	for _, groups := range v4aSingleHeader.FindAllStringSubmatch(patch, -1) {
		headers = append(headers, header{groups[3], groups[2] == "Update" || groups[2] == "Add"})
	}
	for _, groups := range v4aMoveHeader.FindAllStringSubmatch(patch, -1) {
		headers = append(headers, header{groups[2], false}, header{groups[3], false})
	}
	for _, h := range headers {
		target := trimSpace(h.raw)
		if tool.HasTraversal(target) {
			return nil, nil, nil, "V4A patch header contains '..' traversal: " + quote(target) + ". " +
				"Use the agent's cwd-relative path (no '..') or an absolute " +
				"path in '*** Update File:' / '*** Add File:' / " +
				"'*** Delete File:' / '*** Move File:' headers."
		}
		all = append(all, target)
		if h.writesText {
			content = append(content, target)
		} else {
			entry = append(entry, target)
		}
	}
	return all, content, entry, ""
}

func isExpectedWriteDenial(err error) bool {
	return errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EACCES) ||
		errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EROFS)
}

func (t *Tools) resolveOrNone(ctx context.Context, path, taskID string, entry bool) string {
	resolve := t.paths.Resolve
	if entry {
		resolve = t.paths.ResolveEntry
	}
	resolved, err := resolve(ctx, path, taskID)
	if err != nil {
		return ""
	}
	return resolved
}

func (t *Tools) writePrecheck(ctx context.Context, c *tool.Call, paths, contentPaths []string, taskID string, crossProfile bool) (string, error) {
	for _, p := range paths {
		problem, err := t.checkSensitivePath(ctx, p, taskID)
		if err != nil {
			return "", err
		}
		if problem == "" && !crossProfile {
			problem = t.checkMirrorPath(ctx, p, taskID)
		}
		if problem != "" {
			return problem, nil
		}
	}
	for _, p := range contentPaths {
		if problem := t.checkBinaryDocumentWrite(ctx, c, p, taskID); problem != "" {
			return problem, nil
		}
	}
	if problem := t.checkProtectedInstructionWrite(ctx, c, paths, taskID); problem != "" {
		return problem, nil
	}
	return t.checkApprovalRequiredWrite(ctx, c, paths, taskID), nil
}

func (t *Tools) editWarnings(ctx context.Context, paths []string, resolved map[string]string, taskID string) []string {
	var warnings []string
	for _, p := range paths {
		target := resolved[p]
		if target == "" {
			continue
		}
		warning := t.tasks.state.CheckStale(taskID, target)
		if warning == "" {
			warning = t.tasks.tracker.StalenessWarning(p, target, taskID)
		}
		if warning == "" {
			warning = t.paths.ResolutionWarning(ctx, p, target, taskID)
		}
		if warning != "" {
			warnings = append(warnings, warning)
		}
	}
	return warnings
}

func (t *Tools) noteEdited(ctx context.Context, c *tool.Call, taskID string, paths []string, resolved map[string]string) {
	marked := make([]string, len(paths))
	for i, p := range paths {
		marked[i] = resolved[p]
		if marked[i] == "" {
			marked[i] = p
		}
	}
	root, err := t.paths.WorkspaceRoot(ctx, taskID)
	if err == nil {
		err = MarkVerificationStale(ctx, t.marker, taskID, marked, c.SessionID, root)
	}
	if err != nil {
		slog.DebugContext(ctx, "verification stale marker failed", "error", err)
	}
	for _, p := range paths {
		if target := resolved[p]; target != "" {
			t.tasks.tracker.UpdateReadTimestamp(target, taskID)
			t.tasks.state.NoteWrite(taskID, target)
		}
	}
}

func (t *Tools) wholeFileRewriteHint(ctx context.Context, ops *Operations, resolved, newContent string) string {
	size := charLen(newContent)
	if resolved == "" || size < rewriteHintMinChars || size > rewriteHintMaxChars {
		return ""
	}
	current, err := ops.ReadRaw(ctx, resolved)
	if err != nil || current.Error != "" {
		return ""
	}
	if n := charLen(current.Content); n < rewriteHintMinChars || n > rewriteHintMaxChars {
		return ""
	}
	oldLines, newLines := splitLines(current.Content, false), splitLines(newContent, false)
	if len(oldLines) == 0 {
		return ""
	}
	remaining := make(map[string]int, len(oldLines))
	for _, line := range oldLines {
		remaining[line]++
	}
	unchanged := 0
	for _, line := range newLines {
		if remaining[line] > 0 {
			remaining[line]--
			unchanged++
		}
	}
	longest := max(len(oldLines), len(newLines))
	ratio := float64(unchanged) / float64(longest)
	if ratio < rewriteHintMinUnchanged {
		return ""
	}
	changed := longest - unchanged
	return commas(int64(unchanged)) + " of " + commas(int64(len(newLines))) + " lines were already on disk (" +
		strconv.FormatFloat(ratio*100, 'f', 0, 64) + "% unchanged); ~" + commas(int64(changed)) + " " +
		"line(s) actually changed. Re-sending a " + commas(int64(size)) + "-char file costs output tokens for every " +
		"unchanged line; for edits like this use patch (old_string/new_string), which sends only the changed region."
}

func (t *Tools) writeFile(ctx context.Context, c *tool.Call, path, content string, crossProfile bool) (tool.Result, error) {
	taskID := taskOf(c)
	problem, err := t.checkSensitivePath(ctx, path, taskID)
	if err != nil {
		return tool.Result{}, err
	}
	if problem == "" {
		problem = t.checkBinaryDocumentWrite(ctx, c, path, taskID)
	}
	if problem == "" {
		problem = t.checkProtectedInstructionWrite(ctx, c, []string{path}, taskID)
	}
	if problem == "" {
		problem = t.checkApprovalRequiredWrite(ctx, c, []string{path}, taskID)
	}
	if problem == "" && !crossProfile {
		problem = t.checkMirrorPath(ctx, path, taskID)
	}
	if problem == "" && isInternalFileToolContent(content) {
		problem = "Refusing to write internal read_file display text as file content. " +
			"Strip read_file line-number prefixes or reconstruct the intended " +
			"file contents before writing."
	}
	if problem != "" {
		return tool.Failure(validText(problem)), nil
	}
	result, err := t.writeLocked(ctx, c, path, content, taskID)
	if err != nil {
		if isExpectedWriteDenial(err) {
			slog.DebugContext(ctx, "write_file expected denial", "error", err)
		} else {
			slog.ErrorContext(ctx, "write_file error", "error", err)
		}
		return t.failed(ctx, err)
	}
	return result, nil
}

func (t *Tools) writeLocked(ctx context.Context, c *tool.Call, path, content, taskID string) (tool.Result, error) {
	resolved := t.resolveOrNone(ctx, path, taskID, false)
	pathToResolved := map[string]string{path: resolved}
	if resolved != "" {
		unlock := t.tasks.state.LockPath(resolved)
		defer unlock()
	}
	if blocker := t.staleOverwriteBlocker(path, resolved, taskID); blocker != "" {
		return encode(staleWriteRefusal(path, blocker, resolved)), nil
	}
	warnings := t.editWarnings(ctx, []string{path}, pathToResolved, taskID)
	ops, err := t.ops(ctx, c)
	if err != nil {
		return tool.Result{}, err
	}
	rewriteHint := t.wholeFileRewriteHint(ctx, ops, resolved, content)
	target := resolved
	if target == "" {
		target = path
	}
	result, err := ops.Write(ctx, target, content, nil)
	if err != nil {
		return tool.Result{}, err
	}
	fields := fieldList(result.Fields())
	if len(warnings) > 0 {
		fields.set("_warning", warnings[0])
	}
	if rewriteHint != "" && result.Error == "" {
		fields.set("hint", rewriteHint)
	}
	if resolved != "" {
		fields.set("resolved_path", resolved)
	}
	if result.Error != "" {
		if resolved != "" {
			t.tasks.tracker.UpdateReadTimestamp(resolved, taskID)
		}
		return encode(fields), nil
	}
	if resolved != "" {
		fields.set("files_modified", []string{resolved})
		t.tasks.tracker.MarkFullWriteBaseline(resolved, taskID, result.ContentSHA256)
	}
	t.noteEdited(ctx, c, taskID, []string{path}, pathToResolved)
	return encode(fields), nil
}

func (t *Tools) patchFile(ctx context.Context, c *tool.Call, req patchRequest) (tool.Result, error) {
	taskID := taskOf(c)
	var pathsToCheck, contentPaths, entryPaths []string
	if req.path != "" {
		pathsToCheck = []string{req.path}
		contentPaths = []string{req.path}
	}
	if req.mode == patchName && req.patch != "" {
		all, content, entry, failure := collectPatchPaths(req.patch)
		if failure != "" {
			return tool.Failure(validText(failure)), nil
		}
		pathsToCheck = append(pathsToCheck, all...)
		contentPaths = append(contentPaths, content...)
		entryPaths = entry
	}
	problem, err := t.writePrecheck(ctx, c, pathsToCheck, contentPaths, taskID, req.crossProfile)
	if err != nil {
		return tool.Result{}, err
	}
	if problem != "" {
		return tool.Failure(validText(problem)), nil
	}
	pathToResolved := make(map[string]string, len(pathsToCheck))
	for _, p := range pathsToCheck {
		pathToResolved[p] = t.resolveOrNone(ctx, p, taskID, false)
	}
	pathToEntry := make(map[string]string, len(entryPaths))
	for _, p := range entryPaths {
		pathToEntry[p] = t.resolveOrNone(ctx, p, taskID, true)
	}
	result, fields, refusal, err := t.patchLocked(ctx, c, req, taskID, pathsToCheck, pathToResolved, pathToEntry)
	if err != nil {
		return t.failed(ctx, err)
	}
	if refusal != "" {
		return tool.Failure(validText(refusal)), nil
	}
	if result.Error != "" && strings.Contains(result.Error, "Could not find") {
		failures := 0
		if req.mode == "replace" && req.path != "" {
			target := pathToResolved[req.path]
			if target == "" {
				target = req.path
			}
			failures = t.tasks.tracker.RecordPatchFailure(taskID, target)
		}
		switch {
		case failures >= patchFailureEscalation:
			fields.set("_hint", fmt.Sprintf("This is failure #%d patching %s. "+
				"Stop retrying with variations of the same old_string. "+
				"Either: (1) re-read the file fresh to verify current "+
				"content, (2) use a longer / more unique old_string with "+
				"surrounding context lines, or (3) re-read the file in "+
				"full, then use write_file to replace it if the targeted "+
				"region is hard to anchor.", failures, quote(req.path)))
		case !strings.Contains(result.Error, "Did you mean one of these sections?"):
			fields.set("_hint", "old_string not found. Use read_file to verify the current "+
				"content, or search_files to locate the text.")
		}
	}
	return encode(fields), nil
}

func (t *Tools) patchLocked(ctx context.Context, c *tool.Call, req patchRequest, taskID string, pathsToCheck []string,
	pathToResolved, pathToEntry map[string]string) (PatchResult, fieldList, string, error) {
	held := make(map[string]struct{})
	var resolvedTargets []string
	for _, p := range pathsToCheck {
		if target := pathToResolved[p]; target != "" {
			held[target] = struct{}{}
			resolvedTargets = append(resolvedTargets, target)
		}
	}
	for _, target := range pathToEntry {
		if target != "" {
			held[target] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(held))
	for target := range held {
		ordered = append(ordered, target)
	}
	sort.Strings(ordered)
	for _, target := range ordered {
		unlock := t.tasks.state.LockPath(target)
		defer unlock()
	}
	staleWarnings := t.editWarnings(ctx, pathsToCheck, pathToResolved, taskID)
	ops, err := t.ops(ctx, c)
	if err != nil {
		return PatchResult{}, nil, "", err
	}
	known := t.tasks.tracker.KnownFullContentSHA256(resolvedTargets, taskID)

	var result PatchResult
	switch req.mode {
	case "replace":
		if req.path == "" {
			return PatchResult{}, nil, "path required", nil
		}
		if !req.hasOld || !req.hasNew {
			return PatchResult{}, nil, "old_string and new_string required", nil
		}
		target := pathToResolved[req.path]
		if target == "" {
			target = req.path
		}
		result, err = ops.Replace(ctx, target, req.oldString, req.newString, req.replaceAll)
	case patchName:
		if req.patch == "" {
			return PatchResult{}, nil, "patch content required", nil
		}
		text := req.patch
		if ops.env.HostFilesystem() {
			text = rewritePatchPaths(text, pathToResolved, pathToEntry)
		}
		result, err = ops.Patch(ctx, text)
	default:
		return PatchResult{}, nil, "Unknown mode: " + req.mode, nil
	}
	if err != nil {
		return PatchResult{}, nil, "", err
	}
	fields := fieldList(result.Fields())
	if len(staleWarnings) > 0 {
		fields.set("_warning", strings.Join(staleWarnings, " | "))
	}
	if result.Error != "" {
		return result, fields, "", nil
	}
	modified := make([]string, len(pathsToCheck))
	for i, p := range pathsToCheck {
		modified[i] = p
		if target := pathToEntry[p]; target != "" {
			modified[i] = target
		} else if target := pathToResolved[p]; target != "" {
			modified[i] = target
		}
	}
	fields.set("files_modified", modified)
	if len(modified) == 1 {
		fields.set("resolved_path", modified[0])
	}
	t.noteEdited(ctx, c, taskID, pathsToCheck, pathToResolved)
	t.tasks.tracker.CarryFullWriteBaselines(taskID, known, result.Writes, pathToResolved)
	t.tasks.tracker.ResetPatchFailures(taskID, dedupe(resolvedTargets))
	return result, fields, "", nil
}

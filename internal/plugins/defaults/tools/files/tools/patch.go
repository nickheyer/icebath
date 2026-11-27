package tools

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/tools/fuzzy"
)

// One patch call: a replace of old with new in one file, or a V4A patch of several
type patchCall struct {
	mode         string
	path         string
	old, new     *string
	all          bool
	patch        string
	crossProfile bool
}

// Patches files in replace mode or as a V4A patch behind the shared write guards, every file it touches locked while it runs
func (o *ops) patch(c patchCall) string {
	var paths []string
	if c.path != "" {
		paths = []string{c.path}
	}
	contents := append([]string{}, paths...)
	var entries []string
	if c.mode == "patch" && c.patch != "" {
		all, written, acted, refusal := headerPaths(c.patch)
		if refusal != "" {
			return refusal
		}
		paths = append(paths, all...)
		contents = append(contents, written...)
		entries = acted
	}
	resolved := map[string]string{}
	for _, p := range paths {
		resolved[p] = o.resolve(p)
	}
	values := make([]string, len(paths))
	for i, p := range paths {
		values[i] = resolved[p]
	}
	for _, p := range paths {
		refusal := o.sensitive(p, resolved[p])
		if refusal == "" && !c.crossProfile {
			refusal = o.mirror(resolved[p])
		}
		if refusal != "" {
			return failure(refusal)
		}
	}
	for _, p := range contents {
		if refusal := o.binaryWrite(p, resolved[p]); refusal != "" {
			return failure(refusal)
		}
	}
	if refusal := o.approveInstructions(paths, values); refusal != "" {
		return failure(refusal)
	}
	if refusal := o.approveSSHConfig(paths, values); refusal != "" {
		return failure(refusal)
	}
	entry := map[string]string{}
	for _, p := range entries {
		entry[p] = o.resolveEntry(p)
	}
	locked := append([]string{}, values...)
	for _, e := range entry {
		locked = append(locked, e)
	}
	unlock := o.r.memory.lockAll(locked)
	out, early := o.patchLocked(c, paths, resolved, entry, values)
	unlock()
	if early {
		return result(out)
	}
	if text := out.Error; text != "" && fuzzy.HoldsNoMatch(text) {
		failures := 0
		if c.mode == "replace" && c.path != "" && o.host {
			failures = o.r.memory.failed(o.track, resolved[c.path])
		}
		if failures >= 3 {
			out.Hint = fmt.Sprintf("This is failure #%d patching %s. Stop retrying with variations of the same old_string. Either: (1) re-read the "+
				"file fresh to verify current content, (2) use a longer / more unique old_string with surrounding context lines, or (3) re-read the "+
				"file in full, then use write_file to replace it if the targeted region is hard to anchor.", failures, strconv.Quote(c.path))
		} else if !strings.Contains(text, fuzzy.DidYouMean) {
			out.Hint = "old_string not found. Use read_file to verify the current content, or search_files to locate the text."
		}
	}
	return result(out)
}

// The patch with its files locked; early when it was refused before any file was touched
func (o *ops) patchLocked(c patchCall, paths []string, resolved, entry map[string]string, values []string) (*patchResult, bool) {
	warnings := o.editWarnings(paths, resolved)
	known := map[string]string{}
	if o.host {
		known = o.r.memory.known(o.track, values)
	}
	var out *patchResult
	switch c.mode {
	case "replace":
		switch {
		case c.path == "":
			return &patchResult{Error: "path required"}, true
		case c.old == nil || c.new == nil:
			return &patchResult{Error: "old_string and new_string required"}, true
		}
		out = o.patchReplace(resolved[c.path], *c.old, *c.new, c.all)
	case "patch":
		if c.patch == "" {
			return &patchResult{Error: "patch content required"}, true
		}
		out = o.patchV4A(rewriteHeaders(c.patch, resolved, entry))
	default:
		return &patchResult{Error: "Unknown mode: " + c.mode}, true
	}
	out.Error = tools.BoundError(out.Error)
	if len(warnings) > 0 {
		out.Warning = strings.Join(warnings, " | ")
	}
	if out.Error != "" {
		return out, false
	}
	modified := make([]string, len(paths))
	for i, p := range paths {
		switch {
		case entry[p] != "":
			modified[i] = entry[p]
		case resolved[p] != "":
			modified[i] = resolved[p]
		default:
			modified[i] = p
		}
	}
	out.FilesModified = modified
	if len(modified) == 1 {
		out.ResolvedPath = modified[0]
	}
	o.edited(paths, resolved)
	if o.host {
		o.r.memory.carry(o.track, known, out.writes, resolved)
		o.r.memory.succeeded(o.track, values)
	}
	return out, false
}

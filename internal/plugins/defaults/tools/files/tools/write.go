package tools

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/tools/diff"
	"github.com/nickheyer/icebath/pkg/strs"
)

// A large overwrite that keeps most of what is on disk is an edit done the expensive way
const (
	rewriteMin       = 20_000
	rewriteUnchanged = 0.80
	rewriteMax       = 400_000
)

// Why write_file must not replace the file, decided before anything is written; empty to go ahead
func (o *ops) staleBlock(raw, resolved string) string {
	if o.unguarded() || !o.host {
		return ""
	}
	m := o.r.memory
	if stale := m.stale(o.track, resolved); stale != "" {
		return stale
	}
	if m.drifted(o.track, resolved) {
		return raw + " was modified since you last read it (external edit or concurrent agent). Re-read the file before writing."
	}
	if m.hasBaseline(o.track, resolved) {
		return ""
	}
	if _, err := os.Stat(resolved); err != nil {
		return ""
	}
	if m.ownBlind(o.track, resolved) {
		return "Your patch changed this file without a full view of its current content. Read the current file in full before a whole-file " +
			"overwrite, or continue with targeted patches."
	}
	return resolved + " exists but this task has not seen its full current content (never read, patched without a prior full read, or only a " +
		"redacted/partial view). Read the file — every page of it, if it needs offset/limit — or use patch for a targeted edit; a stale " +
		"conversation copy must not overwrite the current disk content."
}

// Refusal of write_file a stale guard gave, which stale_write_blocked tells apart from a failure of the write
func staleRefusal(raw, reason, resolved string) string {
	return result(struct {
		Error        string `json:"error"`
		Blocked      bool   `json:"stale_write_blocked"`
		Path         string `json:"path"`
		ResolvedPath string `json:"resolved_path,omitempty"`
	}{"Refusing to overwrite " + raw + ": " + reason + " The file was NOT modified. Reload the current contents with read_file (every page, for a " +
		"file that needs offset/limit), merge the requested change, then call write_file again. For small edits, prefer patch so existing unrelated " +
		"changes are preserved.", true, raw, resolved})
}

// One warning per path before an edit: another session wrote it, it changed since it was read, or it resolved outside the workspace
func (o *ops) editWarnings(raws []string, resolved map[string]string) []string {
	var warnings []string
	m := o.r.memory
	for _, raw := range raws {
		r := resolved[raw]
		w := ""
		if o.host && !o.unguarded() {
			w = m.stale(o.track, r)
		}
		if w == "" && o.host && m.drifted(o.track, r) {
			w = "Warning: " + raw + " was modified since you last read it (external edit or concurrent agent). The content you read may be stale. " +
				"Consider re-reading the file to verify before writing."
		}
		if w == "" {
			w = o.outside(raw, r)
		}
		if w != "" {
			warnings = append(warnings, w)
		}
	}
	return warnings
}

// Bookkeeping after an edit landed: the mtime of each path noted, so the session's own edits never look stale, and the session recorded as
// each path's last writer
func (o *ops) edited(raws []string, resolved map[string]string) {
	m := o.r.memory
	for _, raw := range raws {
		r := resolved[raw]
		if r == "" || !o.host {
			continue
		}
		m.stamp(o.track, r)
		if !o.unguarded() {
			m.noteWrite(o.track, r)
		}
	}
}

// Hint when the new content mostly sends again what is already on disk
func (o *ops) rewriteHint(resolved, content string) string {
	length := runes(content)
	if resolved == "" || length < rewriteMin || length > rewriteMax {
		return ""
	}
	existing := o.readRaw(resolved)
	if existing.Error != "" {
		return ""
	}
	if n := runes(existing.Content); n < rewriteMin || n > rewriteMax {
		return ""
	}
	before, after := diff.Split(existing.Content), diff.Split(content)
	if len(before) == 0 {
		return ""
	}
	left := map[string]int{}
	for _, line := range before {
		left[line]++
	}
	unchanged := 0
	for _, line := range after {
		if left[line] > 0 {
			left[line]--
			unchanged++
		}
	}
	longest := max(len(before), len(after))
	ratio := float64(unchanged) / float64(longest)
	if ratio < rewriteUnchanged {
		return ""
	}
	return fmt.Sprintf("%s of %s lines were already on disk (%.0f%% unchanged); ~%s line(s) actually changed. Re-sending a %s-char file costs output "+
		"tokens for every unchanged line; for edits like this use patch (old_string/new_string), which sends only the changed region.",
		strs.Thousands(unchanged), strs.Thousands(len(after)), ratio*100, strs.Thousands(longest-unchanged), strs.Thousands(length))
}

// Writes a whole file behind the write guards and the stale overwrite guard
func (o *ops) write(raw, content string, crossProfile bool) string {
	resolved := o.resolve(raw)
	refusal := o.sensitive(raw, resolved)
	if refusal == "" {
		refusal = o.binaryWrite(raw, resolved)
	}
	if refusal == "" {
		refusal = o.approveInstructions([]string{raw}, []string{resolved})
	}
	if refusal == "" {
		refusal = o.approveSSHConfig([]string{raw}, []string{resolved})
	}
	if refusal == "" && !crossProfile {
		refusal = o.mirror(resolved)
	}
	if refusal == "" && displayText(content) {
		refusal = "Refusing to write internal read_file display text as file content. Strip read_file line-number prefixes or reconstruct the " +
			"intended file contents before writing."
	}
	if refusal != "" {
		return failure(refusal)
	}
	unlock := o.r.memory.lock(resolved)
	defer unlock()
	if reason := o.staleBlock(raw, resolved); reason != "" {
		return staleRefusal(raw, reason, resolved)
	}
	paths := map[string]string{raw: resolved}
	warnings := o.editWarnings([]string{raw}, paths)
	hint := o.rewriteHint(resolved, content)
	before := o.before(resolved)
	written := o.writeFile(resolved, content, nil)
	if len(warnings) > 0 {
		written.Warning = warnings[0]
	}
	if hint != "" && written.Error == "" {
		written.Hint = hint
	}
	written.ResolvedPath = resolved
	if written.Error != "" {
		if o.host {
			o.r.memory.stamp(o.track, resolved)
		}
		return result(written)
	}
	written.FilesModified = []string{resolved}
	if o.host {
		o.r.memory.markBaseline(o.track, resolved, written.sha256)
	}
	o.edited([]string{raw}, paths)
	if o.host {
		name := raw
		if rel, under := relativeTo(resolved, o.base); under {
			name = rel
		}
		o.call.Display = diff.Files(before, content, name)
	}
	return result(written)
}

// Text of a file on this machine before a write replaces it, which the user is shown the change against; empty for a new file, one not on
// this machine, or one that can't be read as text
func (o *ops) before(resolved string) string {
	if !o.host {
		return ""
	}
	data, err := os.ReadFile(resolved)
	if err != nil || !utf8.Valid(data) {
		return ""
	}
	return strings.ReplaceAll(strings.TrimPrefix(string(data), "\ufeff"), "\r\n", "\n")
}

// Kind of a JSON value as a refusal names it
func kind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case float64:
		return "a number"
	case []any:
		return "a list"
	case map[string]any:
		return "an object"
	}
	return fmt.Sprintf("%T", v)
}

// Whether a JSON value counts as set
func truthy(v any) bool {
	switch v := v.(type) {
	case bool:
		return v
	case string:
		return strings.TrimSpace(v) != "" && v != "false"
	case float64:
		return v != 0
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return v != nil
}

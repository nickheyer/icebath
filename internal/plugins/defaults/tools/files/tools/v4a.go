package tools

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/tools/diff"
	"github.com/nickheyer/icebath/internal/tools/fuzzy"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Kinds of V4A operation
const (
	v4aAdd    = "add"
	v4aUpdate = "update"
	v4aDelete = "delete"
	v4aMove   = "move"
)

// One hunk line and its prefix: space, minus or plus
type hunkLine struct {
	prefix  byte
	content string
}

type hunk struct {
	hint  string
	lines []hunkLine
}

// One operation of a patch; to is set only for a move
type operation struct {
	kind  string
	path  string
	to    string
	hunks []*hunk
}

// Whitespace as Python's str.isspace knows it, as a character class
const space = `[\t\n\v\f\r \x{1c}-\x{1f}\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]`

var (
	beginPatch = regexp.MustCompile(`^\*\*\*` + space + `*Begin` + space + `+Patch` + space + `*$`)
	endPatch   = regexp.MustCompile(`^\*\*\*` + space + `*End` + space + `+Patch` + space + `*$`)
	markers    = []struct {
		kind string
		re   *regexp.Regexp
	}{
		{v4aUpdate, regexp.MustCompile(`^\*\*\*` + space + `*Update` + space + `+File:` + space + `*(.+)`)},
		{v4aAdd, regexp.MustCompile(`^\*\*\*` + space + `*Add` + space + `+File:` + space + `*(.+)`)},
		{v4aDelete, regexp.MustCompile(`^\*\*\*` + space + `*Delete` + space + `+File:` + space + `*(.+)`)},
		{v4aMove, regexp.MustCompile(`^\*\*\*` + space + `*Move` + space + `+File:` + space + `*(.+?)` + space + `*->` + space + `*(.+)`)},
	}
	hintRe = regexp.MustCompile(`^@@` + space + `*(.+?)` + space + `*@@`)
	// Headers of a whole patch: the prefix, the operation and the path; and the prefix, source and destination of a move
	singleHeader = regexp.MustCompile(`(?m)^(\*\*\*` + space + `*(Update|Add|Delete)` + space + `+File:` + space + `*)(.+)$`)
	moveHeader   = regexp.MustCompile(`(?m)^(\*\*\*` + space + `*Move` + space + `+File:` + space + `*)(.+?)` + space + `*->` + space + `*(.+)$`)
)

// Operations of a V4A patch, or the parse errors joined
func parseV4A(text string) ([]*operation, string) {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSuffix(line, "\r")
	}
	start, end := -1, len(lines)
	for i, line := range lines {
		if beginPatch.MatchString(line) {
			start = i
		} else if endPatch.MatchString(line) {
			end = i
			break
		}
	}
	var operations []*operation
	var current *operation
	var h *hunk
	flushHunk := func() {
		if current != nil && h != nil && len(h.lines) > 0 {
			current.hunks = append(current.hunks, h)
		}
	}
	for _, line := range lines[start+1 : end] {
		kind, m := "", []string(nil)
		for _, marker := range markers {
			if m = marker.re.FindStringSubmatch(line); m != nil {
				kind = marker.kind
				break
			}
		}
		switch {
		case m != nil:
			if current != nil {
				flushHunk()
				operations = append(operations, current)
			}
			current, h = &operation{kind: kind, path: strings.TrimSpace(m[1])}, nil
			if kind == v4aMove {
				current.to = strings.TrimSpace(m[2])
			}
			if kind == v4aAdd {
				h = &hunk{}
			}
			if kind == v4aDelete || kind == v4aMove {
				operations = append(operations, current)
				current = nil
			}
		case strings.HasPrefix(line, "@@"):
			if current != nil {
				flushHunk()
				h = &hunk{}
				if hm := hintRe.FindStringSubmatch(line); hm != nil {
					h.hint = hm[1]
				}
			}
		case current != nil && line != "":
			if h == nil {
				h = &hunk{}
			}
			switch line[0] {
			case '+', '-', ' ':
				h.lines = append(h.lines, hunkLine{line[0], line[1:]})
			case '\\':
			default:
				h.lines = append(h.lines, hunkLine{' ', line})
			}
		}
	}
	if current != nil {
		flushHunk()
		operations = append(operations, current)
	}
	var errs []string
	for _, op := range operations {
		if op.path == "" {
			errs = append(errs, "Operation with empty file path")
		}
		if op.kind == v4aUpdate && len(op.hunks) == 0 {
			errs = append(errs, "UPDATE "+strconv.Quote(op.path)+": no hunks found")
		}
		if op.kind == v4aMove && op.to == "" {
			errs = append(errs, "MOVE "+strconv.Quote(op.path)+": missing destination path (expected 'src -> dst')")
		}
	}
	if len(errs) > 0 {
		return nil, "Parse error: " + strings.Join(errs, "; ")
	}
	return operations, ""
}

// Every path the headers name, those whose text the patch writes, and those it acts on as entries; the error result when one climbs out
func headerPaths(patch string) ([]string, []string, []string, string) {
	type header struct {
		raw    string
		writes bool
	}
	var headers []header
	for _, m := range singleHeader.FindAllStringSubmatch(patch, -1) {
		headers = append(headers, header{m[3], m[2] == "Update" || m[2] == "Add"})
	}
	for _, m := range moveHeader.FindAllStringSubmatch(patch, -1) {
		headers = append(headers, header{m[2], false}, header{m[3], false})
	}
	var all, content, entries []string
	for _, h := range headers {
		p := strings.TrimSpace(h.raw)
		if traverses(p) {
			return nil, nil, nil, failure(fmt.Sprintf("V4A patch header contains '..' traversal: %q. Use the agent's cwd-relative path (no '..') or "+
				"an absolute path in '*** Update File:' / '*** Add File:' / '*** Delete File:' / '*** Move File:' headers.", p))
		}
		all = append(all, p)
		if h.writes {
			content = append(content, p)
		} else {
			entries = append(entries, p)
		}
	}
	return all, content, entries, ""
}

// Patch with its header paths replaced by the resolved paths, so the operations act on the files the call locked
func rewriteHeaders(patch string, resolved, entries map[string]string) string {
	pick := func(raw string, entry bool) string {
		raw = strings.TrimSpace(raw)
		table := resolved
		if entry {
			table = entries
		}
		if r := table[raw]; r != "" {
			return r
		}
		return raw
	}
	patch = replaceGroups(singleHeader, patch, func(m []string) string { return m[1] + pick(m[3], m[2] == "Delete") })
	return replaceGroups(moveHeader, patch, func(m []string) string { return m[1] + pick(m[2], true) + " -> " + pick(m[3], true) })
}

// Every match replaced by what the callback makes of its groups
func replaceGroups(re *regexp.Regexp, s string, with func([]string) string) string {
	var b strings.Builder
	last := 0
	for _, loc := range re.FindAllStringSubmatchIndex(s, -1) {
		groups := make([]string, len(loc)/2)
		for i := range groups {
			if loc[2*i] >= 0 {
				groups[i] = s[loc[2*i]:loc[2*i+1]]
			}
		}
		b.WriteString(s[last:loc[0]])
		b.WriteString(with(groups))
		last = loc[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// Occurrences of the pattern, overlapping ones counted
func occurrences(text, pattern string) int {
	if pattern == "" {
		return utf8.RuneCountInString(text) + 1
	}
	n := 0
	for i := 0; i <= len(text)-len(pattern); i++ {
		if text[i:i+len(pattern)] == pattern {
			n++
		}
	}
	return n
}

// Lines a hunk looks for, its context and removed lines, and the lines it leaves, its context and added lines
func (h *hunk) split() ([]string, []string) {
	search, replace := []string{}, []string{}
	for _, l := range h.lines {
		if l.prefix != '+' {
			search = append(search, l.content)
		}
		if l.prefix != '-' {
			replace = append(replace, l.content)
		}
	}
	return search, replace
}

// Occurrences of an addition only hunk's hint, and the error when there are several
func ambiguous(content, hint, tail string) (int, string) {
	n := occurrences(content, hint)
	if n > 1 {
		return n, fmt.Sprintf("context hint '%s' is ambiguous (%d occurrences)%s", hint, n, tail)
	}
	return n, ""
}

// Lines an Add creates, the added lines of every hunk
func (op *operation) added() []string {
	var out []string
	for _, h := range op.hunks {
		for _, l := range h.lines {
			if l.prefix == '+' {
				out = append(out, l.content)
			}
		}
	}
	return out
}

// Runs every operation in order over files held in memory and answers what would fail
func (o *ops) validateV4A(operations []*operation) []string {
	var errs []string
	changes := 0
	pending := map[string]string{}
	removed := map[string]bool{}
	read := func(path string) (string, string) {
		if content, ok := pending[path]; ok {
			return content, ""
		}
		if removed[path] {
			return "", "file not found"
		}
		r := o.readRaw(path)
		return r.Content, r.Error
	}
	occupied := func(path string) string {
		if _, ok := pending[path]; ok {
			return "exists"
		}
		if removed[path] {
			return ""
		}
		r := o.readRaw(path)
		switch {
		case r.Error == "":
			return "exists"
		case r.NotFound:
			return ""
		}
		return "could not confirm the path is free — " + r.Error
	}
	remove := func(path string) {
		removed[path] = true
		delete(pending, path)
	}
	for _, op := range operations {
		if op.kind != v4aUpdate {
			changes++
		}
		switch op.kind {
		case v4aUpdate:
			simulated, failed := read(op.path)
			if failed != "" {
				errs = append(errs, op.path+": "+failed)
				continue
			}
			for i, h := range op.hunks {
				search, replace := h.split()
				if slices.Equal(search, replace) {
					if slices.ContainsFunc(h.lines, func(l hunkLine) bool { return l.prefix == '-' || l.prefix == '+' }) {
						changes++
					}
					continue
				}
				changes++
				if len(search) == 0 {
					if h.hint != "" {
						if n, unclear := ambiguous(simulated, h.hint, ""); n == 0 {
							errs = append(errs, fmt.Sprintf("%s: addition-only hunk context hint '%s' not found", op.path, h.hint))
						} else if unclear != "" {
							errs = append(errs, op.path+": addition-only hunk "+unclear)
						}
					}
					continue
				}
				pattern, replacement := strings.Join(search, "\n"), strings.Join(replace, "\n")
				result, err := fuzzy.Replace(simulated, pattern, replacement, false)
				if err == nil && result.Count > 0 {
					simulated = result.Content
					continue
				}
				if fuzzy.Applied(simulated, pattern, replacement) {
					continue
				}
				label := "(no hint)"
				if h.hint != "" {
					label = "'" + h.hint + "'"
				}
				message := fmt.Sprintf("%s: hunk %d %s not found", op.path, i+1, label)
				reason := ""
				if err != nil {
					reason = err.Error()
					message += " — " + reason
				}
				errs = append(errs, message+fuzzy.NoMatchHint(reason, 0, pattern, simulated))
			}
			pending[op.path] = simulated
		case v4aDelete:
			if _, failed := read(op.path); failed != "" {
				errs = append(errs, op.path+": file not found for deletion")
			} else {
				remove(op.path)
			}
		case v4aMove:
			content, failed := read(op.path)
			if failed != "" {
				errs = append(errs, op.path+": source file not found for move")
			}
			switch taken := occupied(op.to); {
			case taken == "exists":
				errs = append(errs, op.to+": destination already exists — move would overwrite")
			case taken != "":
				errs = append(errs, op.to+": "+taken)
			case failed == "":
				pending[op.to] = content
				remove(op.path)
			}
		case v4aAdd:
			switch taken := occupied(op.path); {
			case taken == "exists":
				errs = append(errs, op.path+": file already exists — use Update File, not Add File")
			case taken != "":
				errs = append(errs, op.path+": "+taken)
			default:
				delete(removed, op.path)
				pending[op.path] = strings.Join(op.added(), "\n")
			}
		}
	}
	if len(errs) == 0 && changes == 0 {
		errs = append(errs, "Patch contains no changes (only context lines were provided)")
	}
	return errs
}

// Outcome of one applied operation: its diff or error, what the write said and the notes it left
type applied struct {
	ok          bool
	text        string
	diagnostics string
	lint        *lintResult
	write       *patchWrite
	notes       []string
}

func bullets(errs []string) string {
	rows := make([]string, len(errs))
	for i, e := range errs {
		rows[i] = "  • " + e
	}
	return strings.Join(rows, "\n")
}

// Parses then applies a V4A patch: every operation checked first, none applied when any would fail
func (o *ops) patchV4A(text string) *patchResult {
	operations, failed := parseV4A(text)
	if failed != "" {
		return &patchResult{Error: "Failed to parse patch: " + failed}
	}
	if errs := o.validateV4A(operations); len(errs) > 0 {
		return &patchResult{Error: "Patch validation failed (no files were modified):\n" + bullets(errs)}
	}
	files := map[string][]string{"created": {}, "deleted": {}, "modified": {}}
	var errs, diffs, notes, diagnostics []string
	lints := map[string]*lintResult{}
	var order []string
	var writes []patchWrite
	for _, op := range operations {
		var verb, bucket string
		var a applied
		switch op.kind {
		case v4aAdd:
			verb, bucket, a = "add", "created", o.applyAdd(op)
		case v4aDelete:
			verb, bucket, a = "delete", "deleted", o.applyDelete(op)
		case v4aMove:
			verb, bucket, a = "move", "modified", o.applyMove(op)
		default:
			verb, bucket, a = "update", "modified", o.applyUpdate(op)
		}
		if !a.ok {
			errs = append(errs, fmt.Sprintf("Failed to %s %s: %s", verb, op.path, a.text))
			continue
		}
		name := op.path
		if op.kind == v4aMove {
			name += " -> " + op.to
		}
		files[bucket] = append(files[bucket], name)
		diffs = append(diffs, a.text)
		if a.write != nil {
			writes = append(writes, *a.write)
		}
		notes = append(notes, a.notes...)
		if a.diagnostics != "" {
			diagnostics = append(diagnostics, a.diagnostics)
		}
		if a.lint != nil {
			if _, seen := lints[op.path]; !seen {
				order = append(order, op.path)
			}
			lints[op.path] = a.lint
		}
	}
	result := &patchResult{Success: len(errs) == 0, Diff: strings.Join(diffs, "\n"), FilesModified: files["modified"], FilesCreated: files["created"],
		FilesDeleted: files["deleted"], LSPDiagnostics: strings.Join(diagnostics, "\n\n"), writes: writes, Note: strings.Join(strs.Unique(notes), "\n")}
	if len(errs) > 0 {
		result.Error = "Apply phase failed (state may be inconsistent — run `git diff` to assess):\n" + bullets(errs)
	}
	if len(order) > 0 {
		result.Lint = orderedLints{order, lints}
	}
	return result
}

// Lints of a patch by file, in the order the files were written
type orderedLints struct {
	order []string
	lints map[string]*lintResult
}

func (l orderedLints) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteString("{")
	for i, path := range l.order {
		raw, err := l.lints[path].MarshalJSON()
		if err != nil {
			return nil, err
		}
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(encode(path) + ":" + string(raw))
	}
	b.WriteString("}")
	return []byte(b.String()), nil
}

// Applied write: the error, or its diff with what the write said and the sha256 of the bytes it replaced
func written(result *writeResult, text, path string, read *string) applied {
	if result.Error != "" {
		return applied{text: result.Error}
	}
	w := &patchWrite{path: path}
	if read != nil {
		w.read, w.readKnown = *read, true
	}
	if result.sha256 != "" {
		w.written, w.writtenKnown = result.sha256, true
	}
	return applied{ok: true, text: text, diagnostics: result.LSPDiagnostics, lint: result.Lint, write: w}
}

// Unified diff of a file from its old content, a deletion when there is no new one
func fileDiff(path, old string, updated *string) string {
	var after []string
	to := "/dev/null"
	if updated != nil {
		after, to = diff.Lines(*updated), "b/"+path
	}
	return diff.Unified(diff.Lines(old), after, "a/"+path, to, 3)
}

// Creates a file from the added lines, refusing one that is already there
func (o *ops) applyAdd(op *operation) applied {
	existing := o.readRaw(op.path)
	if existing.Error == "" {
		return applied{text: op.path + ": file already exists — use Update File, not Add File"}
	}
	if !existing.NotFound {
		return applied{text: op.path + ": could not confirm the path is free — " + existing.Error}
	}
	lines := op.added()
	plus := make([]string, len(lines))
	for i, l := range lines {
		plus[i] = "+" + l
	}
	empty := ""
	return written(o.writeFile(op.path, strings.Join(lines, "\n"), nil), "--- /dev/null\n+++ b/"+op.path+"\n"+strings.Join(plus, "\n"), op.path, &empty)
}

// Deletes a file, answering with the diff of what it held
func (o *ops) applyDelete(op *operation) applied {
	existing := o.readRaw(op.path)
	if existing.Error != "" {
		return applied{text: "Cannot delete " + op.path + ": file not found"}
	}
	failed := o.remove(op.path)
	text := fileDiff(op.path, existing.Content, nil)
	if text == "" {
		text = "# Deleted: " + op.path
	}
	if failed != "" {
		return applied{text: failed}
	}
	return applied{ok: true, text: text}
}

// Moves a file after checking again that the destination is free
func (o *ops) applyMove(op *operation) applied {
	destination := o.readRaw(op.to)
	if destination.Error == "" {
		return applied{text: op.to + ": destination already exists — move would overwrite"}
	}
	if !destination.NotFound {
		return applied{text: op.to + ": could not confirm the destination is free — " + destination.Error}
	}
	if failed := o.move(op.path, op.to); failed != "" {
		return applied{text: failed}
	}
	return applied{ok: true, text: "# Moved: " + op.path + " -> " + op.to}
}

// Places an addition only hunk after the line of its one hint, or at the end of the file
func addition(content string, h *hunk, text string) (string, string) {
	if h.hint != "" {
		n, unclear := ambiguous(content, h.hint, " — provide a more unique hint")
		if unclear != "" {
			return "", "Addition-only hunk: " + unclear
		}
		if n == 1 {
			at := strings.Index(content, h.hint)
			eol := strings.IndexByte(content[at:], '\n')
			if eol < 0 {
				return content + "\n" + text, ""
			}
			eol += at
			return content[:eol+1] + text + "\n" + content[eol+1:], ""
		}
	}
	return strings.TrimRight(content, "\n") + "\n" + text + "\n", ""
}

// Byte offset n characters before at, or after it, clamped at the ends
func back(s string, at, n int) int {
	for ; n > 0 && at > 0; n-- {
		_, size := utf8.DecodeLastRuneInString(s[:at])
		at -= size
	}
	return at
}

func forward(s string, at, n int) int {
	for ; n > 0 && at < len(s); n-- {
		_, size := utf8.DecodeRuneInString(s[at:])
		at += size
	}
	return at
}

// Applies each hunk through the fuzzy matcher, looking again near its hint when it does not match, then writes the file once
func (o *ops) applyUpdate(op *operation) applied {
	existing := o.readRaw(op.path)
	if existing.Error != "" {
		return applied{text: "Cannot read file: " + existing.Error}
	}
	current := existing.Content
	content := current
	var notes, used []string
	for _, h := range op.hunks {
		search, replace := h.split()
		if len(search) > 0 && slices.Equal(search, replace) {
			continue
		}
		pattern, replacement := strings.Join(search, "\n"), strings.Join(replace, "\n")
		if len(search) == 0 {
			next, failed := addition(content, h, replacement)
			if failed != "" {
				return applied{text: failed}
			}
			content = next
			continue
		}
		result, err := fuzzy.Replace(content, pattern, replacement, false)
		if err == nil || result.Count > 0 {
			content = result.Content
			if result.Count > 0 {
				used = append(used, result.Strategy)
				notes = appendNote(notes, result.Note)
			}
			continue
		}
		reason := err.Error()
		if h.hint != "" {
			if at := strings.Index(content, h.hint); at >= 0 {
				start, end := back(content, at, 500), forward(content, at, 2000)
				windowed, werr := fuzzy.Replace(content[start:end], pattern, replacement, false)
				if werr != nil {
					reason = werr.Error()
				}
				if windowed.Count > 0 {
					used = append(used, windowed.Strategy)
					notes = appendNote(notes, windowed.Note)
					content = content[:start] + windowed.Content + content[end:]
					reason = ""
				}
			}
		}
		if reason != "" {
			if fuzzy.Applied(content, pattern, replacement) {
				continue
			}
			return applied{text: "Could not apply hunk: " + reason + fuzzy.NoMatchHint(reason, 0, pattern, content)}
		}
	}
	read := existing.sha256
	result := written(o.writeFile(op.path, content, &current), fileDiff(op.path, current, &content), op.path, &read)
	if !result.ok {
		return result
	}
	if note := fuzzy.BoundaryNote(used, "the hunk's", " in "+op.path); note != "" {
		notes = append(notes, note)
	}
	result.notes = notes
	return result
}

func appendNote(notes []string, note string) []string {
	if note == "" {
		return notes
	}
	return append(notes, note)
}

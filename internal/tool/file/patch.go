package file

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

type opKind int

const (
	opAdd opKind = iota
	opUpdate
	opDeleteFile
	opMove
)

type hunkLine struct {
	prefix  byte
	content string
}

type hunk struct {
	hint  string
	lines []hunkLine
}

type patchOp struct {
	kind    opKind
	path    string
	newPath string
	hunks   []hunk
}

var (
	beginMarker  = regexp.MustCompile(`^\*\*\*[` + spaceClass + `]*Begin[` + spaceClass + `]+Patch[` + spaceClass + `]*$`)
	endMarker    = regexp.MustCompile(`^\*\*\*[` + spaceClass + `]*End[` + spaceClass + `]+Patch[` + spaceClass + `]*$`)
	updateMarker = regexp.MustCompile(`^\*\*\*[` + spaceClass + `]*Update[` + spaceClass + `]+File:[` + spaceClass + `]*(.+)`)
	addMarker    = regexp.MustCompile(`^\*\*\*[` + spaceClass + `]*Add[` + spaceClass + `]+File:[` + spaceClass + `]*(.+)`)
	deleteMarker = regexp.MustCompile(`^\*\*\*[` + spaceClass + `]*Delete[` + spaceClass + `]+File:[` + spaceClass + `]*(.+)`)
	moveMarker   = regexp.MustCompile(`^\*\*\*[` + spaceClass + `]*Move[` + spaceClass + `]+File:[` + spaceClass + `]*(.+?)[` + spaceClass + `]*->[` + spaceClass + `]*(.+)`)
	hintMarker   = regexp.MustCompile(`^@@[` + spaceClass + `]*(.+?)[` + spaceClass + `]*@@`)
)

func matchOpMarker(line string) (patchOp, bool) {
	if m := updateMarker.FindStringSubmatch(line); m != nil {
		return patchOp{kind: opUpdate, path: trimSpace(m[1])}, true
	}
	if m := addMarker.FindStringSubmatch(line); m != nil {
		return patchOp{kind: opAdd, path: trimSpace(m[1])}, true
	}
	if m := deleteMarker.FindStringSubmatch(line); m != nil {
		return patchOp{kind: opDeleteFile, path: trimSpace(m[1])}, true
	}
	if m := moveMarker.FindStringSubmatch(line); m != nil {
		return patchOp{kind: opMove, path: trimSpace(m[1]), newPath: trimSpace(m[2])}, true
	}
	return patchOp{}, false
}

func parsePatch(text string) ([]patchOp, string) {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSuffix(line, "\r")
	}
	startIdx, endIdx := -1, len(lines)
	for i, line := range lines {
		if beginMarker.MatchString(line) {
			startIdx = i
		} else if endMarker.MatchString(line) {
			endIdx = i
			break
		}
	}
	var ops []patchOp
	var current *patchOp
	var currentHunk *hunk
	flushHunk := func() {
		if current != nil && currentHunk != nil && len(currentHunk.lines) > 0 {
			current.hunks = append(current.hunks, *currentHunk)
		}
	}
	flush := func() {
		if current != nil {
			flushHunk()
			ops = append(ops, *current)
		}
	}
	for _, line := range lines[startIdx+1 : endIdx] {
		if op, ok := matchOpMarker(line); ok {
			flush()
			current, currentHunk = &op, nil
			if op.kind == opAdd {
				currentHunk = &hunk{}
			}
			if op.kind == opDeleteFile || op.kind == opMove {
				ops = append(ops, op)
				current = nil
			}
			continue
		}
		if strings.HasPrefix(line, "@@") {
			if current != nil {
				flushHunk()
				currentHunk = &hunk{}
				if m := hintMarker.FindStringSubmatch(line); m != nil {
					currentHunk.hint = m[1]
				}
			}
			continue
		}
		if current == nil || line == "" {
			continue
		}
		if currentHunk == nil {
			currentHunk = &hunk{}
		}
		switch line[0] {
		case '+', '-', ' ':
			currentHunk.lines = append(currentHunk.lines, hunkLine{line[0], line[1:]})
		case '\\':
		default:
			currentHunk.lines = append(currentHunk.lines, hunkLine{' ', line})
		}
	}
	flush()
	var problems []string
	for _, op := range ops {
		if op.path == "" {
			problems = append(problems, "Operation with empty file path")
		}
		if op.kind == opUpdate && len(op.hunks) == 0 {
			problems = append(problems, "UPDATE "+quote(op.path)+": no hunks found")
		}
		if op.kind == opMove && op.newPath == "" {
			problems = append(problems, "MOVE "+quote(op.path)+": missing destination path (expected 'src -> dst')")
		}
	}
	if len(problems) > 0 {
		return nil, "Parse error: " + strings.Join(problems, "; ")
	}
	return ops, ""
}

func countOccurrences(text, pattern string) int {
	count := 0
	for at := 0; at <= len(text); {
		idx := strings.Index(text[at:], pattern)
		if idx < 0 {
			break
		}
		count++
		at += idx + 1
	}
	return count
}

func splitHunk(h hunk) ([]string, []string) {
	var search, replace []string
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

func sameLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func addedLines(op patchOp) []string {
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

func hintAmbiguity(content, hint, tail string) (int, string) {
	n := countOccurrences(content, hint)
	if n > 1 {
		return n, "context hint '" + hint + "' is ambiguous (" + strconv.Itoa(n) + " occurrences)" + tail
	}
	return n, ""
}

type patchOverlay struct {
	ops     *Operations
	pending map[string]string
	removed map[string]struct{}
}

func (v *patchOverlay) read(ctx context.Context, path string) (string, string, error) {
	if content, ok := v.pending[path]; ok {
		return content, "", nil
	}
	if _, gone := v.removed[path]; gone {
		return "", "file not found", nil
	}
	r, err := v.ops.ReadRaw(ctx, path)
	if err != nil {
		return "", "", err
	}
	if r.Error != "" {
		return "", r.Error, nil
	}
	return r.Content, "", nil
}

func (v *patchOverlay) occupied(ctx context.Context, path string) (string, error) {
	if _, ok := v.pending[path]; ok {
		return "exists", nil
	}
	if _, gone := v.removed[path]; gone {
		return "", nil
	}
	r, err := v.ops.ReadRaw(ctx, path)
	if err != nil {
		return "", err
	}
	if r.Error == "" {
		return "exists", nil
	}
	if r.NotFound {
		return "", nil
	}
	return "could not confirm the path is free — " + r.Error, nil
}

func (v *patchOverlay) remove(path string) {
	v.removed[path] = struct{}{}
	delete(v.pending, path)
}

func (o *Operations) validatePatch(ctx context.Context, ops []patchOp) ([]string, error) {
	var problems []string
	realChanges := 0
	overlay := &patchOverlay{ops: o, pending: make(map[string]string), removed: make(map[string]struct{})}

	validateUpdate := func(op patchOp) error {
		simulated, readErr, err := overlay.read(ctx, op.path)
		if err != nil {
			return err
		}
		if readErr != "" {
			problems = append(problems, op.path+": "+readErr)
			return nil
		}
		for index, h := range op.hunks {
			search, replace := splitHunk(h)
			if sameLines(search, replace) {
				for _, l := range h.lines {
					if l.prefix == '-' || l.prefix == '+' {
						realChanges++
						break
					}
				}
				continue
			}
			realChanges++
			if len(search) == 0 {
				if h.hint != "" {
					occurrences, ambiguous := hintAmbiguity(simulated, h.hint, "")
					if occurrences == 0 {
						problems = append(problems, op.path+": addition-only hunk context hint '"+h.hint+"' not found")
					} else if ambiguous != "" {
						problems = append(problems, op.path+": addition-only hunk "+ambiguous)
					}
				}
				continue
			}
			pattern, replacement := strings.Join(search, "\n"), strings.Join(replace, "\n")
			found, err := FuzzyReplace(ctx, simulated, pattern, replacement, false)
			matchText := ""
			var matchErr *MatchError
			switch {
			case errors.As(err, &matchErr):
				matchText = matchErr.Msg
			case err != nil:
				return err
			}
			if found.Count > 0 {
				simulated = found.Content
				continue
			}
			if alreadyApplied(simulated, pattern, replacement) {
				continue
			}
			label := "(no hint)"
			if h.hint != "" {
				label = "'" + h.hint + "'"
			}
			problem := op.path + ": hunk " + strconv.Itoa(index+1) + " " + label + " not found"
			if matchText != "" {
				problem += " — " + matchText
			}
			problems = append(problems, problem+NoMatchHint(matchText, 0, pattern, simulated))
		}
		overlay.pending[op.path] = simulated
		return nil
	}

	for _, op := range ops {
		if op.kind == opUpdate {
			if err := validateUpdate(op); err != nil {
				return nil, err
			}
			continue
		}
		realChanges++
		switch op.kind {
		case opDeleteFile:
			_, readErr, err := overlay.read(ctx, op.path)
			if err != nil {
				return nil, err
			}
			if readErr != "" {
				problems = append(problems, op.path+": file not found for deletion")
			} else {
				overlay.remove(op.path)
			}
		case opMove:
			if op.newPath == "" {
				problems = append(problems, op.path+": MOVE operation missing destination path")
				continue
			}
			srcContent, srcErr, err := overlay.read(ctx, op.path)
			if err != nil {
				return nil, err
			}
			if srcErr != "" {
				problems = append(problems, op.path+": source file not found for move")
			}
			taken, err := overlay.occupied(ctx, op.newPath)
			if err != nil {
				return nil, err
			}
			switch {
			case taken == "exists":
				problems = append(problems, op.newPath+": destination already exists — move would overwrite")
			case taken != "":
				problems = append(problems, op.newPath+": "+taken)
			case srcErr == "":
				overlay.pending[op.newPath] = srcContent
				overlay.remove(op.path)
			}
		case opAdd:
			taken, err := overlay.occupied(ctx, op.path)
			if err != nil {
				return nil, err
			}
			switch {
			case taken == "exists":
				problems = append(problems, op.path+": file already exists — use Update File, not Add File")
			case taken != "":
				problems = append(problems, op.path+": "+taken)
			default:
				delete(overlay.removed, op.path)
				overlay.pending[op.path] = strings.Join(addedLines(op), "\n")
			}
		}
	}
	if len(problems) == 0 && realChanges == 0 {
		problems = append(problems, "Patch contains no changes (only context lines were provided)")
	}
	return problems, nil
}

type applied struct {
	ok      bool
	payload string
	lsp     string
	lint    *LintResult
	write   *PatchWrite
	notes   []string
}

func failApply(msg string) applied {
	return applied{payload: msg}
}

func writtenResult(w WriteResult, diff, path, readSHA string, readKnown bool) applied {
	if w.Error != "" {
		return failApply(w.Error)
	}
	return applied{ok: true, payload: diff, lsp: w.LSPDiagnostics, lint: w.Lint,
		write: &PatchWrite{Path: path, ReadSHA256: readSHA, ReadKnown: readKnown, WrittenSHA256: w.ContentSHA256}}
}

func (o *Operations) applyAdd(ctx context.Context, op patchOp) (applied, error) {
	readBack, err := o.ReadRaw(ctx, op.path)
	if err != nil {
		return applied{}, err
	}
	if readBack.Error == "" {
		return failApply(op.path + ": file already exists — use Update File, not Add File"), nil
	}
	if !readBack.NotFound {
		return failApply(op.path + ": could not confirm the path is free — " + readBack.Error), nil
	}
	lines := addedLines(op)
	written, err := o.Write(ctx, op.path, strings.Join(lines, "\n"), nil)
	if err != nil {
		return applied{}, err
	}
	marked := make([]string, len(lines))
	for i, line := range lines {
		marked[i] = "+" + line
	}
	diff := "--- /dev/null\n+++ b/" + op.path + "\n" + strings.Join(marked, "\n")
	return writtenResult(written, diff, op.path, "", true), nil
}

func (o *Operations) applyDelete(ctx context.Context, op patchOp) (applied, error) {
	read, err := o.ReadRaw(ctx, op.path)
	if err != nil {
		return applied{}, err
	}
	if read.Error != "" {
		return failApply("Cannot delete " + op.path + ": file not found"), nil
	}
	deleted, err := o.Delete(ctx, op.path)
	if err != nil {
		return applied{}, err
	}
	if deleted.Error != "" {
		return failApply(deleted.Error), nil
	}
	diff := unifiedDiff(splitLines(read.Content, true), nil, "a/"+op.path, "/dev/null")
	if diff == "" {
		diff = "# Deleted: " + op.path
	}
	return applied{ok: true, payload: diff}, nil
}

func (o *Operations) applyMove(ctx context.Context, op patchOp) (applied, error) {
	dst, err := o.ReadRaw(ctx, op.newPath)
	if err != nil {
		return applied{}, err
	}
	if dst.Error == "" {
		return failApply(op.newPath + ": destination already exists — move would overwrite"), nil
	}
	if !dst.NotFound {
		return failApply(op.newPath + ": could not confirm the destination is free — " + dst.Error), nil
	}
	moved, err := o.Move(ctx, op.path, op.newPath)
	if err != nil {
		return applied{}, err
	}
	if moved.Error != "" {
		return failApply(moved.Error), nil
	}
	return applied{ok: true, payload: "# Moved: " + op.path + " -> " + op.newPath}, nil
}

func insertAdditionOnly(content string, h hunk, insert string) (string, string) {
	if h.hint != "" {
		occurrences, ambiguous := hintAmbiguity(content, h.hint, " — provide a more unique hint")
		if ambiguous != "" {
			return "", "Addition-only hunk: " + ambiguous
		}
		if occurrences == 1 {
			at := strings.Index(content, h.hint)
			eol := strings.Index(content[at:], "\n")
			if eol == -1 {
				return content + "\n" + insert, ""
			}
			eol += at
			return content[:eol+1] + insert + "\n" + content[eol+1:], ""
		}
	}
	return strings.TrimRight(content, "\n") + "\n" + insert + "\n", ""
}

func (o *Operations) applyUpdate(ctx context.Context, op patchOp) (applied, error) {
	read, err := o.ReadRaw(ctx, op.path)
	if err != nil {
		return applied{}, err
	}
	if read.Error != "" {
		return failApply("Cannot read file: " + read.Error), nil
	}
	current := read.Content
	updated := current
	var notes, used []string
	replaceIn := func(content, pattern, replacement string) (Replacement, string, error) {
		found, err := FuzzyReplace(ctx, content, pattern, replacement, false)
		var matchErr *MatchError
		switch {
		case errors.As(err, &matchErr):
			return found, matchErr.Msg, nil
		case err != nil:
			return found, "", err
		}
		if found.Note != "" {
			notes = append(notes, found.Note)
		}
		return found, "", nil
	}
	for _, h := range op.hunks {
		search, replace := splitHunk(h)
		if len(search) > 0 && sameLines(search, replace) {
			continue
		}
		pattern, replacement := strings.Join(search, "\n"), strings.Join(replace, "\n")
		if len(search) == 0 {
			inserted, problem := insertAdditionOnly(updated, h, replacement)
			if problem != "" {
				return failApply(problem), nil
			}
			updated = inserted
			continue
		}
		found, matchText, err := replaceIn(updated, pattern, replacement)
		if err != nil {
			return applied{}, err
		}
		updated = found.Content
		if !(matchText != "" && found.Count == 0) {
			if found.Count > 0 {
				used = append(used, found.Strategy)
			}
			continue
		}
		hintPos := -1
		if h.hint != "" {
			hintPos = strings.Index(updated, h.hint)
		}
		if hintPos != -1 {
			windowStart := charOffsetBack(updated, hintPos, 500)
			windowEnd := charOffsetForward(updated, hintPos, 2000)
			windowed, windowText, err := replaceIn(updated[windowStart:windowEnd], pattern, replacement)
			if err != nil {
				return applied{}, err
			}
			matchText = windowText
			if windowed.Count > 0 {
				used = append(used, windowed.Strategy)
				updated = updated[:windowStart] + windowed.Content + updated[windowEnd:]
				matchText = ""
			}
		}
		if matchText != "" {
			if alreadyApplied(updated, pattern, replacement) {
				continue
			}
			return failApply("Could not apply hunk: " + matchText + NoMatchHint(matchText, 0, pattern, updated)), nil
		}
	}
	written, err := o.Write(ctx, op.path, updated, &current)
	if err != nil {
		return applied{}, err
	}
	result := writtenResult(written, fileDiff(current, updated, op.path), op.path, read.ContentSHA256, read.ContentSHA256 != "")
	if !result.ok {
		return result, nil
	}
	if note := boundaryNote(used, "the hunk's", " in "+op.path); note != "" {
		notes = append(notes, note)
	}
	result.notes = notes
	return result, nil
}

func bullets(problems []string) string {
	rows := make([]string, len(problems))
	for i, p := range problems {
		rows[i] = "  • " + p
	}
	return strings.Join(rows, "\n")
}

func (o *Operations) Patch(ctx context.Context, patchText string) (PatchResult, error) {
	ops, parseProblem := parsePatch(patchText)
	if parseProblem != "" {
		return PatchResult{Error: "Failed to parse patch: " + parseProblem}, nil
	}
	problems, err := o.validatePatch(ctx, ops)
	if err != nil {
		return PatchResult{}, err
	}
	if len(problems) > 0 {
		return PatchResult{Error: "Patch validation failed (no files were modified):\n" + bullets(problems)}, nil
	}
	var result PatchResult
	var diffs, notes, lspBlocks, failures []string
	seenNotes := make(map[string]struct{})
	for _, op := range ops {
		var outcome applied
		var verb string
		var bucket *[]string
		var err error
		switch op.kind {
		case opAdd:
			verb, bucket = "add", &result.FilesCreated
			outcome, err = o.applyAdd(ctx, op)
		case opDeleteFile:
			verb, bucket = "delete", &result.FilesDeleted
			outcome, err = o.applyDelete(ctx, op)
		case opMove:
			verb, bucket = "move", &result.FilesModified
			outcome, err = o.applyMove(ctx, op)
		default:
			verb, bucket = "update", &result.FilesModified
			outcome, err = o.applyUpdate(ctx, op)
		}
		if err != nil {
			failures = append(failures, "Error processing "+op.path+": "+err.Error())
			continue
		}
		if !outcome.ok {
			failures = append(failures, "Failed to "+verb+" "+op.path+": "+outcome.payload)
			continue
		}
		if op.kind == opMove {
			*bucket = append(*bucket, op.path+" -> "+op.newPath)
		} else {
			*bucket = append(*bucket, op.path)
		}
		diffs = append(diffs, outcome.payload)
		if outcome.write != nil {
			result.Writes = append(result.Writes, *outcome.write)
		}
		for _, note := range outcome.notes {
			if _, dup := seenNotes[note]; !dup {
				seenNotes[note] = struct{}{}
				notes = append(notes, note)
			}
		}
		if outcome.lsp != "" {
			lspBlocks = append(lspBlocks, outcome.lsp)
		}
		if outcome.lint != nil {
			replaced := false
			for i := range result.LintByFile {
				if result.LintByFile[i].Path == op.path {
					result.LintByFile[i].Lint, replaced = *outcome.lint, true
					break
				}
			}
			if !replaced {
				result.LintByFile = append(result.LintByFile, FileLint{Path: op.path, Lint: *outcome.lint})
			}
		}
	}
	result.Success = len(failures) == 0
	if len(failures) > 0 {
		result.Error = "Apply phase failed (state may be inconsistent — run `git diff` to assess):\n" + bullets(failures)
	}
	result.Diff = strings.Join(diffs, "\n")
	result.LSPDiagnostics = strings.Join(lspBlocks, "\n\n")
	result.Note = strings.Join(notes, "\n")
	return result, nil
}

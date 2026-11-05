package tools

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// V4A operation kinds
const (
	v4aAdd    = "add"
	v4aUpdate = "update"
	v4aDelete = "delete"
	v4aMove   = "move"
)

// One hunk line with its prefix of space, minus or plus
type v4aHunkLine struct {
	prefix  byte
	content string
}

// One hunk with its optional context hint
type v4aHunk struct {
	contextHint string
	lines       []v4aHunkLine
}

// One patch operation, newPath set only for a move
type v4aOperation struct {
	kind     string
	filePath string
	newPath  string
	hunks    []*v4aHunk
}

// File backend the V4A applier reads and writes through
type v4aFileOps interface {
	ReadFileRaw(path string) *ReadResult
	WriteFile(path, content string, preContent *string) *WriteResult
	DeleteFile(path string) *WriteResult
	MoveFile(src, dst string) *WriteResult
}

// Optional face of a file backend carrying the metered edit call's probe
type v4aProbed interface {
	editProbe() *editProbe
}

// Probe of the metered edit call the backend belongs to, nil when the call is not metered
func v4aProbe(fileOps v4aFileOps) *editProbe {
	if p, ok := fileOps.(v4aProbed); ok {
		return p.editProbe()
	}
	return nil
}

// Python str.isspace characters as a regexp class
const pyWSClass = `[\t\n\v\f\r \x{1c}-\x{1f}\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]`

var (
	v4aBeginMarker = regexp.MustCompile(`^\*\*\*` + pyWSClass + `*Begin` + pyWSClass + `+Patch` + pyWSClass + `*$`)
	v4aEndMarker   = regexp.MustCompile(`^\*\*\*` + pyWSClass + `*End` + pyWSClass + `+Patch` + pyWSClass + `*$`)
	v4aOpMarkers   = []struct {
		kind string
		re   *regexp.Regexp
	}{
		{v4aUpdate, regexp.MustCompile(`^\*\*\*` + pyWSClass + `*Update` + pyWSClass + `+File:` + pyWSClass + `*(.+)`)},
		{v4aAdd, regexp.MustCompile(`^\*\*\*` + pyWSClass + `*Add` + pyWSClass + `+File:` + pyWSClass + `*(.+)`)},
		{v4aDelete, regexp.MustCompile(`^\*\*\*` + pyWSClass + `*Delete` + pyWSClass + `+File:` + pyWSClass + `*(.+)`)},
		{v4aMove, regexp.MustCompile(`^\*\*\*` + pyWSClass + `*Move` + pyWSClass + `+File:` + pyWSClass + `*(.+?)` + pyWSClass + `*->` + pyWSClass + `*(.+)`)},
	}
	v4aHintRe = regexp.MustCompile(`^@@` + pyWSClass + `*(.+?)` + pyWSClass + `*@@`)
)

// Python str.strip with no arguments
func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

// Python repr of a str with its quote choice and escapes
func pyReprStr(s string) string {
	quote := '\''
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteRune(quote)
	for i := 0; i < len(s); {
		r, size := wtf8DecodeRune(s[i:])
		i += size
		switch {
		case r == quote || r == '\\':
			b.WriteRune('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < ' ' || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x7f || unicode.IsPrint(r):
			b.WriteRune(r)
		case r <= 0xff:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r <= 0xffff:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteRune(quote)
	return b.String()
}

// Parses V4A text into operations or returns the joined parse errors
func parseV4APatch(patchContent string) ([]*v4aOperation, string) {
	lines := strings.Split(patchContent, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimSuffix(ln, "\r")
	}
	startIdx, endIdx := -1, len(lines)
	for i, line := range lines {
		if v4aBeginMarker.MatchString(line) {
			startIdx = i
		} else if v4aEndMarker.MatchString(line) {
			endIdx = i
			break
		}
	}
	var operations []*v4aOperation
	var currentOp *v4aOperation
	var currentHunk *v4aHunk
	flushHunk := func() {
		if currentOp != nil && currentHunk != nil && len(currentHunk.lines) > 0 {
			currentOp.hunks = append(currentOp.hunks, currentHunk)
		}
	}
	flush := func() {
		if currentOp != nil {
			flushHunk()
			operations = append(operations, currentOp)
		}
	}
	for _, line := range lines[startIdx+1 : endIdx] {
		kind, m := "", []string(nil)
		for _, om := range v4aOpMarkers {
			if m = om.re.FindStringSubmatch(line); m != nil {
				kind = om.kind
				break
			}
		}
		switch {
		case m != nil:
			flush()
			currentOp = &v4aOperation{kind: kind, filePath: pyStrip(m[1])}
			if kind == v4aMove {
				currentOp.newPath = pyStrip(m[2])
			}
			currentHunk = nil
			if kind == v4aAdd {
				currentHunk = &v4aHunk{}
			}
			if kind == v4aDelete || kind == v4aMove {
				operations = append(operations, currentOp)
				currentOp = nil
			}
		case strings.HasPrefix(line, "@@"):
			if currentOp != nil {
				flushHunk()
				currentHunk = &v4aHunk{}
				if hm := v4aHintRe.FindStringSubmatch(line); hm != nil {
					currentHunk.contextHint = hm[1]
				}
			}
		case currentOp != nil && line != "":
			if currentHunk == nil {
				currentHunk = &v4aHunk{}
			}
			switch line[0] {
			case '+', '-', ' ':
				currentHunk.lines = append(currentHunk.lines, v4aHunkLine{line[0], line[1:]})
			case '\\':
			default:
				currentHunk.lines = append(currentHunk.lines, v4aHunkLine{' ', line})
			}
		}
	}
	flush()
	var parseErrors []string
	for _, op := range operations {
		if op.filePath == "" {
			parseErrors = append(parseErrors, "Operation with empty file path")
		}
		if op.kind == v4aUpdate && len(op.hunks) == 0 {
			parseErrors = append(parseErrors, "UPDATE "+pyReprStr(op.filePath)+": no hunks found")
		}
		if op.kind == v4aMove && op.newPath == "" {
			parseErrors = append(parseErrors, "MOVE "+pyReprStr(op.filePath)+": missing destination path (expected 'src -> dst')")
		}
	}
	if len(parseErrors) > 0 {
		return []*v4aOperation{}, "Parse error: " + strings.Join(parseErrors, "; ")
	}
	if operations == nil {
		operations = []*v4aOperation{}
	}
	return operations, ""
}

// Counts occurrences advancing one character per hit so overlaps count
func countOccurrences(text, pattern string) int {
	if pattern == "" {
		return wtf8RuneCount(text) + 1
	}
	n := 0
	for i := 0; i <= len(text)-len(pattern); i++ {
		if text[i:i+len(pattern)] == pattern {
			n++
		}
	}
	return n
}

// Search lines are context plus removed, replace lines are context plus added
func splitHunk(h *v4aHunk) ([]string, []string) {
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

// Did you mean suffix that never lets a hint failure mask the real error
func v4aNoMatchHint(errText, searchPattern, content string) (hint string) {
	defer func() {
		if recover() != nil {
			hint = ""
		}
	}()
	return formatNoMatchHint(errText, 0, searchPattern, content)
}

// Occurrence count and ambiguity error for an addition only hunk hint
func hintAmbiguity(content, hint, tail string) (int, string) {
	n := countOccurrences(content, hint)
	if n > 1 {
		return n, fmt.Sprintf("context hint '%s' is ambiguous (%d occurrences)%s", hint, n, tail)
	}
	return n, ""
}

func stringSlicesEqual(a, b []string) bool {
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

// Joins the plus lines of every hunk as the content an Add creates
func v4aAddedContent(op *v4aOperation) []string {
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

// Dry runs every operation in order over an overlay and returns the errors found
func validateV4AOperations(operations []*v4aOperation, fileOps v4aFileOps) []string {
	var errs []string
	realChangeCount := 0
	pendingContent := map[string]string{}
	removedPaths := map[string]bool{}
	read := func(path string) (string, string) {
		if c, ok := pendingContent[path]; ok {
			return c, ""
		}
		if removedPaths[path] {
			return "", "file not found"
		}
		r := fileOps.ReadFileRaw(path)
		if r.Error != "" {
			return "", r.Error
		}
		return r.Content, ""
	}
	occupied := func(path string) string {
		if _, ok := pendingContent[path]; ok {
			return "exists"
		}
		if removedPaths[path] {
			return ""
		}
		r := fileOps.ReadFileRaw(path)
		if r.Error == "" {
			return "exists"
		}
		if r.NotFound {
			return ""
		}
		return "could not confirm the path is free — " + r.Error
	}
	validateUpdate := func(op *v4aOperation) {
		simulated, readErr := read(op.filePath)
		if readErr != "" {
			errs = append(errs, op.filePath+": "+readErr)
			return
		}
		for idx, h := range op.hunks {
			hunkIndex := idx + 1
			searchLines, replaceLines := splitHunk(h)
			if stringSlicesEqual(searchLines, replaceLines) {
				for _, l := range h.lines {
					if l.prefix == '-' || l.prefix == '+' {
						realChangeCount++
						break
					}
				}
				continue
			}
			realChangeCount++
			if len(searchLines) == 0 {
				if h.contextHint != "" {
					occurrences, ambiguous := hintAmbiguity(simulated, h.contextHint, "")
					if occurrences == 0 {
						errs = append(errs, fmt.Sprintf("%s: addition-only hunk context hint '%s' not found", op.filePath, h.contextHint))
					} else if ambiguous != "" {
						errs = append(errs, op.filePath+": addition-only hunk "+ambiguous)
					}
				}
				continue
			}
			searchPattern, replacement := strings.Join(searchLines, "\n"), strings.Join(replaceLines, "\n")
			newSimulated, count, _, matchErr := fuzzyFindAndReplace(simulated, searchPattern, replacement, false, nil, v4aProbe(fileOps))
			if count > 0 {
				simulated = newSimulated
			} else if !isAlreadyApplied(simulated, searchPattern, replacement) {
				label := "(no hint)"
				if h.contextHint != "" {
					label = "'" + h.contextHint + "'"
				}
				msg := fmt.Sprintf("%s: hunk %d %s not found", op.filePath, hunkIndex, label)
				if matchErr != "" {
					msg += " — " + matchErr
				}
				errs = append(errs, msg+v4aNoMatchHint(matchErr, searchPattern, simulated))
			}
		}
		pendingContent[op.filePath] = simulated
	}
	remove := func(path string) {
		removedPaths[path] = true
		delete(pendingContent, path)
	}
	for _, op := range operations {
		if op.kind == v4aUpdate {
			validateUpdate(op)
			continue
		}
		realChangeCount++
		switch op.kind {
		case v4aDelete:
			if _, e := read(op.filePath); e != "" {
				errs = append(errs, op.filePath+": file not found for deletion")
			} else {
				remove(op.filePath)
			}
		case v4aMove:
			if op.newPath == "" {
				errs = append(errs, op.filePath+": MOVE operation missing destination path")
				continue
			}
			srcContent, srcErr := read(op.filePath)
			if srcErr != "" {
				errs = append(errs, op.filePath+": source file not found for move")
			}
			dstTaken := occupied(op.newPath)
			switch {
			case dstTaken == "exists":
				errs = append(errs, op.newPath+": destination already exists — move would overwrite")
			case dstTaken != "":
				errs = append(errs, op.newPath+": "+dstTaken)
			case srcErr == "":
				pendingContent[op.newPath] = srcContent
				remove(op.filePath)
			}
		case v4aAdd:
			addTaken := occupied(op.filePath)
			switch {
			case addTaken == "exists":
				errs = append(errs, op.filePath+": file already exists — use Update File, not Add File")
			case addTaken != "":
				errs = append(errs, op.filePath+": "+addTaken)
			default:
				delete(removedPaths, op.filePath)
				pendingContent[op.filePath] = strings.Join(v4aAddedContent(op), "\n")
			}
		}
	}
	if len(errs) == 0 && realChangeCount == 0 {
		errs = append(errs, "Patch contains no changes (only context lines were provided)")
	}
	return errs
}

// Outcome of one apply handler with its diff or error, diagnostics, write record and notes
type v4aApplyResult struct {
	ok      bool
	payload string
	lsp     string
	lint    *pyJSONObject
	write   *patchWrite
	notes   []string
}

func v4aFail(errText string) v4aApplyResult { return v4aApplyResult{payload: errText} }

// Success with LSP and lint carried from the write and the sha record of the replaced bytes
func v4aWritten(result *WriteResult, diff, path string, readSha *string) v4aApplyResult {
	if result.Error != "" {
		return v4aFail(result.Error)
	}
	w := &patchWrite{path: path}
	if readSha != nil {
		w.readSha, w.readKnown = *readSha, true
	}
	if result.ContentSha256 != nil {
		w.writtenSha, w.writtenKnown = *result.ContentSha256, true
	}
	return v4aApplyResult{ok: true, payload: diff, lsp: result.LSPDiagnostics, lint: result.Lint, write: w}
}

// Python str.splitlines with keepends over the line boundaries str recognizes
func pySplitLinesKeepEnds(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		end := -1
		switch r {
		case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', '\x85', '\u2028', '\u2029':
			end = i + size
		case '\r':
			end = i + 1
			if i+1 < len(s) && s[i+1] == '\n' {
				end = i + 2
			}
		}
		if end >= 0 {
			out = append(out, s[start:end])
			start, i = end, end
			continue
		}
		i += size
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// Unified diff from a slash path to b slash path, deleted when newContent is nil
func v4aUnifiedDiff(path, old string, newContent *string) string {
	var b []string
	to := "/dev/null"
	if newContent != nil {
		b = pySplitLinesKeepEnds(*newContent)
		to = "b/" + path
	}
	return strings.Join(unifiedDiff(pySplitLinesKeepEnds(old), b, "a/"+path, to, 3), "")
}

func v4aBullets(errs []string) string {
	rows := make([]string, len(errs))
	for i, e := range errs {
		rows[i] = "  • " + e
	}
	return strings.Join(rows, "\n")
}

// Validates every operation then applies them, failing atomically when validation fails
func applyV4AOperations(operations []*v4aOperation, fileOps v4aFileOps) *PatchResult {
	errs := validateV4AOperations(operations, fileOps)
	if len(errs) > 0 {
		return &PatchResult{Error: "Patch validation failed (no files were modified):\n" + v4aBullets(errs)}
	}
	files := map[string][]string{"created": {}, "deleted": {}, "modified": {}}
	var allDiffs, notes, lspBlocks []string
	lintResults := &pyJSONObject{vals: map[string]any{}}
	var writes []patchWrite
	for _, op := range operations {
		d := v4aApplyDispatch[op.kind]
		res, failed := runV4AHandler(d.handler, op, fileOps)
		if !res.ok {
			prefix := "Failed to " + d.verb
			if failed {
				prefix = "Error processing"
			}
			errs = append(errs, fmt.Sprintf("%s %s: %s", prefix, op.filePath, res.payload))
			continue
		}
		if op.kind == v4aMove {
			files[d.bucket] = append(files[d.bucket], op.filePath+" -> "+op.newPath)
		} else {
			files[d.bucket] = append(files[d.bucket], op.filePath)
		}
		allDiffs = append(allDiffs, res.payload)
		if res.write != nil {
			writes = append(writes, *res.write)
		}
		notes = append(notes, res.notes...)
		if res.lsp != "" {
			lspBlocks = append(lspBlocks, res.lsp)
		}
		if res.lint != nil && len(res.lint.keys) > 0 {
			lintResults.set(op.filePath, res.lint)
		}
	}
	out := &PatchResult{
		Success:        len(errs) == 0,
		Diff:           strings.Join(allDiffs, "\n"),
		FilesModified:  files["modified"],
		FilesCreated:   files["created"],
		FilesDeleted:   files["deleted"],
		LSPDiagnostics: strings.Join(lspBlocks, "\n\n"),
		Writes:         writes,
		Note:           strings.Join(dedupeOrdered(notes), "\n"),
	}
	if len(errs) > 0 {
		out.Error = "Apply phase failed (state may be inconsistent — run `git diff` to assess):\n" + v4aBullets(errs)
	}
	if len(lintResults.keys) > 0 {
		out.Lint = lintResults
	}
	return out
}

// Order preserving dedupe like dict.fromkeys
func dedupeOrdered(items []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range items {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// Runs one handler and reports a raised panic as an Error processing failure
func runV4AHandler(h func(*v4aOperation, v4aFileOps) v4aApplyResult, op *v4aOperation, fileOps v4aFileOps) (res v4aApplyResult, failed bool) {
	defer func() {
		if r := recover(); r != nil {
			res, failed = v4aApplyResult{payload: fmt.Sprint(r)}, true
		}
	}()
	return h(op, fileOps), false
}

// Creates a file from the plus lines and fails closed when the target already exists
func applyV4AAdd(op *v4aOperation, fileOps v4aFileOps) v4aApplyResult {
	readBack := fileOps.ReadFileRaw(op.filePath)
	if readBack.Error == "" {
		return v4aFail(op.filePath + ": file already exists — use Update File, not Add File")
	}
	if !readBack.NotFound {
		return v4aFail(op.filePath + ": could not confirm the path is free — " + readBack.Error)
	}
	contentLines := v4aAddedContent(op)
	result := fileOps.WriteFile(op.filePath, strings.Join(contentLines, "\n"), nil)
	plus := make([]string, len(contentLines))
	for i, l := range contentLines {
		plus[i] = "+" + l
	}
	diff := "--- /dev/null\n+++ b/" + op.filePath + "\n" + strings.Join(plus, "\n")
	return v4aWritten(result, diff, op.filePath, shaRef(""))
}

// Deletes a file and reports a real unified diff of the removed content
func applyV4ADelete(op *v4aOperation, fileOps v4aFileOps) v4aApplyResult {
	readResult := fileOps.ReadFileRaw(op.filePath)
	if readResult.Error != "" {
		return v4aFail("Cannot delete " + op.filePath + ": file not found")
	}
	result := fileOps.DeleteFile(op.filePath)
	diff := v4aUnifiedDiff(op.filePath, readResult.Content, nil)
	if diff == "" {
		diff = "# Deleted: " + op.filePath
	}
	if result.Error != "" {
		return v4aFail(result.Error)
	}
	return v4aApplyResult{ok: true, payload: diff}
}

// Moves a file after rechecking that the destination is still free
func applyV4AMove(op *v4aOperation, fileOps v4aFileOps) v4aApplyResult {
	dst := fileOps.ReadFileRaw(op.newPath)
	if dst.Error == "" {
		return v4aFail(op.newPath + ": destination already exists — move would overwrite")
	}
	if !dst.NotFound {
		return v4aFail(op.newPath + ": could not confirm the destination is free — " + dst.Error)
	}
	result := fileOps.MoveFile(op.filePath, op.newPath)
	if result.Error != "" {
		return v4aFail(result.Error)
	}
	return v4aApplyResult{ok: true, payload: "# Moved: " + op.filePath + " -> " + op.newPath}
}

// Places an addition only hunk after its unique context hint or at the end of the file
func insertAdditionOnly(newContent string, h *v4aHunk, insertText string) (string, string) {
	if h.contextHint != "" {
		occurrences, ambiguous := hintAmbiguity(newContent, h.contextHint, " — provide a more unique hint")
		if ambiguous != "" {
			return "", "Addition-only hunk: " + ambiguous
		}
		if occurrences == 1 {
			at := strings.Index(newContent, h.contextHint)
			eol := strings.IndexByte(newContent[at:], '\n')
			if eol == -1 {
				return newContent + "\n" + insertText, ""
			}
			eol += at
			return newContent[:eol+1] + insertText + "\n" + newContent[eol+1:], ""
		}
	}
	return strings.TrimRight(newContent, "\n") + "\n" + insertText + "\n", ""
}

// Byte offset n characters before pos, clamped at the start
func runeOffsetBack(s string, pos, n int) int {
	for ; n > 0 && pos > 0; n-- {
		_, size := wtf8DecodeLastRune(s[:pos])
		pos -= size
	}
	return pos
}

// Byte offset n characters after pos, clamped at the end
func runeOffsetFwd(s string, pos, n int) int {
	for ; n > 0 && pos < len(s); n-- {
		_, size := wtf8DecodeRune(s[pos:])
		pos += size
	}
	return pos
}

// Applies each hunk through fuzzy replace, retrying near the hint, then writes once
func applyV4AUpdate(op *v4aOperation, fileOps v4aFileOps) v4aApplyResult {
	readResult := fileOps.ReadFileRaw(op.filePath)
	if readResult.Error != "" {
		return v4aFail("Cannot read file: " + readResult.Error)
	}
	currentContent := readResult.Content
	newContent := currentContent
	var notes, strategies []string
	for _, h := range op.hunks {
		searchLines, replaceLines := splitHunk(h)
		if len(searchLines) > 0 && stringSlicesEqual(searchLines, replaceLines) {
			continue
		}
		searchPattern, replacement := strings.Join(searchLines, "\n"), strings.Join(replaceLines, "\n")
		if len(searchLines) == 0 {
			next, errText := insertAdditionOnly(newContent, h, replacement)
			if errText != "" {
				return v4aFail(errText)
			}
			newContent = next
			continue
		}
		next, count, strategy, errText := fuzzyFindAndReplace(newContent, searchPattern, replacement, false, &notes, v4aProbe(fileOps))
		newContent = next
		if !(errText != "" && count == 0) {
			if count > 0 {
				strategies = append(strategies, strategy)
			}
			continue
		}
		hintPos := -1
		if h.contextHint != "" {
			hintPos = strings.Index(newContent, h.contextHint)
		}
		if hintPos != -1 {
			windowStart := runeOffsetBack(newContent, hintPos, 500)
			windowEnd := runeOffsetFwd(newContent, hintPos, 2000)
			windowNew, wCount, wStrategy, wErr := fuzzyFindAndReplace(newContent[windowStart:windowEnd], searchPattern, replacement, false, &notes, v4aProbe(fileOps))
			errText = wErr
			if wCount > 0 {
				strategies = append(strategies, wStrategy)
				newContent = newContent[:windowStart] + windowNew + newContent[windowEnd:]
				errText = ""
			}
		}
		if errText != "" {
			if isAlreadyApplied(newContent, searchPattern, replacement) {
				continue
			}
			return v4aFail("Could not apply hunk: " + errText + v4aNoMatchHint(errText, searchPattern, newContent))
		}
	}
	writeResult := fileOps.WriteFile(op.filePath, newContent, &currentContent)
	result := v4aWritten(writeResult, v4aUnifiedDiff(op.filePath, currentContent, &newContent), op.filePath, readResult.ContentSha256)
	if !result.ok {
		return result
	}
	if note := boundaryNote(strategies, "the hunk's", " in "+op.filePath); note != "" {
		notes = append(notes, note)
	}
	result.notes = notes
	return result
}

// Handler, error verb and files bucket for each operation kind
var v4aApplyDispatch = map[string]struct {
	handler func(*v4aOperation, v4aFileOps) v4aApplyResult
	verb    string
	bucket  string
}{
	v4aAdd:    {applyV4AAdd, "add", "created"},
	v4aDelete: {applyV4ADelete, "delete", "deleted"},
	v4aMove:   {applyV4AMove, "move", "modified"},
	v4aUpdate: {applyV4AUpdate, "update", "modified"},
}

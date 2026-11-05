package tools

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Byte span of one match in the original content
type matchSpan struct{ start, end int }

const identicalStringsError = "No edit was applied because old_string and new_string are identical. " +
	"Provide the existing text to replace in old_string and the changed " +
	"replacement text in new_string."

// Typographic characters and the ASCII each normalizes to
var unicodeMap = map[rune]string{
	'\u201c': `"`, '\u201d': `"`,
	'\u2018': "'", '\u2019': "'",
	'\u2014': "--", '\u2013': "-",
	'\u2026': "...", '\u00a0': " ",
	'\u2212': "-",
	'\u2000': " ", '\u2001': " ", '\u2002': " ", '\u2003': " ",
	'\u2004': " ", '\u2005': " ", '\u2006': " ", '\u2007': " ",
	'\u2008': " ", '\u2009': " ", '\u200a': " ", '\u202f': " ",
	'\u205f': " ", '\u3000': " ",
}

// Maps typographic Unicode variants to their ASCII equivalents
func unicodeNormalize(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if repl, ok := unicodeMap[r]; ok {
			b.WriteString(repl)
		} else {
			b.WriteString(text[i : i+size])
		}
		i += size
	}
	return b.String()
}

// Byte offset prefix sums where entry k covers the first k lines and their newlines
func linePrefixSums(lines []string) []int {
	prefix := make([]int, len(lines)+1)
	for i, l := range lines {
		prefix[i+1] = prefix[i] + len(l) + 1
	}
	return prefix
}

// Spans of every n line window starting at an accepted line index
func windowSpans(content string, contentLines []string, n int, accept func(i int) bool) []matchSpan {
	var out []matchSpan
	prefix := linePrefixSums(contentLines)
	for i := 0; i < len(contentLines)-n+1; i++ {
		if accept(i) {
			out = append(out, matchSpan{prefix[i], min(len(content), prefix[i+n]-1)})
		}
	}
	return out
}

// Matches pattern lines against content windows after transforming each block
func matchTransformedLines(content, pattern string, transform func([]string) []string) []matchSpan {
	contentLines := strings.Split(content, "\n")
	patternNorm := transform(strings.Split(pattern, "\n"))
	n := len(patternNorm)
	return windowSpans(content, contentLines, n, func(i int) bool {
		return stringSlicesEqual(transform(contentLines[i:i+n]), patternNorm)
	})
}

func mapLines(lines []string, f func(string) string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = f(l)
	}
	return out
}

// Strips whitespace on the first and last lines only
func stripBoundary(lines []string) []string {
	out := append([]string(nil), lines...)
	out[0] = pyStrip(out[0])
	if len(out) > 1 {
		out[len(out)-1] = pyStrip(out[len(out)-1])
	}
	return out
}

// Per character offsets of the original and of its unicode normalized form, with a sentinel
type normMap struct {
	origOff []int
	norm    []int
}

// Maps each original character to its normalized position counted in bytes or in runes
func buildOrigToNormMap(original string, normInRunes bool) normMap {
	var m normMap
	pos := 0
	for i := 0; i < len(original); {
		r, size := wtf8DecodeRune(original[i:])
		m.origOff = append(m.origOff, i)
		m.norm = append(m.norm, pos)
		if repl, ok := unicodeMap[r]; ok {
			pos += len(repl)
		} else if normInRunes {
			pos++
		} else {
			pos += size
		}
		i += size
	}
	m.origOff = append(m.origOff, len(original))
	m.norm = append(m.norm, pos)
	return m
}

// Walks from an original character index until the mapped position reaches normEnd
func normEndToOrig(m normMap, origStart, normEnd int) int {
	origLen := len(m.norm) - 1
	origEnd := origStart
	for origEnd < origLen && m.norm[origEnd] < normEnd {
		origEnd++
	}
	return origEnd
}

// Converts spans in the normalized string to byte spans in the original
func mapPositionsNormToOrig(m normMap, normMatches []matchSpan) []matchSpan {
	firstOrig := map[int]int{}
	for k := 0; k < len(m.norm)-1; k++ {
		if _, ok := firstOrig[m.norm[k]]; !ok {
			firstOrig[m.norm[k]] = k
		}
	}
	var out []matchSpan
	for _, s := range normMatches {
		if k, ok := firstOrig[s.start]; ok {
			out = append(out, matchSpan{m.origOff[k], m.origOff[normEndToOrig(m, k, s.end)]})
		}
	}
	return out
}

func isSpaceOrTab(c byte) bool { return c == ' ' || c == '\t' }

// Maps spans found after collapsing space and tab runs back onto the original
func mapNormalizedPositions(original, normalized string, normalizedMatches []matchSpan) []matchSpan {
	origToNorm := make([]int, 0, len(original))
	oi, ni := 0, 0
	for oi < len(original) && ni < len(normalized) {
		switch {
		case original[oi] == normalized[ni]:
			origToNorm = append(origToNorm, ni)
			oi++
			ni++
		case isSpaceOrTab(original[oi]) && normalized[ni] == ' ':
			origToNorm = append(origToNorm, ni)
			oi++
			if oi < len(original) && !isSpaceOrTab(original[oi]) {
				ni++
			}
		default:
			origToNorm = append(origToNorm, ni)
			oi++
		}
	}
	for ; oi < len(original); oi++ {
		origToNorm = append(origToNorm, len(normalized))
	}
	startOf, endOf := map[int]int{}, map[int]int{}
	for pos, n := range origToNorm {
		if _, ok := startOf[n]; !ok {
			startOf[n] = pos
		}
		endOf[n] = pos
	}
	var out []matchSpan
	for _, s := range normalizedMatches {
		origStart, ok := startOf[s.start]
		if !ok {
			origStart = -1
			for i, n := range origToNorm {
				if n >= s.start {
					origStart = i
					break
				}
			}
			if origStart < 0 {
				panic("min() arg is an empty sequence")
			}
		}
		var origEnd int
		if e, ok := endOf[s.end-1]; ok {
			origEnd = e + 1
		} else {
			origEnd = origStart + (s.end - s.start)
		}
		if s.end < len(normalized) && normalized[s.end-1] == ' ' {
			for origEnd < len(original) && isSpaceOrTab(original[origEnd]) {
				origEnd++
			}
		}
		out = append(out, matchSpan{origStart, min(origEnd, len(original))})
	}
	return out
}

// Exact non overlapping occurrences with str.replace semantics
func strategyExact(content, pattern string) []matchSpan {
	var out []matchSpan
	if pattern == "" {
		for i := 0; i <= len(content); {
			out = append(out, matchSpan{i, i})
			if i == len(content) {
				break
			}
			_, size := wtf8DecodeRune(content[i:])
			i += size
		}
		return out
	}
	for at := 0; ; {
		k := strings.Index(content[at:], pattern)
		if k < 0 {
			return out
		}
		out = append(out, matchSpan{at + k, at + k + len(pattern)})
		at += k + len(pattern)
	}
}

// Compares lines after stripping each one
func strategyLineTrimmed(content, pattern string) []matchSpan {
	return matchTransformedLines(content, pattern, func(ls []string) []string { return mapLines(ls, pyStrip) })
}

// Collapses runs of spaces and tabs to one space
func collapseSpaceTabRuns(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if isSpaceOrTab(s[i]) {
			for i+1 < len(s) && isSpaceOrTab(s[i+1]) {
				i++
			}
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// Matches after collapsing space and tab runs on both sides
func strategyWhitespaceNormalized(content, pattern string) []matchSpan {
	contentNormalized := collapseSpaceTabRuns(content)
	matches := strategyExact(contentNormalized, collapseSpaceTabRuns(pattern))
	if len(matches) == 0 {
		return nil
	}
	return mapNormalizedPositions(content, contentNormalized, matches)
}

// Ignores leading indentation entirely
func strategyIndentationFlexible(content, pattern string) []matchSpan {
	return matchTransformedLines(content, pattern, func(ls []string) []string {
		return mapLines(ls, func(l string) string { return strings.TrimLeftFunc(l, pyIsSpace) })
	})
}

// Treats literal backslash n, t and r in the pattern as control characters
func strategyEscapeNormalized(content, pattern string) []matchSpan {
	unescaped := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(pattern, `\n`, "\n"), `\t`, "\t"), `\r`, "\r")
	if unescaped == pattern {
		return nil
	}
	return strategyExact(content, unescaped)
}

// Strips whitespace on the first and last pattern lines only
func strategyTrimmedBoundary(content, pattern string) []matchSpan {
	return matchTransformedLines(content, pattern, stripBoundary)
}

// Exact or line trimmed match after Unicode to ASCII normalization of both sides
func strategyUnicodeNormalized(content, pattern string) []matchSpan {
	normPattern := unicodeNormalize(pattern)
	normContent := unicodeNormalize(content)
	if normContent == content && normPattern == pattern {
		return nil
	}
	normMatches := strategyExact(normContent, normPattern)
	if len(normMatches) == 0 {
		normMatches = strategyLineTrimmed(normContent, normPattern)
	}
	if len(normMatches) == 0 {
		return nil
	}
	return mapPositionsNormToOrig(buildOrigToNormMap(content, false), normMatches)
}

// Anchors on the first and last lines and similarity scores the middle
func strategyBlockAnchor(content, pattern string) []matchSpan {
	patternLines := strings.Split(unicodeNormalize(pattern), "\n")
	if len(patternLines) < 2 {
		return nil
	}
	firstLine := pyStrip(patternLines[0])
	lastLine := pyStrip(patternLines[len(patternLines)-1])
	n := len(patternLines)
	normContentLines := strings.Split(unicodeNormalize(content), "\n")
	potential := map[int]bool{}
	for i := 0; i < len(normContentLines)-n+1; i++ {
		if pyStrip(normContentLines[i]) == firstLine && pyStrip(normContentLines[i+n-1]) == lastLine {
			potential[i] = true
		}
	}
	threshold := 0.70
	if len(potential) == 1 {
		threshold = 0.50
	}
	patternMiddle := strings.Join(patternLines[1:n-1], "\n")
	return windowSpans(content, strings.Split(content, "\n"), n, func(i int) bool {
		if !potential[i] {
			return false
		}
		if n <= 2 {
			return true
		}
		contentMiddle := strings.Join(normContentLines[i+1:i+n-1], "\n")
		return stringRatio(contentMiddle, patternMiddle) >= threshold
	})
}

// Anchored per line similarity where every non blank line scores at least 0.80
func strategyContextAware(content, pattern string) []matchSpan {
	patternLines := strings.Split(pattern, "\n")
	contentLines := strings.Split(content, "\n")
	n := len(patternLines)
	if n > len(contentLines) {
		return nil
	}
	firstPat := pyStrip(patternLines[0])
	lastPat := pyStrip(patternLines[n-1])
	sim := func(a, b string) float64 {
		if a == b {
			return 1.0
		}
		return stringRatio(a, b)
	}
	return windowSpans(content, contentLines, n, func(i int) bool {
		block := contentLines[i : i+n]
		if sim(firstPat, pyStrip(block[0])) < 0.80 {
			return false
		}
		if sim(lastPat, pyStrip(block[n-1])) < 0.80 {
			return false
		}
		for k, pLine := range patternLines {
			p := pyStrip(pLine)
			if p != "" && sim(p, pyStrip(block[k])) < 0.80 {
				return false
			}
		}
		return true
	})
}

// Ordered chain from precise strategies to similarity based ones
var fuzzyStrategies = []struct {
	name string
	fn   func(content, pattern string) []matchSpan
}{
	{"exact", strategyExact},
	{"line_trimmed", strategyLineTrimmed},
	{"whitespace_normalized", strategyWhitespaceNormalized},
	{"indentation_flexible", strategyIndentationFlexible},
	{"escape_normalized", strategyEscapeNormalized},
	{"trimmed_boundary", strategyTrimmedBoundary},
	{"unicode_normalized", strategyUnicodeNormalized},
	{"block_anchor", strategyBlockAnchor},
	{"context_aware", strategyContextAware},
}

// Strategies whose matches only approximately resemble old_string
var similarityStrategies = setOf("block_anchor", "context_aware")

// Note for an edit whose span a similarity strategy located, empty when every strategy was exact
func boundaryNote(strategies []string, target, where string) string {
	var used []string
	for _, s := range strategies {
		if similarityStrategies[s] {
			used = append(used, s)
		}
	}
	used = dedupeOrdered(used)
	if len(used) == 0 {
		return ""
	}
	return "Non-exact match (strategy: " + strings.Join(used, ", ") + ")" + where + ". The edit span was located " +
		"approximately, not matched to " + target + " literal boundaries — review the diff below to " +
		"confirm the change landed exactly where intended (a shifted boundary can duplicate or " +
		"drop a line just outside the edited region)."
}

// Reports a re sent edit whose non trivial new text is present and old text gone
func isAlreadyApplied(content, oldString, newString string) bool {
	if newString == "" || wtf8RuneCount(pyStrip(newString)) < 8 || !strings.Contains(content, newString) {
		return false
	}
	return oldString == newString || !strings.Contains(content, oldString)
}

func matchedRegions(content string, matches []matchSpan) string {
	var b strings.Builder
	for _, m := range matches {
		b.WriteString(content[m.start:m.end])
	}
	return b.String()
}

// Renders up to cap match positions as line number and snippet rows
func formatMatchLocations(content string, matches []matchSpan, limit int) string {
	var rows []string
	for idx, m := range matches {
		if idx >= limit {
			break
		}
		lineNo := strings.Count(content[:m.start], "\n") + 1
		lineStart := strings.LastIndexByte(content[:m.start], '\n') + 1
		lineEnd := strings.IndexByte(content[lineStart:], '\n')
		if lineEnd == -1 {
			lineEnd = len(content)
		} else {
			lineEnd += lineStart
		}
		snippet := pyStrip(content[lineStart:lineEnd])
		if wtf8RuneCount(snippet) > 80 {
			snippet = runePrefix(snippet, 77) + "..."
		}
		rows = append(rows, fmt.Sprintf("  L%d: %s", lineNo, snippet))
	}
	if extra := len(matches) - limit; extra > 0 {
		rows = append(rows, fmt.Sprintf("  ... and %d more", extra))
	}
	return strings.Join(rows, "\n")
}

// Finds and replaces through the strategy chain, returning new content, count, strategy and error
func fuzzyFindAndReplace(content, oldString, newString string, replaceAll bool, correctionNotes *[]string, probe *editProbe) (string, int, string, string) {
	if oldString == "" {
		return content, 0, "", "old_string is empty — nothing to match. Set old_string to the exact " +
			"existing text the replacement should replace (read the file first if " +
			"unsure). To create a new file or fully rewrite one, use write_file " +
			"instead. Do not re-send this call unchanged."
	}
	if pyStrip(oldString) == "" {
		return content, 0, "", "old_string is only whitespace — provide non-blank text to match. Set it " +
			"to the exact existing text the replacement should replace (read the file " +
			"first if unsure). Do not re-send this call unchanged."
	}
	oldString, newString, correctionNote, err := maybeCorrectEscapeDrift(oldString, newString, content)
	if err != nil {
		return content, 0, "", err.Error()
	}
	if oldString == newString {
		return content, 0, "", identicalStringsError
	}
	for _, st := range fuzzyStrategies {
		matches := st.fn(content, oldString)
		if len(matches) == 0 {
			continue
		}
		if len(matches) > 1 && !replaceAll {
			probe.noteEditMatch("", "ambiguous")
			return content, 0, "", fmt.Sprintf("Found %d matches for old_string. "+
				"Provide more context to make it unique, or use replace_all=True. "+
				"Matches:\n%s", len(matches), formatMatchLocations(content, matches, 5))
		}
		if replaceAll && len(matches) > 1 && similarityStrategies[st.name] {
			probe.noteEditMatch("", "ambiguous")
			return content, 0, "", fmt.Sprintf("Found %d approximate matches via the "+
				"'%s' strategy; replace_all only applies to exact "+
				"matches. Provide the precise text (whitespace included) so an "+
				"exact/line-trimmed match can be made.", len(matches), st.name)
		}
		if st.name != "exact" {
			if driftErr := detectEscapeDrift(content, matches, oldString, newString); driftErr != "" {
				return content, 0, "", driftErr
			}
		}
		effectiveNew := maybeUnescapeNewString(newString, content, matches)
		var plan *unicodeEditPlan
		if st.name == "unicode_normalized" {
			plan = newUnicodeEditPlan(oldString, effectiveNew)
		}
		newContent := applyReplacements(content, matches, effectiveNew, oldString, st.name != "exact", plan)
		probe.noteEditMatch(st.name, "")
		if correctionNote != "" && correctionNotes != nil {
			*correctionNotes = append(*correctionNotes, correctionNote)
		}
		return newContent, len(matches), st.name, ""
	}
	probe.noteEditMatch("", "no_match")
	return content, 0, "", "Could not find a match for old_string in the file"
}

// Error when a literal backslash n in the arguments stands in for a real line break in the file
func detectNewlineLiteralDrift(content string, matches []matchSpan, oldString, newString string) string {
	if !strings.Contains(newString, `\n`) {
		return ""
	}
	oldLiterals := strings.Count(oldString, `\n`)
	for _, m := range matches {
		region := content[m.start:m.end]
		if strings.Contains(region, "\n") && oldLiterals > strings.Count(region, `\n`) {
			return "Escape-drift detected: old_string contains more literal " +
				`'\\n' sequences than the matched region of the file, which ` +
				"has real line breaks there instead. This is almost always a " +
				"tool-call serialization artifact where a line break got escaped " +
				"one extra time; new_string would write it as backslash + n. " +
				"Re-read the file with read_file and pass old_string/new_string " +
				`with actual line breaks, keeping only the '\\n' sequences ` +
				"that appear literally in the file."
		}
	}
	return ""
}

// Error when new_string carries quote escapes, doubled backslash runs or literal newline drift
func detectEscapeDrift(content string, matches []matchSpan, oldString, newString string) string {
	hasQuoteSuspects := strings.Contains(newString, `\'`) || strings.Contains(newString, `\"`)
	if !hasQuoteSuspects && !strings.Contains(oldString, `\`) {
		return ""
	}
	regions := matchedRegions(content, matches)
	if hasQuoteSuspects {
		for _, suspect := range []string{`\'`, `\"`} {
			if strings.Contains(newString, suspect) && strings.Contains(oldString, suspect) && !strings.Contains(regions, suspect) {
				plain := suspect[1:]
				return "Escape-drift detected: old_string and new_string contain " +
					"the literal sequence " + pyReprStr(suspect) + " but the matched region of " +
					"the file does not. This is almost always a tool-call " +
					"serialization artifact where an apostrophe or quote got " +
					"prefixed with a spurious backslash. Re-read the file with " +
					"read_file and pass old_string/new_string without " +
					"backslash-escaping " + pyReprStr(plain) + " characters."
			}
		}
	}
	if drift := detectNewlineLiteralDrift(content, matches, oldString, newString); drift != "" {
		return drift
	}
	return detectBackslashDoubling(regions, oldString, newString)
}

func intSlicesEqual(a, b []int) bool {
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

// Error when every backslash run in old_string is exactly twice the file's run
func detectBackslashDoubling(regions, oldString, newString string) string {
	oldRuns := backslashRuns(oldString)
	fileRuns := backslashRuns(regions)
	if len(oldRuns) == 0 || len(fileRuns) == 0 || len(oldRuns) != len(fileRuns) || intSlicesEqual(oldRuns, fileRuns) {
		return ""
	}
	strong := len(fileRuns) >= 2
	for i, f := range fileRuns {
		if oldRuns[i] != f*2 {
			return ""
		}
		if f >= 2 {
			strong = true
		}
	}
	if !strong || intSlicesEqual(backslashRuns(newString), fileRuns) {
		return ""
	}
	return "Escape-drift detected: every backslash run in old_string is exactly " +
		"twice as long as in the matched region of the file (e.g. the file " +
		"has `\\\\` where old_string has `\\\\\\\\`). The tool-call arguments " +
		"were JSON-escaped one extra time; applying new_string verbatim would " +
		"double every backslash in the file. Re-read the file with read_file " +
		"and resend old_string/new_string with the backslash counts exactly " +
		"as they appear in the file."
}

// Converts literal backslash t and r in new_string when the matched region holds the real control character
func maybeUnescapeNewString(newString, content string, matches []matchSpan) string {
	if !strings.Contains(newString, `\t`) && !strings.Contains(newString, `\r`) {
		return newString
	}
	regions := matchedRegions(content, matches)
	for _, pair := range [][2]string{{`\t`, "\t"}, {`\r`, "\r"}} {
		if strings.Contains(newString, pair[0]) && strings.Contains(regions, pair[1]) {
			newString = strings.ReplaceAll(newString, pair[0], pair[1])
		}
	}
	return newString
}

func leadingWhitespace(line string) string {
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
}

// First line holding non whitespace text
func firstMeaningfulLine(text string) (string, bool) {
	for _, line := range strings.Split(text, "\n") {
		if pyStrip(line) != "" {
			return line, true
		}
	}
	return "", false
}

// Reports whether text outside the match precedes its first non blank line
func matchStartsMidLine(content string, start, end int) bool {
	firstLineStart := start
	for _, line := range strings.Split(content[start:end], "\n") {
		if pyStrip(line) != "" {
			lineStart := strings.LastIndexByte(content[:firstLineStart], '\n') + 1
			return strings.Trim(content[lineStart:firstLineStart], " \t\r") != ""
		}
		firstLineStart += len(line) + 1
	}
	return false
}

// Re anchors the replacement indentation onto the file base indent after a non exact match
func reindentReplacement(fileRegion, oldString, newString string, midLine bool) string {
	if newString == "" || midLine {
		return newString
	}
	oldFirst, ok1 := firstMeaningfulLine(oldString)
	fileFirst, ok2 := firstMeaningfulLine(fileRegion)
	if !ok1 || !ok2 {
		return newString
	}
	oldIndent := leadingWhitespace(oldFirst)
	fileIndent := leadingWhitespace(fileFirst)
	if oldIndent == fileIndent {
		return newString
	}
	lines := strings.Split(newString, "\n")
	out := make([]string, len(lines))
	for i, line := range lines {
		switch {
		case pyStrip(line) == "":
			out[i] = line
		case strings.HasPrefix(leadingWhitespace(line), oldIndent):
			out[i] = fileIndent + line[len(oldIndent):]
		default:
			out[i] = fileIndent + strings.TrimLeft(line, " \t")
		}
	}
	return strings.Join(out, "\n")
}

// Normalized old_string and its character opcodes toward new_string, shared across matches
type unicodeEditPlan struct {
	normOld string
	opcodes []diffOpcode
}

func newUnicodeEditPlan(oldString, newString string) *unicodeEditPlan {
	normOld := unicodeNormalize(oldString)
	return &unicodeEditPlan{normOld, newStringMatcher(normOld, newString).getOpcodes()}
}

// Byte offset of every character start in s plus the end sentinel
func runeOffsets(s string) []int {
	offs := make([]int, 0, len(s)+1)
	for i := 0; i < len(s); {
		offs = append(offs, i)
		_, size := wtf8DecodeRune(s[i:])
		i += size
	}
	return append(offs, len(s))
}

// Applies only the old to new edits onto the region so its typography survives
func preserveUnicodeInReplacement(fileRegion, newString string, plan *unicodeEditPlan) string {
	if plan.normOld != unicodeNormalize(fileRegion) {
		return newString
	}
	m := buildOrigToNormMap(fileRegion, true)
	newOffs := runeOffsets(newString)
	var b strings.Builder
	for _, op := range plan.opcodes {
		switch op.tag {
		case "equal":
			origStart := sort.Search(len(m.norm), func(k int) bool { return m.norm[k] > op.i1 }) - 1
			origEnd := normEndToOrig(m, origStart, op.i2)
			b.WriteString(fileRegion[m.origOff[origStart]:m.origOff[origEnd]])
		case "delete":
		default:
			b.WriteString(newString[newOffs[op.j1]:newOffs[op.j2]])
		}
	}
	return b.String()
}

// Splices the replacement over each span from the end, reindenting and keeping typography when asked
func applyReplacements(content string, matches []matchSpan, newString, oldString string, reindent bool, plan *unicodeEditPlan) string {
	ordered := append([]matchSpan(nil), matches...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].start > ordered[j].start })
	result := content
	for _, m := range ordered {
		adjusted := newString
		if reindent {
			region := content[m.start:m.end]
			if plan != nil {
				adjusted = preserveUnicodeInReplacement(region, adjusted, plan)
			}
			adjusted = reindentReplacement(region, oldString, adjusted, matchStartsMidLine(content, m.start, m.end))
		}
		result = result[:m.start] + adjusted + result[m.end:]
	}
	return result
}

// Renders the leading whitespace run with an arrow per tab and a middle dot per space
func visualizeWhitespace(line string) string {
	stripped := strings.TrimLeft(line, " \t")
	prefix := line[:len(line)-len(stripped)]
	return strings.ReplaceAll(strings.ReplaceAll(prefix, "\t", "→"), " ", "·") + stripped
}

// Python str.splitlines without line terminators
func pySplitLines(s string) []string {
	lines := pySplitLinesKeepEnds(s)
	for i, l := range lines {
		switch {
		case strings.HasSuffix(l, "\r\n"):
			lines[i] = l[:len(l)-2]
		default:
			r, size := utf8.DecodeLastRuneInString(l)
			switch r {
			case '\n', '\r', '\v', '\f', '\x1c', '\x1d', '\x1e', '\x85', '\u2028', '\u2029':
				lines[i] = l[:len(l)-size]
			}
		}
	}
	return lines
}

// Numbered snippets of the lines most similar to the old_string anchor line
func findClosestLines(oldString, content string, contextLines, maxResults int) string {
	if oldString == "" || content == "" {
		return ""
	}
	oldLines := pySplitLines(oldString)
	contentLines := pySplitLines(content)
	if len(oldLines) == 0 || len(contentLines) == 0 {
		return ""
	}
	anchor := pyStrip(oldLines[0])
	if anchor == "" {
		for _, l := range oldLines {
			if s := pyStrip(l); s != "" {
				anchor = s
				break
			}
		}
	}
	if anchor == "" {
		return ""
	}
	type scoredLine struct {
		ratio float64
		idx   int
	}
	var scored []scoredLine
	for i, line := range contentLines {
		if s := pyStrip(line); s != "" {
			scored = append(scored, scoredLine{stringRatio(anchor, s), i})
		}
	}
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].ratio > scored[j].ratio })
	var top []scoredLine
	for _, s := range scored {
		if s.ratio > 0.3 {
			top = append(top, s)
		}
	}
	if len(top) > maxResults {
		top = top[:maxResults]
	}
	if len(top) == 0 {
		return ""
	}
	var parts []string
	seen := map[[2]int]bool{}
	for _, t := range top {
		start := max(0, t.idx-contextLines)
		end := min(len(contentLines), t.idx+len(oldLines)+contextLines)
		if seen[[2]int{start, end}] {
			continue
		}
		seen[[2]int{start, end}] = true
		rows := make([]string, end-start)
		for j := range rows {
			rows[j] = fmt.Sprintf("%4d| %s", start+j+1, contentLines[start+j])
		}
		parts = append(parts, strings.Join(rows, "\n"))
	}
	result := strings.Join(parts, "\n---\n")
	bestLine := contentLines[top[0].idx]
	if pyStrip(bestLine) == anchor && bestLine != oldLines[0] {
		result += "\n\nWhitespace difference detected (→ = tab, · = space):\n" +
			"  file has: " + visualizeWhitespace(bestLine) + "\n" +
			"  you sent: " + visualizeWhitespace(oldLines[0]) + "\n" +
			"Use the exact whitespace shown in 'file has'."
	}
	return result
}

// Did you mean snippet for plain no match errors only, empty otherwise
func formatNoMatchHint(errText string, matchCount int, oldString, content string) string {
	if matchCount != 0 || errText == "" || !strings.HasPrefix(errText, "Could not find") {
		return ""
	}
	hint := findClosestLines(oldString, content, 2, 3)
	if hint == "" {
		return ""
	}
	return "\n\nDid you mean one of these sections?\n" + hint
}

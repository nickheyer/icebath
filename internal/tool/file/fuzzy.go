package file

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

const identicalStringsError = "No edit was applied because old_string and new_string are identical. " +
	"Provide the existing text to replace in old_string and the changed " +
	"replacement text in new_string."

const (
	strategyExact             = "exact"
	strategyLineTrimmed       = "line_trimmed"
	strategyWhitespace        = "whitespace_normalized"
	strategyIndentation       = "indentation_flexible"
	strategyEscape            = "escape_normalized"
	strategyTrimmedBoundary   = "trimmed_boundary"
	strategyUnicode           = "unicode_normalized"
	strategyBlockAnchor       = "block_anchor"
	strategyContextAware      = "context_aware"
	missAmbiguous             = "ambiguous"
	missNoMatch               = "no_match"
	noMatchError              = "Could not find a match for old_string in the file"
	similarityFloor           = 0.80
	closestLineFloor          = 0.3
	alreadyAppliedMinimumSize = 8
)

type MatchError struct {
	Msg string
}

func (e *MatchError) Error() string {
	return e.Msg
}

type Replacement struct {
	Content  string
	Count    int
	Strategy string
	Note     string
}

type MatchObserver interface {
	EditMatched(strategy string)
	EditMissed(reason string)
}

type matchObserverKey struct{}

func WithMatchObserver(ctx context.Context, o MatchObserver) context.Context {
	return context.WithValue(ctx, matchObserverKey{}, o)
}

func noteMatch(ctx context.Context, strategy, miss string) {
	o, ok := ctx.Value(matchObserverKey{}).(MatchObserver)
	if !ok || o == nil {
		return
	}
	if strategy != "" {
		o.EditMatched(strategy)
		return
	}
	o.EditMissed(miss)
}

type span struct {
	start, end int
}

var unicodeFold = newUnicodeFold()

func newUnicodeFold() *strings.Replacer {
	folded := [...]rune{
		0x201c, 0x201d, 0x2018, 0x2019, 0x2014, 0x2013, 0x2026, 0x00a0, 0x2212,
		0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005, 0x2006, 0x2007,
		0x2008, 0x2009, 0x200a, 0x202f, 0x205f, 0x3000,
	}
	pairs := make([]string, 0, 2*len(folded))
	for _, r := range folded {
		repl, _ := unicodeReplacement(r)
		pairs = append(pairs, string(r), repl)
	}
	return strings.NewReplacer(pairs...)
}

func unicodeReplacement(r rune) (string, bool) {
	switch r {
	case 0x201c, 0x201d:
		return `"`, true
	case 0x2018, 0x2019:
		return "'", true
	case 0x2014:
		return "--", true
	case 0x2013, 0x2212:
		return "-", true
	case 0x2026:
		return "...", true
	case 0x00a0, 0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005, 0x2006, 0x2007,
		0x2008, 0x2009, 0x200a, 0x202f, 0x205f, 0x3000:
		return " ", true
	}
	return "", false
}

func unicodeNormalize(text string) string {
	return unicodeFold.Replace(text)
}

func lineOffsets(lines []string) []int {
	offsets := make([]int, len(lines)+1)
	for i, line := range lines {
		offsets[i+1] = offsets[i] + len(line) + 1
	}
	return offsets
}

func windowSpans(content string, lines []string, n int, accept func(i int) bool) []span {
	var out []span
	offsets := lineOffsets(lines)
	for i := 0; i < len(lines)-n+1; i++ {
		if accept(i) {
			out = append(out, span{offsets[i], min(len(content), offsets[i+n]-1)})
		}
	}
	return out
}

func mapEach(lines []string, f func(string) string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = f(line)
	}
	return out
}

func matchMappedLines(content, pattern string, f func(string) string) []span {
	lines := strings.Split(content, "\n")
	want := mapEach(strings.Split(pattern, "\n"), f)
	have := mapEach(lines, f)
	n := len(want)
	return windowSpans(content, lines, n, func(i int) bool {
		for k := 0; k < n; k++ {
			if have[i+k] != want[k] {
				return false
			}
		}
		return true
	})
}

func exactSpans(content, pattern string) []span {
	var out []span
	for at := 0; pattern != "" && at <= len(content); {
		idx := strings.Index(content[at:], pattern)
		if idx < 0 {
			break
		}
		out = append(out, span{at + idx, at + idx + len(pattern)})
		at += idx + len(pattern)
	}
	return out
}

func lineTrimmedSpans(content, pattern string) []span {
	return matchMappedLines(content, pattern, trimSpace)
}

func collapseBlanks(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inRun := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' {
			if !inRun {
				b.WriteByte(' ')
				inRun = true
			}
			continue
		}
		inRun = false
		b.WriteByte(c)
	}
	return b.String()
}

func isBlankByte(c byte) bool {
	return c == ' ' || c == '\t'
}

func mapCollapsedSpans(original, normalized string, matches []span) []span {
	origToNorm := make([]int, 0, len(original))
	oi, ni := 0, 0
	for oi < len(original) && ni < len(normalized) {
		switch {
		case original[oi] == normalized[ni]:
			origToNorm = append(origToNorm, ni)
			oi++
			ni++
		case isBlankByte(original[oi]) && normalized[ni] == ' ':
			origToNorm = append(origToNorm, ni)
			oi++
			if oi < len(original) && !isBlankByte(original[oi]) {
				ni++
			}
		default:
			origToNorm = append(origToNorm, ni)
			oi++
		}
	}
	for len(origToNorm) < len(original) {
		origToNorm = append(origToNorm, len(normalized))
	}
	first := make([]int, len(normalized)+1)
	last := make([]int, len(normalized)+1)
	for i := range first {
		first[i], last[i] = -1, -1
	}
	for origPos, normPos := range origToNorm {
		if first[normPos] < 0 {
			first[normPos] = origPos
		}
		last[normPos] = origPos
	}
	out := make([]span, 0, len(matches))
	for _, m := range matches {
		origStart := first[m.start]
		if origStart < 0 {
			origStart = len(original)
			for i, n := range origToNorm {
				if n >= m.start {
					origStart = i
					break
				}
			}
		}
		origEnd := origStart + (m.end - m.start)
		if m.end >= 1 && last[m.end-1] >= 0 {
			origEnd = last[m.end-1] + 1
		}
		if m.end < len(normalized) && normalized[m.end-1] == ' ' {
			for origEnd < len(original) && isBlankByte(original[origEnd]) {
				origEnd++
			}
		}
		out = append(out, span{origStart, min(origEnd, len(original))})
	}
	return out
}

func whitespaceSpans(content, pattern string) []span {
	normalized := collapseBlanks(content)
	matches := exactSpans(normalized, collapseBlanks(pattern))
	if len(matches) == 0 {
		return nil
	}
	return mapCollapsedSpans(content, normalized, matches)
}

func indentationSpans(content, pattern string) []span {
	return matchMappedLines(content, pattern, trimLeftSpace)
}

func escapeSpans(content, pattern string) []span {
	unescaped := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(pattern, `\n`, "\n"), `\t`, "\t"), `\r`, "\r")
	if unescaped == pattern {
		return nil
	}
	return exactSpans(content, unescaped)
}

func trimmedBoundarySpans(content, pattern string) []span {
	lines := strings.Split(content, "\n")
	want := strings.Split(pattern, "\n")
	n := len(want)
	want[0] = trimSpace(want[0])
	if n > 1 {
		want[n-1] = trimSpace(want[n-1])
	}
	return windowSpans(content, lines, n, func(i int) bool {
		if trimSpace(lines[i]) != want[0] {
			return false
		}
		if n == 1 {
			return true
		}
		if trimSpace(lines[i+n-1]) != want[n-1] {
			return false
		}
		for k := 1; k < n-1; k++ {
			if lines[i+k] != want[k] {
				return false
			}
		}
		return true
	})
}

type normEntry struct {
	orig, norm int
}

func normEntries(original string) []normEntry {
	out := make([]normEntry, 0, len(original)+1)
	norm := 0
	for i := 0; i < len(original); {
		r, size := utf8.DecodeRuneInString(original[i:])
		out = append(out, normEntry{i, norm})
		if repl, ok := unicodeReplacement(r); ok {
			norm += len(repl)
		} else {
			norm += size
		}
		i += size
	}
	return append(out, normEntry{len(original), norm})
}

func mapFoldedSpans(entries []normEntry, matches []span) []span {
	last := len(entries) - 1
	first := make(map[int]int, last)
	for idx, e := range entries[:last] {
		if _, seen := first[e.norm]; !seen {
			first[e.norm] = idx
		}
	}
	var out []span
	for _, m := range matches {
		idx, ok := first[m.start]
		if !ok {
			continue
		}
		end := idx
		for end < last && entries[end].norm < m.end {
			end++
		}
		out = append(out, span{entries[idx].orig, entries[end].orig})
	}
	return out
}

func unicodeSpans(content, pattern string) []span {
	normPattern := unicodeNormalize(pattern)
	normContent := unicodeNormalize(content)
	if normContent == content && normPattern == pattern {
		return nil
	}
	matches := exactSpans(normContent, normPattern)
	if len(matches) == 0 {
		matches = lineTrimmedSpans(normContent, normPattern)
	}
	if len(matches) == 0 {
		return nil
	}
	return mapFoldedSpans(normEntries(content), matches)
}

func blockAnchorSpans(content, pattern string) []span {
	patternLines := strings.Split(unicodeNormalize(pattern), "\n")
	n := len(patternLines)
	if n < 2 {
		return nil
	}
	firstLine := trimSpace(patternLines[0])
	lastLine := trimSpace(patternLines[n-1])
	normLines := strings.Split(unicodeNormalize(content), "\n")
	potential := make(map[int]struct{})
	for i := 0; i < len(normLines)-n+1; i++ {
		if trimSpace(normLines[i]) == firstLine && trimSpace(normLines[i+n-1]) == lastLine {
			potential[i] = struct{}{}
		}
	}
	threshold := 0.70
	if len(potential) == 1 {
		threshold = 0.50
	}
	patternMiddle := strings.Join(patternLines[1:n-1], "\n")
	return windowSpans(content, strings.Split(content, "\n"), n, func(i int) bool {
		if _, ok := potential[i]; !ok {
			return false
		}
		if n <= 2 {
			return true
		}
		contentMiddle := strings.Join(normLines[i+1:i+n-1], "\n")
		return similarity(contentMiddle, patternMiddle) >= threshold
	})
}

func contextAwareSpans(content, pattern string) []span {
	patternLines := strings.Split(pattern, "\n")
	lines := strings.Split(content, "\n")
	n := len(patternLines)
	if n > len(lines) {
		return nil
	}
	want := mapEach(patternLines, trimSpace)
	have := mapEach(lines, trimSpace)
	return windowSpans(content, lines, n, func(i int) bool {
		if !similarAtLeast(want[0], have[i], similarityFloor) {
			return false
		}
		if !similarAtLeast(want[n-1], have[i+n-1], similarityFloor) {
			return false
		}
		for k := 0; k < n; k++ {
			if want[k] != "" && !similarAtLeast(want[k], have[i+k], similarityFloor) {
				return false
			}
		}
		return true
	})
}

type strategy struct {
	name string
	find func(content, pattern string) []span
}

var strategies = [...]strategy{
	{strategyExact, exactSpans},
	{strategyLineTrimmed, lineTrimmedSpans},
	{strategyWhitespace, whitespaceSpans},
	{strategyIndentation, indentationSpans},
	{strategyEscape, escapeSpans},
	{strategyTrimmedBoundary, trimmedBoundarySpans},
	{strategyUnicode, unicodeSpans},
	{strategyBlockAnchor, blockAnchorSpans},
	{strategyContextAware, contextAwareSpans},
}

func isSimilarityStrategy(name string) bool {
	return name == strategyBlockAnchor || name == strategyContextAware
}

func boundaryNote(used []string, target, where string) string {
	var distinct []string
	seen := make(map[string]struct{})
	for _, s := range used {
		if !isSimilarityStrategy(s) {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		distinct = append(distinct, s)
	}
	if len(distinct) == 0 {
		return ""
	}
	return fmt.Sprintf("Non-exact match (strategy: %s)%s. The edit span was located "+
		"approximately, not matched to %s literal boundaries — review the diff below to "+
		"confirm the change landed exactly where intended (a shifted boundary can duplicate or "+
		"drop a line just outside the edited region).", strings.Join(distinct, ", "), where, target)
}

func alreadyApplied(content, oldString, newString string) bool {
	if newString == "" || charLen(trimSpace(newString)) < alreadyAppliedMinimumSize || !strings.Contains(content, newString) {
		return false
	}
	return oldString == newString || !strings.Contains(content, oldString)
}

func matchedRegions(content string, matches []span) string {
	var b strings.Builder
	for _, m := range matches {
		b.WriteString(content[m.start:m.end])
	}
	return b.String()
}

func matchLocations(content string, matches []span) string {
	const limit = 5
	var rows []string
	for i, m := range matches {
		if i == limit {
			break
		}
		lineNo := strings.Count(content[:m.start], "\n") + 1
		lineStart := strings.LastIndex(content[:m.start], "\n") + 1
		lineEnd := len(content)
		if idx := strings.Index(content[lineStart:], "\n"); idx >= 0 {
			lineEnd = lineStart + idx
		}
		snippet := trimSpace(content[lineStart:lineEnd])
		if charLen(snippet) > 80 {
			snippet = charPrefix(snippet, 77) + "..."
		}
		rows = append(rows, fmt.Sprintf("  L%d: %s", lineNo, snippet))
	}
	if extra := len(matches) - limit; extra > 0 {
		rows = append(rows, fmt.Sprintf("  ... and %d more", extra))
	}
	return strings.Join(rows, "\n")
}

func FuzzyReplace(ctx context.Context, content, oldString, newString string, replaceAll bool) (Replacement, error) {
	fail := func(msg string) (Replacement, error) {
		return Replacement{Content: content}, &MatchError{Msg: msg}
	}
	if oldString == "" {
		return fail("old_string is empty — nothing to match. Set old_string to the exact " +
			"existing text the replacement should replace (read the file first if " +
			"unsure). To create a new file or fully rewrite one, use write_file " +
			"instead. Do not re-send this call unchanged.")
	}
	if isBlank(oldString) {
		return fail("old_string is only whitespace — provide non-blank text to match. Set it " +
			"to the exact existing text the replacement should replace (read the file " +
			"first if unsure). Do not re-send this call unchanged.")
	}
	oldString, newString, correction, err := correctEscapeDrift(oldString, newString, content)
	if err != nil {
		return fail(err.Error())
	}
	if oldString == newString {
		return fail(identicalStringsError)
	}
	for _, s := range strategies {
		matches := s.find(content, oldString)
		if len(matches) == 0 {
			continue
		}
		if len(matches) > 1 && !replaceAll {
			noteMatch(ctx, "", missAmbiguous)
			return fail(fmt.Sprintf("Found %d matches for old_string. "+
				"Provide more context to make it unique, or use replace_all=True. "+
				"Matches:\n%s", len(matches), matchLocations(content, matches)))
		}
		if replaceAll && len(matches) > 1 && isSimilarityStrategy(s.name) {
			noteMatch(ctx, "", missAmbiguous)
			return fail(fmt.Sprintf("Found %d approximate matches via the "+
				"'%s' strategy; replace_all only applies to exact "+
				"matches. Provide the precise text (whitespace included) so an "+
				"exact/line-trimmed match can be made.", len(matches), s.name))
		}
		if s.name != strategyExact {
			if drift := detectEscapeDrift(content, matches, oldString, newString); drift != "" {
				return fail(drift)
			}
		}
		effective := unescapeNewString(newString, content, matches)
		edit := replacementPlan{text: effective}
		if s.name != strategyExact {
			edit.reindent, edit.old = true, oldString
		}
		if s.name == strategyUnicode {
			edit.unicode = newUnicodePlan(oldString, effective)
		}
		updated := applyReplacements(content, matches, edit)
		noteMatch(ctx, s.name, "")
		return Replacement{Content: updated, Count: len(matches), Strategy: s.name, Note: correction}, nil
	}
	noteMatch(ctx, "", missNoMatch)
	return fail(noMatchError)
}

func leadingBlanks(line string) string {
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
}

func firstMeaningfulLine(text string) (string, bool) {
	for _, line := range strings.Split(text, "\n") {
		if !isBlank(line) {
			return line, true
		}
	}
	return "", false
}

func matchStartsMidLine(content string, start, end int) bool {
	firstLineStart := start
	for _, line := range strings.Split(content[start:end], "\n") {
		if !isBlank(line) {
			lineStart := strings.LastIndex(content[:firstLineStart], "\n") + 1
			return strings.Trim(content[lineStart:firstLineStart], " \t\r") != ""
		}
		firstLineStart += len(line) + 1
	}
	return false
}

func reindentReplacement(region, oldString, newString string, midLine bool) string {
	if newString == "" || midLine {
		return newString
	}
	oldFirst, ok := firstMeaningfulLine(oldString)
	if !ok {
		return newString
	}
	fileFirst, ok := firstMeaningfulLine(region)
	if !ok {
		return newString
	}
	oldIndent := leadingBlanks(oldFirst)
	fileIndent := leadingBlanks(fileFirst)
	if oldIndent == fileIndent {
		return newString
	}
	lines := strings.Split(newString, "\n")
	for i, line := range lines {
		switch {
		case isBlank(line):
		case strings.HasPrefix(leadingBlanks(line), oldIndent):
			lines[i] = fileIndent + line[len(oldIndent):]
		default:
			lines[i] = fileIndent + strings.TrimLeft(line, " \t")
		}
	}
	return strings.Join(lines, "\n")
}

type unicodePlan struct {
	normOld string
	codes   []opcode
}

func newUnicodePlan(oldString, newString string) *unicodePlan {
	normOld := unicodeNormalize(oldString)
	return &unicodePlan{normOld: normOld, codes: newMatcher(decodeRunes(normOld), decodeRunes(newString)).opcodes()}
}

func preserveUnicode(region, newString string, plan *unicodePlan) string {
	if plan.normOld != unicodeNormalize(region) {
		return newString
	}
	regionRunes := decodeRunes(region)
	newRunes := decodeRunes(newString)
	toNorm := make([]int, 0, len(regionRunes)+1)
	pos := 0
	for _, r := range regionRunes {
		toNorm = append(toNorm, pos)
		if repl, ok := unicodeReplacement(r); ok {
			pos += len(repl)
		} else {
			pos++
		}
	}
	toNorm = append(toNorm, pos)
	last := len(toNorm) - 1
	var out []rune
	for _, c := range plan.codes {
		switch c.tag {
		case opEqual:
			origStart := sort.Search(len(toNorm), func(k int) bool { return toNorm[k] > c.i1 }) - 1
			origEnd := origStart
			for origEnd < last && toNorm[origEnd] < c.i2 {
				origEnd++
			}
			out = append(out, regionRunes[origStart:origEnd]...)
		case opDelete:
		default:
			out = append(out, newRunes[c.j1:c.j2]...)
		}
	}
	return encodeRunes(out)
}

type replacementPlan struct {
	text     string
	reindent bool
	old      string
	unicode  *unicodePlan
}

func applyReplacements(content string, matches []span, plan replacementPlan) string {
	ordered := append([]span(nil), matches...)
	sort.SliceStable(ordered, func(p, q int) bool { return ordered[p].start > ordered[q].start })
	result := content
	for _, m := range ordered {
		adjusted := plan.text
		if plan.reindent {
			region := content[m.start:m.end]
			if plan.unicode != nil {
				adjusted = preserveUnicode(region, adjusted, plan.unicode)
			}
			adjusted = reindentReplacement(region, plan.old, adjusted, matchStartsMidLine(content, m.start, m.end))
		}
		result = result[:m.start] + adjusted + result[m.end:]
	}
	return result
}

func visualizeWhitespace(line string) string {
	stripped := strings.TrimLeft(line, " \t")
	prefix := line[:len(line)-len(stripped)]
	return strings.ReplaceAll(strings.ReplaceAll(prefix, "\t", "→"), " ", "·") + stripped
}

func closestLines(oldString, content string) string {
	const contextLines, maxResults = 2, 3
	if oldString == "" || content == "" {
		return ""
	}
	oldLines := splitLines(oldString, false)
	contentLines := splitLines(content, false)
	if len(oldLines) == 0 || len(contentLines) == 0 {
		return ""
	}
	anchor := trimSpace(oldLines[0])
	if anchor == "" {
		for _, l := range oldLines {
			if s := trimSpace(l); s != "" {
				anchor = s
				break
			}
		}
	}
	if anchor == "" {
		return ""
	}
	type scoredLine struct {
		score float64
		index int
	}
	var scored []scoredLine
	anchorRunes := decodeRunes(anchor)
	for i, line := range contentLines {
		stripped := trimSpace(line)
		if stripped == "" {
			continue
		}
		scored = append(scored, scoredLine{newMatcher(anchorRunes, decodeRunes(stripped)).ratio(), i})
	}
	sort.SliceStable(scored, func(p, q int) bool { return scored[p].score > scored[q].score })
	var top []scoredLine
	for _, s := range scored {
		if s.score > closestLineFloor {
			top = append(top, s)
			if len(top) == maxResults {
				break
			}
		}
	}
	if len(top) == 0 {
		return ""
	}
	var parts []string
	seen := make(map[span]struct{})
	for _, s := range top {
		start := max(0, s.index-contextLines)
		end := min(len(contentLines), s.index+len(oldLines)+contextLines)
		if _, dup := seen[span{start, end}]; dup {
			continue
		}
		seen[span{start, end}] = struct{}{}
		rows := make([]string, 0, end-start)
		for j := start; j < end; j++ {
			rows = append(rows, fmt.Sprintf("%4d| %s", j+1, contentLines[j]))
		}
		parts = append(parts, strings.Join(rows, "\n"))
	}
	result := strings.Join(parts, "\n---\n")
	best := contentLines[top[0].index]
	if trimSpace(best) == anchor && best != oldLines[0] {
		result += "\n\nWhitespace difference detected (→ = tab, · = space):\n" +
			"  file has: " + visualizeWhitespace(best) + "\n" +
			"  you sent: " + visualizeWhitespace(oldLines[0]) + "\n" +
			"Use the exact whitespace shown in 'file has'."
	}
	return result
}

func NoMatchHint(errText string, matchCount int, oldString, content string) string {
	if matchCount != 0 || errText == "" || !strings.HasPrefix(errText, "Could not find") {
		return ""
	}
	hint := closestLines(oldString, content)
	if hint == "" {
		return ""
	}
	return "\n\nDid you mean one of these sections?\n" + hint
}

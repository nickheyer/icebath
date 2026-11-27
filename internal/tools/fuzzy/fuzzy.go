// Package fuzzy finds the text an edit means to replace when the model's copy of it is not byte for byte the file's: nine strategies
// from an exact match to similarity scoring, each tried in turn, with the guards that keep a near miss from editing the wrong place
package fuzzy

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/tools/diff"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Failure of a replace whose old_string matches nowhere in the file
const NoMatch = "Could not find a match for old_string in the file"

// Lead of the no match failure that names the file
const noMatchInLead = "Could not find match for old_string in "

// Lead of the closest lines hint appended to a no match failure
const DidYouMean = "\n\nDid you mean one of these sections?\n"

const identical = "No edit was applied because old_string and new_string are identical. Provide the existing text to replace in old_string and " +
	"the changed replacement text in new_string."

// No match failure naming the file
func NoMatchIn(path string) string { return noMatchInLead + path }

// Whether a failure text is or carries a no match failure
func HoldsNoMatch(text string) bool {
	return strings.Contains(text, NoMatch) || strings.Contains(text, noMatchInLead)
}

// Outcome of a replace
type Result struct {
	Content string
	// Spans replaced
	Count int
	// Strategy that found them
	Strategy string
	// What an escape drift correction changed before matching, empty when nothing was corrected
	Note string
}

// Byte span of one match in the content
type span struct{ start, end int }

// One replacement: the span it covers and the text that takes its place
type edit struct {
	span span
	text string
}

type strategy struct {
	name string
	find func(content, pattern string) []span
}

// Strategies from precise to similarity based, in the order they are tried
var strategies = []strategy{
	{"exact", exact},
	{"line_trimmed", lineTrimmed},
	{"whitespace_normalized", whitespaceNormalized},
	{"indentation_flexible", indentationFlexible},
	{"escape_normalized", escapeNormalized},
	{"trimmed_boundary", trimmedBoundary},
	{"unicode_normalized", unicodeNormalized},
	{"block_anchor", blockAnchor},
	{"context_aware", contextAware},
}

// Strategies whose matches only resemble old_string
func approximate(name string) bool { return name == "block_anchor" || name == "context_aware" }

// Replaces old with new in the content through the first strategy that finds it, every match when all is set. Content that is not valid
// UTF-8 is matched as a reader shows it, each undecodable byte as U+FFFD, and every byte the edit leaves unchanged is written back as it was
func Replace(content, old, new string, all bool) (Result, error) {
	if utf8.ValidString(content) {
		edits, result, err := find(content, old, new, all)
		if err != nil {
			return Result{Content: content}, err
		}
		for _, e := range edits {
			content = content[:e.span.start] + e.text + content[e.span.end:]
		}
		result.Content = content
		return result, nil
	}
	view, holes := undecodable(content)
	edits, result, err := find(view, old, new, all)
	if err != nil {
		return Result{Content: content}, err
	}
	out := content
	for _, e := range edits {
		start, end := original(holes, e.span.start), original(holes, e.span.end)
		out = out[:start] + keepBytes(content, view, holes, e.span, e.text) + out[end:]
	}
	result.Content = out
	return result, nil
}

// Edits of the first strategy that matches, last span first so splicing them in order keeps earlier offsets valid
func find(content, old, new string, all bool) ([]edit, Result, error) {
	if old == "" {
		return nil, Result{}, errors.New("old_string is empty — nothing to match. Set old_string to the exact existing text the replacement should " +
			"replace (read the file first if unsure). To create a new file or fully rewrite one, use write_file instead. Do not re-send this call unchanged.")
	}
	if strings.TrimSpace(old) == "" {
		return nil, Result{}, errors.New("old_string is only whitespace — provide non-blank text to match. Set it to the exact existing text the " +
			"replacement should replace (read the file first if unsure). Do not re-send this call unchanged.")
	}
	old, new, note, err := correctDrift(old, new, content)
	if err != nil {
		return nil, Result{}, err
	}
	if old == new {
		return nil, Result{}, errors.New(identical)
	}
	for _, s := range strategies {
		matches := s.find(content, old)
		if len(matches) == 0 {
			continue
		}
		if len(matches) > 1 && !all {
			return nil, Result{}, fmt.Errorf("Found %d matches for old_string. Provide more context to make it unique, or use replace_all=True. "+
				"Matches:\n%s", len(matches), locations(content, matches, 5))
		}
		if all && len(matches) > 1 && approximate(s.name) {
			return nil, Result{}, fmt.Errorf("Found %d approximate matches via the '%s' strategy; replace_all only applies to exact matches. Provide the "+
				"precise text (whitespace included) so an exact/line-trimmed match can be made.", len(matches), s.name)
		}
		if s.name != "exact" {
			if drift := escapeDrift(content, matches, old, new); drift != "" {
				return nil, Result{}, errors.New(drift)
			}
		}
		replacement := unescapeControls(new, content, matches)
		var plan *unicodePlan
		if s.name == "unicode_normalized" {
			plan = planUnicode(old, replacement)
		}
		return replacements(content, matches, replacement, old, s.name != "exact", plan), Result{Count: len(matches), Strategy: s.name, Note: note}, nil
	}
	return nil, Result{}, errors.New(NoMatch)
}

// Typographic characters and the ASCII each stands for
var typographic = map[rune]string{
	'“': `"`, '”': `"`, '‘': "'", '’': "'", '—': "--", '–': "-", '…': "...", ' ': " ", '−': "-",
	' ': " ", ' ': " ", ' ': " ", ' ': " ", ' ': " ", ' ': " ", ' ': " ", ' ': " ", ' ': " ",
	' ': " ", ' ': " ", ' ': " ", ' ': " ", '　': " ",
}

// Text with typographic quotes, dashes, ellipses and spaces as ASCII
func ascii(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if repl, ok := typographic[r]; ok {
			b.WriteString(repl)
		} else {
			b.WriteString(text[i : i+size])
		}
		i += size
	}
	return b.String()
}

// Byte offsets where each of the first k lines ends, its line break counted
func prefixSums(lines []string) []int {
	sums := make([]int, len(lines)+1)
	for i, line := range lines {
		sums[i+1] = sums[i] + len(line) + 1
	}
	return sums
}

// Spans of every window of n lines that starts at an accepted line
func windows(content string, lines []string, n int, accept func(i int) bool) []span {
	var out []span
	sums := prefixSums(lines)
	for i := 0; i < len(lines)-n+1; i++ {
		if accept(i) {
			out = append(out, span{sums[i], min(len(content), sums[i+n]-1)})
		}
	}
	return out
}

// Windows of the content whose lines, transformed, equal the pattern's
func transformedLines(content, pattern string, transform func([]string) []string) []span {
	lines := strings.Split(content, "\n")
	want := transform(strings.Split(pattern, "\n"))
	n := len(want)
	return windows(content, lines, n, func(i int) bool { return slices.Equal(transform(lines[i:i+n]), want) })
}

func each(f func(string) string) func([]string) []string {
	return func(lines []string) []string {
		out := make([]string, len(lines))
		for i, line := range lines {
			out[i] = f(line)
		}
		return out
	}
}

// Lines with the first and last trimmed
func trimEnds(lines []string) []string {
	out := slices.Clone(lines)
	out[0] = strings.TrimSpace(out[0])
	if len(out) > 1 {
		out[len(out)-1] = strings.TrimSpace(out[len(out)-1])
	}
	return out
}

// Non overlapping occurrences
func exact(content, pattern string) []span {
	if pattern == "" {
		return nil
	}
	var out []span
	for at := 0; ; {
		k := strings.Index(content[at:], pattern)
		if k < 0 {
			return out
		}
		out = append(out, span{at + k, at + k + len(pattern)})
		at += k + len(pattern)
	}
}

func lineTrimmed(content, pattern string) []span {
	return transformedLines(content, pattern, each(strings.TrimSpace))
}

func spaceOrTab(c byte) bool { return c == ' ' || c == '\t' }

// Runs of spaces and tabs as one space
func collapse(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if spaceOrTab(s[i]) {
			for i+1 < len(s) && spaceOrTab(s[i+1]) {
				i++
			}
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func whitespaceNormalized(content, pattern string) []span {
	collapsed := collapse(content)
	matches := exact(collapsed, collapse(pattern))
	if len(matches) == 0 {
		return nil
	}
	return uncollapse(content, collapsed, matches)
}

// Spans found in the collapsed text mapped back onto the original
func uncollapse(original, collapsed string, matches []span) []span {
	at := make([]int, 0, len(original))
	oi, ci := 0, 0
	for oi < len(original) && ci < len(collapsed) {
		switch {
		case original[oi] == collapsed[ci]:
			at = append(at, ci)
			oi++
			ci++
		case spaceOrTab(original[oi]) && collapsed[ci] == ' ':
			at = append(at, ci)
			oi++
			if oi < len(original) && !spaceOrTab(original[oi]) {
				ci++
			}
		default:
			at = append(at, ci)
			oi++
		}
	}
	for ; oi < len(original); oi++ {
		at = append(at, len(collapsed))
	}
	first, last := map[int]int{}, map[int]int{}
	for pos, c := range at {
		if _, ok := first[c]; !ok {
			first[c] = pos
		}
		last[c] = pos
	}
	var out []span
	for _, m := range matches {
		start, ok := first[m.start]
		if !ok {
			start = slices.IndexFunc(at, func(c int) bool { return c >= m.start })
			if start < 0 {
				continue
			}
		}
		end := start + (m.end - m.start)
		if e, ok := last[m.end-1]; ok {
			end = e + 1
		}
		if m.end < len(collapsed) && collapsed[m.end-1] == ' ' {
			for end < len(original) && spaceOrTab(original[end]) {
				end++
			}
		}
		out = append(out, span{start, min(end, len(original))})
	}
	return out
}

func indentationFlexible(content, pattern string) []span {
	return transformedLines(content, pattern, each(func(line string) string { return strings.TrimLeftFunc(line, unicode.IsSpace) }))
}

// Literal \n, \t and \r of the pattern read as the characters they name
func escapeNormalized(content, pattern string) []span {
	unescaped := strings.NewReplacer(`\n`, "\n", `\t`, "\t", `\r`, "\r").Replace(pattern)
	if unescaped == pattern {
		return nil
	}
	return exact(content, unescaped)
}

func trimmedBoundary(content, pattern string) []span {
	return transformedLines(content, pattern, trimEnds)
}

// Offsets of each character of a text and where it lands once made ASCII, counted in bytes or characters, with an end sentinel
type asciiMap struct {
	from []int
	to   []int
}

func mapASCII(text string, inRunes bool) asciiMap {
	var m asciiMap
	pos := 0
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		m.from = append(m.from, i)
		m.to = append(m.to, pos)
		switch repl, ok := typographic[r]; {
		case ok:
			pos += len(repl)
		case inRunes:
			pos++
		default:
			pos += size
		}
		i += size
	}
	m.from = append(m.from, len(text))
	m.to = append(m.to, pos)
	return m
}

// Character index of the original past which the ASCII position reaches end, walking from start
func (m asciiMap) until(start, end int) int {
	last := len(m.to) - 1
	for start < last && m.to[start] < end {
		start++
	}
	return start
}

// Exact or line trimmed match once both sides are made ASCII
func unicodeNormalized(content, pattern string) []span {
	asciiPattern, asciiContent := ascii(pattern), ascii(content)
	if asciiContent == content && asciiPattern == pattern {
		return nil
	}
	matches := exact(asciiContent, asciiPattern)
	if len(matches) == 0 {
		matches = lineTrimmed(asciiContent, asciiPattern)
	}
	if len(matches) == 0 {
		return nil
	}
	m := mapASCII(content, false)
	first := map[int]int{}
	for k := 0; k < len(m.to)-1; k++ {
		if _, ok := first[m.to[k]]; !ok {
			first[m.to[k]] = k
		}
	}
	var out []span
	for _, s := range matches {
		if k, ok := first[s.start]; ok {
			out = append(out, span{m.from[k], m.from[m.until(k, s.end)]})
		}
	}
	return out
}

// Windows whose first and last lines equal the pattern's and whose middle is similar enough
func blockAnchor(content, pattern string) []span {
	lines := strings.Split(ascii(pattern), "\n")
	if len(lines) < 2 {
		return nil
	}
	n := len(lines)
	head, tail := strings.TrimSpace(lines[0]), strings.TrimSpace(lines[n-1])
	normalized := strings.Split(ascii(content), "\n")
	anchored := map[int]bool{}
	for i := 0; i < len(normalized)-n+1; i++ {
		if strings.TrimSpace(normalized[i]) == head && strings.TrimSpace(normalized[i+n-1]) == tail {
			anchored[i] = true
		}
	}
	threshold := 0.70
	if len(anchored) == 1 {
		threshold = 0.50
	}
	middle := strings.Join(lines[1:n-1], "\n")
	return windows(content, strings.Split(content, "\n"), n, func(i int) bool {
		if !anchored[i] {
			return false
		}
		return n <= 2 || diff.Ratio(strings.Join(normalized[i+1:i+n-1], "\n"), middle) >= threshold
	})
}

// Windows whose first and last lines, and every non blank line, are at least 80 percent similar to the pattern's
func contextAware(content, pattern string) []span {
	want := strings.Split(pattern, "\n")
	lines := strings.Split(content, "\n")
	n := len(want)
	if n > len(lines) {
		return nil
	}
	similar := func(a, b string) bool { return a == b || diff.Ratio(a, b) >= 0.80 }
	return windows(content, lines, n, func(i int) bool {
		block := lines[i : i+n]
		if !similar(strings.TrimSpace(want[0]), strings.TrimSpace(block[0])) || !similar(strings.TrimSpace(want[n-1]), strings.TrimSpace(block[n-1])) {
			return false
		}
		for k, line := range want {
			if p := strings.TrimSpace(line); p != "" && !similar(p, strings.TrimSpace(block[k])) {
				return false
			}
		}
		return true
	})
}

// Note for an edit a similarity strategy located, empty when every strategy matched exactly
func BoundaryNote(used []string, target, where string) string {
	var named []string
	for _, s := range used {
		if approximate(s) && !slices.Contains(named, s) {
			named = append(named, s)
		}
	}
	if len(named) == 0 {
		return ""
	}
	return "Non-exact match (strategy: " + strings.Join(named, ", ") + ")" + where + ". The edit span was located approximately, not matched to " +
		target + " literal boundaries — review the diff below to confirm the change landed exactly where intended (a shifted boundary can " +
		"duplicate or drop a line just outside the edited region)."
}

// Whether a re sent edit already landed: its new text, of some length, is in the content and its old text is gone; content compared as a
// reader shows it
func Applied(content, old, new string) bool {
	if !utf8.ValidString(content) {
		content, _ = undecodable(content)
	}
	if new == "" || utf8.RuneCountInString(strings.TrimSpace(new)) < 8 || !strings.Contains(content, new) {
		return false
	}
	return old == new || !strings.Contains(content, old)
}

// Up to limit match positions as line number and snippet rows
func locations(content string, matches []span, limit int) string {
	var rows []string
	for i, m := range matches {
		if i >= limit {
			break
		}
		lineStart := strings.LastIndexByte(content[:m.start], '\n') + 1
		lineEnd := strings.IndexByte(content[lineStart:], '\n')
		if lineEnd < 0 {
			lineEnd = len(content)
		} else {
			lineEnd += lineStart
		}
		snippet := strings.TrimSpace(content[lineStart:lineEnd])
		if utf8.RuneCountInString(snippet) > 80 {
			snippet = strs.Head(snippet, 77) + "..."
		}
		rows = append(rows, fmt.Sprintf("  L%d: %s", strings.Count(content[:m.start], "\n")+1, snippet))
	}
	if extra := len(matches) - limit; extra > 0 {
		rows = append(rows, fmt.Sprintf("  ... and %d more", extra))
	}
	return strings.Join(rows, "\n")
}

// Content with each byte that starts no valid UTF-8 sequence as U+FFFD, and the offsets of those characters in it
func undecodable(content string) (string, []int) {
	var b strings.Builder
	b.Grow(len(content) + 8)
	var holes []int
	start := 0
	for i := 0; i < len(content); {
		r, size := utf8.DecodeRuneInString(content[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteString(content[start:i])
			holes = append(holes, b.Len())
			b.WriteRune(utf8.RuneError)
			i++
			start = i
			continue
		}
		i += size
	}
	b.WriteString(content[start:])
	return b.String(), holes
}

// Offset in the content of an offset in its view, one inside a replacement character landing after the byte it stands for
func original(holes []int, at int) int {
	k := sort.SearchInts(holes, at)
	if k > 0 && at < holes[k-1]+utf8.RuneLen(utf8.RuneError) {
		return holes[k-1] - 2*(k-1) + 1
	}
	return at - 2*k
}

// Replacement of a span of the view, every stretch the edit leaves unchanged written as the content's own bytes
func keepBytes(content, view string, holes []int, s span, text string) string {
	region := view[s.start:s.end]
	if region == content[original(holes, s.start):original(holes, s.end)] {
		return text
	}
	regionAt, textAt := offsets(region), offsets(text)
	var b strings.Builder
	for _, op := range diff.New[rune](nil, []rune(region), []rune(text), false).Opcodes() {
		if op.Tag == "equal" {
			b.WriteString(content[original(holes, s.start+regionAt[op.I1]):original(holes, s.start+regionAt[op.I2])])
			continue
		}
		b.WriteString(text[textAt[op.J1]:textAt[op.J2]])
	}
	return b.String()
}

// Byte offset of every character of s and its end
func offsets(s string) []int {
	out := make([]int, 0, len(s)+1)
	for i := range s {
		out = append(out, i)
	}
	return append(out, len(s))
}

func regions(content string, matches []span) string {
	var b strings.Builder
	for _, m := range matches {
		b.WriteString(content[m.start:m.end])
	}
	return b.String()
}

// Error when a literal \n of the arguments stands in for a real line break of the file
func newlineDrift(content string, matches []span, old, new string) string {
	if !strings.Contains(new, `\n`) {
		return ""
	}
	literals := strings.Count(old, `\n`)
	for _, m := range matches {
		region := content[m.start:m.end]
		if strings.Contains(region, "\n") && literals > strings.Count(region, `\n`) {
			return `Escape-drift detected: old_string contains more literal '\\n' sequences than the matched region of the file, which has real ` +
				"line breaks there instead. This is almost always a tool-call serialization artifact where a line break got escaped one extra " +
				"time; new_string would write it as backslash + n. Re-read the file with read_file and pass old_string/new_string with actual line " +
				`breaks, keeping only the '\\n' sequences that appear literally in the file.`
		}
	}
	return ""
}

// Error when the arguments carry quote escapes, doubled backslash runs or literal newlines the matched file text lacks
func escapeDrift(content string, matches []span, old, new string) string {
	quotes := strings.Contains(new, `\'`) || strings.Contains(new, `\"`)
	if !quotes && !strings.Contains(old, `\`) {
		return ""
	}
	matched := regions(content, matches)
	if quotes {
		for _, suspect := range []string{`\'`, `\"`} {
			if strings.Contains(new, suspect) && strings.Contains(old, suspect) && !strings.Contains(matched, suspect) {
				return fmt.Sprintf("Escape-drift detected: old_string and new_string contain the literal sequence %q but the matched region of the "+
					"file does not. This is almost always a tool-call serialization artifact where an apostrophe or quote got prefixed with a "+
					"spurious backslash. Re-read the file with read_file and pass old_string/new_string without backslash-escaping %q characters.",
					suspect, suspect[1:])
			}
		}
	}
	if drift := newlineDrift(content, matches, old, new); drift != "" {
		return drift
	}
	return backslashDoubling(matched, old, new)
}

// Error when every backslash run of old_string is exactly twice the file's
func backslashDoubling(matched, old, new string) string {
	olds, files := runs(old), runs(matched)
	if len(olds) == 0 || len(files) == 0 || len(olds) != len(files) || slices.Equal(olds, files) {
		return ""
	}
	strong := len(files) >= 2
	for i, f := range files {
		if olds[i] != f*2 {
			return ""
		}
		if f >= 2 {
			strong = true
		}
	}
	if !strong || slices.Equal(runs(new), files) {
		return ""
	}
	return "Escape-drift detected: every backslash run in old_string is exactly twice as long as in the matched region of the file (e.g. the " +
		"file has `\\\\` where old_string has `\\\\\\\\`). The tool-call arguments were JSON-escaped one extra time; applying new_string " +
		"verbatim would double every backslash in the file. Re-read the file with read_file and resend old_string/new_string with the " +
		"backslash counts exactly as they appear in the file."
}

// Literal \t and \r of the replacement as the real characters when the matched file text holds them
func unescapeControls(new, content string, matches []span) string {
	if !strings.Contains(new, `\t`) && !strings.Contains(new, `\r`) {
		return new
	}
	matched := regions(content, matches)
	for _, pair := range [][2]string{{`\t`, "\t"}, {`\r`, "\r"}} {
		if strings.Contains(new, pair[0]) && strings.Contains(matched, pair[1]) {
			new = strings.ReplaceAll(new, pair[0], pair[1])
		}
	}
	return new
}

func indentOf(line string) string {
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
}

// First line holding text
func firstText(text string) (string, bool) {
	for line := range strings.SplitSeq(text, "\n") {
		if strings.TrimSpace(line) != "" {
			return line, true
		}
	}
	return "", false
}

// Whether text outside the match comes before its first non blank line
func midLine(content string, start, end int) bool {
	at := start
	for line := range strings.SplitSeq(content[start:end], "\n") {
		if strings.TrimSpace(line) != "" {
			lineStart := strings.LastIndexByte(content[:at], '\n') + 1
			return strings.Trim(content[lineStart:at], " \t\r") != ""
		}
		at += len(line) + 1
	}
	return false
}

// Replacement moved onto the file's indentation after a match that was not exact
func reindent(region, old, new string, mid bool) string {
	if new == "" || mid {
		return new
	}
	oldFirst, ok1 := firstText(old)
	fileFirst, ok2 := firstText(region)
	if !ok1 || !ok2 {
		return new
	}
	oldIndent, fileIndent := indentOf(oldFirst), indentOf(fileFirst)
	if oldIndent == fileIndent {
		return new
	}
	lines := strings.Split(new, "\n")
	for i, line := range lines {
		switch {
		case strings.TrimSpace(line) == "":
		case strings.HasPrefix(indentOf(line), oldIndent):
			lines[i] = fileIndent + line[len(oldIndent):]
		default:
			lines[i] = fileIndent + strings.TrimLeft(line, " \t")
		}
	}
	return strings.Join(lines, "\n")
}

// old_string made ASCII and the character changes from it to new_string, shared by every match
type unicodePlan struct {
	old     string
	opcodes []diff.Opcode
}

func planUnicode(old, new string) *unicodePlan {
	plain := ascii(old)
	return &unicodePlan{plain, diff.Runes(plain, new).Opcodes()}
}

// Only the changes from old to new applied onto the region, so its typography survives
func keepTypography(region, new string, plan *unicodePlan) string {
	if plan.old != ascii(region) {
		return new
	}
	m := mapASCII(region, true)
	at := offsets(new)
	var b strings.Builder
	for _, op := range plan.opcodes {
		switch op.Tag {
		case "equal":
			start := sort.Search(len(m.to), func(k int) bool { return m.to[k] > op.I1 }) - 1
			b.WriteString(region[m.from[start]:m.from[m.until(start, op.I2)]])
		case "delete":
		default:
			b.WriteString(new[at[op.J1]:at[op.J2]])
		}
	}
	return b.String()
}

// Replacement of each span, last first, reindented and keeping typography when asked
func replacements(content string, matches []span, new, old string, adjust bool, plan *unicodePlan) []edit {
	ordered := slices.Clone(matches)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].start > ordered[j].start })
	edits := make([]edit, len(ordered))
	for i, m := range ordered {
		text := new
		if adjust {
			region := content[m.start:m.end]
			if plan != nil {
				text = keepTypography(region, text, plan)
			}
			text = reindent(region, old, text, midLine(content, m.start, m.end))
		}
		edits[i] = edit{m, text}
	}
	return edits
}

// Leading spaces and tabs shown as dots and arrows
func showIndent(line string) string {
	body := strings.TrimLeft(line, " \t")
	return strings.NewReplacer("\t", "→", " ", "·").Replace(line[:len(line)-len(body)]) + body
}

// Numbered snippets of the lines most like old_string's first line, and the whitespace difference when the best one differs only there
func closest(old, content string, context, results int) string {
	if old == "" || content == "" {
		return ""
	}
	oldLines, lines := diff.Split(old), diff.Split(content)
	if len(oldLines) == 0 || len(lines) == 0 {
		return ""
	}
	anchor := ""
	for _, line := range oldLines {
		if anchor = strings.TrimSpace(line); anchor != "" {
			break
		}
	}
	if anchor == "" {
		return ""
	}
	type scored struct {
		ratio float64
		at    int
	}
	var all []scored
	for i, line := range lines {
		if s := strings.TrimSpace(line); s != "" {
			all = append(all, scored{diff.Ratio(anchor, s), i})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].ratio > all[j].ratio })
	var top []scored
	for _, s := range all {
		if s.ratio > 0.3 && len(top) < results {
			top = append(top, s)
		}
	}
	if len(top) == 0 {
		return ""
	}
	var parts []string
	seen := map[[2]int]bool{}
	for _, t := range top {
		start, end := max(0, t.at-context), min(len(lines), t.at+len(oldLines)+context)
		if seen[[2]int{start, end}] {
			continue
		}
		seen[[2]int{start, end}] = true
		rows := make([]string, end-start)
		for j := range rows {
			rows[j] = fmt.Sprintf("%4d| %s", start+j+1, lines[start+j])
		}
		parts = append(parts, strings.Join(rows, "\n"))
	}
	out := strings.Join(parts, "\n---\n")
	if best := lines[top[0].at]; strings.TrimSpace(best) == anchor && best != oldLines[0] {
		out += "\n\nWhitespace difference detected (→ = tab, · = space):\n  file has: " + showIndent(best) + "\n  you sent: " + showIndent(oldLines[0]) +
			"\nUse the exact whitespace shown in 'file has'."
	}
	return out
}

// Did you mean snippet for a failure that found no match at all, empty for any other
func NoMatchHint(failure string, count int, old, content string) string {
	if count != 0 || !(failure == NoMatch || strings.HasPrefix(failure, noMatchInLead)) {
		return ""
	}
	if hint := closest(old, content, 2, 3); hint != "" {
		return DidYouMean + hint
	}
	return ""
}

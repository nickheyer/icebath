package tools

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// SearchMatch is one search hit with the modification time used for sorting
type SearchMatch struct {
	Path       string
	LineNumber int
	// Decimal digits of a line number beyond the int range, exact as the Python int
	LineNumberOverflow string
	Content            string
	Mtime              float64
}

// Match whose line number is parsed like Python int on a decimal digit run of any length
func newSearchMatch(path, digits, content string) SearchMatch {
	n, overflow := searchLineNumber(digits)
	return SearchMatch{Path: path, LineNumber: n, LineNumberOverflow: overflow, Content: content}
}

// Python int of a decimal digit string, the stripped digits returned instead when it exceeds int
func searchLineNumber(digits string) (int, string) {
	stripped := strings.TrimLeft(digits, "0")
	if stripped == "" {
		return 0, ""
	}
	n, err := strconv.ParseInt(stripped, 10, 0)
	if err != nil {
		return 0, stripped
	}
	return int(n), ""
}

// Line number as decimal text, exact for values beyond the int range
func (m SearchMatch) lineNumberText() string {
	if m.LineNumberOverflow != "" {
		return m.LineNumberOverflow
	}
	return strconv.Itoa(m.LineNumber)
}

// SearchCount is one path and its match count, kept in discovery order like a Python dict
type SearchCount struct {
	Path  string
	Count int
}

// SearchResult is one search outcome with the key order and omission rules the model reads
type SearchResult struct {
	Matches     []SearchMatch
	Files       []string
	Counts      []SearchCount
	TotalCount  int
	Truncated   bool
	LimitReason string
	Warning     string
	Error       string
	// Unreadable paths behind the skipped sentence in Warning, never serialized
	SkippedPaths   []string
	SkippedOmitted int
}

// Below this many matches the verbose array is already compact enough
const searchDensifyMinMatches = 5

// Sets a path count in place or appends it, the dict assignment of counts
func (r *SearchResult) setCount(path string, n int) {
	for i := range r.Counts {
		if r.Counts[i].Path == path {
			r.Counts[i].Count = n
			return
		}
	}
	r.Counts = append(r.Counts, SearchCount{Path: path, Count: n})
}

// Merges another result's counts with dict update semantics
func (r *SearchResult) updateCounts(other []SearchCount) {
	for _, c := range other {
		r.setCount(c.Path, c.Count)
	}
}

// Path grouped text block of the matches, empty with false below the densify threshold
func (r *SearchResult) densifyMatches() (string, bool) {
	if len(r.Matches) < searchDensifyMinMatches {
		return "", false
	}
	var lines []string
	current, started := "", false
	for _, m := range r.Matches {
		if !started || m.Path != current {
			lines = append(lines, m.Path)
			current, started = m.Path, true
		}
		lines = append(lines, "  "+m.lineNumberText()+": "+pyRStrip(m.Content))
	}
	return strings.Join(lines, "\n"), true
}

// Wire shape of one verbose match row in path, line, content order
type searchMatchJSON struct {
	Path    string      `json:"path"`
	Line    json.Number `json:"line"`
	Content string      `json:"content"`
}

// Ordered dict of the result, path grouped matches when densify is set and worth it
func (r *SearchResult) toDict(densify bool) *pyJSONObject {
	out := &pyJSONObject{vals: map[string]any{}}
	out.set("total_count", r.TotalCount)
	if len(r.Matches) > 0 {
		dense, ok := "", false
		if densify {
			dense, ok = r.densifyMatches()
		}
		if ok {
			out.set("matches_format", "path-grouped: each file path on its own line, followed by "+
				"indented '<line>: <content>' rows for matches in that file")
			out.set("matches_text", dense)
		} else {
			rows := make([]searchMatchJSON, len(r.Matches))
			for i, m := range r.Matches {
				rows[i] = searchMatchJSON{Path: m.Path, Line: json.Number(m.lineNumberText()), Content: m.Content}
			}
			out.set("matches", rows)
		}
	}
	if len(r.Files) > 0 {
		out.set("files", r.Files)
	}
	if len(r.Counts) > 0 {
		counts := &pyJSONObject{vals: map[string]any{}}
		for _, c := range r.Counts {
			counts.set(c.Path, c.Count)
		}
		out.set("counts", counts)
	}
	if r.Truncated {
		out.set("truncated", true)
		out.set("total_count_is_lower_bound", true)
	}
	for _, kv := range [][2]string{{"limit_reason", r.LimitReason}, {"warning", r.Warning}, {"error", r.Error}} {
		if kv[1] != "" {
			out.set(kv[0], kv[1])
		}
	}
	return out
}

// MarshalJSON writes the object keys in insertion order without HTML escaping, surrogates kept as code points
func (o *pyJSONObject) MarshalJSON() ([]byte, error) {
	buf := []byte{'{'}
	for i, k := range o.keys {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = appendJSONStringWTF8(buf, k)
		buf = append(buf, ':')
		val, err := jsonEncodeWTF8(o.vals[k])
		if err != nil {
			return nil, err
		}
		buf = append(buf, val...)
	}
	return append(buf, '}'), nil
}

// LintResult is one syntax check outcome, construct with newLintResult for the success default
type LintResult struct {
	Success bool
	Skipped bool
	Output  string
	Message string
}

// Lint result carrying the dataclass defaults of success true and nothing skipped
func newLintResult() *LintResult { return &LintResult{Success: true} }

// Lint dict in status, output, message order or the skipped status with its message
func (l *LintResult) toDict() *pyJSONObject {
	out := &pyJSONObject{vals: map[string]any{}}
	if l.Skipped {
		out.set("status", "skipped")
		out.set("message", l.Message)
		return out
	}
	status := "error"
	if l.Success {
		status = "ok"
	}
	out.set("status", status)
	out.set("output", l.Output)
	if l.Message != "" {
		out.set("message", l.Message)
	}
	return out
}

// ExecuteResult is one fenced shell command outcome, CwdError set when the cd wrapper failed
type ExecuteResult struct {
	Stdout   string
	ExitCode int
	CwdError string
}

var (
	oscSequenceRe = regexp.MustCompile("\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)")
	fenceMarkerRe = regexp.MustCompile("'?\x07?__ICEBATH_FENCE_[A-Za-z0-9]+__\x07?'?")
	conflictOpen  = regexp.MustCompile(`(?m)^` + pyWSClass + `*\p{Nd}+\|<<<<<<< `)
	conflictClose = regexp.MustCompile(`(?m)^` + pyWSClass + `*\p{Nd}+\|>>>>>>> `)
)

// Balanced unresolved merge conflict blocks in a numbered read, zero without a balanced pair
func countConflictBlocks(formattedContent string) int {
	opens := len(conflictOpen.FindAllStringIndex(formattedContent, -1))
	if opens == 0 {
		return 0
	}
	return min(opens, len(conflictClose.FindAllStringIndex(formattedContent, -1)))
}

// Removes leaked OSC sequences and fence markers, dropping lines that were only wrapper
func stripTerminalFenceLeaks(text string) string {
	if text == "" {
		return text
	}
	var b strings.Builder
	for _, line := range pySplitLinesKeepEnds(text) {
		hadWrapper := strings.Contains(line, "__ICEBATH_FENCE_") || strings.Contains(line, "\x1b]")
		cleaned := strings.ReplaceAll(fenceMarkerRe.ReplaceAllString(oscSequenceRe.ReplaceAllString(line, ""), ""), "\x07", "")
		if hadWrapper && strings.Trim(cleaned, "'\r\n\t ") == "" {
			continue
		}
		b.WriteString(cleaned)
	}
	return b.String()
}

// Byte prefix of s holding at most n characters, a surrogate or stray byte counting as one character
func runePrefix(s string, n int) string {
	i := 0
	for count := 0; i < len(s) && count < n; count++ {
		_, size := wtf8DecodeRune(s[i:])
		i += size
	}
	return s[:i]
}

// Dominant line ending of the first 4096 characters, CRLF when any appears, empty for single line text
func detectLineEnding(sample string) string {
	head := runePrefix(sample, 4096)
	if strings.Contains(head, "\r\n") {
		return "\r\n"
	}
	if strings.Contains(head, "\n") {
		return "\n"
	}
	return ""
}

// Every CRLF, lone CR and LF converted to target, text unchanged for any other target
func normalizeLineEndings(text, target string) string {
	lf := strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	switch target {
	case "\n":
		return lf
	case "\r\n":
		return strings.ReplaceAll(lf, "\n", "\r\n")
	}
	return text
}

// Leading UTF-8 byte order mark that some Windows editors prepend
const utf8BOM = "\xef\xbb\xbf"

// Text without its leading byte order mark and whether one was there
func stripBOM(text string) (string, bool) {
	if hasBOM(text) {
		return text[len(utf8BOM):], true
	}
	return text, false
}

// True when text begins with a UTF-8 byte order mark
func hasBOM(text string) bool { return strings.HasPrefix(text, utf8BOM) }

// Pagination defaults for read_file and search_files
const (
	DefaultReadOffset   = 1
	DefaultReadLimit    = 2000
	DefaultSearchOffset = 0
	DefaultSearchLimit  = 50
)

// Integer the way Python int() reads a pagination input, the default on any type or value error
func coercePaginationInt(value any, def int) int {
	switch v := value.(type) {
	case nil:
		return def
	case string:
		if n, ok := pyIntParse(v); ok {
			return n
		}
		return def
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return def
		}
		return int(v)
	case float32:
		return coercePaginationInt(float64(v), def)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
		if f, err := v.Float64(); err == nil {
			return coercePaginationInt(f, def)
		}
		return def
	}
	if n, ok := pyIntValue(value); ok {
		return n
	}
	return def
}

// Read offset at least one and limit between one and tool_output.max_lines, nil meaning the default
func normalizeReadPagination(env *Env, offset, limit any) (int, int) {
	o := max(1, coercePaginationInt(offset, DefaultReadOffset))
	l := max(1, min(coercePaginationInt(limit, DefaultReadLimit), ToolOutputLimits(env).MaxLines))
	return o, l
}

// Search offset at least zero and limit at least one, nil meaning the default
func normalizeSearchPagination(offset, limit any) (int, int) {
	return max(0, coercePaginationInt(offset, DefaultSearchOffset)), max(1, coercePaginationInt(limit, DefaultSearchLimit))
}

// Python str.rstrip with no argument
func pyRStrip(s string) string { return strings.TrimRightFunc(s, pyIsSpace) }

// Word break MidLetter, MidNumLet and Single_Quote code points Python counts as case ignorable
var pyCaseIgnorablePunct = map[rune]bool{
	0x27: true, 0x2e: true, 0x3a: true, 0xb7: true, 0x387: true, 0x55f: true, 0x5f4: true, 0x2018: true,
	0x2019: true, 0x2024: true, 0x2027: true, 0xfe13: true, 0xfe52: true, 0xfe55: true, 0xff07: true,
	0xff0e: true, 0xff1a: true,
}

// Python case ignorable property used by the final sigma rule
func pyCaseIgnorable(r rune) bool {
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk) || pyCaseIgnorablePunct[r]
}

// Python cased property used by the final sigma rule
func pyCased(r rune) bool {
	return unicode.In(r, unicode.Lu, unicode.Ll, unicode.Lt, unicode.Other_Lowercase, unicode.Other_Uppercase)
}

// Python str.lower with the dotted capital I expansion and the final sigma context rule
func pyLower(s string) string {
	if isASCIIString(s) {
		return strings.ToLower(s)
	}
	rs := wtf8Runes(s)
	var b strings.Builder
	for i, at := 0, 0; at < len(s); i++ {
		r, size := wtf8DecodeRune(s[at:])
		switch {
		case isSurrogateRune(r):
			b.WriteString(s[at : at+size])
		case r == 0x130:
			b.WriteString("i\xcc\x87")
		case r == 0x3a3:
			b.WriteRune(pyLowerSigma(rs, i))
		default:
			b.WriteRune(unicode.ToLower(r))
		}
		at += size
	}
	return b.String()
}

// Final small sigma when a cased letter precedes and none follows past case ignorable runes
func pyLowerSigma(rs []rune, i int) rune {
	j := i - 1
	for j >= 0 && pyCaseIgnorable(rs[j]) {
		j--
	}
	final := j >= 0 && pyCased(rs[j])
	if final && i+1 < len(rs) {
		j = i + 1
		for j < len(rs) && pyCaseIgnorable(rs[j]) {
			j++
		}
		final = j == len(rs) || !pyCased(rs[j])
	}
	if final {
		return 0x3c2
	}
	return 0x3c3
}

// Reports whether every byte of s is ASCII
func isASCIIString(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// ntpath.splitroot into drive, root and the rest, slashes and backslashes both separators
func ntSplitRoot(p string) (string, string, string) {
	normp := strings.ReplaceAll(p, "/", `\`)
	if strings.HasPrefix(normp, `\`) {
		if strings.HasPrefix(normp[1:], `\`) {
			start := 2
			if len(normp) >= 8 && strings.ToUpper(normp[:8]) == `\\?\UNC\` {
				start = 8
			}
			index := strings.Index(normp[min(start, len(normp)):], `\`)
			if index < 0 {
				return p, "", ""
			}
			index += start
			index2 := strings.Index(normp[index+1:], `\`)
			if index2 < 0 {
				return p, "", ""
			}
			index2 += index + 1
			return p[:index2], p[index2 : index2+1], p[index2+1:]
		}
		return "", p[:1], p[1:]
	}
	if len(normp) >= 2 && normp[1] == ':' {
		if len(normp) >= 3 && normp[2] == '\\' {
			return p[:2], p[2:3], p[3:]
		}
		return p[:2], "", p[2:]
	}
	return "", "", p
}

// os.path.split with ntpath rules on Windows hosts and posixpath rules elsewhere
func pathSplitOS(p string) (string, string) {
	if !isWindowsHost {
		return posixSplit(p)
	}
	d, r, rest := ntSplitRoot(p)
	i := len(rest)
	for i > 0 && rest[i-1] != '/' && rest[i-1] != '\\' {
		i--
	}
	head, tail := rest[:i], rest[i:]
	return d + r + strings.TrimRight(head, `/\`), tail
}

// os.path.dirname for the host path flavour
func pathDirnameOS(p string) string {
	head, _ := pathSplitOS(p)
	return head
}

// os.path.basename for the host path flavour
func pathBasenameOS(p string) string {
	_, tail := pathSplitOS(p)
	return tail
}

// os.path.join of two parts with ntpath drive and root rules on Windows hosts
func pathJoinOS(a, b string) string {
	if !isWindowsHost {
		return posixJoin(a, b)
	}
	rd, rr, rp := ntSplitRoot(a)
	pd, pr, pp := ntSplitRoot(b)
	switch {
	case pr != "":
		if pd != "" || rd == "" {
			rd = pd
		}
		rr, rp = pr, pp
	case pd != "" && pd != rd && !strings.EqualFold(pd, rd):
		rd, rr, rp = pd, pr, pp
	default:
		if pd != "" {
			rd = pd
		}
		if rp != "" && !strings.ContainsRune(`\/`, rune(rp[len(rp)-1])) {
			rp += `\`
		}
		rp += pp
	}
	if rp != "" && rr == "" && rd != "" && !strings.ContainsRune(`:\/`, rune(rd[len(rd)-1])) {
		return rd + `\` + rp
	}
	return rd + rr + rp
}

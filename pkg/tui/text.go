package tui

import (
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
)

const (
	liveRenderMaxChars   = 16_000
	liveRenderMaxLines   = 240
	verboseTrailMaxChars = 800
	verboseTrailMaxLines = 12
	thinkingCotMax       = 160
	jsWS                 = `\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`
)

// Status words the face ticker cycles through while the agent thinks
var thinkingVerbs = []string{"pondering", "contemplating", "musing", "cogitating", "ruminating", "deliberating", "mulling",
	"reflecting", "processing", "reasoning", "analyzing", "computing", "synthesizing", "formulating", "brainstorming"}

var (
	wsRun               = regexp.MustCompile(`[` + jsWS + `]+`)
	thinkingStatusRe    = regexp.MustCompile(`(?i)^(?:` + strings.Join(thinkingVerbs, "|") + `)\.{0,3}$`)
	thinkingStatusChunk = regexp.MustCompile(`(?i)[^A-Za-z\n]+[` + jsWS + `]*(?:` + strings.Join(thinkingVerbs, "|") + `)\.{0,3}[` + jsWS + `]*`)
	newlineRuns         = regexp.MustCompile(`\n{3,}`)
	estimateImage       = regexp.MustCompile(`!\[(.*?)\]\(([^)` + jsWS + `]+)\)`)
	estimateLink        = regexp.MustCompile(`\[(.+?)\]\((https?://[^` + jsWS + `)]+)\)`)
	estimateCode        = regexp.MustCompile("`([^`]+)`")
	estimateBold        = regexp.MustCompile(`\*\*(.+?)\*\*`)
	estimateItalic      = regexp.MustCompile(`\*(.+?)\*`)
	estimateStrike      = regexp.MustCompile(`~~(.+?)~~`)
	estimateMark        = regexp.MustCompile(`==(.+?)==`)
	estimateFootnote    = regexp.MustCompile(`\[\^([^\]]+)\]`)
	estimateHeading     = regexp.MustCompile(`^#{1,6}[` + jsWS + `]+`)
	estimateChecklist   = regexp.MustCompile(`^[` + jsWS + `]*[-*+][` + jsWS + `]+\[( |x|X)\][` + jsWS + `]+`)
	estimateBullet      = regexp.MustCompile(`^[` + jsWS + `]*[-*+][` + jsWS + `]+`)
	estimateNumbered    = regexp.MustCompile(`^[` + jsWS + `]*(\d+)\.[` + jsWS + `]+`)
	estimateQuote       = regexp.MustCompile(`^[` + jsWS + `]*(?:>[` + jsWS + `]*)+`)
	estimateFence       = regexp.MustCompile("^[" + jsWS + "]*(`{3,}|~{3,})(.*)$")
	tableRule           = regexp.MustCompile(`^[|` + jsWS + `:-]+$`)
	ansiOSC             = regexp.MustCompile(`\x1b\][\s\S]*?(?:\x07|\x1b\\)`)
	ansiString          = regexp.MustCompile(`\x1b[PX^_][\s\S]*?(?:\x07|\x1b\\)`)
	ansiCSI             = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	ansiNonCSI          = regexp.MustCompile(`\x1b(?:[ -/]+[0-~]|[0-OQ-WYZ\\\x60-~])`)
	ansiStray           = regexp.MustCompile(`\x1b(?:[^\[]|$)`)
	controlBytes        = regexp.MustCompile(`[\x00-\x08\x0B\x0C\x0D\x0E-\x1A\x1C-\x1F\x7F]`)
	pasteBacked         = regexp.MustCompile(`\[\[paste:\d+(?:[^\n]*?)\]\]|\[paste #\d+ (?:attached|excerpt)(?:[^\n]*?)\]`)
	trailingNewlines    = regexp.MustCompile(`\n+$`)
	toolDuration        = regexp.MustCompile(`^(.*?)( \(\d+(?:\.\d)?s\))$`)
)

// Text of UTF-16 units, the index space of a JavaScript string
func units(s string) []uint16 { return utf16.Encode([]rune(s)) }

func fromUnits(u []uint16) string { return string(utf16.Decode(u)) }

// Substring by UTF-16 unit offsets with JavaScript slice semantics for negative and overflowing bounds
func jsSlice(s string, start, end int) string {
	u := units(s)
	n := len(u)
	clamp := func(i int) int {
		if i < 0 {
			i += n
		}
		return min(max(i, 0), n)
	}
	from, to := clamp(start), clamp(end)
	if from >= to {
		return ""
	}
	return fromUnits(u[from:to])
}

// Text with every whitespace run collapsed to one space and trimmed, as replace(/\s+/g, ' ').trim()
func collapseWS(s string) string {
	return strings.TrimFunc(wsRun.ReplaceAllString(s, " "), isJSSpace)
}

// Number formatted to fixed decimals the way JavaScript toFixed rounds, half away from zero on the exact value
func jsToFixed(x float64, digits int) string {
	if math.IsNaN(x) {
		return "NaN"
	}
	if math.IsInf(x, 0) || math.Abs(x) >= 1e21 {
		return jsString(x)
	}
	exact := new(big.Float).SetPrec(2048).SetFloat64(math.Abs(x))
	scale := new(big.Float).SetPrec(2048).SetFloat64(math.Pow(10, float64(digits)))
	exact.Mul(exact, scale)
	exact.Add(exact, new(big.Float).SetFloat64(0.5))
	n, _ := exact.Int(nil)
	text := n.String()
	if digits > 0 {
		for len(text) <= digits {
			text = "0" + text
		}
		text = text[:len(text)-digits] + "." + text[len(text)-digits:]
	}
	if x < 0 && strings.Trim(text, "0.") != "" {
		text = "-" + text
	}
	return text
}

// Count shortened to k or M with one decimal, zero for nothing or non finite
func compactNumber(value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return "0"
	}
	scaled := func(v float64, suffix string) string {
		return strings.TrimSuffix(jsToFixed(v, 1), ".0") + suffix
	}
	switch {
	case value >= 999_950:
		return scaled(value/1_000_000, "M")
	case value >= 999.5:
		return scaled(value/1_000, "k")
	}
	return strconv.FormatFloat(math.Floor(value+0.5), 'f', -1, 64)
}

// Visible text of a line, escape sequences and control bytes removed
func stripAnsi(s string) string {
	s = ansiOSC.ReplaceAllString(s, "")
	s = ansiString.ReplaceAllString(s, "")
	s = stripIncompleteCSI(s)
	s = ansiCSI.ReplaceAllString(s, "")
	s = stripIncompleteCSI(s)
	s = ansiNonCSI.ReplaceAllString(s, "")
	s = ansiStray.ReplaceAllString(s, "")
	return controlBytes.ReplaceAllString(s, "")
}

// Drops a CSI introducer cut off before its final byte, one followed by ESC, a newline or the end
func stripIncompleteCSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] >= '0' && s[j] <= '?' {
				j++
			}
			for j < len(s) && s[j] >= ' ' && s[j] <= '/' {
				j++
			}
			if j == len(s) || s[j] == 0x1b || s[j] == '\n' {
				i = j
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// Whether a byte is a JavaScript word character
func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// Replaces a delimited span with its body where no word character touches either delimiter, as (?<!\w)d(.+?)d(?!\w)
func replaceUnwordedSpan(s, delim string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if strings.HasPrefix(s[i:], delim) && (i == 0 || !isWordByte(s[i-1])) {
			start := i + len(delim)
			end := -1
			for j := start + 1; j+len(delim) <= len(s); j++ {
				if s[j-1] == '\n' {
					break
				}
				if strings.HasPrefix(s[j:], delim) && (j+len(delim) == len(s) || !isWordByte(s[j+len(delim)])) {
					end = j
					break
				}
			}
			if end > start && !strings.Contains(s[start:end], "\n") {
				b.WriteString(s[start:end])
				i = end + len(delim)
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// Plain rendering of one markdown line for row estimates
func renderEstimateLine(line string) string {
	trimmed := strings.TrimFunc(line, isJSSpace)
	if strings.HasPrefix(trimmed, "|") {
		var cells []string
		for cell := range strings.SplitSeq(trimmed, "|") {
			if cell != "" {
				cells = append(cells, strings.TrimFunc(cell, isJSSpace))
			}
		}
		return strings.Join(cells, "  ")
	}
	out := estimateImage.ReplaceAllString(line, "[image: $1]")
	out = estimateLink.ReplaceAllString(out, "$1")
	out = estimateCode.ReplaceAllString(out, "$1")
	out = estimateBold.ReplaceAllString(out, "$1")
	out = replaceUnwordedSpan(out, "__")
	out = estimateItalic.ReplaceAllString(out, "$1")
	out = replaceUnwordedSpan(out, "_")
	out = estimateStrike.ReplaceAllString(out, "$1")
	out = estimateMark.ReplaceAllString(out, "$1")
	out = estimateFootnote.ReplaceAllString(out, "[$1]")
	out = estimateHeading.ReplaceAllString(out, "")
	out = estimateChecklist.ReplaceAllStringFunc(out, func(m string) string {
		checked := estimateChecklist.FindStringSubmatch(m)[1]
		if strings.ToLower(checked) == "x" {
			return "• [x] "
		}
		return "• [ ] "
	})
	out = estimateBullet.ReplaceAllString(out, "• ")
	out = estimateNumbered.ReplaceAllString(out, "$1. ")
	return estimateQuote.ReplaceAllString(out, "│ ")
}

// One line preview cut to max units with an ellipsis
func compactPreview(s string, limit int) string {
	one := collapseWS(s)
	if one == "" {
		return ""
	}
	if unitLen(one) > limit {
		return jsSlice(one, 0, limit-1) + "…"
	}
	return one
}

// Rough token count, a quarter of the UTF-16 length rounded up
func estimateTokensRough(text string) int {
	if text == "" {
		return 0
	}
	return (unitLen(text) + 3) >> 2
}

// Preview keeping the head and tail of long text around two dots
func edgePreview(s string, head, tail int) string {
	one := strings.ReplaceAll(collapseWS(s), "]]", "] ]")
	if one == "" {
		return ""
	}
	if unitLen(one) <= head+tail+4 {
		return one
	}
	return strings.TrimRightFunc(jsSlice(one, 0, head), isJSSpace) + ".. " + strings.TrimLeftFunc(jsSlice(one, -tail, unitLen(one)), isJSSpace)
}

// Chip a long paste collapses to, its edges around the line count
func pasteTokenLabel(text string, lineCount int) string {
	preview := edgePreview(text, 16, 28)
	chip := tr("libText.text.pasteLinesChip", compactNumber(float64(lineCount)))
	if preview == "" {
		return "[[ " + chip + " ]]"
	}
	head, tail, found := strings.Cut(preview, ".. ")
	if found && tail != "" {
		return "[[ " + strings.TrimRightFunc(head, isJSSpace) + ".. " + chip + " .. " + strings.TrimLeftFunc(tail, isJSSpace) + " ]]"
	}
	return "[[ " + preview + " " + chip + " ]]"
}

// Index after which a markdown heading starts on the same line, the boundary a paragraph break goes before
func headingBreaks(s string) string {
	r := []rune(s)
	var b strings.Builder
	for i, c := range r {
		b.WriteRune(c)
		if c == '\n' || i+3 >= len(r) || r[i+1] != '*' || r[i+2] != '*' || r[i+3] == '*' || r[i+3] == '\n' {
			continue
		}
		for j := i + 5; j < len(r); j++ {
			if r[j] == '\n' || r[j-1] == '\n' {
				break
			}
			if r[j-1] == '*' && r[j] == '*' {
				b.WriteString("\n\n")
				break
			}
		}
	}
	return b.String()
}

// Reasoning with the face ticker's status fragments removed and headings set off as paragraphs
func cleanThinkingText(reasoning string) string {
	var kept []string
	for line := range strings.SplitSeq(reasoning, "\n") {
		line = strings.TrimFunc(thinkingStatusChunk.ReplaceAllString(line, ""), isJSSpace)
		if line != "" && !thinkingStatusRe.MatchString(strings.TrimFunc(strings.TrimSuffix(line, "..."), isJSSpace)) {
			kept = append(kept, line)
		}
	}
	out := headingBreaks(strings.Join(kept, "\n"))
	return strings.TrimFunc(newlineRuns.ReplaceAllString(out, "\n\n"), isJSSpace)
}

// Reasoning shown for a thinking mode, the bounded tail cleaned, nothing when collapsed
func thinkingPreview(reasoning, mode string, limit int) string {
	bound := int(liveRenderMaxChars * 1.5)
	bounded := reasoning
	if unitLen(reasoning) > bound {
		bounded = jsSlice(reasoning, -bound, unitLen(reasoning))
	}
	raw := cleanThinkingText(bounded)
	switch {
	case raw == "" || mode == "collapsed":
		return ""
	case mode == "full":
		return raw
	}
	return compactPreview(wsRun.ReplaceAllString(raw, " "), limit)
}

// Live text kept to its tail within the render budget, a label naming what was omitted
func boundedLiveRenderText(text string, maxChars, maxLines int) string {
	return boundedRenderText(text, tr("libText.text.showingLiveTail"), maxChars, maxLines)
}

func boundedRenderText(text, labelPrefix string, maxChars, maxLines int) string {
	u := units(text)
	n := len(u)
	lines := 1
	for _, c := range u {
		if c == '\n' {
			lines++
		}
	}
	if n <= maxChars && lines <= maxLines {
		return text
	}
	lastIndex := func(from int) int {
		for i := min(from, n-1); i >= 0; i-- {
			if u[i] == '\n' {
				return i
			}
		}
		return -1
	}
	start, idx := 0, n
	for seen := 0; seen < maxLines && idx > 0; seen++ {
		idx = lastIndex(idx - 1)
		if idx < 0 {
			start = 0
			break
		}
		start = idx + 1
	}
	lineStart := start
	start = max(lineStart, n-maxChars)
	if start > lineStart {
		for i := start; i < n; i++ {
			if u[i] == '\n' {
				if i < n-1 {
					start = i + 1
				}
				break
			}
		}
	}
	tail := strings.TrimLeftFunc(fromUnits(u[start:]), isJSSpace)
	omittedLines := 0
	for _, c := range u[:start] {
		if c == '\n' {
			omittedLines++
		}
	}
	omittedChars := max(0, n-unitLen(tail))
	label := tr("libText.text.omittedChars", labelPrefix, compactNumber(float64(omittedChars)))
	if omittedLines > 0 {
		label = tr("libText.text.omittedLinesChars", labelPrefix, compactNumber(float64(omittedLines)), compactNumber(float64(omittedChars)))
	}
	return label + "\n" + tail
}

// Paste text without its trailing newlines unless it holds nothing else
func stripTrailingPasteNewlines(text string) string {
	if strings.Trim(text, "\n") == "" {
		return text
	}
	return trailingNewlines.ReplaceAllString(text, "")
}

// Title cased tool name, read_file as Read File
func toolTrailLabel(name string) string {
	var parts []string
	for p := range strings.SplitSeq(name, "_") {
		if p != "" {
			u := units(p)
			parts = append(parts, strings.ToUpper(fromUnits(u[:1]))+fromUnits(u[1:]))
		}
	}
	if label := strings.Join(parts, " "); label != "" {
		return label
	}
	return name
}

// Tool call label with its context preview
func formatToolCall(name, context string) string {
	label := toolTrailLabel(name)
	if preview := compactPreview(context, 64); preview != "" {
		return label + `("` + preview + `")`
	}
	return label
}

// Label the gateway phrased for a bridged call, emoji then text and preview
type toolLabel struct {
	Emoji   string
	Text    string
	Preview string
}

// One row for an already phrased call
func formatToolLabel(label toolLabel) string {
	text := label.Text
	if label.Preview != "" {
		text = label.Text + "  " + label.Preview
	}
	return label.Emoji + " " + compactPreview(text, 72)
}

// First label of a call and how many more it bundles
func formatToolLabels(labels []toolLabel) string {
	if len(labels) == 0 {
		return ""
	}
	if len(labels) > 1 {
		return formatToolLabel(labels[0]) + " +" + strconv.Itoa(len(labels)-1)
	}
	return formatToolLabel(labels[0])
}

// Duration suffix of a trail row, empty without one
func tookSuffix(duration *float64) string {
	if duration == nil {
		return ""
	}
	return " (" + jsToFixed(*duration, 1) + "s)"
}

func resultMark(failed bool) string {
	if failed {
		return "✗"
	}
	return "✓"
}

// Finished trail row of an already phrased call
func toolTrailLine(call string, failed bool, note string, duration *float64) string {
	detail := compactPreview(note, 72)
	line := call + tookSuffix(duration)
	if detail != "" {
		line += " :: " + detail
	}
	return line + " " + resultMark(failed)
}

// Finished trail row of a named call with its context
func buildToolTrailLine(name, context string, failed bool, note string, duration *float64) string {
	return toolTrailLine(formatToolCall(name, context), failed, note, duration)
}

// Labeled block of a verbose row, capped to a small persisted preview
func verboseToolBlock(label, text string) string {
	body := strings.TrimFunc(text, isJSSpace)
	if body == "" {
		return ""
	}
	return label + ":\n" + boundedLiveRenderText(body, verboseTrailMaxChars, verboseTrailMaxLines)
}

// Finished trail row with the Args and Result blocks of an already phrased call
func verboseToolTrailLine(call string, failed bool, duration *float64, argsText, resultText string) string {
	resultLabel := tr("libText.text.resultLabel")
	if failed {
		resultLabel = tr("libText.text.errorLabel")
	}
	var blocks []string
	for _, block := range []string{verboseToolBlock(tr("libText.text.argsLabel"), argsText), verboseToolBlock(resultLabel, resultText)} {
		if block != "" {
			blocks = append(blocks, block)
		}
	}
	line := call + tookSuffix(duration)
	if detail := strings.Join(blocks, "\n"); detail != "" {
		line += " :: " + detail
	}
	return line + " " + resultMark(failed)
}

// Verbose trail row of a named call with its context
func buildVerboseToolTrailLine(name, context string, failed bool, duration *float64, argsText, resultText string) string {
	return verboseToolTrailLine(formatToolCall(name, context), failed, duration, argsText, resultText)
}

// Whether a trail line ends with a result mark
func isToolTrailResultLine(line string) bool {
	return strings.HasSuffix(line, " ✓") || strings.HasSuffix(line, " ✗")
}

// Call, detail and mark of a finished trail row
type toolTrailResult struct {
	Call   string
	Detail string
	Mark   string
}

// Parts of a finished trail row, ok false for a line without a result mark
func parseToolTrailResultLine(line string) (toolTrailResult, bool) {
	if !isToolTrailResultLine(line) {
		return toolTrailResult{}, false
	}
	mark := "✓"
	if strings.HasSuffix(line, " ✗") {
		mark = "✗"
	}
	body := jsSlice(line, 0, -2)
	if call, detail, found := strings.Cut(body, " :: "); found {
		return toolTrailResult{Call: call, Detail: detail, Mark: mark}, true
	}
	if i := strings.Index(body, ": "); i > 0 {
		return toolTrailResult{Call: body[:i], Detail: body[i+2:], Mark: mark}, true
	}
	return toolTrailResult{Call: body, Mark: mark}, true
}

// Call label and its duration suffix
func splitToolDuration(call string) (label, duration string) {
	if m := toolDuration.FindStringSubmatch(call); m != nil {
		return m[1], m[2]
	}
	return call, ""
}

// Whether a trail line is a passing status that a real row replaces
func isTransientTrailLine(line string) bool {
	return strings.HasPrefix(line, "drafting ") || line == "analyzing tool output…"
}

// Whether a trail entry belongs to the tool group of the label
func sameToolTrailGroup(label, entry string) bool {
	return entry == label+" ✓" || entry == label+" ✗" || strings.HasPrefix(entry, label+"(") ||
		strings.HasPrefix(entry, label+" ::") || strings.HasPrefix(entry, label+":")
}

// Index of the last trail line that is not a finished row, minus one when every line is
func lastCotTrailIndex(trail []string) int {
	for i := len(trail) - 1; i >= 0; i-- {
		if !isToolTrailResultLine(trail[i]) {
			return i
		}
	}
	return -1
}

// Rows markdown text takes at a width, fences and table rules sized as they render
func estimateRows(text string, w int, compact bool) int {
	var fenceChar byte
	fenceLen, rows := 0, 0
	for raw := range strings.SplitSeq(text, "\n") {
		line := stripAnsi(raw)
		if m := estimateFence.FindStringSubmatch(line); m != nil {
			marker, lang := m[1], strings.TrimFunc(m[2], isJSSpace)
			switch {
			case fenceLen == 0:
				fenceChar, fenceLen = marker[0], len(marker)
				if lang != "" {
					rows += int(math.Ceil(float64(max(unitLen("─ "+lang), 1)) / float64(w)))
				}
			case marker[0] == fenceChar && len(marker) >= fenceLen:
				fenceLen = 0
			}
			continue
		}
		inCode := fenceLen > 0
		trimmed := strings.TrimFunc(line, isJSSpace)
		if !inCode && strings.HasPrefix(trimmed, "|") && tableRule.MatchString(trimmed) {
			continue
		}
		rendered := line
		if !inCode {
			rendered = renderEstimateLine(line)
		}
		if compact && strings.TrimFunc(rendered, isJSSpace) == "" {
			continue
		}
		rows += int(math.Ceil(float64(max(unitLen(rendered), 1)) / float64(w)))
	}
	return max(1, rows)
}

// Whether text carries a paste reference token
func isPasteBackedText(text string) bool { return pasteBacked.MatchString(text) }

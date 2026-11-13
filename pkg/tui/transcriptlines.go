package tui

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// Progress of the stable prefix scan over a streaming reply, the settled blocks and open fences
type streamScanState struct {
	blocks     []string
	codeOpen   bool
	mathOpener string
	scanned    string
	settledLen int
}

var (
	codeFenceRe     = regexp.MustCompile("^(?:`{3,}|~{3,})")
	mathDollarOpen  = regexp.MustCompile(`^\$\$`)
	mathDollarClose = regexp.MustCompile(`\$\$$`)
	mathBracketOpen = regexp.MustCompile(`^\\\[`)
	mathBracketEnd  = regexp.MustCompile(`\\\]$`)
)

// Fresh scan with nothing settled
func createScanState() *streamScanState { return &streamScanState{} }

// Fence state after one trimmed line, code fences toggling and display math opening or closing
func (st *streamScanState) applyLine(line string) {
	if codeFenceRe.MatchString(line) {
		st.codeOpen = !st.codeOpen
		return
	}
	if st.codeOpen {
		return
	}
	switch {
	case st.mathOpener == "":
		if mathDollarOpen.MatchString(line) && !(unitLen(line) >= 4 && mathDollarClose.MatchString(line)) {
			st.mathOpener = "$$"
		} else if mathBracketOpen.MatchString(line) && !mathBracketEnd.MatchString(line) {
			st.mathOpener = `\[`
		}
	case st.mathOpener == "$$" && mathDollarClose.MatchString(line):
		st.mathOpener = ""
	case st.mathOpener == `\[` && mathBracketEnd.MatchString(line):
		st.mathOpener = ""
	}
}

// Scans the complete lines added since the last call, settling each block a blank line ends outside fences
func (st *streamScanState) advanceScan(text string) {
	start := len(st.scanned)
	i := start
	for i < len(text) {
		nl := strings.IndexByte(text[i:], '\n')
		if nl < 0 {
			break
		}
		nl += i
		if nl == i {
			if i > 0 && !st.codeOpen && st.mathOpener == "" {
				block := text[st.settledLen : nl+1]
				if strings.IndexFunc(block, func(r rune) bool { return !isJSSpace(r) }) >= 0 {
					st.blocks = append(st.blocks, block)
					st.settledLen = nl + 1
				}
			}
		} else {
			st.applyLine(strings.TrimFunc(text[i:nl], isJSSpace))
		}
		i = nl + 1
	}
	if i > start {
		st.scanned += text[start:i]
	}
}

// End of the settled prefix of a streaming reply, minus one when nothing settled
func findStableBoundary(text string) int {
	st := createScanState()
	st.advanceScan(text)
	if st.settledLen > 0 {
		return st.settledLen
	}
	return -1
}

// Local hour and minute stamp of a message, empty when its time is missing or invalid
func fmtMsgTimestamp(createdAt *float64) (string, bool) {
	if createdAt == nil || math.IsNaN(*createdAt) || math.IsInf(*createdAt, 0) || *createdAt <= 0 {
		return "", false
	}
	ms := *createdAt * 1000
	if math.Abs(ms) > 8.64e15 {
		return "", false
	}
	t := time.UnixMilli(int64(math.Trunc(ms))).In(clockLocation())
	return fmt.Sprintf("[%02d:%02d]", t.Hour(), t.Minute()), true
}

// Whether an assistant reply with text gets the separator above it while details show
func shouldShowResponseSeparator(m msg, showDetails bool) bool {
	return m.Role == "assistant" && showDetails && strings.IndexFunc(m.Text, func(r rune) bool { return !isJSSpace(r) }) >= 0
}

// Whether the thinking trail of a message renders, always for a mixture reference
func shouldShowThinkingTrail(m msg, thinkingMode, toolsMode, activityMode string) bool {
	return m.IsMoaReference || thinkingMode != "hidden" || toolsMode != "hidden" || activityMode != "hidden"
}

// Theme tone of a filled gauge run
func gaugeThemeColor(t theme, style string) string {
	switch style {
	case "red":
		return t.Color.Error
	case "yellow":
		return t.Color.Warn
	}
	return t.Color.Ok
}

// Whether a plain one line system message is a usage row with a gauge bar
func isUsageGaugeRow(m msg) bool {
	return m.Role == "system" && m.Kind == "" && !strings.Contains(m.Text, "\n") && hasUsageGauge(m.Text)
}

// Usage row with its gauge runs colored and the rest muted
func usageGaugeRowView(t theme, text string) string {
	var b strings.Builder
	for _, seg := range splitUsageGaugeLine(stripAnsi(text)) {
		tone := t.Color.Muted
		if seg.Style != "" {
			tone = gaugeThemeColor(t, seg.Style)
		}
		b.WriteString(paint(seg.Text, tone, true))
	}
	return b.String()
}

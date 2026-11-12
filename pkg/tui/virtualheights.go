package tui

import (
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
)

const (
	termuxPrefix      = "/data/data/com.termux/files/usr"
	maxEstimateLines  = 800
	blankScanUnits    = 16_000
	maxBlankBlockRows = 6
)

var (
	blankBlock    = regexp.MustCompile(`\n[` + jsWS + `]*\n`)
	truthyToggle  = regexp.MustCompile(`(?i)^(?:1|true|yes|on)$`)
	termuxTUIMode = isTermuxTUIMode(os.Getenv)
)

// Section of a slash command panel row
type panelSection struct {
	Items []string
	Rows  [][2]string
	Text  *string
	Title *string
}

// Panel a slash command prints into the transcript
type panelData struct {
	Sections []panelSection
	Title    string
}

// Whether the process runs inside Termux, by its version variable or install prefix
func isTermuxEnv(env func(string) string) bool {
	return env("TERMUX_VERSION") != "" || strings.Contains(env("PREFIX"), termuxPrefix)
}

// Whether Termux layout defaults apply, on inside Termux unless ICEBATH_TUI_TERMUX_MODE turns them off
func isTermuxTUIMode(env func(string) string) bool {
	if !isTermuxEnv(env) {
		return false
	}
	if override := strings.ToLower(strings.TrimFunc(env("ICEBATH_TUI_TERMUX_MODE"), isJSSpace)); override != "" {
		return truthyToggle.MatchString(override)
	}
	return true
}

// Base 36 djb2 xor hash over UTF-16 units with JavaScript 32 bit wrapping
func hashText(text string) string {
	h := int64(5381)
	for _, c := range units(text) {
		shifted := int64(int32(uint32(h) << 5))
		h = int64(int32(uint32(shifted+h))) ^ int64(c)
		h = int64(int32(uint32(h)))
	}
	return strconv.FormatUint(uint64(uint32(h)), 36)
}

// Cache key of a message's rendered height, stable across equal message values
func messageHeightKey(m msg) string {
	var todoSig []string
	for _, t := range m.Todos {
		todoSig = append(todoSig, t.Status+":"+t.Content)
	}
	var panelSig []string
	if m.PanelData != nil {
		for _, s := range m.PanelData.Sections {
			title, textLen := "", 0
			if s.Title != nil {
				title = *s.Title
			}
			if s.Text != nil {
				textLen = unitLen(*s.Text)
			}
			panelSig = append(panelSig, title+":"+strconv.Itoa(textLen)+":"+strconv.Itoa(len(s.Items))+":"+strconv.Itoa(len(s.Rows)))
		}
	}
	introSig := ""
	if m.Kind == "intro" {
		if v, has := m.Info["version"]; has && v != nil {
			introSig = jsString(v)
		}
	}
	body := strings.Join([]string{m.Text, m.Thinking, strings.Join(m.Tools, "\n"), strings.Join(todoSig, "\u0001"), strings.Join(panelSig, "\u0001"), introSig}, "\x00")
	return m.Role + ":" + m.Kind + ":" + hashText(body)
}

// Wrapped row count of text at a width, counting stops at the row cap
func wrappedLines(text string, width, maxLines int) int {
	u := units(text)
	w := max(1, width)
	budget := min(len(u), maxLines*w+maxLines)
	n, start := 0, 0
	for i := 0; i <= budget; i++ {
		if i == len(u) || i == budget || u[i] == '\n' {
			rows := max(1, int(math.Ceil(float64(i-start)/float64(w))))
			if rows >= maxLines-n {
				n += maxLines - n
			} else {
				n += rows
			}
			start = i + 1
			if n >= maxLines {
				return maxLines
			}
		}
	}
	return n
}

// Inputs of a height estimate, nil visibility flags following details and thinking visibility
type heightOpts struct {
	Compact          bool
	Details          bool
	LeadGap          bool
	ThinkingExpanded *bool
	ThinkingVisible  *bool
	ToolsVisible     *bool
	UserPrompt       string
	WithSeparator    bool
}

// Whether text has a character that is not JavaScript whitespace
func hasNonSpace(s string) bool { return strings.TrimFunc(s, isJSSpace) != "" }

// Rows a message takes before measurement, from wrapping, details, separators and gaps
func estimatedMsgHeight(m msg, cols int, o heightOpts) int {
	thinkingVisible := o.Details
	if o.ThinkingVisible != nil {
		thinkingVisible = *o.ThinkingVisible
	}
	thinkingExpanded := thinkingVisible
	if o.ThinkingExpanded != nil {
		thinkingExpanded = *o.ThinkingExpanded
	}
	toolsVisible := o.Details
	if o.ToolsVisible != nil {
		toolsVisible = *o.ToolsVisible
	}
	switch {
	case m.Kind == "intro":
		if truthyJS(m.Info["version"]) {
			return 9
		}
		return 5
	case m.Kind == "panel":
		sections := 1
		if m.PanelData != nil {
			sections = len(m.PanelData.Sections)
		}
		return max(3, sections*2+1)
	case m.Kind == "trail" && len(m.Todos) > 0:
		if m.TodoCollapsedByDefault {
			return 2
		}
		return max(2, len(m.Todos)+2)
	}
	bodyWidth := transcriptBodyWidth(cols, m.Role, o.UserPrompt, termuxTUIMode)
	text := m.Text
	if text == "" {
		text = " "
	}
	h := wrappedLines(text, bodyWidth, maxEstimateLines)
	if !o.Compact && m.Role == "assistant" {
		scan := m.Text
		if unitLen(scan) > blankScanUnits {
			scan = jsSlice(scan, 0, blankScanUnits)
		}
		h += min(maxBlankBlockRows, len(blankBlock.FindAllStringIndex(scan, -1)))
	}
	if o.Details {
		visibleTools := toolsVisible && len(m.Tools) > 0
		visibleThinking := thinkingVisible && hasNonSpace(m.Thinking)
		if visibleTools || visibleThinking {
			if visibleTools {
				h += len(m.Tools)
			}
			if visibleThinking {
				if thinkingExpanded {
					h += wrappedLines(m.Thinking, bodyWidth, maxEstimateLines)
				} else {
					h++
				}
			}
			if m.Role == "assistant" && hasNonSpace(m.Text) {
				h += 2
			}
		}
	}
	switch {
	case m.Role == "user" || m.Kind == "diff":
		h += 2
	case m.Kind == "slash":
		h++
	}
	if o.LeadGap {
		h++
	}
	if o.WithSeparator {
		h += 2
	}
	return max(1, h)
}

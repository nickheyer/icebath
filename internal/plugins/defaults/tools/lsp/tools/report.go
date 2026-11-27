package tools

import (
	"fmt"
	"html"
	"slices"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/internal/contexts"
	"github.com/nickheyer/icebath/internal/tools/diff"
)

// Caps of a file's block: errors listed, characters of the whole note, and of each field a server controls
const (
	maxPerFile    = 20
	maxTotalChars = 4000
	maxMessage    = 300
	maxCode       = 80
	maxSource     = 80
)

var severities = map[int]string{1: "ERROR", 2: "WARN", 3: "INFO", 4: "HINT"}

// Field of a server made safe to show the model: on one line, without what doesn't print, capped and escaped
func sanitize(s string, limit int) string {
	s = strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
	s = strings.Map(func(r rune) rune {
		if r == ' ' || unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s)
	if runes := []rune(strings.TrimSpace(s)); len(runes) > limit {
		s = string(runes[:limit])
	} else {
		s = string(runes)
	}
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// One diagnostic on one line, positions counted from 1
func (d diagnostic) format() string {
	severity, ok := severities[d.severity()]
	if !ok {
		severity = "ERROR"
	}
	out := fmt.Sprintf("%s [%d:%d] %s", severity, d.Range.Start.Line+1, d.Range.Start.Character+1, sanitize(string(d.Message), maxMessage))
	if code := sanitize(string(d.Code), maxCode); code != "" {
		out += " [" + code + "]"
	}
	if source := sanitize(string(d.Source), maxSource); source != "" {
		out += " (" + source + ")"
	}
	return out
}

// Block of a file's errors, empty when none of error severity is left
func block(path string, diags []diagnostic) string {
	var errs []diagnostic
	for _, d := range diags {
		if d.severity() == 1 {
			errs = append(errs, d)
		}
	}
	if len(errs) == 0 {
		return ""
	}
	shown := errs[:min(len(errs), maxPerFile)]
	lines := make([]string, len(shown))
	for i, d := range shown {
		lines[i] = d.format()
	}
	body := strings.Join(lines, "\n")
	if len(errs) > maxPerFile {
		body += fmt.Sprintf("\n... and %d more", len(errs)-maxPerFile)
	}
	return "<diagnostics file=\"" + html.EscapeString(path) + "\">\n" + body + "\n</diagnostics>"
}

// Header and block of a file's errors cut to the note's cap, empty when there are none
func note(path string, diags []diagnostic, header string) string {
	b := block(path, diags)
	if b == "" {
		return ""
	}
	return contexts.Elide(header+b, maxTotalChars)
}

// Line an earlier line of a text moved to, false when it was deleted
type shift func(line int) (int, bool)

// Where each line of the text before an edit went in the text after, from the matching of their lines
func lineShift(before, after string) shift {
	pre, post := lines(before), lines(after)
	if slices.Equal(pre, post) {
		return func(line int) (int, bool) { return line, true }
	}
	opcodes := diff.New[string](nil, pre, post, false).Opcodes()
	return func(line int) (int, bool) {
		for _, op := range opcodes {
			if op.I1 <= line && line < op.I2 {
				if op.Tag == "equal" {
					return line - op.I1 + op.J1, true
				}
				return 0, false
			}
			if line < op.I1 {
				break
			}
		}
		if len(post) > 0 {
			return len(post) - 1, true
		}
		return 0, false
	}
}

// Diagnostics of the text before an edit moved to the lines they sit on after it, those on deleted lines dropped
func shifted(baseline []diagnostic, moved shift) []diagnostic {
	out := []diagnostic{}
	for _, d := range baseline {
		start, ok := moved(d.Range.Start.Line)
		if !ok {
			continue
		}
		end, ok := moved(d.Range.End.Line)
		if !ok {
			end = start
		}
		d.Range.Start.Line, d.Range.End.Line = start, end
		out = append(out, d)
	}
	return out
}

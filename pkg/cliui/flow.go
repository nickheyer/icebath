package cliui

import (
	"bytes"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// What the last finished line of a flow was, which decides the spacing of the next one
type lineKind int

const (
	kindNone lineKind = iota
	kindText
	kindBlank
	kindAnswer
)

// Left margin every setup line starts at
const flowMargin = 2

// Writer giving setup output one left margin, single blank lines, brand headers, colored status marks and word wrap
type flowWriter struct {
	mu         sync.Mutex
	c          *Console
	w          io.Writer
	line       []byte
	streaming  bool
	dirty      bool
	last       lineKind
	owedBlank  bool
	busy       *busySpinner
	graceMu    sync.Mutex
	graceTimer *time.Timer
}

// Writer under a flow writer, the writer itself otherwise
func rawWriter(w io.Writer) io.Writer {
	if f, ok := w.(*flowWriter); ok {
		return f.w
	}
	return w
}

// Flow writer a console prints through, nil when it prints straight to its output
func (c *Console) flow() *flowWriter {
	f, _ := c.Out.(*flowWriter)
	return f
}

// Flow returns a console whose prints pass through the setup presentation, the receiver itself off a terminal
func (c *Console) Flow() *Console {
	if c.flow() != nil || !c.interactive() {
		return c
	}
	base := c
	return &Console{In: c.In, Out: &flowWriter{c: base, w: c.Out}, Getenv: c.Getenv,
		StdinTTY: c.stdinTTY, StdoutTTY: c.stdoutTTY, TermSize: c.termSize,
		MaskedSecretPrompt: c.MaskedSecretPrompt, OpenScreen: c.OpenScreen}
}

// IsFlow reports whether prints on this console pass through the setup presentation
func (c *Console) IsFlow() bool { return c.flow() != nil }

func (f *flowWriter) Write(p []byte) (int, error) {
	f.stopBusy()
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []byte
	for _, b := range p {
		switch {
		case b == '\n' && f.streaming:
			out = append(out, '\n')
			f.streaming, f.dirty, f.last = false, false, kindText
		case b == '\n':
			out = f.finish(out)
		case f.streaming && b == '\r':
			out = append(append(out, '\r'), strings.Repeat(" ", flowMargin)...)
		case f.streaming:
			out = append(out, b)
		default:
			f.line = append(f.line, b)
		}
	}
	if len(f.line) > 0 && utf8.Valid(f.line) && !bytes.HasSuffix(f.line, []byte{0x1b}) {
		out = f.stream(out)
	}
	if len(out) > 0 {
		if _, err := f.w.Write(out); err != nil {
			return 0, err
		}
		f.grace()
	}
	return len(p), nil
}

// Shows the start of a line that arrived without its end, the rest then passing through as it comes
func (f *flowWriter) stream(out []byte) []byte {
	indent, text := splitIndent(overwritten(string(f.line)))
	if ansi.Strip(text) == "" {
		return out
	}
	f.line = nil
	out = f.blankBefore(out, false)
	out = append(append(out, strings.Repeat(" ", max(indent, flowMargin))...), text...)
	f.streaming, f.dirty = true, true
	return out
}

// Text after the last carriage return of a line, what a terminal would leave visible
func overwritten(line string) string {
	line = strings.TrimSuffix(line, "\r")
	if i := strings.LastIndexByte(line, '\r'); i >= 0 {
		return line[i+1:]
	}
	return line
}

// Leading spaces of a line counted through its leading color codes, and the line without those spaces
func splitIndent(line string) (int, string) {
	indent := 0
	var lead strings.Builder
	rest := line
	for len(rest) > 0 {
		switch {
		case rest[0] == ' ':
			indent++
			rest = rest[1:]
		case rest[0] == '\t':
			indent += 4
			rest = rest[1:]
		case rest[0] == 0x1b:
			end := escapeLen(rest)
			if end == 0 {
				return indent, lead.String() + rest
			}
			lead.WriteString(rest[:end])
			rest = rest[end:]
		default:
			return indent, lead.String() + rest
		}
	}
	return indent, lead.String()
}

// Length of the escape sequence a string starts with, zero when it is cut short
func escapeLen(s string) int {
	if len(s) < 2 {
		return 0
	}
	switch s[1] {
	case '[':
		for i := 2; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7e {
				return i + 1
			}
		}
		return 0
	case ']':
		for i := 2; i < len(s); i++ {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == '\\' && s[i-1] == 0x1b {
				return i + 1
			}
		}
		return 0
	}
	return 2
}

// Writes the blank line owed after an answer, and one before a header that follows other lines
func (f *flowWriter) blankBefore(out []byte, header bool) []byte {
	if f.owedBlank || (header && (f.last == kindText || f.last == kindAnswer)) {
		out = append(out, '\n')
		f.last = kindBlank
	}
	f.owedBlank = false
	return out
}

var (
	ruleTitleRe = regexp.MustCompile(`^(?:-{3,}|─{3,})\s+(.+?)\s+(?:-{3,}|─{3,})$`)
	bareRuleRe  = regexp.MustCompile(`^[-─━═=]{10,}$`)
	drawingRe   = regexp.MustCompile(`[\x{2500}-\x{259F}]`)
)

// Ends the buffered line, styled by what it opens with and wrapped to the terminal
func (f *flowWriter) finish(out []byte) []byte {
	indent, text := splitIndent(overwritten(string(f.line)))
	f.line = nil
	plain := strings.TrimRight(ansi.Strip(text), " ")
	if plain == "" {
		switch f.last {
		case kindNone, kindBlank:
		case kindAnswer:
			f.owedBlank = true
		default:
			out = append(out, '\n')
			f.last = kindBlank
		}
		return out
	}
	g, t, c := f.c.glyphs(), activeTheme(), f.c
	margin := strings.Repeat(" ", flowMargin)
	first, size := utf8.DecodeRuneInString(plain)
	after := strings.TrimLeft(plain[size:], " ")
	gap := len(plain[size:]) - len(after)
	var rendered string
	header := false
	switch {
	case first == '◆' || first == '☤':
		header = true
		rendered = margin + c.paint(g.header, textStyle{color: t.Accent}) + " " + c.paint(after, textStyle{bold: true})
	case first == '✓' || first == '✔':
		rendered = f.marked(indent, gap, g.ok, t.OK, after)
	case first == '⚠':
		rendered = f.marked(indent, gap, g.warn, t.Warn, after)
	case first == '✗' || first == '✖':
		rendered = f.marked(indent, gap, g.cross, t.Err, after)
	case ruleTitleRe.MatchString(plain):
		header = true
		title := ruleTitleRe.FindStringSubmatch(plain)[1]
		rendered = margin + c.paint(strings.Repeat(g.rule, 2), textStyle{color: t.Faint}) + " " + c.paint(title, textStyle{bold: true})
	case bareRuleRe.MatchString(plain):
		_, cols := f.size()
		width := min(len([]rune(plain)), cols-flowMargin-1)
		rendered = margin + c.paint(strings.Repeat(g.rule, max(width, 1)), textStyle{color: t.Faint})
	default:
		rendered = f.wrapped(max(indent, flowMargin), f.retone(text))
	}
	out = f.blankBefore(out, header)
	out = append(append(out, rendered...), '\n')
	f.last = kindText
	return out
}

var sgrRe = regexp.MustCompile(`\x1b\[([0-9;]*)m`)

// Text with its basic terminal colors swapped for the matching tones of the palette
func (f *flowWriter) retone(text string) string {
	t := activeTheme()
	tones := map[string]string{"31": t.Err, "32": t.OK, "33": t.Warn, "34": t.Frost, "35": t.Accent, "36": t.Frost}
	return sgrRe.ReplaceAllStringFunc(text, func(seq string) string {
		params := strings.Split(seq[2:len(seq)-1], ";")
		for i, param := range params {
			if param == "38" || param == "48" {
				return seq
			}
			if code := f.c.fg(tones[param]); code != "" {
				params[i] = code
			}
		}
		return "\x1b[" + strings.Join(params, ";") + "m"
	})
}

// Terminal rows and columns, eighty by twenty four when the size is unknown
func (f *flowWriter) size() (int, int) {
	rows, cols, err := f.c.termSize()
	if err != nil || rows <= 0 || cols <= 0 {
		return 24, 80
	}
	return rows, cols
}

// Status line with its mark colored and its nesting kept as indentation before the mark
func (f *flowWriter) marked(indent, gap int, mark, tone, text string) string {
	depth := max(indent, flowMargin) + max(gap-1, 0)
	_, cols := f.size()
	hang := strings.Repeat(" ", depth+ansi.StringWidth(mark)+1)
	rows := wrapWords(text, max(cols-1-len(hang), 20))
	return strings.Repeat(" ", depth) + f.c.paint(mark, textStyle{color: tone}) + " " + strings.Join(rows, "\n"+hang)
}

// Text at an indent, broken at word gaps to fit the terminal with the same indent on every row
func (f *flowWriter) wrapped(indent int, text string) string {
	_, cols := f.size()
	pad := strings.Repeat(" ", indent)
	limit := cols - 1 - indent
	if limit < 20 || ansi.StringWidth(text) <= limit || drawingRe.MatchString(text) {
		return pad + text
	}
	return pad + strings.Join(wrapWords(text, limit), "\n"+pad)
}

// Ends an unfinished line and reports what the last line was, for a prompt about to draw
func (f *flowWriter) settle() lineKind {
	f.mu.Lock()
	defer f.mu.Unlock()
	if out := f.stream(nil); len(out) > 0 {
		f.w.Write(out)
	}
	f.line = nil
	if f.dirty {
		io.WriteString(f.w, "\r\n")
		f.last = kindText
	}
	f.streaming, f.dirty, f.owedBlank = false, false, false
	return f.last
}

// Records the kind of line a prompt left as the last one
func (f *flowWriter) note(kind lineKind) {
	f.mu.Lock()
	f.last = kind
	f.mu.Unlock()
}

// Banner prints brand art above a setup, the wide art when the terminal fits it and the compact line otherwise
func (c *Console) Banner(wide []string, compact string, tagline string) {
	_, cols, err := c.termSize()
	if err != nil || cols <= 0 {
		cols = 80
	}
	out := rawWriter(c.Out)
	lines := []string{compact}
	fits := len(wide) > 0
	for _, line := range wide {
		if ansi.StringWidth(line)+flowMargin >= cols {
			fits = false
		}
	}
	if fits {
		lines = wide
	}
	margin := strings.Repeat(" ", flowMargin)
	io.WriteString(out, "\n")
	for _, line := range lines {
		io.WriteString(out, margin+line+"\n")
	}
	if tagline != "" {
		io.WriteString(out, "\n"+margin+c.paint(c.clip(tagline, cols-flowMargin-1), textStyle{color: activeTheme().Muted})+"\n")
	}
	if f := c.flow(); f != nil {
		f.settle()
		f.note(kindText)
	}
}

// Step prints the brand rule that opens one part of a setup, numbered when the setup has several
func (c *Console) Step(index, total int, label string) {
	g, t := c.glyphs(), activeTheme()
	out := rawWriter(c.Out)
	if f := c.flow(); f != nil {
		if last := f.settle(); last != kindBlank && last != kindNone {
			io.WriteString(out, "\n")
		}
	}
	bar := strings.Repeat(g.rule, 6) + g.slash + strings.Repeat(g.rule, 3)
	line := strings.Repeat(" ", flowMargin) + c.paint(bar, textStyle{color: t.Accent})
	if total > 1 {
		line += "  " + c.paint(strconv.Itoa(index)+" / "+strconv.Itoa(total), textStyle{color: t.Muted})
	}
	line += "  " + c.paint(label, textStyle{bold: true})
	io.WriteString(out, line+"\n")
	if f := c.flow(); f != nil {
		f.note(kindText)
	}
}

// Done prints the brand rule that closes a setup, a check beside the label
func (c *Console) Done(label string) {
	g, t := c.glyphs(), activeTheme()
	out := rawWriter(c.Out)
	if f := c.flow(); f != nil {
		if last := f.settle(); last != kindBlank && last != kindNone {
			io.WriteString(out, "\n")
		}
	}
	bar := strings.Repeat(g.rule, 6) + g.slash + strings.Repeat(g.rule, 3)
	io.WriteString(out, strings.Repeat(" ", flowMargin)+c.paint(bar, textStyle{color: t.Accent})+"  "+
		c.paint(g.ok, textStyle{color: t.OK, bold: true})+" "+c.paint(label, textStyle{bold: true})+"\n")
	if f := c.flow(); f != nil {
		f.note(kindText)
	}
}

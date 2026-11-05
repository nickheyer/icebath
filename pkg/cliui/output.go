package cliui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/nickheyer/icebath/pkg/i18n"
)

func (c *Console) println(text string) { fmt.Fprintln(c.Out, text) }

// PrintInfo prints a dim indented line
func (c *Console) PrintInfo(text string) { c.println(c.Color("  "+text, Dim)) }

// PrintSuccess prints a green check line
func (c *Console) PrintSuccess(text string) { c.println(c.Color("✓ "+text, Green)) }

// PrintWarning prints a yellow warning line
func (c *Console) PrintWarning(text string) { c.println(c.Color("⚠ "+text, Yellow)) }

// PrintError prints a red cross line
func (c *Console) PrintError(text string) { c.println(c.Color("✗ "+text, Red)) }

// PrintHeader prints a yellow header after a blank line
func (c *Console) PrintHeader(text string) { c.println(c.Color("\n  "+text, Yellow)) }

// PrintTruncated prints the footer of a capped listing, nil more meaning at least one hidden row
func (c *Console) PrintTruncated(ctx context.Context, more *int, hint string) {
	count := i18n.T(ctx, "cli.shared.more_not_shown", "", nil)
	if more != nil {
		count = i18n.T(ctx, "cli.shared.n_more", "", map[string]any{"count": strconv.Itoa(*more)})
	}
	suffix := ""
	if hint != "" {
		suffix = " (" + hint + ")"
	}
	c.println(c.Color("  … "+count+suffix, Dim))
}

// LineInput reads text with cursor editing keys on a terminal and plain input otherwise
func (c *Console) LineInput(prompt string) (string, error) {
	if !(c.stdinTTY() && c.stdoutTTY()) {
		return c.input(prompt)
	}
	text, _, err := c.editLine(prompt, nil)
	if errors.Is(err, errEditorUnavailable) {
		return c.input(prompt)
	}
	return text, err
}

// Prompt reads a stripped answer or the default on plain Enter, empty after printing a newline on Ctrl-C or EOF
func (c *Console) Prompt(question, def string, password bool) string {
	suffix := ""
	if def != "" {
		suffix = " [" + def + "]"
	}
	display := c.Color("  "+question+suffix+": ", Yellow)
	var value string
	var err error
	if password {
		value, err = c.secretPrompt()(display)
	} else {
		value, err = c.LineInput(display)
	}
	if err != nil {
		if errors.Is(err, ErrInterrupt) || errors.Is(err, ErrEOF) {
			c.println("")
			return ""
		}
		panic(err)
	}
	value = strings.TrimSpace(value)
	if value != "" {
		return value
	}
	return def
}

// PromptYesNo asks a localized yes or no question, accepting y or the localized affirmative initial
func (c *Console) PromptYesNo(ctx context.Context, question string, def bool) bool {
	hint := i18n.T(ctx, "cli.shared.yes_no_default_no", "", nil)
	if def {
		hint = i18n.T(ctx, "cli.shared.yes_no_default_yes", "", nil)
	}
	answer := c.Prompt(question+" ("+hint+")", "", false)
	if answer == "" {
		return def
	}
	yes := strings.ToLower(strings.TrimSpace(i18n.T(ctx, "cli.shared.yes_initial", "", nil)))
	low := strings.ToLower(answer)
	return strings.HasPrefix(low, "y") || (yes != "" && strings.HasPrefix(low, yes))
}

var errEditorUnavailable = errors.New("line editor unavailable")

// Navigation result of a scoped numbered prompt key binding
type numberedNav int

const (
	navNone numberedNav = iota
	navCancelKey
	navBackKey
)

// Bindings that end a scoped numbered prompt early
type editorBindings struct {
	cancel bool
	back   bool
}

// Raw mode emacs style line editor, returning the line, a binding result, or an interrupt or EOF error
func (c *Console) editLine(prompt string, binds *editorBindings) (string, numberedNav, error) {
	restore, err := c.makeRaw()
	if err != nil {
		return "", navNone, errEditorUnavailable
	}
	defer restore()
	io.WriteString(c.Out, "\x1b[?2004h")
	defer io.WriteString(c.Out, "\x1b[?2004l")
	ed := &lineEditor{c: c, prompt: prompt}
	ed.render()
	dec := &uv.EventDecoder{}
	var buf []byte
	pasting := false
	for {
		if len(buf) == 0 {
			b, err := c.readByte(-1, nil)
			if err != nil {
				return "", navNone, err
			}
			buf = append(buf, b)
		}
		buf = c.gatherBurst(buf)
		n, ev := dec.Decode(buf)
		if n <= 0 {
			n = 1
		}
		buf = buf[n:]
		switch e := ev.(type) {
		case uv.PasteStartEvent:
			pasting = true
			continue
		case uv.PasteEndEvent:
			pasting = false
			continue
		case uv.PasteEvent:
			ed.insert(strings.ReplaceAll(strings.ReplaceAll(e.Content, "\r\n", "\n"), "\r", "\n"))
		case uv.KeyPressEvent:
			k := uv.Key(e)
			if pasting {
				switch {
				case k.Code == uv.KeyEnter:
					ed.insert("\n")
				case k.Text != "":
					ed.insert(k.Text)
				}
				ed.render()
				continue
			}
			if binds != nil {
				if binds.cancel && ((k.Code == uv.KeyEscape && k.Mod == 0) || (k.Code == 'c' && k.Mod&uv.ModCtrl != 0)) {
					ed.finish()
					return "", navCancelKey, nil
				}
				if binds.back && k.Code == uv.KeyLeft && k.Mod == 0 {
					ed.finish()
					return "", navBackKey, nil
				}
			}
			done, err := ed.key(k)
			if err != nil {
				ed.finish()
				return "", navNone, err
			}
			if done {
				ed.finish()
				return string(ed.buf), navNone, nil
			}
		}
		ed.render()
	}
}

// Collects bytes that belong to the same key burst so escape sequences decode whole
func (c *Console) gatherBurst(buf []byte) []byte {
	for {
		wait := time.Duration(0)
		if incompleteEscape(buf) {
			wait = 50 * time.Millisecond
		}
		b, err := c.readByte(wait, nil)
		if err != nil {
			return buf
		}
		buf = append(buf, b)
	}
}

// Reports whether the buffer ends inside an escape sequence
func incompleteEscape(buf []byte) bool {
	i := len(buf) - 1
	for i >= 0 && buf[i] != 0x1b {
		i--
	}
	if i < 0 {
		return false
	}
	tail := buf[i:]
	if len(tail) == 1 {
		return true
	}
	if tail[1] != '[' && tail[1] != 'O' {
		return false
	}
	if len(tail) == 2 {
		return true
	}
	last := tail[len(tail)-1]
	if tail[1] == 'O' {
		return false
	}
	return !(last >= 0x40 && last <= 0x7e) || (len(tail) == 3 && last == '[')
}

type lineEditor struct {
	c      *Console
	prompt string
	buf    []rune
	pos    int
	kill   string
}

// Redraws the prompt and buffer on the current line and places the cursor
func (e *lineEditor) render() {
	text := string(e.buf)
	before := string(e.buf[:e.pos])
	out := "\r\x1b[K" + e.prompt + text
	back := ansi.StringWidth(text) - ansi.StringWidth(before)
	if back > 0 {
		out += fmt.Sprintf("\x1b[%dD", back)
	}
	io.WriteString(e.c.Out, out)
}

func (e *lineEditor) finish() { io.WriteString(e.c.Out, "\r\n") }

func (e *lineEditor) insert(s string) {
	rs := []rune(s)
	e.buf = append(e.buf[:e.pos], append(rs, e.buf[e.pos:]...)...)
	e.pos += len(rs)
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }

func (e *lineEditor) wordStartBefore(p int) int {
	for p > 0 && !isWordRune(e.buf[p-1]) {
		p--
	}
	for p > 0 && isWordRune(e.buf[p-1]) {
		p--
	}
	return p
}

func (e *lineEditor) wordEndAfter(p int) int {
	for p < len(e.buf) && !isWordRune(e.buf[p]) {
		p++
	}
	for p < len(e.buf) && isWordRune(e.buf[p]) {
		p++
	}
	return p
}

func (e *lineEditor) cut(from, to int) {
	e.kill = string(e.buf[from:to])
	e.buf = append(e.buf[:from], e.buf[to:]...)
	e.pos = from
}

// Applies one key with prompt_toolkit emacs bindings, true when Enter accepts the line
func (e *lineEditor) key(k uv.Key) (bool, error) {
	ctrl, alt := k.Mod&uv.ModCtrl != 0, k.Mod&uv.ModAlt != 0
	switch {
	case k.Code == uv.KeyEnter || (ctrl && (k.Code == 'm' || k.Code == 'j')):
		return true, nil
	case ctrl && k.Code == 'c':
		return false, ErrInterrupt
	case ctrl && k.Code == 'd':
		if len(e.buf) == 0 {
			return false, ErrEOF
		}
		if e.pos < len(e.buf) {
			e.buf = append(e.buf[:e.pos], e.buf[e.pos+1:]...)
		}
	case (ctrl && k.Code == 'a') || k.Code == uv.KeyHome:
		e.pos = 0
	case (ctrl && k.Code == 'e') || k.Code == uv.KeyEnd:
		e.pos = len(e.buf)
	case (ctrl && k.Code == 'b') || (k.Code == uv.KeyLeft && !ctrl && !alt):
		if e.pos > 0 {
			e.pos--
		}
	case (ctrl && k.Code == 'f') || (k.Code == uv.KeyRight && !ctrl && !alt):
		if e.pos < len(e.buf) {
			e.pos++
		}
	case (alt && k.Code == 'b') || (k.Code == uv.KeyLeft && (ctrl || alt)):
		e.pos = e.wordStartBefore(e.pos)
	case (alt && k.Code == 'f') || (k.Code == uv.KeyRight && (ctrl || alt)):
		e.pos = e.wordEndAfter(e.pos)
	case k.Code == uv.KeyBackspace && alt, ctrl && k.Code == 'w':
		start := e.pos
		if ctrl {
			for start > 0 && unicode.IsSpace(e.buf[start-1]) {
				start--
			}
			for start > 0 && !unicode.IsSpace(e.buf[start-1]) {
				start--
			}
		} else {
			start = e.wordStartBefore(e.pos)
		}
		e.cut(start, e.pos)
	case k.Code == uv.KeyBackspace || (ctrl && k.Code == 'h'):
		if e.pos > 0 {
			e.buf = append(e.buf[:e.pos-1], e.buf[e.pos:]...)
			e.pos--
		}
	case k.Code == uv.KeyDelete:
		if e.pos < len(e.buf) {
			e.buf = append(e.buf[:e.pos], e.buf[e.pos+1:]...)
		}
	case alt && k.Code == 'd':
		e.cut(e.pos, e.wordEndAfter(e.pos))
	case ctrl && k.Code == 'k':
		e.cut(e.pos, len(e.buf))
	case ctrl && k.Code == 'u':
		e.cut(0, e.pos)
	case ctrl && k.Code == 'y':
		e.insert(e.kill)
	case ctrl && k.Code == 't':
		if e.pos > 0 && len(e.buf) > 1 {
			p := e.pos
			if p == len(e.buf) {
				p--
			}
			e.buf[p-1], e.buf[p] = e.buf[p], e.buf[p-1]
			e.pos = p + 1
		}
	case ctrl && k.Code == 'l':
		io.WriteString(e.c.Out, "\x1b[H\x1b[2J")
	case k.Text != "" && !ctrl && !alt:
		e.insert(k.Text)
	}
	return false, nil
}

// Input is Python input, writing the prompt and reading one line with interrupt and end of input as errors
func (c *Console) Input(prompt string) (string, error) { return c.input(prompt) }

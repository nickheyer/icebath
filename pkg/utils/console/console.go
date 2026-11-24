package consoleutil

import (
	"io"
	"os"
	"strings"
	"sync"

	"github.com/charmbracelet/colorprofile"
)

// Printer of console markup, tables, panels and other renderables to a writer, wrapped at a width and styled with escape sequences when color is on
type Console struct {
	// Destination of everything printed, nothing is written when nil
	Out io.Writer
	// Whether output is styled with escape sequences
	Color bool
	// Colors styled output uses, the terminal's own when left unknown or none
	Profile colorprofile.Profile
	// Cells lines wrap at, DetectWidth when zero
	Width int
	// Whether Out is an interactive terminal, the only kind status spinners and progress displays animate on
	Terminal bool
	// Every print dropped
	Quiet bool
	// Automatic coloring of numbers, quoted strings, paths, addresses and similar literals in printed markup turned off
	NoHighlight bool
	// Hyperlinks written as their address in parentheses after the linked text instead of as terminal links
	NoLinks bool
	// Receives the plain text of every print in place of writing it: printed markup unwrapped, tables, panels and renderables as their lines at the width
	Capture func(text string)

	mu sync.Mutex
}

// Console over a writer, styled in the terminal's colors (or forced ones under FORCE_COLOR or CLICOLOR_FORCE) and animated only when the writer is a terminal that is not dumb
func NewConsole(w io.Writer) *Console {
	c := &Console{Out: w, Width: DetectWidth()}
	usable := escapesWork(w)
	c.Terminal = usable && isTerminal(w) && !dumbTerminal()
	if p := detectProfile(w); p > colorprofile.NoTTY && usable {
		c.Color, c.Profile = true, p
	}
	return c
}

func (c *Console) width() int {
	if c.Width > 0 {
		return c.Width
	}
	return DetectWidth()
}

// Profile output is drawn for, none when color is off
func (c *Console) profile() colorprofile.Profile {
	switch {
	case !c.Color:
		return colorprofile.NoTTY
	case c.Profile > colorprofile.NoTTY:
		return c.Profile
	}
	return TerminalProfile()
}

// Layout of a print: the console width, links written out when the profile cannot carry them or NoLinks asks
func (c *Console) layout(p colorprofile.Profile, noWrap bool) layout {
	return layout{width: c.width(), noWrap: noWrap, links: p > colorprofile.NoTTY && !c.NoLinks}
}

func (c *Console) capture(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Capture(text)
}

// Writes lines drawn for the profile, above the live display when one runs on the terminal the console writes to
func (c *Console) write(lines []segments, p colorprofile.Profile, links bool) {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(renderLine(l, p, links))
		b.WriteByte('\n')
	}
	if l := liveActive.Load(); l != nil && l.above(c, b.String()) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	io.WriteString(c.Out, b.String())
}

// Prints markup, colored by its literals when highlight is set, wrapped at the width unless soft
func (c *Console) printMarkup(markup string, highlighted, soft bool) {
	if c.Quiet {
		return
	}
	if c.Capture != nil {
		c.capture(inlineLinks(markupText(markup)).plain())
		return
	}
	if c.Out == nil {
		return
	}
	p := c.profile()
	o := c.layout(p, soft)
	plain, spans := parseMarkup(markup)
	if highlighted && !c.NoHighlight && p > colorprofile.NoTTY {
		spans = append(highlight(plain), spans...)
	}
	t := styled(plain, spans)
	if !o.links {
		t = inlineLinks(t)
	}
	c.write(wrap(t, o.width, wrapOptions{noWrap: soft}), p, o.links)
}

// Prints a renderable at the console width with base laid under it
func (c *Console) printRenderable(r Renderable, base Style, soft bool) {
	if c.Quiet {
		return
	}
	if c.Capture != nil {
		c.capture(plainLines(r.render(c.layout(colorprofile.NoTTY, soft))))
		return
	}
	if c.Out == nil {
		return
	}
	p := c.profile()
	o := c.layout(p, soft)
	lines := r.render(o)
	if !base.isZero() {
		for i, l := range lines {
			lines[i] = l.over(base)
		}
	}
	c.write(lines, p, o.links)
}

// Prints console markup ([bold red]text[/] styles a run, [link=https://x.io]site[/] links one, \[ is a literal bracket) joined by spaces, wrapped at the width, its literals colored unless NoHighlight is set, a blank line when called without markup
func (c *Console) Print(markup ...string) { c.printMarkup(strings.Join(markup, " "), true, false) }

// Prints markup joined by spaces, wrapped at the width, with no coloring of its literals
func (c *Console) PrintNoHighlight(markup ...string) {
	c.printMarkup(strings.Join(markup, " "), false, false)
}

// Prints markup on lines left unwrapped for the terminal to wrap, so long commands and paths stay whole for copying
func (c *Console) PrintSoft(markup string) { c.printMarkup(markup, true, true) }

// Prints a table fitted to the width
func (c *Console) PrintTable(t *Table) { c.printRenderable(t, Style{}, false) }

// Prints a panel at the width, or fitted to its content when it asks
func (c *Console) PrintPanel(p Panel) { c.printRenderable(p, Style{}, false) }

// Prints a renderable at the width with the options applied, a style that does not parse laying nothing under it
func (c *Console) PrintRenderableWith(r Renderable, o PrintOptions) {
	c.printRenderable(r, styleOf(o.Style), o.SoftWrap)
}

// Line typed on standard input after the markup prompt, without its line end; io.EOF when input ends before any character
func (c *Console) Input(prompt string) (string, error) {
	if prompt != "" && !c.Quiet && c.Out != nil {
		p := c.profile()
		plain, spans := parseMarkup(prompt)
		if !c.NoHighlight && p > colorprofile.NoTTY {
			spans = append(highlight(plain), spans...)
		}
		t := styled(plain, spans)
		links := p > colorprofile.NoTTY && !c.NoLinks
		if !links {
			t = inlineLinks(t)
		}
		c.mu.Lock()
		io.WriteString(c.Out, renderLines(t.lines(), p, links))
		c.mu.Unlock()
	}
	var line []byte
	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if n == 1 {
			if buf[0] == '\n' {
				return strings.TrimSuffix(string(line), "\r"), nil
			}
			line = append(line, buf[0])
		}
		if err != nil {
			if err == io.EOF && len(line) > 0 {
				return string(line), nil
			}
			return string(line), err
		}
	}
}

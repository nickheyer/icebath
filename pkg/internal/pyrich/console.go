package pyrich

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	xterm "github.com/charmbracelet/x/term"
	"github.com/mattn/go-runewidth"
	"github.com/nickheyer/icebath/pkg/internal/pysys"
)

var widthCond = &runewidth.Condition{EastAsianWidth: false}

// Terminal cells a string occupies, as rich cell_len counts them
func CellLen(s string) int {
	n := 0
	for _, r := range s {
		if r < 32 || r == 0x7f {
			continue
		}
		n += widthCond.RuneWidth(r)
	}
	return n
}

// Console writes markup the way rich.console.Console.print does, wrapped at Width cells
type Console struct {
	Out io.Writer
	// Line source of Input, standard input when nil
	In io.Reader
	// ANSI styling on, as for a rich console on a color terminal
	Color bool
	// Wrap width in cells, rich reads COLUMNS or the terminal size and falls back to 80
	Width int
	// Interactive terminal output, the only kind a status spinner draws on
	Terminal bool
	// Every print dropped, as Console(quiet=True)
	Quiet bool
	// Repr highlighting of printed strings off, as Console(highlight=False)
	NoHighlight bool
	// Colors dropped while attributes and links stay, as Console(no_color=True) or a non empty NO_COLOR
	NoColor bool
	// Forced color system truecolor, 256 or standard as Console(color_system=...), the terminal's own when empty
	ColorSystem string
	// Receives the raw markup of every print in place of output, a renderable arriving as its plain render
	Capture func(raw string)
	mu      sync.Mutex
}

// Hands raw to the capture under the console lock, false when no capture is set
func (c *Console) captured(raw string) bool {
	if c.Capture == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Capture(raw)
	return true
}

// Console over a writer, styled and animated only when the writer is a terminal
func NewConsole(w io.Writer) *Console {
	tty := pysys.Isatty(w)
	term := os.Getenv("TERM")
	color := tty && term != "dumb" && term != "unknown"
	return &Console{Out: w, Color: color, Width: DetectWidth(), Terminal: tty, NoColor: os.Getenv("NO_COLOR") != ""}
}

// Console width rich computes, the first terminal among stdin stdout stderr, then COLUMNS, then 80
func DetectWidth() int {
	width := 0
	for _, fd := range []uintptr{0, 1, 2} {
		if w, _, err := xterm.GetSize(fd); err == nil {
			width = w
			break
		}
	}
	if cols := os.Getenv("COLUMNS"); cols != "" && isDigits(cols) {
		width, _ = strconv.Atoi(cols)
	}
	if width == 0 {
		width = 80
	}
	return width
}

func isDigits(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return s != ""
}

func (c *Console) write(lines []string) {
	if c.Quiet || c.Out == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range lines {
		io.WriteString(c.Out, c.colorless(l)+"\n")
	}
}

// Color system the console renders styles for
func (c *Console) system() string { return resolveSystem(c.ColorSystem) }

// Rendered output with color codes removed when the console is in no color mode
func (c *Console) colorless(rendered string) string {
	if c.NoColor && c.Color {
		return removeColor(rendered)
	}
	return rendered
}

// Every SGR segment stripped of its color codes and dropped with its reset when no code stays, as Segment.remove_color
func removeColor(rendered string) string {
	var b strings.Builder
	dropReset := false
	pos := 0
	for _, m := range ansiRE.FindAllStringIndex(rendered, -1) {
		b.WriteString(rendered[pos:m[0]])
		pos = m[1]
		seq := rendered[m[0]+2 : m[1]-1]
		if seq == "0" {
			if !dropReset {
				b.WriteString("\x1b[0m")
			}
			dropReset = false
			continue
		}
		if kept := attributeCodes(seq); kept != "" {
			b.WriteString("\x1b[" + kept + "m")
		} else {
			dropReset = true
		}
	}
	b.WriteString(rendered[pos:])
	return b.String()
}

// SGR parameters with every foreground and background color code removed
func attributeCodes(seq string) string {
	params := strings.Split(seq, ";")
	var kept []string
	for i := 0; i < len(params); i++ {
		n, err := strconv.Atoi(params[i])
		switch {
		case err != nil:
			kept = append(kept, params[i])
		case n == 38 || n == 48:
			if i+1 < len(params) && params[i+1] == "5" {
				i += 2
			} else if i+1 < len(params) && params[i+1] == "2" {
				i += 4
			}
		case n >= 30 && n <= 37, n == 39, n >= 40 && n <= 47, n == 49, n >= 90 && n <= 97, n >= 100 && n <= 107:
		default:
			kept = append(kept, params[i])
		}
	}
	return strings.Join(kept, ";")
}

// Prints markup wrapped at the console width, an empty call printing a blank line
func (c *Console) Print(markup ...string) {
	text := strings.Join(markup, " ")
	if c.captured(text) {
		return
	}
	c.write(WrapStyled(renderHighlighted(text, c.Color, !c.NoHighlight, c.system()), c.Width, true))
}

// Prints markup wrapped at the console width with repr highlighting off, as print(..., highlight=False)
func (c *Console) PrintNoHighlight(markup ...string) {
	text := strings.Join(markup, " ")
	if c.captured(text) {
		return
	}
	c.write(WrapStyled(renderHighlighted(text, c.Color, false, c.system()), c.Width, true))
}

// Prints markup on unwrapped lines, as print(..., soft_wrap=True)
func (c *Console) PrintSoft(markup string) {
	if c.captured(markup) {
		return
	}
	c.write([]string{renderHighlighted(markup, c.Color, !c.NoHighlight, c.system())})
}

// Prints a table fitted to the console width
func (c *Console) PrintTable(t *Table) {
	if c.Capture != nil {
		c.captured(t.renderWidth(false, c.system(), c.Width))
		return
	}
	c.write(strings.Split(t.renderWidth(c.Color, c.system(), c.Width), "\n"))
}

// Prints a panel expanded to the console width
func (c *Console) PrintPanel(p Panel) {
	if c.Capture != nil {
		c.captured(p.render(false, c.system(), c.Width))
		return
	}
	c.write(strings.Split(p.render(c.Color, c.system(), c.Width), "\n"))
}

// Input prints the prompt without a newline and reads one line, io.EOF standing for EOFError
func (c *Console) Input(prompt string) (string, error) {
	if prompt != "" && !c.Quiet && c.Out != nil {
		c.mu.Lock()
		io.WriteString(c.Out, c.colorless(renderHighlighted(prompt, c.Color, !c.NoHighlight, c.system())))
		c.mu.Unlock()
	}
	in := c.In
	if in == nil {
		in = os.Stdin
	}
	var line []byte
	buf := make([]byte, 1)
	for {
		n, err := in.Read(buf)
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

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

var escapeSeqRE = regexp.MustCompile("\x1b\\[[0-9;]*m|\x1b\\]8;[^\x1b]*\x1b\\\\")

type styledRune struct {
	r      rune
	prefix string
}

// Visible runes of ANSI styled text, each with the escape sequences right before it, plus the trailing escapes
func splitStyled(s string) ([]styledRune, string) {
	var out []styledRune
	pending := ""
	for len(s) > 0 {
		if loc := escapeSeqRE.FindStringIndex(s); loc != nil && loc[0] == 0 {
			pending += s[:loc[1]]
			s = s[loc[1]:]
			continue
		}
		r, n := utf8.DecodeRuneInString(s)
		out = append(out, styledRune{r: r, prefix: pending})
		pending = ""
		s = s[n:]
	}
	return out, pending
}

type escState struct{ link, sgr string }

// Escapes reopening the hyperlink and SGR attributes in force
func (e escState) open() string { return e.link + e.sgr }

// Escapes ending the SGR attributes then the hyperlink in force, the order rich closes a segment in
func (e escState) close() string {
	out := ""
	if e.sgr != "" {
		out += "\x1b[0m"
	}
	if e.link != "" {
		out += "\x1b]8;;\x1b\\"
	}
	return out
}

// SGR and hyperlink state in force after the escapes, a reset clearing SGR and an empty link closing the link
func activeAfter(state escState, escapes string) escState {
	for _, esc := range escapeSeqRE.FindAllString(escapes, -1) {
		switch {
		case esc == "\x1b[0m":
			state.sgr = ""
		case esc == "\x1b]8;;\x1b\\":
			state.link = ""
		case strings.HasPrefix(esc, "\x1b]"):
			state.link = esc
		default:
			state.sgr += esc
		}
	}
	return state
}

// Styled lines of runes cut at the given rune ranges, each line reopening the style in force at its start
func joinStyled(runes []styledRune, tail string, cuts [][2]int) []string {
	out := make([]string, 0, len(cuts))
	var state escState
	pos := 0
	for _, cut := range cuts {
		for ; pos < cut[0]; pos++ {
			state = activeAfter(state, runes[pos].prefix)
		}
		var b strings.Builder
		cur := state
		for i := cut[0]; i < cut[1]; i++ {
			cur = activeAfter(cur, runes[i].prefix)
			if i == cut[0] {
				b.WriteString(cur.open())
			} else {
				b.WriteString(runes[i].prefix)
			}
			b.WriteRune(runes[i].r)
		}
		pos = cut[1]
		state = cur
		if cut[1] > cut[0] {
			if cut[1] == len(runes) {
				b.WriteString(tail)
				cur = activeAfter(cur, tail)
			}
			b.WriteString(cur.close())
		}
		out = append(out, b.String())
	}
	return out
}

var wordRE = regexp.MustCompile(`\s*\S+\s*`)

// Break offsets in runes where rich divide_line starts a new line of at most width cells
func DivideLine(text string, width int, fold bool) []int {
	var breaks []int
	offset := 0
	runeIdx := func(byteIdx int) int { return utf8.RuneCountInString(text[:byteIdx]) }
	for _, loc := range wordRE.FindAllStringIndex(text, -1) {
		start := runeIdx(loc[0])
		word := text[loc[0]:loc[1]]
		wordLen := CellLen(strings.TrimRightFunc(word, unicode.IsSpace))
		if width-offset >= wordLen {
			offset += CellLen(word)
			continue
		}
		if wordLen > width {
			if fold {
				chunks := chopCells(word, width)
				for i, chunk := range chunks {
					if start > 0 {
						breaks = append(breaks, start)
					}
					if i == len(chunks)-1 {
						offset = CellLen(chunk)
					} else {
						start += utf8.RuneCountInString(chunk)
					}
				}
			} else {
				if start > 0 {
					breaks = append(breaks, start)
				}
				offset = CellLen(word)
			}
		} else if offset > 0 && start > 0 {
			breaks = append(breaks, start)
			offset = CellLen(word)
		}
	}
	return breaks
}

// Pieces of a word of at most width cells each, as rich chop_cells folds it
func chopCells(word string, width int) []string {
	var out []string
	var cur strings.Builder
	total := 0
	for _, r := range word {
		w := widthCond.RuneWidth(r)
		if total+w > width && cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
			total = 0
		}
		cur.WriteRune(r)
		total += w
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func plainOf(runes []styledRune, from, to int) string {
	var b strings.Builder
	for i := from; i < to; i++ {
		b.WriteRune(runes[i].r)
	}
	return b.String()
}

// Rune ranges of one logical line wrapped at width, trailing whitespace trimmed only past the width
func wrapRanges(runes []styledRune, from, to, width int, fold bool) [][2]int {
	text := plainOf(runes, from, to)
	if width <= 0 {
		return [][2]int{{from, to}}
	}
	starts := append([]int{0}, DivideLine(text, width, fold)...)
	var out [][2]int
	for i, s := range starts {
		e := to - from
		if i+1 < len(starts) {
			e = starts[i+1]
		}
		a, b := from+s, from+e
		excess := CellLen(plainOf(runes, a, b)) - width
		for excess > 0 && b > a && unicode.IsSpace(runes[b-1].r) {
			excess -= widthCond.RuneWidth(runes[b-1].r)
			b--
		}
		out = append(out, [2]int{a, b})
	}
	return out
}

// Lines of ANSI styled text wrapped at width cells the way rich wraps printed text
func WrapStyled(rendered string, width int, fold bool) []string {
	runes, tail := splitStyled(rendered)
	var cuts [][2]int
	start := 0
	for i := 0; i <= len(runes); i++ {
		if i < len(runes) && runes[i].r != '\n' {
			continue
		}
		cuts = append(cuts, wrapRanges(runes, start, i, width, fold)...)
		start = i + 1
	}
	return joinStyled(runes, tail, cuts)
}

// Cut of a styled line to width cells, overflow ellipsis ending in a horizontal ellipsis
func truncateStyled(line string, width int, overflow string) string {
	runes, _ := splitStyled(line)
	if CellLen(plainOf(runes, 0, len(runes))) <= width {
		return line
	}
	limit := width
	if overflow == "ellipsis" {
		limit = width - 1
	}
	n, cells := 0, 0
	for n < len(runes) && cells+widthCond.RuneWidth(runes[n].r) <= limit {
		cells += widthCond.RuneWidth(runes[n].r)
		n++
	}
	kept := append([]styledRune(nil), runes[:n]...)
	next := n
	if cells < limit && n < len(runes) {
		kept = append(kept, styledRune{r: ' ', prefix: runes[n].prefix})
		next = n + 1
	}
	if overflow == "ellipsis" && limit >= 0 {
		prefix := ""
		if next < len(runes) {
			prefix = runes[next].prefix
		}
		kept = append(kept, styledRune{r: '…', prefix: prefix})
	}
	return joinStyled(kept, "", [][2]int{{0, len(kept)}})[0]
}

// Rich status spinner shown on a terminal while work runs, erased when stopped
type Status struct {
	c    *Console
	mu   sync.Mutex
	text string
	stop chan struct{}
	done chan struct{}
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Starts a dots spinner beside the markup, drawing nothing unless the console is a terminal
func (c *Console) Status(markup string) *Status {
	mustMarkupText(markup)
	s := &Status{c: c, text: markup}
	if !c.Terminal || c.Quiet {
		return s
	}
	s.stop, s.done = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(s.done)
		tick := time.NewTicker(80 * time.Millisecond)
		defer tick.Stop()
		io.WriteString(c.Out, "\x1b[?25l")
		for i := 0; ; i++ {
			s.mu.Lock()
			text := s.text
			s.mu.Unlock()
			c.mu.Lock()
			fmt.Fprintf(c.Out, "\r\x1b[2K%s %s", c.colorless(RenderSystem("[green]"+spinnerFrames[i%len(spinnerFrames)]+"[/]", c.Color, c.system())), c.colorless(Render(text, c.Color)))
			c.mu.Unlock()
			select {
			case <-s.stop:
				c.mu.Lock()
				io.WriteString(c.Out, "\r\x1b[2K\x1b[?25h")
				c.mu.Unlock()
				return
			case <-tick.C:
			}
		}
	}()
	return s
}

// Replaces the status markup
func (s *Status) Update(markup string) {
	mustMarkupText(markup)
	s.mu.Lock()
	s.text = markup
	s.mu.Unlock()
}

// Stops the spinner and erases its line
func (s *Status) Stop() {
	if s.stop == nil {
		return
	}
	close(s.stop)
	<-s.done
	s.stop = nil
}

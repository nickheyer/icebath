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
	// ANSI styling on, as for a rich console on a color terminal
	Color bool
	// Wrap width in cells, rich reads COLUMNS or the terminal size and falls back to 80
	Width int
	// Interactive terminal output, the only kind a status spinner draws on
	Terminal bool
	// Every print dropped, as Console(quiet=True)
	Quiet bool
	mu    sync.Mutex
}

// Console over a writer, styled and animated only when the writer is a terminal
func NewConsole(w io.Writer) *Console {
	tty := false
	if f, ok := w.(*os.File); ok {
		tty = xterm.IsTerminal(f.Fd())
	}
	term := os.Getenv("TERM")
	color := tty && os.Getenv("NO_COLOR") == "" && term != "dumb" && term != "unknown"
	return &Console{Out: w, Color: color, Width: DetectWidth(), Terminal: tty}
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
		io.WriteString(c.Out, l+"\n")
	}
}

// Prints markup wrapped at the console width, an empty call printing a blank line
func (c *Console) Print(markup ...string) {
	text := strings.Join(markup, " ")
	c.write(WrapStyled(Render(text, c.Color), c.Width, true))
}

// Prints markup on unwrapped lines, as print(..., soft_wrap=True)
func (c *Console) PrintSoft(markup string) {
	c.write([]string{Render(markup, c.Color)})
}

// Prints a table fitted to the console width
func (c *Console) PrintTable(t *Table) {
	c.write(strings.Split(t.RenderWidth(c.Color, c.Width), "\n"))
}

// Prints a panel expanded to the console width
func (c *Console) PrintPanel(p Panel) {
	c.write(strings.Split(p.Render(c.Color, c.Width), "\n"))
}

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

type styledRune struct {
	r      rune
	prefix string
}

// Visible runes of ANSI styled text, each with the escape sequences right before it, plus the trailing escapes
func splitStyled(s string) ([]styledRune, string) {
	var out []styledRune
	pending := ""
	for len(s) > 0 {
		if loc := ansiRE.FindStringIndex(s); loc != nil && loc[0] == 0 {
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

// SGR state in force after the escapes, empty after a reset
func activeAfter(state, escapes string) string {
	for _, esc := range ansiRE.FindAllString(escapes, -1) {
		if esc == "\x1b[0m" {
			state = ""
		} else {
			state += esc
		}
	}
	return state
}

// Styled lines of runes cut at the given rune ranges, each line reopening the style in force at its start
func joinStyled(runes []styledRune, tail string, cuts [][2]int) []string {
	out := make([]string, 0, len(cuts))
	state := ""
	pos := 0
	for _, cut := range cuts {
		for ; pos < cut[0]; pos++ {
			state = activeAfter(state, runes[pos].prefix)
		}
		var b strings.Builder
		b.WriteString(state)
		cur := state
		for i := cut[0]; i < cut[1]; i++ {
			b.WriteString(runes[i].prefix)
			cur = activeAfter(cur, runes[i].prefix)
			b.WriteRune(runes[i].r)
		}
		pos = cut[1]
		state = cur
		if cut[1] == len(runes) {
			b.WriteString(tail)
			cur = activeAfter(cur, tail)
		}
		if cur != "" {
			b.WriteString("\x1b[0m")
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
	runes, tail := splitStyled(line)
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
	out := joinStyled(runes[:n], "", [][2]int{{0, n}})[0]
	if overflow == "ellipsis" && limit >= 0 {
		out += "…"
	}
	_ = tail
	return out
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
			fmt.Fprintf(c.Out, "\r\x1b[2K%s %s", Render("[green]"+spinnerFrames[i%len(spinnerFrames)]+"[/]", c.Color), Render(text, c.Color))
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

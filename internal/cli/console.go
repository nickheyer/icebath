package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
)

// Colors of a console's lines
const (
	Cyan   = "\033[36m"
	Green  = "\033[32m"
	Yellow = "\033[33m"
	Red    = "\033[31m"
	Dim    = "\033[2m"
	Bold   = "\033[1m"
	Gray   = "\033[90m"
	Reset  = "\033[0m"
)

// Failure of a command with the exit status it ends with, its lines already printed
type Exit int

func (e Exit) Error() string { return "" }

func (e Exit) ExitCode() int { return int(e) }

// Line read from the input, or why none came
type Line struct {
	Text string
	Err  error
}

// Terminal of one command: lines in color when it is a terminal, answers read one line at a time, secrets without echo, at most one read
// outstanding
type Console struct {
	ctx     context.Context
	out     io.Writer
	in      *bufio.Reader
	color   bool
	tty     bool
	outTTY  bool
	pending chan Line
}

func NewConsole(cmd *cobra.Command) *Console {
	return console(cmd.Context(), cmd.OutOrStdout(), cmd.InOrStdin())
}

// Console of this process's own terminal, for a hook that runs around a command rather than in one
func Terminal(ctx context.Context) *Console {
	return console(ctx, os.Stdout, os.Stdin)
}

func console(ctx context.Context, out io.Writer, in io.Reader) *Console {
	if ctx == nil {
		ctx = context.Background()
	}
	_, noColor := os.LookupEnv("NO_COLOR")
	outTTY := out == os.Stdout && term.IsTerminal(os.Stdout.Fd())
	return &Console{ctx: ctx, out: out, in: bufio.NewReader(in), color: outTTY && !noColor, outTTY: outTTY,
		tty: in == os.Stdin && term.IsTerminal(os.Stdin.Fd())}
}

// Whether a person answers at a terminal
func (c *Console) Interactive() bool { return c.tty }

// Whether a person answers at a terminal and reads the lines on one
func (c *Console) Attended() bool { return c.tty && c.outTTY }

// Where the console's lines go
func (c *Console) Out() io.Writer { return c.out }

// Whether the lines go to a terminal that shows color, which redraws a line in place too
func (c *Console) Colored() bool { return c.color }

// Text in the colors given when the console shows color
func (c *Console) Paint(text string, codes ...string) string {
	if !c.color {
		return text
	}
	return Painted(text, codes...)
}

// Text in the colors given, for a screen that always shows them
func Painted(text string, codes ...string) string {
	if len(codes) == 0 {
		return text
	}
	return strings.Join(codes, "") + text + Reset
}

// Prints a line in the colors given
func (c *Console) Say(text string, codes ...string) { fmt.Fprintln(c.out, c.Paint(text, codes...)) }

func (c *Console) Blank() { fmt.Fprintln(c.out) }

// Lines of a wizard: a note, a step done, a warning and a failure, each indented and marked
func (c *Console) Info(text string)    { c.Say("  "+text, Dim) }
func (c *Console) Success(text string) { c.Say("  ✓ "+text, Green) }
func (c *Console) Warning(text string) { c.Say("  ⚠ "+text, Yellow) }
func (c *Console) Failure(text string) { c.Say("  ✗ "+text, Red) }

// Says a failure in red and ends the command with status 1
func (c *Console) Fail(text string) error {
	c.Say(text, Red)
	return Exit(1)
}

// Yes in green or no dimmed
func (c *Console) YesNo(on bool) string {
	if on {
		return c.Paint("yes", Green)
	}
	return c.Paint("no", Dim)
}

// Check mark in green
func (c *Console) Tick() string { return c.Paint("✓", Green) }

// Prints a step of a wizard: its number and title in bold after a blank line
func (c *Console) Step(n int, title string) {
	c.Blank()
	fmt.Fprintln(c.out, c.Paint(fmt.Sprintf("Step %d", n), Bold)+"  "+title)
}

// Starts a read of one line, or joins the one already outstanding; a secret line is not echoed
func (c *Console) start(secret bool) chan Line {
	if c.pending != nil {
		return c.pending
	}
	ch := make(chan Line, 1)
	c.pending = ch
	go func() {
		if secret && c.tty {
			text, err := c.masked()
			fmt.Fprintln(c.out)
			ch <- Line{Text: text, Err: err}
			return
		}
		text, err := c.in.ReadString('\n')
		if err == io.EOF && text != "" {
			err = nil
		}
		ch <- Line{Text: strings.TrimRight(text, "\r\n"), Err: err}
	}()
	return ch
}

// Line typed at the terminal with a star shown for each character: backspace takes one back, enter ends it, ctrl+c and ctrl+d on an empty
// line give up; without raw mode nothing is echoed at all
func (c *Console) masked() (string, error) {
	fd := os.Stdin.Fd()
	state, err := term.MakeRaw(fd)
	if err != nil {
		data, err := term.ReadPassword(fd)
		return string(data), err
	}
	defer term.Restore(fd, state)
	var typed []rune
	var pending []byte
	// 0 outside an escape sequence, 1 after its escape, 2 inside its parameters
	escape := 0
	one := make([]byte, 1)
	for {
		if _, err := os.Stdin.Read(one); err != nil {
			return "", err
		}
		b := one[0]
		switch {
		case escape == 1:
			escape = 0
			if b == '[' || b == 'O' {
				escape = 2
			}
			continue
		case escape == 2:
			if b >= 0x40 && b <= 0x7e {
				escape = 0
			}
			continue
		}
		switch {
		case b == '\r' || b == '\n':
			return string(typed), nil
		case b == 3:
			return "", errors.New("interrupted")
		case b == 4 && len(typed) == 0:
			return "", io.EOF
		case b == 0x7f || b == 0x08:
			if len(typed) > 0 {
				typed = typed[:len(typed)-1]
				fmt.Fprint(c.out, "\b \b")
			}
		case b == 0x1b:
			escape = 1
		case b < 0x20:
		default:
			pending = append(pending, b)
			if !utf8.FullRune(pending) {
				continue
			}
			if r, _ := utf8.DecodeRune(pending); r != utf8.RuneError {
				typed = append(typed, r)
				fmt.Fprint(c.out, "*")
			}
			pending = pending[:0]
		}
	}
}

// Line typed after the prompt, false at the end of the input or on an interrupt
func (c *Console) read(prompt string, secret bool) (string, bool) {
	fmt.Fprint(c.out, prompt)
	ch := c.start(secret)
	select {
	case got := <-ch:
		c.pending = nil
		return got.Text, got.Err == nil
	case <-c.ctx.Done():
		c.Blank()
		return "", false
	}
}

// Context of the command the console is for
func (c *Console) Context() context.Context { return c.ctx }

// Line typed after the prompt, read while the caller waits on other things, joining a read already outstanding; Took is called once it
// came
func (c *Console) Await(prompt string) <-chan Line {
	fmt.Fprint(c.out, prompt)
	return c.start(false)
}

// Marks the line Await delivered as taken, so the next read waits for a new one
func (c *Console) Took() { c.pending = nil }

// Line typed after the prompt, false at the end of the input or on an interrupt
func (c *Console) Input(prompt string) (string, bool) { return c.read(prompt, false) }

// Line typed after the prompt without its surrounding spaces, false at the end of the input or on an interrupt
func (c *Console) Answer(prompt string) (string, bool) {
	value, ok := c.read(prompt, false)
	return strings.TrimSpace(value), ok
}

// Line typed after the prompt without echo where the input is a terminal, false at the end of the input or on an interrupt
func (c *Console) Secret(prompt string) (string, bool) {
	if !c.tty {
		fmt.Fprintln(c.out, c.Paint("Warning: Password input may be echoed.", Yellow))
	}
	return c.read(prompt, true)
}

// Yes or no from one line after the question and its y/N hint, the default on an empty line, the end of the input or an interrupt
func (c *Console) Confirm(question string, def bool) bool {
	return c.Agree(question+" ["+hint(def)+"] ", def)
}

// Confirm of a wizard: the question indented and in yellow, its hint followed by a colon
func (c *Console) Ask(question string, def bool) bool {
	return c.Agree(c.Paint("  "+question+" ["+hint(def)+"]: ", Yellow), def)
}

// Yes or no from one line after the prompt as given: y or yes is yes, an empty line, the end of the input or an interrupt the default
func (c *Console) Agree(prompt string, def bool) bool {
	value, ok := c.Input(prompt)
	if !ok {
		c.Blank()
		return def
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return def
	case "y", "yes":
		return true
	}
	return false
}

// Hint of a yes or no question, its default in capitals
func hint(def bool) string {
	if def {
		return "Y/n"
	}
	return "y/N"
}

// Answer of a wizard to the question, indented and in yellow with its default when it is no secret; the default when nothing is typed
func (c *Console) Prompt(question, def string, secret bool) string {
	shown := "  " + question
	if def != "" && !secret {
		shown += " [" + def + "]"
	}
	value, ok := c.read(c.Paint(shown+": ", Yellow), secret)
	if !ok {
		return def
	}
	if value = strings.TrimSpace(value); value == "" {
		return def
	}
	return value
}

// One of the items picked by its number, false when none was
func (c *Console) Choose(title string, items []string) (int, bool) {
	c.Blank()
	c.Say("  "+title, Cyan, Bold)
	for i, item := range items {
		fmt.Fprintf(c.out, "  %3d  %s\n", i+1, item)
	}
	for {
		value, ok := c.Input(c.Paint("  Number (empty or q to quit): ", Yellow))
		value = strings.ToLower(strings.TrimSpace(value))
		if !ok || value == "" || value == "q" {
			return 0, false
		}
		if n, err := strconv.Atoi(value); err == nil && n >= 1 && n <= len(items) {
			return n - 1, true
		}
		c.Failure("Pick a number from 1 to " + strconv.Itoa(len(items)))
	}
}

// Items checked and unchecked by their numbers until the list is accepted; the picked ones, the selection given when cancelled
func (c *Console) Checklist(title string, items []string, selected map[int]bool) map[int]bool {
	picked := map[int]bool{}
	for i, on := range selected {
		if on {
			picked[i] = true
		}
	}
	for {
		c.Blank()
		c.Say("  "+title, Cyan, Bold)
		for i, item := range items {
			mark := "[ ]"
			if picked[i] {
				mark = c.Paint("[x]", Green)
			}
			fmt.Fprintf(c.out, "  %3d %s %s\n", i+1, mark, item)
		}
		value, ok := c.Input(c.Paint("  Toggle numbers (1 3-5), a for all, n for none, empty to accept, q to cancel: ", Yellow))
		value = strings.ToLower(strings.TrimSpace(value))
		switch {
		case !ok || value == "q":
			return selected
		case value == "":
			return picked
		case value == "a":
			for i := range items {
				picked[i] = true
			}
			continue
		case value == "n":
			clear(picked)
			continue
		}
		for word := range strings.FieldsFuncSeq(value, func(r rune) bool { return r == ' ' || r == ',' }) {
			from, to, ranged := strings.Cut(word, "-")
			a, errA := strconv.Atoi(from)
			b, errB := a, error(nil)
			if ranged {
				b, errB = strconv.Atoi(to)
			}
			if errA != nil || errB != nil || a < 1 || b > len(items) || a > b {
				c.Failure("Not a number or range of the list: " + word)
				continue
			}
			for i := a - 1; i < b; i++ {
				picked[i] = !picked[i]
			}
		}
	}
}

// Indexes of a selection in ascending order
func Picked(set map[int]bool) []int {
	var out []int
	for i, on := range set {
		if on {
			out = append(out, i)
		}
	}
	slices.Sort(out)
	return out
}

// Number from 1 to count typed after the prompt, asked again until one is; 0 for an empty line when empty is allowed, false at the end
// of the input
func (c *Console) Index(prompt string, count int, empty bool, emptyLine string) (int, bool) {
	for {
		value, ok := c.Input(prompt)
		if !ok {
			return 0, false
		}
		value = strings.TrimSpace(value)
		if value == "" {
			if empty {
				return 0, true
			}
			if emptyLine != "" {
				c.Say(emptyLine, Red)
			}
			continue
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			c.Say("  Enter a number.", Red)
			continue
		}
		if n >= 1 && n <= int64(count) {
			return int(n), true
		}
		c.Say(fmt.Sprintf("  Out of range — pick 1-%d.", count), Red)
	}
}

// Width of a text as shown, its escape sequences left out
func Width(text string) int {
	return utf8.RuneCountInString(ansi.Strip(text))
}

// Text padded with spaces to the width shown
func Pad(text string, n int) string {
	if w := Width(text); w < n {
		return text + strings.Repeat(" ", n-w)
	}
	return text
}

// Prints a table: the headers in bold over a rule, then each row, every column as wide as its widest cell
func (c *Console) Table(headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for i, header := range headers {
		widths[i] = Width(header)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) {
				widths[i] = max(widths[i], Width(cell))
			}
		}
	}
	cells := func(row []string, header bool) string {
		parts := make([]string, len(widths))
		for i := range widths {
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			if header {
				cell = c.Paint(cell, Bold)
			}
			parts[i] = Pad(cell, widths[i])
		}
		return strings.TrimRight("  "+strings.Join(parts, "  "), " ")
	}
	fmt.Fprintln(c.out, cells(headers, true))
	rule := make([]string, len(widths))
	for i, w := range widths {
		rule[i] = strings.Repeat("─", w)
	}
	fmt.Fprintln(c.out, c.Paint("  "+strings.Join(rule, "  "), Dim))
	for _, row := range rows {
		fmt.Fprintln(c.out, cells(row, false))
	}
}

// Prints labels and values in two columns, the labels in bold, under a title in a frame
func (c *Console) Pairs(title string, rows [][2]string) {
	labels := 0
	for _, row := range rows {
		labels = max(labels, Width(row[0]))
	}
	var lines []string
	for _, row := range rows {
		lines = append(lines, c.Paint(Pad(row[0], labels), Bold)+"  "+row[1])
	}
	c.Panel(title, lines...)
}

// Prints lines in a frame, the title on its top edge
func (c *Console) Panel(title string, lines ...string) {
	inner := Width(title) + 2
	for _, l := range lines {
		inner = max(inner, Width(l))
	}
	top := "─ " + title + " "
	if title == "" {
		top = ""
	}
	fmt.Fprintln(c.out, c.Paint("╭"+top+strings.Repeat("─", inner+2-Width(top))+"╮", Cyan))
	for _, l := range lines {
		fmt.Fprintln(c.out, c.Paint("│", Cyan)+" "+Pad(l, inner)+" "+c.Paint("│", Cyan))
	}
	fmt.Fprintln(c.out, c.Paint("╰"+strings.Repeat("─", inner+2)+"╯", Cyan))
}

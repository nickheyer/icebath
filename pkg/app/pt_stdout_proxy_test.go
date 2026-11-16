package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/internal/pysys"
)

// Screen row of the first line holding each text, in the order given
func screenRowsOf(t *testing.T, h *ptHarness, texts ...string) []int {
	t.Helper()
	screen := h.screen()
	rows := make([]int, len(texts))
	for i, text := range texts {
		rows[i] = -1
		for y, row := range screen {
			if strings.Contains(row, text) {
				rows[i] = y
				break
			}
		}
		if rows[i] < 0 {
			t.Fatalf("%q is not on the screen:\n%s", text, strings.Join(screen, "\n"))
		}
	}
	return rows
}

func TestStdoutProxyPrintsLinesAboveTheRunningPromptAndNeverPastIt(t *testing.T) {
	submitted := make(chan string, 1)
	h := newPTHarness(t, 60, 14, func(a *ptApplication) { basicPTLayout(a, submitted) })
	terminal := &lockedBuffer{}
	proxy := &ptStdoutProxy{app: func() *ptApplication { return h.app }, out: terminal}
	fmt.Fprint(proxy, "before the prompt\npart")
	if got := terminal.String(); got != "before the prompt\n" {
		t.Fatalf("with no prompt running the terminal got %q", got)
	}
	h.start()
	h.waitShows("STATUS ROW")
	fmt.Fprint(proxy, "ial line\nfirst line\n  second line, indented\n")
	h.waitShows("second line, indented")
	rows := screenRowsOf(t, h, "partial line", "first line", "  second line, indented", "STATUS ROW")
	if rows[1] != rows[0]+1 || rows[2] != rows[1]+1 || rows[3] != rows[2]+1 {
		t.Fatalf("rows %v, screen:\n%s", rows, strings.Join(h.screen(), "\n"))
	}
	for _, row := range h.screen() {
		if i := strings.Index(row, "first line"); i > 0 {
			t.Fatalf("a printed line starts at column %d:\n%s", i, strings.Join(h.screen(), "\n"))
		}
	}
	if got := terminal.String(); got != "before the prompt\n" {
		t.Fatalf("a print went past the running prompt: %q", got)
	}
}

func TestStdoutProxyGivesTheTerminalHolderItsWritesAtOnce(t *testing.T) {
	submitted := make(chan string, 1)
	h := newPTHarness(t, 60, 14, func(a *ptApplication) { basicPTLayout(a, submitted) })
	terminal := &lockedBuffer{}
	proxy := &ptStdoutProxy{app: func() *ptApplication { return h.app }, out: terminal}
	h.start()
	h.waitShows("STATUS ROW")
	asked, answer, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	h.app.RunInTerminal(func() {
		fmt.Fprint(proxy, "Proceed? [y/N] ")
		close(asked)
		<-answer
		fmt.Fprint(proxy, "kept\n")
		close(done)
	})
	select {
	case <-asked:
	case <-time.After(3 * time.Second):
		t.Fatal("the function never got the terminal")
	}
	if got := terminal.String(); got != "Proceed? [y/N] " {
		t.Fatalf("the holder of the terminal wrote %q", got)
	}
	fmt.Fprint(proxy, "from another goroutine\n")
	if got := terminal.String(); got != "Proceed? [y/N] " {
		t.Fatalf("a print from outside the holder reached the terminal: %q", got)
	}
	close(answer)
	<-done
	h.waitShows("from another goroutine")
	if got := terminal.String(); got != "Proceed? [y/N] kept\n" {
		t.Fatalf("terminal got %q", got)
	}
	if h.app.TerminalReleased() {
		t.Fatal("terminal still marked released")
	}
}

func TestPatchStdoutRoutesEveryPrintStreamThroughTheProxyAndRestoresThem(t *testing.T) {
	c := &CLI{ctx: context.Background(), console: console()}
	terminal := &lockedBuffer{}
	proxy := &ptStdoutProxy{app: func() *ptApplication { return nil }, out: terminal}
	wasConsole, wasDefault, wasStderr, wasField := console(), cliui.Default(), stderrWriter(), c.console
	restore := c.patchStdout(proxy)
	fmt.Fprintln(console().Out, "console")
	fmt.Fprintln(stderrWriter(), "stderr")
	fmt.Fprintln(cliui.Default().Out, "default console")
	fmt.Fprintln(c.console.Out, "cli console")
	fmt.Fprintln(pysys.Stdout, "sys stdout")
	fmt.Fprintln(pysys.Stderr, "sys stderr")
	printLines("print lines")
	cliui.Default().PrintInfo("info")
	want := "console\nstderr\ndefault console\ncli console\nsys stdout\nsys stderr\nprint lines\n"
	if got := terminal.String(); !strings.HasPrefix(got, want) || !strings.Contains(got[len(want):], "info") {
		t.Fatalf("proxy got %q", got)
	}
	restore()
	if console() != wasConsole || cliui.Default() != wasDefault || stderrWriter() != wasStderr || c.console != wasField {
		t.Fatal("a stream was not restored")
	}
	seen := terminal.String()
	fmt.Fprint(pysys.Stderr, "")
	if terminal.String() != seen {
		t.Fatal("the proxy still receives prints after the restore")
	}
}

func TestSlashOutputPrintedThroughTheConsoleLandsUnderItsHeaderAboveThePrompt(t *testing.T) {
	submitted := make(chan string, 1)
	h := newPTHarness(t, 80, 16, func(a *ptApplication) { basicPTLayout(a, submitted) })
	terminal := &lockedBuffer{}
	c := &CLI{ctx: context.Background(), console: console()}
	proxy := cliStdoutProxy(h.app, terminal)
	setTUIApp(h.app)
	defer setTUIApp(nil)
	defer c.patchStdout(proxy)()
	h.start()
	h.waitShows("STATUS ROW")
	cprint("header of the command")
	printLines("first output line\n  second output line")
	fmt.Fprintln(pysys.Stderr, "a warning on stderr")
	h.waitShows("a warning on stderr")
	rows := screenRowsOf(t, h, "header of the command", "first output line", "  second output line", "a warning on stderr", "STATUS ROW")
	for i := 1; i < len(rows); i++ {
		if rows[i] != rows[i-1]+1 {
			t.Fatalf("rows %v, screen:\n%s", rows, strings.Join(h.screen(), "\n"))
		}
	}
	if got := terminal.String(); got != "" {
		t.Fatalf("a print went past the running prompt: %q", got)
	}
}

func TestCLIProxyPrintsAboveAnApplicationThatIsNotTheRegisteredOne(t *testing.T) {
	submitted := make(chan string, 1)
	h := newPTHarness(t, 60, 12, func(a *ptApplication) { basicPTLayout(a, submitted) })
	terminal := &lockedBuffer{}
	proxy := cliStdoutProxy(h.app, terminal)
	h.start()
	h.waitShows("STATUS ROW")
	fmt.Fprintln(proxy, "printed with no registered application")
	h.waitShows("printed with no registered application")
	if got := terminal.String(); got != "" {
		t.Fatalf("a print went past the running prompt: %q", got)
	}
}

func TestKanbanHelpDrawsItsMarkdownBoldOnlyWhereStylesShow(t *testing.T) {
	styled := &cliui.Console{StdoutTTY: func() bool { return true }, Getenv: func(k string) (string, bool) {
		return map[string]string{"TERM": "xterm-256color"}[k], k == "TERM"
	}}
	restore := SetStreams(styled, stderrWriter())
	if got := terminalBold("**/kanban** — board"); got != boldANSI+"/kanban"+resetANSI+" — board" {
		t.Fatalf("styled console drew %q", got)
	}
	restore()
	plain := &cliui.Console{StdoutTTY: func() bool { return false }}
	defer SetStreams(plain, stderrWriter())()
	if got := terminalBold("**/kanban** — board"); got != "**/kanban** — board" {
		t.Fatalf("plain console drew %q", got)
	}
}

package cliui

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/nickheyer/icebath/pkg/term"
)

// Real pseudo terminal with an emulator following everything written to it
type ptyRig struct {
	master, slave *os.File
	emu           *term.Emulator
	c             *Console
}

func newPtyRig(t *testing.T) *ptyRig {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	fd := int(master.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: 30, Col: 100}); err != nil {
		t.Fatal(err)
	}
	p := &ptyRig{master: master, slave: slave, emu: term.New(100, 30, term.Options{Scrollback: 200})}
	go io.Copy(p.emu, master)
	env := map[string]string{"TERM": "xterm-256color", "LANG": "en_US.UTF-8"}
	p.c = &Console{In: slave, Out: slave, Getenv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
	guard := p.c.termGuard()
	t.Cleanup(func() {
		guard.mu.Lock()
		defer guard.mu.Unlock()
		slave.Close()
		master.Close()
	})
	return p
}

func (p *ptyRig) modes(t *testing.T) unix.Termios {
	t.Helper()
	state, err := unix.IoctlGetTermios(int(p.slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	return *state
}

func (p *ptyRig) screen() string {
	var b strings.Builder
	base := p.emu.Base()
	for y := uint64(0); y < 30; y++ {
		line, _ := p.emu.Row(base + y)
		for _, cell := range line.Cells {
			if cell.Content != "" {
				b.WriteString(cell.Content)
			} else if cell.Width != 0 {
				b.WriteByte(' ')
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func (p *ptyRig) await(t *testing.T, text string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(p.screen(), text) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("terminal never showed %q:\n%s", text, p.screen())
}

func cooked(m unix.Termios) bool {
	return m.Lflag&unix.ICANON != 0 && m.Lflag&unix.ECHO != 0 && m.Lflag&unix.ISIG != 0 && m.Oflag&unix.OPOST != 0
}

func TestRealTerminalIsRawOnlyWhileAPromptRunsAndComesBackExactly(t *testing.T) {
	p := newPtyRig(t)
	before := p.modes(t)
	if !cooked(before) {
		t.Fatal("the pseudo terminal did not start cooked")
	}
	done := make(chan int, 1)
	go func() {
		got, _ := p.c.CursesRadiolist(context.Background(), "Pick", items("a", "b"), 0, cancelAt(-1))
		done <- got
	}()
	p.await(t, "❯ a")
	if cooked(p.modes(t)) || p.emu.Cursor().Visible {
		t.Fatal("the prompt is not holding raw mode with the cursor hidden")
	}
	io.WriteString(p.master, "\x1b[B\r")
	if got := <-done; got != 1 {
		t.Fatalf("picked %d", got)
	}
	if after := p.modes(t); after != before {
		t.Fatalf("terminal modes changed\nbefore %+v\nafter  %+v", before, after)
	}
	p.await(t, "› Pick  b")
	if !p.emu.Cursor().Visible {
		t.Fatal("cursor hidden after the prompt")
	}
}

func TestRealTerminalComesBackWhenBackNavigationUnwindsThroughAPrompt(t *testing.T) {
	p := newPtyRig(t)
	before := p.modes(t)
	ctx := WithMenuNavigationHandler(context.Background(), func(event MenuNavigationEvent, _ ...any) (*MenuNavigationStart, error) {
		if event == NavEventBack {
			panic("back")
		}
		return &MenuNavigationStart{AllowBack: true}, nil
	})
	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		p.c.CursesRadiolist(ctx, "Pick", items("a", "b"), 0, cancelAt(-1))
	}()
	p.await(t, "❯ a")
	io.WriteString(p.master, "\x1b[D")
	if got := <-done; got != "back" {
		t.Fatalf("recovered %v", got)
	}
	if after := p.modes(t); after != before {
		t.Fatalf("terminal modes changed\nbefore %+v\nafter  %+v", before, after)
	}
	typed := make(chan string, 1)
	go func() {
		line, _ := p.c.Input("next: ")
		typed <- line
	}()
	p.await(t, "next:")
	io.WriteString(p.master, "typed\r")
	if got := <-typed; got != "typed" {
		t.Fatalf("a plain line read after the back step got %q", got)
	}
}

func TestRealTerminalStaysRawAcrossNestedHoldsUntilTheLastRelease(t *testing.T) {
	p := newPtyRig(t)
	before := p.modes(t)
	outer, err := p.c.holdRaw()
	if err != nil {
		t.Fatal(err)
	}
	inner, _ := p.c.holdRaw()
	inner()
	inner()
	if cooked(p.modes(t)) {
		t.Fatal("releasing the inner hold left raw mode")
	}
	outer()
	if after := p.modes(t); after != before {
		t.Fatal("the last release did not restore the terminal")
	}
}

func TestRestoreTerminalEndsEveryHoldAndScreenMode(t *testing.T) {
	p := newPtyRig(t)
	before := p.modes(t)
	release, _ := p.c.holdRaw()
	p.c.enterMode("cursor", "\x1b[?25l", "\x1b[?25h")
	p.c.enterMode("paste", "\x1b[?2004h", "\x1b[?2004l")
	time.Sleep(50 * time.Millisecond)
	RestoreTerminal()
	time.Sleep(50 * time.Millisecond)
	if after := p.modes(t); after != before {
		t.Fatal("terminal modes not restored")
	}
	if !p.emu.Cursor().Visible || p.emu.BracketedPaste() {
		t.Fatal("screen modes not cleared")
	}
	release()
	if after := p.modes(t); after != before {
		t.Fatal("a late release changed the terminal")
	}
}

func TestAwaitSilencesEchoAndDropsWhatWasTyped(t *testing.T) {
	p := newPtyRig(t)
	before := p.modes(t)
	flow := p.c.Flow()
	stop := flow.Await("Checking providers")
	p.await(t, "Checking providers")
	during := p.modes(t)
	if during.Lflag&unix.ECHO != 0 || during.Lflag&unix.ISIG == 0 {
		t.Fatal("the wait echoes keys or blocks the interrupt key")
	}
	io.WriteString(p.master, "stray keys\r")
	time.Sleep(100 * time.Millisecond)
	stop()
	if after := p.modes(t); after != before {
		t.Fatal("terminal modes not restored after the wait")
	}
	if strings.Contains(p.screen(), "stray") {
		t.Fatalf("typed keys were echoed during the wait:\n%s", p.screen())
	}
	done := make(chan string, 1)
	go func() {
		got, _ := flow.LineInput("Name: ")
		done <- got
	}()
	p.await(t, "● Name")
	io.WriteString(p.master, "real\r")
	if got := <-done; got != "real" {
		t.Fatalf("the field read %q, keys typed during the wait leaked into it", got)
	}
	time.Sleep(echoGrace + 100*time.Millisecond)
	if after := p.modes(t); after != before {
		t.Fatal("terminal modes not restored once the grace after the field ended")
	}
}

func TestPromptInsideAWaitEndsTheSilenceAndLeavesTheTerminalAsItWas(t *testing.T) {
	p := newPtyRig(t)
	before := p.modes(t)
	flow := p.c.Flow()
	stop := flow.Await("Loading")
	p.await(t, "Loading")
	io.WriteString(p.master, "\r")
	time.Sleep(100 * time.Millisecond)
	done := make(chan int, 1)
	go func() {
		got, _ := flow.CursesRadiolist(context.Background(), "Pick", items("a", "b"), 0, cancelAt(-1))
		done <- got
	}()
	p.await(t, "❯ a")
	if m := p.modes(t); m.Lflag&unix.ICANON != 0 {
		t.Fatal("the prompt inside a wait is not raw")
	}
	select {
	case got := <-done:
		t.Fatalf("an Enter typed during the wait answered the prompt with %d", got)
	case <-time.After(300 * time.Millisecond):
	}
	io.WriteString(p.master, "\x1b[B\r")
	if got := <-done; got != 1 {
		t.Fatalf("picked %d", got)
	}
	stop()
	time.Sleep(echoGrace + 100*time.Millisecond)
	if after := p.modes(t); after != before {
		t.Fatal("terminal modes not restored")
	}
}

func TestKeysTypedRightAfterASetupPromptAreNeverEchoedAndReachTheNextPrompt(t *testing.T) {
	p := newPtyRig(t)
	before := p.modes(t)
	flow := p.c.Flow()
	done := make(chan int, 1)
	go func() {
		got, _ := flow.CursesRadiolist(context.Background(), "Auth", items("OAuth", "API key"), 0, cancelAt(-1))
		done <- got
	}()
	p.await(t, "❯ OAuth")
	io.WriteString(p.master, "\x1b[B\r")
	<-done
	if m := p.modes(t); m.Lflag&unix.ECHO != 0 || m.Lflag&unix.ICANON == 0 || m.Lflag&unix.ISIG == 0 {
		t.Fatal("right after the prompt the terminal echoes, or is not back to line input with the interrupt key")
	}
	io.WriteString(p.master, "sk-secret-typed-early")
	time.Sleep(50 * time.Millisecond)
	secret := make(chan string, 1)
	go func() {
		got, _ := flow.SecretLine("API key: ", "*")
		secret <- got
	}()
	p.await(t, "21 characters")
	if strings.Contains(p.screen(), "sk-secret") {
		t.Fatalf("a secret typed ahead of its field was echoed:\n%s", p.screen())
	}
	io.WriteString(p.master, "\r")
	if got := <-secret; got != "sk-secret-typed-early" {
		t.Fatalf("the field read %q", got)
	}
	time.Sleep(echoGrace + 100*time.Millisecond)
	if after := p.modes(t); after != before {
		t.Fatalf("terminal modes not restored once the grace ended\nbefore %+v\nafter  %+v", before, after)
	}
	plain := newPtyRig(t)
	go plain.c.CursesRadiolist(context.Background(), "Pick", items("a"), 0, cancelAt(-1))
	plain.await(t, "❯ a")
	start := plain.modes(t)
	io.WriteString(plain.master, "\r")
	plain.await(t, "› Pick  a")
	time.Sleep(20 * time.Millisecond)
	if m := plain.modes(t); m.Lflag&unix.ECHO == 0 || m == start {
		t.Fatal("a prompt outside a setup presentation must hand the terminal straight back")
	}
}

// Child half of the signal test, a prompt on the inherited terminal that never gets an answer
func TestHelperPromptOnInheritedTerminal(t *testing.T) {
	if os.Getenv("CLIUI_PROMPT_HELPER") != "1" {
		return
	}
	c := &Console{In: os.Stdin, Out: os.Stdout, Getenv: func(k string) (string, bool) {
		v, ok := map[string]string{"TERM": "xterm-256color", "LANG": "en_US.UTF-8"}[k]
		return v, ok
	}}
	c.CursesRadiolist(context.Background(), "Pick", items("a", "b"), 0, cancelAt(-1))
	os.Exit(3)
}

func TestTerminatingSignalDuringAPromptRestoresTheTerminalAndStillKills(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT} {
		p := newPtyRig(t)
		before := p.modes(t)
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperPromptOnInheritedTerminal$")
		cmd.Env = append(os.Environ(), "CLIUI_PROMPT_HELPER=1")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = p.slave, p.slave, p.slave
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		p.await(t, "❯ a")
		if cooked(p.modes(t)) {
			t.Fatal("the child prompt is not raw")
		}
		cmd.Process.Signal(sig)
		err := cmd.Wait()
		status, _ := cmd.ProcessState.Sys().(syscall.WaitStatus)
		if sig == syscall.SIGINT {
			if cmd.ProcessState.ExitCode() != 3 {
				t.Fatalf("an interrupt signal during a prompt ended the child with %v, want the prompt to cancel", err)
			}
		} else if !status.Signaled() || status.Signal() != sig {
			t.Fatalf("%v ended the child with %v", sig, err)
		}
		if after := p.modes(t); after != before {
			t.Fatalf("%v left the terminal changed\nbefore %+v\nafter  %+v", sig, before, after)
		}
		time.Sleep(50 * time.Millisecond)
		if !p.emu.Cursor().Visible {
			t.Fatalf("%v left the cursor hidden", sig)
		}
	}
}

//go:build linux

package tty

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/sys/unix"

	"github.com/nickheyer/icebath/pkg/term"
)

// Environment variable that makes the test binary run a program on its terminal instead of the tests
const childEnv = "ICEBATH_TTY_TEST_CHILD"

func TestMain(m *testing.M) {
	if mode := os.Getenv(childEnv); mode != "" {
		os.Exit(runChild(mode))
	}
	os.Exit(m.Run())
}

// Model of the child program, a counter that quits, panics in Update or panics in a goroutine on keys
type childModel struct{ n int }

func (c *childModel) Init() tea.Cmd { return nil }

func (c *childModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		switch k.String() {
		case "q":
			return c, tea.Quit
		case "p":
			panic("update panic")
		case "g":
			go func() { panic("goroutine panic") }()
		case "z":
			return c, tea.Suspend
		case "e":
			return c, Exec(exec.Command("sh", "-c", "printf 'from exec'; sleep 0.2"), func(error) tea.Msg { return tea.KeyPressMsg{Code: 'x', Text: "x"} })
		}
		c.n++
	}
	return c, nil
}

func (c *childModel) View() tea.View {
	v := tea.NewView(fmt.Sprintf("hello %d\n\x1b[1;38;2;200;10;10mwide 中文 ❤️ end\x1b[m", c.n))
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.ReportFocus = true
	v.Cursor = tea.NewCursor(2, 2)
	v.WindowTitle = "child"
	return v
}

func runChild(mode string) int {
	if mode == "job" {
		return runJobShell()
	}
	_, err := NewProgram(context.Background(), &childModel{}).Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		return 3
	}
	return 0
}

// Job control shell of the pty running the program as its foreground job, fg on SIGUSR1
func runJobShell() int {
	fg := make(chan os.Signal, 1)
	signal.Notify(fg, syscall.SIGUSR1)
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "shell:", err)
		return 4
	}
	job := exec.Command(exe)
	job.Env = append(os.Environ(), childEnv+"=plain")
	job.Stdin, job.Stdout, job.Stderr = os.Stdin, os.Stdout, os.Stderr
	job.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Foreground: true, Ctty: 0}
	if err := job.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "shell:", err)
		return 4
	}
	signal.Ignore(syscall.SIGTTOU)
	pid := job.Process.Pid
	for {
		var ws syscall.WaitStatus
		if _, err := syscall.Wait4(pid, &ws, syscall.WUNTRACED, nil); err != nil {
			fmt.Fprintln(os.Stderr, "shell:", err)
			return 5
		}
		switch {
		case ws.Exited():
			return ws.ExitStatus()
		case !ws.Stopped():
			fmt.Fprintln(os.Stderr, "shell: job ended by", ws.Signal())
			return 6
		}
		if err := unix.IoctlSetPointerInt(0, unix.TIOCSPGRP, unix.Getpgrp()); err != nil {
			fmt.Fprintln(os.Stderr, "shell: taking the terminal:", err)
			return 7
		}
		<-fg
		if err := unix.IoctlSetPointerInt(0, unix.TIOCSPGRP, pid); err != nil {
			fmt.Fprintln(os.Stderr, "shell: giving the terminal:", err)
			return 7
		}
		if err := syscall.Kill(-pid, syscall.SIGCONT); err != nil {
			fmt.Fprintln(os.Stderr, "shell: continuing the job:", err)
			return 7
		}
	}
}

// Fields of /proc/<pid>/stat after the command name, the state first
func procStat(pid int) []string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil
	}
	text := string(data)
	return strings.Fields(text[strings.LastIndexByte(text, ')')+1:])
}

// Pid of the one process whose parent is parent, zero while there is none
func procChild(parent int) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if fields := procStat(pid); len(fields) > 1 && fields[1] == strconv.Itoa(parent) {
			return pid
		}
	}
	return 0
}

// Terminal the child runs on, a pty whose master feeds icebath's emulator and answers its queries
type ptyTerm struct {
	t      *testing.T
	master *os.File
	slave  *os.File
	mu     sync.Mutex
	emu    *term.Emulator
	log    bytes.Buffer
	reply  bytes.Buffer
	cmd    *exec.Cmd
	stderr bytes.Buffer
	done   chan struct{}
}

func openPty(t *testing.T, w, h int) *ptyTerm {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetUint32(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.IoctlSetWinsize(int(s.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: uint16(h), Col: uint16(w)}); err != nil {
		t.Fatal(err)
	}
	p := &ptyTerm{t: t, master: m, slave: s, done: make(chan struct{})}
	p.emu = term.New(w, h, term.Options{Reply: &p.reply})
	t.Cleanup(func() { m.Close(); s.Close() })
	return p
}

// Starts the child as the session of the pty, in job mode a shell running the program as a job
func (p *ptyTerm) start(mode string) {
	exe, err := os.Executable()
	if err != nil {
		p.t.Fatal(err)
	}
	p.cmd = exec.Command(exe)
	p.cmd.Env = append(os.Environ(), childEnv+"="+mode, "TERM=xterm-256color", "COLORTERM=truecolor")
	p.cmd.Stdin, p.cmd.Stdout = p.slave, p.slave
	p.cmd.Stderr = &p.stderr
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := p.cmd.Start(); err != nil {
		p.t.Fatal(err)
	}
	go func() {
		defer close(p.done)
		buf := make([]byte, 64<<10)
		for {
			n, err := p.master.Read(buf)
			if n > 0 {
				p.mu.Lock()
				p.log.Write(buf[:n])
				_, _ = p.emu.Write(buf[:n])
				ans := append([]byte(nil), p.reply.Bytes()...)
				p.reply.Reset()
				p.mu.Unlock()
				if len(ans) > 0 {
					_, _ = p.master.Write(ans)
				}
			}
			if err != nil {
				return
			}
		}
	}()
}

// Screen text of the emulator
func (p *ptyTerm) screen() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, h := p.emu.Size()
	var b strings.Builder
	for y := 0; y < h; y++ {
		line, _ := p.emu.Row(p.emu.Base() + uint64(y))
		for x := 0; x < w && x < len(line.Cells); x++ {
			b.WriteString(line.Cells[x].Content)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// Waits until the condition holds, failing after a few seconds
func (p *ptyTerm) until(what string, cond func() bool) {
	p.t.Helper()
	end := time.Now().Add(8 * time.Second)
	for !cond() {
		if time.Now().After(end) {
			p.t.Fatalf("timed out waiting for %s, screen\n%s", what, p.screen())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Reports whether the terminal is in canonical cooked mode, the state the shell left it in
func (p *ptyTerm) cooked() bool {
	tio, err := unix.IoctlGetTermios(int(p.slave.Fd()), unix.TCGETS)
	return err == nil && tio.Lflag&unix.ICANON != 0 && tio.Lflag&unix.ECHO != 0
}

// Waits for the child to exit and its output to drain
func (p *ptyTerm) wait() error {
	err := p.cmd.Wait()
	time.Sleep(100 * time.Millisecond)
	return err
}

func (p *ptyTerm) alt() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.emu.AltScreen()
}

func (p *ptyTerm) logged() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.log.String()
}

func TestProgramOwnsAndRestoresTheTerminal(t *testing.T) {
	p := openPty(t, 60, 12)
	p.start("plain")
	p.until("the first frame", func() bool { return strings.Contains(p.screen(), "hello 0") && strings.Contains(p.screen(), "end") })
	if !p.alt() || p.cooked() {
		t.Fatalf("alt %v cooked %v while running", p.alt(), p.cooked())
	}
	p.master.Write([]byte("a"))
	p.until("a repaint after a key", func() bool { return strings.Contains(p.screen(), "hello 1") })
	p.master.Write([]byte("q"))
	if err := p.wait(); err != nil {
		t.Fatalf("child: %v, stderr %q", err, p.stderr.String())
	}
	if p.alt() || !p.cooked() || p.emu.Cursor().Visible == false {
		t.Fatalf("after exit alt %v cooked %v cursor %v", p.alt(), p.cooked(), p.emu.Cursor().Visible)
	}
	for _, s := range []string{"\x1b[?1002l", "\x1b[?1004l", "\x1b[?2004l", "\x1b[?7h", "\x1b]2;\a"} {
		if !strings.Contains(p.logged(), s) {
			t.Errorf("exit did not write %q", s)
		}
	}
	if p.stderr.Len() != 0 {
		t.Fatalf("stderr %q", p.stderr.String())
	}
}

func TestPanicInUpdateIsReportedAfterTheRestore(t *testing.T) {
	p := openPty(t, 60, 12)
	p.start("plain")
	p.until("the first frame", func() bool { return strings.Contains(p.screen(), "hello 0") })
	p.master.Write([]byte("p"))
	_ = p.wait()
	if p.alt() || !p.cooked() {
		t.Fatalf("after the panic alt %v cooked %v", p.alt(), p.cooked())
	}
	if !strings.Contains(p.stderr.String(), "update panic") {
		t.Fatalf("panic report missing from stderr %q", p.stderr.String())
	}
}

func TestGuardRestoresAfterACrash(t *testing.T) {
	p := openPty(t, 60, 12)
	p.start("plain")
	p.until("the first frame", func() bool { return strings.Contains(p.screen(), "hello 0") })
	p.master.Write([]byte("g"))
	_ = p.wait()
	p.until("the guard's restore", func() bool { return !p.alt() && p.cooked() })
	p.until("the crash report", func() bool { return strings.Contains(p.stderr.String(), "goroutine panic") })
}

func TestExecGivesTheTerminalToTheCommand(t *testing.T) {
	p := openPty(t, 60, 12)
	p.start("plain")
	p.until("the first frame", func() bool { return strings.Contains(p.screen(), "hello 0") })
	p.master.Write([]byte("e"))
	p.until("the command's output on the main screen", func() bool { return !p.alt() && strings.Contains(p.screen(), "from exec") })
	p.until("the program back", func() bool { return p.alt() && strings.Contains(p.screen(), "hello 1") })
	p.master.Write([]byte("q"))
	if err := p.wait(); err != nil {
		t.Fatalf("child: %v, stderr %q", err, p.stderr.String())
	}
}

func TestSuspendStopsAndContinues(t *testing.T) {
	p := openPty(t, 60, 12)
	p.start("job")
	p.until("the first frame", func() bool { return strings.Contains(p.screen(), "hello 0") })
	job := procChild(p.cmd.Process.Pid)
	if job == 0 {
		t.Fatalf("the shell runs no job, stderr %q", p.stderr.String())
	}
	p.master.Write([]byte("z"))
	p.until("the stop", func() bool {
		fields := procStat(job)
		return len(fields) > 0 && fields[0] == "T"
	})
	p.until("the terminal given back", func() bool { return !p.alt() && p.cooked() })
	_ = syscall.Kill(p.cmd.Process.Pid, syscall.SIGUSR1)
	p.until("the program back", func() bool { return p.alt() && !p.cooked() && strings.Contains(p.screen(), "hello 0") })
	p.master.Write([]byte("a"))
	p.until("keys read again", func() bool { return strings.Contains(p.screen(), "hello 1") })
	p.master.Write([]byte("q"))
	if err := p.wait(); err != nil {
		t.Fatalf("child: %v, stderr %q", err, p.stderr.String())
	}
}

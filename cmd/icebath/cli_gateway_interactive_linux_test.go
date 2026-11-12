//go:build linux

package main

import (
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Program on a pseudo terminal with everything it painted kept for the case to search
type terminalProgram struct {
	command *exec.Cmd
	master  *os.File
	mu      sync.Mutex
	painted []byte
	exited  chan struct{}
}

var terminalEscape = regexp.MustCompile("\x1b\\[[0-9;?<>=]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(\x07|\x1b\\\\)|\x1b[()][0-9A-B]|\x1b[=>78cDEHM]")

// Starts the binary as the program on a fresh 120 by 50 terminal
func startTerminalProgram(t *testing.T, bin string, env []string, args ...string) *terminalProgram {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	fd := int(master.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: 50, Col: 120}); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(bin, args...)
	command.Env = append(append([]string{}, env...), "TERM=xterm-256color")
	command.Stdin, command.Stdout, command.Stderr = slave, slave, slave
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	slave.Close()
	p := &terminalProgram{command: command, master: master, exited: make(chan struct{})}
	go func() {
		chunk := make([]byte, 65536)
		for {
			n, err := master.Read(chunk)
			if n > 0 {
				p.mu.Lock()
				p.painted = append(p.painted, chunk[:n]...)
				p.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		_ = command.Wait()
		close(p.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-p.exited:
		default:
			_ = command.Process.Kill()
			<-p.exited
		}
		master.Close()
	})
	return p
}

// Everything painted so far with the terminal control sequences removed
func (p *terminalProgram) text() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return terminalEscape.ReplaceAllString(string(p.painted), "")
}

// Waits until the painted text holds want, false after the timeout or once the program exited without it
func (p *terminalProgram) waitFor(want string, timeout time.Duration) bool {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if strings.Contains(p.text(), want) {
			return true
		}
		select {
		case <-p.exited:
			return strings.Contains(p.text(), want)
		default:
		}
	}
	return false
}

// Types text one key at a time the way a person does
func (p *terminalProgram) typeKeys(t *testing.T, text string) {
	t.Helper()
	for _, key := range []byte(text) {
		if _, err := p.master.Write([]byte{key}); err != nil {
			t.Fatalf("typing %q: %v", text, err)
		}
		time.Sleep(40 * time.Millisecond)
	}
}

var bannerSessionLabel = regexp.MustCompile(`Session:\s+\S+`)

func TestClassicInteractiveSessionRunsItsTurnInTheGatewayItLaunches(t *testing.T) {
	peer := newModelPeer(t)
	h, env := cliGatewayHomes(t, peer)
	bin := cliProgramBinary(t)
	bootHome = h
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			stopCLIGateway(t, bin, h, env)
		}
	})
	program := startTerminalProgram(t, bin, env)
	if !program.waitFor("Help improve", 60*time.Second) {
		t.Fatalf("no consent menu on a fresh home\n%s\n%s", program.text(), homeFailureLines(h))
	}
	program.typeKeys(t, "\r")
	if !program.waitFor("Ask anything", 90*time.Second) {
		t.Fatalf("the prompt never became ready\n%s\n%s", program.text(), homeFailureLines(h))
	}
	if !bannerSessionLabel.MatchString(program.text()) {
		t.Fatalf("the banner carries no gateway session label\n%s", program.text())
	}
	program.typeKeys(t, "WS_SHARED from the terminal")
	time.Sleep(time.Second)
	program.typeKeys(t, "\r")
	if !program.waitFor("LOCAL_ACK_WS_SHARED", 90*time.Second) {
		t.Fatalf("the typed turn printed no reply\n%s\n%s", program.text(), homeFailureLines(h))
	}
	time.Sleep(2 * time.Second)
	program.typeKeys(t, "/quit\r")
	select {
	case <-program.exited:
	case <-time.After(60 * time.Second):
		t.Fatalf("the program was still running a minute after /quit\n%s", program.text())
	}
	if code := program.command.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("/quit exited %d\n%s", code, program.text())
	}
	if got := peer.chatRequests(); got != 1 {
		t.Fatalf("model requests = %d", got)
	}
	stopCLIGateway(t, bin, h, env)
	stopped = true
	code, stdout, stderr := runCLIProgram(t, bin, env, 60*time.Second, "gateway", "status")
	if !strings.Contains(strings.ToLower(stdout+stderr), "not running") {
		t.Fatalf("gateway status after gateway stop = %d\n%s\n%s", code, stdout, stderr)
	}
}

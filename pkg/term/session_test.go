//go:build linux || darwin

package term

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
)

// Waits until the screen shows the text or the time runs out
func awaitText(t *testing.T, s *Session, text string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-s.Wake():
		case <-time.After(50 * time.Millisecond):
		}
		if strings.Contains(screenText(s.Emu), text) {
			return
		}
	}
	t.Fatalf("screen never showed %q:\n%s", text, screenText(s.Emu))
}

func screenText(e *Emulator) string {
	w, h := e.Size()
	buf := uv.NewBuffer(w, h)
	e.Draw(buf, View{})
	var lines []string
	for _, l := range buf.Lines {
		lines = append(lines, strings.TrimRight(uv.Line(l).String(), " "))
	}
	return strings.Join(lines, "\n")
}

func TestSessionRunsProgram(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	s, err := Open(Command{Path: sh, Args: []string{"-c", "stty size; printf 'hello\\n'; read line; echo got $line; exit 3"}, Env: Environ(nil, "test")}, 40, 10, Options{Scrollback: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	awaitText(t, s, "10 40")
	awaitText(t, s, "hello")
	s.Resize(50, 12)
	if w, h := s.Emu.Size(); w != 50 || h != 12 {
		t.Fatalf("size %dx%d", w, h)
	}
	s.Input([]byte("world\r"))
	awaitText(t, s, "got world")
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("program did not end")
	}
	if s.Exit().Code != 3 {
		t.Fatalf("exit %+v", s.Exit())
	}
}

func TestSessionCloseKillsProgram(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	s, err := Open(Command{Path: sh, Args: []string{"-c", "trap '' HUP; echo up; sleep 60"}}, 20, 5, Options{})
	if err != nil {
		t.Fatal(err)
	}
	awaitText(t, s, "up")
	start := time.Now()
	s.Close()
	if !s.Exited() {
		t.Fatal("not exited after close")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("close took too long")
	}
}

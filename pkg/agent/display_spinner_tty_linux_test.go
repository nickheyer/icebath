package agent

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Pseudo terminal pair whose slave answers isatty like an interactive stdout
func spinnerTestPty(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	return master, slave
}

// Spinner over the process stdout swapped to the given file, run briefly, with what reached the reader
func spinnerOverStdout(t *testing.T, stdout, reader *os.File) (bool, string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_SPINNER_PAUSE", "")
	prev := os.Stdout
	os.Stdout = stdout
	t.Cleanup(func() { os.Stdout = prev })
	s := newKawaiiSpinner(context.Background(), "thinking", "dots", nil)
	tty := s.isTTY()
	s.Start()
	time.Sleep(350 * time.Millisecond)
	s.Stop("")
	os.Stdout = prev
	stdout.Close()
	buf := make([]byte, 1<<16)
	var out strings.Builder
	_ = reader.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	for {
		n, err := reader.Read(buf)
		out.Write(buf[:n])
		if err != nil || n == 0 {
			break
		}
	}
	return tty, out.String()
}

func TestSpinnerOverATerminalStdoutAnimatesFrames(t *testing.T) {
	master, slave := spinnerTestPty(t)
	tty, out := spinnerOverStdout(t, slave, master)
	if !tty {
		t.Fatal("the process stdout on a terminal reads as not a tty")
	}
	if strings.Count(out, "\r") < 2 || !strings.Contains(out, "thinking") {
		t.Fatalf("no animated frames on the terminal: %q", out)
	}
}

func TestSpinnerOverAPipeStdoutLogsOneLine(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	tty, out := spinnerOverStdout(t, w, r)
	if tty {
		t.Fatal("a pipe reads as a tty")
	}
	if strings.Contains(out, "\r") || strings.Count(out, "thinking") != 1 {
		t.Fatalf("a pipe got animation instead of one line: %q", out)
	}
}

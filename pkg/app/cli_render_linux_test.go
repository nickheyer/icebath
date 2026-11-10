//go:build linux

package app

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Opens a pseudo terminal pair through /dev/ptmx
func openPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		master.Close()
		slave.Close()
	})
	return master, slave
}

// Reports canonical or echo mode on the descriptor
func isCooked(t *testing.T, fd int) bool {
	t.Helper()
	attrs, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	return attrs.Lflag&(unix.ICANON|unix.ECHO) != 0
}

// Puts the descriptor in raw mode as tty.setraw does
func setRaw(t *testing.T, fd int) {
	t.Helper()
	attrs, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	attrs.Iflag &^= unix.BRKINT | unix.ICRNL | unix.INPCK | unix.ISTRIP | unix.IXON
	attrs.Oflag &^= unix.OPOST
	attrs.Cflag &^= unix.CSIZE | unix.PARENB
	attrs.Cflag |= unix.CS8
	attrs.Lflag &^= unix.ECHO | unix.ICANON | unix.IEXTEN | unix.ISIG
	attrs.Cc[unix.VMIN], attrs.Cc[unix.VTIME] = 1, 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETSF, attrs); err != nil {
		t.Fatal(err)
	}
}

func TestHealsCookedDriftBackToRaw(t *testing.T) {
	_, slave := openPTY(t)
	fd := int(slave.Fd())
	if !isCooked(t, fd) {
		t.Fatal("precondition: fresh pty must be cooked")
	}
	if !healCookedModeDrift(fd) {
		t.Fatal("heal reported nothing to do")
	}
	attrs, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	if attrs.Lflag&(unix.ICANON|unix.ECHO|unix.ISIG|unix.IEXTEN) != 0 {
		t.Fatalf("lflag %#x still cooked", attrs.Lflag)
	}
	if attrs.Iflag&(unix.IXON|unix.IXOFF|unix.ICRNL|unix.INLCR|unix.IGNCR) != 0 {
		t.Fatalf("iflag %#x not patched", attrs.Iflag)
	}
	if attrs.Cc[unix.VMIN] != 1 {
		t.Fatalf("VMIN %d", attrs.Cc[unix.VMIN])
	}
}

func TestNoopWhenAlreadyRaw(t *testing.T) {
	_, slave := openPTY(t)
	fd := int(slave.Fd())
	setRaw(t, fd)
	before, _ := unix.IoctlGetTermios(fd, unix.TCGETS)
	if healCookedModeDrift(fd) {
		t.Fatal("an already raw tty must not be treated as drifted")
	}
	after, _ := unix.IoctlGetTermios(fd, unix.TCGETS)
	if *before != *after {
		t.Fatal("raw tty attrs must be untouched")
	}
}

func TestReturnsFalseOnNonTty(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if healCookedModeDrift(int(r.Fd())) || healCookedModeDrift(int(w.Fd())) {
		t.Fatal("a pipe must not be healed")
	}
}

func TestHealPreservesUnrelatedFlags(t *testing.T) {
	_, slave := openPTY(t)
	fd := int(slave.Fd())
	attrs, _ := unix.IoctlGetTermios(fd, unix.TCGETS)
	attrs.Oflag |= unix.OPOST
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, attrs); err != nil {
		t.Fatal(err)
	}
	if !healCookedModeDrift(fd) {
		t.Fatal("heal reported nothing to do")
	}
	after, _ := unix.IoctlGetTermios(fd, unix.TCGETS)
	if after.Oflag&unix.OPOST == 0 {
		t.Fatal("oflag must be preserved by the heal")
	}
}

// Child side of the DA1 fence tests, prints the query result and whatever stayed in the tty buffer
func TestOsc11ChildProcess(t *testing.T) {
	if os.Getenv("ICEBATH_OSC11_CHILD") != "1" {
		return
	}
	bg, ok := queryOSC11Background()
	result := "None"
	if ok {
		result = "'" + bg + "'"
	}
	fmt.Printf("RESULT:%s\n", result)
	fd := int(os.Stdin.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err == nil {
		cbreak := *old
		cbreak.Lflag &^= unix.ECHO | unix.ICANON
		cbreak.Cc[unix.VMIN], cbreak.Cc[unix.VTIME] = 1, 0
		_ = unix.IoctlSetTermios(fd, unix.TCSETSF, &cbreak)
	}
	var buf []byte
	chunk := make([]byte, 256)
	deadline := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !fdReadable(fd, time.Now().Add(100*time.Millisecond)) {
			continue
		}
		n, err := unix.Read(fd, chunk)
		if err != nil || n == 0 {
			break
		}
		buf = append(buf, chunk[:n]...)
	}
	if old != nil {
		_ = unix.IoctlSetTermios(fd, unix.TCSETSW, old)
	}
	fmt.Printf("LEFTOVER:%q\n", buf)
}

// Runs the query in a child on a real pty, the parent playing the terminal through replyFn
func runOsc11Child(t *testing.T, replyFn func(age time.Duration) []byte) (string, string) {
	t.Helper()
	master, slave := openPTY(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestOsc11ChildProcess$", "-test.v")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	env := []string{"ICEBATH_OSC11_CHILD=1", "ICEBATH_HOME=" + t.TempDir(), "TERM=xterm-256color"}
	for _, kv := range os.Environ() {
		key := strings.SplitN(kv, "=", 2)[0]
		switch key {
		case "SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY", "ICEBATH_OSC11_CHILD", "ICEBATH_HOME", "TERM":
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	slave.Close()
	var mu sync.Mutex
	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		chunk := make([]byte, 1024)
		for {
			n, err := master.Read(chunk)
			if n > 0 {
				mu.Lock()
				out.Write(chunk[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	answered := 0
	var queryAt time.Time
	start := time.Now()
	for time.Since(start) < 8*time.Second {
		mu.Lock()
		text := out.String()
		mu.Unlock()
		if queryAt.IsZero() && strings.Count(text, "\x1b]11;?") > answered {
			queryAt = time.Now()
		}
		if !queryAt.IsZero() {
			if payload := replyFn(time.Since(queryAt)); payload != nil {
				_, _ = master.Write(payload)
				answered++
				queryAt = time.Time{}
			}
		}
		if idx := strings.Index(text, "LEFTOVER:"); idx >= 0 && strings.Contains(text[idx:], "\n") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = cmd.Wait()
	master.Close()
	<-done
	mu.Lock()
	text := out.String()
	mu.Unlock()
	t.Logf("child raw output: %q", text)
	result, leftover := "", ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if i := strings.Index(line, "RESULT:"); i >= 0 && result == "" {
			result = line[i+len("RESULT:"):]
		} else if i := strings.Index(line, "LEFTOVER:"); i >= 0 && leftover == "" {
			leftover = line[i+len("LEFTOVER:"):]
		}
	}
	return result, leftover
}

func TestOsc11Da1FenceHerdrStyleDa1OnlyReturnsNoneWithoutLeak(t *testing.T) {
	result, leftover := runOsc11Child(t, func(time.Duration) []byte { return []byte("\x1b[?62;22c") })
	if result != "None" || leftover != `""` {
		t.Fatalf("result %q leftover %q", result, leftover)
	}
}

func TestOsc11Da1FenceSlowInorderReplyIsConsumedNotLeaked(t *testing.T) {
	result, leftover := runOsc11Child(t, func(age time.Duration) []byte {
		if age > 300*time.Millisecond {
			return []byte("\x1b]11;rgb:1e1e/1e1e/2e2e\x1b\\\x1b[?62;22c")
		}
		return nil
	})
	if result != "'#1E1E2E'" || leftover != `""` {
		t.Fatalf("result %q leftover %q", result, leftover)
	}
}

func TestOsc11Da1FenceMuteTerminalTimesOutClean(t *testing.T) {
	result, leftover := runOsc11Child(t, func(time.Duration) []byte { return nil })
	if result != "None" || leftover != `""` {
		t.Fatalf("result %q leftover %q", result, leftover)
	}
}

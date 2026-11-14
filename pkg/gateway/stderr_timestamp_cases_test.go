//go:build !windows

package gateway

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var stampedLineRe = `\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2},\d{3}`

// Runs the wrapper in process over a shell child, its stdout and stderr captured
func runStderrTimestampWrapper(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("sh: %v", err)
	}
	var out, errOut bytes.Buffer
	savedOut, savedErr := stderrTimestampStdout, stderrTimestampStderr
	stderrTimestampStdout, stderrTimestampStderr = &out, &errOut
	t.Cleanup(func() { stderrTimestampStdout, stderrTimestampStderr = savedOut, savedErr })
	argv := append([]string{}, args[:2]...)
	argv = append(argv, "--", sh, "-c")
	argv = append(argv, args[2:]...)
	return StderrTimestampMain(argv), out.String(), errOut.String()
}

func TestMainTimestampsEachStderrLine(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "gateway.error.log")
	rc, _, _ := runStderrTimestampWrapper(t, "--error-log", logPath,
		`printf 'first failure\n' >&2; printf 'second failure without newline\n' >&2; printf '2026-07-15 12:34:56,789 already timestamped' >&2; exit 7`)
	if rc != 7 {
		t.Fatalf("rc = %d", rc)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := pystr.Splitlines(string(data), false)
	if len(lines) != 3 {
		t.Fatalf("lines = %q", lines)
	}
	if !regexp.MustCompile(`^` + stampedLineRe + ` first failure$`).MatchString(lines[0]) {
		t.Fatalf("line 0 = %q", lines[0])
	}
	if !regexp.MustCompile(`^` + stampedLineRe + ` second failure without newline$`).MatchString(lines[1]) {
		t.Fatalf("line 1 = %q", lines[1])
	}
	if lines[2] != "2026-07-15 12:34:56,789 already timestamped" {
		t.Fatalf("line 2 = %q", lines[2])
	}
}

func TestWrapperTimestampsChildStdoutIntoItsOwnStdout(t *testing.T) {
	errorLog := filepath.Join(t.TempDir(), "gateway.error.log")
	rc, stdout, _ := runStderrTimestampWrapper(t, "--error-log", errorLog,
		`printf '[whatsapp] Bridge started on port 3000\n'; printf 'stderr line\n' >&2; printf '2026-07-15 12:34:56,789 INFO already timestamped\n'; printf 'last line without newline'; exit 7`)
	if rc != 7 {
		t.Fatalf("rc = %d", rc)
	}
	lines := pystr.Splitlines(stdout, false)
	if len(lines) != 3 {
		t.Fatalf("lines = %q", lines)
	}
	if !regexp.MustCompile(`^` + stampedLineRe + ` \[whatsapp\] Bridge started on port 3000$`).MatchString(lines[0]) {
		t.Fatalf("line 0 = %q", lines[0])
	}
	if lines[1] != "2026-07-15 12:34:56,789 INFO already timestamped" {
		t.Fatalf("line 1 = %q", lines[1])
	}
	if !regexp.MustCompile(`^` + stampedLineRe + ` last line without newline$`).MatchString(lines[2]) {
		t.Fatalf("line 2 = %q", lines[2])
	}
	data, err := os.ReadFile(errorLog)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^` + stampedLineRe + ` stderr line$`).MatchString(pystr.Strip(string(data))) {
		t.Fatalf("error log = %q", data)
	}
}

func TestWrapperForwardsSigusr1RestartRequestToChild(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "gateway.error.log")
	ready := filepath.Join(dir, "ready")
	child := `trap 'kill $p; printf "restart requested\n" >&2; exit ` + strconv.Itoa(GatewayServiceRestartExitCode) + `' USR1; ` +
		`printf 1 > '` + ready + `'; sleep 20 >/dev/null 2>&1 </dev/null & p=$!; wait $p; exit 1`
	done := make(chan int, 1)
	go func() {
		rc, _, _ := runStderrTimestampWrapper(t, "--error-log", logPath, child)
		done <- rc
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child never started")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	var rc int
	select {
	case rc = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("wrapper never returned")
	}
	if rc != GatewayServiceRestartExitCode {
		t.Fatalf("rc = %d", rc)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "restart requested") {
		t.Fatalf("error log = %q", data)
	}
}

func TestStderrTimestampDecodesEachInvalidByteAsReplacement(t *testing.T) {
	var out bytes.Buffer
	if err := copyLinesWithTimestamps(strings.NewReader("2026-07-15 12:34:56,789 a\xff\xfeb\n"), &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "2026-07-15 12:34:56,789 a��b\n" {
		t.Fatalf("got %q", got)
	}
}

func TestStderrTimestampUnopenableLogIsUncaughtException(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rc, _, stderr := runStderrTimestampWrapper(t, "--error-log", filepath.Join(blocker, "gateway.error.log"), `printf 'x\n' >&2`)
	if rc != 1 {
		t.Fatalf("rc = %d", rc)
	}
	want := "Traceback (most recent call last):\nFileExistsError: [Errno 17] File exists: '" + blocker + "'\n"
	if stderr != want {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestStderrTimestampBrokenStdoutEndsPumpWithThreadTraceback(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "gateway.error.log")
	var errOut bytes.Buffer
	savedOut, savedErr := stderrTimestampStdout, stderrTimestampStderr
	stderrTimestampStdout, stderrTimestampStderr = brokenStream{}, &errOut
	t.Cleanup(func() { stderrTimestampStdout, stderrTimestampStderr = savedOut, savedErr })
	rc := StderrTimestampMain([]string{"--error-log", logPath, "--", "sh", "-c", `printf 'out\n'; exit 3`})
	if rc != 3 {
		t.Fatalf("rc = %d", rc)
	}
	want := "Exception in thread Thread-1 (_copy_stdout_with_timestamps):\nTraceback (most recent call last):\nBrokenPipeError: [Errno 32] Broken pipe\n"
	if errOut.String() != want {
		t.Fatalf("stderr = %q", errOut.String())
	}
	if err := writeTimestampedLine(brokenStream{}, "x"); !errors.Is(err, syscall.EPIPE) {
		t.Fatalf("err = %v", err)
	}
}

//go:build !windows

package dashboard

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Incarnation marker of a pid from /proc starttime on Linux and ps lstart elsewhere, ProcessLookupError when absent
func processStartMarker(pid int) (string, error) {
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if errors.Is(err, fs.ErrNotExist) {
			return "", &ProcessLookupError{PID: pid}
		}
		if err != nil {
			return "", err
		}
		if !utf8.Valid(data) {
			return "", fmt.Errorf("'utf-8' codec can't decode /proc/%d/stat", pid)
		}
		text := string(data)
		idx := strings.LastIndex(text, ")")
		if idx < 0 {
			return "", fmt.Errorf("list index out of range")
		}
		fields := pystr.SplitWhitespace(pystr.Strip(text[idx+1:]), -1)
		if len(fields) < 20 || !pystr.StrIsDigit(fields[19]) {
			return "", fmt.Errorf("invalid /proc stat data for PID %d", pid)
		}
		return "linux:" + fields[19], nil
	}
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return "", runErr
	}
	out, _ := pycodecs.DecodeReplace(stdout.Bytes(), "utf-8")
	errText, _ := pycodecs.DecodeReplace(stderr.Bytes(), "utf-8")
	return psStartMarker(pid, cmd.ProcessState.ExitCode(), out, errText)
}

// Marker of a ps lstart run, ProcessLookupError only for a missing process and an error for any other failure
func psStartMarker(pid, code int, out, errText string) (string, error) {
	marker := pystr.Strip(out)
	if code == 0 && marker != "" {
		return "ps:" + marker, nil
	}
	if (code == 1 && marker == "") || strings.Contains(strings.ToLower(errText), "no such process") {
		return "", &ProcessLookupError{PID: pid}
	}
	return "", fmt.Errorf("ps could not inspect PID %d: %s", pid, pystr.Strip(errText))
}

// Delivers the signal to this process so every registered handler runs
func raiseSignal(sig syscall.Signal) {
	_ = syscall.Kill(os.Getpid(), sig)
}

// Registers a channel for process signals
func notifySignals(ch chan<- os.Signal, sigs ...os.Signal) { signal.Notify(ch, sigs...) }

// Unregisters a channel from process signals
func stopSignals(ch chan<- os.Signal) { signal.Stop(ch) }

// Whether a reuse address bind of host and port fails with address in use, the uvicorn bind flags
func probeBindConflict(network, host string, port int) bool {
	ln, err := net.Listen(network, net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return isAddrInUseError(err)
	}
	ln.Close()
	return false
}

package updater

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	relaunchWaiterScript         = "update-relaunch-waiter.ps1"
	relaunchWaiterReadyFile      = "ready.txt"
	relaunchWaiterLogFile        = "hermes-relaunch-waiter.log"
	relaunchWaiterDefaultSeconds = 900
	relaunchWaiterHandshake      = 10 * time.Second
	relaunchWaiterPoll           = 250 * time.Millisecond
	relaunchWaiterCancelTimeout  = 10 * time.Second
	relaunchWaiterLogTailBytes   = 16 * 1024
)

var lineBreakRE = regexp.MustCompile(`\r?\n`)

// Detached MSIX relaunch waiter that survives this process and reopens the swapped package
type relaunchWaiter struct {
	cmd     *exec.Cmd
	closed  chan struct{}
	cleanup func() error
	cancel  func() error
}

func windowsSystemRoot() string {
	for _, key := range []string{"SystemRoot", "SYSTEMROOT"} {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return `C:\Windows`
}

func powershellPath() string {
	return filepath.Join(windowsSystemRoot(), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
}

func errorName(err error) string {
	return fmt.Sprintf("Error: %s", err.Error())
}

// Lines this attempt wrote to the waiter log, read from its last 16 KiB
func waiterLogTail(file, attempt string) string {
	handle, err := os.Open(file)
	if err != nil {
		return file + ": not written (the script never ran far enough to log)"
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		return file + ": not written (the script never ran far enough to log)"
	}
	length := min(info.Size(), relaunchWaiterLogTailBytes)
	buffer := make([]byte, length)
	if _, err := handle.ReadAt(buffer, info.Size()-length); err != nil && !errors.Is(err, io.EOF) {
		return file + ": not written (the script never ran far enough to log)"
	}
	var lines []string
	for _, line := range lineBreakRE.Split(string(buffer), -1) {
		if strings.Contains(line, "["+attempt+"]") {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return file + ":\nno lines from this attempt"
	}
	return file + ":\n" + strings.Join(lines, "\n")
}

// Starts the staged relaunch waiter for this process, nil when it cannot start or never handshakes
func (s *Service) startUpdateRelaunchWaiter(identityName string, timeoutSeconds int) (*relaunchWaiter, error) {
	if timeoutSeconds == 0 {
		timeoutSeconds = relaunchWaiterDefaultSeconds
	}
	created := s.processStartTimeMS()
	stageDir, err := os.MkdirTemp(os.TempDir(), "hermes-relaunch-")
	if err != nil {
		s.log(fmt.Sprintf("[updates] relaunch waiter: cannot create a staging dir under %s: %s", os.TempDir(), errorName(err)))
		return nil, nil
	}
	cleanup := func() error { return os.RemoveAll(stageDir) }
	source := filepath.Join(s.opts.ResourcesPath, relaunchWaiterScript)
	scriptPath := filepath.Join(stageDir, relaunchWaiterScript)
	readyFile := filepath.Join(stageDir, relaunchWaiterReadyFile)
	if err := copyFile(source, scriptPath); err != nil {
		s.log(fmt.Sprintf("[updates] relaunch waiter: cannot copy %s to %s: %s", source, scriptPath, errorName(err)))
		if cleanupErr := cleanup(); cleanupErr != nil {
			return nil, &aggregateError{message: "Relaunch waiter staging cleanup failed", errs: []error{err, cleanupErr}}
		}
		return nil, nil
	}
	args := []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath,
		"-ProcessId", strconv.Itoa(os.Getpid()), "-ProcessStartTimeMs", strconv.FormatInt(created, 10),
		"-IdentityName", identityName, "-ReadyFile", readyFile, "-TimeoutSeconds", strconv.Itoa(timeoutSeconds)}
	_, wrapped := wrapHandoffForDetachedConsole(UpdateScriptHandoff{Command: powershellPath(), Args: args, ScriptPath: scriptPath}, nil, true)
	cmd := exec.Command(filepath.Join(windowsSystemRoot(), "System32", "cmd.exe"), wrapped...)
	cmd.Dir = os.TempDir()
	hideConsole(cmd)
	started := time.Now()
	if err := cmd.Start(); err != nil {
		s.log(fmt.Sprintf("[updates] relaunch waiter: PowerShell failed to start: %s", errorName(err)))
		if cleanupErr := cleanup(); cleanupErr != nil {
			return nil, &aggregateError{message: "Relaunch waiter spawn cleanup failed", errs: []error{err, cleanupErr}}
		}
		return nil, nil
	}
	waiter := &relaunchWaiter{cmd: cmd, closed: make(chan struct{}), cleanup: cleanup}
	go func() {
		_ = cmd.Wait()
		close(waiter.closed)
	}()
	attempt := filepath.Base(stageDir)
	logPath := filepath.Join(os.TempDir(), relaunchWaiterLogFile)
	var cancelled bool
	var cancelErr error
	waiter.cancel = func() error {
		if cancelled {
			return cancelErr
		}
		cancelled = true
		select {
		case <-waiter.closed:
		default:
			kill := exec.Command(filepath.Join(windowsSystemRoot(), "System32", "taskkill.exe"), "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F")
			hideConsole(kill)
			if err := runWithin(kill, relaunchWaiterCancelTimeout); err != nil {
				cancelErr = err
				return cancelErr
			}
			select {
			case <-waiter.closed:
			case <-time.After(relaunchWaiterCancelTimeout):
				cancelErr = errors.New("Relaunch waiter did not exit after cancellation")
				return cancelErr
			}
		}
		cancelErr = cleanup()
		return cancelErr
	}
	deadline := time.Now().Add(relaunchWaiterHandshake)
	ticker := time.NewTicker(relaunchWaiterPoll)
	defer ticker.Stop()
	for {
		if pathExists(readyFile) {
			return waiter, nil
		}
		if !time.Now().Before(deadline) {
			s.log(fmt.Sprintf("[updates] relaunch waiter: no handshake within %dms (pid %d); stopping it\n%s", relaunchWaiterHandshake.Milliseconds(), cmd.Process.Pid, waiterLogTail(logPath, attempt)))
			return nil, waiter.cancel()
		}
		select {
		case <-waiter.closed:
			code := "none"
			if cmd.ProcessState != nil {
				code = strconv.Itoa(cmd.ProcessState.ExitCode())
			}
			s.log(fmt.Sprintf("[updates] relaunch waiter: exited before its handshake after %dms (exit code %s, signal none; 1 = script error, 2 = identity %s not found or has no app id)\n%s", time.Since(started).Milliseconds(), code, identityName, waiterLogTail(logPath, attempt)))
			return nil, waiter.cancel()
		case <-ticker.C:
		}
	}
}

// Start time of this process in epoch milliseconds, rounded as the uptime arithmetic rounds it
func (s *Service) processStartTimeMS() int64 {
	if created := processCreated(os.Getpid()); created != nil {
		return int64(math.Round(*created * 1000))
	}
	return int64(math.Round(float64(s.started.UnixNano()) / 1e6))
}

func runWithin(cmd *exec.Cmd, timeout time.Duration) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return fmt.Errorf("Command failed: %s", strings.Join(cmd.Args, " "))
	}
}

func copyFile(source, target string) error {
	raw, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(target, raw, 0o666)
}

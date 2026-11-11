package gateway

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	xterm "github.com/charmbracelet/x/term"
	"github.com/nickheyer/icebath/pkg/home"
)

// Root home whose logs directory holds update.log, swapped by tests
var updateOutputRootHome = home.Root

// Writer of icebath update that mirrors to the update log and drops screen output once the terminal is gone
type UpdateOutputStream struct {
	original       io.Writer
	log            io.Writer
	originalBroken bool
}

func NewUpdateOutputStream(original, log io.Writer) *UpdateOutputStream {
	return &UpdateOutputStream{original: original, log: log}
}

// Mirrors to the log first, then the original until it fails once, always reporting the full length written
func (s *UpdateOutputStream) Write(data []byte) (int, error) {
	if s.log != nil {
		func() {
			defer func() { _ = recover() }()
			_, _ = s.log.Write(data)
		}()
	}
	if !s.originalBroken {
		n, err := s.original.Write(data)
		if err == nil {
			return n, nil
		}
		s.originalBroken = true
	}
	return len(data), nil
}

// Flushes the log and the original, a failing original marked broken
func (s *UpdateOutputStream) Flush() {
	if f, ok := s.log.(interface{ Sync() error }); ok {
		_ = f.Sync()
	} else if f, ok := s.log.(interface{ Flush() error }); ok {
		_ = f.Flush()
	}
	if s.originalBroken {
		return
	}
	if f, ok := s.original.(interface{ Flush() error }); ok {
		if err := f.Flush(); err != nil {
			s.originalBroken = true
		}
	}
}

// True when the original is still usable and is a terminal
func (s *UpdateOutputStream) IsATTY() bool {
	if s.originalBroken {
		return false
	}
	fd, ok := s.Fd()
	return ok && xterm.IsTerminal(fd)
}

// File descriptor of the original stream, false when it has none
func (s *UpdateOutputStream) Fd() (uintptr, bool) {
	f, ok := s.original.(interface{ Fd() uintptr })
	if !ok {
		return 0, false
	}
	return f.Fd(), true
}

// True once a write or flush to the original failed
func (s *UpdateOutputStream) OriginalBroken() bool { return s.originalBroken }

// Streams and log handle of one protected update run, the wrapped pair current while installed
type UpdateOutputState struct {
	PrevStdout, PrevStderr io.Writer
	Stdout, Stderr         io.Writer
	LogFile                *os.File
	Installed              bool
}

// Ignores SIGHUP outside gateway mode and wraps both streams with a mirror to the root home update log
func InstallHangupProtection(stdout, stderr io.Writer, gatewayMode bool) *UpdateOutputState {
	state := &UpdateOutputState{PrevStdout: stdout, PrevStderr: stderr, Stdout: stdout, Stderr: stderr}
	if !gatewayMode {
		ignoreHangupSignal()
	}
	logFile, err := openUpdateLog()
	if err != nil {
		return state
	}
	state.LogFile = logFile
	state.Stdout = NewUpdateOutputStream(stdout, logFile)
	state.Stderr = NewUpdateOutputStream(stderr, logFile)
	state.Installed = true
	return state
}

// Opens update.log for append under the root home logs directory and writes the run header line
func openUpdateLog() (logFile *os.File, err error) {
	defer func() {
		if r := recover(); r != nil {
			logFile, err = nil, fmt.Errorf("%v", r)
		}
	}()
	logsDir := filepath.Join(updateOutputRootHome(), "logs")
	if err := os.MkdirAll(logsDir, 0o777); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(logsDir, "update.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return nil, err
	}
	stage := "started"
	if os.Getenv("ICEBATH_UPDATE_POST_SWAP") == "1" {
		stage = "continued on the pulled code"
	}
	if _, err := fmt.Fprintf(f, "\n=== icebath update %s %s ===\n", stage, time.Now().Format("2006-01-02T15:04:05")); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// Restores the stream pair of an installed state and closes its update log handle
func FinalizeUpdateOutput(state *UpdateOutputState) {
	if state == nil {
		return
	}
	if state.Installed {
		state.Stdout, state.Stderr = state.PrevStdout, state.PrevStderr
	}
	if state.LogFile != nil {
		_ = state.LogFile.Sync()
		_ = state.LogFile.Close()
	}
}

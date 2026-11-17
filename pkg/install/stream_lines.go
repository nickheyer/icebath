package install

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// How long a finished child's pipes may stay open under a surviving descendant
const DrainGrace = 20 * time.Second

// Longest run of bytes reported as one line
const maxStreamedLine = 4 * 1024 * 1024

// Reports each line written to it with the line ending dropped and invalid UTF-8 replaced
type lineWriter struct {
	report  func(string)
	pending []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	rest := p
	for len(rest) > 0 {
		room := maxStreamedLine - len(w.pending)
		i := bytes.IndexByte(rest, '\n')
		if i >= 0 && i <= room {
			w.pending = append(w.pending, rest[:i]...)
			w.flush()
			rest = rest[i+1:]
			continue
		}
		if len(rest) < room {
			w.pending = append(w.pending, rest...)
			break
		}
		w.pending = append(w.pending, rest[:room]...)
		w.flush()
		rest = rest[room:]
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	w.report(strings.ToValidUTF8(strings.TrimRight(string(w.pending), "\r"), "�"))
	w.pending = w.pending[:0]
}

// Reports the unterminated last line
func (w *lineWriter) Close() {
	if len(w.pending) > 0 {
		w.flush()
	}
}

// Runs the command reporting every output line, exit code from the process, pipes a descendant holds past grace abandoned
func streamLines(cmd *exec.Cmd, grace time.Duration, report func(LogStream, string)) (exitCode int, abandoned bool, err error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return -1, false, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return -1, false, err
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	hideConsole(cmd)
	err = cmd.Start()
	outW.Close()
	errW.Close()
	if err != nil {
		outR.Close()
		errR.Close()
		return -1, false, fmt.Errorf("spawning %s %v: %w", cmd.Path, cmd.Args[1:], err)
	}
	var pumps sync.WaitGroup
	pump := func(r *os.File, stream LogStream) {
		defer pumps.Done()
		lines := &lineWriter{report: func(line string) { report(stream, line) }}
		io.Copy(lines, r)
		lines.Close()
	}
	pumps.Add(2)
	go pump(outR, Stdout)
	go pump(errR, Stderr)
	drained := make(chan struct{})
	go func() {
		pumps.Wait()
		close(drained)
	}()
	cmd.Wait()
	timer := time.NewTimer(grace)
	select {
	case <-drained:
		timer.Stop()
	case <-timer.C:
		abandoned = true
	}
	outR.Close()
	errR.Close()
	<-drained
	exitCode = -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	return exitCode, abandoned, nil
}

// Note a caller reports when a drain was abandoned, a truncated log never passing for an empty one
func abandonedDrainNote(program string, grace time.Duration) string {
	return fmt.Sprintf("%s exited but a surviving descendant still holds its stdout/stderr; gave up on the last %ds of output (#90455)", program, int(grace.Seconds()))
}

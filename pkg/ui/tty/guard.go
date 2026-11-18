package tty

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Environment variable that turns a process started from the program's own binary into its guard
const guardEnv = "ICEBATH_TTY_GUARD"

// Kinds of message the program sends its guard
const (
	guardModes   = 'T'
	guardRestore = 'R'
	guardDone    = 'D'
)

// Runs the guard instead of the program when the binary was started as one
func init() {
	if os.Getenv(guardEnv) != "" {
		os.Exit(runGuard())
	}
}

// Program side of the guard process that restores the terminal and shows stderr however the program dies
type guard struct {
	cmd    *exec.Cmd
	ctrl   *os.File
	ack    *os.File
	real   *os.File
	marker string
	mu     sync.Mutex
	over   bool
	err    error
}

// Writes one framed message to the guard, taking stderr back from a guard that can no longer read it
func (g *guard) send(kind byte, payload []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.over {
		return
	}
	var head [5]byte
	head[0] = kind
	binary.BigEndian.PutUint32(head[1:], uint32(len(payload)))
	if _, err := g.ctrl.Write(append(head[:], payload...)); err != nil {
		g.lostLocked(fmt.Errorf("tty: guard control: %w", err))
	}
}

// Ends the guard's watch early, stderr pointed back at the terminal so writes to it cannot hit a dead pipe
func (g *guard) lostLocked(err error) {
	g.over = true
	g.err = errors.Join(err, restoreStderr(g.real))
}

// Tells the guard the bytes that undo the session as it stands
func (g *guard) restore(b []byte) { g.send(guardRestore, b) }

// Stderr as it was before the program captured it, where children write
func (g *guard) stderr() *os.File {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.over {
		return os.Stderr
	}
	return g.real
}

// Ends the session cleanly, the guard showing captured stderr and the real stderr put back before it returns
func (g *guard) finish() error {
	g.mu.Lock()
	over := g.over
	g.mu.Unlock()
	var err error
	if !over {
		if _, werr := os.Stderr.WriteString(g.marker); werr != nil {
			err = fmt.Errorf("tty: guard end marker: %w", werr)
		}
		g.send(guardDone, nil)
		var b [1]byte
		if _, rerr := g.ack.Read(b[:]); rerr != nil {
			err = errors.Join(err, fmt.Errorf("tty: guard acknowledgement: %w", rerr))
		}
	}
	g.mu.Lock()
	if !g.over {
		g.over = true
		g.err = restoreStderr(g.real)
	}
	err = errors.Join(err, g.err)
	g.mu.Unlock()
	return errors.Join(err, g.ctrl.Close(), g.ack.Close())
}

// Starts the guard on the terminal ends and moves stderr into it, closing everything it opened when it fails
func startGuard(in, out *os.File, st *ttyState) (_ *guard, err error) {
	var opened []*os.File
	var started *exec.Cmd
	defer func() {
		if err == nil {
			return
		}
		for _, f := range opened {
			err = errors.Join(err, f.Close())
		}
		if started != nil {
			err = errors.Join(err, started.Wait())
		}
	}()
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	var mark [16]byte
	if _, err := rand.Read(mark[:]); err != nil {
		return nil, err
	}
	g := &guard{marker: "\x00icebath-tty-" + hex.EncodeToString(mark[:]) + "\x00"}
	pipe := func() (*os.File, *os.File, error) {
		r, w, err := os.Pipe()
		if err == nil {
			opened = append(opened, r, w)
		}
		return r, w, err
	}
	ctrlR, ctrlW, err := pipe()
	if err != nil {
		return nil, err
	}
	ackR, ackW, err := pipe()
	if err != nil {
		return nil, err
	}
	capR, capW, err := pipe()
	if err != nil {
		return nil, err
	}
	real, err := dupStderr()
	if err != nil {
		return nil, err
	}
	opened = append(opened, real)
	g.ctrl, g.ack, g.real = ctrlW, ackR, real
	g.cmd = exec.Command(exe)
	g.cmd.Env = append(os.Environ(), guardEnv+"="+g.marker[1:len(g.marker)-1])
	if err := spawnGuard(g.cmd, []*os.File{ctrlR, ackW, capR, real, in, out}); err != nil {
		return nil, err
	}
	started = g.cmd
	opened = []*os.File{ctrlW, ackR, capW, real}
	if err := errors.Join(ctrlR.Close(), ackW.Close(), capR.Close()); err != nil {
		return nil, err
	}
	opened = []*os.File{ctrlW, ackR}
	if err := redirectStderr(capW); err != nil {
		return nil, errors.Join(err, restoreStderr(real))
	}
	go g.watch()
	g.send(guardModes, saveModes(st))
	return g, nil
}

// Waits for the guard process, taking stderr back when it ends before the session does
func (g *guard) watch() {
	err := g.cmd.Wait()
	if err == nil {
		err = errGuardExited
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.over {
		g.lostLocked(fmt.Errorf("tty: guard ended early: %w", err))
	}
}

// Read side of the stderr capture, holding everything until the terminal is back and passing writes on after that
type spool struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	live   bool
	out    io.Writer
	marker []byte
	seen   chan struct{}
	once   sync.Once
	err    error
}

// Copies the capture pipe into the spool until every writer closed it
func (s *spool) pump(r io.Reader) {
	chunk := make([]byte, 32<<10)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			s.take(chunk[:n])
		}
		if err != nil {
			s.once.Do(func() { close(s.seen) })
			return
		}
	}
}

// Takes captured bytes, cutting the end marker out and noting that it arrived
func (s *spool) take(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live {
		s.write(b)
		return
	}
	s.buf.Write(b)
	if i := bytes.Index(s.buf.Bytes(), s.marker); i >= 0 {
		rest := append([]byte(nil), s.buf.Bytes()[i+len(s.marker):]...)
		s.buf.Truncate(i)
		s.buf.Write(rest)
		s.once.Do(func() { close(s.seen) })
	}
}

// Writes what the spool held and passes everything after it straight through
func (s *spool) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.write(s.buf.Bytes())
	s.buf.Reset()
	s.live = true
}

// Writes to the real stderr, keeping the first failure
func (s *spool) write(b []byte) {
	if _, err := s.out.Write(b); err != nil && s.err == nil {
		s.err = err
	}
}

// Guard process body, exit status zero once it restored or was told nothing needs restoring and showed every byte
func runGuard() int {
	ignoreSignals()
	ctrl, ack, capture, real, in, out, err := guardFiles()
	if err != nil {
		return 2
	}
	sp := &spool{out: real, marker: []byte("\x00" + os.Getenv(guardEnv) + "\x00"), seen: make(chan struct{})}
	pumped := make(chan struct{})
	go func() {
		defer close(pumped)
		sp.pump(capture)
	}()
	var modes, undo []byte
	clean := false
	for !clean {
		var head [5]byte
		if _, err := io.ReadFull(ctrl, head[:]); err != nil {
			break
		}
		payload := make([]byte, binary.BigEndian.Uint32(head[1:]))
		if _, err := io.ReadFull(ctrl, payload); err != nil {
			break
		}
		switch head[0] {
		case guardModes:
			modes = payload
		case guardRestore:
			undo = payload
		case guardDone:
			clean = true
		}
	}
	var fails []error
	if clean {
		select {
		case <-sp.seen:
		case <-time.After(2 * time.Second):
		}
		sp.release()
		if _, err := ack.Write([]byte{1}); err != nil {
			fails = append(fails, fmt.Errorf("acknowledging the end: %w", err))
		}
	} else {
		if _, err := out.Write(undo); err != nil {
			fails = append(fails, fmt.Errorf("writing the terminal undo: %w", err))
		}
		if modes != nil {
			if err := loadModes(in, out, modes); err != nil {
				fails = append(fails, fmt.Errorf("restoring the terminal modes: %w", err))
			}
		}
		select {
		case <-pumped:
		case <-time.After(300 * time.Millisecond):
		}
		sp.release()
	}
	<-pumped
	sp.mu.Lock()
	if sp.err != nil {
		fails = append(fails, fmt.Errorf("showing captured stderr: %w", sp.err))
	}
	sp.mu.Unlock()
	if len(fails) == 0 {
		return 0
	}
	fmt.Fprintln(real, "icebath: terminal guard:", errors.Join(fails...))
	return 1
}

// Error for a guard message that does not decode
var errGuardModes = errors.New("tty: guard modes do not decode")

// Error for a guard that exited before the session ended
var errGuardExited = errors.New("exited")

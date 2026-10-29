package term

import (
	"errors"
	"os"
	"sync"
	"time"
)

// Time a closed program gets to end on its own before it is killed
const closeGrace = 2 * time.Second

// Bytes read from the program at once
const readSize = 32 << 10

// Program to run on the terminal
type Command struct {
	Path string
	Args []string
	Dir  string
	Env  []string
}

// How a program ended
type Exit struct {
	Code   int
	Signal string
	Err    error
}

// Program on a pseudo terminal feeding an emulator, with a wake channel the host waits on for output
type Session struct {
	Emu  *Emulator
	proc *Process
	cmd  Command
	wake chan struct{}
	done chan struct{}
	// Input waiting for the program and the kick that tells the writer about it
	queue [][]byte
	kick  chan struct{}
	once  sync.Once
	mu    sync.Mutex
	exit  Exit
	// Bytes the program wrote
	bytes int64
}

// Starts the command on a terminal of cols by rows that feeds a fresh emulator built with the options
func Open(c Command, cols, rows int, o Options) (*Session, error) {
	if c.Path == "" {
		return nil, errors.New("term: no program to run")
	}
	s := &Session{cmd: c, wake: make(chan struct{}, 1), done: make(chan struct{}), kick: make(chan struct{}, 1)}
	o.Reply = s
	s.Emu = New(cols, rows, o)
	p, err := startProcess(c, cols, rows)
	if err != nil {
		return nil, err
	}
	s.proc = p
	go s.read()
	go s.write()
	return s, nil
}

// Command the session runs
func (s *Session) Command() Command { return s.cmd }

// Process id of the program
func (s *Session) Pid() int { return s.proc.Pid() }

// Working directory of the program, what it announced over OSC 7, else what the kernel reports
func (s *Session) Cwd() string {
	if dir := s.Emu.Cwd(); dir != "" {
		return dir
	}
	return s.proc.Cwd()
}

// Bytes the program has written so far
func (s *Session) Bytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes
}

// Channel that gets a token whenever output arrived since the host last took one
func (s *Session) Wake() <-chan struct{} { return s.wake }

// Channel closed once the program has ended
func (s *Session) Done() <-chan struct{} { return s.done }

// How the program ended, meaningful once Done is closed
func (s *Session) Exit() Exit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exit
}

// Reports whether the program has ended
func (s *Session) Exited() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// Queues bytes for the program without waiting, dropping them once it has ended
func (s *Session) Input(b []byte) {
	if len(b) == 0 || s.Exited() {
		return
	}
	s.mu.Lock()
	s.queue = append(s.queue, append([]byte(nil), b...))
	s.mu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Queues an answer the emulator owes the program
func (s *Session) Write(b []byte) (int, error) {
	s.Input(b)
	return len(b), nil
}

// Tells the program and the emulator the terminal is cols by rows
func (s *Session) Resize(cols, rows int) {
	cols, rows = max(cols, 1), max(rows, 1)
	s.Emu.Resize(cols, rows)
	if !s.Exited() {
		s.proc.Resize(cols, rows)
	}
}

// Ends the program and the goroutines feeding it
func (s *Session) Close() {
	s.once.Do(func() {
		s.proc.Close()
		<-s.done
	})
}

// Copies program output into the emulator and wakes the host until the program ends
func (s *Session) read() {
	buf := make([]byte, readSize)
	for {
		n, err := s.proc.Read(buf)
		if n > 0 {
			s.Emu.Write(buf[:n])
			s.mu.Lock()
			s.bytes += int64(n)
			s.mu.Unlock()
			s.signal()
		}
		if err != nil {
			break
		}
	}
	<-s.proc.done
	s.mu.Lock()
	s.exit = s.proc.exit
	s.mu.Unlock()
	close(s.done)
	s.signal()
}

// Hands queued input to the program in order
func (s *Session) write() {
	for {
		select {
		case <-s.kick:
		case <-s.done:
			return
		}
		for {
			s.mu.Lock()
			if len(s.queue) == 0 {
				s.mu.Unlock()
				break
			}
			b := s.queue[0]
			s.queue[0] = nil
			s.queue = s.queue[1:]
			s.mu.Unlock()
			if _, err := s.proc.Write(b); err != nil && (errors.Is(err, os.ErrClosed) || s.Exited()) {
				return
			}
		}
	}
}

// Leaves one token on the wake channel
func (s *Session) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

package pty

import (
	"io"
	"sync"
	"unicode/utf8"
)

// Size, directory, environment and terminal name a shell starts with
type spawnOptions struct {
	Cols int
	Rows int
	Cwd  string
	Env  map[string]string
	Name string
}

// Running shell on a pseudo terminal
type ptyProcess interface {
	Write(data string)
	Resize(cols, rows int) error
	Kill()
	Pid() int
}

// Length of the prefix of b that ends on a whole UTF-8 sequence, holding back a trailing partial one
func completeUTF8(b []byte) int {
	for back := 1; back <= utf8.UTFMax-1 && back <= len(b); back++ {
		c := b[len(b)-back]
		if c < 0x80 {
			return len(b)
		}
		if c >= 0xC0 {
			need := 2
			switch {
			case c >= 0xF0:
				need = 4
			case c >= 0xE0:
				need = 3
			}
			if need > back {
				return len(b) - back
			}
			return len(b)
		}
	}
	return len(b)
}

// Reads r until it fails, handing every chunk on as text without splitting a UTF-8 sequence
func pumpUTF8(r io.Reader, onData func(string)) {
	buf := make([]byte, 64*1024)
	var carry []byte
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := append(carry, buf[:n]...)
			cut := completeUTF8(chunk)
			if cut > 0 {
				onData(string(chunk[:cut]))
			}
			carry = append([]byte(nil), chunk[cut:]...)
		}
		if err != nil {
			if len(carry) > 0 {
				onData(string(carry))
			}
			return
		}
	}
}

// Hands writes to the terminal in order on its own goroutine, dropping the backlog when a write fails
type writeQueue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	pending [][]byte
	closed  bool
}

func newWriteQueue(w io.Writer) *writeQueue {
	q := &writeQueue{}
	q.cond = sync.NewCond(&q.mu)
	go q.run(w)
	return q
}

// Queues data behind every earlier write, dropping it once the terminal is closed
func (q *writeQueue) push(data string) {
	if data == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.pending = append(q.pending, []byte(data))
	q.cond.Signal()
}

// Stops the writer and drops what it has not written
func (q *writeQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.pending = nil
	q.mu.Unlock()
	q.cond.Broadcast()
}

func (q *writeQueue) run(w io.Writer) {
	for {
		q.mu.Lock()
		for len(q.pending) == 0 && !q.closed {
			q.cond.Wait()
		}
		if q.closed {
			q.mu.Unlock()
			return
		}
		next := q.pending[0]
		q.pending = q.pending[1:]
		q.mu.Unlock()
		if _, err := w.Write(next); err != nil {
			q.mu.Lock()
			q.pending = nil
			q.mu.Unlock()
		}
	}
}

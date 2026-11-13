package logging

import (
	"bytes"
	"context"
	"os"
)

// Size of one read the redacting pump takes from a redirected stream
const pumpBuffer = 64 << 10

// Standard streams pointed at a pipe whose pump redacts every chunk on its way to the file they wrote before
type redirect struct {
	targets  []**os.File
	original *os.File
	w        *os.File
	done     chan struct{}
}

// Routes stdout and stderr of a detached process through redaction once, one pump keeping order on a shared file
func (s *System) RedactDetachedStdio() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var pending []**os.File
	for _, t := range []**os.File{&os.Stdout, &os.Stderr} {
		if *t != nil && !s.redirectedLocked(*t) {
			pending = append(pending, t)
		}
	}
	if len(pending) == 2 && sameFile(*pending[0], *pending[1]) {
		return s.redirectLocked(pending...)
	}
	for _, t := range pending {
		if err := s.redirectLocked(t); err != nil {
			return err
		}
	}
	return nil
}

// Reports whether f is the pipe of a redirected stream, caller holding mu
func (s *System) redirectedLocked(f *os.File) bool {
	for _, rd := range s.stdio {
		if rd.w == f {
			return true
		}
	}
	return false
}

// Reports whether two open files are the same file
func sameFile(a, b *os.File) bool {
	sa, err := a.Stat()
	if err != nil {
		return false
	}
	sb, err := b.Stat()
	return err == nil && os.SameFile(sa, sb)
}

// Points every target at one new pipe pumped redacted into the file the first target held, caller holding mu
func (s *System) redirectLocked(targets ...**os.File) error {
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	rd := &redirect{targets: targets, original: *targets[0], w: w, done: make(chan struct{})}
	go s.pump(r, rd.original, rd.done)
	for _, t := range targets {
		*t = w
	}
	s.stdio = append(s.stdio, rd)
	return nil
}

// Copies each chunk of the pipe to dst redacted, holding a partial line back only while more is pending
func (s *System) pump(r, dst *os.File, done chan struct{}) {
	defer close(done)
	defer r.Close()
	buf := make([]byte, pumpBuffer)
	var pending []byte
	for {
		n, err := r.Read(buf)
		data := append(pending, buf[:n]...)
		pending = nil
		if n == len(buf) && err == nil {
			if i := bytes.LastIndexByte(data, '\n'); i >= 0 {
				data, pending = data[:i+1], bytes.Clone(data[i+1:])
			}
		}
		if len(data) > 0 {
			_, _ = dst.WriteString(s.redact.Redact(context.Background(), string(data)))
		}
		if err != nil {
			return
		}
	}
}

// Puts the original streams back and waits until every redacted byte reached them
func (s *System) restoreStdio() {
	s.mu.Lock()
	streams := s.stdio
	s.stdio = nil
	for _, rd := range streams {
		for _, t := range rd.targets {
			if *t == rd.w {
				*t = rd.original
			}
		}
	}
	s.mu.Unlock()
	for _, rd := range streams {
		_ = rd.w.Close()
		<-rd.done
	}
}

package portaudio

import (
	"fmt"
	"sync"
	"unsafe"
)

// Where a stream is in its life: open, closing while its last calls drain, closed
type state uint8

const (
	streamOpen state = iota
	streamClosing
	streamClosed
)

// Open PortAudio stream whose calls take turns so none runs on a freed stream; only Pa_AbortStream, of Abort and Close, runs beside a
// blocking Read or Write, since aborting is how a host API gives up a wait for frames that may never come
type Stream struct {
	id       uintptr
	channels int
	onInput  InputCallback
	onOutput outputCallback

	mu       sync.Mutex
	changed  *sync.Cond
	ptr      uintptr
	state    state
	io       bool
	busy     bool
	aborting bool
}

func newStream(id uintptr, channels int, onInput InputCallback, onOutput outputCallback) *Stream {
	s := &Stream{id: id, channels: channels, onInput: onInput, onOutput: onOutput}
	s.changed = sync.NewCond(&s.mu)
	return s
}

// Waits until no other call is inside PortAudio, then marks a blocking read or write or a control call as running
func (s *Stream) enter(io bool) (uintptr, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.state == streamOpen && (s.io || s.busy || s.aborting) {
		s.changed.Wait()
	}
	if s.state != streamOpen {
		return 0, ErrClosed
	}
	if io {
		s.io = true
	} else {
		s.busy = true
	}
	return s.ptr, nil
}

// Marks the call returned, whether Close began while it ran
func (s *Stream) leave(io bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if io {
		s.io = false
	} else {
		s.busy = false
	}
	s.changed.Broadcast()
	return s.state != streamOpen
}

// Whether the stream runs
func (s *Stream) Active() (bool, error) {
	ptr, err := s.enter(false)
	if err != nil {
		return false, err
	}
	code := paIsStreamActive(ptr)
	s.leave(false)
	if err := check(code, "query stream activity"); err != nil {
		return false, err
	}
	return code == 1, nil
}

// Starts processing, a running stream no error
func (s *Stream) Start() error {
	ptr, err := s.enter(false)
	if err != nil {
		return err
	}
	code := paStartStream(ptr)
	s.leave(false)
	if code == paStreamIsNotStopped {
		return nil
	}
	return check(code, "start stream")
}

// Stops once the queued buffers played, a stopped stream no error
func (s *Stream) Stop() error {
	ptr, err := s.enter(false)
	if err != nil {
		return err
	}
	code := paStopStream(ptr)
	s.leave(false)
	if code == paStreamIsStopped {
		return nil
	}
	return check(code, "stop stream")
}

// Stops at once, the queued buffers dropped, beside a Read or Write in progress so it returns
func (s *Stream) Abort() error {
	s.mu.Lock()
	for s.state == streamOpen && (s.busy || s.aborting) {
		s.changed.Wait()
	}
	if s.state != streamOpen {
		s.mu.Unlock()
		return ErrClosed
	}
	s.aborting = true
	ptr := s.ptr
	s.mu.Unlock()
	code := paAbortStream(ptr)
	s.mu.Lock()
	s.aborting = false
	s.changed.Broadcast()
	s.mu.Unlock()
	if code == paStreamIsStopped {
		return nil
	}
	return check(code, "abort stream")
}

// Closes the stream once no call is inside PortAudio, aborting a Read or Write in progress first; every call from then on returns
// ErrClosed. Never from the stream's own callback, which PortAudio waits for while closing
func (s *Stream) Close() error {
	s.mu.Lock()
	if s.state != streamOpen {
		for s.state == streamClosing {
			s.changed.Wait()
		}
		s.mu.Unlock()
		return ErrClosed
	}
	s.state = streamClosing
	s.changed.Broadcast()
	for s.busy || s.aborting {
		s.changed.Wait()
	}
	ptr := s.ptr
	for s.io {
		s.aborting = true
		s.mu.Unlock()
		paAbortStream(ptr)
		s.mu.Lock()
		s.aborting = false
		if s.io {
			s.changed.Wait()
		}
	}
	s.mu.Unlock()
	var code int32
	quietly(func() { code = paCloseStream(ptr) })
	streams.Delete(s.id)
	s.mu.Lock()
	s.state, s.ptr = streamClosed, 0
	s.changed.Broadcast()
	s.mu.Unlock()
	return check(code, "close stream")
}

// Blocks until the frames are captured, an input overflow reported as true; ErrClosed when Close cut the read short
func (s *Stream) Read(frames int) ([]int16, bool, error) {
	if frames < 0 {
		return nil, false, fmt.Errorf("read stream: negative frame count %d", frames)
	}
	buf := make([]int16, frames*s.channels)
	ptr, err := s.enter(true)
	if err != nil {
		return nil, false, err
	}
	if len(buf) == 0 {
		s.leave(true)
		return buf, false, nil
	}
	code := paReadStream(ptr, unsafe.Pointer(&buf[0]), uint(frames))
	closing := s.leave(true)
	switch {
	case code == paInputOverflowed:
		return buf, true, nil
	case code < 0 && closing:
		return nil, false, ErrClosed
	}
	if err := check(code, "read stream"); err != nil {
		return nil, false, err
	}
	return buf, false, nil
}

// Frames that can be read without blocking
func (s *Stream) ReadAvailable() (int, error) {
	ptr, err := s.enter(false)
	if err != nil {
		return 0, err
	}
	code := paGetStreamReadAvailable(ptr)
	s.leave(false)
	if err := check(code, "query read availability"); err != nil {
		return 0, err
	}
	return int(code), nil
}

// Blocks until the interleaved frames are queued, an output underflow reported as true; ErrClosed when Close cut the write short
func (s *Stream) Write(data []int16) (bool, error) {
	if len(data)%s.channels != 0 {
		return false, fmt.Errorf("write stream: %w: %d samples for %d channels", ErrChannelMismatch, len(data), s.channels)
	}
	ptr, err := s.enter(true)
	if err != nil {
		return false, err
	}
	frames := len(data) / s.channels
	if frames == 0 {
		s.leave(true)
		return false, nil
	}
	code := paWriteStream(ptr, unsafe.Pointer(&data[0]), uint(frames))
	closing := s.leave(true)
	switch {
	case code == paOutputUnderflowed:
		return true, nil
	case code < 0 && closing:
		return false, ErrClosed
	}
	return false, check(code, "write stream")
}

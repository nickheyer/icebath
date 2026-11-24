package portaudioutil

import (
	"fmt"
	"sync"
	"unsafe"
)

// Where a stream is in its life: open, closing while its last calls drain, closed
type streamState uint8

const (
	streamOpen streamState = iota
	streamClosing
	streamClosed
)

// Open PortAudio stream whose calls are tracked so none ever runs on a freed stream. PortAudio documents no call as safe beside another on the same stream, so every call takes its turn, with one exception: Pa_AbortStream, issued by Abort and Close, may run while a blocking Read or Write is inside PortAudio, because aborting is how a host API is told to give up a wait for frames that may never come
type stream struct {
	id       uintptr
	channels int
	onInput  InputCallback
	onOutput outputCallback

	mu       sync.Mutex
	changed  *sync.Cond
	ptr      uintptr
	state    streamState
	io       bool
	busy     bool
	aborting bool
}

func newStream(id uintptr, channels int, onInput InputCallback, onOutput outputCallback) *stream {
	s := &stream{id: id, channels: channels, onInput: onInput, onOutput: onOutput}
	s.changed = sync.NewCond(&s.mu)
	return s
}

// Waits until no other call is inside PortAudio, then marks a blocking read or write (io) or a control call as running; ErrClosed once Close has begun
func (s *stream) enter(io bool) (uintptr, error) {
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

// Marks the call as returned, reporting whether Close began while it ran
func (s *stream) leave(io bool) (closing bool) {
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

// Whether the stream is running; waits for a Read or Write in progress to return
func (s *stream) Active() (bool, error) {
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

// Starts processing, a stream already running being no error
func (s *stream) Start() error {
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

// Stops once the queued buffers have played, a stream already stopped being no error; waits for a Read or Write in progress to return, which Abort and Close cut short
func (s *stream) Stop() error {
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

// Stops at once, dropping the queued buffers; runs beside a Read or Write in progress so that it returns, and waits only for a control call in progress; a stream already stopped is no error
func (s *stream) Abort() error {
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

// Closes the stream once no call is inside PortAudio, aborting it first when a Read or Write is in progress so that the call returns, and forgets its callback; every call from the moment Close begins, a second Close included, returns ErrClosed. Not to be called from the stream's own callback, which PortAudio waits for while closing
func (s *stream) Close() error {
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

// Blocks until the frames are captured, reporting an input overflow as true instead of failing on it; ErrClosed when Close cut the read short
func (s *stream) Read(frames int) ([]int16, bool, error) {
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

// Frames that can be read without blocking; waits for a Read or Write in progress to return
func (s *stream) ReadAvailable() (int, error) {
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

// Blocks until the interleaved frames are queued, reporting an output underflow as true instead of failing on it; data must hold whole frames; ErrClosed when Close cut the write short
func (s *stream) Write(data []int16) (bool, error) {
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

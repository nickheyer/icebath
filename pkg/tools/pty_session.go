package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

const (
	WSCloseProcessExited       = 4410
	WSCloseProcessExitedReason = "Process exited"
	WSCloseSuperseded          = 4409
	WSCloseViewerStalled       = 1001
	WSCloseViewerStalledReason = "Terminal viewer stopped reading; reconnect"
)

// Form feed asking a live full screen TUI for one complete redraw
const TUIForceRedraw = "\x0c"

// Message of a registry whose every slot holds a viewed PTY
const RegistryFullMessage = "Too many chat terminals are open in other tabs; close one and try again."

var (
	ptyViewerSendTimeout       = 5 * time.Second
	ptyReplayMinBytesPerSecond = 32 * 1024
	ptyReplayFloorBytes        = 16 * 1024
	ptyViewerCloseTimeout      = 5 * time.Second
	ptyMonotonic               = time.Now
	ptySessionLog              *slog.Logger
)

// Logger of keep alive sessions, the default logger unless a test installs one
func ptySessionLogger() *slog.Logger {
	if ptySessionLog != nil {
		return ptySessionLog
	}
	return slog.Default()
}

// Browser socket a keep alive session streams to, implemented by pkg/web over its websocket
type PtyViewer interface {
	SendBytes(ctx context.Context, data []byte) error
	SendText(ctx context.Context, text string) error
	Close(ctx context.Context, code int, reason string) error
}

// RingBuffer keeps only the most recent capacity bytes appended to it
type RingBuffer struct {
	mu        sync.Mutex
	capacity  int
	buf       []byte
	truncated bool
}

// Empty ring holding at most capacity bytes
func NewRingBuffer(capacity int) *RingBuffer { return &RingBuffer{capacity: capacity} }

// Appends data and drops the oldest bytes beyond capacity
func (r *RingBuffer) Append(data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, data...)
	if overflow := len(r.buf) - r.capacity; overflow > 0 {
		r.buf = append([]byte(nil), r.buf[min(overflow, len(r.buf)):]...)
		r.truncated = true
	}
}

// Copy of the held bytes
func (r *RingBuffer) Snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte{}, r.buf...)
}

// Whether any appended byte was ever dropped
func (r *RingBuffer) Truncated() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.truncated
}

// Viewer callback that panicked, re raised by the caller that owns the viewer
type ptyViewerPanic struct{ value any }

func (p *ptyViewerPanic) Error() string { return fmt.Sprint("viewer panicked: ", p.value) }

// Runs one viewer call bounded by timeout, cancelled with parent, a panic returned as ptyViewerPanic
func viewerCall(parent context.Context, timeout time.Duration, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(parent, max(timeout, 0))
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- &ptyViewerPanic{r}
			}
		}()
		done <- fn(ctx)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Closes a viewer with code and reason, waiting at most five seconds and ignoring every failure
func closePtyViewer(ws PtyViewer, code int, reason string) {
	if ws == nil {
		return
	}
	_ = viewerCall(context.Background(), ptyViewerCloseTimeout, func(ctx context.Context) error { return ws.Close(ctx, code, reason) })
}

// Pids of the live ancestors of pid, empty when the process is gone or unreadable
var ptyProcessAncestors = func(pid int) map[int]bool {
	out := map[int]bool{}
	parents, err := proc.Parents(pid)
	if err != nil {
		return out
	}
	for _, p := range parents {
		out[p] = true
	}
	return out
}

// Profile and resume segments a keep alive key was registered under, empty for a key without them
func keySegments(key string) (string, string) {
	parts := strings.Split(key, "\x00")
	if len(parts) >= 3 {
		return parts[1], parts[2]
	}
	return "", ""
}

// Lock a waiter can abandon when its context ends
type ptyLock chan struct{}

func newPtyLock() ptyLock { return make(ptyLock, 1) }

// Takes the lock, false when ctx ended first
func (l ptyLock) lock(ctx context.Context) bool {
	select {
	case l <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (l ptyLock) unlock() { <-l }

// Options of a keep alive session, a nil DrainExecutor running every read on its own goroutine
type PtySessionOptions struct {
	BufferCap         int
	ReadTimeout       time.Duration
	ActiveSessionFile string
	DrainExecutor     func(task func())
}

// In flight bridge write a reattach or close cancels
type ptyInputTask struct{ cancel context.CancelFunc }

// PtySession keeps one PTY alive across browser sockets, buffering its output for replay
type PtySession struct {
	Key               string
	Bridge            TerminalBridge
	ActiveSessionFile string
	Buffer            *RingBuffer

	readTimeout   time.Duration
	drainExecutor func(task func())

	mu                   sync.Mutex
	activeSessionCleanup func()
	replayLimit          int
	alive                bool
	attached             bool
	lastDetachedAt       time.Time
	ws                   PtyViewer
	attachGeneration     int
	outputLock           ptyLock
	inputTask            *ptyInputTask
	outputs              int
	outputArrived        chan struct{}
	drainFailure         any

	viewerMu    sync.Mutex
	writeLock   ptyLock
	drainCtx    context.Context
	drainCancel context.CancelFunc
	drainDone   chan struct{}
	closeOnce   sync.Once
	closeDone   chan struct{}
}

// Session over a spawned bridge, detached and alive until its drain sees end of file
func NewPtySession(key string, bridge TerminalBridge, o PtySessionOptions) *PtySession {
	s := &PtySession{
		Key: key, Bridge: bridge, ActiveSessionFile: o.ActiveSessionFile, Buffer: NewRingBuffer(o.BufferCap),
		readTimeout: o.ReadTimeout, drainExecutor: o.DrainExecutor, replayLimit: o.BufferCap, alive: true,
		lastDetachedAt: ptyMonotonic(), outputLock: newPtyLock(), outputArrived: make(chan struct{}),
		writeLock: newPtyLock(), closeDone: make(chan struct{}),
	}
	s.drainCtx, s.drainCancel = context.WithCancel(context.Background())
	return s
}

// Starts the single drain that reads the PTY into the buffer and the attached viewer
func (s *PtySession) Start() {
	s.mu.Lock()
	if s.drainDone != nil {
		s.mu.Unlock()
		return
	}
	s.drainDone = make(chan struct{})
	s.mu.Unlock()
	go s.drain()
}

// Whether the PTY still runs as far as the session knows
func (s *PtySession) Alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.alive
}

// Whether a viewer is attached
func (s *PtySession) Attached() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attached
}

// When the last viewer left, false while one is attached
func (s *PtySession) LastDetachedAt() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastDetachedAt, !s.lastDetachedAt.IsZero()
}

// Installs the callback the close path runs once after the PTY is gone
func (s *PtySession) SetActiveSessionCleanup(fn func()) {
	s.mu.Lock()
	s.activeSessionCleanup = fn
	s.mu.Unlock()
}

// Viewer currently attached
func (s *PtySession) currentViewer() PtyViewer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ws
}

type ptyReadResult struct {
	chunk    []byte
	err      error
	panicked any
	failed   bool
}

// Submits one blocking read to the drain executor
func (s *PtySession) submitRead(results chan<- ptyReadResult) {
	task := func() {
		defer func() {
			if r := recover(); r != nil {
				results <- ptyReadResult{panicked: r, failed: true}
			}
		}()
		chunk, err := s.Bridge.Read(s.readTimeout)
		results <- ptyReadResult{chunk: chunk, err: err}
	}
	if s.drainExecutor != nil {
		s.drainExecutor(task)
		return
	}
	go task()
}

// Reads the PTY until end of file, buffering every chunk and forwarding it to the attached viewer
func (s *PtySession) drain() {
	defer close(s.drainDone)
	ctx := s.drainCtx
	for s.Alive() {
		results := make(chan ptyReadResult, 1)
		s.submitRead(results)
		var r ptyReadResult
		select {
		case r = <-results:
		case <-ctx.Done():
			return
		}
		if r.failed {
			s.mu.Lock()
			s.drainFailure = r.panicked
			s.mu.Unlock()
			return
		}
		chunk := r.chunk
		if r.err != nil {
			chunk = nil
		}
		if chunk == nil {
			s.mu.Lock()
			s.alive = false
			ws, lock := s.ws, s.outputLock
			s.mu.Unlock()
			if !lock.lock(ctx) {
				return
			}
			if s.currentViewer() == ws {
				closePtyViewer(ws, WSCloseProcessExited, WSCloseProcessExitedReason)
			}
			lock.unlock()
			return
		}
		if len(chunk) == 0 {
			continue
		}
		s.Buffer.Append(chunk)
		s.mu.Lock()
		s.outputs++
		close(s.outputArrived)
		s.outputArrived = make(chan struct{})
		ws, lock := s.ws, s.outputLock
		s.mu.Unlock()
		if ws == nil {
			continue
		}
		if !lock.lock(ctx) {
			return
		}
		var err error
		if s.currentViewer() == ws {
			err = viewerCall(ctx, ptyViewerSendTimeout, func(c context.Context) error { return ws.SendBytes(c, chunk) })
		}
		lock.unlock()
		if ctx.Err() != nil {
			return
		}
		if err != nil && s.currentViewer() == ws {
			s.Detach(ws)
			closePtyViewer(ws, WSCloseViewerStalled, WSCloseViewerStalledReason)
		}
	}
}

// Serializes input from the attached viewer, discarding bytes from a superseded one
func (s *PtySession) Write(ctx context.Context, ws PtyViewer, data []byte, timeout time.Duration, fence PtyWriteFence) (bool, error) {
	if !s.writeLock.lock(ctx) {
		return false, ctx.Err()
	}
	defer s.writeLock.unlock()
	s.mu.Lock()
	if s.ws != ws || !s.alive {
		s.mu.Unlock()
		return true, nil
	}
	generation := s.attachGeneration
	inputCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	task := &ptyInputTask{cancel: cancel}
	s.inputTask = task
	s.mu.Unlock()
	var bridgeFence PtyWriteFence
	if fence != nil {
		bridgeFence = func(write PtyWriteAttempt) (int, bool, error) {
			return fence(func() (int, bool, error) {
				s.viewerMu.Lock()
				defer s.viewerMu.Unlock()
				s.mu.Lock()
				admitted := s.ws == ws && s.attachGeneration == generation && s.alive
				s.mu.Unlock()
				if !admitted {
					return 0, false, nil
				}
				return write()
			})
		}
	}
	done := make(chan ptyWriteResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- ptyWriteResult{panicked: r}
			}
		}()
		delivered, err := s.Bridge.Write(inputCtx, data, timeout, bridgeFence)
		done <- ptyWriteResult{delivered: delivered, err: err}
	}()
	var r ptyWriteResult
	select {
	case r = <-done:
		if r.panicked != nil {
			panic(r.panicked)
		}
	case <-inputCtx.Done():
	}
	delivered, err := r.delivered, r.err
	cancelled := inputCtx.Err() != nil
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inputTask == task {
		s.inputTask = nil
	}
	if cancelled {
		if s.attachGeneration != generation {
			return false, nil
		}
		return false, inputCtx.Err()
	}
	if err != nil {
		return false, err
	}
	if !delivered && s.ws == ws && s.attachGeneration == generation {
		s.alive = false
	}
	return delivered, nil
}

// Resizes the PTY for the attached viewer of a live session
func (s *PtySession) Resize(ws PtyViewer, cols, rows int) {
	s.mu.Lock()
	ok := s.ws == ws && s.alive
	s.mu.Unlock()
	if ok {
		s.Bridge.Resize(cols, rows)
	}
}

// PTY columns and rows, false where the bridge cannot tell
func (s *PtySession) Size() (int, int, bool) {
	if sized, ok := s.Bridge.(interface{ Size() (int, int, bool) }); ok {
		return sized.Size()
	}
	return 0, 0, false
}

// Chunks of output read from the program so far
func (s *PtySession) Outputs() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.outputs
}

// Returns once output past since arrived, the session ended, or timeout passed
func (s *PtySession) WaitForOutput(ctx context.Context, since int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		s.mu.Lock()
		if !s.alive || s.outputs != since {
			s.mu.Unlock()
			return nil
		}
		arrived := s.outputArrived
		s.mu.Unlock()
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		timer := time.NewTimer(remaining)
		select {
		case <-arrived:
			timer.Stop()
		case <-timer.C:
			return nil
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

// Replay options, InitialText receiving whether the replay leaves out older output
type PtyAttachOptions struct {
	ForceRedraw bool
	InitialText func(trimmed bool) string
}

// Initial text that ignores whether the replay was trimmed
func PtyStaticText(text string) func(bool) string { return func(bool) string { return text } }

// Attaches a viewer, supersedes the previous one, replays the buffer and resumes live output
func (s *PtySession) Attach(ctx context.Context, ws PtyViewer, o PtyAttachOptions) (bool, error) {
	s.mu.Lock()
	s.attachGeneration++
	generation := s.attachGeneration
	if s.inputTask != nil {
		s.inputTask.cancel()
	}
	s.mu.Unlock()
	for !s.viewerMu.TryLock() {
		select {
		case <-time.After(time.Millisecond):
		case <-ctx.Done():
			return false, ctx.Err()
		}
		s.mu.Lock()
		stale := s.attachGeneration != generation
		s.mu.Unlock()
		if stale {
			return false, nil
		}
	}
	s.mu.Lock()
	if s.attachGeneration != generation {
		s.mu.Unlock()
		s.viewerMu.Unlock()
		return false, nil
	}
	oldWs := s.ws
	s.ws = ws
	s.attached = true
	s.lastDetachedAt = time.Time{}
	lock := newPtyLock()
	lock <- struct{}{}
	s.outputLock = lock
	replayLimit := s.replayLimit
	s.mu.Unlock()
	s.viewerMu.Unlock()
	ok, err := s.replayTo(ctx, ws, oldWs, lock, replayLimit, o.InitialText)
	if err != nil {
		s.Detach(ws)
		var p *ptyViewerPanic
		if errors.As(err, &p) {
			panic(p.value)
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, nil
	}
	if !ok || s.currentViewer() != ws {
		return false, nil
	}
	if !s.Alive() {
		s.Detach(ws)
		closePtyViewer(ws, WSCloseProcessExited, WSCloseProcessExitedReason)
		return false, nil
	}
	if o.ForceRedraw {
		return s.Write(ctx, ws, []byte(TUIForceRedraw), PtyWriteTimeout, nil)
	}
	return true, nil
}

// Sends the initial text and the newest replay frame under the viewer output lock
func (s *PtySession) replayTo(ctx context.Context, ws, oldWs PtyViewer, lock ptyLock, replayLimit int, initialText func(bool) string) (ok bool, err error) {
	defer lock.unlock()
	defer func() {
		if r := recover(); r != nil {
			ok, err = false, &ptyViewerPanic{r}
		}
	}()
	held := s.Buffer.Snapshot()
	snap := held[len(held)-min(len(held), max(replayLimit, 0)):]
	trimmed := s.Buffer.Truncated() || len(snap) < len(held)
	if oldWs != ws {
		closePtyViewer(oldWs, WSCloseSuperseded, "")
	}
	if s.currentViewer() != ws {
		return false, nil
	}
	if initialText != nil {
		text := initialText(trimmed)
		if err := viewerCall(ctx, ptyViewerSendTimeout, func(c context.Context) error { return ws.SendText(c, text) }); err != nil {
			return false, err
		}
	}
	if s.currentViewer() != ws {
		return false, nil
	}
	if len(snap) > 0 || initialText != nil {
		replayTimeout := ptyViewerSendTimeout + time.Duration(float64(len(snap))/float64(ptyReplayMinBytesPerSecond)*float64(time.Second))
		if err := viewerCall(ctx, replayTimeout, func(c context.Context) error { return ws.SendBytes(c, snap) }); err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				s.mu.Lock()
				s.replayLimit = max(ptyReplayFloorBytes, len(snap)/2)
				s.mu.Unlock()
			}
			return false, err
		}
	}
	return true, nil
}

// Marks the session detached when ws is the attached viewer, a superseded viewer changes nothing
func (s *PtySession) Detach(ws PtyViewer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ws != ws {
		return
	}
	s.ws = nil
	s.attached = false
	s.lastDetachedAt = ptyMonotonic()
}

// Whether pid is the PTY leader or one of its descendants, false for a nil pid or a pidless bridge
func (s *PtySession) HostsPid(pid *int) bool {
	if pid == nil {
		return false
	}
	leader := s.Bridge.Pid()
	if leader <= 0 {
		return false
	}
	return leader == *pid || ptyProcessAncestors(*pid)[leader]
}

// Ends the session once and waits for the close, which outlives a cancelled caller
func (s *PtySession) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.alive = false
		s.attachGeneration++
		if s.inputTask != nil {
			s.inputTask.cancel()
		}
		ws := s.ws
		s.mu.Unlock()
		s.Detach(ws)
		go s.closeNow(ws)
	})
	select {
	case <-s.closeDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Closes the viewer, stops the drain, closes the bridge and removes the active session marker
func (s *PtySession) closeNow(ws PtyViewer) {
	defer close(s.closeDone)
	closePtyViewer(ws, WSCloseProcessExited, "")
	s.drainCancel()
	s.mu.Lock()
	drainDone := s.drainDone
	s.mu.Unlock()
	if drainDone != nil {
		<-drainDone
		s.mu.Lock()
		failure := s.drainFailure
		s.mu.Unlock()
		if failure != nil {
			ptySessionLogger().Warn("PTY output drain failed", "error", fmt.Sprint(failure))
		}
	}
	func() {
		defer func() { _ = recover() }()
		_ = s.Bridge.Close()
	}()
	if s.ActiveSessionFile != "" {
		_ = os.Remove(s.ActiveSessionFile)
	}
	s.mu.Lock()
	cleanup := s.activeSessionCleanup
	s.mu.Unlock()
	if cleanup != nil {
		func() {
			defer func() { _ = recover() }()
			cleanup()
		}()
		s.mu.Lock()
		s.activeSessionCleanup = nil
		s.mu.Unlock()
	}
}

// RegistryFullError reports that every keep alive slot holds a PTY some tab is attached to
type RegistryFullError struct{ Message string }

func (e *RegistryFullError) Error() string { return e.Message }

// Registry full error with the given message, the default message when empty
func NewRegistryFull(message string) *RegistryFullError {
	if message == "" {
		message = RegistryFullMessage
	}
	return &RegistryFullError{Message: message}
}

// Reaps idle and dead keep alive sessions every interval until ctx ends
func RunPtyReaper(ctx context.Context, registry *PtySessionRegistry, interval time.Duration) {
	for {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		registry.ReapIdle(ctx, time.Time{})
	}
}

// Options of a keep alive registry
type PtySessionRegistryOptions struct {
	TTL           time.Duration
	MaxSessions   int
	BufferCap     int
	ReadTimeout   time.Duration
	DrainExecutor func(task func())
	RetainIdle    bool
	ReapIdle      func(ctx context.Context, now time.Time)
}

// PtySessionRegistry tracks keep alive sessions by attach key under one spawn reservation
type PtySessionRegistry struct {
	o                PtySessionRegistryOptions
	mu               sync.Mutex
	sessions         map[string]*PtySession
	order            []string
	closed           bool
	spawnLock        ptyLock
	spawnCleanup     sync.WaitGroup
	backgroundCloses sync.WaitGroup
}

// Empty registry taking spawns
func NewPtySessionRegistry(o PtySessionRegistryOptions) *PtySessionRegistry {
	return &PtySessionRegistry{o: o, sessions: map[string]*PtySession{}, spawnLock: newPtyLock()}
}

// Registers a session under key, keeping insertion order
func (r *PtySessionRegistry) putLocked(key string, s *PtySession) {
	if _, ok := r.sessions[key]; !ok {
		r.order = append(r.order, key)
	}
	r.sessions[key] = s
}

// Removes and returns the session under key
func (r *PtySessionRegistry) popLocked(key string) *PtySession {
	s, ok := r.sessions[key]
	if !ok {
		return nil
	}
	delete(r.sessions, key)
	for i, k := range r.order {
		if k == key {
			r.order = append(r.order[:i:i], r.order[i+1:]...)
			break
		}
	}
	return s
}

// Session registered under key
func (r *PtySessionRegistry) Get(key string) *PtySession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[key]
}

// Keys in registration order
func (r *PtySessionRegistry) Keys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.order...)
}

type ptyAdmission struct {
	session  *PtySession
	created  bool
	err      error
	panicked any
}

// Attaches to the live session under key or spawns one, the registry owning a spawn its caller abandons
func (r *PtySessionRegistry) AttachOrSpawn(ctx context.Context, key string, spawn func() (TerminalBridge, error), activeSessionFile string) (*PtySession, bool, error) {
	if !r.spawnLock.lock(ctx) {
		return nil, false, ctx.Err()
	}
	done := make(chan ptyAdmission, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- ptyAdmission{panicked: p}
			}
		}()
		s, created, err := r.attachOrSpawnLocked(key, spawn, activeSessionFile)
		done <- ptyAdmission{session: s, created: created, err: err}
	}()
	select {
	case a := <-done:
		r.spawnLock.unlock()
		if a.panicked != nil {
			panic(a.panicked)
		}
		return a.session, a.created, a.err
	case <-ctx.Done():
		r.spawnCleanup.Add(1)
		go r.discardAdmission(done)
		return nil, false, ctx.Err()
	}
}

// Closes a session admitted after its caller disconnected, then releases the spawn reservation
func (r *PtySessionRegistry) discardAdmission(done <-chan ptyAdmission) {
	defer r.spawnCleanup.Done()
	defer r.spawnLock.unlock()
	a := <-done
	if a.err != nil || a.panicked != nil {
		ptySessionLogger().Debug("PTY spawn failed after its caller disconnected", "error", fmt.Sprint(a.err, a.panicked))
		return
	}
	if a.created {
		r.mu.Lock()
		if r.sessions[a.session.Key] == a.session {
			r.popLocked(a.session.Key)
		}
		r.mu.Unlock()
		_ = a.session.Close(context.Background())
	}
}

// Admission under the spawn reservation, reaping first and making room by evicting the oldest idle session
func (r *PtySessionRegistry) attachOrSpawnLocked(key string, spawn func() (TerminalBridge, error), activeSessionFile string) (*PtySession, bool, error) {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return nil, false, NewRegistryFull("Terminal service is shutting down.")
	}
	r.ReapIdle(context.Background(), time.Time{})
	r.mu.Lock()
	existing := r.sessions[key]
	if existing != nil && existing.Alive() {
		r.mu.Unlock()
		return existing, false, nil
	}
	if existing != nil {
		r.popLocked(key)
		r.closeInBackground(existing)
	}
	if len(r.sessions) >= r.o.MaxSessions {
		if err := r.reapOneIdleOrRaiseLocked(); err != nil {
			r.mu.Unlock()
			return nil, false, err
		}
	}
	r.mu.Unlock()
	bridge, err := spawn()
	if err != nil {
		return nil, false, err
	}
	s := NewPtySession(key, bridge, PtySessionOptions{
		BufferCap: r.o.BufferCap, ReadTimeout: r.o.ReadTimeout, ActiveSessionFile: activeSessionFile, DrainExecutor: r.o.DrainExecutor,
	})
	s.Start()
	r.mu.Lock()
	r.putLocked(key, s)
	r.mu.Unlock()
	return s, true, nil
}

// Closes every session of the same logical client except keepKey, superseding a sibling viewer
func (r *PtySessionRegistry) CloseOtherSessions(ctx context.Context, prefix, keepKey string) error {
	if !r.spawnLock.lock(ctx) {
		return ctx.Err()
	}
	defer r.spawnLock.unlock()
	var keys []string
	for _, key := range r.Keys() {
		if key != keepKey && (key == prefix || strings.HasPrefix(key, prefix+"\x00")) {
			keys = append(keys, key)
		}
	}
	for _, key := range keys {
		r.mu.Lock()
		s := r.popLocked(key)
		r.mu.Unlock()
		if s == nil {
			continue
		}
		ws := s.currentViewer()
		s.Detach(ws)
		closePtyViewer(ws, WSCloseSuperseded, "")
		if err := s.Close(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Closes unviewed sessions stranded under a rotated token for the same profile chat or lease holder
func (r *PtySessionRegistry) CloseOrphanedSessions(ctx context.Context, resume, keepKey string, holderPid *int) error {
	if resume == "" {
		return nil
	}
	profile, _ := keySegments(keepKey)
	if !r.spawnLock.lock(ctx) {
		return ctx.Err()
	}
	var doomed []*PtySession
	for _, key := range r.Keys() {
		s := r.Get(key)
		if s == nil || key == keepKey || s.Attached() {
			continue
		}
		p, res := keySegments(key)
		if (p == profile && res == resume) || s.HostsPid(holderPid) {
			r.mu.Lock()
			if r.sessions[key] == s {
				r.popLocked(key)
				doomed = append(doomed, s)
			}
			r.mu.Unlock()
		}
	}
	r.spawnLock.unlock()
	for _, s := range doomed {
		if err := s.Close(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Detaches ws from the session under key
func (r *PtySessionRegistry) Detach(key string, ws PtyViewer) {
	if s := r.Get(key); s != nil {
		s.Detach(ws)
	}
}

// Whether s has had no viewer for longer than the retention TTL
func (r *PtySessionRegistry) Expired(s *PtySession, now time.Time) bool {
	if now.IsZero() {
		now = ptyMonotonic()
	}
	last, ok := s.LastDetachedAt()
	return !s.Attached() && ok && now.Sub(last) > r.o.TTL
}

// Whether s ended, its process died with a helper holding the PTY, or it expired
func (r *PtySessionRegistry) reapable(s *PtySession, now time.Time) bool {
	return !s.Alive() || !s.Bridge.IsAlive() || r.Expired(s, now)
}

// Runs the reap pass an embedding registry installed, the default pass otherwise
func (r *PtySessionRegistry) ReapIdle(ctx context.Context, now time.Time) {
	if r.o.ReapIdle != nil {
		r.o.ReapIdle(ctx, now)
		return
	}
	r.reapIdleDefault(ctx, now)
}

// Closes every dead or expired session, skipping a key a concurrent reap or reattach already changed
func (r *PtySessionRegistry) reapIdleDefault(ctx context.Context, now time.Time) {
	if now.IsZero() {
		now = ptyMonotonic()
	}
	type entry struct {
		key string
		s   *PtySession
	}
	var doomed []entry
	for _, key := range r.Keys() {
		if s := r.Get(key); s != nil && r.reapable(s, now) {
			doomed = append(doomed, entry{key, s})
		}
	}
	for _, d := range doomed {
		still := r.reapable(d.s, now)
		r.mu.Lock()
		if r.sessions[d.key] != d.s || !still {
			r.mu.Unlock()
			continue
		}
		s := r.popLocked(d.key)
		r.mu.Unlock()
		if s != nil {
			if err := s.Close(ctx); err != nil {
				return
			}
		}
	}
}

// Evicts the longest idle session to free a slot, RegistryFull when every session is viewed or idle ones are retained
func (r *PtySessionRegistry) reapOneIdleOrRaiseLocked() error {
	if r.o.RetainIdle {
		return NewRegistryFull("")
	}
	var oldest *PtySession
	var oldestAt time.Time
	for _, key := range r.order {
		s := r.sessions[key]
		at, ok := s.LastDetachedAt()
		if s.Attached() || !ok {
			continue
		}
		if oldest == nil || at.Before(oldestAt) {
			oldest, oldestAt = s, at
		}
	}
	if oldest == nil {
		return NewRegistryFull("")
	}
	r.popLocked(oldest.Key)
	r.closeInBackground(oldest)
	return nil
}

// Closes a removed session on its own goroutine that shutdown still waits for, the channel closed when done
func (r *PtySessionRegistry) closeInBackground(s *PtySession) <-chan struct{} {
	r.backgroundCloses.Add(1)
	done := make(chan struct{})
	go func() {
		defer r.backgroundCloses.Done()
		defer close(done)
		_ = s.Close(context.Background())
	}()
	return done
}

// Takes spawns again after CloseAll
func (r *PtySessionRegistry) Reopen() {
	r.mu.Lock()
	r.closed = false
	r.mu.Unlock()
}

// Refuses new spawns, waits for admitted ones, then closes every session concurrently
func (r *PtySessionRegistry) CloseAll(ctx context.Context) {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	if r.spawnLock.lock(ctx) {
		r.spawnLock.unlock()
	}
	r.spawnCleanup.Wait()
	r.mu.Lock()
	var sessions []*PtySession
	for _, key := range append([]string{}, r.order...) {
		sessions = append(sessions, r.popLocked(key))
	}
	r.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Close(ctx)
		}()
	}
	wg.Wait()
	r.backgroundCloses.Wait()
}

// Outcome of the bridge write an input task runs
type ptyWriteResult struct {
	delivered bool
	err       error
	panicked  any
}

package gateway

import (
	"bufio"
	"context"
	"errors"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Verified interpreter behind a worker handle, its pid and kernel birth
type workerIdentity struct {
	pid   int
	birth float64
}

// Owner side of one managed worker child, its control writer, demand driven frame reader and stop latch
type managedWorker struct {
	process       *workerProcess
	mu            sync.Mutex
	identity      *workerIdentity
	writeLock     sync.Mutex
	commands      chan *pyjson.Object
	closed        pythread.Event
	stopCh        chan struct{}
	stopOnce      sync.Once
	stoppedAt     *float64
	stopCause     *string
	writerStarted bool
	writerDone    chan struct{}
	readerStarted bool
	readerDone    chan struct{}
	demand        chan struct{}
	requested     bool
	frames        []any
	arrived       chan struct{}
	arrivedSet    bool
	stdout        *bufio.Reader
	stderrDone    chan struct{}
	secrets       []string
}

// Worker handle around a spawned child, nothing started yet
func newManagedWorker(process *workerProcess) *managedWorker {
	return &managedWorker{process: process, commands: make(chan *pyjson.Object, 16), stopCh: make(chan struct{}),
		writerDone: make(chan struct{}), demand: make(chan struct{}, 8), arrived: make(chan struct{}),
		stdout: bufio.NewReaderSize(process.stdout, 64*1024)}
}

// Whether the worker was closed, the closed event the active work count reads
func (w *managedWorker) Closed() bool { return w.closed.IsSet() }

// Whether a stop was latched
func (w *managedWorker) stopped() bool {
	select {
	case <-w.stopCh:
		return true
	default:
		return false
	}
}

// Recorded restart or shutdown cause of the stop that signalled this worker
func (w *managedWorker) cause() *string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stopCause
}

// Records the restart or shutdown cause carried by the stop that signals this worker
func (w *managedWorker) setCause(reason *string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopCause = reason
}

// Whether a frame is exactly the type key with that value
func isTypeFrame(frame *pyjson.Object, kind string) bool {
	return frameKeysAre(frame, "type") && frame.Vals["type"] == kind
}

// Error kind of a pipe failure, OSError unless it carries a Python class
func pipeErrorKind(err error) string {
	if kind := pyErrorKind(err); kind != "" {
		return kind
	}
	return "OSError"
}

// Control writer body, a pipe or encoding failure closing the worker
func (w *managedWorker) writeControls() {
	defer close(w.writerDone)
	for !w.closed.IsSet() {
		select {
		case frame := <-w.commands:
			if err := w.send(frame); err != nil {
				if kind := pipeErrorKind(err); kind == "OSError" || kind == "ValueError" {
					w.closed.Set()
				}
				return
			}
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Starts the control writer once the child has its bootstrap
func (w *managedWorker) startWriter() {
	w.mu.Lock()
	w.writerStarted = true
	w.mu.Unlock()
	go w.writeControls()
}

// Reader body owning stdout, one frame per demand, a failure delivered once and ending the reader
func (w *managedWorker) readFrames() {
	defer func() {
		w.process.stdout.Close()
		close(w.readerDone)
	}()
	for {
		<-w.demand
		if w.closed.IsSet() {
			return
		}
		frame, err := ReadFrame(w.stdout)
		if err != nil {
			w.deliver(err)
			return
		}
		w.deliver(frame)
	}
}

// Queues a frame or failure and wakes the owner's read
func (w *managedWorker) deliver(item any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.frames = append(w.frames, item)
	if !w.arrivedSet {
		w.arrivedSet = true
		close(w.arrived)
	}
}

// Whether a frame or failure is queued
func (w *managedWorker) hasFrames() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.frames) > 0
}

// Channel closed once a frame is queued
func (w *managedWorker) arrival() chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.arrived
}

// Oldest queued frame, a queued failure staying queued so every later read fails the same way
func (w *managedWorker) take() (*pyjson.Object, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err, failed := w.frames[0].(error); failed {
		return nil, err
	}
	frame := w.frames[0].(*pyjson.Object)
	w.frames = w.frames[1:]
	w.requested = false
	if len(w.frames) == 0 && w.arrivedSet {
		w.arrivedSet = false
		w.arrived = make(chan struct{})
	}
	return frame, nil
}

// Routes the child's stderr into the profile's bounded redacted private log
func (w *managedWorker) drainStderr(ctx context.Context, path string) {
	w.stderrDone = make(chan struct{})
	go func() {
		defer close(w.stderrDone)
		drainWorkerStderr(ctx, w.process.stderr, path, w.process.Pid, w.knownSecrets)
	}()
}

// Exact secrets this worker was handed so far
func (w *managedWorker) knownSecrets() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.secrets...)
}

// Queues a control frame, a stop latching first, refused once closed or when the queue is full
func (w *managedWorker) control(frame *pyjson.Object) error {
	if w.closed.IsSet() {
		return runtimeRefusal("managed_worker_lost")
	}
	if isTypeFrame(frame, "stop") {
		w.mu.Lock()
		if w.stoppedAt == nil {
			now := pythread.Monotonic()
			w.stoppedAt = &now
		}
		w.mu.Unlock()
		w.stopOnce.Do(func() { close(w.stopCh) })
	}
	select {
	case w.commands <- frame:
		return nil
	default:
		return runtimeRefusal("worker_control_backpressure")
	}
}

// Seconds left of the acknowledgement window that opened at the first stop
func (w *managedWorker) stopBudget(ack float64) float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return *w.stoppedAt + ack - pythread.Monotonic()
}

// One frame, a latched stop bounding supervision to ack seconds in total from the first stop
func (w *managedWorker) nextFrame(ctx context.Context, timeout *float64, ack float64) (*pyjson.Object, error) {
	w.mu.Lock()
	if !w.readerStarted {
		w.readerStarted = true
		w.readerDone = make(chan struct{})
		go w.readFrames()
	}
	if len(w.frames) == 0 && !w.requested {
		w.requested = true
		w.demand <- struct{}{}
	}
	w.mu.Unlock()
	if !w.hasFrames() && !w.stopped() {
		var expiry <-chan time.Time
		if timeout != nil {
			timer := time.NewTimer(time.Duration(*timeout * float64(time.Second)))
			defer timer.Stop()
			expiry = timer.C
		}
		select {
		case <-w.arrival():
		case <-w.stopCh:
		case <-expiry:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if !w.hasFrames() && !w.stopped() {
			return nil, runtimeRefusal("managed_worker_hello_timeout")
		}
	}
	if !w.hasFrames() {
		if remaining := w.stopBudget(ack); remaining > 0 {
			timer := time.NewTimer(time.Duration(remaining * float64(time.Second)))
			defer timer.Stop()
			select {
			case <-w.arrival():
			case <-timer.C:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	if !w.hasFrames() {
		return nil, runtimeRefusal("managed_worker_stopped")
	}
	return w.take()
}

// Cooperative stop of the worker's turn, the legacy interrupt a hard stop falls back to
func (w *managedWorker) Interrupt() error {
	return w.control(pyjson.ObjectOf("type", "stop"))
}

// Forwards a viewer's answer to the worker's approval or clarify prompt
func (w *managedWorker) respond(kind, promptID, value string) error {
	return w.control(pyjson.ObjectOf("type", kind, "prompt_id", promptID, "value", value))
}

// Writes one frame to the child's control pipe
func (w *managedWorker) send(frame any) error {
	data, err := EncodeFrame(frame)
	if err != nil {
		return err
	}
	w.writeLock.Lock()
	defer w.writeLock.Unlock()
	if _, err := w.process.stdin.Write(data); err != nil {
		return &PyError{Kind: "OSError", Msg: err.Error()}
	}
	return nil
}

// Signals the verified interpreter when it is not the handle itself, only while it keeps its recorded birth
func (w *managedWorker) signalWorker(kill bool) {
	w.mu.Lock()
	identity := w.identity
	w.mu.Unlock()
	if identity == nil || identity.pid == w.process.Pid {
		return
	}
	birth, err := proc.WorkerBirth(identity.pid)
	if err != nil || birth != identity.birth {
		return
	}
	_ = signalVerifiedWorker(identity.pid, kill)
}

// Waits on a done channel for at most seconds
func joinFor(done chan struct{}, seconds float64) {
	timer := time.NewTimer(time.Duration(seconds * float64(time.Second)))
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

// Ends the child, escalating to a kill after five seconds, then joins the writer, reader and stderr drain
func (w *managedWorker) close() error {
	w.closed.Set()
	select {
	case w.demand <- struct{}{}:
	default:
	}
	if !w.process.poll() {
		w.signalWorker(false)
		if err := w.signalGroup(false); err != nil {
			return err
		}
	}
	if !w.process.wait(5) {
		w.signalWorker(true)
		if err := w.signalGroup(true); err != nil {
			return err
		}
		w.process.wait(5)
	} else {
		w.signalWorker(true)
	}
	w.mu.Lock()
	writer, reader := w.writerStarted, w.readerStarted
	w.mu.Unlock()
	if writer {
		joinFor(w.writerDone, 5)
	}
	w.process.stdin.Close()
	if !reader {
		w.process.stdout.Close()
	} else {
		joinFor(w.readerDone, 5)
	}
	if w.stderrDone != nil {
		joinFor(w.stderrDone, 5)
	} else if w.process.stderr != nil {
		w.process.stderr.Close()
	}
	return nil
}

// Whether an error is the cancellation of the owner's turn
func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled)
}

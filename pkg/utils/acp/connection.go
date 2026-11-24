package acputil

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/logging"
)

// Function a connection calls for each incoming request and notification, params the raw JSON of the frame's params, nil when it has none; every call runs on its own goroutine, concurrently with the others and in no set order, under a context Close cancels; a request is answered with the result, or with the *RequestError in the error's chain, else an internal error carrying the error's text
type Handler func(ctx context.Context, method string, params json.RawMessage, notification bool) (any, error)

// JSON-RPC 2.0 connection exchanging one JSON object per line; safe for concurrent use
type Connection struct {
	handler Handler
	w       io.Writer
	r       io.Reader
	limit   int
	log     *logging.Logger

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan reply
	serving bool
	// Set once no reply can arrive any more: the input ended or the connection closed
	ended  bool
	closed bool
	// Handler calls running now, and the channel Shutdown waits on, closed when the last of them returns
	running map[*task]struct{}
	drained chan struct{}

	// Parent of every handler context, cancelled by Close or by the end of Serve's context
	ctx    context.Context
	cancel context.CancelFunc
	quit   chan struct{}

	obsMu     sync.Mutex
	observers []StreamObserver
}

type reply struct {
	result json.RawMessage
	err    error
}

// One running handler call: its method, the request id, nil for a notification, and when it started
type task struct {
	method  string
	id      json.RawMessage
	started time.Time
}

// The call as a shutdown report names it, such as "session/prompt (request 3, running 12.5s)"
func (t *task) describe() string {
	kind := "notification"
	if t.id != nil {
		kind = "request " + string(t.id)
	}
	return fmt.Sprintf("%s (%s, running %s)", t.method, kind, time.Since(t.started).Round(time.Millisecond))
}

// Frame the reader hands on: its text, or only its first bytes when it ran past the line limit
type inbound struct {
	frame []byte
	over  bool
}

// Connection calling handler for the frames it reads from r and writing its own frames to w; frames are read once Serve runs
func NewConnection(handler Handler, w io.Writer, r io.Reader, opts ...Option) *Connection {
	return newConnection(handler, w, r, newConfig(opts))
}

func newConnection(handler Handler, w io.Writer, r io.Reader, cfg config) *Connection {
	ctx, cancel := context.WithCancel(context.Background())
	return &Connection{
		handler: handler,
		w:       w,
		r:       r,
		limit:   cfg.limit,
		log:     pkgLog(),
		pending: map[int64]chan reply{},
		running: map[*task]struct{}{},
		ctx:     ctx,
		cancel:  cancel,
		quit:    make(chan struct{}),
	}
}

// Reads and dispatches frames until the input ends, reading fails, ctx is done or the connection closes, then fails the requests still waiting for a reply; nil at the end of the input. Handlers still running go on: Close cancels them, Shutdown also waits for them. A reader blocked in Read keeps the goroutine reading it until that Read returns
func (c *Connection) Serve(ctx context.Context) error {
	c.mu.Lock()
	switch {
	case c.closed || c.ended:
		c.mu.Unlock()
		return ErrConnectionClosed
	case c.serving:
		c.mu.Unlock()
		return errors.New("acp connection is already being served")
	}
	c.serving = true
	c.mu.Unlock()
	stop := context.AfterFunc(ctx, c.cancel)
	defer stop()
	frames := make(chan inbound)
	failed := make(chan error, 1)
	done := make(chan struct{})
	defer close(done)
	go c.read(frames, failed, done)
	err := c.receive(ctx, frames, failed)
	c.end(err)
	return err
}

// Reads frames on its own goroutine, sending each to frames and the reason reading stopped to failed, nil at the end of the input
func (c *Connection) read(frames chan<- inbound, failed chan<- error, done <-chan struct{}) {
	br := bufio.NewReaderSize(c.r, readBuffer)
	for {
		frame, over, err := readFrame(br, c.limit)
		if err != nil && !errors.Is(err, io.EOF) {
			failed <- err
			return
		}
		if len(frame) > 0 {
			select {
			case frames <- inbound{frame: frame, over: over}:
			case <-done:
				return
			}
		}
		if err != nil {
			failed <- nil
			return
		}
	}
}

func (c *Connection) receive(ctx context.Context, frames <-chan inbound, failed <-chan error) error {
	for {
		select {
		case in := <-frames:
			if in.over {
				c.dropOversized(in.frame)
			} else {
				c.dispatch(in.frame)
			}
		case err := <-failed:
			if err != nil {
				c.log.Error("acp connection input failed\n" + err.Error())
			}
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-c.quit:
			return nil
		}
	}
}

// Shows one incoming frame to the observers and hands it on: requests and notifications to handler goroutines, replies to the request waiting for them
func (c *Connection) dispatch(frame []byte) {
	frame = bytes.TrimSpace(frame)
	if len(frame) == 0 {
		return
	}
	if !json.Valid(frame) {
		var probe any
		err := json.Unmarshal(frame, &probe)
		c.log.Error(fmt.Sprintf("acp frame is not valid JSON\n%v", err))
		return
	}
	if frame[0] != '{' {
		c.log.Error("acp frame is " + describe(frame) + ", not an object")
		return
	}
	f := parseFrame(frame)
	method, isCall := f.call()
	c.notify(StreamIncoming, method, frame)
	switch {
	case isCall && f.id != nil:
		c.spawn(method, f.id, func() { c.serveRequest(f.id, method, f.params) })
	case isCall:
		c.spawn(method, nil, func() { c.serveNotification(method, f.params) })
	case f.id != nil:
		c.deliver(f)
	}
}

// Answers what it can of a frame dropped for running past the line limit, judged by its first bytes: a request gets an invalid request error, a reply fails the request waiting for it, and the drop is logged
func (c *Connection) dropOversized(head []byte) {
	var f inFrame
	last := ""
	for key, value := range members(bytes.TrimSpace(head)) {
		switch last = string(key); last {
		case "id":
			f.id = value
		case "method":
			f.method = value
		case "result":
			f.result = value
		case "error":
			f.err = value
		}
	}
	// The cut may fall inside the last member read, so its value is not relied on
	switch last {
	case "id":
		f.id = nil
	case "method":
		f.method = nil
	}
	tooLong := fmt.Errorf("%w of %d bytes", ErrFrameTooLong, c.limit)
	method, isCall := f.call()
	what := "frame"
	switch {
	case isCall && f.id != nil:
		what = "request " + string(f.id) + " of " + method
		c.spawn(method, f.id, func() { c.writeError(f.id, invalidRequest(details(tooLong))) })
	case isCall:
		what = "notification of " + method
	case f.id != nil && (f.result != nil || f.err != nil):
		what = "reply to request " + string(f.id)
		if ch := c.take(f.id); ch != nil {
			ch <- reply{err: tooLong}
		}
	}
	c.log.Error("acp " + what + " dropped: " + tooLong.Error())
}

// Runs fn on a handler goroutine Shutdown waits for, nothing once the connection is closed
func (c *Connection) spawn(method string, id json.RawMessage, fn func()) {
	t := &task{method: method, id: id, started: time.Now()}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.running[t] = struct{}{}
	c.mu.Unlock()
	go func() {
		defer c.finish(t)
		fn()
	}()
}

// Forgets a handler call that returned, waking Shutdown when it was the last
func (c *Connection) finish(t *task) {
	c.mu.Lock()
	delete(c.running, t)
	if len(c.running) == 0 && c.drained != nil {
		close(c.drained)
		c.drained = nil
	}
	c.mu.Unlock()
}

// Calls the handler, a panic turned into the error the request is answered with
func (c *Connection) invoke(ctx context.Context, method string, params json.RawMessage, notification bool) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			c.log.Debug(fmt.Sprintf("acp handler of %s panicked\n%s", method, debug.Stack()))
			result, err = nil, &handlerPanic{value: r}
		}
	}()
	return c.handler(ctx, method, params, notification)
}

// Answers one request with its handler's result or error, then runs what the handler scheduled for after the reply
func (c *Connection) serveRequest(id json.RawMessage, method string, params json.RawMessage) {
	ctx, flush := WithResponseScope(c.ctx)
	defer flush()
	result, err := c.invoke(ctx, method, params, false)
	if err == nil {
		line, encErr := encodeHandlerFrame(resultFrame{JSONRPC: "2.0", ID: id, Result: result})
		if encErr == nil {
			_ = c.writeLine(line, "")
			return
		}
		err = fmt.Errorf("result of %s has no JSON form: %w", method, encErr)
	}
	c.writeError(id, err)
	c.log.Exception("Background task failed", err)
}

func (c *Connection) serveNotification(method string, params json.RawMessage) {
	if _, err := c.invoke(c.ctx, method, params, true); err != nil {
		c.log.Debug("acp notification " + method + " failed\n" + err.Error())
	}
}

// Frame holding values a handler produced, a panic of their own encoding methods returned as the error
func encodeHandlerFrame(frame any) (line []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			line, err = nil, &handlerPanic{value: r}
		}
	}()
	return encodeFrame(frame)
}

// Writes the error frame answering the request with id, its data dropped when it has no JSON form
func (c *Connection) writeError(id json.RawMessage, err error) {
	re := errorFor(err)
	line, encErr := encodeHandlerFrame(errorFrame{JSONRPC: "2.0", ID: id, Error: wireError{Code: re.Code, Message: re.Message, Data: re.Data}})
	if encErr != nil {
		line, encErr = encodeFrame(errorFrame{JSONRPC: "2.0", ID: id, Error: wireError{Code: re.Code, Message: re.Message}})
		if encErr != nil {
			c.log.Error("acp error reply has no JSON form\n" + encErr.Error())
			return
		}
	}
	_ = c.writeLine(line, "")
}

// Channel of the request waiting for the reply with the raw id, taken out of the pending table; nil when no request waits for it
func (c *Connection) take(id []byte) chan reply {
	key, ok := requestKey(id)
	if !ok {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := c.pending[key]
	delete(c.pending, key)
	return ch
}

// Hands a reply to the request waiting for it: its result when it has one, else its error, else an empty result
func (c *Connection) deliver(f inFrame) {
	ch := c.take(f.id)
	switch {
	case ch == nil:
	case f.result != nil:
		ch <- reply{result: f.result}
	case f.err != nil:
		ch <- reply{err: decodeWireError(f.err)}
	default:
		ch <- reply{}
	}
}

// Writes one encoded frame and its line end, flushing a buffered writer, then shows the frame to the observers
func (c *Connection) writeLine(line []byte, method string) error {
	c.writeMu.Lock()
	_, err := c.w.Write(line)
	if f, ok := c.w.(interface{ Flush() error }); ok && err == nil {
		err = f.Flush()
	}
	c.writeMu.Unlock()
	if err != nil {
		c.log.Error("acp connection output failed\n" + err.Error())
		return err
	}
	c.notify(StreamOutgoing, method, line[:len(line)-1])
	return nil
}

// Sends a request and waits for its result, decoded with objects as *jsonutil.Object; a *RequestError when the peer answers with an error
func (c *Connection) SendRequest(ctx context.Context, method string, params any) (any, error) {
	raw, err := c.request(ctx, method, params)
	if err != nil {
		return nil, err
	}
	return dynamic(raw)
}

// Sends a request and waits for the raw JSON of its result, nil when the reply carried none
func (c *Connection) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.ended || c.closed {
		c.mu.Unlock()
		return nil, ErrConnectionClosed
	}
	id := c.nextID
	c.nextID++
	ch := make(chan reply, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	line, err := encodeFrame(requestFrame{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err == nil {
		err = c.writeLine(line, method)
	}
	if err != nil {
		c.forget(id)
		return nil, err
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-ctx.Done():
		c.forget(id)
		return nil, ctx.Err()
	}
}

func (c *Connection) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// Sends a notification
func (c *Connection) SendNotification(ctx context.Context, method string, params any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrConnectionClosed
	}
	line, err := encodeFrame(notificationFrame{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	return c.writeLine(line, method)
}

// Marks that no reply can arrive any more and fails the requests waiting for one with ErrConnectionClosed, wrapped around the reason the input failed
func (c *Connection) end(cause error) {
	reason := ErrConnectionClosed
	if cause != nil && !errors.Is(cause, context.Canceled) && !errors.Is(cause, context.DeadlineExceeded) {
		reason = fmt.Errorf("%w: %w", ErrConnectionClosed, cause)
	}
	c.mu.Lock()
	c.ended, c.serving = true, false
	pending := c.pending
	c.pending = map[int64]chan reply{}
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- reply{err: reason}
	}
}

// Stops the connection at once: no further frame is read or handled, every handler's context is cancelled and every request waiting for a reply fails with ErrConnectionClosed; it does not wait for running handlers, so any goroutine may call it, a handler included, and Shutdown is the call that waits
func (c *Connection) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	c.cancel()
	close(c.quit)
	c.end(nil)
	return nil
}

// Closes the connection, then waits until every running handler has returned or ctx is done; handlers still running then are logged by method and run time, named in the error, which wraps ctx's, and left to finish on their own. A handler calls Close instead: Shutdown would wait for that handler itself
func (c *Connection) Shutdown(ctx context.Context) error {
	c.Close()
	c.mu.Lock()
	if len(c.running) == 0 {
		c.mu.Unlock()
		return nil
	}
	if c.drained == nil {
		c.drained = make(chan struct{})
	}
	drained := c.drained
	c.mu.Unlock()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
	}
	c.mu.Lock()
	left := slices.Collect(maps.Keys(c.running))
	c.mu.Unlock()
	if len(left) == 0 {
		return nil
	}
	slices.SortFunc(left, func(a, b *task) int { return a.started.Compare(b.started) })
	names := make([]string, len(left))
	for i, t := range left {
		names[i] = t.describe()
	}
	err := fmt.Errorf("acp connection shut down with handlers still running, left to finish on their own: %s: %w", strings.Join(names, ", "), ctx.Err())
	c.log.Error(err.Error())
	return err
}

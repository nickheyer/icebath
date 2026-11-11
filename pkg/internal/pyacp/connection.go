package pyacp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/logging"
)

// Handler the connection calls for every incoming request or notification
type MethodHandler func(ctx context.Context, method string, params any, isNotification bool) (any, error)

// ConnectionError rejecting outgoing requests when the connection closes
var ErrConnectionClosed = errors.New("Connection closed")

type outcome struct {
	result any
	err    error
}

// Minimal JSON-RPC 2.0 connection over newline delimited JSON frames
type Connection struct {
	handler  MethodHandler
	writer   io.Writer
	reader   *bufio.Reader
	limit    int
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	nextID   int64
	outgoing map[int64]chan outcome
	sendMu   sync.Mutex
	closed   bool
	tasks    sync.WaitGroup
	log      *logging.Logger
	obsMu    sync.Mutex
	observer []StreamObserver
}

// Direction of a raw JSON-RPC message seen by a stream observer
type StreamDirection string

const (
	StreamIncoming StreamDirection = "incoming"
	StreamOutgoing StreamDirection = "outgoing"
)

// One raw JSON-RPC message with its direction, a deep copy of the frame
type StreamEvent struct {
	Direction StreamDirection
	Message   any
}

// Callback receiving every raw JSON-RPC message of a connection
type StreamObserver func(event StreamEvent)

// Registers a callback that receives every raw JSON-RPC message
func (c *Connection) AddObserver(observer StreamObserver) {
	c.obsMu.Lock()
	defer c.obsMu.Unlock()
	c.observer = append(c.observer, observer)
}

func (c *Connection) notifyObservers(direction StreamDirection, message any) {
	c.obsMu.Lock()
	observers := append([]StreamObserver(nil), c.observer...)
	c.obsMu.Unlock()
	if len(observers) == 0 {
		return
	}
	event := StreamEvent{Direction: direction, Message: pycopy.DeepCopy(message)}
	for _, observer := range observers {
		func() {
			defer func() {
				if r := recover(); r != nil {
					c.log.Error(fmt.Sprintf("Stream observer failed\n%v", r))
				}
			}()
			observer(event)
		}()
	}
}

type afterResponseKey struct{}

type afterResponse struct {
	mu   sync.Mutex
	sent bool
	fns  []func()
}

// Runs fn on its own goroutine once the response of the request is written, at once outside one
func AfterResponse(ctx context.Context, fn func()) {
	if ar, ok := ctx.Value(afterResponseKey{}).(*afterResponse); ok {
		ar.mu.Lock()
		if !ar.sent {
			ar.fns = append(ar.fns, fn)
			ar.mu.Unlock()
			return
		}
		ar.mu.Unlock()
	}
	go fn()
}

// Request scope of ctx whose after response callbacks run when the returned flush is called
func WithResponseScope(ctx context.Context) (context.Context, func()) {
	scope := &afterResponse{}
	return context.WithValue(ctx, afterResponseKey{}, scope), scope.flush
}

func (ar *afterResponse) flush() {
	ar.mu.Lock()
	fns := ar.fns
	ar.sent, ar.fns = true, nil
	ar.mu.Unlock()
	for _, fn := range fns {
		go fn()
	}
}

// Error of a handler that panicked, its text the panic value
type handlerPanic struct{ value any }

func (e *handlerPanic) Error() string {
	if err, ok := e.value.(error); ok {
		return err.Error()
	}
	return fmt.Sprint(e.value)
}

// Calls the handler, a panic becoming the error the Python handler would have raised
func (c *Connection) callHandler(ctx context.Context, method string, params any, isNotification bool) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			result, err = nil, &handlerPanic{value: r}
		}
	}()
	return c.handler(ctx, method, params, isNotification)
}

// Connection reading frames from reader and writing to writer, receiving in the background when listening
func NewConnection(handler MethodHandler, writer io.Writer, reader io.Reader, listening bool, limit int) *Connection {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Connection{
		handler:  handler,
		writer:   writer,
		reader:   bufio.NewReaderSize(reader, 64*1024),
		limit:    limit,
		ctx:      ctx,
		cancel:   cancel,
		outgoing: map[int64]chan outcome{},
		log:      logging.GetLogger("root"),
	}
	if listening {
		c.tasks.Add(1)
		go func() {
			defer c.tasks.Done()
			if err := c.receiveLoop(); err != nil {
				c.log.Error("Receive loop failed\n" + err.Error())
				c.rejectAll(err)
			}
		}()
	}
	return c
}

// Stops receiving, waits for in flight handlers and rejects pending outgoing requests
func (c *Connection) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	c.cancel()
	c.tasks.Wait()
	c.rejectAll(ErrConnectionClosed)
	return nil
}

// Receives frames until end of input, logging and returning a failure of the loop
func (c *Connection) MainLoop() error {
	if err := c.receiveLoop(); err != nil {
		c.log.Error("Connection main loop failed\n" + err.Error())
		c.rejectAll(err)
		return err
	}
	return nil
}

// Sends a request and waits for its result or error response
func (c *Connection) SendRequest(ctx context.Context, method string, params any) (any, error) {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	ch := make(chan outcome, 1)
	c.outgoing[id] = ch
	c.mu.Unlock()
	payload := pyjson.NewObject()
	payload.Set("jsonrpc", "2.0")
	payload.Set("id", id)
	payload.Set("method", method)
	payload.Set("params", params)
	if err := c.send(payload); err != nil {
		c.mu.Lock()
		delete(c.outgoing, id)
		c.mu.Unlock()
		return nil, err
	}
	c.notifyObservers(StreamOutgoing, payload)
	select {
	case out := <-ch:
		return out.result, out.err
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.outgoing, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Sends a notification frame
func (c *Connection) SendNotification(ctx context.Context, method string, params any) error {
	payload := pyjson.NewObject()
	payload.Set("jsonrpc", "2.0")
	payload.Set("method", method)
	payload.Set("params", params)
	if err := c.send(payload); err != nil {
		return err
	}
	c.notifyObservers(StreamOutgoing, payload)
	return nil
}

// Writes one compact ASCII JSON frame followed by a newline
func (c *Connection) send(payload *pyjson.Object) error {
	text, err := pyjson.Dumps(payload, pyjson.Separators(",", ":"))
	if err != nil {
		return err
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if _, err := io.WriteString(c.writer, text+"\n"); err != nil {
		c.log.Error("Send loop failed\n" + err.Error())
		return err
	}
	if f, ok := c.writer.(interface{ Flush() error }); ok {
		return f.Flush()
	}
	if f, ok := c.writer.(interface{ Sync() error }); ok {
		_ = f.Sync()
	}
	return nil
}

// One line of at most limit bytes, the asyncio readline errors past it
func (c *Connection) readLine() ([]byte, error) {
	var line []byte
	for {
		chunk, err := c.reader.ReadSlice('\n')
		line = append(line, chunk...)
		if c.limit > 0 && len(line) > c.limit {
			if err == nil {
				return nil, errors.New("Separator is found, but chunk is longer than limit")
			}
			return nil, errors.New("Separator is not found, and chunk exceed the limit")
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF {
			return line, nil
		}
		return line, err
	}
}

func (c *Connection) receiveLoop() error {
	for {
		if c.ctx.Err() != nil {
			return nil
		}
		line, err := c.readLine()
		if err != nil {
			return err
		}
		if len(line) == 0 {
			return nil
		}
		message, err := pyjson.LoadsBytes(line)
		if err != nil {
			c.log.Error("Error parsing JSON-RPC message\n" + err.Error())
			continue
		}
		c.notifyObservers(StreamIncoming, message)
		if err := c.processMessage(message); err != nil {
			return err
		}
	}
}

func (c *Connection) processMessage(raw any) error {
	message, ok := raw.(*pyjson.Object)
	if !ok {
		return fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(raw))
	}
	method, _ := message.Get("method")
	_, hasID := message.Get("id")
	switch {
	case method != nil && hasID:
		c.spawn(func() { c.runRequest(message) })
	case method != nil:
		c.spawn(func() { c.runNotification(message) })
	case hasID:
		return c.handleResponse(message)
	}
	return nil
}

func (c *Connection) spawn(fn func()) {
	c.mu.Lock()
	closed := c.closed
	if !closed {
		c.tasks.Add(1)
	}
	c.mu.Unlock()
	if closed {
		return
	}
	go func() {
		defer c.tasks.Done()
		fn()
	}()
}

// Error object for a failed handler, invalid params for validation and internal error otherwise
func errorFor(err error) *RequestError {
	var re *RequestError
	if errors.As(err, &re) {
		return re
	}
	var ve *ValidationError
	if errors.As(err, &ve) {
		d := pyjson.NewObject()
		d.Set("errors", ve.Errors())
		return InvalidParams(d)
	}
	data, perr := pyjson.Loads(err.Error())
	if perr != nil {
		d := pyjson.NewObject()
		d.Set("details", err.Error())
		data = d
	}
	return InternalError(data)
}

func (c *Connection) runRequest(message *pyjson.Object) {
	id, _ := message.Get("id")
	method, _ := message.Get("method")
	params, _ := message.Get("params")
	payload := pyjson.NewObject()
	payload.Set("jsonrpc", "2.0")
	payload.Set("id", id)
	name, _ := method.(string)
	scoped, flush := WithResponseScope(c.ctx)
	defer flush()
	result, err := c.callHandler(scoped, name, params, false)
	if err != nil {
		payload.Set("error", errorFor(err).ToErrorObj())
		if c.send(payload) == nil {
			c.notifyObservers(StreamOutgoing, payload)
		}
		c.log.Exception("Background task failed", err)
		return
	}
	if IsNil(result) {
		result = nil
	} else if IsModel(result) {
		result = Dump(result, ResultDump)
	}
	payload.Set("result", result)
	if c.send(payload) == nil {
		c.notifyObservers(StreamOutgoing, payload)
	}
}

func (c *Connection) runNotification(message *pyjson.Object) {
	method, _ := message.Get("method")
	params, _ := message.Get("params")
	name, _ := method.(string)
	_, _ = c.callHandler(c.ctx, name, params, true)
}

func requestKey(id any) (int64, bool) {
	switch x := id.(type) {
	case int64:
		return x, true
	case float64:
		if x == math.Trunc(x) {
			return int64(x), true
		}
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func (c *Connection) take(id any) chan outcome {
	key, ok := requestKey(id)
	if !ok {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := c.outgoing[key]
	delete(c.outgoing, key)
	return ch
}

func (c *Connection) handleResponse(message *pyjson.Object) error {
	id, _ := message.Get("id")
	if result, ok := message.Get("result"); ok {
		if ch := c.take(id); ch != nil {
			ch <- outcome{result: result}
		}
		return nil
	}
	if errVal, ok := message.Get("error"); ok {
		errObj, isObj := errVal.(*pyjson.Object)
		if !isObj {
			if errVal != nil && errVal != false && errVal != "" {
				return fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(errVal))
			}
			errObj = pyjson.NewObject()
		}
		code := -32603
		if v, ok := errObj.Get("code"); ok {
			if n, ok := requestKey(v); ok {
				code = int(n)
			}
		}
		msg := "Error"
		if v, ok := errObj.Get("message"); ok {
			msg = fmt.Sprint(v)
		}
		data, _ := errObj.Get("data")
		if ch := c.take(id); ch != nil {
			ch <- outcome{err: NewRequestError(code, msg, data)}
		}
		return nil
	}
	if ch := c.take(id); ch != nil {
		ch <- outcome{}
	}
	return nil
}

func (c *Connection) rejectAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, ch := range c.outgoing {
		ch <- outcome{err: err}
		delete(c.outgoing, id)
	}
}

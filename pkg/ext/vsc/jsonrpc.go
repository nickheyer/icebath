package vsc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// JSON-RPC error codes this peer returns
const (
	codeNoMethod  = -32601
	codeBadParams = -32602
	codeInternal  = -32603
)

// Error a handler returns to choose the code, message and data sent to the caller
type RPCError struct {
	Code    int
	Message string
	Data    any
}

func (e *RPCError) Error() string { return fmt.Sprintf("jsonrpc %d: %s", e.Code, e.Message) }

// Handles an incoming request and returns the result or an error
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// Handles an incoming notification
type NotifyHandler func(params json.RawMessage)

type wire struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *wireError      `json:"error,omitempty"`
}

type wireError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type pending struct {
	result any
	done   chan *wireError
}

// Speaks newline delimited JSON-RPC 2.0 over one connection as both client and server
type Peer struct {
	conn     io.ReadWriteCloser
	writeMu  sync.Mutex
	mu       sync.Mutex
	handlers map[string]Handler
	notifies map[string]NotifyHandler
	calls    map[int64]*pending
	nextID   int64
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	closeErr error
	closed   bool
	trace    func(dir string, line []byte)
	start    sync.Once
}

// Routes every line read or written to fn, dir being in or out, nil to stop
func (p *Peer) SetTrace(fn func(dir string, line []byte)) {
	p.mu.Lock()
	p.trace = fn
	p.mu.Unlock()
}

func (p *Peer) tracer() func(dir string, line []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.trace
}

// Builds a peer that serves requests and sends its own over conn until it is closed
func NewPeer(conn io.ReadWriteCloser) *Peer {
	p := newPeer(conn)
	p.Start()
	return p
}

// Builds a peer that reads nothing until Start, so handlers can be registered ahead of the first line
func newPeer(conn io.ReadWriteCloser) *Peer {
	ctx, cancel := context.WithCancel(context.Background())
	return &Peer{
		conn:     conn,
		handlers: map[string]Handler{},
		notifies: map[string]NotifyHandler{},
		calls:    map[int64]*pending{},
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
}

// Begins reading the connection, once
func (p *Peer) Start() {
	p.start.Do(func() { go p.read() })
}

// Registers the handler for incoming requests of the method
func (p *Peer) Handle(method string, fn Handler) {
	p.mu.Lock()
	p.handlers[method] = fn
	p.mu.Unlock()
}

// Registers the handler for incoming notifications of the method
func (p *Peer) HandleNotify(method string, fn NotifyHandler) {
	p.mu.Lock()
	p.notifies[method] = fn
	p.mu.Unlock()
}

func (p *Peer) read() {
	br := bufio.NewReaderSize(p.conn, 1<<20)
	var err error
	for {
		var line []byte
		line, err = readLine(br)
		if len(line) > 0 {
			if tr := p.tracer(); tr != nil {
				tr("in", line)
			}
			p.dispatch(line)
		}
		if err != nil {
			break
		}
	}
	p.finish(err)
}

// Reads one newline delimited message, growing past the reader buffer for long lines
func readLine(br *bufio.Reader) ([]byte, error) {
	var full []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			full = append(full, chunk...)
			continue
		}
		full = append(full, chunk...)
		if err != nil {
			return trimNewline(full), err
		}
		return trimNewline(full), nil
	}
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

func (p *Peer) dispatch(line []byte) {
	var msg wire
	if err := json.Unmarshal(line, &msg); err != nil {
		return
	}
	switch {
	case msg.Method != "" && len(msg.ID) > 0:
		go p.serve(msg)
	case msg.Method != "":
		p.deliverNotify(msg)
	case len(msg.ID) > 0:
		p.answer(msg)
	}
}

func (p *Peer) serve(msg wire) {
	p.mu.Lock()
	fn := p.handlers[msg.Method]
	p.mu.Unlock()
	if fn == nil {
		p.replyError(msg.ID, &wireError{Code: codeNoMethod, Message: "method not found " + msg.Method})
		return
	}
	result, err := fn(p.ctx, msg.Params)
	if err != nil {
		p.replyError(msg.ID, toWireError(err))
		return
	}
	raw, mErr := json.Marshal(result)
	if mErr != nil {
		p.replyError(msg.ID, &wireError{Code: codeInternal, Message: mErr.Error()})
		return
	}
	p.send(wire{Version: "2.0", ID: msg.ID, Result: raw})
}

func toWireError(err error) *wireError {
	var re *RPCError
	if errors.As(err, &re) {
		we := &wireError{Code: re.Code, Message: re.Message}
		if re.Data != nil {
			if raw, mErr := json.Marshal(re.Data); mErr == nil {
				we.Data = raw
			}
		}
		return we
	}
	return &wireError{Code: codeInternal, Message: err.Error()}
}

func (p *Peer) deliverNotify(msg wire) {
	p.mu.Lock()
	fn := p.notifies[msg.Method]
	p.mu.Unlock()
	if fn != nil {
		fn(msg.Params)
	}
}

func (p *Peer) answer(msg wire) {
	id, err := idValue(msg.ID)
	if err != nil {
		return
	}
	p.mu.Lock()
	call := p.calls[id]
	delete(p.calls, id)
	p.mu.Unlock()
	if call == nil {
		return
	}
	if msg.Error != nil {
		call.done <- msg.Error
		return
	}
	if call.result != nil && len(msg.Result) > 0 {
		if err := json.Unmarshal(msg.Result, call.result); err != nil {
			call.done <- &wireError{Code: codeInternal, Message: err.Error()}
			return
		}
	}
	call.done <- nil
}

// Sends a request and waits for its response, returning early when the context ends
func (p *Peer) Call(ctx context.Context, method string, params any, result any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return p.closeErr
	}
	p.nextID++
	id := p.nextID
	call := &pending{result: result, done: make(chan *wireError, 1)}
	p.calls[id] = call
	p.mu.Unlock()
	idRaw, _ := json.Marshal(id)
	if err := p.send(wire{Version: "2.0", ID: idRaw, Method: method, Params: raw}); err != nil {
		p.mu.Lock()
		delete(p.calls, id)
		p.mu.Unlock()
		return err
	}
	select {
	case we := <-call.done:
		if we != nil {
			return &RPCError{Code: we.Code, Message: we.Message, Data: we.Data}
		}
		return nil
	case <-ctx.Done():
		p.mu.Lock()
		delete(p.calls, id)
		p.mu.Unlock()
		return ctx.Err()
	case <-p.done:
		return p.closeErr
	}
}

// Sends a notification that expects no response
func (p *Peer) Notify(method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	return p.send(wire{Version: "2.0", Method: method, Params: raw})
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func (p *Peer) replyError(id json.RawMessage, we *wireError) {
	p.send(wire{Version: "2.0", ID: id, Error: we})
}

func (p *Peer) send(msg wire) error {
	msg.Version = "2.0"
	line, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	p.mu.Lock()
	closed := p.closed
	cause := p.closeErr
	p.mu.Unlock()
	if closed {
		if cause != nil {
			return cause
		}
		return errClosed
	}
	if tr := p.tracer(); tr != nil {
		tr("out", line)
	}
	if _, err := p.conn.Write(append(line, '\n')); err != nil {
		return err
	}
	return nil
}

var errClosed = errors.New("vsc: peer closed")

func idValue(raw json.RawMessage) (int64, error) {
	var id int64
	if err := json.Unmarshal(raw, &id); err == nil {
		return id, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, err
	}
	var n int64
	_, err := fmt.Sscan(s, &n)
	return n, err
}

func (p *Peer) finish(err error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	if err == nil || errors.Is(err, io.EOF) {
		err = io.EOF
	}
	p.closeErr = err
	waiting := p.calls
	p.calls = map[int64]*pending{}
	p.mu.Unlock()
	p.cancel()
	for _, call := range waiting {
		call.done <- &wireError{Code: codeInternal, Message: "connection closed"}
	}
	close(p.done)
}

// Closes the connection and ends every waiting call
func (p *Peer) Close() error {
	p.mu.Lock()
	already := p.closed
	p.mu.Unlock()
	err := p.conn.Close()
	if !already {
		p.finish(errClosed)
	}
	return err
}

// Channel closed when the connection has ended
func (p *Peer) Done() <-chan struct{} { return p.done }

// Why the connection ended, nil while it is open
func (p *Peer) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closeErr == io.EOF {
		return nil
	}
	return p.closeErr
}

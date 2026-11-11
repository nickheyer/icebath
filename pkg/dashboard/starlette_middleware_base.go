package dashboard

import (
	"context"
	"fmt"
	"sync"
)

// Wraps an ASGI app, the shape of every app.add_middleware entry
type Middleware func(app ASGIApp) ASGIApp

// Next app of an http middleware, returning the downstream response as a stream
type CallNext func(ctx context.Context, req *Request) (ASGIResponse, error)

// Http middleware body given the request and the call to the rest of the stack
type DispatchFunc func(ctx context.Context, req *Request, callNext CallNext) (ASGIResponse, error)

// Request whose body a dispatch function read is replayed to the downstream app
type cachedRequest struct {
	*Request
	rcvDisconnected bool
	rcvConsumed     bool
}

// Receive of the downstream app, the cached body or the rest of the stream then the disconnect
func (r *cachedRequest) wrappedReceive(ctx context.Context) (Message, error) {
	if r.rcvDisconnected {
		return Message{Type: "http.disconnect"}, nil
	}
	if r.rcvConsumed {
		if r.disconnected {
			r.rcvDisconnected = true
			return Message{Type: "http.disconnect"}, nil
		}
		m, err := r.receive(ctx)
		if err != nil {
			return m, err
		}
		if m.Type != "http.disconnect" {
			return m, &RuntimeError{Msg: fmt.Sprintf("Unexpected message received: %s", m.Type)}
		}
		r.rcvDisconnected = true
		return m, nil
	}
	if r.haveBody {
		r.rcvConsumed = true
		return Message{Type: "http.request", Body: r.body}, nil
	}
	if r.streamConsumed {
		r.rcvConsumed = true
		return Message{Type: "http.request", Body: []byte{}}, nil
	}
	chunk, err := r.firstChunk(ctx)
	if err != nil {
		if _, ok := err.(ClientDisconnect); ok {
			r.rcvDisconnected = true
			return Message{Type: "http.disconnect"}, nil
		}
		return Message{}, err
	}
	r.rcvConsumed = r.streamConsumed
	return Message{Type: "http.request", Body: chunk, MoreBody: !r.streamConsumed}, nil
}

// First chunk the body stream yields, the empty final chunk when the body is empty
func (r *Request) firstChunk(ctx context.Context) ([]byte, error) {
	for !r.streamConsumed {
		m, err := r.receive(ctx)
		if err != nil {
			return nil, err
		}
		switch m.Type {
		case "http.request":
			if !m.MoreBody {
				r.streamConsumed = true
			}
			if len(m.Body) > 0 {
				return m.Body, nil
			}
		case "http.disconnect":
			r.disconnected = true
			return nil, ClientDisconnect{}
		}
	}
	return []byte{}, nil
}

// Downstream response replayed from the start event and the body events the app sent
type middlewareStream struct {
	status  int
	headers []Header
	body    <-chan Message
}

func (m *middlewareStream) Status() int     { return m.status }
func (m *middlewareStream) Header() Headers { return headersOfRaw(&m.headers) }

// Sends the captured start then each non-empty body chunk and a closing empty chunk
func (m *middlewareStream) Serve(ctx context.Context, _ *Scope, _ Receive, send Send) error {
	if err := send(ctx, Message{Type: "http.response.start", Status: m.status, Headers: m.headers}); err != nil {
		return err
	}
	closeBody := true
	for msg := range m.body {
		if msg.Type == "http.response.pathsend" {
			closeBody = false
			if err := send(ctx, msg); err != nil {
				return err
			}
			break
		}
		if len(msg.Body) > 0 {
			if err := send(ctx, Message{Type: "http.response.body", Body: msg.Body, MoreBody: true}); err != nil {
				return err
			}
		}
		if !msg.MoreBody {
			break
		}
	}
	if closeBody {
		return send(ctx, Message{Type: "http.response.body", Body: []byte{}})
	}
	return nil
}

// Http middleware that runs dispatch with a call_next streaming the rest of the stack, the BaseHTTPMiddleware
func BaseHTTPMiddleware(dispatch DispatchFunc) Middleware {
	return func(app ASGIApp) ASGIApp {
		return func(ctx context.Context, scope *Scope, receive Receive, send Send) error {
			if scope.Type != "http" {
				return app(ctx, scope, receive, send)
			}
			req := &cachedRequest{Request: NewRequest(scope, receive, send)}
			responseSent := make(chan struct{})
			streamClosed := make(chan struct{})
			msgs := make(chan Message)
			var (
				appErr       error
				appDone      chan struct{}
				alreadyRaise bool
				cancelApp    context.CancelFunc = func() {}
				closeOnce    sync.Once
			)
			closeStream := func() { closeOnce.Do(func() { close(streamClosed) }) }
			callNext := func(ctx context.Context, _ *Request) (ASGIResponse, error) {
				receiveOrDisconnect := func(rctx context.Context) (Message, error) {
					select {
					case <-responseSent:
						return Message{Type: "http.disconnect"}, nil
					default:
					}
					wctx, cancel := context.WithCancel(rctx)
					defer cancel()
					type result struct {
						m   Message
						err error
					}
					got := make(chan result, 1)
					go func() {
						m, err := req.wrappedReceive(wctx)
						got <- result{m, err}
					}()
					select {
					case res := <-got:
						select {
						case <-responseSent:
							return Message{Type: "http.disconnect"}, nil
						default:
						}
						return res.m, res.err
					case <-responseSent:
						cancel()
						<-got
						return Message{Type: "http.disconnect"}, nil
					}
				}
				sendNoError := func(sctx context.Context, m Message) error {
					select {
					case msgs <- m:
					case <-streamClosed:
						return nil
					case <-sctx.Done():
						return sctx.Err()
					}
					if m.Type == "http.response.pathsend" || (m.Type == "http.response.body" && !m.MoreBody) {
						select {
						case <-responseSent:
						case <-sctx.Done():
							return sctx.Err()
						}
					}
					return nil
				}
				actx, cancel := context.WithCancel(ctx)
				cancelApp = cancel
				appDone = make(chan struct{})
				go func() {
					defer close(appDone)
					defer close(msgs)
					if err := app(actx, scope, receiveOrDisconnect, sendNoError); err != nil {
						appErr = err
					}
				}()
				for {
					m, ok := <-msgs
					if !ok {
						if appErr != nil {
							alreadyRaise = true
							return nil, appErr
						}
						return nil, &RuntimeError{Msg: "No response returned."}
					}
					switch m.Type {
					case "http.response.debug":
						continue
					case "http.response.early_hint":
						if err := send(ctx, m); err != nil {
							return nil, err
						}
						continue
					}
					if m.Type != "http.response.start" {
						return nil, &RuntimeError{Msg: fmt.Sprintf("Unexpected message: %s", m.Type)}
					}
					return &middlewareStream{status: m.Status, headers: m.Headers, body: msgs}, nil
				}
			}
			finish := func() {
				closeStream()
				if appDone != nil {
					<-appDone
				}
			}
			resp, err := dispatch(ctx, req.Request, callNext)
			if err != nil {
				cancelApp()
				finish()
				return err
			}
			if err := resp.Serve(ctx, scope, req.wrappedReceive, send); err != nil {
				cancelApp()
				finish()
				return err
			}
			close(responseSent)
			finish()
			if appErr != nil && !alreadyRaise {
				return appErr
			}
			return nil
		}
	}
}

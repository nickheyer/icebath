package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Python http.client reason phrases where the Go status text differs
var pyStatusPhrases = map[int]string{413: "Content Too Large", 414: "URI Too Long", 416: "Range Not Satisfiable",
	418: "I'm a Teapot", 422: "Unprocessable Content"}

// Python http.client.responses phrase of a status, empty for an unknown code
func statusPhrase(status int) string {
	if p, ok := pyStatusPhrases[status]; ok {
		return p
	}
	return http.StatusText(status)
}

// HTTP error a route or dependency raises, rendered as a JSON detail body
type HTTPException struct {
	StatusCode int
	Detail     any
	Headers    []string
}

// HTTP error of a status, the status phrase standing in for a nil detail, headers as name and value pairs
func NewHTTPException(status int, detail any, headers ...string) *HTTPException {
	if detail == nil {
		detail = statusPhrase(status)
	}
	return &HTTPException{StatusCode: status, Detail: detail, Headers: headers}
}

func (e *HTTPException) Error() string {
	return fmt.Sprintf("%d: %s", e.StatusCode, pyrepr.Str(e.Detail))
}
func (e *HTTPException) PyTypeName() string { return "HTTPException" }

// WebSocket error that closes the socket with its code and reason
type WebSocketException struct {
	Code   int
	Reason string
}

func (e *WebSocketException) Error() string      { return fmt.Sprintf("%d: %s", e.Code, e.Reason) }
func (e *WebSocketException) PyTypeName() string { return "WebSocketException" }

// Request validation failure carrying the pydantic error entries, answered with a 422
type RequestValidationError struct {
	Errors []ErrorDetails
	Body   any
}

func (e *RequestValidationError) Error() string {
	return fmt.Sprintf("%d validation errors", len(e.Errors))
}
func (e *RequestValidationError) PyTypeName() string { return "RequestValidationError" }

// WebSocket parameter validation failure, closing the socket with a policy violation
type WebSocketRequestValidationError struct {
	Errors []ErrorDetails
}

func (e *WebSocketRequestValidationError) Error() string {
	return fmt.Sprintf("%d validation errors", len(e.Errors))
}
func (e *WebSocketRequestValidationError) PyTypeName() string {
	return "WebSocketRequestValidationError"
}

// Whether a status code may carry a body
func isBodyAllowedForStatusCode(status int) bool {
	return !(status < 200 || status == 204 || status == 205 || status == 304)
}

// Handler of a raised error for an http request or websocket, nil response when it answered itself
type exceptionHandler func(ctx context.Context, req *Request, ws *WebSocket, err error) (ASGIResponse, error)

// Handler for an error class, the FastAPI defaults for its four exception classes, nil for any other error
func lookupExceptionHandler(err error) exceptionHandler {
	var he *HTTPException
	if errors.As(err, &he) {
		return fastapiHTTPExceptionHandler
	}
	var wse *WebSocketException
	if errors.As(err, &wse) {
		return starletteWebSocketExceptionHandler
	}
	var rve *RequestValidationError
	if errors.As(err, &rve) {
		return fastapiRequestValidationHandler
	}
	var wsve *WebSocketRequestValidationError
	if errors.As(err, &wsve) {
		return fastapiWebSocketValidationHandler
	}
	return nil
}

// JSON detail body of an HTTP error, a bare response for a status that carries no body
func fastapiHTTPExceptionHandler(_ context.Context, _ *Request, _ *WebSocket, err error) (ASGIResponse, error) {
	var he *HTTPException
	errors.As(err, &he)
	if !isBodyAllowedForStatusCode(he.StatusCode) {
		return NewResponse(nil, he.StatusCode, "", he.Headers...), nil
	}
	return JSONResponse(jsonObj("detail", jsonableEncoder(he.Detail)), he.StatusCode, he.Headers...)
}

// Closes the socket with the code and reason of the error
func starletteWebSocketExceptionHandler(ctx context.Context, _ *Request, ws *WebSocket, err error) (ASGIResponse, error) {
	var wse *WebSocketException
	errors.As(err, &wse)
	return nil, ws.Close(ctx, wse.Code, wse.Reason)
}

// 422 body listing every validation error
func fastapiRequestValidationHandler(_ context.Context, _ *Request, _ *WebSocket, err error) (ASGIResponse, error) {
	var rve *RequestValidationError
	errors.As(err, &rve)
	return JSONResponse(jsonObj("detail", errorDetailsJSON(rve.Errors)), 422)
}

// Closes the socket with a policy violation whose reason is the JSON error list
func fastapiWebSocketValidationHandler(ctx context.Context, _ *Request, ws *WebSocket, err error) (ASGIResponse, error) {
	var wsve *WebSocketRequestValidationError
	errors.As(err, &wsve)
	reason, rerr := renderJSON(errorDetailsJSON(wsve.Errors))
	if rerr != nil {
		return nil, rerr
	}
	return nil, ws.Close(ctx, 1008, string(reason))
}

// Runs app turning a raised error into its handler response unless the response already started
func wrapAppHandlingExceptions(app ASGIApp, req *Request, ws *WebSocket) ASGIApp {
	return func(ctx context.Context, scope *Scope, receive Receive, send Send) error {
		installed := scope.handlersInstalled
		started := false
		sender := func(ctx context.Context, m Message) error {
			if m.Type == "http.response.start" {
				started = true
			}
			return send(ctx, m)
		}
		err := app(ctx, scope, receive, sender)
		if err == nil {
			return nil
		}
		if !installed {
			return err
		}
		handler := lookupExceptionHandler(err)
		if handler == nil {
			return err
		}
		if started {
			return &RuntimeError{Msg: "Caught handled exception, but response already started."}
		}
		resp, herr := handler(ctx, req, ws, err)
		if herr != nil {
			return herr
		}
		if resp != nil {
			return resp.Serve(ctx, scope, receive, sender)
		}
		return nil
	}
}

// Installs the error handlers into the scope and wraps the inner app with them
func exceptionMiddleware(app ASGIApp) ASGIApp {
	return func(ctx context.Context, scope *Scope, receive Receive, send Send) error {
		if scope.Type != "http" && scope.Type != "websocket" {
			return app(ctx, scope, receive, send)
		}
		scope.handlersInstalled = true
		if scope.Type == "http" {
			return wrapAppHandlingExceptions(app, NewRequest(scope, receive, send), nil)(ctx, scope, receive, send)
		}
		return wrapAppHandlingExceptions(app, nil, NewWebSocket(scope, receive, send))(ctx, scope, receive, send)
	}
}

// Answers an unhandled http error with a plain 500 when nothing was sent, then raises it to the server
func serverErrorMiddleware(app ASGIApp) ASGIApp {
	return func(ctx context.Context, scope *Scope, receive Receive, send Send) error {
		if scope.Type != "http" {
			return app(ctx, scope, receive, send)
		}
		started := false
		err := app(ctx, scope, receive, func(ctx context.Context, m Message) error {
			if m.Type == "http.response.start" {
				started = true
			}
			return send(ctx, m)
		})
		if err == nil {
			return nil
		}
		if !started {
			if serr := PlainTextResponse("Internal Server Error", 500).Serve(ctx, scope, receive, send); serr != nil {
				return serr
			}
		}
		return err
	}
}

package acputil

import (
	"errors"
	"fmt"
	"strconv"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// JSON-RPC error codes the connection and router answer with
const (
	// Frame the connection cannot take as a request, such as one longer than its line limit
	CodeInvalidRequest = -32600
	// Request for a method the peer does not serve, or an unstable one it was not opened for
	CodeMethodNotFound = -32601
	// Request whose params do not decode into the method's request type
	CodeInvalidParams = -32602
	// Failure inside the handler of a request
	CodeInternalError = -32603
)

// JSON-RPC error: returned by a handler and sent to the peer, or received in answer to a request; match it with errors.As
type RequestError struct {
	Code    int
	Message string
	// Extra detail, nil when there is none; a *jsonutil.Object when it came from the wire as an object
	Data any
}

// The message as the peer or the handler wrote it
func (e *RequestError) Error() string { return e.Message }

// Error with the given code, message and data
func NewRequestError(code int, message string, data any) *RequestError {
	return &RequestError{Code: code, Message: message, Data: data}
}

// Method not found error whose data is an object naming the method
func MethodNotFound(method string) *RequestError {
	return NewRequestError(CodeMethodNotFound, "Method not found", jsonutil.ObjectOf("method", method))
}

// Invalid request error carrying data
func invalidRequest(data any) *RequestError {
	return NewRequestError(CodeInvalidRequest, "Invalid request", data)
}

// Invalid params error carrying data
func InvalidParams(data any) *RequestError {
	return NewRequestError(CodeInvalidParams, "Invalid params", data)
}

// Internal error carrying data
func InternalError(data any) *RequestError {
	return NewRequestError(CodeInternalError, "Internal error", data)
}

// Error data describing a Go error: an object whose details member holds its text
func details(err error) *jsonutil.Object { return jsonutil.ObjectOf("details", err.Error()) }

// Error object sent for a failed request: the *RequestError in the chain, else an internal error with the failure's text as details
func errorFor(err error) *RequestError {
	var re *RequestError
	if errors.As(err, &re) {
		return re
	}
	return InternalError(details(err))
}

// Reported by requests on a connection that is closed or whose input ended, and wrapped around the reason the input ended
var ErrConnectionClosed = errors.New("acp connection closed")

// Reported, wrapped with the limit, by a request whose reply was longer than the connection's line limit, and given as the reason a request that long was refused
var ErrFrameTooLong = errors.New("acp frame longer than the line limit")

// Decoding failure of a message that lacks a member the protocol requires, or holds null in it
type MissingFieldError struct {
	// Type of the message, such as "PromptRequest"
	Type string
	// Wire name of the member, such as "sessionId"
	Field string
}

// Which member is missing from which message, such as `missing required field "prompt" in PromptRequest`
func (e *MissingFieldError) Error() string {
	return "missing required field " + strconv.Quote(e.Field) + " in " + e.Type
}

// Decoding failure of a union value whose discriminator names a variant this package does not know
type UnknownVariantError struct {
	// Union type, such as "ContentBlock"
	Union string
	// Discriminator member, such as "type"
	Key string
	// Discriminator value found
	Value string
}

// The union, its discriminator and the value found, such as `unknown ContentBlock type "video"`
func (e *UnknownVariantError) Error() string {
	return "unknown " + e.Union + " " + e.Key + " " + strconv.Quote(e.Value)
}

// Failure of a handler that panicked, its text the panic value
type handlerPanic struct{ value any }

// Text of the panic value
func (e *handlerPanic) Error() string {
	if err, ok := e.value.(error); ok {
		return err.Error()
	}
	return fmt.Sprint(e.value)
}

// Unwraps to the panic value when that is an error
func (e *handlerPanic) Unwrap() error {
	err, _ := e.value.(error)
	return err
}

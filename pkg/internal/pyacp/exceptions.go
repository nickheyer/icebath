package pyacp

import "github.com/nickheyer/icebath/pkg/internal/pyjson"

// JSON-RPC error raised by a handler or received for an outgoing request
type RequestError struct {
	Code    int
	Message string
	Data    any
}

// Message text, the str form of the Python exception
func (e *RequestError) Error() string { return e.Message }

// Error with the given code, message and data
func NewRequestError(code int, message string, data any) *RequestError {
	return &RequestError{Code: code, Message: message, Data: data}
}

func dataOrNil(data *pyjson.Object) any {
	if data == nil {
		return nil
	}
	return data
}

// Code -32700 Parse error
func ParseError(data *pyjson.Object) *RequestError {
	return NewRequestError(-32700, "Parse error", dataOrNil(data))
}

// Code -32600 Invalid request
func InvalidRequest(data *pyjson.Object) *RequestError {
	return NewRequestError(-32600, "Invalid request", dataOrNil(data))
}

// Code -32601 Method not found carrying the method name
func MethodNotFound(method string) *RequestError {
	d := pyjson.NewObject()
	d.Set("method", method)
	return NewRequestError(-32601, "Method not found", d)
}

// Code -32602 Invalid params
func InvalidParams(data *pyjson.Object) *RequestError {
	return NewRequestError(-32602, "Invalid params", dataOrNil(data))
}

// Code -32603 Internal error
func InternalError(data any) *RequestError {
	return NewRequestError(-32603, "Internal error", data)
}

// Code -32000 Authentication required
func AuthRequired(data *pyjson.Object) *RequestError {
	return NewRequestError(-32000, "Authentication required", dataOrNil(data))
}

// Code -32002 Resource not found carrying the uri when given
func ResourceNotFound(uri *string) *RequestError {
	if uri == nil {
		return NewRequestError(-32002, "Resource not found", nil)
	}
	d := pyjson.NewObject()
	d.Set("uri", *uri)
	return NewRequestError(-32002, "Resource not found", d)
}

// Error object with code, message and data in wire order
func (e *RequestError) ToErrorObj() *pyjson.Object {
	o := pyjson.NewObject()
	o.Set("code", int64(e.Code))
	o.Set("message", e.Message)
	o.Set("data", e.Data)
	return o
}

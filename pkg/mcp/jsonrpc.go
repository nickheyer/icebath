// Package mcp connects the configured Model Context Protocol servers and exposes their tools to the agent
package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
)

// JSON-RPC 2.0 error codes the client reasons about
const (
	CodeParseError                 = -32700
	CodeInvalidRequest             = -32600
	CodeMethodNotFound             = -32601
	CodeInvalidParams              = -32602
	CodeInternalError              = -32603
	CodeUnsupportedProtocolVersion = -32022
)

// RPCError is the error member of a JSON-RPC response
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error renders the code and message the way the Python SDK stringifies MCPError
func (e *RPCError) Error() string {
	if e.Data != nil && len(e.Data) > 0 && string(e.Data) != "null" {
		return fmt.Sprintf("MCP error %d: %s (%s)", e.Code, e.Message, string(e.Data))
	}
	return fmt.Sprintf("MCP error %d: %s", e.Code, e.Message)
}

// Message is one JSON-RPC 2.0 request, notification, or response on the wire
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// IsRequest reports whether the message carries a method and an id
func (m *Message) IsRequest() bool { return m.Method != "" && len(m.ID) > 0 && string(m.ID) != "null" }

// IsNotification reports whether the message carries a method without an id
func (m *Message) IsNotification() bool {
	return m.Method != "" && (len(m.ID) == 0 || string(m.ID) == "null")
}

// IsResponse reports whether the message answers a request
func (m *Message) IsResponse() bool {
	return m.Method == "" && len(m.ID) > 0 && (m.Result != nil || m.Error != nil)
}

// IDKey normalizes a raw id so numeric and string spellings index one pending map
func IDKey(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return "s:" + s
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return "n:" + n.String()
	}
	return "r:" + string(raw)
}

var requestCounter atomic.Int64

// nextID returns a fresh integer request id encoded as JSON
func nextID() json.RawMessage {
	return json.RawMessage(strconv.FormatInt(requestCounter.Add(1), 10))
}

// NewRequest builds a request message with a fresh id
func NewRequest(method string, params any) (*Message, error) {
	raw, err := marshalParams(params)
	if err != nil {
		return nil, err
	}
	return &Message{JSONRPC: "2.0", ID: nextID(), Method: method, Params: raw}, nil
}

// NewNotification builds a notification message
func NewNotification(method string, params any) (*Message, error) {
	raw, err := marshalParams(params)
	if err != nil {
		return nil, err
	}
	return &Message{JSONRPC: "2.0", Method: method, Params: raw}, nil
}

// NewResponse builds a success response for the given id
func NewResponse(id json.RawMessage, result any) (*Message, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	return &Message{JSONRPC: "2.0", ID: id, Result: raw}, nil
}

// NewErrorResponse builds an error response for the given id
func NewErrorResponse(id json.RawMessage, code int, message string) *Message {
	return &Message{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: message}}
}

// marshalParams encodes params, mapping nil to an empty object
func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return json.RawMessage("{}"), nil
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("encode params: %w", err)
	}
	return raw, nil
}

// DecodeMessages parses one JSON document that is a single message or a batch
func DecodeMessages(data []byte) ([]*Message, error) {
	trimmed := trimLeadingSpace(data)
	if len(trimmed) == 0 {
		return nil, errors.New("empty JSON-RPC payload")
	}
	if trimmed[0] == '[' {
		var batch []*Message
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			return nil, fmt.Errorf("decode JSON-RPC batch: %w", err)
		}
		return batch, nil
	}
	var one Message
	if err := json.Unmarshal(trimmed, &one); err != nil {
		return nil, fmt.Errorf("decode JSON-RPC message: %w", err)
	}
	return []*Message{&one}, nil
}

// trimLeadingSpace drops JSON whitespace before the first token
func trimLeadingSpace(b []byte) []byte {
	i := 0
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return b[i:]
}

// RPCCode extracts the JSON-RPC error code carried anywhere in an error chain, zero when none
func RPCCode(err error) int {
	var rpc *RPCError
	if errors.As(err, &rpc) {
		return rpc.Code
	}
	return 0
}

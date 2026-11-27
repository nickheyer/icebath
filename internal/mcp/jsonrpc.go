package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
)

// JSON-RPC 2.0 error codes the client and server reason about
const (
	CodeParseError                 = -32700
	CodeInvalidRequest             = -32600
	CodeMethodNotFound             = -32601
	CodeInvalidParams              = -32602
	CodeInternalError              = -32603
	CodeUnsupportedProtocolVersion = -32022
	// The transport closed under a pending request
	CodeConnectionClosed = -32000
	// A request held up until a browser step of a URL mode elicitation is finished
	CodeURLElicitationRequired = -32042
)

// Error member of a JSON-RPC response
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if len(e.Data) > 0 && string(e.Data) != "null" {
		return fmt.Sprintf("MCP error %d: %s (%s)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("MCP error %d: %s", e.Code, e.Message)
}

// One JSON-RPC 2.0 request, notification or response on the wire
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

func (m *Message) hasID() bool { return len(m.ID) > 0 && string(m.ID) != "null" }

func (m *Message) IsRequest() bool { return m.Method != "" && m.hasID() }

func (m *Message) IsNotification() bool { return m.Method != "" && !m.hasID() }

func (m *Message) IsResponse() bool {
	return m.Method == "" && m.hasID() && (m.Result != nil || m.Error != nil)
}

// Key of a raw id, so numeric and string spellings of one id index the same entry
func IDKey(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return "s:" + text
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		return "n:" + number.String()
	}
	return "r:" + string(raw)
}

var counter atomic.Int64

func nextID() json.RawMessage {
	return json.RawMessage(strconv.FormatInt(counter.Add(1), 10))
}

func NewRequest(method string, params any) (*Message, error) {
	raw, err := encodeParams(params)
	if err != nil {
		return nil, err
	}
	return &Message{JSONRPC: "2.0", ID: nextID(), Method: method, Params: raw}, nil
}

func NewNotification(method string, params any) (*Message, error) {
	raw, err := encodeParams(params)
	if err != nil {
		return nil, err
	}
	return &Message{JSONRPC: "2.0", Method: method, Params: raw}, nil
}

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

func NewErrorResponse(id json.RawMessage, code int, message string) *Message {
	return &Message{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: message}}
}

// Params as JSON, nil standing for an empty object
func encodeParams(params any) (json.RawMessage, error) {
	if params == nil {
		return json.RawMessage("{}"), nil
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("Could not write the request: %w", err)
	}
	return raw, nil
}

// Messages of one JSON document, a single message or a batch
func DecodeMessages(data []byte) ([]*Message, error) {
	trimmed := trimSpace(data)
	if len(trimmed) == 0 {
		return nil, errors.New("The server sent an empty message.")
	}
	if trimmed[0] == '[' {
		var batch []*Message
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			return nil, fmt.Errorf("The server sent messages that cannot be read: %w", err)
		}
		return batch, nil
	}
	var one Message
	if err := json.Unmarshal(trimmed, &one); err != nil {
		return nil, fmt.Errorf("The server sent a message that cannot be read: %w", err)
	}
	return []*Message{&one}, nil
}

func trimSpace(b []byte) []byte {
	i := 0
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return b[i:]
}

// JSON-RPC error code anywhere in the chain, zero when there is none
func RPCCode(err error) int {
	if rpc, ok := errors.AsType[*RPCError](err); ok {
		return rpc.Code
	}
	return 0
}

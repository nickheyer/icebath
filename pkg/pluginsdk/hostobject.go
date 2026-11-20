package pluginsdk

import (
	"context"
	"fmt"
	"sync"
)

// Object living in icebath that the plugin reads and calls by name
type HostObject struct {
	peer *peer
	id   int64
	// Type name icebath reports for the object
	Type string
}

// Reads one attribute of the host object, a method name answering a TypeError
func (o *HostObject) Get(ctx context.Context, name string) (any, error) {
	value, err := o.peer.call(ctx, "parent_object", map[string]any{"id": o.id, "method": name, "probe": true})
	if err != nil {
		return nil, err
	}
	if probe, ok := value.(map[string]any); ok && len(probe) == 2 && probe["__method__"] == true {
		return nil, &Error{Type: "TypeError", Message: fmt.Sprintf("%s of %s is a method, call it", name, o.Type)}
	}
	return value, nil
}

// Calls one method of the host object
func (o *HostObject) Call(ctx context.Context, method string, args []any, kwargs map[string]any) (any, error) {
	return o.peer.call(ctx, "parent_object", map[string]any{"id": o.id, "method": method,
		"args": o.peer.encode(listOrEmpty(args)), "kwargs": o.peer.encode(mapOrEmpty(kwargs))})
}

// Text icebath shows for the host object
func (o *HostObject) Text(ctx context.Context) (string, error) {
	value, err := o.peer.call(ctx, "parent_object", map[string]any{"id": o.id, "repr": true})
	return text(value), err
}

// Error code of a message operation refused on the gateway loop, where the form for that loop runs in its place
const messageOnLoop = "on_event_loop"

// Outcome of one post or edit of a plugin message
type MessageResult struct {
	// Whether the operation succeeded
	OK bool
	// Id of the message the handle follows, empty before any message landed
	MessageID string
	// Code of a refusal or failure such as no_conversation, gateway_unavailable, timeout, send_failed or edit_failed
	Error string
	// Text explaining a refusal or failure
	Detail string
	// Whether an edit replaced the message in place
	Edited bool
	// Whether an edit posted a new message because the surface cannot edit
	Fallback bool
	// Whether an operation the gateway has not finished is still running, its outcome read later with Refresh
	Pending bool
	// Every field the gateway answered
	Fields map[string]any
}

// Message a plugin posted into the session of the call, its text replaceable in place
type Message struct {
	obj  *HostObject
	mu   sync.Mutex
	last MessageResult
}

// Outcome of the last operation this handle ran
func (m *Message) Result() MessageResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last
}

// Records the fields of an operation, the handle following a new message id only when the operation succeeded with one
func (m *Message) settle(ctx context.Context, fields map[string]any) (MessageResult, error) {
	result := MessageResult{Fields: mapOrEmpty(fields), Error: scalarText(fields["error"]), Detail: scalarText(fields["detail"])}
	result.OK, _ = fields["ok"].(bool)
	result.Edited, _ = fields["edited"].(bool)
	result.Fallback, _ = fields["fallback"].(bool)
	var err error
	if result.Error == "timeout" {
		var pending any
		pending, err = m.obj.Get(ctx, "pending")
		result.Pending, _ = pending.(bool)
	}
	m.mu.Lock()
	result.MessageID = m.last.MessageID
	if id := scalarText(fields["message_id"]); result.OK && id != "" {
		result.MessageID = id
	}
	m.last = result
	m.mu.Unlock()
	return result, err
}

// Replaces the text of the message, posting it anew where the surface cannot edit
func (m *Message) Edit(ctx context.Context, text string) (MessageResult, error) {
	value, err := m.obj.Call(ctx, "edit", []any{text}, nil)
	if err != nil {
		return MessageResult{}, err
	}
	fields, _ := value.(map[string]any)
	if scalarText(fields["error"]) == messageOnLoop {
		if value, err = m.obj.Call(ctx, "aedit", []any{text}, nil); err != nil {
			return MessageResult{}, err
		}
		fields, _ = value.(map[string]any)
	}
	return m.settle(ctx, fields)
}

// Reads the outcome the gateway holds for the message, the one of an operation that finished after it timed out included
func (m *Message) Refresh(ctx context.Context) (MessageResult, error) {
	value, err := m.obj.Get(ctx, "result")
	if err != nil {
		return MessageResult{}, err
	}
	fields, _ := value.(map[string]any)
	result := MessageResult{Fields: mapOrEmpty(fields), Error: scalarText(fields["error"]), Detail: scalarText(fields["detail"])}
	result.OK, _ = fields["ok"].(bool)
	result.Edited, _ = fields["edited"].(bool)
	result.Fallback, _ = fields["fallback"].(bool)
	id, err := m.obj.Get(ctx, "message_id")
	if err != nil {
		return MessageResult{}, err
	}
	result.MessageID = scalarText(id)
	pending, err := m.obj.Get(ctx, "pending")
	if err != nil {
		return MessageResult{}, err
	}
	result.Pending, _ = pending.(bool)
	m.mu.Lock()
	m.last = result
	m.mu.Unlock()
	return result, nil
}

// Handle of a posted message with the outcome of the post read into it
func (h *host) message(ctx context.Context, method, text string) (*Message, MessageResult, error) {
	value, err := h.Call(ctx, method, []any{text}, nil)
	if err != nil {
		return nil, MessageResult{}, err
	}
	obj, ok := value.(*HostObject)
	if !ok || obj == nil {
		return nil, MessageResult{}, &Error{Type: "TypeError", Message: fmt.Sprintf("%s answered no message handle", method)}
	}
	m := &Message{obj: obj}
	fields, err := obj.Get(ctx, "result")
	if err != nil {
		return nil, MessageResult{}, err
	}
	result, err := m.settle(ctx, asMap(fields))
	return m, result, err
}

// Posts text into the session of the call, in the form for the gateway loop when the call runs on it
func (h *host) PostMessage(ctx context.Context, text string) (*Message, error) {
	m, result, err := h.message(ctx, "post_message", text)
	if err == nil && result.Error == messageOnLoop {
		m, _, err = h.message(ctx, "apost_message", text)
	}
	return m, err
}

// Text of a scalar wire value, empty for none
func scalarText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	}
	return fmt.Sprint(v)
}

// Mapping of a wire value, nil for anything else
func asMap(v any) map[string]any {
	out, _ := v.(map[string]any)
	return out
}

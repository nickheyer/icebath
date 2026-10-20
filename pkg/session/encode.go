package session

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"
)

// Metadata and usage of a session as stored
type Header struct {
	ID      string            `json:"id"`
	Created time.Time         `json:"created"`
	Updated time.Time         `json:"updated"`
	Title   string            `json:"title,omitempty"`
	Model   string            `json:"model,omitempty"`
	Parent  string            `json:"parent,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
	Usage   protocol.Usage    `json:"usage"`
	Last    protocol.Usage    `json:"last"`
}

// On disk form of a whole session
type file struct {
	Header
	Messages []message `json:"messages"`
}

type message struct {
	Role    protocol.Role `json:"role"`
	Content []part        `json:"content"`
}

// Tagged union over every neutral part kind
type part struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	MediaType string          `json:"media_type,omitempty"`
	Data      []byte          `json:"data,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Redacted  bool            `json:"redacted,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Args      json.RawMessage `json:"args,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Content   []part          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// Metadata of the session as a store writes it
func (s *Session) Header() Header {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Header{ID: s.ID, Created: s.Created, Updated: s.Updated, Title: s.Title, Model: s.Model, Parent: s.Parent, Labels: s.Labels, Usage: s.Usage, Last: s.Last}
}

// Session rebuilt from a header and messages
func FromHeader(h Header, msgs []protocol.Message) *Session {
	return &Session{ID: h.ID, Created: h.Created, Updated: h.Updated, Title: h.Title, Model: h.Model, Parent: h.Parent, Labels: h.Labels, Usage: h.Usage, Last: h.Last, Messages: msgs}
}

// Serializes the whole session as one JSON document
func Marshal(s *Session) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	msgs, err := encodeMessages(s.Messages)
	if err != nil {
		return nil, err
	}
	return json.Marshal(file{Header: Header{ID: s.ID, Created: s.Created, Updated: s.Updated, Title: s.Title, Model: s.Model, Parent: s.Parent, Labels: s.Labels, Usage: s.Usage, Last: s.Last}, Messages: msgs})
}

// Rebuilds a session from a JSON document written by Marshal
func Unmarshal(data []byte) (*Session, error) {
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	msgs, err := decodeMessages(f.Messages)
	if err != nil {
		return nil, fmt.Errorf("session %s: %w", f.ID, err)
	}
	return FromHeader(f.Header, msgs), nil
}

// Reads only the header of a JSON document written by Marshal
func UnmarshalHeader(data []byte) (Header, int, error) {
	var f struct {
		Header
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return Header{}, 0, fmt.Errorf("session: %w", err)
	}
	return f.Header, len(f.Messages), nil
}

// Serializes parts as the tagged JSON array a stored message carries
func MarshalParts(parts []protocol.Part) ([]byte, error) {
	encoded, err := encodeParts(parts)
	if err != nil {
		return nil, err
	}
	return json.Marshal(encoded)
}

// Serializes one message
func MarshalMessage(m protocol.Message) ([]byte, error) {
	parts, err := encodeParts(m.Content)
	if err != nil {
		return nil, err
	}
	return json.Marshal(message{Role: m.Role, Content: parts})
}

// Rebuilds one message written by MarshalMessage
func UnmarshalMessage(data []byte) (protocol.Message, error) {
	var m message
	if err := json.Unmarshal(data, &m); err != nil {
		return protocol.Message{}, fmt.Errorf("session: %w", err)
	}
	parts, err := decodeParts(m.Content)
	if err != nil {
		return protocol.Message{}, err
	}
	return protocol.Message{Role: m.Role, Content: parts}, nil
}

func encodeMessages(msgs []protocol.Message) ([]message, error) {
	out := make([]message, len(msgs))
	for i, m := range msgs {
		parts, err := encodeParts(m.Content)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", i, err)
		}
		out[i] = message{Role: m.Role, Content: parts}
	}
	return out, nil
}

func encodeParts(parts []protocol.Part) ([]part, error) {
	out := make([]part, len(parts))
	for i, p := range parts {
		switch v := p.(type) {
		case protocol.Text:
			out[i] = part{Type: "text", Text: v.Text}
		case protocol.Image:
			out[i] = part{Type: "image", MediaType: v.MediaType, Data: v.Data}
		case protocol.Thinking:
			out[i] = part{Type: "thinking", Text: v.Text, Signature: v.Signature, Redacted: v.Redacted}
		case protocol.ToolCall:
			out[i] = part{Type: "tool_call", ID: v.ID, Name: v.Name, Args: v.Args, Signature: v.Signature}
		case protocol.ToolResult:
			content, err := encodeParts(v.Content)
			if err != nil {
				return nil, err
			}
			out[i] = part{Type: "tool_result", CallID: v.CallID, Name: v.Name, Content: content, IsError: v.IsError}
		default:
			return nil, fmt.Errorf("session: unknown part %T", p)
		}
	}
	return out, nil
}

func decodeMessages(msgs []message) ([]protocol.Message, error) {
	out := make([]protocol.Message, len(msgs))
	for i, m := range msgs {
		parts, err := decodeParts(m.Content)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", i, err)
		}
		out[i] = protocol.Message{Role: m.Role, Content: parts}
	}
	return out, nil
}

func decodeParts(parts []part) ([]protocol.Part, error) {
	out := make([]protocol.Part, len(parts))
	for i, p := range parts {
		switch p.Type {
		case "text":
			out[i] = protocol.Text{Text: p.Text}
		case "image":
			out[i] = protocol.Image{MediaType: p.MediaType, Data: p.Data}
		case "thinking":
			out[i] = protocol.Thinking{Text: p.Text, Signature: p.Signature, Redacted: p.Redacted}
		case "tool_call":
			out[i] = protocol.ToolCall{ID: p.ID, Name: p.Name, Args: p.Args, Signature: p.Signature}
		case "tool_result":
			content, err := decodeParts(p.Content)
			if err != nil {
				return nil, err
			}
			out[i] = protocol.ToolResult{CallID: p.CallID, Name: p.Name, Content: content, IsError: p.IsError}
		default:
			return nil, fmt.Errorf("part %d has unknown type %q", i, p.Type)
		}
	}
	return out, nil
}

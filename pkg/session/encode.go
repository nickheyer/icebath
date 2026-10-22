package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"
)

// Metadata and usage of a session as stored
type Header struct {
	ID       string            `json:"id"`
	Created  time.Time         `json:"created"`
	Updated  time.Time         `json:"updated"`
	Title    string            `json:"title,omitempty"`
	Named    bool              `json:"named,omitempty"`
	Model    string            `json:"model,omitempty"`
	Parent   string            `json:"parent,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	Usage    protocol.Usage    `json:"usage"`
	Last     protocol.Usage    `json:"last"`
	Messages int               `json:"messages"`
	Records  int               `json:"records"`
}

// Listing entry built from a header alone
func (h Header) Info() Info {
	return Info{ID: h.ID, Created: h.Created, Updated: h.Updated, Title: h.Title, Model: h.Model, Parent: h.Parent, Labels: copyLabels(h.Labels), Messages: h.Messages, Records: h.Records, Usage: h.Usage}
}

// On disk form of a whole session, header fields first so a reader can stop before the body
type file struct {
	Header
	Entries []entry  `json:"entries"`
	Log     []record `json:"log,omitempty"`
}

type entry struct {
	Seq     int            `json:"seq"`
	Time    time.Time      `json:"time"`
	Agent   string         `json:"agent,omitempty"`
	Model   string         `json:"model,omitempty"`
	Usage   protocol.Usage `json:"usage"`
	Role    protocol.Role  `json:"role"`
	Content []part         `json:"content"`
}

type record struct {
	Seq   int             `json:"seq"`
	Time  time.Time       `json:"time"`
	Agent string          `json:"agent,omitempty"`
	Kind  string          `json:"kind"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// Tagged union over every neutral part kind
type part struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Citations []citation      `json:"citations,omitempty"`
	MediaType string          `json:"media_type,omitempty"`
	Data      []byte          `json:"data,omitempty"`
	FileName  string          `json:"file_name,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Redacted  bool            `json:"redacted,omitempty"`
	Origin    protocol.ID     `json:"origin,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Args      json.RawMessage `json:"args,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Content   []part          `json:"content,omitempty"`
	Raw       json.RawMessage `json:"raw,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type citation struct {
	Title    string `json:"title,omitempty"`
	URL      string `json:"url,omitempty"`
	Document string `json:"document,omitempty"`
	Start    int    `json:"start"`
	End      int    `json:"end"`
	Cited    string `json:"cited,omitempty"`
}

// Metadata of the session as a store writes it
func (s *Session) Header() Header {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.header()
}

func (s *Session) header() Header {
	return Header{ID: s.id, Created: s.created, Updated: s.updated, Title: s.title, Named: s.named, Model: s.model, Parent: s.parent, Labels: copyLabels(s.labels), Usage: s.usage, Last: s.last, Messages: len(s.entries), Records: len(s.records)}
}

// Session rebuilt from a header, its entries and its records
func FromHeader(h Header, entries []Entry, records []Record) *Session {
	s := &Session{id: h.ID, created: h.Created, updated: h.Updated, title: h.Title, named: h.Named, model: h.Model, parent: h.Parent, labels: copyLabels(h.Labels), usage: h.Usage, last: h.Last, now: utcNow}
	s.entries = copyEntries(entries)
	s.records = make([]Record, len(records))
	copy(s.records, records)
	for i := range s.entries {
		if s.entries[i].Seq >= s.next {
			s.next = s.entries[i].Seq + 1
		}
	}
	for _, r := range s.records {
		if r.Seq >= s.next {
			s.next = r.Seq + 1
		}
	}
	return s
}

// Serializes the whole session as one JSON document
func Marshal(s *Session) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries, err := encodeEntries(s.entries)
	if err != nil {
		return nil, err
	}
	return json.Marshal(file{Header: s.header(), Entries: entries, Log: encodeRecords(s.records)})
}

// Rebuilds a session from a JSON document written by Marshal
func Unmarshal(data []byte) (*Session, error) {
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	entries, err := decodeEntries(f.Entries)
	if err != nil {
		return nil, fmt.Errorf("session %s: %w", f.ID, err)
	}
	return FromHeader(f.Header, entries, decodeRecords(f.Log)), nil
}

// Reads only the header of a JSON document written by Marshal, stopping before the entries
func ReadHeader(data []byte) (Header, error) {
	return ReadHeaderFrom(bytes.NewReader(data))
}

// Reads the header fields from the front of a document, leaving the body unread
func ReadHeaderFrom(r io.Reader) (Header, error) {
	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err != nil {
		return Header{}, fmt.Errorf("session: %w", err)
	}
	if tok != json.Delim('{') {
		return Header{}, fmt.Errorf("session: document is not an object")
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return Header{}, fmt.Errorf("session: %w", err)
		}
		key, _ := keyTok.(string)
		if key == "entries" || key == "log" {
			break
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return Header{}, fmt.Errorf("session: %w", err)
		}
		fields[key] = raw
	}
	joined, err := json.Marshal(fields)
	if err != nil {
		return Header{}, fmt.Errorf("session: %w", err)
	}
	var h Header
	if err := json.Unmarshal(joined, &h); err != nil {
		return Header{}, fmt.Errorf("session: %w", err)
	}
	if h.ID == "" {
		return Header{}, fmt.Errorf("session: header has no id")
	}
	return h, nil
}

// Serializes parts as the tagged JSON array a stored message carries
func MarshalParts(parts []protocol.Part) ([]byte, error) {
	encoded, err := encodeParts(parts)
	if err != nil {
		return nil, err
	}
	return json.Marshal(encoded)
}

// Rebuilds parts written by MarshalParts
func UnmarshalParts(data []byte) ([]protocol.Part, error) {
	var encoded []part
	if err := json.Unmarshal(data, &encoded); err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	return decodeParts(encoded)
}

// Serializes one entry
func MarshalEntry(e Entry) ([]byte, error) {
	enc, err := encodeEntry(e)
	if err != nil {
		return nil, err
	}
	return json.Marshal(enc)
}

// Rebuilds one entry written by MarshalEntry
func UnmarshalEntry(data []byte) (Entry, error) {
	var e entry
	if err := json.Unmarshal(data, &e); err != nil {
		return Entry{}, fmt.Errorf("session: %w", err)
	}
	return decodeEntry(e)
}

// Serializes one record
func MarshalRecord(r Record) ([]byte, error) {
	return json.Marshal(record{Seq: r.Seq, Time: r.Time, Agent: r.Agent, Kind: r.Kind, Data: r.Data})
}

// Rebuilds one record written by MarshalRecord
func UnmarshalRecord(data []byte) (Record, error) {
	var r record
	if err := json.Unmarshal(data, &r); err != nil {
		return Record{}, fmt.Errorf("session: %w", err)
	}
	if r.Kind == "" {
		return Record{}, fmt.Errorf("session: record without a kind")
	}
	return Record{Seq: r.Seq, Time: r.Time, Agent: r.Agent, Kind: r.Kind, Data: r.Data}, nil
}

func encodeEntries(entries []Entry) ([]entry, error) {
	out := make([]entry, len(entries))
	for i, e := range entries {
		enc, err := encodeEntry(e)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", i, err)
		}
		out[i] = enc
	}
	return out, nil
}

func encodeEntry(e Entry) (entry, error) {
	parts, err := encodeParts(e.Content)
	if err != nil {
		return entry{}, err
	}
	return entry{Seq: e.Seq, Time: e.Time, Agent: e.Agent, Model: e.Model, Usage: e.Usage, Role: e.Role, Content: parts}, nil
}

func encodeRecords(records []Record) []record {
	if len(records) == 0 {
		return nil
	}
	out := make([]record, len(records))
	for i, r := range records {
		out[i] = record{Seq: r.Seq, Time: r.Time, Agent: r.Agent, Kind: r.Kind, Data: r.Data}
	}
	return out
}

func decodeRecords(records []record) []Record {
	out := make([]Record, len(records))
	for i, r := range records {
		out[i] = Record{Seq: r.Seq, Time: r.Time, Agent: r.Agent, Kind: r.Kind, Data: r.Data}
	}
	return out
}

func encodeParts(parts []protocol.Part) ([]part, error) {
	out := make([]part, len(parts))
	for i, p := range parts {
		switch v := p.(type) {
		case protocol.Text:
			out[i] = part{Type: "text", Text: v.Text, Citations: encodeCitations(v.Citations)}
		case protocol.Image:
			out[i] = part{Type: "image", MediaType: v.MediaType, Data: v.Data}
		case protocol.Document:
			out[i] = part{Type: "document", MediaType: v.MediaType, Data: v.Data, FileName: v.Name}
		case protocol.Thinking:
			out[i] = part{Type: "thinking", Text: v.Text, Signature: v.Signature, Redacted: v.Redacted, Origin: v.Origin}
		case protocol.ToolCall:
			out[i] = part{Type: "tool_call", ID: v.ID, Name: v.Name, Args: v.Args, Signature: v.Signature, Origin: v.Origin}
		case protocol.ToolResult:
			content, err := encodeParts(v.Content)
			if err != nil {
				return nil, err
			}
			out[i] = part{Type: "tool_result", CallID: v.CallID, Name: v.Name, Content: content, IsError: v.IsError}
		case protocol.ServerToolCall:
			out[i] = part{Type: "server_tool_call", ID: v.ID, Name: v.Name, Args: v.Args, Origin: v.Origin}
		case protocol.ServerToolResult:
			out[i] = part{Type: "server_tool_result", CallID: v.CallID, Name: v.Name, Raw: v.Content, Origin: v.Origin}
		default:
			return nil, fmt.Errorf("session: unknown part %T", p)
		}
	}
	return out, nil
}

func encodeCitations(cs []protocol.Citation) []citation {
	if len(cs) == 0 {
		return nil
	}
	out := make([]citation, len(cs))
	for i, c := range cs {
		out[i] = citation{Title: c.Title, URL: c.URL, Document: c.Document, Start: c.Start, End: c.End, Cited: c.Cited}
	}
	return out
}

func decodeCitations(cs []citation) []protocol.Citation {
	if len(cs) == 0 {
		return nil
	}
	out := make([]protocol.Citation, len(cs))
	for i, c := range cs {
		out[i] = protocol.Citation{Title: c.Title, URL: c.URL, Document: c.Document, Start: c.Start, End: c.End, Cited: c.Cited}
	}
	return out
}

func decodeEntries(entries []entry) ([]Entry, error) {
	out := make([]Entry, len(entries))
	for i, e := range entries {
		dec, err := decodeEntry(e)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", i, err)
		}
		if dec.Seq == 0 && i > 0 {
			dec.Seq = out[i-1].Seq + 1
		}
		out[i] = dec
	}
	return out, nil
}

func decodeEntry(e entry) (Entry, error) {
	parts, err := decodeParts(e.Content)
	if err != nil {
		return Entry{}, err
	}
	return Entry{Seq: e.Seq, Time: e.Time, Agent: e.Agent, Model: e.Model, Usage: e.Usage, Message: protocol.Message{Role: e.Role, Content: parts}}, nil
}

func decodeParts(parts []part) ([]protocol.Part, error) {
	out := make([]protocol.Part, len(parts))
	for i, p := range parts {
		switch p.Type {
		case "text":
			out[i] = protocol.Text{Text: p.Text, Citations: decodeCitations(p.Citations)}
		case "image":
			out[i] = protocol.Image{MediaType: p.MediaType, Data: p.Data}
		case "document":
			out[i] = protocol.Document{MediaType: p.MediaType, Data: p.Data, Name: p.FileName}
		case "thinking":
			out[i] = protocol.Thinking{Text: p.Text, Signature: p.Signature, Redacted: p.Redacted, Origin: p.Origin}
		case "tool_call":
			out[i] = protocol.ToolCall{ID: p.ID, Name: p.Name, Args: p.Args, Signature: p.Signature, Origin: p.Origin}
		case "tool_result":
			content, err := decodeParts(p.Content)
			if err != nil {
				return nil, err
			}
			out[i] = protocol.ToolResult{CallID: p.CallID, Name: p.Name, Content: content, IsError: p.IsError}
		case "server_tool_call":
			out[i] = protocol.ServerToolCall{ID: p.ID, Name: p.Name, Args: p.Args, Origin: p.Origin}
		case "server_tool_result":
			out[i] = protocol.ServerToolResult{CallID: p.CallID, Name: p.Name, Content: p.Raw, Origin: p.Origin}
		default:
			return nil, fmt.Errorf("part %d has unknown type %q", i, p.Type)
		}
	}
	return out, nil
}

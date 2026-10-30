// Transcript of one conversation with usage totals and pluggable persistence
package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"
)

// One stored message with when it was added, who produced it and what it cost
type Entry struct {
	// Position on the shared timeline of entries and records
	Seq   int
	Time  time.Time
	Agent string
	Model string
	// Id the provider gave the response an assistant turn came from
	Response string
	Usage    protocol.Usage
	protocol.Message
}

// Event worth replaying on the timeline that is not a message, such as an approval or a compaction
type Record struct {
	Seq   int
	Time  time.Time
	Agent string
	Kind  string
	Data  json.RawMessage
}

// Ordered messages of a conversation and the token usage its responses have cost so far
type Session struct {
	mu      sync.RWMutex
	id      string
	created time.Time
	updated time.Time
	title   string
	named   bool
	model   string
	parent  string
	labels  map[string]string
	entries []Entry
	records []Record
	usage   protocol.Usage
	last    protocol.Usage
	next    int
	// Stubs standing in for tool results in requests, by call id
	pruned map[string]string
}

// Starts an empty session with a random id
func New() *Session {
	var b [8]byte
	rand.Read(b[:])
	now := time.Now().UTC()
	return &Session{id: now.Format("20060102-150405") + "-" + hex.EncodeToString(b[:]), created: now, updated: now}
}

func (s *Session) touch() { s.updated = time.Now().UTC() }

func (s *Session) ID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.id
}

func (s *Session) Created() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.created
}

func (s *Session) Updated() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.updated
}

func (s *Session) Title() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.title
}

func (s *Session) Model() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.model
}

// Id of the session this one was forked from
func (s *Session) Parent() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.parent
}

// Copy of the labels
func (s *Session) Labels() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return copyLabels(s.labels)
}

// Token usage summed over every response
func (s *Session) Usage() protocol.Usage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.usage
}

// Usage of the most recent response, whose input side measures the current context
func (s *Session) Last() protocol.Usage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.last
}

// Appends input from the user as an entry with no agent
func (s *Session) Append(m protocol.Message) Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.add(Entry{Message: copyMessage(m)})
}

// Appends the assistant turn of a response produced by agent and adds its usage to the totals
func (s *Session) AddResponse(agent string, r protocol.Response) Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage = s.usage.Add(r.Usage)
	s.last = r.Usage
	if s.model == "" && r.Model != "" {
		s.model = r.Model
	}
	return s.add(Entry{Agent: agent, Model: r.Model, Response: r.ID, Usage: r.Usage, Message: protocol.Message{Role: protocol.RoleAssistant, Content: copyParts(r.Content)}})
}

// Appends one tool message carrying every result of a tool round run by agent
func (s *Session) AddResults(agent string, results []protocol.ToolResult) Entry {
	parts := make([]protocol.Part, len(results))
	for i, r := range results {
		parts[i] = r
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.add(Entry{Agent: agent, Message: protocol.Message{Role: protocol.RoleTool, Content: copyParts(parts)}})
}

func (s *Session) add(e Entry) Entry {
	e.Seq = s.next
	s.next++
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	s.entries = append(s.entries, e)
	s.touch()
	return e
}

// Appends a timeline record of kind with data serialized as JSON
func (s *Session) Log(agent, kind string, data any) (Record, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Record{}, fmt.Errorf("session: record %s: %w", kind, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := Record{Seq: s.next, Time: time.Now().UTC(), Agent: agent, Kind: kind, Data: raw}
	s.next++
	s.records = append(s.records, r)
	s.touch()
	return r, nil
}

// Copy of every entry in order
func (s *Session) Entries() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return copyEntries(s.entries)
}

// Copy of every record in order
func (s *Session) Records() []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Record, len(s.records))
	for i, r := range s.records {
		r.Data = append(json.RawMessage(nil), r.Data...)
		out[i] = r
	}
	return out
}

// Copies the messages so callers can build a request without holding the lock
func (s *Session) Snapshot() []protocol.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.entries) == 0 {
		return nil
	}
	out := make([]protocol.Message, len(s.entries))
	for i, e := range s.entries {
		out[i] = copyMessage(e.Message)
	}
	return out
}

func (s *Session) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

// Reports whether the session holds no messages, records or title
func (s *Session) Empty() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries) == 0 && len(s.records) == 0 && s.title == ""
}

// Tokens the next request will carry, as measured by the last response
func (s *Session) Context() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.last.Context()
}

// Sets a derived title, left alone once a person named the session
func (s *Session) SetTitle(title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.named {
		return
	}
	s.title = title
	s.touch()
}

// Names the session as a person chose, an empty name handing the title back to derivation
func (s *Session) Rename(title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.title = title
	s.named = title != ""
	s.touch()
}

// Reports whether a person named the session
func (s *Session) Named() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.named
}

func (s *Session) SetModel(model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.model = model
	s.touch()
}

// Sets a label, removing it when the value is empty
func (s *Session) Label(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if value == "" {
		delete(s.labels, key)
	} else {
		if s.labels == nil {
			s.labels = map[string]string{}
		}
		s.labels[key] = value
	}
	s.touch()
}

// Starts a new session carrying a copy of the transcript, usage reset and parent recorded
func (s *Session) Fork() *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f := New()
	f.parent = s.id
	f.title, f.named, f.model, f.last = s.title, s.named, s.model, s.last
	f.entries = copyEntries(s.entries)
	f.records = s.copyRecords()
	f.labels = copyLabels(s.labels)
	f.pruned = copyLabels(s.pruned)
	f.next = s.next
	return f
}

// Remembers the stub that stands in for the tool result in requests from now on
func (s *Session) Prune(callID, stub string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pruned == nil {
		s.pruned = map[string]string{}
	}
	s.pruned[callID] = stub
}

// Stub standing in for the tool result, false while the result still goes out whole
func (s *Session) Pruned(callID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	stub, ok := s.pruned[callID]
	return stub, ok
}

// Drops every entry after the first n and every record newer than the last kept entry
func (s *Session) Truncate(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n < 0 {
		n = 0
	}
	if n >= len(s.entries) {
		return
	}
	s.entries = s.entries[:n]
	limit := -1
	if n > 0 {
		limit = s.entries[n-1].Seq
	}
	kept := s.records[:0]
	for _, r := range s.records {
		if r.Seq < limit {
			kept = append(kept, r)
		}
	}
	s.records = kept
	s.last = protocol.Usage{}
	for i := len(s.entries) - 1; i >= 0; i-- {
		if s.entries[i].Role == protocol.RoleAssistant {
			s.last = s.entries[i].Usage
			break
		}
	}
	s.touch()
}

// Index of the first entry kept verbatim, the first user message at or past the keep boundary
func (s *Session) cut(keep int) int {
	cut := len(s.entries) - max(keep, 0)
	if cut < 0 {
		cut = 0
	}
	for cut < len(s.entries) && s.entries[cut].Role != protocol.RoleUser {
		cut++
	}
	return cut
}

// Entries a compaction keeping that many messages replaces with its summary
func (s *Session) Head(keep int) []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return copyEntries(s.entries[:s.cut(keep)])
}

// Assistant turn written after a summary so the kept tail still follows an assistant message
const Ack = "Understood. I have the summary of the earlier conversation and will continue from here."

// Replaces everything before the last keep messages with a summary ahead of a user turn and resets Last
func (s *Session) Compact(summary string, keep int) error {
	if summary == "" {
		return errors.New("session: summary is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cut := s.cut(keep)
	now := time.Now().UTC()
	head := []Entry{{Seq: s.next, Time: now, Message: protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: summary}}}}}
	s.next++
	if cut < len(s.entries) {
		head = append(head, Entry{Seq: s.next, Time: now, Message: protocol.Message{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.Text{Text: Ack}}}})
		s.next++
	}
	s.entries = append(head, s.entries[cut:]...)
	s.last = protocol.Usage{}
	s.touch()
	return nil
}

// Metadata of a session without its messages
type Info struct {
	ID       string            `json:"id"`
	Created  time.Time         `json:"created"`
	Updated  time.Time         `json:"updated"`
	Title    string            `json:"title,omitempty"`
	Model    string            `json:"model,omitempty"`
	Parent   string            `json:"parent,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	Messages int               `json:"messages"`
	Records  int               `json:"records"`
	Usage    protocol.Usage    `json:"usage"`
	// Whether another process has the session open right now
	Busy bool `json:"busy,omitempty"`
	// Why a store could not read the session, empty when it loaded cleanly
	Error string `json:"error,omitempty"`
}

// Label naming the project root a session was started under, by which stores list the sessions of a project
const ProjectLabel = "project"

// Project root the session was started under, empty for one started before projects were recorded
func (s *Session) Project() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.labels[ProjectLabel]
}

func (s *Session) Info() Info {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Info{ID: s.id, Created: s.created, Updated: s.updated, Title: s.title, Model: s.model, Parent: s.parent, Labels: copyLabels(s.labels), Messages: len(s.entries), Records: len(s.records), Usage: s.usage}
}

// Orders newest first by update time, then by id
func SortInfos(infos []Info) {
	sort.SliceStable(infos, func(i, j int) bool {
		if !infos[i].Updated.Equal(infos[j].Updated) {
			return infos[i].Updated.After(infos[j].Updated)
		}
		return infos[i].ID > infos[j].ID
	})
}

// Reports whether an id is safe to use as a file name
func ValidID(id string) bool {
	if id == "" {
		return false
	}
	for i := 0; i < len(id); i++ {
		if !wordByte(id[i]) {
			return false
		}
	}
	return true
}

// Reports whether the byte is a letter, digit, underscore or dash
func wordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// Error for an id a store cannot use
func InvalidID(id string) error { return fmt.Errorf("session: invalid id %q", id) }

// Error for a session a store does not hold
var ErrNotFound = errors.New("session: not found")

func (s *Session) copyRecords() []Record {
	out := make([]Record, len(s.records))
	for i, r := range s.records {
		r.Data = append(json.RawMessage(nil), r.Data...)
		out[i] = r
	}
	return out
}

func copyEntries(entries []Entry) []Entry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]Entry, len(entries))
	for i, e := range entries {
		e.Message = copyMessage(e.Message)
		out[i] = e
	}
	return out
}

func copyMessage(m protocol.Message) protocol.Message {
	return protocol.Message{Role: m.Role, Content: copyParts(m.Content)}
}

// Copies parts deeply enough that byte slices and nested results never alias the source
func copyParts(parts []protocol.Part) []protocol.Part {
	if parts == nil {
		return nil
	}
	out := make([]protocol.Part, len(parts))
	for i, p := range parts {
		switch v := p.(type) {
		case protocol.Image:
			v.Data = append([]byte(nil), v.Data...)
			out[i] = v
		case protocol.Document:
			v.Data = append([]byte(nil), v.Data...)
			out[i] = v
		case protocol.Text:
			v.Citations = append([]protocol.Citation(nil), v.Citations...)
			out[i] = v
		case protocol.ToolCall:
			v.Args = append(json.RawMessage(nil), v.Args...)
			out[i] = v
		case protocol.ServerToolCall:
			v.Args = append(json.RawMessage(nil), v.Args...)
			out[i] = v
		case protocol.ServerToolResult:
			v.Content = append(json.RawMessage(nil), v.Content...)
			out[i] = v
		case protocol.ToolResult:
			v.Content = copyParts(v.Content)
			out[i] = v
		default:
			out[i] = p
		}
	}
	return out
}

func copyLabels(labels map[string]string) map[string]string {
	if labels == nil {
		return nil
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	return out
}

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

type entry struct {
	Seq      int            `json:"seq"`
	Time     time.Time      `json:"time"`
	Agent    string         `json:"agent,omitempty"`
	Model    string         `json:"model,omitempty"`
	Response string         `json:"response_id,omitempty"`
	Usage    protocol.Usage `json:"usage"`
	Role     protocol.Role  `json:"role"`
	Content  []part         `json:"content"`
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
	s := &Session{id: h.ID, created: h.Created, updated: h.Updated, title: h.Title, named: h.Named, model: h.Model, parent: h.Parent, labels: copyLabels(h.Labels), usage: h.Usage, last: h.Last}
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

// Serializes parts as the tagged JSON array a stored message carries
func MarshalParts(parts []protocol.Part) ([]byte, error) {
	encoded, err := encodeParts(parts)
	if err != nil {
		return nil, err
	}
	return json.Marshal(encoded)
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

func encodeEntry(e Entry) (entry, error) {
	parts, err := encodeParts(e.Content)
	if err != nil {
		return entry{}, err
	}
	return entry{Seq: e.Seq, Time: e.Time, Agent: e.Agent, Model: e.Model, Response: e.Response, Usage: e.Usage, Role: e.Role, Content: parts}, nil
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

func decodeEntry(e entry) (Entry, error) {
	parts, err := decodeParts(e.Content)
	if err != nil {
		return Entry{}, err
	}
	return Entry{Seq: e.Seq, Time: e.Time, Agent: e.Agent, Model: e.Model, Response: e.Response, Usage: e.Usage, Message: protocol.Message{Role: e.Role, Content: parts}}, nil
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

// Controls which parts of a transcript the Markdown export includes
type MarkdownOptions struct {
	// Adds the time and producing agent under each heading
	Timestamps bool
	// Appends the record log after the transcript
	Records bool
}

// Renders the full transcript as Markdown with a heading per turn
func (s *Session) Markdown() string { return s.MarkdownWith(MarkdownOptions{}) }

// Renders the transcript as Markdown honoring the options
func (s *Session) MarkdownWith(o MarkdownOptions) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var sb strings.Builder
	title := s.title
	if title == "" {
		title = s.id
	}
	fmt.Fprintf(&sb, "# %s\n\n", title)
	fmt.Fprintf(&sb, "- id: %s\n- created: %s\n", s.id, s.created.Format("2006-01-02 15:04:05 UTC"))
	if s.model != "" {
		fmt.Fprintf(&sb, "- model: %s\n", s.model)
	}
	if s.parent != "" {
		fmt.Fprintf(&sb, "- forked from: %s\n", s.parent)
	}
	fmt.Fprintf(&sb, "- usage: %d in, %d out, %d cache read, %d cache write, %d reasoning\n", s.usage.Input, s.usage.Output, s.usage.CacheRead, s.usage.CacheWrite, s.usage.Reasoning)
	for _, e := range s.entries {
		fmt.Fprintf(&sb, "\n## %s\n\n", heading(e.Role))
		if o.Timestamps {
			meta := e.Time.Format("2006-01-02 15:04:05 UTC")
			if e.Agent != "" {
				meta += " by " + e.Agent
			}
			if e.Model != "" {
				meta += " on " + e.Model
			}
			if e.Response != "" {
				meta += " as " + e.Response
			}
			fmt.Fprintf(&sb, "_%s_\n\n", meta)
		}
		for _, p := range e.Content {
			writePart(&sb, p)
		}
	}
	if o.Records && len(s.records) > 0 {
		sb.WriteString("\n## Records\n\n")
		for _, r := range s.records {
			fmt.Fprintf(&sb, "- %s %s", r.Time.Format("15:04:05"), r.Kind)
			if r.Agent != "" {
				fmt.Fprintf(&sb, " (%s)", r.Agent)
			}
			if len(r.Data) > 0 {
				fmt.Fprintf(&sb, " `%s`", string(r.Data))
			}
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

func heading(role protocol.Role) string {
	switch role {
	case protocol.RoleUser:
		return "User"
	case protocol.RoleAssistant:
		return "Assistant"
	case protocol.RoleTool:
		return "Tool results"
	}
	return string(role)
}

func writePart(sb *strings.Builder, p protocol.Part) {
	switch v := p.(type) {
	case protocol.Text:
		sb.WriteString(strings.TrimRight(v.Text, "\n"))
		sb.WriteString("\n\n")
		for _, c := range v.Citations {
			label := c.Title
			if label == "" {
				label = c.URL
			}
			if label == "" {
				label = c.Document
			}
			if c.URL != "" {
				fmt.Fprintf(sb, "- [%s](%s)\n", label, c.URL)
			} else {
				fmt.Fprintf(sb, "- %s\n", label)
			}
		}
		if len(v.Citations) > 0 {
			sb.WriteString("\n")
		}
	case protocol.Image:
		fmt.Fprintf(sb, "[image %s, %d bytes]\n\n", v.MediaType, len(v.Data))
	case protocol.Document:
		name := v.Name
		if name == "" {
			name = "document"
		}
		fmt.Fprintf(sb, "[%s %s, %d bytes]\n\n", name, v.MediaType, len(v.Data))
	case protocol.Thinking:
		if v.Redacted {
			sb.WriteString("> [redacted thinking]\n\n")
			return
		}
		for line := range strings.SplitSeq(strings.TrimRight(v.Text, "\n"), "\n") {
			sb.WriteString("> ")
			sb.WriteString(line)
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	case protocol.ToolCall:
		fmt.Fprintf(sb, "**Call %s** `%s`\n\n```json\n%s\n```\n\n", v.ID, v.Name, string(v.Args))
	case protocol.ServerToolCall:
		fmt.Fprintf(sb, "**Server call %s** `%s`\n\n```json\n%s\n```\n\n", v.ID, v.Name, string(v.Args))
	case protocol.ServerToolResult:
		fmt.Fprintf(sb, "**Server result %s** `%s`\n\n```json\n%s\n```\n\n", v.CallID, v.Name, string(v.Content))
	case protocol.ToolResult:
		status := ""
		if v.IsError {
			status = " (error)"
		}
		fmt.Fprintf(sb, "**Result %s** `%s`%s\n\n", v.CallID, v.Name, status)
		for _, inner := range v.Content {
			writePart(sb, inner)
		}
	}
}

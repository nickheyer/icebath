// Transcript of one conversation with usage totals and pluggable persistence
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
	Usage protocol.Usage
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
	now     func() time.Time
}

// Starts an empty session with a random id
func New() *Session {
	var b [8]byte
	rand.Read(b[:])
	now := time.Now().UTC()
	return &Session{id: now.Format("20060102-150405") + "-" + hex.EncodeToString(b[:]), created: now, updated: now, now: utcNow}
}

func utcNow() time.Time { return time.Now().UTC() }

func (s *Session) clock() time.Time {
	if s.now == nil {
		s.now = utcNow
	}
	return s.now()
}

func (s *Session) touch() { s.updated = s.clock() }

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
	return s.add(Entry{Agent: agent, Model: r.Model, Usage: r.Usage, Message: protocol.Message{Role: protocol.RoleAssistant, Content: copyParts(r.Content)}})
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
		e.Time = s.clock()
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
	r := Record{Seq: s.next, Time: s.clock(), Agent: agent, Kind: kind, Data: raw}
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
	f.next = s.next
	return f
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

// Assistant turn written after a summary so the kept tail still follows an assistant message
const Ack = "Understood. I have the summary of the earlier conversation and will continue from here."

// Replaces everything before the last keep messages with a summary ahead of a user turn and resets Last
func (s *Session) Compact(summary string, keep int) error {
	if summary == "" {
		return errors.New("session: summary is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cut := len(s.entries) - max(keep, 0)
	if cut < 0 {
		cut = 0
	}
	for cut < len(s.entries) && s.entries[cut].Role != protocol.RoleUser {
		cut++
	}
	now := s.clock()
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
	// Why a store could not read the session, empty when it loaded cleanly
	Error string `json:"error,omitempty"`
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

// Persists sessions by id
type Store interface {
	Save(ctx context.Context, s *Session) error
	Load(ctx context.Context, id string) (*Session, error)
	// Lists stored sessions newest first, unreadable ones carrying their error
	List(ctx context.Context) ([]Info, error)
	Exists(ctx context.Context, id string) (bool, error)
	Delete(ctx context.Context, id string) error
}

// Store that can name the file a session lives in
type Pather interface {
	Path(id string) (string, error)
}

// Store holding sessions in memory
type Memory struct {
	mu       sync.Mutex
	sessions map[string][]byte
}

func (m *Memory) Save(ctx context.Context, s *Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !ValidID(s.ID()) {
		return InvalidID(s.ID())
	}
	data, err := Marshal(s)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions == nil {
		m.sessions = map[string][]byte{}
	}
	m.sessions[s.ID()] = data
	return nil
}

func (m *Memory) Load(ctx context.Context, id string) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	data, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return nil, ErrNotFound
	}
	return Unmarshal(data)
}

func (m *Memory) List(ctx context.Context) ([]Info, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	infos := make([]Info, 0, len(m.sessions))
	for id, data := range m.sessions {
		h, err := ReadHeader(data)
		if err != nil {
			infos = append(infos, Info{ID: id, Error: err.Error()})
			continue
		}
		infos = append(infos, h.Info())
	}
	SortInfos(infos)
	return infos, nil
}

func (m *Memory) Exists(ctx context.Context, id string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.sessions[id]
	return ok, nil
}

func (m *Memory) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[id]; !ok {
		return ErrNotFound
	}
	delete(m.sessions, id)
	return nil
}

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

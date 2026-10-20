// Transcript of one conversation with usage totals and pluggable persistence
package session

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"
)

// Ordered messages of a conversation and the token usage its responses have cost so far
type Session struct {
	mu      sync.RWMutex
	ID      string
	Created time.Time
	Updated time.Time
	Title   string
	Model   string
	// Id of the session this one was forked from
	Parent   string
	Labels   map[string]string
	Messages []protocol.Message
	Usage    protocol.Usage
	// Usage of the most recent response, whose input side measures the current context
	Last protocol.Usage
}

// Starts an empty session with a random id
func New() *Session {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("session: random id: " + err.Error())
	}
	now := time.Now().UTC()
	return &Session{ID: now.Format("20060102-150405") + "-" + hex.EncodeToString(b[:]), Created: now, Updated: now}
}

func (s *Session) touch() { s.Updated = time.Now().UTC() }

func (s *Session) Append(m protocol.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Messages = append(s.Messages, m)
	s.touch()
}

// Appends the assistant turn of a response and adds its usage to the totals
func (s *Session) AddResponse(r protocol.Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Messages = append(s.Messages, protocol.Message{Role: protocol.RoleAssistant, Content: append([]protocol.Part(nil), r.Content...)})
	s.Usage.Input += r.Usage.Input
	s.Usage.Output += r.Usage.Output
	s.Usage.CacheRead += r.Usage.CacheRead
	s.Usage.CacheWrite += r.Usage.CacheWrite
	s.Usage.Reasoning += r.Usage.Reasoning
	s.Last = r.Usage
	if s.Model == "" && r.Model != "" {
		s.Model = r.Model
	}
	s.touch()
}

// Appends one tool message carrying every result of a tool round
func (s *Session) AddResults(results []protocol.ToolResult) {
	parts := make([]protocol.Part, len(results))
	for i, r := range results {
		parts[i] = r
	}
	s.Append(protocol.Message{Role: protocol.RoleTool, Content: parts})
}

// Copies the messages so callers can build a request without holding the lock
func (s *Session) Snapshot() []protocol.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return copyMessages(s.Messages)
}

func copyMessages(msgs []protocol.Message) []protocol.Message {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]protocol.Message, len(msgs))
	for i, m := range msgs {
		out[i] = protocol.Message{Role: m.Role, Content: append([]protocol.Part(nil), m.Content...)}
	}
	return out
}

func (s *Session) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.Messages)
}

// Tokens the next request will carry, as measured by the last response
func (s *Session) Context() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Last.Input + s.Last.CacheRead + s.Last.CacheWrite
}

func (s *Session) SetTitle(title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Title = title
	s.touch()
}

func (s *Session) SetModel(model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Model = model
	s.touch()
}

// Sets a label, removing it when the value is empty
func (s *Session) Label(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if value == "" {
		delete(s.Labels, key)
	} else {
		if s.Labels == nil {
			s.Labels = map[string]string{}
		}
		s.Labels[key] = value
	}
	s.touch()
}

// Starts a new session carrying a copy of the transcript, usage reset and parent recorded
func (s *Session) Fork() *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f := New()
	f.Parent = s.ID
	f.Title, f.Model, f.Last = s.Title, s.Model, s.Last
	f.Messages = copyMessages(s.Messages)
	if s.Labels != nil {
		f.Labels = make(map[string]string, len(s.Labels))
		for k, v := range s.Labels {
			f.Labels[k] = v
		}
	}
	return f
}

// Drops every message after the first n
func (s *Session) Truncate(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n < 0 {
		n = 0
	}
	if n < len(s.Messages) {
		s.Messages = s.Messages[:n]
		s.touch()
	}
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
	cut := len(s.Messages) - max(keep, 0)
	if cut < 0 {
		cut = 0
	}
	for cut < len(s.Messages) && s.Messages[cut].Role != protocol.RoleUser {
		cut++
	}
	head := []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: summary}}}}
	if cut < len(s.Messages) {
		head = append(head, protocol.Message{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.Text{Text: Ack}}})
	}
	s.Messages = append(head, s.Messages[cut:]...)
	s.Last = protocol.Usage{}
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
	Usage    protocol.Usage    `json:"usage"`
}

func (s *Session) Info() Info {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Info{ID: s.ID, Created: s.Created, Updated: s.Updated, Title: s.Title, Model: s.Model, Parent: s.Parent, Labels: s.Labels, Messages: len(s.Messages), Usage: s.Usage}
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

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Reports whether an id is safe to use as a file name
func ValidID(id string) bool { return idPattern.MatchString(id) }

// Error for an id a store cannot use
func InvalidID(id string) error { return fmt.Errorf("session: invalid id %q", id) }

// Error for a session a store does not hold
var ErrNotFound = errors.New("session: not found")

// Persists sessions by id
type Store interface {
	Save(s *Session) error
	Load(id string) (*Session, error)
	// Lists stored sessions newest first
	List() ([]Info, error)
	Delete(id string) error
}

// Store holding sessions in memory
type Memory struct {
	mu       sync.Mutex
	sessions map[string][]byte
}

func (m *Memory) Save(s *Session) error {
	if !ValidID(s.ID) {
		return InvalidID(s.ID)
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
	m.sessions[s.ID] = data
	return nil
}

func (m *Memory) Load(id string) (*Session, error) {
	m.mu.Lock()
	data, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return nil, ErrNotFound
	}
	return Unmarshal(data)
}

func (m *Memory) List() ([]Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	infos := make([]Info, 0, len(m.sessions))
	for _, data := range m.sessions {
		s, err := Unmarshal(data)
		if err != nil {
			return nil, err
		}
		infos = append(infos, s.Info())
	}
	SortInfos(infos)
	return infos, nil
}

func (m *Memory) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[id]; !ok {
		return ErrNotFound
	}
	delete(m.sessions, id)
	return nil
}

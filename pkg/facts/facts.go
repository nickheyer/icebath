// Store of findings an agent records while it works, persisted as session records and exported as Markdown
package facts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/tool"
)

// Kind of record a session keeps for every fact change
const RecordKind = "fact"

// Where a fact was seen
type Evidence struct {
	// What the reference names, such as file, url, command or tool
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
	Line int    `json:"line,omitempty"`
	// Passage copied from the source
	Quote string `json:"quote,omitempty"`
}

// One recorded finding
type Fact struct {
	ID    string    `json:"id"`
	Time  time.Time `json:"time"`
	Agent string    `json:"agent,omitempty"`
	// Grouping such as finding, risk, decision, question or note
	Kind  string   `json:"kind"`
	Title string   `json:"title"`
	Body  string   `json:"body,omitempty"`
	Tags  []string `json:"tags,omitempty"`
	// Belief from zero to one, one when the agent states it as verified
	Confidence float64    `json:"confidence"`
	Evidence   []Evidence `json:"evidence,omitempty"`
	// Open, resolved or retracted
	Status string `json:"status"`
	// Id of the fact this one refines
	Parent string `json:"parent,omitempty"`
}

const (
	StatusOpen      = "open"
	StatusResolved  = "resolved"
	StatusRetracted = "retracted"
)

// Kinds a fact may use when the caller does not restrict them
var DefaultKinds = []string{"finding", "risk", "decision", "question", "note"}

// Change to the store, written to the session log and handed to listeners
type Change struct {
	Op   string `json:"op"`
	Fact Fact   `json:"fact"`
}

const (
	OpAdd    = "add"
	OpUpdate = "update"
	OpRemove = "remove"
)

// Filter over the store
type Query struct {
	Kind   string
	Tag    string
	Status string
	Agent  string
	// Case insensitive match over title, body, tags and evidence references
	Text string
	// Smallest confidence included
	MinConfidence float64
}

// Facts keyed by id in insertion order with change listeners
type Store struct {
	mu       sync.RWMutex
	facts    map[string]*Fact
	order    []string
	next     int
	session  *session.Session
	onChange []func(Change)
}

func New() *Store {
	return &Store{facts: map[string]*Fact{}}
}

// Writes every later change into the session log so the facts survive with the transcript
func (s *Store) Attach(sess *session.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.session = sess
}

// Registers a listener called after every change outside the store lock
func (s *Store) OnChange(fn func(Change)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onChange = append(s.onChange, fn)
}

func (s *Store) emit(c Change) {
	s.mu.RLock()
	fns := append([]func(Change){}, s.onChange...)
	sess := s.session
	s.mu.RUnlock()
	if sess != nil {
		sess.Log(c.Fact.Agent, RecordKind, c)
	}
	for _, fn := range fns {
		fn(c)
	}
}

// Rebuilds the store from the fact records of a session, replaying adds, updates and removes in order
func (s *Store) Restore(sess *session.Session) error {
	for _, r := range sess.Records() {
		if r.Kind != RecordKind {
			continue
		}
		var c Change
		if err := json.Unmarshal(r.Data, &c); err != nil {
			return fmt.Errorf("facts: record %d: %w", r.Seq, err)
		}
		s.mu.Lock()
		switch c.Op {
		case OpAdd, OpUpdate:
			f := c.Fact
			if _, ok := s.facts[f.ID]; !ok {
				s.order = append(s.order, f.ID)
			}
			s.facts[f.ID] = &f
			s.bump(f.ID)
		case OpRemove:
			s.drop(c.Fact.ID)
		}
		s.mu.Unlock()
	}
	return nil
}

// Raises the counter past any numeric id so new ids never collide with restored ones
func (s *Store) bump(id string) {
	var n int
	if _, err := fmt.Sscanf(id, "f%d", &n); err == nil && n > s.next {
		s.next = n
	}
}

func (s *Store) drop(id string) {
	if _, ok := s.facts[id]; !ok {
		return
	}
	delete(s.facts, id)
	for i, o := range s.order {
		if o == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// Checks what every fact needs and normalizes its kind, status, tags and confidence
func (f *Fact) validate() error {
	f.Title = strings.TrimSpace(f.Title)
	if f.Title == "" {
		return errors.New("facts: title is required")
	}
	f.Kind = strings.ToLower(strings.TrimSpace(f.Kind))
	if f.Kind == "" {
		f.Kind = DefaultKinds[0]
	}
	switch f.Status {
	case "":
		f.Status = StatusOpen
	case StatusOpen, StatusResolved, StatusRetracted:
	default:
		return fmt.Errorf("facts: unknown status %q", f.Status)
	}
	if f.Confidence < 0 || f.Confidence > 1 {
		return fmt.Errorf("facts: confidence %v is outside 0 to 1", f.Confidence)
	}
	tags := f.Tags[:0]
	seen := map[string]bool{}
	for _, t := range f.Tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		tags = append(tags, t)
	}
	f.Tags = tags
	for i := range f.Evidence {
		e := &f.Evidence[i]
		e.Ref = strings.TrimSpace(e.Ref)
		if e.Ref == "" {
			return fmt.Errorf("facts: evidence %d has no reference", i)
		}
		if e.Kind == "" {
			e.Kind = "file"
		}
	}
	return nil
}

// Records a fact, assigning an id and time when the caller gave none
func (s *Store) Add(f Fact) (Fact, error) {
	if err := f.validate(); err != nil {
		return Fact{}, err
	}
	s.mu.Lock()
	if f.ID == "" {
		s.next++
		f.ID = fmt.Sprintf("f%d", s.next)
	} else {
		s.bump(f.ID)
	}
	if _, dup := s.facts[f.ID]; dup {
		s.mu.Unlock()
		return Fact{}, fmt.Errorf("facts: %s already exists", f.ID)
	}
	if f.Time.IsZero() {
		f.Time = time.Now().UTC()
	}
	if f.Parent != "" {
		if _, ok := s.facts[f.Parent]; !ok {
			s.mu.Unlock()
			return Fact{}, fmt.Errorf("facts: parent %s does not exist", f.Parent)
		}
	}
	stored := f
	s.facts[f.ID] = &stored
	s.order = append(s.order, f.ID)
	s.mu.Unlock()
	s.emit(Change{Op: OpAdd, Fact: f})
	return f, nil
}

// Replaces the fact with the same id, keeping its time and position
func (s *Store) Update(f Fact) (Fact, error) {
	if err := f.validate(); err != nil {
		return Fact{}, err
	}
	s.mu.Lock()
	old, ok := s.facts[f.ID]
	if !ok {
		s.mu.Unlock()
		return Fact{}, fmt.Errorf("facts: %s does not exist", f.ID)
	}
	if f.Time.IsZero() {
		f.Time = old.Time
	}
	if f.Agent == "" {
		f.Agent = old.Agent
	}
	stored := f
	s.facts[f.ID] = &stored
	s.mu.Unlock()
	s.emit(Change{Op: OpUpdate, Fact: f})
	return f, nil
}

// Marks a fact resolved or retracted without rewriting it
func (s *Store) SetStatus(id, status string) (Fact, error) {
	f, ok := s.Get(id)
	if !ok {
		return Fact{}, fmt.Errorf("facts: %s does not exist", id)
	}
	f.Status = status
	return s.Update(f)
}

// Forgets a fact
func (s *Store) Remove(id string) bool {
	s.mu.Lock()
	f, ok := s.facts[id]
	if !ok {
		s.mu.Unlock()
		return false
	}
	gone := *f
	s.drop(id)
	s.mu.Unlock()
	s.emit(Change{Op: OpRemove, Fact: gone})
	return true
}

func (s *Store) Get(id string) (Fact, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, ok := s.facts[id]
	if !ok {
		return Fact{}, false
	}
	return clone(*f), true
}

func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.order)
}

// Facts in insertion order, every one when the query is empty
func (s *Store) List(q Query) []Fact {
	s.mu.RLock()
	defer s.mu.RUnlock()
	text := strings.ToLower(strings.TrimSpace(q.Text))
	var out []Fact
	for _, id := range s.order {
		f := s.facts[id]
		if q.Kind != "" && f.Kind != strings.ToLower(q.Kind) {
			continue
		}
		if q.Status != "" && f.Status != q.Status {
			continue
		}
		if q.Agent != "" && f.Agent != q.Agent {
			continue
		}
		if f.Confidence < q.MinConfidence {
			continue
		}
		if q.Tag != "" && !hasTag(f, q.Tag) {
			continue
		}
		if text != "" && !matches(f, text) {
			continue
		}
		out = append(out, clone(*f))
	}
	return out
}

func hasTag(f *Fact, tag string) bool {
	tag = strings.ToLower(tag)
	for _, t := range f.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

func matches(f *Fact, text string) bool {
	if strings.Contains(strings.ToLower(f.Title), text) || strings.Contains(strings.ToLower(f.Body), text) {
		return true
	}
	for _, t := range f.Tags {
		if strings.Contains(t, text) {
			return true
		}
	}
	for _, e := range f.Evidence {
		if strings.Contains(strings.ToLower(e.Ref), text) || strings.Contains(strings.ToLower(e.Quote), text) {
			return true
		}
	}
	return false
}

// Kinds present in the store with how many facts each holds, sorted by kind
func (s *Store) Kinds() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]int{}
	for _, f := range s.facts {
		out[f.Kind]++
	}
	return out
}

// Tags present in the store with how many facts carry each
func (s *Store) Tags() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]int{}
	for _, f := range s.facts {
		for _, t := range f.Tags {
			out[t]++
		}
	}
	return out
}

// One node of the tree view
type Node struct {
	Fact     Fact
	Children []Node
}

// Facts arranged by parent, roots in insertion order and children under the fact they refine
func (s *Store) Tree() []Node {
	s.mu.RLock()
	defer s.mu.RUnlock()
	children := map[string][]string{}
	var roots []string
	for _, id := range s.order {
		f := s.facts[id]
		if f.Parent != "" {
			if _, ok := s.facts[f.Parent]; ok {
				children[f.Parent] = append(children[f.Parent], id)
				continue
			}
		}
		roots = append(roots, id)
	}
	var build func(id string) Node
	build = func(id string) Node {
		n := Node{Fact: clone(*s.facts[id])}
		for _, c := range children[id] {
			n.Children = append(n.Children, build(c))
		}
		return n
	}
	out := make([]Node, 0, len(roots))
	for _, id := range roots {
		out = append(out, build(id))
	}
	return out
}

// Facts grouped by kind, kinds sorted and facts in insertion order
func (s *Store) ByKind() [][]Fact {
	all := s.List(Query{})
	groups := map[string][]Fact{}
	for _, f := range all {
		groups[f.Kind] = append(groups[f.Kind], f)
	}
	kinds := make([]string, 0, len(groups))
	for k := range groups {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	out := make([][]Fact, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, groups[k])
	}
	return out
}

// Renders every fact as Markdown grouped by kind with evidence as a list under each
func (s *Store) Markdown() string {
	var sb strings.Builder
	sb.WriteString("# Findings\n")
	for _, group := range s.ByKind() {
		fmt.Fprintf(&sb, "\n## %s\n", strings.ToUpper(group[0].Kind[:1])+group[0].Kind[1:])
		for _, f := range group {
			sb.WriteString("\n")
			sb.WriteString(f.Markdown())
		}
	}
	return sb.String()
}

// Renders one fact as a Markdown entry
func (f Fact) Markdown() string {
	var sb strings.Builder
	mark := " "
	switch f.Status {
	case StatusResolved:
		mark = "x"
	case StatusRetracted:
		mark = "-"
	}
	fmt.Fprintf(&sb, "- [%s] **%s** (%s, %d%%", mark, f.Title, f.ID, int(f.Confidence*100+0.5))
	if f.Agent != "" {
		fmt.Fprintf(&sb, ", %s", f.Agent)
	}
	sb.WriteString(")")
	if len(f.Tags) > 0 {
		fmt.Fprintf(&sb, " `%s`", strings.Join(f.Tags, "` `"))
	}
	sb.WriteString("\n")
	if body := strings.TrimSpace(f.Body); body != "" {
		for _, line := range strings.Split(body, "\n") {
			sb.WriteString("  " + line + "\n")
		}
	}
	for _, e := range f.Evidence {
		ref := e.Ref
		if e.Line > 0 {
			ref = fmt.Sprintf("%s:%d", e.Ref, e.Line)
		}
		fmt.Fprintf(&sb, "  - %s %s", e.Kind, ref)
		if e.Quote != "" {
			fmt.Fprintf(&sb, " — %q", e.Quote)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// Serializes every fact as a JSON list
func (s *Store) JSON() ([]byte, error) {
	return json.MarshalIndent(s.List(Query{}), "", "  ")
}

func clone(f Fact) Fact {
	f.Tags = append([]string(nil), f.Tags...)
	f.Evidence = append([]Evidence(nil), f.Evidence...)
	return f
}

// Name the model calls to record a finding
const ToolName = "Record"

const schema = `{"type":"object","properties":{
"title":{"type":"string","description":"One line stating the finding"},
"body":{"type":"string","description":"Details, reasoning and what was checked"},
"kind":{"type":"string","description":"One of finding, risk, decision, question or note"},
"tags":{"type":"array","items":{"type":"string"},"description":"Short labels for filtering"},
"confidence":{"type":"number","minimum":0,"maximum":1,"description":"Belief from 0 to 1, use 1 only for verified facts"},
"evidence":{"type":"array","items":{"type":"object","properties":{
 "kind":{"type":"string","description":"file, url, command or tool"},
 "ref":{"type":"string","description":"Path, URL, command line or tool call id"},
 "line":{"type":"integer","description":"Line number inside a file"},
 "quote":{"type":"string","description":"Passage copied from the source"}},"required":["ref"],"additionalProperties":false}},
"id":{"type":"string","description":"Id of an existing fact to update instead of adding a new one"},
"status":{"type":"string","description":"open, resolved or retracted"},
"parent":{"type":"string","description":"Id of the fact this one refines"}},
"required":["title"],"additionalProperties":false}`

// Tool that lets an agent record, refine and resolve facts in the store of its run
type Tool struct {
	// Store of the calling run
	Resolve func(context.Context) *Store
}

func (t Tool) Definition() protocol.Tool {
	return protocol.Tool{
		Name:        ToolName,
		Description: fmt.Sprintf("Keeps a conclusion the user wants after the session, such as a bug found, a risk, a decision or an open question, beside the transcript. Routine command output and progress belong in prose instead. Kinds are %s. Cite evidence with file paths and line numbers. Give an existing id to refine or resolve a fact.", strings.Join(DefaultKinds, ", ")),
		Parameters:  json.RawMessage(schema),
	}
}

func (Tool) Annotations() tool.Annotations {
	return tool.Annotations{Idempotent: true}
}

func (t Tool) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	store := t.Resolve(ctx)
	var in struct {
		ID         string     `json:"id"`
		Title      string     `json:"title"`
		Body       string     `json:"body"`
		Kind       string     `json:"kind"`
		Tags       []string   `json:"tags"`
		Confidence *float64   `json:"confidence"`
		Evidence   []Evidence `json:"evidence"`
		Status     string     `json:"status"`
		Parent     string     `json:"parent"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	if in.Kind != "" && !allowed(DefaultKinds, in.Kind) {
		return tool.Errorf("kind %q is not one of %s", in.Kind, strings.Join(DefaultKinds, ", ")), nil
	}
	f := Fact{ID: in.ID, Agent: policy.AgentFrom(ctx), Kind: in.Kind, Title: in.Title, Body: in.Body, Tags: in.Tags, Evidence: in.Evidence, Status: in.Status, Parent: in.Parent, Confidence: 0.5}
	if in.Confidence != nil {
		f.Confidence = *in.Confidence
	}
	var out Fact
	var err error
	verb := "Recorded"
	if in.ID != "" {
		if old, ok := store.Get(in.ID); ok {
			if in.Confidence == nil {
				f.Confidence = old.Confidence
			}
			if in.Kind == "" {
				f.Kind = old.Kind
			}
			if in.Body == "" {
				f.Body = old.Body
			}
			if len(in.Tags) == 0 {
				f.Tags = old.Tags
			}
			if len(in.Evidence) == 0 {
				f.Evidence = old.Evidence
			}
			if in.Status == "" {
				f.Status = old.Status
			}
			if in.Parent == "" {
				f.Parent = old.Parent
			}
			out, err = store.Update(f)
			verb = "Updated"
		} else {
			out, err = store.Add(f)
		}
	} else {
		out, err = store.Add(f)
	}
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	return tool.Textf("%s %s %s: %s (%d facts on record)", verb, out.Kind, out.ID, out.Title, store.Len()).With(out), nil
}

func allowed(kinds []string, kind string) bool {
	for _, k := range kinds {
		if strings.EqualFold(k, kind) {
			return true
		}
	}
	return false
}

var _ tool.Tool = Tool{}

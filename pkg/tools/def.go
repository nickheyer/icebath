// Package tools registers model callable tools and runs them inside a session environment
package tools

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/state"
)

// Handler runs one tool call and returns the JSON string result shown to the model
type Handler func(ctx context.Context, call *Call) string

// Def is one registered tool with its schema, handler, and availability check
type Def struct {
	Name    string
	Toolset string
	// OpenAI function schema with name, description, and parameters
	Schema      map[string]any
	Handler     Handler
	Check       func(env *Env) bool
	RequiresEnv []string
	Description string
	Emoji       string
	// Zero means the global tool output limit applies
	MaxResultChars int
	// Merged over Schema at every definition read so limits in descriptions stay current
	DynamicSchema func(env *Env) map[string]any
	Timeout       time.Duration
	Override      bool
	Plugin        string
}

// Call is one tool invocation with its arguments and session environment
type Call struct {
	Name         string
	Args         map[string]any
	RawArgs      string // arguments JSON text as the model emitted it, the key order approval displays follow
	ToolCallID   string
	TaskID       string
	SessionID    string
	TurnID       string
	APIRequestID string
	UserTask     string
	Env          *Env
}

// AuxOptions tunes one auxiliary model completion
type AuxOptions struct {
	Task        string
	Model       string
	MaxTokens   int
	Temperature *float64
	JSONSchema  map[string]any
	Timeout     time.Duration
}

// Aux performs auxiliary model completions for vision, summaries, and compression
type Aux interface {
	Complete(ctx context.Context, messages []*state.Message, opts AuxOptions) (string, error)
}

// Question is one prompt the user answers inside a form
type Question struct {
	Header      string
	Text        string
	Choices     []string
	MultiSelect bool
}

// Answer is one user response in question order
type Answer struct {
	Status   string
	Text     string
	Selected []string
}

// FormResult reports the form outcome and the answers in question order
type FormResult struct {
	Outcome string
	Notice  string
	Answers []Answer
}

// Asker is the human in the loop for approvals and clarifying questions
type Asker interface {
	Ask(ctx context.Context, questions []Question) (FormResult, error)
}

// Event is one progress notice a running tool emits to the host surface
type Event struct {
	Kind   string
	Tool   string
	TaskID string
	Text   string
	Data   map[string]any
}

// Env is the per-session service surface every tool handler reads
type Env struct {
	Home     string
	Config   config.Tree
	Secrets  *config.Secrets
	State    *state.DB
	Cwd      string
	Platform string
	Enabled  []string
	Disabled []string
	Aux      Aux
	Ask      Asker
	Notify   func(Event)
	Log      *slog.Logger
	// Shared per-session values keyed by tool name
	Values *sync.Map
}

// Emit sends a progress event when the host registered a listener
func (e *Env) Emit(ev Event) {
	if e != nil && e.Notify != nil {
		e.Notify(ev)
	}
}

// JSON encodes a tool result as the string returned to the model
func JSON(v any) string {
	b, err := jsonEncodeWTF8(v)
	if err != nil {
		return ErrorResult(err)
	}
	return string(b)
}

// ErrorResult formats an error as the JSON object the model reads
func ErrorResult(err error) string {
	b, _ := jsonEncodeWTF8(map[string]any{"error": err.Error()})
	return string(b)
}

// Registry holds every tool definition by name
type Registry struct {
	mu    sync.RWMutex
	tools map[string]*Def
}

// Default is the process wide registry every tool file registers into at init
var Default = NewRegistry()

// NewRegistry returns an empty registry
func NewRegistry() *Registry {
	return &Registry{tools: map[string]*Def{}}
}

// Register adds a tool to the default registry
func Register(d Def) { Default.Register(d) }

// Register adds a tool, replacing a same toolset entry and panicking on a cross toolset shadow without override
func (r *Registry) Register(d Def) {
	if d.Name == "" || d.Handler == nil || d.Schema == nil {
		panic("tools: register requires name, schema, and handler")
	}
	if d.Description == "" {
		if s, ok := d.Schema["description"].(string); ok {
			d.Description = s
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if prev, ok := r.tools[d.Name]; ok && prev.Toolset != d.Toolset && !d.Override {
		panic("tools: " + d.Name + " from toolset " + d.Toolset + " would shadow toolset " + prev.Toolset)
	}
	copy := d
	r.tools[d.Name] = &copy
}

// Get returns the tool with this name or nil
func (r *Registry) Get(name string) *Def {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tools[name]
}

// Names lists registered tool names sorted
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.tools))
	for n := range r.tools {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Remove drops a tool and reports whether it existed
func (r *Registry) Remove(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.tools[name]
	delete(r.tools, name)
	return ok
}

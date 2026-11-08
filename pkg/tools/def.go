// Package tools registers model callable tools and runs them inside a session environment
package tools

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
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
	// Builds Schema on the first registry read when Schema is nil, deferring config reads past init
	SchemaBuilder func() map[string]any
	schemaOnce    *sync.Once
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
	// Profile terminal policy bound for this turn, nil resolves from Home
	TerminalScope *TerminalScope
	// Tool correlation ids of the call this copy was bound for
	observability *observabilityIDs
	// Explicit kanban selection of the schema build this copy was bound for
	kanbanSelection *bool
	// Synthesized from a nil env so availability checks keep bypassing the verdict cache
	unscopedChecks bool
}

// Emit sends a progress event when the host registered a listener
func (e *Env) Emit(ev Event) {
	if e != nil && e.Notify != nil {
		e.Notify(ev)
	}
}

// JSON encodes a tool result as the string returned to the model
func JSON(v any) string {
	b, err := pyjson.EncodeWTF8(v)
	if err != nil {
		return ErrorResult(err)
	}
	return string(b)
}

// ErrorResult formats an error as the JSON object the model reads
func ErrorResult(err error) string {
	b, _ := pyjson.EncodeWTF8(map[string]any{"error": err.Error()})
	return string(b)
}

// Registry holds every tool definition by name
type Registry struct {
	mu      sync.RWMutex
	tools   map[string]*Def
	builtin map[string]builtinEntry
}

// Default is the process wide registry every tool file registers into at init
var Default = NewRegistry()

// NewRegistry returns an empty registry
func NewRegistry() *Registry {
	return &Registry{tools: map[string]*Def{}}
}

// Register adds a builtin tool to the default registry recording the source module that registered it
func Register(d Def) {
	module := ""
	if _, file, _, ok := runtime.Caller(1); ok {
		module = filepath.Base(filepath.Dir(file)) + "." + strings.TrimSuffix(filepath.Base(file), ".go")
	}
	Default.registerBuiltin(d, module)
}

// Register adds a process global tool, panicking on a malformed schema and logging a refused shadow
func (r *Registry) Register(d Def) {
	if err := r.RegisterScoped("", d); err != nil && !errors.Is(err, ErrRegistrationRejected) {
		panic("tools: " + err.Error())
	}
}

// Get returns the tool with this name or nil
func (r *Registry) Get(name string) *Def {
	r.mu.RLock()
	d := r.globalLocked(name)
	r.mu.RUnlock()
	return d.resolveSchema()
}

// Runs a deferred schema builder once and fills the description, panicking on a malformed schema like Register
func (d *Def) resolveSchema() *Def {
	if d == nil || d.schemaOnce == nil {
		return d
	}
	d.schemaOnce.Do(func() {
		schema := d.SchemaBuilder()
		if err := validateRegistrationSchema(Def{Name: d.Name, Schema: schema}); err != nil {
			panic("tools: " + err.Error())
		}
		d.Schema = schema
		if s, ok := schema["description"].(string); ok && d.Description == "" {
			d.Description = s
		}
	})
	return d
}

// Names lists registered tool names sorted
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.tools))
	for n, d := range r.tools {
		if r.hiddenLocked(n, d) {
			continue
		}
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
	r.noteSlotDelete("", name)
	return ok
}

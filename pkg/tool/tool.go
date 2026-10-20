// Callable tools the model may invoke, with a registry that runs them safely
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"sync"

	"github.com/nickheyer/icebath/pkg/protocol"
)

// Something the model can call by name with JSON arguments
type Tool interface {
	Definition() protocol.Tool
	Run(ctx context.Context, args json.RawMessage) (Result, error)
}

// Behavior hints in the shape MCP defines, the scheduler reading ReadOnly and Idempotent and policies the rest
type Annotations struct {
	ReadOnly    bool
	Destructive bool
	Idempotent  bool
	OpenWorld   bool
}

// Tool that declares its annotations
type Annotated interface {
	Annotations() Annotations
}

// Reports the annotations of t, with every hint off for tools that declare none
func Describe(t Tool) Annotations {
	if a, ok := t.(Annotated); ok {
		return a.Annotations()
	}
	return Annotations{}
}

// Outcome of one run, before the registry attaches the call identity
type Result struct {
	Content []protocol.Part
	IsError bool
}

// Builds a successful text result
func Text(s string) Result {
	return Result{Content: []protocol.Part{protocol.Text{Text: s}}}
}

// Builds a successful formatted text result
func Textf(format string, args ...any) Result {
	return Text(fmt.Sprintf(format, args...))
}

// Builds a failed text result the model is expected to recover from
func Error(s string) Result {
	return Result{Content: []protocol.Part{protocol.Text{Text: s}}, IsError: true}
}

// Builds a failed formatted text result
func Errorf(format string, args ...any) Result {
	return Error(fmt.Sprintf(format, args...))
}

// Builds a successful result carrying one image
func Image(mediaType string, data []byte) Result {
	return Result{Content: []protocol.Part{protocol.Image{MediaType: mediaType, Data: data}}}
}

// Tool made from a closure
type Func struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	Hints       Annotations
	Fn          func(ctx context.Context, args json.RawMessage) (Result, error)
}

func (f Func) Definition() protocol.Tool {
	return protocol.Tool{Name: f.Name, Description: f.Description, Parameters: f.Parameters}
}

func (f Func) Annotations() Annotations { return f.Hints }

func (f Func) Run(ctx context.Context, args json.RawMessage) (Result, error) {
	return f.Fn(ctx, args)
}

type renamed struct {
	Tool
	name string
}

func (r renamed) Definition() protocol.Tool {
	def := r.Tool.Definition()
	def.Name = r.name
	return def
}

func (r renamed) Annotations() Annotations { return Describe(r.Tool) }

// Exposes t to the model under another name
func Rename(t Tool, name string) Tool {
	return renamed{Tool: t, name: name}
}

// Name pattern every supported protocol accepts
var namePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Reports whether name is acceptable to every supported protocol
func ValidName(name string) bool { return namePattern.MatchString(name) }

// Ordered set of tools keyed by name
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
	order []string
}

// Adds tools in order, rejecting bad names, missing schemas and duplicates
func (r *Registry) Add(tools ...Tool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tools == nil {
		r.tools = map[string]Tool{}
	}
	for _, t := range tools {
		def := t.Definition()
		if !namePattern.MatchString(def.Name) {
			return fmt.Errorf("tool: name %q does not match %s", def.Name, namePattern)
		}
		if !json.Valid(def.Parameters) {
			return fmt.Errorf("tool %s: parameters are not a JSON schema", def.Name)
		}
		if _, dup := r.tools[def.Name]; dup {
			return fmt.Errorf("tool %s: already registered", def.Name)
		}
		r.tools[def.Name] = t
		r.order = append(r.order, def.Name)
	}
	return nil
}

// Drops the named tool and reports whether it was registered
func (r *Registry) Remove(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[name]; !ok {
		return false
	}
	delete(r.tools, name)
	for i, n := range r.order {
		if n == name {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	return true
}

func (r *Registry) Lookup(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Lists registered names in registration order
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.order)
}

// Lists definitions in registration order for a protocol request
func (r *Registry) Definitions() []protocol.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	defs := make([]protocol.Tool, 0, len(r.order))
	for _, name := range r.order {
		defs = append(defs, r.tools[name].Definition())
	}
	return defs
}

// Lists the names of registered tools annotated read only, sorted
func (r *Registry) ReadOnly() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var names []string
	for name, t := range r.tools {
		if Describe(t).ReadOnly {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// Runs one call, turning unknown tools, errors and panics into error results
func (r *Registry) Run(ctx context.Context, call protocol.ToolCall) (result protocol.ToolResult) {
	result = protocol.ToolResult{CallID: call.ID, Name: call.Name}
	defer func() {
		if p := recover(); p != nil {
			result.Content = []protocol.Part{protocol.Text{Text: fmt.Sprintf("tool %s panicked: %v", call.Name, p)}}
			result.IsError = true
		}
	}()
	t, ok := r.Lookup(call.Name)
	if !ok {
		result.Content = []protocol.Part{protocol.Text{Text: "unknown tool " + call.Name}}
		result.IsError = true
		return result
	}
	out, err := t.Run(ctx, call.Args)
	if err != nil {
		result.Content = []protocol.Part{protocol.Text{Text: err.Error()}}
		result.IsError = true
		return result
	}
	result.Content, result.IsError = out.Content, out.IsError
	return result
}

// Runs read only calls concurrently and every other call alone in order, returning results in call order
func (r *Registry) RunAll(ctx context.Context, calls []protocol.ToolCall) []protocol.ToolResult {
	results := make([]protocol.ToolResult, len(calls))
	shared := r.repeats(calls)
	var wg sync.WaitGroup
	for i, call := range calls {
		if shared[i] >= 0 {
			continue
		}
		if t, ok := r.Lookup(call.Name); ok && Describe(t).ReadOnly {
			wg.Go(func() {
				results[i] = r.Run(ctx, call)
			})
			continue
		}
		wg.Wait()
		results[i] = r.Run(ctx, call)
	}
	wg.Wait()
	for i, first := range shared {
		if first >= 0 {
			results[i] = results[first]
			results[i].CallID = calls[i].ID
		}
	}
	return results
}

// Maps repeats of an idempotent call with identical arguments to the index of the first, minus one otherwise
func (r *Registry) repeats(calls []protocol.ToolCall) []int {
	shared := make([]int, len(calls))
	first := map[string]int{}
	for i, call := range calls {
		shared[i] = -1
		t, ok := r.Lookup(call.Name)
		if !ok || !Describe(t).Idempotent {
			continue
		}
		key := call.Name + "\x00" + string(call.Args)
		if j, seen := first[key]; seen {
			shared[i] = j
			continue
		}
		first[key] = i
	}
	return shared
}

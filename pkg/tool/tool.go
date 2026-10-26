// Callable tools the model may invoke, with a registry that runs them safely
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

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
	// Structured form of the outcome for callers that render rather than read, never sent to the model
	Data any
}

// Copy of the result carrying structured data
func (r Result) With(data any) Result {
	r.Data = data
	return r
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

// Reports whether name is acceptable to every supported protocol, letters, digits, underscores and dashes up to 64 bytes
func ValidName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

// Checks that a parameter schema is a JSON object describing an object
func ValidSchema(raw json.RawMessage) error {
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(raw, &schema); err != nil || schema == nil {
		return fmt.Errorf("parameters are not a JSON schema object")
	}
	if kind, ok := schema["type"]; ok {
		var t string
		if err := json.Unmarshal(kind, &t); err != nil || t != "object" {
			return fmt.Errorf("parameters schema type must be object")
		}
	}
	return nil
}

// Partial output or a progress mark a running tool reports while it works
type Progress struct {
	CallID string
	Tool   string
	// Name of the output stream the text belongs to, such as stdout or stderr
	Stream string
	Text   string
	Done   int
	Total  int
}

// Receives progress from running tools
type Reporter func(Progress)

type reporterKey struct{}
type callKey struct{}

// Carries a reporter so tools can stream output and progress to whoever runs them
func WithReporter(ctx context.Context, r Reporter) context.Context {
	return context.WithValue(ctx, reporterKey{}, r)
}

// Carries the call a tool is serving so progress reports name it
func WithCall(ctx context.Context, call protocol.ToolCall) context.Context {
	return context.WithValue(ctx, callKey{}, call)
}

// Call the context was opened for, if any
func CallFrom(ctx context.Context) (protocol.ToolCall, bool) {
	c, ok := ctx.Value(callKey{}).(protocol.ToolCall)
	return c, ok
}

// Delivers progress to the reporter in the context, stamping the call identity
func Report(ctx context.Context, p Progress) {
	r, ok := ctx.Value(reporterKey{}).(Reporter)
	if !ok || r == nil {
		return
	}
	if c, ok := CallFrom(ctx); ok {
		if p.CallID == "" {
			p.CallID = c.ID
		}
		if p.Tool == "" {
			p.Tool = c.Name
		}
	}
	r(p)
}

// Completed run of one call with its structured data and timing
type Outcome struct {
	protocol.ToolResult
	Data     any
	Started  time.Time
	Duration time.Duration
}

// Ordered set of tools keyed by name
type Registry struct {
	// Budget for one run, unlimited when zero
	Timeout  time.Duration
	mu       sync.RWMutex
	tools    map[string]Tool
	order    []string
	off      map[string]bool
	onChange []func()
}

// Adds tools in order, rejecting bad names, missing schemas and duplicates without registering any
func (r *Registry) Add(tools ...Tool) error {
	r.mu.Lock()
	if r.tools == nil {
		r.tools = map[string]Tool{}
	}
	seen := map[string]bool{}
	for _, t := range tools {
		def := t.Definition()
		if !ValidName(def.Name) {
			r.mu.Unlock()
			return fmt.Errorf("tool: name %q must be letters, digits, underscores and dashes up to 64 bytes", def.Name)
		}
		if err := ValidSchema(def.Parameters); err != nil {
			r.mu.Unlock()
			return fmt.Errorf("tool %s: %w", def.Name, err)
		}
		if _, dup := r.tools[def.Name]; dup || seen[def.Name] {
			r.mu.Unlock()
			return fmt.Errorf("tool %s: already registered", def.Name)
		}
		seen[def.Name] = true
	}
	for _, t := range tools {
		name := t.Definition().Name
		r.tools[name] = t
		r.order = append(r.order, name)
	}
	fns := append([]func(){}, r.onChange...)
	r.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
	return nil
}

// Drops the named tool and reports whether it was registered
func (r *Registry) Remove(name string) bool {
	r.mu.Lock()
	if _, ok := r.tools[name]; !ok {
		r.mu.Unlock()
		return false
	}
	delete(r.tools, name)
	for i, n := range r.order {
		if n == name {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	fns := append([]func(){}, r.onChange...)
	r.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
	return true
}

// Registers a callback run after every change to the set of tools
func (r *Registry) OnChange(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onChange = append(r.onChange, fn)
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

// Lists definitions of enabled tools in registration order for a protocol request
func (r *Registry) Definitions() []protocol.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	defs := make([]protocol.Tool, 0, len(r.order))
	for _, name := range r.order {
		if !r.off[name] {
			defs = append(defs, r.tools[name].Definition())
		}
	}
	return defs
}

// Turns a tool on or off by name, an off tool withheld from requests and refused when called
func (r *Registry) SetEnabled(name string, on bool) {
	r.mu.Lock()
	if r.off == nil {
		r.off = map[string]bool{}
	}
	if on {
		delete(r.off, name)
	} else {
		r.off[name] = true
	}
	fns := append([]func(){}, r.onChange...)
	r.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// Reports whether the tool is on, true for names never turned off
func (r *Registry) Enabled(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return !r.off[name]
}

// Names turned off, sorted, registered or not
func (r *Registry) Disabled() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.off))
	for name := range r.off {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
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

// Runs one call, turning unknown tools, errors, panics and cancellation into error results
func (r *Registry) Run(ctx context.Context, call protocol.ToolCall) (out Outcome) {
	out = Outcome{ToolResult: protocol.ToolResult{CallID: call.ID, Name: call.Name}, Started: time.Now()}
	defer func() {
		if p := recover(); p != nil {
			out.Content = []protocol.Part{protocol.Text{Text: fmt.Sprintf("tool %s panicked: %v", call.Name, p)}}
			out.IsError = true
		}
		out.Duration = time.Since(out.Started)
	}()
	if err := ctx.Err(); err != nil {
		out.Content = []protocol.Part{protocol.Text{Text: "cancelled before " + call.Name + " ran"}}
		out.IsError = true
		return out
	}
	t, ok := r.Lookup(call.Name)
	if !ok {
		out.Content = []protocol.Part{protocol.Text{Text: "unknown tool " + call.Name}}
		out.IsError = true
		return out
	}
	if !r.Enabled(call.Name) {
		out.Content = []protocol.Part{protocol.Text{Text: "tool " + call.Name + " is turned off"}}
		out.IsError = true
		return out
	}
	ctx = WithCall(ctx, call)
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	res, err := t.Run(ctx, call.Args)
	if err != nil {
		out.Content = []protocol.Part{protocol.Text{Text: err.Error()}}
		out.IsError = true
		return out
	}
	out.Content, out.IsError, out.Data = res.Content, res.IsError, res.Data
	return out
}

// Runs read only calls concurrently and every other call alone in order, reporting each outcome through done as soon as it is known
func (r *Registry) RunEach(ctx context.Context, calls []protocol.ToolCall, done func(i int, out Outcome)) []Outcome {
	results := make([]Outcome, len(calls))
	shared := r.repeats(calls)
	var mu sync.Mutex
	report := func(i int, out Outcome) {
		if done == nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		done(i, out)
	}
	var wg sync.WaitGroup
	for i, call := range calls {
		if shared[i] >= 0 {
			continue
		}
		if t, ok := r.Lookup(call.Name); ok && Describe(t).ReadOnly {
			wg.Go(func() {
				results[i] = r.Run(ctx, call)
				report(i, results[i])
			})
			continue
		}
		wg.Wait()
		results[i] = r.Run(ctx, call)
		report(i, results[i])
	}
	wg.Wait()
	for i, first := range shared {
		if first >= 0 {
			results[i] = results[first]
			results[i].CallID = calls[i].ID
			report(i, results[i])
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

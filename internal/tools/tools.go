package tools

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
)

type Handler func(ctx context.Context, call *Call) string

type Def struct {
	Name           string
	Toolset        string
	Schema         map[string]any
	Handler        Handler
	Check          func(env *Env) bool
	MaxResultChars int
}

type Call struct {
	Name       string
	Args       map[string]any
	RawArgs    string
	ToolCallID string
	SessionID  string
	Env        *Env
}

type Env struct {
	Cwd     string
	Config  *config.Config
	Approve func(ctx context.Context, action, reason string) error
}

type Registrant interface {
	Tools() []Def
}

type Registry struct {
	registrants []Registrant
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Tools() []Def {
	var defs []Def
	for _, registrant := range r.registrants {
		defs = append(defs, registrant.Tools()...)
	}
	return defs
}

func (r *Registry) Definitions(env *Env) []map[string]any {
	defs := r.available(env)
	slices.SortFunc(defs, func(a, b Def) int { return cmp.Compare(a.Name, b.Name) })
	definitions := make([]map[string]any, len(defs))
	for i, def := range defs {
		schema := maps.Clone(def.Schema)
		schema["name"] = def.Name
		definitions[i] = map[string]any{"type": "function", "function": schema}
	}
	return definitions
}

func (r *Registry) Dispatch(ctx context.Context, call *Call) string {
	result := r.dispatch(ctx, call)
	limit := call.Env.Config.GetInt(toolsconfig.OutputMaxBytes)
	for _, def := range r.Tools() {
		if def.Name == call.Name && def.MaxResultChars > 0 {
			limit = def.MaxResultChars
		}
	}
	if result = strings.ToValidUTF8(Bound(result, limit), "�"); result == "" {
		return "(no output)"
	}
	return result
}

func (r *Registry) dispatch(ctx context.Context, call *Call) string {
	for _, def := range r.available(call.Env) {
		if def.Name != call.Name {
			continue
		}
		if err := json.Unmarshal([]byte(cmp.Or(call.RawArgs, "{}")), &call.Args); err != nil {
			return ErrorResult(fmt.Errorf("arguments are not a JSON object: %w", err))
		}
		parameters, _ := def.Schema["parameters"].(map[string]any)
		required, _ := parameters["required"].([]string)
		for _, name := range required {
			if _, ok := call.Args[name]; !ok {
				return ErrorResult(fmt.Errorf("missing argument %s", name))
			}
		}
		return def.Handler(ctx, call)
	}
	return ErrorResult(fmt.Errorf("unknown tool %s", call.Name))
}

func (r *Registry) available(env *Env) []Def {
	var defs []Def
	for _, def := range r.Tools() {
		if def.Schema != nil && def.Handler != nil && (def.Check == nil || def.Check(env)) {
			defs = append(defs, def)
		}
	}
	return defs
}

type Tools struct {
	Registry
}

func New(cfg *config.Config) *Tools {
	toolsconfig.Register(cfg)
	return &Tools{}
}

func Bind[T any](run func(ctx context.Context, call *Call, args T) (string, error)) Handler {
	return func(ctx context.Context, call *Call) string {
		var args T
		if err := json.Unmarshal([]byte(cmp.Or(call.RawArgs, "{}")), &args); err != nil {
			return ErrorResult(err)
		}
		result, err := run(ctx, call, args)
		if err != nil {
			return ErrorResult(err)
		}
		return result
	}
}

func Schema(description string, required []string, properties map[string]any) map[string]any {
	return map[string]any{
		"description": description,
		"parameters":  map[string]any{"type": "object", "properties": properties, "required": required},
	}
}

func Property(kind, description string) map[string]any {
	return map[string]any{"type": kind, "description": description}
}

func ErrorResult(err error) string {
	result, _ := json.Marshal(map[string]string{"error": err.Error()})
	return string(result)
}

func Failed(result string) bool {
	var body map[string]json.RawMessage
	if json.Unmarshal([]byte(result), &body) != nil {
		return false
	}
	message, ok := body["error"]
	return ok && string(message) != "null"
}

func Bound(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	output := NewCapture(limit)
	output.Write([]byte(text))
	return output.String()
}

type Capture struct {
	half       int
	head, tail []byte
	total      int
}

func NewCapture(limit int) *Capture {
	return &Capture{half: max(limit-64, 0) / 2}
}

func (c *Capture) Write(data []byte) (int, error) {
	c.total += len(data)
	keep := min(c.half-len(c.head), len(data))
	c.head = append(c.head, data[:keep]...)
	c.tail = append(c.tail, data[keep:]...)
	c.tail = c.tail[max(len(c.tail)-c.half, 0):]
	return len(data), nil
}

func (c *Capture) String() string {
	if omitted := c.total - len(c.head) - len(c.tail); omitted > 0 {
		return fmt.Sprintf("%s\n[%d bytes omitted]\n%s", c.head, omitted, c.tail)
	}
	return string(c.head) + string(c.tail)
}

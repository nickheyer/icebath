package tool

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/model/wire"
)

type Dispatcher struct {
	r          *Registry
	hooks      Hooks
	editGate   EditGate
	connectors Connectors
	observers  []CallObserver
	metrics    SearchMetrics
	translate  func(key string, args map[string]string) string
}

type DispatchOptions struct {
	Hooks      Hooks
	EditGate   EditGate
	Connectors Connectors
	Observers  []CallObserver
	Metrics    SearchMetrics
	Translate  func(key string, args map[string]string) string
}

type DispatchFlags struct {
	SkipPreHook           bool
	SkipRequestMiddleware bool
	SkipExecMiddleware    bool
	SkipPostHook          bool
}

func NewDispatcher(r *Registry, o DispatchOptions) *Dispatcher {
	return &Dispatcher{
		r:          r,
		hooks:      o.Hooks,
		editGate:   o.EditGate,
		connectors: o.Connectors,
		observers:  slices.Clone(o.Observers),
		metrics:    o.Metrics,
		translate:  o.Translate,
	}
}

func (d *Dispatcher) Registry() *Registry { return d.r }

func legacyToolAlias(name string) string {
	switch name {
	case "todo":
		return "todo_list"
	case "cronjob":
		return "cronjob_manage"
	case "process":
		return "process_manage"
	case "tour":
		return "gui_tour"
	case "tip":
		return "show_tip"
	}
	return name
}

func agentLoopTool(name string) bool {
	switch name {
	case "todo_list", "memory", "session_search", "delegate_task":
		return true
	}
	return false
}

var toolErrorStrip = []*regexp.Regexp{
	regexp.MustCompile(`(?i)</?(?:tool_call|function_call|result|response|output|input|system|assistant|user)>`),
	regexp.MustCompile("(?m)^\\s*```(?:json|xml|html|markdown)?\\s*"),
	regexp.MustCompile("(?m)\\s*```\\s*$"),
	regexp.MustCompile(`(?s)<!\[CDATA\[.*?\]\]>`),
}

func sanitizeToolError(msg string) string {
	if msg == "" {
		return "[TOOL_ERROR] "
	}
	for _, re := range toolErrorStrip {
		msg = re.ReplaceAllString(msg, "")
	}
	if utf8.RuneCountInString(msg) > maxErrorChars {
		msg = runePrefix(msg, maxErrorChars-3) + "..."
	}
	return "[TOOL_ERROR] " + msg
}

func (d *Dispatcher) selection(c *Call) Selection {
	if c.Selection != nil {
		return *c.Selection
	}
	return Selection{Scope: c.Scope}
}

func (d *Dispatcher) coerce(name string, at Scope, args Args) Args {
	if len(args) == 0 {
		return args
	}
	e, ok := d.r.Lookup(name, at)
	if !ok {
		return args
	}
	params, err := parseNode(e.Tool.Parameters)
	if err != nil {
		return args
	}
	return coerceArgs(name, params, args)
}

func (d *Dispatcher) Dispatch(ctx context.Context, c *Call, args Args, f DispatchFlags) Result {
	call := *c
	args = d.coerce(call.Name, call.Scope, args)
	if args == nil {
		args = Args{}
	}
	call.Trace = slices.Clone(c.Trace)
	call.Name = legacyToolAlias(call.Name)
	start := time.Now()
	emit := func(r Result, ev Event) Result {
		if !f.SkipPostHook {
			ev.Call, ev.Args, ev.Result = &call, args, r
			d.PostToolCall(ctx, ev)
		}
		return r
	}
	if IsBridgeTool(call.Name) {
		res, next, nextArgs, entries := d.bridge(ctx, &call, args)
		switch next {
		case "":
			return emit(res, Event{Duration: time.Since(start)})
		case ConnectorBatch:
			return emit(d.connectorBatch(ctx, &call, entries), Event{Duration: time.Since(start)})
		}
		inner := call
		inner.Name = next
		inner.Bridged = true
		return d.Dispatch(ctx, &inner, nextArgs, f)
	}
	if call.Name == "manage_connections" || strings.HasPrefix(call.Name, ConnectorPrefix) {
		if !d.r.selected(d.selection(&call))["manage_connections"] {
			return emit(Failure("Connectors are not available in this session."), Event{})
		}
		if strings.HasPrefix(call.Name, ConnectorPrefix) {
			if _, _, ok := ParseConnectorName(call.Name); !ok {
				return emit(Failure("Malformed connector tool name; expected connectors__<connector>__{tool}."), Event{})
			}
		}
	}
	original := args.Clone()
	if !f.SkipRequestMiddleware && d.hooks != nil {
		payload, orig, trace, err := d.hooks.RequestMiddleware(ctx, &call, args)
		if err != nil {
			slog.Debug(fmt.Sprintf("tool_request middleware error: %v", err))
		} else {
			args, original, call.Trace = payload, orig, trace
		}
	}
	if agentLoopTool(call.Name) {
		if e, ok := d.r.Lookup(call.Name, call.Scope); !ok || e.Scope.Session == "" {
			return Failure(call.Name + " must be handled by the agent loop")
		}
	}
	if !f.SkipPreHook && d.hooks != nil && d.hooks.Has("pre_tool_call") {
		block, modified, err := d.hooks.PreToolCall(ctx, &call, args)
		if err != nil {
			slog.Debug(fmt.Sprintf("pre_tool_call hook error: %v", err))
		} else {
			if modified != nil {
				args = modified
			}
			if block != "" {
				return emit(Failure(block), Event{Status: "blocked", ErrorType: "plugin_block", ErrorText: block})
			}
		}
	}
	if d.editGate != nil {
		block, blocked, err := d.editGate.Check(ctx, &call, args)
		switch {
		case err != nil:
			slog.Debug(fmt.Sprintf("ACP edit approval guard error: %v", err))
			if call.Name == "write_file" || call.Name == "patch" {
				return emit(Failure("Edit approval denied: approval guard failed"), Event{Status: "blocked", ErrorType: "edit_approval_error"})
			}
		case blocked:
			return emit(block, Event{Status: "blocked", ErrorType: "edit_approval_denied"})
		}
	}
	for _, o := range d.observers {
		o.Called(&call)
	}
	start = time.Now()
	result := d.execute(ctx, &call, args, original, f.SkipExecMiddleware)
	duration := time.Since(start)
	emit(result, Event{Duration: duration})
	return d.TransformResult(ctx, Event{Call: &call, Args: args, Result: result, Duration: duration})
}

func (d *Dispatcher) execute(ctx context.Context, c *Call, args, original Args, skipMiddleware bool) Result {
	if c.Name == "execute_code" && c.SandboxTools == nil {
		defs, err := d.r.Definitions(ctx, d.selection(c), DefinitionOptions{SkipToolSearch: true})
		if err != nil {
			return Failure(sanitizeToolError("Tool execution failed: " + err.Error()))
		}
		c.SandboxTools = defs.Names
	}
	next := func(a Args) Result {
		if strings.HasPrefix(c.Name, ConnectorPrefix) {
			if d.connectors == nil {
				return failureOf(&UnfilledError{Name: "connectors"})
			}
			return d.connectors.Dispatch(ctx, c, a)
		}
		return d.invoke(ctx, c, a)
	}
	if skipMiddleware || d.hooks == nil {
		return next(args)
	}
	return d.hooks.ExecutionMiddleware(ctx, c, args, original, next)
}

func (d *Dispatcher) invoke(ctx context.Context, c *Call, args Args) Result {
	e, ok := d.r.Lookup(c.Name, c.Scope)
	if !ok {
		return Failure("Unknown tool: " + c.Name)
	}
	if e.Tool.Handler == nil {
		err := &UnfilledError{Name: "handler of tool " + c.Name}
		slog.Error(fmt.Sprintf("Tool %s dispatch error: %s", c.Name, err.Error()))
		return Failure(sanitizeToolError("Tool execution failed: " + err.Error()))
	}
	res, err := e.Tool.Handler(ctx, c, args)
	if err != nil {
		slog.Error(fmt.Sprintf("Tool %s dispatch error: %s", c.Name, boundError(err.Error())))
		return Failure(sanitizeToolError("Tool execution failed: " + err.Error()))
	}
	if res.Parts == nil {
		res.Text = boundJSONError(res.Text)
	}
	return res
}

func (d *Dispatcher) observerFields(name string, r Result) (string, string, string) {
	if r.Parts == nil {
		if v, err := wire.ParseValue([]byte(r.Text)); err == nil && v.IsObject() {
			if e := v.Get("error"); e.Truthy() {
				return "error", "tool_error", e.Display()
			}
		}
	}
	if failed, suffix := DetectFailure(name, r, d.translate); failed {
		return "error", "tool_error", strings.Trim(strings.TrimSpace(suffix), "[]")
	}
	return "ok", "", ""
}

func (d *Dispatcher) PostToolCall(ctx context.Context, ev Event) {
	if d.hooks == nil || !d.hooks.Has("post_tool_call") {
		return
	}
	if ev.Status == "" {
		ev.Status, ev.ErrorType, ev.ErrorText = d.observerFields(eventTool(ev), ev.Result)
	}
	d.hooks.PostToolCall(ctx, ev)
}

func eventTool(ev Event) string {
	if ev.Call == nil {
		return ""
	}
	return ev.Call.Name
}

func (d *Dispatcher) TransformResult(ctx context.Context, ev Event) Result {
	if d.hooks == nil || !d.hooks.Has("transform_tool_result") {
		return ev.Result
	}
	ev.Status, ev.ErrorType, ev.ErrorText = d.observerFields(eventTool(ev), ev.Result)
	text, ok := d.hooks.TransformResult(ctx, ev)
	if !ok {
		return ev.Result
	}
	r := ev.Result
	r.Text, r.Parts, r.Summary = text, nil, ""
	return r
}

func (d *Dispatcher) bridge(ctx context.Context, c *Call, args Args) (Result, string, Args, []Args) {
	defs, err := d.r.Definitions(ctx, d.selection(c), DefinitionOptions{SkipToolSearch: true})
	if err != nil {
		return failureOf(err), "", nil, nil
	}
	switch c.Name {
	case searchToolName:
		return d.dispatchSearch(ctx, c, args, defs.Tools), "", nil, nil
	case describeToolName:
		return d.dispatchDescribe(ctx, c, args, defs.Tools), "", nil, nil
	}
	cfg, err := d.r.searchConfig(ctx)
	if err != nil {
		return failureOf(err), "", nil, nil
	}
	session := toolNames(defs.Tools)
	u, msg := d.r.resolveUnderlying(nodeOf(args), c.Scope, session, cfg.deferTools)
	if msg != "" || u.name == "" {
		if msg == "" {
			msg = "tool_call could not be resolved"
		}
		return Failure(msg), "", nil, nil
	}
	if u.name == ConnectorBatch {
		if !connectionsInScope(defs.Tools) {
			return Failure("Connectors are not available in this session."), "", nil, nil
		}
		entries := make([]Args, len(u.entries))
		for i, e := range u.entries {
			entries[i] = Args{"name": e.name, "arguments": e.args.value()}
		}
		return Result{}, ConnectorBatch, nil, entries
	}
	_, deferrable := d.r.classifyTools(defs.Tools, c.Scope, cfg.deferTools)
	if !slices.ContainsFunc(deferrable, func(t wire.Tool) bool { return t.Name == u.name }) {
		return Failure(d.r.scopeBlock(u.name, c.Scope)), "", nil, nil
	}
	m, _ := u.args.value().(map[string]any)
	next := Args(m)
	if next == nil {
		next = Args{}
	}
	if probe := d.validateDeferred(u.name, c.Scope, next); probe != nil {
		return *probe, "", nil, nil
	}
	return Result{}, u.name, next, nil
}

type Unwrapped struct {
	Name       string
	Args       Args
	ScopeBlock string
	Probe      *Result
}

func (d *Dispatcher) Unwrap(ctx context.Context, c *Call, args Args) (Unwrapped, error) {
	out := Unwrapped{Name: c.Name, Args: args}
	if c.Name != callToolName {
		return out, nil
	}
	cfg, err := d.r.searchConfig(ctx)
	if err != nil {
		return out, err
	}
	u, msg := d.r.resolveUnderlying(nodeOf(args), c.Scope, nil, cfg.deferTools)
	if msg != "" || u.name == "" || u.name == ConnectorBatch {
		return out, nil
	}
	defs, err := d.r.Definitions(ctx, d.selection(c), DefinitionOptions{SkipToolSearch: true})
	if err != nil {
		return out, err
	}
	_, deferrable := d.r.classifyTools(defs.Tools, c.Scope, cfg.deferTools)
	if !slices.ContainsFunc(deferrable, func(t wire.Tool) bool { return t.Name == u.name }) {
		out.ScopeBlock = d.r.scopeBlock(u.name, c.Scope)
		return out, nil
	}
	m, _ := u.args.value().(map[string]any)
	next := Args(m)
	if next == nil {
		next = Args{}
	}
	if probe := d.validateDeferred(u.name, c.Scope, next); probe != nil {
		out.Probe = probe
		return out, nil
	}
	return Unwrapped{Name: u.name, Args: next}, nil
}

func (d *Dispatcher) connectorBatch(ctx context.Context, c *Call, entries []Args) Result {
	if d.connectors == nil {
		return failureOf(&UnfilledError{Name: "connectors"})
	}
	return d.connectors.Batch(ctx, c, entries, func(ctx context.Context, entry *Call, args Args) Result {
		return d.Dispatch(ctx, entry, args, DispatchFlags{})
	})
}

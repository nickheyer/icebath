package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Raised when a plugin overrides a built-in tool without the operator opt in
type PluginToolOverrideError struct{ Msg string }

func (e *PluginToolOverrideError) Error() string { return e.Msg }

// Matches PluginPermissionError the way the Python class subclasses PermissionError
func (e *PluginToolOverrideError) Is(target error) bool {
	_, ok := target.(*PluginPermissionError)
	return ok
}

const mcpResultCharCap = 65536

var (
	approvalTransportNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	skillNamespaceRe        = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
)

// Installed collaborator picked by get, or a missing service error naming it
func needService[T any](name string, get func(*Services) T) (T, error) {
	var zero T
	s := installedServices()
	if s == nil {
		return zero, &ServiceMissingError{Name: name}
	}
	v := get(s)
	if reflect.ValueOf(&v).Elem().IsZero() {
		return zero, &ServiceMissingError{Name: name}
	}
	return v, nil
}

// Whether a value is a non nil function the way Python callable() accepts it
func callable(v any) bool {
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Func && !rv.IsNil()
}

// Warns and reports true when a registrar received an object of the wrong host class
func (c *Context) wrongType(ok bool, article, label, base string) bool {
	if ok {
		return false
	}
	logWarn(fmt.Sprintf("Plugin '%s' tried to register %s %s that does not inherit from %s. Ignoring.", c.manifest.Name, article, label, base))
	return true
}

// Host object name of one kind, false when obj does not implement that kind
func (c *Context) hostObject(kind string, obj any) (string, bool, error) {
	fn, err := needService("HostObject", func(s *Services) func(string, any) (string, bool) { return s.HostObject })
	if err != nil {
		return "", false, err
	}
	name, ok := fn(kind, obj)
	return name, ok, nil
}

// Cached facade built once per context through a service constructor
func (c *Context) cachedFacade(slot *any, name string, build func(*Services) func(*Context) any) (any, error) {
	c.facadeMu.Lock()
	defer c.facadeMu.Unlock()
	if *slot != nil {
		return *slot, nil
	}
	fn, err := needService(name, build)
	if err != nil {
		return nil, err
	}
	*slot = fn(c)
	return *slot, nil
}

// Capability gated platform action facade re-checking gateway.platform_actions on every call
func (c *Context) PlatformActions() (any, error) {
	c.facadeMu.Lock()
	defer c.facadeMu.Unlock()
	if c.platformActions == nil {
		c.platformActions = &PlatformActions{pluginID: c.PluginID()}
	}
	return c.platformActions, nil
}

// Host owned completion facade on the active model, overrides gated per plugin
func (c *Context) LLM() *PluginLlm {
	c.facadeMu.Lock()
	defer c.facadeMu.Unlock()
	if c.llm == nil {
		c.llm = newPluginLlm(c.PluginID())
	}
	return c.llm
}

// Plugin safe subagent lifecycle service returning serializable handles and snapshots
func (c *Context) SubagentLifecycle() *SubagentLifecycleService {
	c.facadeMu.Lock()
	defer c.facadeMu.Unlock()
	if c.subagentLifecycle == nil {
		c.subagentLifecycle = NewSubagentLifecycleService(GetActiveSubagentParent)
	}
	return c.subagentLifecycle
}

// Delegation backend listing and selection service
func (c *Context) DelegationBackends() (any, error) {
	return c.cachedFacade(&c.delegationBackends, "DelegationBackends", func(s *Services) func(*Context) any { return s.DelegationBackends })
}

// Active profile name derived from the context home, default or custom outside named profiles
func (c *Context) ProfileName(ctx context.Context) string {
	h := home.Dir(ctx)
	if h == "" {
		return "default"
	}
	return profileName(h)
}

// Cron run executing in this context, nil outside a cron fire
func (c *Context) CurrentCronExecution(ctx context.Context) (any, error) {
	fn, err := needService("CurrentCronExecution", func(s *Services) func(context.Context) any { return s.CurrentCronExecution })
	if err != nil {
		return nil, err
	}
	return fn(ctx), nil
}

// Registers a cleanup callback run in reverse acquisition order on unload, errors logged never raised
func (c *Context) OnUnload(callback func() error) (*Registration, error) {
	if c.ignoredAfterAbandonedLoad("on_unload") {
		return nil, nil
	}
	if callback == nil {
		return nil, errors.New("on_unload callback must be callable")
	}
	_, name := callbackNames(callback)
	handle := c.track("on_unload", name, callback, false)
	logDebug(fmt.Sprintf("Plugin %s registered on_unload callback", c.manifest.Name))
	return handle, nil
}

// Supervised background task of a plugin cancelled by unload or force reload
type PluginTask struct {
	Name   string
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

// Closed once the task function returned
func (t *PluginTask) Done() <-chan struct{} { return t.done }

// Error the task function returned, valid after Done closed
func (t *PluginTask) Err() error { return t.err }

// Requests cancellation of the task context
func (t *PluginTask) Cancel() { t.cancel() }

// Starts a supervised task on the running loop, unload cancelling its context
func (c *Context) SpawnTask(ctx context.Context, fn func(context.Context) error, name string) (*PluginTask, error) {
	if fn == nil {
		return nil, errors.New("spawn_task expects a coroutine")
	}
	if !runningLoop(ctx) {
		return nil, errors.New("no running event loop")
	}
	if name == "" {
		name = "plugin:" + c.PluginID() + ":task"
	}
	taskCtx, cancel := context.WithCancel(ctx)
	task := &PluginTask{Name: name, cancel: cancel, done: make(chan struct{})}
	handle := c.track("background_task", name, func() error {
		select {
		case <-task.done:
		default:
			cancel()
		}
		return nil
	}, false)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				task.err = fmt.Errorf("%v", p)
			}
			close(task.done)
			cancel()
			_ = handle.Dispose()
		}()
		task.err = fn(taskCtx)
	}()
	logDebug(fmt.Sprintf("Plugin %s spawned supervised task: %s", c.manifest.Name, name))
	return task, nil
}

// Validated approval transport name, empty with the refusal when invalid
func cleanApprovalTransportName(name string, present tools.ApprovalPresentFunc) (string, error) {
	clean := strings.ToLower(strings.TrimSpace(name))
	if clean == "builtin" {
		return "", errors.New("approval transport name 'builtin' is reserved")
	}
	if !approvalTransportNameRe.MatchString(clean) {
		return "", errors.New("approval transport name must match [a-z0-9][a-z0-9_-]{0,63}")
	}
	if present == nil {
		return "", errors.New("approval transport present_fn must be callable")
	}
	return clean, nil
}

// Resolved absolute home of the context the way Path.resolve renders it
func resolvedHome(ctx context.Context) string {
	h := home.Dir(ctx)
	if abs, err := filepath.Abs(h); err == nil {
		h = abs
	}
	if real, err := filepath.EvalSymlinks(h); err == nil {
		h = real
	}
	return h
}

// Registers a human approval transport inactive until security.approval.transport selects it
func (c *Context) RegisterApprovalTransport(ctx context.Context, name string, present tools.ApprovalPresentFunc) error {
	if c.ignoredAfterAbandonedLoad("register_approval_transport") {
		return nil
	}
	clean, err := cleanApprovalTransportName(name, present)
	if err != nil {
		return err
	}
	m := c.manager
	m.mu.RLock()
	existing := m.approvalTransports[clean]
	m.mu.RUnlock()
	if existing != nil {
		return fmt.Errorf("approval transport %s is already registered by %s", config.PyRepr(clean), config.PyRepr(existing.PluginID))
	}
	entry := &tools.RegisteredApprovalTransport{Name: clean, Present: present, PluginID: c.PluginID(), ProfileHome: resolvedHome(ctx)}
	logInfo(fmt.Sprintf("Plugin %s registered approval transport: %s", c.PluginID(), clean))
	var none *tools.RegisteredApprovalTransport
	trackMappingEntry(c, "approval_transport", clean, m.approvalTransports, entry, &none)
	return nil
}

// Registers a tool in the profile scoped registry and leases it for unload
func (c *Context) RegisterTool(ctx context.Context, spec ToolSpec) (*Registration, error) {
	if c.ignoredAfterAbandonedLoad("register_tool") {
		return nil, nil
	}
	var handle *Registration
	var err error
	serializedReplacement(func() { handle, err = c.registerTool(ctx, spec) })
	return handle, err
}

// Arguments of one plugin tool registration
type ToolSpec struct {
	Name        string
	Toolset     string
	Schema      map[string]any
	Handler     tools.Handler
	Check       func(env *tools.Env) bool
	RequiresEnv []string
	Description string
	Emoji       string
	Override    bool
}

// Snapshot, register and lease body of RegisterTool inside the replacement transaction
func (c *Context) registerTool(ctx context.Context, spec ToolSpec) (*Registration, error) {
	name := spec.Name
	if spec.Override && !c.toolOverrideAllowed(name) {
		return nil, &PluginToolOverrideError{Msg: fmt.Sprintf("Plugin %s cannot override built-in tool %s. Set plugins.entries.%s.allow_tool_override: true in config.yaml to allow this plugin to replace built-in tools.",
			config.PyRepr(c.manifest.Name), config.PyRepr(name), c.PluginID())}
	}
	m := c.manager
	scope := m.home
	previous := tools.Default.SnapshotRegistration(scope, name)
	if previous == nil && !spec.Override && tools.Default.Lookup(ctx, scope, name) != nil {
		logWarn(fmt.Sprintf("Plugin %s tried to shadow global tool %s without override=True", c.manifest.Name, name))
		return nil, nil
	}
	err := tools.Default.RegisterScoped(scope, tools.Def{
		Name: name, Toolset: spec.Toolset, Schema: spec.Schema, Handler: spec.Handler, Check: spec.Check,
		RequiresEnv: spec.RequiresEnv, Description: spec.Description, Emoji: spec.Emoji,
		Override: spec.Override, Plugin: c.policyModule(),
	})
	if errors.Is(err, tools.ErrOverrideDenied) {
		return nil, &PluginToolOverrideError{Msg: err.Error()}
	}
	if err != nil {
		logWarn(err.Error())
	}
	registered := tools.Default.SnapshotRegistration(scope, name)
	var handle *Registration
	if registered != nil && registered != previous && sameRegistration(registered.Handler, spec.Handler) {
		m.mu.Lock()
		m.pluginToolNames[name] = true
		m.mu.Unlock()
		var prev any
		if previous != nil {
			prev = previous
		}
		handle = m.trackScopedRegistration(c.manifest, "tool", name, func(name string, current, replacement any, scope string) error {
			r, _ := replacement.(*tools.Def)
			tools.Default.RestoreRegistration(name, current.(*tools.Def), r, scope)
			return nil
		}, registered, prev, func() error {
			m.removeToolNameIfUnowned(name)
			return nil
		})
	}
	suffix := ""
	if spec.Override {
		suffix = " (override)"
	}
	logDebug(fmt.Sprintf("Plugin %s registered tool: %s%s", c.manifest.Name, name, suffix))
	return handle, nil
}

// Calls a tool on an allowlisted MCP server through the native client and wraps the result in an envelope
func (c *Context) CallMCP(ctx context.Context, server, tool string, arguments map[string]any, timeout any) (map[string]any, error) {
	allowed := false
	for _, s := range mcpAllowlist(home.Dir(ctx), c.PluginID()) {
		allowed = allowed || s == server
	}
	if !allowed {
		return nil, &PluginPermissionError{Msg: fmt.Sprintf("Plugin %s is not allowed to call MCP server %s. Add it to plugins.entries.%s.mcp_allowlist in config.yaml to grant access (default is no MCP access).",
			config.PyRepr(c.manifest.Name), config.PyRepr(server), c.PluginID())}
	}
	seconds := clampMCPTimeout(timeout)
	call, err := needService("CallMCPTool", func(s *Services) func(context.Context, string, string, float64, map[string]any) (string, error) {
		return s.CallMCPTool
	})
	if err != nil {
		return nil, err
	}
	args := map[string]any{}
	for k, v := range arguments {
		args[k] = v
	}
	raw, err := call(ctx, server, tool, seconds, args)
	if err != nil {
		return nil, err
	}
	logDebug(fmt.Sprintf("Plugin %s called MCP %s/%s (timeout=%ss, %d chars returned)", c.manifest.Name, server, tool, config.PyStr(seconds), utf8.RuneCountInString(raw)))
	return mcpEnvelope(raw), nil
}

// MCP call timeout as a float clamped to one through six hundred seconds, thirty when unparseable
func clampMCPTimeout(timeout any) float64 {
	seconds := 30.0
	switch v := timeout.(type) {
	case nil:
	case bool:
		seconds = 0
		if v {
			seconds = 1
		}
	case int:
		seconds = float64(v)
	case int64:
		seconds = float64(v)
	case float64:
		seconds = v
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			seconds = f
		}
	}
	if math.IsNaN(seconds) {
		return 1
	}
	return math.Max(1, math.Min(seconds, 600))
}

// Stable ok, error or result envelope of an MCP handler result string, truncated past the cap
func mcpEnvelope(raw string) map[string]any {
	truncated := utf8.RuneCountInString(raw) > mcpResultCharCap
	if truncated {
		raw = string([]rune(raw)[:mcpResultCharCap]) + "… [truncated]"
	}
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		parsed = nil
	}
	envelope := map[string]any{}
	obj, isObj := parsed.(map[string]any)
	if errVal, has := obj["error"]; isObj && has {
		envelope["ok"], envelope["error"] = false, errVal
	} else if result, has := obj["result"]; isObj && has {
		envelope["ok"], envelope["result"] = true, result
		if sc, ok := obj["structuredContent"]; ok {
			envelope["structuredContent"] = sc
		}
	} else if parsed != nil {
		envelope["ok"], envelope["result"] = true, parsed
	} else {
		envelope["ok"], envelope["result"] = true, raw
	}
	if truncated {
		envelope["truncated"] = true
	}
	return envelope
}

// Operator granted MCP server allowlist of a plugin, empty when missing or unreadable
func mcpAllowlist(h, pluginID string) []string {
	cfg, err := loadConfig(h)
	if err != nil {
		return nil
	}
	list, ok := pluginSettingsEntry(cfg, pluginID)["mcp_allowlist"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		out = append(out, config.PyStr(item))
	}
	return out
}

// Whether this plugin may override built-in tools under its own profile consent, failing closed
func (c *Context) toolOverrideAllowed(string) bool {
	if c.manifest.Source == "bundled" {
		return true
	}
	cfg, err := loadConfig(c.manager.home)
	if err != nil {
		return false
	}
	return CapabilityGranted(cfg, c.PluginID(), "tools.override")
}

// Whether a capability is live for this plugin under the context profile, bundled plugins trusted for overrides
func (c *Context) HasCapability(ctx context.Context, capability string) bool {
	if c.manifest.Source == "bundled" && capability == "tools.override" {
		return true
	}
	return CapabilityGranted(loadConfigOrEmpty(home.Dir(ctx)), c.PluginID(), capability)
}

const sideQuestionCapability = "llm.side_question"

// Asks the session agent about the turn that just ended, nil without the side question capability
func (c *Context) SideQuestion(ctx context.Context, token, prompt, timeout any) (*string, error) {
	cfg, err := loadConfig(c.manager.home)
	if err != nil || !CapabilityGranted(cfg, c.PluginID(), sideQuestionCapability) {
		if !c.sideQuestionDenialLogged.Swap(true) {
			logInfo(fmt.Sprintf("Plugin %s called ctx.side_question without the %s capability; returning None", c.PluginID(), sideQuestionCapability))
		}
		return nil, nil
	}
	redeem, err := needService("RedeemSideQuestion", func(s *Services) func(context.Context, any, any, string, string, any) (*string, error) {
		return s.RedeemSideQuestion
	})
	if err != nil {
		return nil, err
	}
	return redeem(ctx, token, prompt, c.PluginID(), c.manager.home, timeout)
}

// Profile config reader of the injection gate, swapped by tests for a raising loader
var injectionConfigLoader = loadConfig

// Whether this plugin may trigger gateway session turns under its own profile config
func (c *Context) gatewayInjectionAllowed() bool {
	cfg, err := injectionConfigLoader(c.manager.home)
	if err != nil {
		return false
	}
	allowed, ok := pluginSettingsEntry(cfg, c.PluginID())["allow_gateway_injection"].(bool)
	return ok && allowed
}

// Queues content for a session key or an origin chat of this profile, true when a host accepted it
func (c *Context) InjectMessage(ctx context.Context, content, role, sessionKey string, origin any) bool {
	m := c.manager
	msg := content
	if role != "user" {
		msg = "[" + role + "] " + content
	}
	m.mu.RLock()
	cli := m.cliRef
	m.mu.RUnlock()
	if cli != nil && origin == nil {
		if cli.AgentRunning() {
			cli.QueueInterrupt(msg)
		} else {
			cli.QueuePending(msg)
		}
		return true
	}
	if sessionKey != "" && origin != nil {
		logWarn("inject_message: pass session_key or origin, not both")
		return false
	}
	keyless := sessionKey == "" && origin == nil
	if keyless && !m.HasGatewayMessageInjector() {
		logWarn("inject_message: gateway mode requires an existing session_key or an origin")
		return false
	}
	originMap, isMap := origin.(map[string]any)
	if origin != nil && !isMap {
		logWarn("inject_message: origin must be a mapping in the SessionSource.to_dict() shape")
		return false
	}
	if !c.gatewayInjectionAllowed() {
		logWarn(fmt.Sprintf("inject_message: gateway injection denied for plugin %s; set plugins.entries.%s.allow_gateway_injection: true to allow it", c.PluginID(), c.PluginID()))
		return false
	}
	if sessionKey != "" && m.HasTUIMessageInjector() {
		var accepted bool
		if err := callGuarded(func() error {
			accepted = m.InjectTUIMessage(map[string]any{"session_key": sessionKey, "content": msg, "plugin_id": c.PluginID()})
			return nil
		}); err != nil {
			logWarn(fmt.Sprintf("inject_message: TUI scheduling failed for plugin %s", c.PluginID()), "error", err)
			return false
		}
		if accepted {
			return true
		}
	}
	if !m.HasGatewayMessageInjector() {
		logWarn("inject_message: no live gateway is available")
		return false
	}
	target := map[string]any{"plugin_home": m.home, "content": msg, "plugin_id": c.PluginID()}
	switch {
	case keyless:
		target["keyless"] = true
		target["calling_session_key"] = ""
		if s := installedServices(); s != nil && s.BoundSessionKey != nil {
			target["calling_session_key"] = s.BoundSessionKey(ctx)
		}
	case origin == nil:
		target["session_key"] = sessionKey
	default:
		copied := map[string]any{}
		for k, v := range originMap {
			copied[k] = v
		}
		target["origin"] = copied
	}
	var accepted bool
	if err := callGuarded(func() error {
		accepted = m.InjectGatewayMessage(target)
		return nil
	}); err != nil {
		logWarn(fmt.Sprintf("inject_message: gateway scheduling failed for plugin %s", c.PluginID()), "error", err)
		return false
	}
	return accepted
}

// Recorded working directory of the calling session, never the gateway process cwd
func (c *Context) SessionCwd(ctx context.Context) (string, error) {
	return contextCwd(ctx, c.manager)
}

// Client surface or messaging platform of the calling session
func (c *Context) SessionSurface(ctx context.Context) (string, error) {
	return contextSurface(ctx, c.manager)
}

// Posts text into the session bound to this call and returns an editable handle that never raises
func (c *Context) PostMessage(ctx context.Context, text string) *PluginMessage {
	return postMessage(ctx, c, text)
}

// Post for async code already on the gateway loop, its edits made with AEdit
func (c *Context) APostMessage(ctx context.Context, text string) (*PluginMessage, error) {
	return apostMessage(ctx, c, text)
}

// Asks the slash command user to pick one choice, false when nobody can answer
func (c *Context) AskChoice(ctx context.Context, question string, choices []any) (string, bool, error) {
	return askChoice(ctx, question, choices)
}

// Plugin CLI subcommand registration with its parser setup and handler
type CLICommand struct {
	Name        string
	Help        string
	Description string
	Setup       func(parser any)
	Handler     func(args any) error
	Plugin      string
	PluginKey   string
}

// Registers a CLI subcommand whose setup receives the subparser and whose handler runs it
func (c *Context) RegisterCLICommand(name, help string, setup func(parser any), handler func(args any) error, description string) *Registration {
	if c.ignoredAfterAbandonedLoad("register_cli_command") {
		return nil
	}
	entry := &CLICommand{Name: name, Help: help, Description: description, Setup: setup, Handler: handler, Plugin: c.manifest.Name, PluginKey: c.PluginID()}
	var handle *Registration
	serializedReplacement(func() {
		handle = registerEntry(c, "cli_command", name, c.manager.cliCommands, entry, nil,
			fmt.Sprintf("Plugin %s registered CLI command: %s", c.manifest.Name, name))
	})
	return handle
}

// Registers an in session slash command, refusing empty names and built-in conflicts
func (c *Context) RegisterCommand(name string, handler PluginCommandHandler, description, argsHint, argumentMode string) *Registration {
	if c.ignoredAfterAbandonedLoad("register_command") {
		return nil
	}
	var handle *Registration
	serializedReplacement(func() { handle = c.registerCommand(name, handler, description, argsHint, argumentMode) })
	return handle
}

// Validation and lease body of RegisterCommand inside the replacement transaction
func (c *Context) registerCommand(name string, handler PluginCommandHandler, description, argsHint, argumentMode string) *Registration {
	clean := strings.ReplaceAll(strings.TrimLeft(pyStrip(strings.ToLower(name)), "/"), " ", "-")
	if clean == "" {
		logWarn(fmt.Sprintf("Plugin '%s' tried to register a command with an empty name.", c.manifest.Name))
		return nil
	}
	if s := installedServices(); s != nil && s.ResolveBuiltinCommand != nil {
		conflict := false
		if callGuarded(func() error { conflict = s.ResolveBuiltinCommand(clean); return nil }) == nil && conflict {
			logWarn(fmt.Sprintf("Plugin '%s' tried to register command '/%s' which conflicts with a built-in command. Skipping.", c.manifest.Name, clean))
			return nil
		}
	}
	hint := pyStrip(argsHint)
	if description == "" {
		description = "Plugin command"
	}
	mode := ""
	switch {
	case argumentMode == "options" || argumentMode == "text" || argumentMode == "mixed":
		mode = argumentMode
	case hint != "":
		mode = "text"
	}
	entry := &PluginCommand{Handler: handler, Description: description, Plugin: c.manifest.Name, PluginKey: c.PluginID(), ArgsHint: hint, ArgumentMode: mode}
	return registerEntry(c, "command", clean, c.manager.pluginCommands, entry, nil,
		fmt.Sprintf("Plugin %s registered command: /%s", c.manifest.Name, clean))
}

// Python str.strip over Unicode whitespace
func pyStrip(s string) string { return strings.TrimFunc(s, unicode.IsSpace) }

// In session slash command a plugin registered
type PluginCommand struct {
	Handler      PluginCommandHandler
	Description  string
	Plugin       string
	PluginKey    string
	ArgsHint     string
	ArgumentMode string
}

// Slash command handler receiving the raw argument text
type PluginCommandHandler func(ctx context.Context, rawArgs string) (any, error)

// Dispatches a tool through the registry with the CLI agent as parent when one is attached
func (c *Context) DispatchTool(ctx context.Context, toolName string, args map[string]any, kwargs map[string]any) (string, error) {
	fn, err := needService("DispatchTool", func(s *Services) func(context.Context, string, string, map[string]any, map[string]any) (string, error) {
		return s.DispatchTool
	})
	if err != nil {
		return "", err
	}
	forwarded := map[string]any{}
	for k, v := range kwargs {
		forwarded[k] = v
	}
	if _, ok := forwarded["parent_agent"]; !ok {
		c.manager.mu.RLock()
		cli := c.manager.cliRef
		c.manager.mu.RUnlock()
		if cli != nil {
			if agent := cli.Agent(); agent != nil {
				forwarded["parent_agent"] = agent
			}
		}
	}
	return fn(ctx, c.manager.home, toolName, args, forwarded)
}

// Registers the single context engine replacing the built-in compressor, a second one rejected
func (c *Context) RegisterContextEngine(engine any) (*Registration, error) {
	if c.ignoredAfterAbandonedLoad("register_context_engine") {
		return nil, nil
	}
	var handle *Registration
	var err error
	serializedReplacement(func() { handle, err = c.registerContextEngine(engine) })
	return handle, err
}

// Type check, store and lease body of RegisterContextEngine inside the replacement transaction
func (c *Context) registerContextEngine(engine any) (*Registration, error) {
	m := c.manager
	m.mu.RLock()
	occupied := m.contextEngine != nil
	m.mu.RUnlock()
	if occupied {
		logWarn(fmt.Sprintf("Plugin '%s' tried to register a context engine, but one is already registered. Only one context engine plugin is allowed.", c.manifest.Name))
		return nil, nil
	}
	name, ok, err := c.hostObject("context_engine", engine)
	if err != nil {
		return nil, err
	}
	if c.wrongType(ok, "a", "context engine", "ContextEngine") {
		return nil, nil
	}
	m.mu.Lock()
	previous := m.contextEngine
	m.contextEngine = engine
	m.mu.Unlock()
	handle := c.trackReplacement("context_engine", name, [3]any{"manager_value", m, "_context_engine"}, engine, previous, func(replacement any) error {
		m.mu.Lock()
		if sameRegistration(m.contextEngine, engine) {
			m.contextEngine = replacement
		}
		m.mu.Unlock()
		return nil
	})
	logInfo(fmt.Sprintf("Plugin '%s' registered context engine: %s", c.manifest.Name, name))
	return handle, nil
}

// Registers an at prefix context reference provider, built-in prefixes refused with a warning
func (c *Context) RegisterContextReference(provider any) error {
	if c.ignoredAfterAbandonedLoad("register_context_reference") {
		return nil
	}
	prefix, ok, err := c.hostObject("context_reference", provider)
	if err != nil {
		return err
	}
	if c.wrongType(ok, "a", "context reference provider", "ContextReferenceProvider") {
		return nil
	}
	register, err := needService("RegisterContextReference", func(s *Services) func(any) error { return s.RegisterContextReference })
	if err != nil {
		return err
	}
	if err := register(provider); err != nil {
		logWarn(fmt.Sprintf("Plugin '%s' context reference registration failed: %v", c.manifest.Name, err))
		return nil
	}
	logInfo(fmt.Sprintf("Plugin '%s' registered context reference: @%s:", c.manifest.Name, prefix))
	return nil
}

// Records an inert memory provider whose activation the memory plugin loader owns
func (c *Context) RegisterMemoryProvider(provider any) error {
	if c.ignoredAfterAbandonedLoad("register_memory_provider") {
		return nil
	}
	name, ok, err := c.hostObject("memory_provider", provider)
	if err != nil {
		return err
	}
	if c.wrongType(ok, "a", "memory provider", "MemoryProvider") {
		return nil
	}
	c.facadeMu.Lock()
	c.memoryProvider = provider
	c.facadeMu.Unlock()
	if name == "" {
		name = "?"
	}
	logDebug(fmt.Sprintf("Plugin '%s' registered memory provider: %s", c.manifest.Name, name))
	return nil
}

// Memory provider this plugin recorded, nil when none
func (c *Context) MemoryProvider() any {
	c.facadeMu.Lock()
	defer c.facadeMu.Unlock()
	return c.memoryProvider
}

// Registers a process wide dashboard auth provider from the launch scope only, kept out of teardown
func (c *Context) RegisterDashboardAuthProvider(provider any) (*Registration, error) {
	if c.ignoredAfterAbandonedLoad("register_dashboard_auth_provider") {
		return nil, nil
	}
	var handle *Registration
	var err error
	serializedReplacement(func() { handle, err = c.registerDashboardAuthProvider(provider) })
	return handle, err
}

// Scope check, upsert and persistent lease body of RegisterDashboardAuthProvider
func (c *Context) registerDashboardAuthProvider(provider any) (*Registration, error) {
	name, ok, err := c.hostObject("dashboard_auth_provider", provider)
	if err != nil {
		return nil, err
	}
	if c.wrongType(ok, "a", "dashboard-auth provider", "DashboardAuthProvider") {
		return nil, nil
	}
	launchScope := home.Key(home.Process())
	if c.manager.home != launchScope {
		logWarn(fmt.Sprintf("Plugin '%s' tried to register dashboard-auth provider %s from profile scope %s; ignoring it because dashboard auth is owned by launch scope %s.",
			c.manifest.Name, config.PyRepr(name), c.manager.home, launchScope))
		return nil, nil
	}
	registry, err := needService("DashboardAuth", func(s *Services) DashboardAuthRegistry { return s.DashboardAuth })
	if err != nil {
		return nil, err
	}
	display, err := registry.RegisterGlobal(provider)
	if err != nil {
		logWarn(fmt.Sprintf("Plugin '%s' failed to register dashboard-auth provider %s: %v", c.manifest.Name, config.PyRepr(name), err))
		return nil, nil
	}
	handle := c.track("dashboard_auth_provider", name, func() error {
		registry.UnregisterGlobal(name, provider)
		return nil
	}, true)
	logInfo(fmt.Sprintf("Plugin '%s' registered dashboard-auth provider: %s (%s)", c.manifest.Name, name, display))
	return handle, nil
}

// Arguments of one gateway platform adapter registration
type PlatformSpec struct {
	Name           string
	Label          string
	AdapterFactory any
	Check          func() bool
	ValidateConfig any
	RequiredEnv    []string
	InstallHint    string
	Extra          map[string]any
}

// Registers a gateway platform adapter in the profile scoped platform registry and leases it
func (c *Context) RegisterPlatform(spec PlatformSpec) (*Registration, error) {
	if c.ignoredAfterAbandonedLoad("register_platform") {
		return nil, nil
	}
	var handle *Registration
	var err error
	serializedReplacement(func() { handle, err = c.registerPlatform(spec) })
	return handle, err
}

// Entry build, register and lease body of RegisterPlatform inside the replacement transaction
func (c *Context) registerPlatform(spec PlatformSpec) (*Registration, error) {
	registry, err := needService("Platforms", func(s *Services) PlatformRegistry { return s.Platforms })
	if err != nil {
		return nil, err
	}
	trusted, _ := spec.Extra["trusted_inbound"].(bool)
	if trusted && c.manifest.Source != "bundled" && registry.CoreShipsPlatform(spec.Name) {
		return nil, c.refuse(fmt.Sprintf("core platform '%s' with trusted_inbound (it would waive allowlists and pairing)", spec.Name))
	}
	extra := map[string]any{}
	for k, v := range spec.Extra {
		extra[k] = v
	}
	if _, ok := extra["plugin_name"]; !ok {
		extra["plugin_name"] = c.manifest.Name
	}
	spec.Extra = extra
	if spec.RequiredEnv == nil {
		spec.RequiredEnv = []string{}
	}
	entry, err := registry.NewEntry(spec)
	if err != nil {
		return nil, err
	}
	scope := c.manager.home
	previous := registry.Snapshot(spec.Name, scope)
	registry.Register(entry, scope)
	current := registry.Snapshot(spec.Name, scope)
	if !sameRegistration(current.Entry, entry) || current.Loader != nil {
		return nil, nil
	}
	return c.manager.leasePlatform(c.manifest, spec.Name, registry, current, previous), nil
}

// Adds a platform name to the plugin set and leases its registry slot for unload
func (m *Manager) leasePlatform(manifest *Manifest, name string, registry PlatformRegistry, current, previous PlatformSlot) *Registration {
	m.mu.Lock()
	m.pluginPlatformNames[name] = true
	m.mu.Unlock()
	handle := m.trackScopedRegistration(manifest, "platform", name, func(name string, current, replacement any, scope string) error {
		r, _ := replacement.(PlatformSlot)
		registry.Restore(name, current.(PlatformSlot), r, scope)
		return nil
	}, current, previous, func() error {
		m.removePlatformNameIfUnowned(name)
		return nil
	})
	logDebug(fmt.Sprintf("Plugin %s registered platform: %s", manifest.Name, name))
	return handle
}

// Slack Block Kit action handler a plugin registered
type SlackActionHandler struct {
	ActionID any
	Callback any
	Plugin   string
}

// Registers a Slack Block Kit action handler wired into the Slack app at connect
func (c *Context) RegisterSlackActionHandler(actionID any, callback any) (*Registration, error) {
	if c.ignoredAfterAbandonedLoad("register_slack_action_handler") {
		return nil, nil
	}
	if !callable(callback) {
		return nil, c.refuse("a Slack action handler with a non-callable callback")
	}
	if s, isStr := actionID.(string); actionID == nil || (isStr && pyStrip(s) == "") {
		return nil, c.refuse("a Slack action handler with an empty action_id")
	}
	entry := &SlackActionHandler{ActionID: actionID, Callback: callback, Plugin: c.manifest.Name}
	m := c.manager
	m.mu.Lock()
	m.slackActionHandlers = append(m.slackActionHandlers, entry)
	m.mu.Unlock()
	handle := c.track("slack_action_handler", config.PyRepr(actionID), func() error {
		m.mu.Lock()
		m.slackActionHandlers, _ = removeIdentity(m.slackActionHandlers, entry)
		m.mu.Unlock()
		return nil
	}, false)
	logDebug(fmt.Sprintf("Plugin %s registered Slack action handler: %s", c.manifest.Name, config.PyStr(actionID)))
	return handle, nil
}

// Platform handler factory a plugin registered, called with the native client and adapter at connect
type PlatformHandlerFactory struct {
	Factory any
	Plugin  string
}

// Registers a factory invoked at platform connect before or as the core handlers register
func (c *Context) RegisterPlatformHandler(platform string, factory any) error {
	if c.ignoredAfterAbandonedLoad("register_platform_handler") {
		return nil
	}
	if !callable(factory) {
		return c.refuse("a platform handler factory with a non-callable factory")
	}
	key := strings.ToLower(pyStrip(platform))
	if key == "" {
		return c.refuse("a platform handler factory with an empty platform name")
	}
	m := c.manager
	m.mu.Lock()
	m.platformHandlerFactories[key] = append(m.platformHandlerFactories[key], &PlatformHandlerFactory{Factory: factory, Plugin: c.manifest.Name})
	m.mu.Unlock()
	_, fname := callbackNames(factory)
	logDebug(fmt.Sprintf("Plugin %s registered %s handler factory: %s", c.manifest.Name, key, fname))
	return nil
}

// Registers a telegram handler factory
func (c *Context) RegisterTelegramHandler(factory any) error {
	return c.RegisterPlatformHandler("telegram", factory)
}

// Options of one plugin auxiliary task registration, each holding the value the plugin passed
type AuxTaskOptions struct {
	DisplayName any
	Description any
	Defaults    any
	InheritFrom any
}

// Registers an auxiliary LLM task with its own auxiliary config block, the key a str a plugin passed
func (c *Context) RegisterAuxiliaryTask(rawKey any, opts AuxTaskOptions) (*Registration, error) {
	if c.ignoredAfterAbandonedLoad("register_auxiliary_task") {
		return nil, nil
	}
	key, _ := rawKey.(string)
	switch rawKey.(type) {
	case []any:
		return nil, &HostBuiltinError{Type: "TypeError", Message: "unhashable type: 'list'"}
	case map[string]any, Record:
		return nil, &HostBuiltinError{Type: "TypeError", Message: "unhashable type: 'dict'"}
	}
	var handle *Registration
	var err error
	serializedReplacement(func() {
		m := c.manager
		m.mu.RLock()
		existing := m.auxTasks[key]
		registered := map[string]*auxTask{}
		for k, v := range m.auxTasks {
			registered[k] = v
		}
		m.mu.RUnlock()
		var entry *auxTask
		entry, err = buildAuxiliaryTaskEntry(c.manifest.Name, c.PluginID(), rawKey, opts, registered)
		if err != nil {
			return
		}
		handle = registerEntry(c, "auxiliary_task", key, m.auxTasks, entry, &existing,
			fmt.Sprintf("Plugin %s registered auxiliary task: %s (%s)", c.manifest.Name, key, config.PyStr(opts.DisplayName)))
	})
	return handle, err
}

// Registry entry of one plugin auxiliary task
type auxTask struct{ fields map[string]any }

// Validated auxiliary task entry, an error for a bad, built-in or foreign key and no inheritance from a bad base
func buildAuxiliaryTaskEntry(me, ownerID string, rawKey any, opts AuxTaskOptions, registered map[string]*auxTask) (*auxTask, error) {
	key, isStr := rawKey.(string)
	if !isStr || key == "" {
		return nil, fmt.Errorf("Plugin '%s' tried to register auxiliary task with invalid key %s", me, config.PyRepr(rawKey))
	}
	for _, r := range key {
		if !(unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_') {
			return nil, fmt.Errorf("Plugin '%s' auxiliary task key %s must contain only alphanumeric characters and underscores", me, config.PyRepr(key))
		}
	}
	builtinKeys, err := needService("BuiltinAuxTaskKeys", func(s *Services) func() []string { return s.BuiltinAuxTaskKeys })
	if err != nil {
		return nil, err
	}
	builtin := toSet(builtinKeys())
	if builtin[key] {
		return nil, fmt.Errorf("Plugin '%s' cannot register auxiliary task %s — that key is reserved for a built-in task. Pick a plugin-namespaced key (e.g. '%s_%s').", me, config.PyRepr(key), me, key)
	}
	if existing := registered[key]; existing != nil && existing.fields["plugin"] != ownerID {
		return nil, fmt.Errorf("Plugin '%s' cannot register auxiliary task %s — already registered by plugin '%s'", me, config.PyRepr(key), config.PyStr(existing.fields["plugin"]))
	}
	inherit := opts.InheritFrom
	if inherit != nil {
		base, isStr := inherit.(string)
		_, known := registered[base]
		if !isStr || base == key || (!builtin[base] && !known) {
			logWarn(fmt.Sprintf("Plugin '%s' auxiliary task %s: ignoring inherit_from=%s — not a built-in auxiliary task or one already registered by a plugin", me, config.PyRepr(key), config.PyRepr(inherit)))
			inherit = nil
		}
	}
	inheriting := inherit != nil && inherit != ""
	overrides, err := auxTaskOverrides(opts.Defaults, inheriting)
	if err != nil {
		return nil, err
	}
	defaults := map[string]any{}
	if !inheriting {
		defaults = map[string]any{"provider": "auto", "model": "", "base_url": "", "api_key": "", "timeout": 60, "extra_body": map[string]any{}}
	}
	for k, v := range overrides {
		defaults[k] = v
	}
	return &auxTask{fields: map[string]any{
		"key": key, "display_name": opts.DisplayName, "description": opts.Description, "defaults": defaults,
		"inherit_from": inherit, "plugin": ownerID, "plugin_key": ownerID,
	}}, nil
}

// Plugin overrides of an auxiliary task, dict(defaults or {}) when inheriting and a mapping unpack otherwise
func auxTaskOverrides(raw any, inheriting bool) (map[string]any, error) {
	switch m := raw.(type) {
	case map[string]any:
		return m, nil
	case Record:
		return map[string]any(m), nil
	}
	if !config.PyTruthy(raw) {
		return map[string]any{}, nil
	}
	if !inheriting {
		return nil, &HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf("'%s' object is not a mapping", pyTypeName(raw))}
	}
	items, isList := raw.([]any)
	if _, isStr := raw.(string); isStr {
		return nil, &HostBuiltinError{Type: "ValueError", Message: "dictionary update sequence element #0 has length 1; 2 is required"}
	} else if !isList {
		return nil, &HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf("'%s' object is not iterable", pyTypeName(raw))}
	}
	out := map[string]any{}
	for i, item := range items {
		var pair []any
		switch t := item.(type) {
		case []any:
			pair = t
		case string:
			for _, r := range t {
				pair = append(pair, string(r))
			}
		default:
			return nil, &HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf("cannot convert dictionary update sequence element #%d to a sequence", i)}
		}
		if len(pair) != 2 {
			return nil, &HostBuiltinError{Type: "ValueError", Message: fmt.Sprintf("dictionary update sequence element #%d has length %d; 2 is required", i, len(pair))}
		}
		switch pair[0].(type) {
		case []any:
			return nil, &HostBuiltinError{Type: "TypeError", Message: "unhashable type: 'list'"}
		case map[string]any, Record:
			return nil, &HostBuiltinError{Type: "TypeError", Message: "unhashable type: 'dict'"}
		}
		out[config.PyStr(pair[0])] = pair[1]
	}
	return out, nil
}

// Additively registers secret token regexes with the redactor, returning how many were accepted
func (c *Context) RegisterRedactionPatterns(patterns any) int {
	if c.ignoredAfterAbandonedLoad("register_redaction_patterns") {
		return 0
	}
	register, err := needService("RegisterRedactionPatterns", func(s *Services) func(any, string) (int, error) { return s.RegisterRedactionPatterns })
	count := 0
	if err == nil {
		err = callGuarded(func() error {
			var rerr error
			count, rerr = register(patterns, "plugin:"+c.manifest.Name)
			return rerr
		})
	}
	if err != nil {
		logWarn(fmt.Sprintf("Plugin '%s' redaction pattern registration failed: %v", c.manifest.Name, err))
		return 0
	}
	logDebug(fmt.Sprintf("Plugin %s registered %d redaction pattern(s)", c.manifest.Name, count))
	return count
}

// Registers one language pack layer for a surface, unload removing the layer
func (c *Context) RegisterLocale(lang string, source any, endonym string, rtl bool, surface string) (*Registration, error) {
	if c.ignoredAfterAbandonedLoad("register_locale") {
		return nil, nil
	}
	if surface == "" {
		surface = i18n.CoreSurface
	}
	langID := i18n.NormalizeLanguageID(lang)
	if !i18n.IsLanguageID(langID) {
		return nil, c.refuse(fmt.Sprintf("locale with invalid language id %s (expected e.g. 'pl', 'pt-br')", config.PyRepr(lang)))
	}
	known := false
	for _, s := range i18n.Surfaces {
		known = known || s == surface
	}
	if !known {
		return nil, c.refuse(fmt.Sprintf("locale %s for unknown surface %s (one of %s)", config.PyRepr(langID), config.PyRepr(surface), strings.Join(i18n.Surfaces, ", ")))
	}
	messages, err := i18n.LoadLocaleSource(source)
	if err != nil {
		return nil, err
	}
	entry, err := i18n.RegisterPack(langID, surface, messages, "plugin:"+c.manifest.Name, endonym, rtl)
	if err != nil {
		return nil, err
	}
	handle := c.track("locale", langID+"."+surface, func() error {
		i18n.UnregisterPack(entry)
		return nil
	}, false)
	logDebug(fmt.Sprintf("Plugin %s registered locale: %s/%s (%d keys)", c.manifest.Name, langID, surface, len(messages)))
	return handle, nil
}

// Registers every language file of a locales directory, a broken file skipped with a warning
func (c *Context) RegisterLocaleDir(path string, metadata map[string]map[string]any) []*Registration {
	if c.ignoredAfterAbandonedLoad("register_locale_dir") {
		return nil
	}
	handles := []*Registration{}
	for _, file := range i18n.ScanLocaleDir(path) {
		meta := metadata[file.Lang]
		endonym, _ := meta["endonym"].(string)
		handle, err := c.RegisterLocale(file.Lang, file.Path, endonym, truthy(meta["rtl"]), file.Surface)
		if err != nil {
			logWarn(fmt.Sprintf("Plugin '%s' locale file %s skipped: %v", c.manifest.Name, file.Path, err))
			continue
		}
		if handle != nil {
			handles = append(handles, handle)
		}
	}
	return handles
}

// Read only plugin skill resolvable as plugin:name while the plugin is enabled
type PluginSkill struct {
	Path        string
	Plugin      string
	PluginKey   string
	PluginName  string
	Portable    bool
	BareName    string
	Description string
	Frontmatter map[string]any
}

// Registers a read only skill under the plugin namespace, refusing invalid names and missing files
func (c *Context) RegisterSkill(name, path, description string, frontmatter map[string]any) (*Registration, error) {
	if c.ignoredAfterAbandonedLoad("register_skill") {
		return nil, nil
	}
	var handle *Registration
	var err error
	serializedReplacement(func() { handle, err = c.registerSkill(name, path, description, frontmatter) })
	return handle, err
}

// Validation and lease body of RegisterSkill inside the replacement transaction
func (c *Context) registerSkill(name, path, description string, frontmatter map[string]any) (*Registration, error) {
	if strings.Contains(name, ":") {
		return nil, fmt.Errorf("Skill name '%s' must not contain ':' (the namespace is derived from the plugin name '%s' automatically).", name, c.manifest.Name)
	}
	if name == "" || !skillNamespaceRe.MatchString(name) {
		return nil, fmt.Errorf("Invalid skill name '%s'. Must match [a-zA-Z0-9_-]+.", name)
	}
	if !pathExists(path) {
		return nil, &FileNotFoundError{Msg: "SKILL.md not found at " + path}
	}
	namespace := c.manifest.Name
	if c.manifest.Portable {
		namespace = portableSlug(c.manifest.Name)
	}
	qualified := namespace + ":" + name
	m := c.manager
	m.mu.RLock()
	existing := m.pluginSkills[qualified]
	m.mu.RUnlock()
	if existing != nil && (c.manifest.Portable || (existing.Portable && existing.PluginKey != c.PluginID())) {
		return nil, fmt.Errorf("Plugin skill '%s' is already registered by plugin '%s'", qualified, existing.PluginKey)
	}
	fm := map[string]any{}
	for k, v := range frontmatter {
		fm[k] = v
	}
	entry := &PluginSkill{Path: path, Plugin: namespace, PluginKey: c.PluginID(), PluginName: c.manifest.Name,
		Portable: c.manifest.Portable, BareName: name, Description: description, Frontmatter: fm}
	return registerEntry(c, "skill", qualified, m.pluginSkills, entry, nil,
		fmt.Sprintf("Plugin %s registered skill: %s", c.manifest.Name, qualified)), nil
}

// Missing file error mirroring Python FileNotFoundError
type FileNotFoundError struct{ Msg string }

func (e *FileNotFoundError) Error() string { return e.Msg }

// Names plugins.enabled and plugins.disabled may use for the plugin that registered a skill
func pluginSkillOwners(m *Manager, qualified string) map[string]bool {
	m.mu.RLock()
	entry := m.pluginSkills[qualified]
	m.mu.RUnlock()
	owners := map[string]bool{}
	if entry != nil {
		for _, name := range []string{entry.PluginKey, entry.PluginName} {
			if name != "" {
				owners[name] = true
			}
		}
	}
	if len(owners) == 0 {
		prefix, _, _ := strings.Cut(qualified, ":")
		owners[prefix] = true
	}
	return owners
}

// Fields of one plugin automation blueprint registration
type BlueprintSpec struct {
	Title            string
	Description      string
	ScheduleTemplate string
	PromptTemplate   string
	Category         string
	Slots            []any
	DeliverDefault   string
	Skills           []string
	Tags             []string
}

// Adds an automation blueprint to this profile catalog, malformed or duplicate input warned and skipped
func (c *Context) RegisterAutomationBlueprint(key string, spec BlueprintSpec) (*Registration, error) {
	if c.ignoredAfterAbandonedLoad("register_automation_blueprint") {
		return nil, nil
	}
	if spec.Category == "" {
		spec.Category = "general"
	}
	if spec.DeliverDefault == "" {
		spec.DeliverDefault = "origin"
	}
	build, err := needService("BuildPluginBlueprint", func(s *Services) func(string, string, BlueprintSpec) (string, any, error) {
		return s.BuildPluginBlueprint
	})
	if err != nil {
		return nil, err
	}
	bpKey, blueprint, err := build(c.manifest.Name, key, spec)
	if err != nil {
		logWarn(fmt.Sprintf("Plugin '%s' automation blueprint %s rejected: %v", c.manifest.Name, config.PyRepr(key), err))
		return nil, nil
	}
	m := c.manager
	m.mu.RLock()
	_, dup := m.automationBlueprints[bpKey]
	m.mu.RUnlock()
	if dup {
		logWarn(fmt.Sprintf("Plugin '%s' automation blueprint %s is already registered", c.manifest.Name, config.PyRepr(bpKey)))
		return nil, nil
	}
	return registerEntry(c, "automation_blueprint", bpKey, m.automationBlueprints, &blueprintEntry{value: blueprint}, nil,
		fmt.Sprintf("Plugin %s registered automation blueprint: %s", c.manifest.Name, bpKey)), nil
}

// Identity wrapper of one registered automation blueprint
type blueprintEntry struct{ value any }

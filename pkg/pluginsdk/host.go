package pluginsdk

import (
	"context"
	"fmt"
	"sync"
)

// Host bound to one loaded plugin, every method one request on the wire
type host struct {
	peer     *peer
	key      string
	id       string
	name     string
	dir      string
	profile  string
	manifest map[string]any
	// Registrar whose argument a provider load keeps instead of sending, empty for a plugin load
	capture string
	// Whether registrations other than the captured one still reach the host during a provider load
	forward  bool
	mu       sync.Mutex
	loading  context.Context
	unloads  []func() error
	captured []any
}

// Registrar name of each provider kind
var providerRegistrars = map[string]string{
	"image_gen": "register_image_gen_provider", "video_gen": "register_video_gen_provider", "web_search": "register_web_search_provider",
	"browser": "register_browser_provider", "terminal_environment": "register_terminal_environment_provider",
	"secret_source": "register_secret_source", "tts": "register_tts_provider", "transcription": "register_transcription_provider",
	"delegation": "register_delegation_provider", "context_engine": "register_context_engine",
	"context_reference": "register_context_reference", "memory": "register_memory_provider",
	"browser_page_backend": "register_browser_page_backend", "dashboard_auth": "register_dashboard_auth_provider",
	"cron_scheduler": "register_cron_scheduler", "computer_use": "register_computer_use_provider",
	"model_provider": "register_model_provider",
}

func (h *host) PluginID() string         { return h.id }
func (h *host) PluginKey() string        { return h.key }
func (h *host) Name() string             { return h.name }
func (h *host) Dir() string              { return h.dir }
func (h *host) ProfileName() string      { return h.profile }
func (h *host) Manifest() map[string]any { return h.manifest }

// Context of the load request while the register function runs, a background context after it
func (h *host) registering() context.Context {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.loading != nil {
		return h.loading
	}
	return context.Background()
}

func (h *host) Call(ctx context.Context, method string, args []any, kwargs map[string]any) (any, error) {
	return h.peer.call(ctx, "ctx", map[string]any{"plugin": h.key, "method": method, "args": h.peer.encode(listOrEmpty(args)),
		"kwargs": h.peer.encode(mapOrEmpty(kwargs))})
}

func (h *host) Facade(ctx context.Context, facade, method string, args []any, kwargs map[string]any) (any, error) {
	return h.peer.call(ctx, "facade", map[string]any{"plugin": h.key, "facade": facade, "method": method,
		"args": h.peer.encode(listOrEmpty(args)), "kwargs": h.peer.encode(mapOrEmpty(kwargs))})
}

// Keeps the argument of the captured registrar and reports whether a provider load swallows the registration
func (h *host) intercept(method string, args []any, kwargs map[string]any) bool {
	if h.capture == "" {
		return false
	}
	if method != h.capture {
		return !h.forward
	}
	var value any
	if len(args) > 0 {
		value = args[0]
	} else {
		value = kwargs
	}
	h.mu.Lock()
	h.captured = append(h.captured, value)
	h.mu.Unlock()
	return true
}

// Runs one registrar and returns the handle of the registration it made, nil when the host returns none
func (h *host) register(method string, args []any, kwargs map[string]any) (*Handle, error) {
	if h.intercept(method, args, kwargs) {
		return nil, nil
	}
	value, err := h.Call(h.registering(), method, args, kwargs)
	if err != nil {
		return nil, err
	}
	handle, _ := value.(*Handle)
	return handle, nil
}

func (h *host) Config(ctx context.Context, key string, def any) (any, error) {
	return h.Call(ctx, "get_config", []any{key, def}, nil)
}

func (h *host) SetConfig(ctx context.Context, key string, value any) error {
	_, err := h.Call(ctx, "set_config", []any{key, value}, nil)
	return err
}

func (h *host) HasPlugin(ctx context.Context, id string) (bool, error) {
	value, err := h.Call(ctx, "has_plugin", []any{id}, nil)
	ok, _ := value.(bool)
	return ok, err
}

func (h *host) HasCapability(ctx context.Context, capability string) (bool, error) {
	value, err := h.Call(ctx, "has_capability", []any{capability}, nil)
	ok, _ := value.(bool)
	return ok, err
}

func (h *host) RegisterTool(tool Tool) (*Handle, error) {
	if tool.Handler == nil {
		return nil, fmt.Errorf("tool %q has no handler", tool.Name)
	}
	handler := Func(func(ctx context.Context, args []any, kwargs map[string]any) (any, error) {
		call := ToolCall{TaskID: text(kwargs["task_id"]), SessionID: text(kwargs["session_id"]), UserTask: text(kwargs["user_task"])}
		if len(args) > 0 {
			call.Args, _ = args[0].(map[string]any)
		}
		call.Args = mapOrEmpty(call.Args)
		return tool.Handler(ctx, call)
	})
	kwargs := map[string]any{"name": tool.Name, "toolset": tool.Toolset, "schema": mapOrEmpty(tool.Schema),
		"handler": h.peer.funcRef(tool.Name, handler), "description": tool.Description, "emoji": tool.Emoji, "override": tool.Override}
	if len(tool.RequiresEnv) > 0 {
		kwargs["requires_env"] = tool.RequiresEnv
	}
	if tool.Check != nil {
		kwargs["check_fn"] = h.checkRef(tool.Name+".check", tool.Check)
	}
	return h.register("register_tool", nil, kwargs)
}

// Callback reference of an availability check
func (h *host) checkRef(name string, check func(ctx context.Context) bool) map[string]any {
	return h.peer.funcRef(name, func(ctx context.Context, _ []any, _ map[string]any) (any, error) { return check(ctx), nil })
}

func (h *host) RegisterHook(name string, fn HookFunc, holdStream bool) (*Handle, error) {
	return h.register("register_hook", nil, map[string]any{"hook_name": name, "callback": h.hookRef(name, fn), "hold_stream": holdStream})
}

func (h *host) hookRef(name string, fn HookFunc) map[string]any {
	return h.peer.funcRef(name, func(ctx context.Context, _ []any, kwargs map[string]any) (any, error) { return fn(ctx, kwargs) })
}

func (h *host) RegisterMiddleware(kind string, fn HookFunc) (*Handle, error) {
	return h.register("register_middleware", nil, map[string]any{"kind": kind, "callback": h.hookRef(kind, fn)})
}

func (h *host) RegisterCommand(cmd Command) (*Handle, error) {
	if cmd.Handler == nil {
		return nil, fmt.Errorf("command %q has no handler", cmd.Name)
	}
	handler := h.peer.funcRef(cmd.Name, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		raw := ""
		if len(args) > 0 {
			raw = text(args[0])
		}
		return cmd.Handler(ctx, raw)
	})
	kwargs := map[string]any{"name": cmd.Name, "handler": handler, "description": cmd.Description, "args_hint": cmd.ArgsHint}
	if cmd.ArgumentMode != "" {
		kwargs["argument_mode"] = cmd.ArgumentMode
	}
	return h.register("register_command", nil, kwargs)
}

func (h *host) RegisterProvider(kind string, provider *Object) (*Handle, error) {
	method, ok := providerRegistrars[kind]
	if !ok {
		return nil, fmt.Errorf("unknown provider kind %q", kind)
	}
	if provider == nil {
		return nil, fmt.Errorf("provider of kind %q is nil", kind)
	}
	return h.register(method, []any{provider}, nil)
}

func (h *host) RegisterLoginBackend(b LoginBackend) (*Handle, error) {
	if b.Name == "" || b.Prefix == "" {
		return nil, fmt.Errorf("login backend needs a name and a handle prefix")
	}
	return h.register("register_login_backend", []any{loginBackendObject(b)}, nil)
}

func (h *host) RegisterSlackActionHandler(actionID any, fn SlackActionFunc) (*Handle, error) {
	callback := h.peer.funcRef("slack_action", func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		if len(args) < 3 {
			return nil, fmt.Errorf("slack action callback received %d arguments, expected ack, body and action", len(args))
		}
		complete, _ := args[0].(*HostFunc)
		if complete == nil {
			return nil, fmt.Errorf("slack action callback received no acknowledgement function")
		}
		ack := func(payload map[string]any) error {
			_, err := complete.Call(ctx, []any{payload}, nil)
			return err
		}
		body, _ := args[1].(map[string]any)
		action, _ := args[2].(map[string]any)
		return nil, fn(ctx, ack, mapOrEmpty(body), mapOrEmpty(action))
	})
	return h.register("register_slack_action_handler", []any{actionID, callback}, nil)
}

func (h *host) RegisterApprovalTransport(name string, present ApprovalFunc) error {
	callback := h.peer.funcRef(name+".present", func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		var f map[string]any
		if len(args) > 0 {
			f, _ = args[0].(map[string]any)
		}
		req := ApprovalRequest{RequestID: text(f["request_id"]), Digest: text(f["digest"]), Command: text(f["command"]),
			Description: text(f["description"]), PatternKey: text(f["pattern_key"]), Surface: text(f["surface"]),
			PatternKeys: stringsOf(f["pattern_keys"]), AllowedChoices: stringsOf(f["allowed_choices"])}
		req.SchemaVersion, _ = integer(f["schema_version"])
		req.TimeoutSeconds = number(f["timeout_seconds"])
		choice, err := present(ctx, req)
		if err != nil || choice == "" {
			return nil, err
		}
		return Record{Type: "ApprovalDecision", Fields: map[string]any{"request_id": req.RequestID, "request_digest": req.Digest, "choice": choice}}, nil
	})
	_, err := h.register("register_approval_transport", []any{name, callback}, nil)
	return err
}

func (h *host) RegisterWidget(widget Widget) (*Handle, error) {
	if widget.Render == nil {
		return nil, fmt.Errorf("widget %q has no render function", widget.ID)
	}
	kwargs := map[string]any{"id": widget.ID, "help": widget.Help, "usage": widget.Usage, "mode": widget.Mode, "zone": widget.Zone, "width": widget.Width}
	kwargs["render"] = h.peer.funcRef(widget.ID+".render", func(ctx context.Context, _ []any, kw map[string]any) (any, error) {
		cols, _ := integer(kw["cols"])
		rows, _ := integer(kw["rows"])
		return widget.Render(ctx, WidgetFrame{Cols: int(cols), Rows: int(rows), State: kw["state"]})
	})
	if widget.Open != nil {
		kwargs["open"] = h.peer.funcRef(widget.ID+".open", func(ctx context.Context, _ []any, kw map[string]any) (any, error) {
			state, ok, err := widget.Open(ctx, text(kw["arg"]))
			return map[string]any{"state": state, "ok": ok}, err
		})
	}
	if widget.Input != nil {
		kwargs["input"] = h.peer.funcRef(widget.ID+".input", func(ctx context.Context, _ []any, kw map[string]any) (any, error) {
			in, _ := kw["input"].(map[string]any)
			flag := func(name string) bool { b, _ := in[name].(bool); return b }
			next, open, err := widget.Input(ctx, kw["state"], WidgetInput{Text: text(in["text"]), Key: text(in["key"]),
				Ctrl: flag("ctrl"), Alt: flag("alt"), Shift: flag("shift")})
			return map[string]any{"state": next, "open": open}, err
		})
	}
	return h.register("register_widget", nil, kwargs)
}

func (h *host) RegisterSkill(name, path, description string, frontmatter map[string]any) (*Handle, error) {
	return h.register("register_skill", []any{name, path, description, frontmatter}, nil)
}

func (h *host) RegisterLocale(lang string, source any, locale Locale) (*Handle, error) {
	kwargs := map[string]any{"rtl": locale.RTL}
	if locale.Endonym != "" {
		kwargs["endonym"] = locale.Endonym
	}
	if locale.Surface != "" {
		kwargs["surface"] = locale.Surface
	}
	return h.register("register_locale", []any{lang, source}, kwargs)
}

func (h *host) RegisterLocaleDir(path string, metadata map[string]map[string]any) error {
	kwargs := map[string]any{}
	if metadata != nil {
		kwargs["metadata"] = metadata
	}
	_, err := h.register("register_locale_dir", []any{path}, kwargs)
	return err
}

func (h *host) RegisterSystemPromptSection(id string, content any, section Section) (*Handle, error) {
	if render, ok := content.(func(ctx context.Context, session map[string]any) (string, error)); ok {
		content = h.peer.funcRef(id, func(ctx context.Context, args []any, _ map[string]any) (any, error) {
			var session map[string]any
			if len(args) > 0 {
				session, _ = args[0].(map[string]any)
			}
			return render(ctx, mapOrEmpty(session))
		})
	} else if _, isText := content.(string); !isText {
		return nil, fmt.Errorf("system prompt section %q needs a string or a func(context.Context, map[string]any) (string, error)", id)
	}
	kwargs := map[string]any{}
	if section.Position != "" {
		kwargs["position"] = section.Position
	}
	if section.MaxChars > 0 {
		kwargs["max_chars"] = section.MaxChars
	}
	return h.register("register_system_prompt_section", []any{id, content}, kwargs)
}

func (h *host) RegisterAutomationBlueprint(key string, b Blueprint) (*Handle, error) {
	kwargs := map[string]any{"title": b.Title, "description": b.Description, "schedule_template": b.ScheduleTemplate,
		"prompt_template": b.PromptTemplate, "slots": listOrEmpty(b.Slots), "skills": b.Skills, "tags": b.Tags}
	if b.Category != "" {
		kwargs["category"] = b.Category
	}
	if b.DeliverDefault != "" {
		kwargs["deliver_default"] = b.DeliverDefault
	}
	if b.Skills == nil {
		kwargs["skills"] = []any{}
	}
	if b.Tags == nil {
		kwargs["tags"] = []any{}
	}
	return h.register("register_automation_blueprint", []any{key}, kwargs)
}

func (h *host) RegisterAuxiliaryTask(key string, task AuxiliaryTask) (*Handle, error) {
	kwargs := map[string]any{"display_name": task.DisplayName, "description": task.Description}
	if task.Defaults != nil {
		kwargs["defaults"] = task.Defaults
	}
	if task.InheritFrom != "" {
		kwargs["inherit_from"] = task.InheritFrom
	}
	return h.register("register_auxiliary_task", []any{key}, kwargs)
}

func (h *host) RegisterRedactionPatterns(patterns []string) (int, error) {
	if h.intercept("register_redaction_patterns", []any{patterns}, nil) {
		return 0, nil
	}
	value, err := h.Call(h.registering(), "register_redaction_patterns", []any{patterns}, nil)
	n, _ := integer(value)
	return int(n), err
}

// Keeps the dashboard API handler of this plugin for the host to forward requests to
func (h *host) RegisterDashboardAPI(handler APIHandler) {
	h.peer.mu.Lock()
	defer h.peer.mu.Unlock()
	if handler == nil {
		delete(h.peer.apis, h.key)
		return
	}
	h.peer.apis[h.key] = handler
}

func (h *host) RegisterGatewayHook(fn GatewayHookFunc) error {
	if fn == nil {
		return fmt.Errorf("gateway hook has no handler")
	}
	callback := h.peer.funcRef("gateway_hook", func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		eventType := ""
		var hookContext map[string]any
		if len(args) > 0 {
			eventType = text(args[0])
		}
		if len(args) > 1 {
			hookContext, _ = args[1].(map[string]any)
		}
		return fn(ctx, eventType, mapOrEmpty(hookContext))
	})
	_, err := h.register("register_gateway_hook", []any{callback}, nil)
	return err
}

func (h *host) Subscribe(event string, fn HookFunc) error {
	_, err := h.register("subscribe", []any{event, h.hookRef(event, fn)}, nil)
	return err
}

func (h *host) Emit(ctx context.Context, event string, payload map[string]any) (int, error) {
	value, err := h.Call(ctx, "emit", []any{event, mapOrEmpty(payload)}, nil)
	n, _ := integer(value)
	return int(n), err
}

func (h *host) OnUnload(fn func() error) {
	h.mu.Lock()
	h.unloads = append(h.unloads, fn)
	h.mu.Unlock()
}

func (h *host) DispatchTool(ctx context.Context, name string, args map[string]any) (string, error) {
	value, err := h.Call(ctx, "dispatch_tool", []any{name, mapOrEmpty(args)}, nil)
	return text(value), err
}

func (h *host) CallMCP(ctx context.Context, server, tool string, args map[string]any, timeoutSeconds float64) (map[string]any, error) {
	kwargs := map[string]any{"arguments": mapOrEmpty(args)}
	if timeoutSeconds > 0 {
		kwargs["timeout"] = timeoutSeconds
	}
	value, err := h.Call(ctx, "call_mcp", []any{server, tool}, kwargs)
	out, _ := value.(map[string]any)
	return out, err
}

func (h *host) InjectMessage(ctx context.Context, content, role, sessionKey string) (bool, error) {
	kwargs := map[string]any{}
	if role != "" {
		kwargs["role"] = role
	}
	if sessionKey != "" {
		kwargs["session_key"] = sessionKey
	}
	value, err := h.Call(ctx, "inject_message", []any{content}, kwargs)
	ok, _ := value.(bool)
	return ok, err
}

func (h *host) PostMessage(ctx context.Context, text string) (any, error) {
	return h.Call(ctx, "post_message", []any{text}, nil)
}

func (h *host) AskChoice(ctx context.Context, question string, choices []string) (string, bool, error) {
	value, err := h.Call(ctx, "ask_choice", []any{question, choices}, nil)
	choice, ok := value.(string)
	return choice, ok, err
}

func (h *host) SideQuestion(ctx context.Context, token, prompt string, timeoutSeconds float64) (string, bool, error) {
	kwargs := map[string]any{}
	if timeoutSeconds > 0 {
		kwargs["timeout"] = timeoutSeconds
	}
	value, err := h.Call(ctx, "side_question", []any{token, prompt}, kwargs)
	answer, ok := value.(string)
	return answer, ok, err
}

func (h *host) SessionCwd(ctx context.Context) (string, error) {
	value, err := h.Call(ctx, "session_cwd", nil, nil)
	return text(value), err
}

func (h *host) SessionSurface(ctx context.Context) (string, error) {
	value, err := h.Call(ctx, "session_surface", nil, nil)
	return text(value), err
}

func (h *host) CurrentCronExecution(ctx context.Context) (any, error) {
	return h.Call(ctx, "current_cron_execution", nil, nil)
}

// One granted input or output operation on the host
func (h *host) io(ctx context.Context, op string, fields map[string]any) (map[string]any, error) {
	params := map[string]any{"plugin": h.key, "op": op}
	for k, v := range fields {
		params[k] = h.peer.encode(v)
	}
	value, err := h.peer.call(ctx, "io", params)
	if err != nil {
		return nil, err
	}
	out, _ := value.(map[string]any)
	return mapOrEmpty(out), nil
}

func (h *host) ReadFile(ctx context.Context, path string) ([]byte, error) {
	out, err := h.io(ctx, "read_file", map[string]any{"path": path})
	data, _ := out["data"].([]byte)
	return data, err
}

func (h *host) WriteFile(ctx context.Context, path string, data []byte) error {
	if data == nil {
		data = []byte{}
	}
	_, err := h.io(ctx, "write_file", map[string]any{"path": path, "data": data})
	return err
}

func (h *host) ListDir(ctx context.Context, path string) ([]DirEntry, error) {
	out, err := h.io(ctx, "list_dir", map[string]any{"path": path})
	if err != nil {
		return nil, err
	}
	entries := []DirEntry{}
	for _, raw := range asList(out["entries"]) {
		item, _ := raw.(map[string]any)
		size, _ := integer(item["size"])
		dir, _ := item["dir"].(bool)
		entries = append(entries, DirEntry{Name: text(item["name"]), Dir: dir, Size: size})
	}
	return entries, nil
}

func (h *host) Remove(ctx context.Context, path string) error {
	_, err := h.io(ctx, "remove", map[string]any{"path": path})
	return err
}

func (h *host) HTTP(ctx context.Context, req HTTPRequest) (*HTTPResponse, error) {
	fields := map[string]any{"method": req.Method, "url": req.URL, "headers": req.Headers, "timeout": req.TimeoutSeconds}
	if req.Body != nil {
		fields["body"] = req.Body
	}
	out, err := h.io(ctx, "http", fields)
	if err != nil {
		return nil, err
	}
	status, _ := integer(out["status"])
	resp := &HTTPResponse{Status: int(status), Headers: map[string]string{}}
	resp.Body, _ = out["body"].([]byte)
	headers, _ := out["headers"].(map[string]any)
	for k, v := range headers {
		resp.Headers[k] = text(v)
	}
	return resp, nil
}

func (h *host) Env(ctx context.Context, name string) (string, bool, error) {
	out, err := h.io(ctx, "env", map[string]any{"name": name})
	ok, _ := out["ok"].(bool)
	return text(out["value"]), ok, err
}

func stringsOf(v any) []string {
	items := asList(v)
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, text(item))
	}
	return out
}

func number(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	}
	return 0
}

func (h *host) ProviderRequest(ctx context.Context, req ProviderRequest) (*HTTPResponse, error) {
	kwargs := map[string]any{"json": req.JSON, "headers": req.Headers, "timeout": req.TimeoutSeconds}
	value, err := h.Call(ctx, "credentialed_provider_request", []any{req.Provider, req.Method, req.URL}, kwargs)
	if err != nil {
		return nil, err
	}
	out, _ := value.(map[string]any)
	status, _ := integer(out["status"])
	resp := &HTTPResponse{Status: int(status), Headers: map[string]string{}}
	resp.Body, _ = out["body"].([]byte)
	headers, _ := out["headers"].(map[string]any)
	for k, v := range headers {
		resp.Headers[k] = text(v)
	}
	return resp, nil
}

// Runs one host utility by name, such as redact_sensitive_text, get_env_value or build_subprocess_env
func (h *host) SDK(ctx context.Context, op string, args []any, kwargs map[string]any) (any, error) {
	return h.peer.call(ctx, "sdk", map[string]any{"plugin": h.key, "op": op, "args": h.peer.encode(listOrEmpty(args)),
		"kwargs": h.peer.encode(mapOrEmpty(kwargs))})
}

func (h *host) RegisterCLICommand(cmd CLICommand) (*Handle, error) {
	if cmd.Run == nil {
		return nil, fmt.Errorf("CLI command %q has no run function", cmd.Name)
	}
	run := h.peer.funcRef(cmd.Name, func(ctx context.Context, args []any, kwargs map[string]any) (any, error) {
		var argv []string
		if len(args) > 0 {
			argv = stringsOf(args[0])
		}
		options, _ := kwargs["options"].(map[string]any)
		output, code, err := cmd.Run(ctx, argv, mapOrEmpty(options))
		if err != nil {
			return nil, err
		}
		return map[string]any{"output": output, "code": int64(code)}, nil
	})
	return h.register("register_cli_command", nil, map[string]any{"name": cmd.Name, "help": cmd.Help, "run": run, "description": cmd.Description})
}

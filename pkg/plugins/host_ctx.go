package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// One Python parameter of a plugin context method
type pyParam struct {
	name     string
	kwOnly   bool
	varKw    bool
	required bool
	def      any
}

func req(name string) pyParam          { return pyParam{name: name, required: true} }
func opt(name string, def any) pyParam { return pyParam{name: name, def: def} }
func kwo(name string, def any) pyParam { return pyParam{name: name, kwOnly: true, def: def} }
func kwReq(name string) pyParam        { return pyParam{name: name, kwOnly: true, required: true} }
func varKwargs() pyParam               { return pyParam{name: "kwargs", varKw: true} }
func pyNames(names []string) string    { return strings.Join(names, ", ") }
func pyQuoted(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = "'" + n + "'"
	}
	return out
}

// Python list wording of missing parameter names, a and b or a, b, and c
func pyMissingList(names []string) string {
	q := pyQuoted(names)
	switch len(q) {
	case 1:
		return q[0]
	case 2:
		return q[0] + " and " + q[1]
	}
	return pyNames(q[:len(q)-1]) + ", and " + q[len(q)-1]
}

// Binds a call the way Python binds arguments to a PluginContext method, raising its TypeError wording
func bindPy(method string, params []pyParam, args []any, kwargs map[string]any) (map[string]any, map[string]any, error) {
	fname := "PluginContext." + method + "()"
	typeErr := func(format string, a ...any) error {
		return &HostBuiltinError{Type: "TypeError", Message: fname + " " + fmt.Sprintf(format, a...)}
	}
	bound := map[string]any{}
	extra := map[string]any{}
	positional := []pyParam{}
	acceptsVarKw := false
	for _, p := range params {
		switch {
		case p.varKw:
			acceptsVarKw = true
		case !p.kwOnly:
			positional = append(positional, p)
		}
	}
	if len(args) > len(positional) {
		required := 0
		for _, p := range positional {
			if p.required {
				required++
			}
		}
		given := len(args) + 1
		if required == len(positional) {
			word := "arguments"
			if len(positional)+1 == 1 {
				word = "argument"
			}
			return nil, nil, typeErr("takes %d positional %s but %d were given", len(positional)+1, word, given)
		}
		return nil, nil, typeErr("takes from %d to %d positional arguments but %d were given", required+1, len(positional)+1, given)
	}
	for i, v := range args {
		bound[positional[i].name] = v
	}
	known := map[string]pyParam{}
	for _, p := range params {
		if !p.varKw {
			known[p.name] = p
		}
	}
	for _, k := range sortedKeys(kwargs) {
		if _, dup := bound[k]; dup {
			return nil, nil, typeErr("got multiple values for argument '%s'", k)
		}
		if _, ok := known[k]; ok {
			bound[k] = kwargs[k]
			continue
		}
		if !acceptsVarKw {
			return nil, nil, typeErr("got an unexpected keyword argument '%s'", k)
		}
		extra[k] = kwargs[k]
	}
	var missingPos, missingKw []string
	for _, p := range params {
		if p.varKw {
			continue
		}
		if _, ok := bound[p.name]; ok {
			continue
		}
		if p.required {
			if p.kwOnly {
				missingKw = append(missingKw, p.name)
			} else {
				missingPos = append(missingPos, p.name)
			}
			continue
		}
		bound[p.name] = p.def
	}
	if len(missingPos) > 0 {
		word := "argument"
		if len(missingPos) > 1 {
			word = "arguments"
		}
		return nil, nil, typeErr("missing %d required positional %s: %s", len(missingPos), word, pyMissingList(missingPos))
	}
	if len(missingKw) > 0 {
		word := "argument"
		if len(missingKw) > 1 {
			word = "arguments"
		}
		return nil, nil, typeErr("missing %d required keyword-only %s: %s", len(missingKw), word, pyMissingList(missingKw))
	}
	return bound, extra, nil
}

// Marker default of an optional parameter the call did not pass
type unsetDefault struct{}

var notPassed = unsetDefault{}

type hostCtxFunc func(ctx context.Context, h *PluginHost, c *Context, args []any, kwargs map[string]any) (any, error)

// One context method with its Python signature and the Go call it replays
func hostMethod(name string, params []pyParam, run func(ctx context.Context, h *PluginHost, c *Context, a map[string]any, extra map[string]any) (any, error)) hostCtxFunc {
	return func(ctx context.Context, h *PluginHost, c *Context, args []any, kwargs map[string]any) (any, error) {
		bound, extra, err := bindPy(name, params, args, kwargs)
		if err != nil {
			return nil, err
		}
		return run(ctx, h, c, bound, extra)
	}
}

func argStr(v any) string {
	if v == nil {
		return ""
	}
	return config.PyStr(v)
}

func argStrPtr(v any) *string {
	if v == nil {
		return nil
	}
	s := config.PyStr(v)
	return &s
}

func argMap(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case Record:
		return map[string]any(m)
	}
	return nil
}

func argStrings(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = config.PyStr(item)
	}
	return out
}

func argList(v any) []any {
	if items, ok := v.([]any); ok {
		return items
	}
	return nil
}

func argFloat(v any, def float64) float64 {
	if f, ok := pyFloat(v); ok {
		return f
	}
	return def
}

// Callable argument of a registrar, a TypeError naming the parameter when the plugin passed something else
func argCallable(method, param string, v any) (*HostedCallable, error) {
	f, ok := v.(*HostedCallable)
	if !ok {
		return nil, &HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf("%s() %s must be callable, got %s", method, param, pyTypeName(v))}
	}
	return f, nil
}

// Tool handler running a hosted handler(args, **kwargs) and normalizing its result like the registry
func hostedToolHandler(f *HostedCallable) tools.Handler {
	return func(ctx context.Context, call *tools.Call) string {
		args := call.Args
		if args == nil {
			args = map[string]any{}
		}
		kwargs := map[string]any{"task_id": nilIfEmpty(call.TaskID), "session_id": nilIfEmpty(call.SessionID)}
		if call.UserTask != "" {
			kwargs["user_task"] = call.UserTask
		}
		result, err := f.Call(ctx, []any{args}, kwargs)
		if err != nil {
			return pyJSONText(map[string]any{"error": fmt.Sprintf("Tool execution failed: %s: %s", pyErrorTypeName(err), err.Error())})
		}
		if s, ok := result.(string); ok {
			return s
		}
		return pyJSONText(result)
	}
}

// json.dumps with ensure_ascii off
func pyJSONText(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return config.PyStr(v)
	}
	return strings.TrimRight(b.String(), "\n")
}

func hostedHook(f *HostedCallable) HookFunc {
	return func(ctx context.Context, kwargs map[string]any) (any, error) {
		return f.Call(ctx, nil, signatureKwargs(f.Sig, kwargs))
	}
}

// Every Python method of the plugin context a hosted plugin reaches, replayed on the Go context
var hostCtxDispatch map[string]hostCtxFunc

func init() {
	providerRegistrar := func(name, param string, register func(c *Context, p any) (*Registration, error)) hostCtxFunc {
		return hostMethod(name, []pyParam{req(param)}, func(_ context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
			return register(c, a[param])
		})
	}
	hostCtxDispatch = map[string]hostCtxFunc{
		"has_plugin": hostMethod("has_plugin", []pyParam{req("plugin_id")}, func(_ context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
			return c.HasPlugin(argStr(a["plugin_id"])), nil
		}),
		"get_config": hostMethod("get_config", []pyParam{req("key"), opt("default", nil)}, func(ctx context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
			return c.GetConfig(ctx, argStr(a["key"]), a["default"])
		}),
		"set_config": hostMethod("set_config", []pyParam{req("key"), req("value")}, func(ctx context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
			return nil, c.SetConfig(ctx, argStr(a["key"]), a["value"])
		}),
		"current_cron_execution": hostMethod("current_cron_execution", nil, func(ctx context.Context, _ *PluginHost, c *Context, _, _ map[string]any) (any, error) {
			return c.CurrentCronExecution(ctx)
		}),
		"register_tool": hostMethod("register_tool", []pyParam{req("name"), req("toolset"), req("schema"), req("handler"),
			opt("check_fn", nil), opt("requires_env", nil), opt("is_async", false), opt("description", ""), opt("emoji", ""), opt("override", false)},
			func(ctx context.Context, h *PluginHost, c *Context, a, _ map[string]any) (any, error) {
				handler, err := argCallable("register_tool", "handler", a["handler"])
				if err != nil {
					return nil, err
				}
				spec := ToolSpec{Name: argStr(a["name"]), Toolset: argStr(a["toolset"]), Schema: argMap(a["schema"]),
					Handler: hostedToolHandler(handler), RequiresEnv: argStrings(a["requires_env"]),
					Description: argStr(a["description"]), Emoji: argStr(a["emoji"]), Override: config.PyTruthy(a["override"])}
				if check, ok := a["check_fn"].(*HostedCallable); ok {
					spec.Check = func(env *tools.Env) bool {
						v, err := check.Call(h.baseCtx, nil, nil)
						return err == nil && config.PyTruthy(v)
					}
				}
				return c.RegisterTool(ctx, spec)
			}),
		"call_mcp": hostMethod("call_mcp", []pyParam{req("server"), req("tool"), opt("arguments", nil), opt("timeout", int64(30))},
			func(ctx context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
				return c.CallMCP(ctx, argStr(a["server"]), argStr(a["tool"]), argMap(a["arguments"]), a["timeout"])
			}),
		"inject_message": hostMethod("inject_message", []pyParam{req("content"), opt("role", "user"), kwo("session_key", nil), kwo("origin", nil)},
			func(ctx context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
				return c.InjectMessage(ctx, argStr(a["content"]), argStr(a["role"]), argStr(a["session_key"]), a["origin"]), nil
			}),
		"session_cwd": hostMethod("session_cwd", nil, func(ctx context.Context, _ *PluginHost, c *Context, _, _ map[string]any) (any, error) {
			return c.SessionCwd(ctx)
		}),
		"session_surface": hostMethod("session_surface", nil, func(ctx context.Context, _ *PluginHost, c *Context, _, _ map[string]any) (any, error) {
			return c.SessionSurface(ctx)
		}),
		"ask_choice": hostMethod("ask_choice", []pyParam{req("question"), req("choices")}, func(ctx context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
			choice, ok, err := c.AskChoice(ctx, argStr(a["question"]), argList(a["choices"]))
			if err != nil || !ok {
				return nil, err
			}
			return choice, nil
		}),
		"register_command": hostMethod("register_command", []pyParam{req("name"), req("handler"), opt("description", ""), opt("args_hint", ""), opt("argument_mode", nil)},
			func(_ context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
				handler, err := argCallable("register_command", "handler", a["handler"])
				if err != nil {
					return nil, err
				}
				run := func(ctx context.Context, raw string) (any, error) { return handler.Call(ctx, []any{raw}, nil) }
				return c.RegisterCommand(argStr(a["name"]), run, argStr(a["description"]), argStr(a["args_hint"]), argStr(a["argument_mode"])), nil
			}),
		"dispatch_tool": hostMethod("dispatch_tool", []pyParam{req("tool_name"), req("args"), varKwargs()},
			func(ctx context.Context, _ *PluginHost, c *Context, a, extra map[string]any) (any, error) {
				return c.DispatchTool(ctx, argStr(a["tool_name"]), argMap(a["args"]), extra)
			}),
		"register_context_engine": providerRegistrar("register_context_engine", "engine", (*Context).RegisterContextEngine),
		"register_context_reference": hostMethod("register_context_reference", []pyParam{req("provider")}, func(_ context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
			return nil, c.RegisterContextReference(a["provider"])
		}),
		"register_memory_provider": hostMethod("register_memory_provider", []pyParam{req("provider")}, func(_ context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
			return nil, c.RegisterMemoryProvider(a["provider"])
		}),
		"register_auxiliary_task": hostMethod("register_auxiliary_task", []pyParam{req("key"), kwReq("display_name"), kwReq("description"), kwo("defaults", nil), kwo("inherit_from", nil)},
			func(_ context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
				return c.RegisterAuxiliaryTask(argStr(a["key"]), AuxTaskOptions{DisplayName: argStr(a["display_name"]), Description: argStr(a["description"]),
					Defaults: argMap(a["defaults"]), InheritFrom: argStrPtr(a["inherit_from"])})
			}),
		"register_redaction_patterns": hostMethod("register_redaction_patterns", []pyParam{req("patterns")}, func(_ context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
			return int64(c.RegisterRedactionPatterns(a["patterns"])), nil
		}),
		"register_locale": hostMethod("register_locale", []pyParam{req("lang"), req("source"), kwo("endonym", nil), kwo("rtl", false), kwo("surface", "core")},
			func(_ context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
				return c.RegisterLocale(argStr(a["lang"]), a["source"], argStr(a["endonym"]), config.PyTruthy(a["rtl"]), argStr(a["surface"]))
			}),
		"register_locale_dir": hostMethod("register_locale_dir", []pyParam{req("path"), kwo("metadata", nil)}, func(_ context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
			var meta map[string]map[string]any
			if m := argMap(a["metadata"]); m != nil {
				meta = map[string]map[string]any{}
				for k, v := range m {
					meta[k] = argMap(v)
				}
			}
			return c.RegisterLocaleDir(argStr(a["path"]), meta), nil
		}),
		"register_hook": hostMethod("register_hook", []pyParam{req("hook_name"), req("callback"), kwo("hold_stream", false)},
			func(_ context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
				cb, err := argCallable("register_hook", "callback", a["callback"])
				if err != nil {
					return nil, err
				}
				return c.manager.trackHookAs(c, argStr(a["hook_name"]), hostedHook(cb), config.PyTruthy(a["hold_stream"]), cb.Module, cb.Name), nil
			}),
		"register_middleware": hostMethod("register_middleware", []pyParam{req("kind"), req("callback")}, func(_ context.Context, h *PluginHost, c *Context, a, _ map[string]any) (any, error) {
			cb, err := argCallable("register_middleware", "callback", a["callback"])
			if err != nil {
				return nil, err
			}
			run := func(kwargs map[string]any) (any, error) { return cb.Call(h.baseCtx, nil, kwargs) }
			return c.registerMiddlewareAs(argStr(a["kind"]), run, cb.Name), nil
		}),
		"register_system_prompt_section": hostMethod("register_system_prompt_section", []pyParam{req("id"), req("content"), kwo("position", notPassed), kwo("max_chars", notPassed)},
			func(_ context.Context, h *PluginHost, c *Context, a, _ map[string]any) (any, error) {
				var opts []SectionOption
				if a["position"] != notPassed {
					opts = append(opts, WithSectionPosition(argStr(a["position"])))
				}
				if a["max_chars"] != notPassed {
					n, _ := wireInt(a["max_chars"])
					opts = append(opts, WithSectionMaxChars(int(n)))
				}
				content := a["content"]
				if cb, ok := content.(*HostedCallable); ok {
					content = func(sectionCtx map[string]any) (any, error) { return cb.Call(h.baseCtx, []any{sectionCtx}, nil) }
				}
				return c.RegisterSystemPromptSection(argStr(a["id"]), content, opts...)
			}),
		"emit": hostMethod("emit", []pyParam{req("event"), opt("payload", nil)}, func(ctx context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
			n, err := c.Emit(ctx, argStr(a["event"]), argMap(a["payload"]))
			return int64(n), err
		}),
		"subscribe": hostMethod("subscribe", []pyParam{req("event"), req("callback")}, func(_ context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
			cb, err := argCallable("subscribe", "callback", a["callback"])
			if err != nil {
				return nil, err
			}
			return nil, c.Subscribe(argStr(a["event"]), func(ctx context.Context, payload map[string]any) (any, error) {
				return cb.Call(ctx, nil, payload)
			})
		}),
		"register_skill": hostMethod("register_skill", []pyParam{req("name"), req("path"), opt("description", ""), opt("frontmatter", nil)},
			func(_ context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
				return c.RegisterSkill(argStr(a["name"]), argStr(a["path"]), argStr(a["description"]), argMap(a["frontmatter"]))
			}),
		"register_automation_blueprint": hostMethod("register_automation_blueprint", []pyParam{req("key"), kwReq("title"), kwReq("description"),
			kwReq("schedule_template"), kwReq("prompt_template"), kwo("category", "general"), kwo("slots", []any{}), kwo("deliver_default", "origin"),
			kwo("skills", []any{}), kwo("tags", []any{})},
			func(_ context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
				return c.RegisterAutomationBlueprint(argStr(a["key"]), BlueprintSpec{Title: argStr(a["title"]), Description: argStr(a["description"]),
					ScheduleTemplate: argStr(a["schedule_template"]), PromptTemplate: argStr(a["prompt_template"]), Category: argStr(a["category"]),
					Slots: argList(a["slots"]), DeliverDefault: argStr(a["deliver_default"]), Skills: argStrings(a["skills"]), Tags: argStrings(a["tags"])})
			}),
		"has_capability": hostMethod("has_capability", []pyParam{req("capability")}, func(ctx context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
			return c.HasCapability(ctx, argStr(a["capability"])), nil
		}),
		"side_question": hostMethod("side_question", []pyParam{req("token"), req("prompt"), kwo("timeout", 15.0)}, func(ctx context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
			answer, err := c.SideQuestion(ctx, argStr(a["token"]), argStr(a["prompt"]), argFloat(a["timeout"], 15.0))
			if err != nil || answer == nil {
				return nil, err
			}
			return *answer, nil
		}),
		"register_image_gen_provider":            providerRegistrar("register_image_gen_provider", "provider", (*Context).RegisterImageGenProvider),
		"register_video_gen_provider":            providerRegistrar("register_video_gen_provider", "provider", (*Context).RegisterVideoGenProvider),
		"register_web_search_provider":           providerRegistrar("register_web_search_provider", "provider", (*Context).RegisterWebSearchProvider),
		"register_browser_provider":              providerRegistrar("register_browser_provider", "provider", (*Context).RegisterBrowserProvider),
		"register_terminal_environment_provider": providerRegistrar("register_terminal_environment_provider", "provider", (*Context).RegisterTerminalEnvironmentProvider),
		"register_secret_source":                 providerRegistrar("register_secret_source", "source", (*Context).RegisterSecretSource),
		"register_tts_provider":                  providerRegistrar("register_tts_provider", "provider", (*Context).RegisterTTSProvider),
		"register_transcription_provider":        providerRegistrar("register_transcription_provider", "provider", (*Context).RegisterTranscriptionProvider),
		"register_delegation_provider":           providerRegistrar("register_delegation_provider", "provider", (*Context).RegisterDelegationProvider),
	}
}

// Python class name a parent side error reports to the child, plain Go errors answering RuntimeError
func pyErrorTypeName(err error) string {
	name := errorTypeName(err)
	if !strings.HasSuffix(name, "Error") && !strings.HasSuffix(name, "Unavailable") && !strings.HasSuffix(name, "Unsupported") &&
		!strings.HasSuffix(name, "Timeout") && !strings.HasSuffix(name, "Interrupt") || name == "wrapError" {
		return "RuntimeError"
	}
	return name
}

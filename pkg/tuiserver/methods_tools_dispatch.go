package tuiserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Registry command a typed name resolves to with its localized description, category and desktop subcommands
type ResolvedCommand struct {
	Name, Category     string
	Describe           func(ctx context.Context) string
	DesktopSubcommands []string
}

// Registry resolver of pkg/commands assigned at startup, false for a name no command or alias carries
var ResolveCommand func(name string) (ResolvedCommand, bool)

// Slash commands whose success republishes the session control snapshot
var sessionControlSlashes = map[string]bool{"goal": true, "heartbeat": true, "loop": true, "subgoal": true}

// One command.dispatch stage, a nil response falling through to the next stage
type dispatchStage func(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session, name, arg string) map[string]any

// Canonical registry name of a typed command, the name itself when no command matches
func resolveName(name string) string {
	if c, found := ResolveCommand(name); found {
		return c.Name
	}
	return name
}

// Text of a string param, a non string value raising like the str method the Python calls on it
func textParam(params map[string]any, key, method string) string {
	raw, present := params[key]
	if !present {
		return ""
	}
	text, isText := raw.(string)
	if !isText {
		panic(fmt.Errorf("'%s' object has no attribute '%s'", pyrepr.TypeName(raw), method))
	}
	return text
}

// Display only command.dispatch result
func execOut(rid any, output string) map[string]any {
	return ok(rid, map[string]any{"type": "exec", "output": output})
}

// Stdout and stderr of a captured run, the non empty parts newline joined and stripped
func joinedOutput(stdout, stderr string) string {
	var parts []string
	for _, p := range []string{stdout, stderr} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return pystr.Strip(strings.Join(parts, "\n"))
}

// Captured text run of a shell command line, UTF-8 decoded with replacement and no console window
func captureShellRun(ctx context.Context, command string, env map[string]string, timeout int) (*pysubprocess.Completed, string, string, error) {
	cmd, err := pysubprocess.ShellCommand(command)
	if err != nil {
		return nil, "", "", err
	}
	childEnv := make([]string, 0, len(env))
	for k, v := range env {
		childEnv = append(childEnv, k+"="+v)
	}
	argv := append([]string{cmd.Path}, cmd.Args[1:]...)
	res, err := pysubprocess.RunWith(ctx, argv, pysubprocess.Options{Env: childEnv, Timeout: time.Duration(timeout) * time.Second,
		Attr: tools.WindowsHideAttr()})
	if err != nil {
		return res, "", "", err
	}
	return res, pystr.TranslateNewlines(pystr.DecodeUTF8Replace(res.Stdout)), pystr.TranslateNewlines(pystr.DecodeUTF8Replace(res.Stderr)), nil
}

// Skill scope of the bound home with the logical session cwd, the scope project skills resolve against
func boundSkillScope(ctx context.Context) (*skills.Scope, error) {
	scope := skills.NewScope(home.Dir(ctx), nil)
	cwd, err := tools.ResolveAgentCwd(tools.ToolEnvFrom(ctx))
	if err != nil {
		return nil, err
	}
	scope.Cwd = cwd
	return scope, nil
}

// Task id of the session key, empty without a session
func sessionTaskID(session *tuigateway.Session) string {
	if session == nil {
		return ""
	}
	return strOr(session.Get("session_key"))
}

func init() {
	toolsDispatchRegistry.method("command.dispatch", commandDispatch)
}

var toolsDispatchRegistry = &handlerRegistry{}

func init() { toolsDispatchRegistry.install() }

// Routes a slash command through the quick, plugin, bundle, skill and built in stages in the session home
func commandDispatch(ctx context.Context, rid any, params map[string]any) map[string]any {
	name := resolveName(strings.TrimLeft(textParam(params, "name", "lstrip"), "/"))
	arg := strOr(params["arg"])
	sid := strOr(params["session_id"])
	session := tuigateway.SessionRecord(sid)
	stages := []dispatchStage{dispatchQuick, dispatchPlugin, dispatchBundle, dispatchSkill}
	if builtin := slashBuiltins[name]; builtin != nil {
		stages = append(stages, builtin)
	}
	scoped, err := sessionHomeScope(session, "", "")(ctx)
	panicIf(err)
	for _, stage := range stages {
		res := stage(scoped, rid, params, session, name, arg)
		if res == nil {
			continue
		}
		if _, failed := res["error"]; sessionControlSlashes[name] && !failed {
			publishSessionControlSnapshot(scoped, sid, session, false, false)
		}
		return res
	}
	return rpcErr(rid, 4018, "not a quick/plugin/bundle/skill command: "+name, nil)
}

// Quick command of the config, an exec run in a sanitized shell or an alias directive
func dispatchQuick(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session, name, arg string) map[string]any {
	quick, _ := loadCfg(ctx)["quick_commands"].(map[string]any)
	qc, present := quick[name]
	if !present || qc == nil {
		return nil
	}
	entry, isDict := qc.(map[string]any)
	if !isDict {
		panic(fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(qc)))
	}
	if entry["type"] == "exec" {
		env, err := tools.BuildSubprocessEnv(ctx, tools.ToolEnvFrom(ctx), tools.SubprocessEnvOptions{InheritProfileHome: true, ScrubSecrets: true})
		panicIf(err)
		command := ""
		if raw, has := entry["command"]; has {
			command = pyrepr.Str(raw)
		}
		res, stdout, stderr, err := captureShellRun(ctx, command, env, 30)
		panicIf(err)
		output := pystr.Slice(joinedOutput(stdout, stderr), 0, 4000)
		if output != "" {
			output = tools.RedactSensitiveText(output)
		}
		if res.Returncode != 0 {
			if output == "" {
				output = fmt.Sprintf("quick command failed with exit code %d", res.Returncode)
			}
			return rpcErr(rid, 4018, output, nil)
		}
		return execOut(rid, output)
	}
	if entry["type"] == "alias" {
		target, has := entry["target"]
		if !has {
			target = ""
		}
		return ok(rid, map[string]any{"type": "alias", "target": target})
	}
	return nil
}

// Plugin slash handler of a name, false when none is registered or the lookup failed
func pluginCommandHandler(ctx context.Context, name string) (handler plugins.PluginCommandHandler, found bool) {
	if catchPanic(func() { handler, found = plugins.GetPluginCommandHandler(ctx, name) }) != nil {
		return nil, false
	}
	return handler, found
}

// Runs a plugin slash handler under the session binding with clarify backed choices
func runPluginCommand(ctx context.Context, handler plugins.PluginCommandHandler, arg string, session *tuigateway.Session, sid string) (string, error) {
	key := ""
	if session != nil {
		key = strOr(session.Get("session_key"))
	}
	bound := ctx
	if session != nil {
		cwd := strOr(session.Get("cwd"))
		var env *tools.Env
		bound, env = setSessionContext(ctx, key, &cwd, "")
		defer clearSessionContext(env)
	}
	sessionID := ""
	if session != nil {
		sessionID = key
		if a := sessionAgent(session); a != nil && a.SessionID() != "" {
			sessionID = a.SessionID()
		}
	}
	invocation := map[string]any{"source": nil, "session_key": key, "platform": sessionSource(session), "session_id": sessionID}
	bound = plugins.BindAsker(bound, plugins.ClarifyAsker(func(questions []any) map[string]any {
		return clarifyBlock(bound, sid, questionMaps(questions))
	}))
	result, err := plugins.InvokePluginCommand(bound, handler, arg, invocation)
	if err != nil {
		return "", err
	}
	resolved, err := plugins.ResolvePluginCommandResult(bound, result)
	if err != nil {
		return "", err
	}
	if !truthy(resolved) {
		return "", nil
	}
	return pyrepr.Str(resolved), nil
}

// Clarify question payloads as dicts, non dict entries dropped
func questionMaps(questions []any) []map[string]any {
	out := make([]map[string]any, 0, len(questions))
	for _, q := range questions {
		if m, isDict := q.(map[string]any); isDict {
			out = append(out, m)
		}
	}
	return out
}

// Plugin command stage, a failing handler falling through to the next stage
func dispatchPlugin(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session, name, arg string) map[string]any {
	handler, found := pluginCommandHandler(ctx, name)
	if !found {
		return nil
	}
	var res map[string]any
	if catchPanic(func() {
		output, err := runPluginCommand(ctx, handler, arg, session, strOr(params["session_id"]))
		panicIf(err)
		res = ok(rid, map[string]any{"type": "plugin", "output": output})
	}) != nil {
		return nil
	}
	return res
}

// Skill bundle key of a name that is not a registry command, false otherwise or on failure
func bundleKeyFor(ctx context.Context, name string) (key string, found bool) {
	if catchPanic(func() {
		if _, isCommand := ResolveCommand(name); isCommand {
			return
		}
		scope, err := boundSkillScope(ctx)
		panicIf(err)
		key, found, err = scope.ResolveBundleCommandKey(name)
		panicIf(err)
	}) != nil {
		return "", false
	}
	return key, found
}

// Skill bundle stage, the expanded bundle sent as the next turn with its loading notice
func dispatchBundle(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session, name, arg string) map[string]any {
	bundleKey, found := bundleKeyFor(ctx, name)
	if !found {
		return nil
	}
	scope, err := boundSkillScope(ctx)
	panicIf(err)
	msg, loaded, missing, built, err := scope.BuildBundleInvocationMessage(ctx, bundleKey, arg, sessionTaskID(session), resolveSessionPlatform())
	if err != nil {
		return rpcErr(rid, 4018, "bundle dispatch failed: "+errorMessage(err), nil)
	}
	if !built {
		return rpcErr(rid, 4018, "failed to load bundle: "+bundleKey, nil)
	}
	bundles, err := scope.GetSkillBundles()
	panicIf(err)
	bundleName := any(strings.TrimLeft(bundleKey, "/"))
	if entry, has := bundles[bundleKey]; has {
		if named, hasName := entry["name"]; hasName {
			bundleName = named
		}
	}
	notice := fmt.Sprintf("⚡ Loading bundle: %s (%d skills)", pyrepr.Str(bundleName), len(loaded))
	if len(missing) > 0 {
		notice += "\nSkipped missing skills: " + strings.Join(missing, ", ")
	}
	return ok(rid, map[string]any{"type": "send", "message": msg, "notice": notice, "display": tuigateway.SkillScaffoldProjection(msg)})
}

// Skill command stage, stacked leading skill tokens loaded together over the remaining instruction
func dispatchSkill(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session, name, arg string) map[string]any {
	var res map[string]any
	catchPanic(func() {
		scope, err := boundSkillScope(ctx)
		panicIf(err)
		cmds, err := scope.GetInteractiveSkillCommands(ctx)
		panicIf(err)
		key := strings.ToLower("/" + name)
		entry, has := cmds[key]
		if !has {
			return
		}
		skillName := any(name)
		if named, hasName := entry["name"]; hasName {
			skillName = named
		}
		extraKeys, userInstruction, err := scope.SplitStackedSkillCommands(ctx, arg, true)
		panicIf(err)
		if len(extraKeys) > 0 {
			msg, loaded, missing, built, err := scope.BuildStackedSkillInvocationMessage(ctx, append([]string{key}, extraKeys...), userInstruction, sessionTaskID(session))
			panicIf(err)
			if built {
				notice := fmt.Sprintf("⚡ Loading %d stacked skills: %s", len(loaded), strings.Join(loaded, ", "))
				if len(missing) > 0 {
					notice += "\nSkipped missing skills: " + strings.Join(missing, ", ")
				}
				res = ok(rid, map[string]any{"type": "skill", "message": msg, "name": skillName, "notice": notice,
					"display": tuigateway.SkillScaffoldProjection(msg)})
				return
			}
		}
		msg, _, err := scope.BuildSkillInvocationMessage(ctx, key, userInstruction, sessionTaskID(session), "")
		panicIf(err)
		if msg != "" {
			res = ok(rid, map[string]any{"type": "skill", "message": msg, "name": skillName, "display": tuigateway.SkillScaffoldProjection(msg)})
		}
	})
	return res
}

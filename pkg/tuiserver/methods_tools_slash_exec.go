package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// CLI commands that queue onto pending input, routed to command.dispatch since the slash worker cannot read them
var pendingInputCommands = map[string]bool{"retry": true, "queue": true, "q": true, "steer": true, "plan": true, "goal": true,
	"loop": true, "proactive": true, "moa": true, "undo": true, "learn": true, "init": true, "compress": true, "compact": true}

// Commands the slash worker must not run when they restore live state
var workerBlockedCommands = map[string]bool{"snapshot": true, "snap": true}

// Error text of a skill command the slash worker refused before processing it
const skillWorkerRefused = "skill command refused before process: /"

// Slash worker surface slash.exec drives
type slashCommandRunner interface {
	run(command string) (string, error)
}

// Slash worker that composes a next turn prompt for the caller to send
type slashSeedPopper interface {
	popSeed() string
}

var toolsSlashExecRegistry = &handlerRegistry{}

func init() {
	toolsSlashExecRegistry.method("slash.exec", slashExec)
	toolsSlashExecRegistry.method("shell.exec", shellExec)
	toolsSlashExecRegistry.install()
}

// Whether a base is a skill of the session profile, an error when the skill scan failed
func profileSkillCommand(ctx context.Context, session *tuigateway.Session, base string) (hit bool, scanErr any) {
	scanErr = catchPanic(func() {
		scoped, err := sessionHomeScope(session, "", "")(ctx)
		panicIf(err)
		scope, err := boundSkillScope(scoped)
		panicIf(err)
		cmds, err := scope.GetInteractiveSkillCommands(scoped)
		panicIf(err)
		_, hit = cmds["/"+base]
	})
	return hit, scanErr
}

// Directive of command.dispatch for a skill, else a hard error, never an ok banner
func skillDispatchOrRefuse(ctx context.Context, rid any, sid, base, arg string) map[string]any {
	dispatched := lookupMethod("command.dispatch")(ctx, rid, map[string]any{"name": base, "arg": arg, "session_id": sid})
	if _, failed := dispatched["error"]; failed || resultType(dispatched) {
		return dispatched
	}
	return rpcErr(rid, 4018, "skill command: use command.dispatch for /"+base, nil)
}

// Whether a response result carries a truthy type
func resultType(resp map[string]any) bool {
	result, _ := resp["result"].(map[string]any)
	return truthy(result["type"])
}

// Whether a worker failure is its pre process refusal of a skill command
func workerRefusedSkill(err error) bool {
	return strings.Contains(err.Error(), skillWorkerRefused)
}

// Whether a base is a registry built in the slash worker may still run
func isRegistryCommand(base string) (found bool) {
	if catchPanic(func() { _, found = ResolveCommand(base) }) != nil {
		return false
	}
	return found
}

// Session slash worker, spawned once under the session spawn lock on first use
func sessionSlashWorker(ctx context.Context, sid string, session *tuigateway.Session) (any, error) {
	if worker := session.Get("slash_worker"); truthy(worker) {
		return worker, nil
	}
	sessionsLock.Lock()
	spawnLock, _ := session.SetDefault("_slash_spawn_lock", &sync.Mutex{}).(sync.Locker)
	sessionsLock.Unlock()
	spawnLock.Lock()
	defer spawnLock.Unlock()
	if worker := session.Get("slash_worker"); truthy(worker) {
		return worker, nil
	}
	var worker any
	failure := catchPanic(func() {
		sessionKey, hasKey := session.Lookup("session_key")
		if !hasKey {
			panic(fmt.Errorf("'session_key'"))
		}
		model, providerName := resolveModel(ctx), ""
		if a := sessionAgent(session); a != nil {
			model, providerName = a.Model(), a.Provider()
		}
		spawned, err := newSlashWorker(ctx, strOr(sessionKey), model, strOr(session.Get("profile_home")), providerName)
		panicIf(err)
		attachWorker(sid, session, spawned)
		worker = spawned
	})
	if failure != nil {
		return nil, errors.New(panicMessage(failure))
	}
	return worker, nil
}

// Runs a slash command through live output, dispatch routing, skills, plugins or the session slash worker
func slashExec(ctx context.Context, rid any, params map[string]any) map[string]any {
	session, errResp := sessNowait(ctx, params, rid)
	if errResp != nil {
		return errResp
	}
	cmd := pystr.Strip(textParam(params, "command", "strip"))
	if cmd == "" {
		return rpcErr(rid, 4004, "empty command", nil)
	}
	parts := pystr.SplitWhitespace(strings.TrimLeft(cmd, "/"), 1)
	base, arg := "", ""
	if len(parts) > 0 {
		base = strings.ToLower(parts[0])
	}
	if len(parts) > 1 {
		arg = parts[1]
	}
	sid := strOr(params["session_id"])
	if live, found := liveSlashCommandOutput(ctx, sid, session, base, arg); found {
		if live == "" {
			live = "(no output)"
		}
		return ok(rid, map[string]any{"output": live})
	}
	if workerBlockedCommands[base] && isSnapshotRestore(arg) {
		return rpcErr(rid, 4018, "snapshot restore mutates live config/state; use command.dispatch for /snapshot restore", nil)
	}
	target, routed := base, pendingInputCommands[base]
	if !routed {
		scoped, err := sessionHomeScope(session, "", "")(ctx)
		panicIf(err)
		target, routed = bundleKeyFor(scoped, base)
	}
	if routed {
		return lookupMethod("command.dispatch")(ctx, rid, map[string]any{"name": strings.TrimLeft(target, "/"), "arg": arg, "session_id": sid})
	}
	hit, scanErr := profileSkillCommand(ctx, session, base)
	if scanErr == nil && hit {
		return rpcErr(rid, 4018, "skill command: use command.dispatch for /"+base, nil)
	}
	if scanErr != nil {
		dispatched := skillDispatchOrRefuse(ctx, rid, sid, base, arg)
		if resultType(dispatched) || !isRegistryCommand(base) {
			return dispatched
		}
	}
	if base != "" {
		if handler, found := pluginCommandHandler(ctx, base); found {
			var output string
			failure := catchPanic(func() {
				out, err := runPluginCommand(ctx, handler, arg, session, sid)
				panicIf(err)
				output = out
			})
			if failure != nil {
				return ok(rid, map[string]any{"output": "Plugin command error: " + panicMessage(failure)})
			}
			if output == "" {
				output = "(no output)"
			}
			return ok(rid, map[string]any{"output": output})
		}
	}
	worker, err := sessionSlashWorker(ctx, sid, session)
	if err != nil {
		return rpcErr(rid, 5030, "slash worker start failed: "+err.Error(), nil)
	}
	runner, isRunner := worker.(slashCommandRunner)
	if !isRunner {
		closeSlashWorker(worker)
		session.Set("slash_worker", nil)
		return rpcErr(rid, 5030, fmt.Sprintf("'%s' object has no attribute 'run'", pyrepr.TypeName(worker)), nil)
	}
	output, err := runner.run(cmd)
	if err != nil {
		if workerRefusedSkill(err) {
			return skillDispatchOrRefuse(ctx, rid, sid, base, arg)
		}
		closeSlashWorker(worker)
		session.Set("slash_worker", nil)
		return rpcErr(rid, 5030, err.Error(), nil)
	}
	if popper, hasSeed := worker.(slashSeedPopper); hasSeed {
		if seed := popper.popSeed(); seed != "" {
			return ok(rid, map[string]any{"type": "send", "message": seed})
		}
	}
	if output == "" {
		output = "(no output)"
	}
	payload := map[string]any{"output": output}
	if warning := mirrorSlashSideEffects(ctx, sid, session, cmd); warning != "" {
		payload["warning"] = warning
	}
	if sessionControlSlashes[base] {
		publishSessionControlSnapshot(ctx, sid, session, false, false)
	}
	return ok(rid, payload)
}

// Runs a user typed shell command with a sanitized env, refusing hardline and dangerous commands
func shellExec(ctx context.Context, rid any, params map[string]any) map[string]any {
	raw, present := params["command"]
	if !present {
		raw = ""
	}
	if !truthy(raw) {
		return rpcErr(rid, 4004, "empty command", nil)
	}
	cmd, isText := raw.(string)
	if !isText {
		panic(fmt.Errorf("expected string or bytes-like object, got '%s'", pyrepr.TypeName(raw)))
	}
	if hardline, desc := tools.DetectHardlineCommand(cmd); hardline {
		return rpcErr(rid, 4005, fmt.Sprintf("blocked (hardline): %s. Use the agent for dangerous commands.", desc), nil)
	}
	if _, desc, dangerous := tools.DetectDangerousCommand(cmd); dangerous {
		return rpcErr(rid, 4005, fmt.Sprintf("blocked: %s. Use the agent for dangerous commands.", desc), nil)
	}
	env, err := tools.BuildSubprocessEnv(ctx, tools.ToolEnvFrom(ctx), tools.SubprocessEnvOptions{InheritProfileHome: true, ScrubSecrets: true})
	panicIf(err)
	res, stdout, stderr, err := captureShellRun(ctx, cmd, env, 30)
	var expired *pysubprocess.TimeoutExpired
	if errors.As(err, &expired) {
		return rpcErr(rid, 5002, "command timed out (30s)", nil)
	}
	if err != nil {
		return rpcErr(rid, 5003, err.Error(), nil)
	}
	redact := func(text string) string {
		return memory.RedactSensitiveText(text, memory.RedactOptions{RedactURLCredentials: true})
	}
	return ok(rid, map[string]any{"stdout": pystr.Slice(redact(stdout), -4000, math.MaxInt),
		"stderr": pystr.Slice(redact(stderr), -2000, math.MaxInt), "code": res.Returncode})
}

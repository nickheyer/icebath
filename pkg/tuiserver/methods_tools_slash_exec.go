package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Commands that queue onto pending input, routed straight to command.dispatch ahead of the skill and plugin stages
var pendingInputCommands = map[string]bool{"retry": true, "queue": true, "q": true, "steer": true, "plan": true, "goal": true,
	"loop": true, "proactive": true, "moa": true, "undo": true, "learn": true, "init": true, "compress": true, "compact": true}

// Snapshot command words, their restore form refused here since it mutates live config and state
var snapshotCommands = map[string]bool{"snapshot": true, "snap": true}

// Whether a base is a skill of the session profile, an error when the skill scan failed
func profileSkillCommand(ctx context.Context, session *tuihost.Session, base string) (hit bool, scanErr any) {
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

// Whether a base is a registry built in, a name no skill slug can carry
func isRegistryCommand(base string) (found bool) {
	if catchPanic(func() { _, found = ResolveCommand(base) }) != nil {
		return false
	}
	return found
}

// Runs a slash command through live output, dispatch routing, skills and plugins, handing every other name to command.dispatch
func slashExec(ctx context.Context, rid any, params map[string]any) map[string]any {
	session, errResp := sessNowait(ctx, params, rid)
	if errResp != nil {
		return errResp
	}
	cmd := pystr.Strip(textParam(params, "command"))
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
		return ok(rid, tuigateway.Plain(tuigateway.SlashExecResult{Output: live}))
	}
	if snapshotCommands[base] && isSnapshotRestore(arg) {
		return rpcErr(rid, 4018, "snapshot restore mutates live config/state; use command.dispatch for /snapshot restore", nil)
	}
	target, routed := base, pendingInputCommands[base]
	if !routed {
		scoped, err := sessionHomeScope(session, "", "")(ctx)
		panicIf(err)
		target, routed = bundleKeyFor(scoped, base)
	}
	if _, bound := gateway.ResolveBoundCommand(base); bound && !routed {
		target, routed = base, true
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
				return ok(rid, tuigateway.Plain(tuigateway.SlashExecResult{Output: "Plugin command error: " + panicMessage(failure)}))
			}
			if output == "" {
				output = "(no output)"
			}
			return ok(rid, tuigateway.Plain(tuigateway.SlashExecResult{Output: output}))
		}
	}
	return lookupMethod("command.dispatch")(ctx, rid, map[string]any{"name": base, "arg": arg, "session_id": sid})
}

// Runs a user typed shell command with a sanitized env in the named session's cwd and profile, refusing hardline and dangerous commands
func shellExec(ctx context.Context, rid any, params map[string]any) map[string]any {
	p, err := tuigateway.ShellExec.Params(params)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil)
	}
	if pystr.Strip(p.Command) == "" {
		return rpcErr(rid, 4004, "empty command", nil)
	}
	if hardline, desc := tools.DetectHardlineCommand(p.Command); hardline {
		return rpcErr(rid, 4005, fmt.Sprintf("blocked (hardline): %s. Use the agent for dangerous commands.", desc), nil)
	}
	if _, desc, dangerous := tools.DetectDangerousCommand(p.Command); dangerous {
		return rpcErr(rid, 4005, fmt.Sprintf("blocked: %s. Use the agent for dangerous commands.", desc), nil)
	}
	scoped, dir := ctx, ""
	if p.SessionID != "" {
		session, errResp := sessNowait(ctx, params, rid)
		if errResp != nil {
			return errResp
		}
		scoped, dir = sessionProfileRuntimeScope(ctx, session, true), sessionCwd(ctx, session)
	}
	env, err := tools.BuildSubprocessEnv(scoped, tools.ToolEnvFrom(scoped), tools.SubprocessEnvOptions{InheritProfileHome: true, ScrubSecrets: true})
	panicIf(err)
	res, stdout, stderr, err := captureShellRun(scoped, p.Command, env, dir, 30)
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
	return ok(rid, tuigateway.Plain(tuigateway.ShellExecResult{Stdout: pystr.Slice(redact(stdout), -4000, math.MaxInt),
		Stderr: pystr.Slice(redact(stderr), -2000, math.MaxInt), Code: int64(res.Returncode)}))
}

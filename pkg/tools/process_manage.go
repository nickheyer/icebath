package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// The process_manage tool, poll, wait, log, kill, stdin and handoff of terminal background processes

// ProcessSchema is the exact process_manage function schema
var ProcessSchema = map[string]any{
	"name": "process_manage",
	"description": "Poll, wait on, send input to, or kill terminal(background=true) processes. " +
		"Completed results remain retrievable by session_id when resuming their owning conversation " +
		"(up to 7 days, newest 64 results per profile; rolling output tail). " +
		"poll: status + new output. log: full output, paged. wait: block " +
		"until exit or timeout (partial output on timeout). write vs " +
		"submit: submit appends Enter — use it to answer prompts; write " +
		"sends raw bytes, no newline. close: EOF stdin. kill: terminate. " +
		"handoff (subagents only): transfer a running process you started to your parent agent, which then " +
		"receives its completion; `data` = one sentence on its purpose. Subagent-owned processes are otherwise " +
		"killed when the subagent finishes and their notifications never reach the parent.",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type": "string",
				"enum": []any{"list", "poll", "log", "wait", "kill", "write", "submit", "close", "handoff"},
			},
			"session_id": map[string]any{
				"type":        "string",
				"description": "From terminal background output; any unique prefix works ('4dae' for proc_4dae56ca81f6). Required except for 'list'.",
			},
			"data": map[string]any{
				"type":        "string",
				"description": "Stdin text for write/submit; purpose sentence for handoff.",
			},
			"timeout": map[string]any{
				"type":        "integer",
				"description": "Max seconds for 'wait'.",
				"minimum":     1,
			},
			"offset": map[string]any{
				"type":        "integer",
				"description": "Log line offset (default: last 200).",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Max log lines.",
				"minimum":     1,
			},
		},
		"required": []any{"action"},
	},
}

// Runs the plugin transform_terminal_output seam over background process output, nil returncode while running
func transformProcessOutput(env *Env, output, command string, returncode *int, taskID string) string {
	if TerminalOutputTransformHook == nil {
		return output
	}
	callID, _ := loadSessionString(env, "approval.tool_call_id")
	var code any
	if returncode != nil {
		code = *returncode
	}
	results := TerminalOutputTransformHook(map[string]any{
		"command": command, "output": output, "returncode": code, "task_id": taskID,
		"env_type": "", "tool_call_id": callID,
	})
	for _, r := range results {
		if s, ok := r.(string); ok {
			return s
		}
	}
	return output
}

// Transforms, then redacts secrets from background process output and its command before it reaches the model
func redactProcessResult(env *Env, result map[string]any) map[string]any {
	if result == nil {
		return result
	}
	command := asStr(result["command"])
	taskID := asStr(result["task_id"])
	if taskID == "" {
		if s := processRegistry.Get(env, asStr(result["session_id"])); s != nil {
			taskID = s.TaskID
		}
	}
	var returncode *int
	if code, ok := asIntValue(result["exit_code"]); ok {
		returncode = &code
	}
	for _, key := range []string{"output", "output_preview"} {
		if value, ok := result[key].(string); ok && value != "" {
			value = transformProcessOutput(env, value, command, returncode, taskID)
			result[key] = RedactTerminalCommandOutput(env, value, command, false)
		}
	}
	if command != "" {
		result["command"] = RedactSensitiveTextWith(env, command, RedactOptions{CodeFile: true})
	}
	return result
}

// Lists the task's processes plus the session scoped ones sharing the gateway session key
func listProcesses(ctx context.Context, env *Env, taskID string) *pyjson.Object {
	sessionKey := getCurrentSessionKey(env, "")
	processes := []*pyjson.Object{}
	for _, p := range processRegistry.listSessionDicts(WithEnvHome(ctx, env), env, taskID, sessionKey, true) {
		redactProcessResult(env, p.Vals)
		processes = append(processes, p)
	}
	return Dict("processes", processes)
}

// Session actions with their handler and whether the result carries output to redact
type processAction struct {
	run    func(ctx context.Context, env *Env, sid string, args map[string]any) *pyjson.Object
	redact bool
}

var sessionActions = map[string]processAction{
	"poll": {func(ctx context.Context, env *Env, sid string, a map[string]any) *pyjson.Object {
		return processRegistry.pollDict(WithEnvHome(ctx, env), env, sid)
	}, true},
	"log": {func(ctx context.Context, env *Env, sid string, a map[string]any) *pyjson.Object {
		var offset *int
		if n, ok := asIntValue(a["offset"]); ok && a["offset"] != nil {
			offset = &n
		}
		limit := 200
		if n, ok := asIntValue(a["limit"]); ok && a["limit"] != nil {
			limit = n
		}
		return processRegistry.readLogDict(env, sid, offset, limit)
	}, true},
	"wait": {func(ctx context.Context, env *Env, sid string, a map[string]any) *pyjson.Object {
		var timeout *int
		if n, ok := asIntValue(a["timeout"]); ok && a["timeout"] != nil {
			timeout = &n
		}
		return processRegistry.waitDict(ctx, env, sid, timeout)
	}, true},
	"kill": {func(ctx context.Context, env *Env, sid string, a map[string]any) *pyjson.Object {
		return processRegistry.killDict(WithEnvHome(ctx, env), env, sid, "process.kill", true)
	}, true},
	"write": {func(ctx context.Context, env *Env, sid string, a map[string]any) *pyjson.Object {
		return processRegistry.writeStdinDict(env, sid, pyStrOrEmpty(a["data"]))
	}, false},
	"submit": {func(ctx context.Context, env *Env, sid string, a map[string]any) *pyjson.Object {
		return processRegistry.submitStdinDict(env, sid, pyStrOrEmpty(a["data"]))
	}, false},
	"close": {func(ctx context.Context, env *Env, sid string, a map[string]any) *pyjson.Object {
		return processRegistry.closeStdinDict(env, sid)
	}, false},
}

// Handoffs one subagent may make before it must wait on or kill the rest
const maxHandoffsPerChild = 3

// Transfers a running background process from a subagent to its parent agent
func handoffProcess(env *Env, sessionID string, args map[string]any, taskID string) *pyjson.Object {
	var child SubagentHandoff
	if SubagentHandoffResolver != nil {
		child = SubagentHandoffResolver(taskID)
	}
	if child == nil || child.ParentSessionID() == "" && child.ParentOwner() == "" {
		return Dict("error", "handoff is only available to a running subagent with a live parent; you are not one.")
	}
	parentOwner := child.ParentOwner()
	if parentOwner == "" {
		return Dict("error", "parent has no process owner id yet; retry after the parent's turn has started.")
	}
	if len(child.HandedOff()) >= maxHandoffsPerChild {
		return Dict("error", fmt.Sprintf("handoff cap reached (%d per subagent); wait on or kill the rest yourself.", maxHandoffsPerChild))
	}
	note := strings.TrimSpace(pyStrOrEmpty(args["data"]))
	if note == "" {
		return Dict("error", "handoff requires `data`: one sentence saying what the process is for and what the parent should do with its result.")
	}
	toTaskID, err := resolveContainerTaskID(env, parentOwner)
	if err != nil {
		return Dict("error", err.Error())
	}
	s := processRegistry.TransferOwnership(env, sessionID, taskID, parentOwner, toTaskID, child.ParentSessionID(), note)
	if s == nil {
		return Dict("error", fmt.Sprintf("cannot hand off %s: not a running process you own (already exited? read its result with poll/log and report it instead).", sessionID))
	}
	child.AppendHandoff(map[string]any{"session_id": s.ID, "command": s.Command, "note": note})
	return Dict(
		"status", "handed_off", "session_id", s.ID, "command", s.Command,
		"note", "Your parent now owns this process and will receive its completion; you will not. Mention the handoff in your final answer.",
	)
}

// JSON without HTML escaping, the ensure_ascii false dump the model reads
func processJSON(v any) string {
	return JSON(v)
}

// Dispatches one process_manage call
func handleProcess(ctx context.Context, call *Call) string {
	args := call.Args
	env := call.Env
	action := asStr(args["action"])
	sessionID := ""
	if v, ok := args["session_id"]; ok && v != nil {
		sessionID = pyStr(v)
	}
	if action == "list" {
		return processJSON(listProcesses(WithEnvHome(ctx, env), env, call.TaskID))
	}
	if action == "handoff" {
		if sessionID == "" {
			return toolErrorJSON("session_id is required for handoff", nil)
		}
		return processJSON(handoffProcess(env, sessionID, args, call.TaskID))
	}
	if entry, ok := sessionActions[action]; ok {
		if sessionID == "" {
			return toolErrorJSON(fmt.Sprintf("session_id is required for %s", action), nil)
		}
		result := applyWaitPollPolicy(ctx, env, action, sessionID, entry.run(ctx, env, sessionID, args))
		if entry.redact {
			redactProcessResult(env, result.Vals)
		}
		return processJSON(result)
	}
	return toolErrorJSON(fmt.Sprintf("Unknown process action: %s. Use: list, poll, log, wait, kill, write, submit, close, handoff", action), nil)
}

func init() {
	Register(Def{
		Name:    "process_manage",
		Toolset: "terminal",
		Schema:  ProcessSchema,
		Handler: handleProcess,
		Emoji:   "⚙️",
	})
}

package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/internal/tool"
)

const (
	ToolName            = "process_manage"
	maxHandoffsPerChild = 3
	defaultLogLimit     = 200
)

const toolDescription = "Poll, wait on, send input to, or kill terminal(background=true) processes. " +
	"Completed results remain retrievable by session_id when resuming their owning conversation " +
	"(up to 7 days, newest 64 results per profile; rolling output tail). " +
	"poll: status + new output. log: full output, paged. wait: block " +
	"until exit or timeout (partial output on timeout). write vs " +
	"submit: submit appends Enter — use it to answer prompts; write " +
	"sends raw bytes, no newline. close: EOF stdin. kill: terminate. " +
	"handoff (subagents only): transfer a running process you started to your parent agent, which then " +
	"receives its completion; `data` = one sentence on its purpose. Subagent-owned processes are otherwise " +
	"killed when the subagent finishes and their notifications never reach the parent."

const toolParameters = `{"type": "object", "properties": {` +
	`"action": {"type": "string", "enum": ["list", "poll", "log", "wait", "kill", "write", "submit", "close", "handoff"]}, ` +
	`"session_id": {"type": "string", "description": "From terminal background output; any unique prefix works ('4dae' for proc_4dae56ca81f6). Required except for 'list'."}, ` +
	`"data": {"type": "string", "description": "Stdin text for write/submit; purpose sentence for handoff."}, ` +
	`"timeout": {"type": "integer", "description": "Max seconds for 'wait'.", "minimum": 1}, ` +
	`"offset": {"type": "integer", "description": "Log line offset (default: last 200)."}, ` +
	`"limit": {"type": "integer", "description": "Max log lines.", "minimum": 1}` +
	`}, "required": ["action"]}`

func Tools(r *Registry) []tool.Tool {
	return []tool.Tool{{
		Name:        ToolName,
		Toolset:     "terminal",
		Description: toolDescription,
		Parameters:  json.RawMessage(toolParameters),
		Handler:     r.handle,
		Emoji:       "⚙️",
		Effects:     tool.Mutating,
	}}
}

func argText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	}
	return fmt.Sprint(v)
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case bool:
		return x
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

func intArg(args tool.Args, key string, def *int) (*int, error) {
	v, ok := args[key]
	if !ok {
		return def, nil
	}
	n, isNumber := v.(json.Number)
	if !isNumber {
		if v == nil && def == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("%s must be an integer, got %s", key, argText(v))
	}
	i, err := n.Int64()
	if err != nil {
		return nil, fmt.Errorf("%s must be an integer, got %s", key, n.String())
	}
	out := int(i)
	return &out, nil
}

func floatArg(args tool.Args, key string) (*float64, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return nil, nil
	}
	n, isNumber := v.(json.Number)
	if !isNumber {
		return nil, fmt.Errorf("%s must be a number, got %s", key, argText(v))
	}
	f, err := n.Float64()
	if err != nil {
		return nil, err
	}
	return &f, nil
}

var errNoLimit = errors.New("limit must be an integer")

func (r *Registry) handle(ctx context.Context, c *tool.Call, args tool.Args) (tool.Result, error) {
	action := ""
	if v, ok := args["action"]; ok {
		action = argText(v)
	}
	sid := ""
	if v, ok := args["session_id"]; ok && v != nil {
		sid = argText(v)
	}
	caller := CallerOf(c)
	switch action {
	case "list":
		entries := r.List(ctx, caller, ListOptions{TaskID: caller.TaskID, SessionKey: caller.SessionKey, IncludeRetained: true})
		for _, e := range entries {
			r.redactReply(ctx, caller, e)
		}
		return tool.JSON(newReply("processes", entries)), nil
	case "handoff":
		if sid == "" {
			return tool.Failure("session_id is required for handoff"), nil
		}
		return tool.JSON(r.handoff(ctx, c, sid, args)), nil
	case "poll", "log", "wait", "kill", "write", "submit", "close":
	default:
		return tool.Failure("Unknown process action: " + action + ". Use: list, poll, log, wait, kill, write, submit, close, handoff"), nil
	}
	if sid == "" {
		return tool.Failure("session_id is required for " + action), nil
	}
	data := ""
	if v, ok := args["data"]; ok {
		data = argText(v)
	}
	var res *Reply
	redacted := true
	switch action {
	case "poll":
		res = r.Poll(ctx, caller, sid)
	case "log":
		offset, err := intArg(args, "offset", nil)
		if err != nil {
			return tool.Result{}, err
		}
		def := defaultLogLimit
		limit, err := intArg(args, "limit", &def)
		if err != nil {
			return tool.Result{}, err
		}
		if limit == nil {
			return tool.Result{}, errNoLimit
		}
		res = r.ReadLog(ctx, caller, sid, offset, *limit)
	case "wait":
		timeout, err := floatArg(args, "timeout")
		if err != nil {
			return tool.Result{}, err
		}
		if res, err = r.Wait(ctx, caller, sid, timeout); err != nil {
			return tool.Result{}, err
		}
	case "kill":
		res = r.Kill(ctx, caller, sid, KillOptions{Source: SourceProcessKill, Consume: true})
	case "write":
		res, redacted = r.WriteStdin(ctx, caller, sid, data), false
	case "submit":
		res, redacted = r.SubmitStdin(ctx, caller, sid, data), false
	case "close":
		res, redacted = r.CloseStdin(ctx, caller, sid), false
	}
	res, err := r.applyWaitPolicy(ctx, c, action, sid, res)
	if err != nil {
		return tool.Result{}, err
	}
	if redacted {
		r.redactReply(ctx, caller, res)
	}
	return tool.JSON(res), nil
}

func (r *Registry) handoff(ctx context.Context, c *tool.Call, sid string, args tool.Args) *Reply {
	parent, ok := r.delegation.HandoffParent(ctx, c.TaskID)
	if !ok {
		return newReply("error", "handoff is only available to a running subagent with a live parent; you are not one.")
	}
	if parent.Owner == "" {
		return newReply("error", "parent has no process owner id yet; retry after the parent's turn has started.")
	}
	if parent.Handed >= maxHandoffsPerChild {
		return newReply("error", fmt.Sprintf("handoff cap reached (%d per subagent); wait on or kill the rest yourself.",
			maxHandoffsPerChild))
	}
	note := ""
	if v := args["data"]; truthy(v) {
		note = strings.TrimSpace(argText(v))
	}
	if note == "" {
		return newReply("error", "handoff requires `data`: one sentence saying what the process is for and what "+
			"the parent should do with its result.")
	}
	s := r.TransferOwnership(ctx, sid, c.TaskID, parent.Owner, parent.TaskID, parent.SessionKey, note)
	if s == nil {
		return newReply("error", "cannot hand off "+sid+": not a running process you own (already exited? read its result "+
			"with poll/log and report it instead).")
	}
	r.delegation.RecordHandoff(c.TaskID, HandedOff{SessionID: s.id, Command: s.command, Note: note})
	return newReply("status", "handed_off", "session_id", s.id, "command", s.command,
		"note", "Your parent now owns this process and will receive its completion; you will not. Mention the handoff "+
			"in your final answer.")
}

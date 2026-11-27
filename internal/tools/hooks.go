package tools

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/nickheyer/icebath/internal/hooks"
)

// Hook points of the tools' host, nil for a registry of no host
func (r *Registry) hooking() *hooks.Hooks {
	if r.host == nil {
		return nil
	}
	return r.host.hooks
}

// Keeps a turn that changed code going for the pre_verify hooks, which may send the agent back to check its work
type verifying struct {
	hooks *hooks.Hooks
}

func (verifying) Tools() []Def { return nil }

func (v verifying) Continue(ctx context.Context, env *Env, ending Ending) string {
	if !v.hooks.Has(env.Config, hooks.PreVerify) {
		return ""
	}
	return v.hooks.PreVerify(ctx, env.Config, hooks.Verify{Session: ending.Session, Platform: ending.Platform, Model: ending.Model, Coding: ending.Coding,
		Attempt: ending.Attempt, Final: ending.Final, Changed: ending.Changed})
}

// Runs the call's tool between the hooks set on tool calls: pre_tool_call may block it, lay arguments over its own or hold it for the user's
// approval; post_tool_call hears what came of it, and transform_tool_result may replace the result before anything else reads it
func (r *Registry) hooked(ctx context.Context, def Def, call *Call, handle func(ctx context.Context) string) string {
	h, cfg := r.hooking(), call.Env.Config
	turn := hooks.TurnOf(ctx)
	ids := func() hooks.Call {
		args := maps.Clone(call.Args)
		if args == nil {
			args = map[string]any{}
		}
		return hooks.Call{Tool: call.Name, Args: args, Task: cmp.Or(turn.Task, call.SessionID), Session: call.SessionID, ID: call.ToolCallID,
			Turn: turn.ID, Request: turn.Request}
	}
	if h.Has(cfg, hooks.PreToolCall) {
		d := h.PreToolCall(ctx, cfg, hooks.ToolCall{Call: ids(), Trace: []any{}})
		if d.Args != nil {
			raw, err := json.Marshal(d.Args)
			if err != nil {
				return ErrorResult(err)
			}
			call.RawArgs = string(raw)
			if d.Block != "" {
				// A block refuses the call as the hooks rewrote it, whatever its rewritten arguments would make of it
				call.Args = d.Args
			} else if err := parse(def, call); err != nil {
				return ErrorResult(err)
			}
		}
		block := d.Block
		if block == "" && d.Approve != nil {
			block = approval(ctx, def, call, *d.Approve)
		}
		if block != "" {
			result := ErrorResult(errors.New(block))
			if h.Has(cfg, hooks.PostToolCall) {
				done := hooks.Outcome{Call: ids(), Result: result, Status: "blocked", ErrorType: hooks.Optional("plugin_block"), ErrorMessage: &block}
				h.Fire(ctx, cfg, hooks.ToolDone{Outcome: done, Trace: []any{}})
			}
			return result
		}
	}
	started := time.Now()
	result := handle(hooks.WithCall(ctx, call.ToolCallID))
	if !h.Has(cfg, hooks.PostToolCall) && !h.Has(cfg, hooks.TransformToolResult) {
		return result
	}
	done := hooks.Outcome{Call: ids(), Result: result, Duration: time.Since(started).Milliseconds(), Status: "ok"}
	if broke, how := Broke(call.Name, result); broke || Failed(result) {
		done.Status, done.ErrorType = "error", hooks.Optional("tool_error")
		if message := cmp.Or(ErrorText(result), how); message != "" {
			done.ErrorMessage = &message
		}
	}
	h.Fire(ctx, cfg, hooks.ToolDone{Outcome: done, Trace: []any{}})
	if text, ok := h.TransformResult(ctx, cfg, hooks.ToolResult{Outcome: done}); ok {
		return text
	}
	return result
}

// Asks the user to allow a call a plugin holds for approval, the answer remembered under the plugin's rule: why it may not run, empty when
// it may
func approval(ctx context.Context, def Def, call *Call, ask hooks.Ask) string {
	name := call.Name
	description := cmp.Or(ask.Message, "a plugin asks before "+def.Label()+" runs")
	subject := fmt.Sprintf("Tool '%s' requires approval (%s)", name, description)
	nobody := "BLOCKED: " + subject + " but no interactive user or gateway is present to approve it. A plugin flagged this action for human confirmation."
	if call.Env.Gate == nil {
		return nobody
	}
	err := call.Env.Gate(ctx, Gate{Key: "plugin_rule:" + cmp.Or(ask.Rule, name), Description: description, Target: def.Label() + " tool, held for your approval by a plugin",
		Subject: subject, Advice: "Find an alternative approach.", Unanswered: nobody})
	if err == nil {
		return ""
	}
	return cmp.Or(err.Error(), "BLOCKED: plugin approval required for "+name)
}

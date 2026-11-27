package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	terminalconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/terminal/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
)

var processSchema = tools.Schema(
	"Poll, wait on, send input to, or kill terminal(background=true) processes. Completed results remain retrievable by session_id when "+
		"resuming their owning conversation (up to 7 days, newest 64 results per profile; rolling output tail). poll: status + new output. "+
		"log: full output, paged. wait: block until exit or timeout (partial output on timeout). write vs submit: submit appends Enter — use "+
		"it to answer prompts; write sends raw bytes, no newline. close: EOF stdin. kill: terminate. list: every process of this conversation. "+
		"handoff (subagents only): transfer a running process you started to your parent agent, which then receives its completion; `data` = "+
		"one sentence on its purpose. Subagent-owned processes are otherwise killed when the subagent finishes and their notifications never "+
		"reach the parent.",
	[]string{"action"},
	map[string]any{
		"action": map[string]any{"type": "string", "enum": []string{"list", "poll", "log", "wait", "kill", "write", "submit", "close", "handoff"}},
		"session_id": tools.Property("string", "From terminal background output; any unique prefix works ('4dae' for proc_4dae56ca81f6). Required "+
			"except for 'list'."),
		"data":    tools.Property("string", "Stdin text for write/submit; purpose sentence for handoff."),
		"timeout": map[string]any{"type": "integer", "minimum": 1, "description": "Max seconds for 'wait'."},
		"offset":  tools.Property("integer", "Log line offset (default: last 200)."),
		"limit":   map[string]any{"type": "integer", "minimum": 1, "description": "Max log lines."},
	},
)

// What a wait or poll on a quiet process adds once its completion notice is turned on
const quietNote = "The process is still running and has printed nothing new since your last check. icebath has turned on notify_on_complete " +
	"for it: when it exits, a message with its exit code and final output starts a new turn automatically, even after you end this turn. Do " +
	"NOT call process_manage wait or poll on %s again, and do not sleep or loop to wait for it. If you have other work, do it now; otherwise end " +
	"your turn with a one-line status and report the result when the completion message arrives."

// Optional whole number argument
func number(args map[string]any, name string) *int {
	if n, ok := config.Whole(args[name]); ok {
		return &n
	}
	return nil
}

// Polls, waits on, reads, writes to and kills background processes of the session
func (r *registrant) process(ctx context.Context, call *tools.Call) string {
	args, env, cfg, jobs := call.Args, call.Env, call.Env.Config, r.host.Jobs
	jobs.Remember(call.SessionID, deliver(env))
	r.host.Bind(cfg, call.SessionID, env.Client)
	action, _ := args["action"].(string)
	id := ""
	if raw, ok := args["session_id"]; ok && raw != nil {
		id = fmt.Sprint(raw)
	}
	if action == "list" {
		reports := nonNil(jobs.List(cfg, call.SessionID, true))
		for _, report := range reports {
			r.redactReport(ctx, call, report)
		}
		return encode(map[string]any{"processes": reports})
	}
	var run func() *terminal.Report
	data, _ := args["data"].(string)
	switch action {
	case "poll":
		run = func() *terminal.Report { return jobs.Poll(cfg, call.SessionID, id) }
	case "log":
		limit := 200
		if n := number(args, "limit"); n != nil {
			limit = *n
		}
		run = func() *terminal.Report { return jobs.Log(cfg, call.SessionID, id, number(args, "offset"), limit) }
	case "wait":
		run = func() *terminal.Report {
			return jobs.Wait(ctx, cfg, call.SessionID, id, number(args, "timeout"), env.Yield)
		}
	case "kill":
		run = func() *terminal.Report { return jobs.Kill(ctx, cfg, call.SessionID, id, "process.kill", true) }
	case "write":
		run = func() *terminal.Report { return jobs.Write(cfg, call.SessionID, id, data) }
	case "submit":
		run = func() *terminal.Report { return jobs.Submit(cfg, call.SessionID, id, data) }
	case "close":
		run = func() *terminal.Report { return jobs.EOF(cfg, call.SessionID, id) }
	case "handoff":
		if id == "" {
			return tools.ErrorResult(errors.New("session_id is required for handoff"))
		}
		return encode(r.handOver(call, id, data))
	default:
		return tools.ErrorResult(fmt.Errorf("Unknown process action: %s. Use: list, poll, log, wait, kill, write, submit, close, handoff", action))
	}
	if id == "" {
		return tools.ErrorResult(errors.New("session_id is required for " + action))
	}
	report := run()
	if action == "wait" || action == "poll" {
		report = r.quiet(ctx, call, action, id, report)
	}
	switch action {
	case "poll", "log", "wait", "kill":
		r.redactReport(ctx, call, report)
	}
	return encode(report)
}

// Background processes one subagent may hand over before it must wait on or kill the rest
const maxHandoffs = 3

// Gives a running process of a subagent's session to the agent that delegated to it, which hears of its end from then on
func (r *registrant) handOver(call *tools.Call, id, note string) map[string]any {
	child := call.Env.Delegated
	if child == nil || child.Parent == "" {
		return map[string]any{"error": "handoff is only available to a running subagent with a live parent; you are not one."}
	}
	if note = strings.TrimSpace(note); note == "" {
		return map[string]any{"error": "handoff requires `data`: one sentence saying what the process is for and what the parent should do with its result."}
	}
	j := r.host.Jobs.Get(call.Env.Config, call.SessionID, id)
	if j == nil || j.Session != call.SessionID || j.Exited() {
		return map[string]any{"error": fmt.Sprintf("cannot hand off %s: not a running process you own (already exited? read its result with poll/log and "+
			"report it instead).", id)}
	}
	if child.Handoffs() >= maxHandoffs {
		return map[string]any{"error": fmt.Sprintf("handoff cap reached (%d per subagent); wait on or kill the rest yourself.", maxHandoffs)}
	}
	var send terminal.Deliver
	if child.Notify != nil {
		send = func(text, title string) error { return child.Notify(tools.Notice{Text: text, Title: title}) }
	}
	if r.host.Jobs.Transfer(call.Env.Config, call.SessionID, j.ID, child.Parent, send, note) == nil {
		return map[string]any{"error": fmt.Sprintf("cannot hand off %s: not a running process you own (already exited? read its result with poll/log and "+
			"report it instead).", id)}
	}
	child.Handoff(j.ID, j.Command, note)
	return map[string]any{"status": "handed_off", "session_id": j.ID, "command": terminal.RedactCommand(call.Env.Config, j.Command, false),
		"note": "Your parent now owns this process and will receive its completion; you will not. Mention the handoff in your final answer."}
}

// Hands the output a report gives the model through the transform_terminal_output hooks, its exit code nil while the process runs, then
// masks the secrets of the output and the command, as the profile hides them
func (r *registrant) redactReport(ctx context.Context, call *tools.Call, report *terminal.Report) {
	cfg := call.Env.Config
	job := r.host.Jobs.Get(cfg, call.SessionID, report.SessionID)
	for _, output := range []*string{report.Output, report.OutputPreview} {
		if output != nil {
			*output = r.host.Jobs.Transform(ctx, cfg, job, *output, report.Command, report.ExitCode, call.ToolCallID)
			*output = terminal.Redact(cfg, *output, report.Command, false)
		}
	}
	report.Command = terminal.RedactCommand(cfg, report.Command, false)
}

func nonNil(reports []*terminal.Report) []*terminal.Report {
	if reports == nil {
		return []*terminal.Report{}
	}
	return reports
}

// Turns on the completion notice of a running process waited on or polled twice in a row with nothing new, telling the model to stop
// waiting on it
func (r *registrant) quiet(ctx context.Context, call *tools.Call, action, id string, report *terminal.Report) *terminal.Report {
	cfg, jobs := call.Env.Config, r.host.Jobs
	if !cfg.GetBool(terminalconfig.WaitNotify) {
		return report
	}
	j := jobs.Get(cfg, call.SessionID, id)
	if j == nil {
		return report
	}
	running := action == "wait" && report.Status == "timeout" || action == "poll" && report.Status == "running"
	if !running {
		if report.Status != "interrupted" {
			jobs.Unquiet(j.ID)
		}
		return report
	}
	if j.Exited() || !jobs.Quiet(j) || call.Env.Notify == nil || !jobs.Allowed(cfg) || j.Session != call.SessionID {
		return report
	}
	if !j.Notifies() && !jobs.Promote(j) {
		jobs.Unquiet(j.ID)
		if action == "poll" {
			return jobs.Poll(cfg, call.SessionID, id)
		}
		return jobs.Wait(ctx, cfg, call.SessionID, id, ptr(5), call.Env.Yield)
	}
	note := fmt.Sprintf(quietNote, j.ID)
	report.Notify = true
	if action == "wait" {
		clamp := ""
		if strings.HasPrefix(report.TimeoutNote, "Requested wait") {
			clamp = strings.SplitN(report.TimeoutNote, ". Wait window", 2)[0] + ". "
		}
		report.TimeoutNote = clamp + note
	} else {
		report.Note = note
	}
	return report
}

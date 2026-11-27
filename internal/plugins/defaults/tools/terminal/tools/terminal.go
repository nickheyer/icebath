package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	terminalconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/terminal/config"
	"github.com/nickheyer/icebath/internal/terminal"
	hostconfig "github.com/nickheyer/icebath/internal/terminal/config"
	"github.com/nickheyer/icebath/internal/tools"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
)

const description = `Run shell commands on the OS/shell/backend named in your environment section. The working directory, files and exported variables persist between calls: activate a virtualenv or export once, not before every command.

Use read_file, search_files, patch and write_file instead of cat/head/tail, grep/find/ls, sed/awk and echo/heredoc. Output is auto-truncated with the full text saved to a file; never pipe through head/tail to shorten it.

Foreground (default) returns as soon as the command finishes. To wait a fixed time, run the wait in the foreground; never background a sleep or polling loop. Background is only for commands that must keep running after this call returns; health-check a server in a separate call and manage jobs with process_manage. When a command changes directory, trust the result's "cwd" instead of prefixing later commands with cd.
`

// Longest the spill files of truncated output are kept
const spillAge = 7 * 24 * time.Hour

// Retries of a command whose run failed inside icebath, not in the command
const retries = 3

// Model visible schema of the terminal tool, the ceiling of the profile in use quoted
func (r *registrant) terminalSchema() map[string]any {
	ceiling := r.config.GetInt(terminalconfig.MaxForeground)
	return tools.Schema(description, []string{"command"}, map[string]any{
		"command":    tools.Property("string", "The shell command to execute"),
		"background": map[string]any{"type": "boolean", "default": false, "description": "Keep running after this call returns; returns a session_id."},
		"timeout": map[string]any{"type": "integer", "minimum": 1, "description": fmt.Sprintf("Max seconds (default %d, foreground max %d). Finishing "+
			"early returns at once, so set it generously. Above %ds the command runs as a tracked background process with notify_on_complete=true "+
			"(the result says so; do not re-run it).", r.config.GetInt(hostconfig.Timeout), ceiling, ceiling)},
		"workdir": tools.Property("string", "Absolute working directory for this command only."),
		"pty": map[string]any{"type": "boolean", "default": false, "description": "With background=true: pseudo-terminal for interactive CLIs (REPLs, " +
			"coding agents); drive it with process_manage write/submit. Local backend only."},
		"notify": map[string]any{
			"description": "With background=true. true: one notification on exit; use it for anything that ends (builds, tests, deploys), or the job " +
				"runs silently. ['pattern', ...]: notify when a line matches, only for one-shot readiness on processes that never exit; rate-limited. " +
				"Omit for servers/daemons.",
			"anyOf": []any{map[string]any{"type": "boolean"}, map[string]any{"type": "array", "items": map[string]any{"type": "string"}}},
		},
		"heartbeat": map[string]any{"type": "integer", "minimum": 0, "default": 0, "description": "With background=true: also notify every N seconds " +
			"(min 60) with new output; quiet ticks are skipped. For bounded jobs you must react to mid-run, never servers. Implies notify=true; 0 = off."},
		"persist_on_release": map[string]any{"type": "boolean", "default": false, "description": "With background=true: survive session end, /new, " +
			"compression and other cleanup (not host exit). Only for jobs the user explicitly wants to outlive the conversation."},
	})
}

// One terminal call, its arguments read
type request struct {
	command    string
	background bool
	timeout    *int
	workdir    string
	pty        bool
	notify     bool
	watch      []string
	heartbeat  int
	persist    bool
}

// Kind of a JSON value as an error names it
func kind(v any) string {
	switch v.(type) {
	case nil:
		return "nothing"
	case bool:
		return "a boolean"
	case float64:
		return "a number"
	case []any:
		return "a list"
	case map[string]any:
		return "an object"
	}
	return fmt.Sprintf("%T", v)
}

// Reads the arguments, mapping notify onto completion and pattern notices and refusing the options a foreground call can't take
func parse(args map[string]any) (request, string) {
	var req request
	if _, ok := args["command"]; !ok {
		if _, code := args["code"]; code {
			return req, "terminal received a 'code' parameter, but it requires a shell command in 'command'. Use execute_code(code=...) for " +
				"Python; for shell, retry as terminal(command=...)."
		}
	}
	command, ok := args["command"].(string)
	if !ok {
		return req, fmt.Sprintf("Invalid command: expected text, got %s", kind(args["command"]))
	}
	req.command = command
	notify, hasNotify := args["notify"]
	req.notify = config.Truthy(args["notify_on_complete"])
	if raw, ok := args["watch_patterns"].([]any); ok {
		for _, p := range raw {
			req.watch = append(req.watch, fmt.Sprint(p))
		}
	}
	if raw, ok := args["heartbeat"]; ok && raw != nil {
		n, isWhole := config.Whole(raw)
		if _, isBool := raw.(bool); isBool || !isWhole || n < 0 {
			return req, "heartbeat must be a whole number of seconds (0 disables; positive values are clamped to min 60)."
		}
		req.heartbeat = n
	}
	req.persist = config.Truthy(args["persist_on_release"])
	req.background = config.Truthy(args["background"])
	req.pty = config.Truthy(args["pty"])
	if !req.background {
		switch {
		case hasNotify && config.Truthy(notify) || len(req.watch) > 0 || req.notify:
			return req, "notify only applies to background commands (foreground results return directly). Either drop it, or run as " +
				"terminal(command=..., background=true, notify=...)."
		case req.pty:
			return req, "pty requires background=true (a PTY session is interacted with via process_manage(action='write'/'submit'), which " +
				"needs a tracked background process). Retry as terminal(command=..., background=true, pty=true)."
		case req.persist:
			return req, "persist_on_release only applies to background commands (a foreground process is awaited inline and has nothing to " +
				"persist). Retry as terminal(command=..., background=true, persist_on_release=true)."
		}
		req.heartbeat = 0
	}
	if hasNotify && notify != nil {
		switch n := notify.(type) {
		case bool:
			req.notify, req.watch = n, nil
		case []any:
			req.notify, req.watch = false, nil
			for _, p := range n {
				req.watch = append(req.watch, fmt.Sprint(p))
			}
		default:
			return req, "notify must be true/false (notify on exit) or a list of strings (notify on output pattern match)."
		}
	}
	if req.heartbeat != 0 {
		req.notify = true
	}
	if raw, ok := args["timeout"]; ok && raw != nil {
		if n, ok := config.Whole(raw); ok {
			req.timeout = &n
		}
	}
	req.workdir, _ = args["workdir"].(string)
	return req, ""
}

// Runs a command in the environment of the session's backend, in the foreground or as a tracked background process
func (r *registrant) terminal(ctx context.Context, call *tools.Call) (out string) {
	defer func() {
		if p := recover(); p != nil {
			err, ok := p.(error)
			if !ok {
				err = fmt.Errorf("%v", p)
			}
			out = fatal(err)
		}
	}()
	req, refusal := parse(call.Args)
	if refusal != "" {
		if strings.HasPrefix(refusal, "Invalid command") {
			return failure(refusal, -1, "error")
		}
		return tools.ErrorResult(errors.New(refusal))
	}
	out, err := r.run(ctx, call, req)
	if err == nil {
		return out
	}
	if missing, ok := errors.AsType[*terminal.MissingError](err); ok {
		return failure(terminal.RedactError(fmt.Sprintf("Terminal tool disabled: environment creation failed (%s)", missing)), -1, "disabled")
	}
	return fatal(err)
}

// What the model is told after the terminal tool itself failed
const fatalHint = "The terminal tool failed inside icebath, so the command may not have run. Fix what the error names (the terminal backend, its " +
	"sandbox image or the command arguments) and call terminal again. If the same error comes back, stop retrying and tell the user, whose " +
	"icebath log holds the details."

func fatal(err error) string {
	slog.Error("terminal_tool exception", "error", err)
	text := terminal.RedactError(fmt.Sprintf("Failed to execute command: %s", err))
	return encode(result{ExitCode: ptr(-1), Error: &text, RetryHint: fatalHint, Status: "error"})
}

// Result of a call that ran nothing
func failure(text string, code int, status string) string {
	return encode(result{ExitCode: &code, Error: &text, Status: status})
}

func ptr[T any](v T) *T { return &v }

func (r *registrant) run(ctx context.Context, call *tools.Call, req request) (string, error) {
	env, cfg := call.Env, call.Env.Config
	r.host.Jobs.Remember(call.SessionID, deliver(env))
	r.host.Bind(cfg, call.SessionID, env.Client)
	if req.timeout != nil && *req.timeout <= 0 {
		return tools.ErrorResult(fmt.Errorf("timeout must be a positive number of seconds (got %d).", *req.timeout)), nil
	}
	command, background, auto, promoted := req.command, req.background, false, 0
	if !background {
		if stripped := trailingAmp(cfg, command); stripped != "" {
			command, background, auto = stripped, true, true
		} else if guidance := foregroundGuidance(command); guidance != "" {
			return failure(guidance, -1, "error"), nil
		} else if req.timeout != nil && *req.timeout > cfg.GetInt(terminalconfig.MaxForeground) {
			promoted = *req.timeout
		}
	}
	timeout := time.Duration(cfg.GetInt(hostconfig.Timeout)) * time.Second
	if req.timeout != nil {
		timeout = time.Duration(*req.timeout) * time.Second
	}
	lease, err := r.host.Acquire(ctx, cfg, call.SessionID, env.Cwd)
	if err != nil {
		if broken, ok := errors.AsType[*terminal.ConnectionError](err); ok {
			return degraded(cfg, nil, broken), nil
		}
		return "", err
	}
	defer lease.Release()
	if refusal := r.guard(ctx, call, lease, req, command, timeout); refusal != "" {
		return refusal, nil
	}
	refusal, note := r.approve(ctx, call, lease, command)
	if refusal != "" {
		return refusal, nil
	}
	ptyOff := req.pty && pipeStdin(command)
	notify, watch := req.notify, req.watch
	if promoted != 0 {
		background, notify, watch = true, true, nil
	}
	if background {
		out := r.background(ctx, call, lease, req, command, notify, watch, note, ptyOff, auto && promoted == 0)
		if promoted != 0 {
			out = promote(out, promoted, cfg.GetInt(terminalconfig.MaxForeground))
		}
		return out, nil
	}
	return r.foreground(ctx, call, lease, req, command, timeout, note), nil
}

const ptyOffNote = "PTY disabled for this command because it expects piped stdin/EOF (for example gh auth login --with-token). For local background " +
	"processes, call process_manage(action='close') after writing so it receives EOF."

// Result when the backend could not be reached: degraded and evicted, or the error envelope when the profile wants failures to stop
func degraded(cfg *config.Config, lease *terminal.Lease, broken *terminal.ConnectionError) string {
	if strings.ToLower(strings.TrimSpace(cfg.GetString(hostconfig.Degraded))) == hostconfig.DegradedFail {
		return fatal(broken)
	}
	slog.Warn("terminal backend degraded", "reason", broken.Reason)
	if lease != nil {
		lease.Evict()
	}
	text := "Terminal backend degraded: " + broken.Reason
	return encode(result{ExitCode: ptr(-1), Error: &text, Status: "degraded", Reason: broken.Reason, RetryHint: broken.Hint})
}

// Runs the command and waits for it, retrying a run that failed inside icebath, handing it to the background when the user asks
func (r *registrant) foreground(ctx context.Context, call *tools.Call, lease *terminal.Lease, req request, command string, timeout time.Duration, note string) string {
	cfg, env := call.Env.Config, call.Env
	run := command
	if runtime.GOOS == "darwin" && !lease.Backend.Remote {
		run = macosOpen(run)
	}
	run, stdin := r.sudo(ctx, call, lease, run)
	limit := cfg.GetInt(toolsconfig.OutputMaxBytes)
	if limit <= 0 {
		limit = 50_000
	}
	var res *terminal.Result
	cwd := ""
	for attempt := 0; ; attempt++ {
		cwd = lease.Folder(req.workdir)
		exec := terminal.Exec{Cwd: cwd, Timeout: timeout, Stdin: stdin, Limit: limit, Spill: r.spill(cfg), Yield: env.Yield, Env: tools.SessionVars(call)}
		if _, local := lease.Env.(terminal.Spawner); local {
			exec.Handoff = r.handoff(call, command, cwd)
		}
		var err error
		if res, err = lease.Env.Execute(ctx, run, exec); err == nil {
			break
		}
		if broken, ok := errors.AsType[*terminal.ConnectionError](err); ok {
			return degraded(cfg, lease, broken)
		}
		if strings.Contains(strings.ToLower(err.Error()), "timeout") {
			return failure(fmt.Sprintf("Command timed out after %d seconds", int(timeout.Seconds())), 124, "")
		}
		if attempt >= retries {
			slog.Error("Execution failed after retries", "retries", retries, "command", command, "error", err, "backend", lease.Backend.Name)
			return failure(terminal.RedactError(fmt.Sprintf("Command execution failed: %T: %s", err, err)), -1, "")
		}
		wait := time.Duration(1<<(attempt+1)) * time.Second
		slog.Warn("Execution error, retrying", "wait", wait, "attempt", attempt+1, "max", retries, "command", command, "error", err)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
	}
	if res.Yielded != "" {
		return encode(result{Output: res.Output, Status: "yielded_to_background", SessionID: res.Yielded, Pid: res.Pid, Notify: ptr(true), Note: yieldedNote})
	}
	return r.finish(ctx, call, lease, req, command, cwd, note, res)
}

// Note of a foreground command moved to the background because the user sent a message
const yieldedNote = "The user sent a message while this command was running, so it was moved to the background WITHOUT being killed and is still " +
	"running. You will be notified when it exits (notify_on_complete). Read the user's message and respond to it now; use " +
	"process_manage(action='poll'|'wait'|'log', session_id=...) to check on this command."

// Takes the still running command into the background, notified of its end when the session can be told
func (r *registrant) handoff(call *tools.Call, command, cwd string) func(terminal.Proc, string) *terminal.Result {
	return func(p terminal.Proc, soFar string) *terminal.Result {
		host, ok := p.(*terminal.HostProc)
		if !ok {
			slog.Warn("foreground command could not be handed to the background", "handle", fmt.Sprintf("%T", p))
			return nil
		}
		send := deliver(call.Env)
		job := r.host.Jobs.Adopt(terminal.Launch{Config: call.Env.Config, Session: call.SessionID, Command: command, Cwd: cwd, Deliver: send,
			Notify: send != nil && r.host.Jobs.Allowed(call.Env.Config)}, host, soFar)
		slog.Info("foreground command yielded to background", "session_id", job.ID, "pid", job.Pid)
		return &terminal.Result{Output: soFar, Yielded: job.ID, Pid: job.Pid}
	}
}

// File the output of a command that overflows is copied to, the folder's files older than a week pruned once
func (r *registrant) spill(cfg *config.Config) string {
	dir := filepath.Join(cfg.ProfileDir(cfg.Profile()), "cache", "terminal-output")
	if _, done := r.pruned.LoadOrStore(dir, true); !done {
		old, _ := filepath.Glob(filepath.Join(dir, "out-*.log"))
		for _, path := range old {
			if st, err := os.Stat(path); err == nil && time.Since(st.ModTime()) > spillAge {
				os.Remove(path)
			}
		}
	}
	return filepath.Join(dir, fmt.Sprintf("out-%d-%d-%s.log", time.Now().Unix(), os.Getpid(), strconv.FormatInt(time.Now().UnixNano()&0xffff, 16)))
}

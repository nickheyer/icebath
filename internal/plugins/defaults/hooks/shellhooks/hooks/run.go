package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"math"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/hooks"
	profileconfig "github.com/nickheyer/icebath/internal/profile/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// Exit code with which a hook blocks the action it was asked about
	BlockExit = 2
	// Said when a hook blocks without saying why
	defaultBlock = "A shell hook blocked this call."
	// Characters of a hook's error output kept for the log and a block's message
	stderrLimit = 400
	notDirect   = "can't run by itself on Windows. Start it with its interpreter, such as bash"
)

// Fields of an event written at the top of a hook's input rather than under extra
var topLevel = strs.Set("tool_name", "args", "session_id", "parent_session_id")

// The two ways a hook may phrase its answer to pre_tool_call: action, message, then the other key a block's message may be under, and the
// key of the arguments of a modify
var dialects = [][4]string{{"action", "message", "reason", "args"}, {"decision", "reason", "message", "tool_input"}}

// How one run of a hook's command ended
type Run struct {
	// Exit code, nil when the command never ran to its end
	Code           *int
	Stdout, Stderr string
	TimedOut       bool
	Seconds        float64
	// Why the command could not run, empty when it ran
	Error string
	// What it answered in the shape icebath acts on, nil when it answered nothing
	Directive map[string]any
}

// Input of a run on standard input: the event's name, the tool and its arguments, the session, the folder, the profile, and every other
// field of the event under extra
func input(ctx context.Context, cfg *config.Config, event string, fields map[string]any) ordered {
	return append(ordered{{"hook_event_name", event}}, common(ctx, cfg, fields)...)
}

// Fields every hook's input and every posted event carry, in the order they are written
func common(ctx context.Context, cfg *config.Config, fields map[string]any) ordered {
	extra := ordered{}
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		if !topLevel[key] {
			extra = append(extra, field{key, fields[key]})
		}
	}
	if _, set := fields["kanban_task_id"]; !set {
		extra = append(extra, field{"kanban_task_id", nil})
	}
	var tool any
	if args, ok := fields["args"].(map[string]any); ok {
		tool = args
	}
	session := fields["session_id"]
	if !config.Truthy(session) {
		session = fields["parent_session_id"]
	}
	if !config.Truthy(session) {
		session = ""
	}
	return ordered{{"tool_name", fields["tool_name"]}, {"tool_input", tool}, {"session_id", session}, {"cwd", folder(ctx)}, {"profile", cfg.Profile()},
		{"extra", extra}}
}

// Folder of the turn the event fires in, this process's outside one
func folder(ctx context.Context) string {
	if cwd := hooks.TurnOf(ctx).Cwd; cwd != "" {
		return cwd
	}
	cwd, _ := os.Getwd()
	return cwd
}

// Runs the hook's command once with the event as its input, the answer it gave read as its directive
func (r *Registrant) once(ctx context.Context, cfg *config.Config, spec Spec, fields map[string]any) (*Run, error) {
	stdin, err := encode(input(ctx, cfg, spec.Event, fields), false)
	if err != nil {
		return nil, err
	}
	run, err := r.spawn(ctx, cfg, spec, stdin)
	if err != nil {
		return nil, err
	}
	run.Directive = evaluate(spec, run)
	return run, nil
}

// Runs the hook's command with the input on standard input, its whole tree ended when it runs out of time or the context ends
func (r *Registrant) spawn(ctx context.Context, cfg *config.Config, spec Spec, stdin string) (*Run, error) {
	argv, err := terminal.Split(fsx.ExpandHome(spec.Command))
	if err != nil {
		slog.Debug("shell hook command unparsable: " + err.Error())
		return &Run{Error: fmt.Sprintf("The command %s can't be read, because its quotes or escapes are not closed.", spec.Command)}, nil
	}
	if len(argv) == 0 {
		return &Run{Error: "The hook has no command."}, nil
	}
	return r.exec(ctx, cfg, argv, "", spec.Timeout, stdin)
}

// Runs the words as a hook's process in the folder, this process's when empty, with the input on standard input for at most the seconds
// given
func (r *Registrant) exec(ctx context.Context, cfg *config.Config, argv []string, dir string, timeout int, stdin string) (*Run, error) {
	run := &Run{}
	started := time.Now()
	prefix, err := r.interpreter(ctx, argv)
	if err != nil {
		run.Error = err.Error()
		return run, nil
	}
	argv = append(prefix, argv...)
	// The profile's environment as its commands get it; a command of icebath the hook runs works on the hook's own profile
	env := r.terminal.ChildEnv(cfg, map[string]string{profileconfig.ActiveEnv: cfg.Profile()})
	path, err := executable(argv[0], env)
	if err != nil {
		run.Error = startFailure(argv[0], err)
		return run, nil
	}
	cmd := exec.Command(path, argv[1:]...)
	cmd.Args[0] = argv[0]
	cmd.Dir = dir
	cmd.Env = environ(env)
	cmd.Stdin = strings.NewReader(stdin)
	detach(cmd)
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		fsx.CloseAll(outR, outW)
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	err = cmd.Start()
	fsx.CloseAll(outW, errW)
	if err != nil {
		fsx.CloseAll(outR, errR)
		run.Error = startFailure(argv[0], err)
		return run, nil
	}
	proc := terminal.Watch(cmd)
	var stdout, stderr bytes.Buffer
	var copies sync.WaitGroup
	copies.Add(2)
	go func() { defer copies.Done(); io.Copy(&stdout, outR) }()
	go func() { defer copies.Done(); io.Copy(&stderr, errR) }()
	done := make(chan struct{})
	code := 0
	go func() {
		copies.Wait()
		code, _ = proc.Wait(-1)
		fsx.CloseAll(outR, errR)
		close(done)
	}()
	reap := func() {
		proc.KillTree()
		select {
		case <-done:
		case <-time.After(time.Second):
			// Pipes a descendant still holds are let go of
			fsx.CloseAll(outR, errR)
		}
	}
	timer := time.NewTimer(time.Duration(timeout) * time.Second)
	defer timer.Stop()
	select {
	case <-done:
		run.Code = &code
		run.Stdout, run.Stderr = text(stdout.Bytes()), text(stderr.Bytes())
	case <-timer.C:
		reap()
		run.TimedOut = true
	case <-ctx.Done():
		reap()
		return nil, ctx.Err()
	}
	run.Seconds = math.Round(time.Since(started).Seconds()*1000) / 1000
	return run, nil
}

// Output of a command as text: bytes that are no UTF-8 replaced, line ends made \n
func text(data []byte) string {
	out := strings.ToValidUTF8(string(data), "�")
	return strings.ReplaceAll(strings.ReplaceAll(out, "\r\n", "\n"), "\r", "\n")
}

// Environment as the sorted NAME=value list a process takes
func environ(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for name, value := range env {
		out = append(out, name+"="+value)
	}
	slices.Sort(out)
	return out
}

// Why a command could not start, in a few words
func startFailure(name string, err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, exec.ErrNotFound):
		return "The command " + name + " was not found."
	case errors.Is(err, fs.ErrPermission):
		return "The command " + name + " may not run. Make the file runnable with chmod +x."
	case badFormat(err):
		return name + " " + notDirect + " " + name + "."
	}
	var failure *fs.PathError
	if errors.As(err, &failure) {
		return fmt.Sprintf("The command could not start: %s at %s.", failure.Err, failure.Path)
	}
	return err.Error()
}

// Block naming the hook that failed closed and why
func failedClosed(spec Spec, reason string) map[string]any {
	return map[string]any{"action": hooks.Block, "message": fmt.Sprintf("The hook %s blocks the call when it fails, and it failed. %s", spec.Command,
		reason)}
}

// What a run contributes to the point it fired on: its answer, nothing when it failed, ran out of time or answered nothing, unless it fails
// closed on a point that can be blocked, where every such failure blocks
func evaluate(spec Spec, run *Run) map[string]any {
	point, _ := hooks.PointOf(spec.Event)
	closed := spec.FailClosed && point.Blocking
	switch {
	case run.Error != "":
		slog.Warn(fmt.Sprintf("shell hook failed (event=%s command=%s): %s", spec.Event, spec.Command, run.Error))
	case run.TimedOut:
		slog.Warn(fmt.Sprintf("shell hook timed out after %.2fs (event=%s command=%s)", run.Seconds, spec.Event, spec.Command))
	}
	if run.Error != "" || run.TimedOut {
		if !closed {
			return nil
		}
		reason := run.Error
		if reason == "" {
			reason = "It did not finish within " + strs.Span(time.Duration(spec.Timeout)*time.Second) + "."
		}
		return failedClosed(spec, reason)
	}
	stderr := strings.TrimSpace(run.Stderr)
	limited := strs.Head(stderr, stderrLimit)
	if stderr != "" {
		slog.Debug(fmt.Sprintf("shell hook stderr (event=%s command=%s): %s", spec.Event, spec.Command, limited))
	}
	code := 0
	if run.Code != nil {
		code = *run.Code
	}
	if code == BlockExit && point.Blocking {
		if parsed := parse(spec.Event, run.Stdout); parsed != nil && parsed["action"] == hooks.Block {
			return parsed
		}
		message := limited
		if message == "" {
			message = defaultBlock
		}
		slog.Info(fmt.Sprintf("shell hook exited %d — blocking (event=%s command=%s): %s", BlockExit, spec.Event, spec.Command, message))
		return map[string]any{"action": hooks.Block, "message": message}
	}
	if code != 0 {
		slog.Warn(fmt.Sprintf("shell hook exited %d (event=%s command=%s); stderr=%s", code, spec.Event, spec.Command, limited))
	}
	stdout := strings.TrimSpace(run.Stdout)
	parsed := parse(spec.Event, stdout)
	if parsed == nil && closed && code != 0 {
		return failedClosed(spec, fmt.Sprintf("It ended with exit code %d and gave no answer.", code))
	}
	if parsed == nil && closed && stdout != "" && !object(stdout) {
		return failedClosed(spec, "Its answer is not a JSON object.")
	}
	return parsed
}

// Whether the text is a JSON object
func object(text string) bool {
	var value map[string]any
	return json.Unmarshal([]byte(text), &value) == nil && value != nil
}

// Message of a block: the first key's text, else the second's, else the default
func blockMessage(primary, secondary any) string {
	value := primary
	if !config.Truthy(value) {
		value = secondary
	}
	if text, ok := value.(string); ok && text != "" {
		return text
	}
	return defaultBlock
}

// Block, or modify, in either dialect, or the approval a pre_tool_call answer asks for
func preToolCall(data map[string]any) map[string]any {
	for _, d := range dialects {
		if data[d[0]] == hooks.Block {
			return map[string]any{"action": hooks.Block, "message": blockMessage(data[d[1]], data[d[2]])}
		}
	}
	for _, d := range dialects {
		if args, ok := data[d[3]].(map[string]any); ok && data[d[0]] == hooks.Modify {
			return map[string]any{"action": hooks.Modify, "args": args}
		}
	}
	if data["action"] == hooks.Approve {
		directive := map[string]any{"action": hooks.Approve}
		for _, key := range []string{"message", "rule_key"} {
			if text, ok := data[key].(string); ok && strings.TrimSpace(text) != "" {
				directive[key] = strings.TrimSpace(text)
			}
		}
		return directive
	}
	return nil
}

// The nudge a pre_verify answer sends the agent back with, nil without a message
func preVerify(data map[string]any) map[string]any {
	action := data["action"]
	if !config.Truthy(action) {
		action = data["decision"]
	}
	word := ""
	if config.Truthy(action) {
		word = strings.ToLower(strings.TrimSpace(fmt.Sprint(action)))
	}
	message := data["message"]
	if !config.Truthy(message) {
		message = data["reason"]
	}
	if text, ok := message.(string); ok && (word == hooks.Continue || word == hooks.Block) && strings.TrimSpace(text) != "" {
		return map[string]any{"action": hooks.Continue, "message": strings.TrimSpace(text)}
	}
	return nil
}

// Context an answer gives the turn, nil without one
func contextOf(data map[string]any) map[string]any {
	if text, ok := data["context"].(string); ok && strings.TrimSpace(text) != "" {
		return map[string]any{"context": text}
	}
	return nil
}

// Veto of the turn in either dialect, else the context a pre_llm_call answer gives it
func preLLMCall(data map[string]any) map[string]any {
	for _, d := range dialects {
		if data[d[0]] == hooks.Block {
			return map[string]any{"action": hooks.Block, "message": blockMessage(data[d[1]], data[d[2]])}
		}
	}
	return contextOf(data)
}

// Directive of what a hook printed, nil when it printed nothing, no JSON or no object
func parse(event, stdout string) map[string]any {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return nil
	}
	var value any
	if err := json.Unmarshal([]byte(stdout), &value); err != nil {
		slog.Warn(fmt.Sprintf("shell hook stdout was not valid JSON (event=%s): %s", event, strs.Head(stdout, 200)))
		return nil
	}
	data, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	switch event {
	case hooks.PreToolCall:
		return preToolCall(data)
	case hooks.PreVerify:
		return preVerify(data)
	case hooks.PreLLMCall:
		return preLLMCall(data)
	}
	return contextOf(data)
}

// Result the host acts on of a directive, nil for none
func result(directive map[string]any) *hooks.Result {
	if directive == nil {
		return nil
	}
	out := &hooks.Result{}
	out.Action, _ = directive["action"].(string)
	out.Message, _ = directive["message"].(string)
	out.Args, _ = directive["args"].(map[string]any)
	out.Rule, _ = directive["rule_key"].(string)
	out.Context, _ = directive["context"].(string)
	return out
}

// Directive as the wire shape a test or a check shows, empty for none
func Wire(directive map[string]any) string {
	if len(directive) == 0 {
		return ""
	}
	text, err := encode(directive, true)
	if err != nil {
		return fmt.Sprint(directive)
	}
	return text
}

package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/hooks"
	shellconfig "github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

// One configured hook with its consent
type View struct {
	Spec
	// On the allowlist, so it runs
	Allowed bool
	// When it was allowed, empty when the allowlist does not say
	ApprovedAt string
	// 1 when the script changed since it was allowed, 0 when it did not, -1 when that cannot be told; with its time then and now
	Drift    int
	DriftWas string
	DriftNow string
}

// What checking one configured hook found
type Check struct {
	// The script is there and can run as the command runs it
	Runnable bool
	Allowed  bool
	// When it was allowed, a question mark when the allowlist does not say
	ApprovedAt string
	// Whether the allowlist holds a script time to compare, with Drift as a View has it when it does
	Compared bool
	Drift    int
	DriftWas string
	DriftNow string
	// Run on a sample event, nil for a hook that is not allowed or cannot run
	Run *Run
	// What a finished run printed: empty, json or invalid
	Output string
	// How many of the findings are problems
	Problems int
}

// Events a shell hook may be set on: every point but those whose answer a command cannot give
func Events() []string {
	var out []string
	for _, name := range hooks.Points() {
		if point, _ := hooks.PointOf(name); !point.NoShell {
			out = append(out, name)
		}
	}
	return out
}

// Every configured hook of the profile with its consent, and every outbound webhook
func (r *Registrant) Overview(cfg *config.Config) ([]View, []*Target, error) {
	specs := r.Configured(cfg)
	targets := r.Targets(cfg)
	views := make([]View, 0, len(specs))
	allowed, _ := r.allowed(cfg)
	for _, spec := range specs {
		view := View{Spec: spec, Allowed: allowed[spec.pair()], Drift: -1}
		if view.Allowed {
			if entry := Entry(cfg, spec.Event, spec.Command); config.Truthy(entry["approved_at"]) {
				view.ApprovedAt = fmt.Sprint(entry["approved_at"])
				var err error
				if view.Drift, view.DriftWas, view.DriftNow, err = drift(spec, entry); err != nil {
					return nil, nil, err
				}
			}
		}
		views = append(views, view)
	}
	return views, targets, nil
}

// Whether the hook's script changed since it was allowed: 1 when it did, 0 when it did not, -1 when either time is not known; with the
// time recorded and the time now
func drift(spec Spec, entry map[string]any) (int, string, string, error) {
	now, _ := Mtime(spec.Command)
	recorded := entry["script_mtime_at_approval"]
	at := show(recorded)
	if text, ok := recorded.(string); ok {
		at = text
	}
	if now == "" || !config.Truthy(recorded) {
		return -1, at, now, nil
	}
	text, ok := recorded.(string)
	if !ok {
		return 0, at, now, fmt.Errorf("The allowlist keeps the time of the hook script as %s, where it should be text.", kindOf(recorded))
	}
	switch {
	case now > text:
		return 1, at, now, nil
	case now == text:
		return 0, at, now, nil
	}
	return -1, at, now, nil
}

// Sample event of the point for a run of a hook, shaped as the agent fires it, naming the tool when one is given
func Sample(event, tool string) map[string]any {
	payload, ok := sample(event)
	if !ok {
		payload = map[string]any{"session_id": "test-session"}
	}
	if tool != "" {
		payload["tool_name"] = tool
	}
	return payload
}

// Sample event of the points the agent fires most, false for the others
func sample(event string) (map[string]any, bool) {
	args := func() map[string]any { return map[string]any{"command": "echo hello"} }
	merge := func(base, extra map[string]any) map[string]any {
		for k, v := range extra {
			base[k] = v
		}
		return base
	}
	api := func() map[string]any {
		return map[string]any{"session_id": "test-session", "task_id": "test-task", "platform": "cli", "model": "claude-sonnet-4-6", "provider": "anthropic",
			"base_url": "https://api.anthropic.com", "api_mode": "anthropic_messages", "api_call_count": 1}
	}
	aux := func() map[string]any {
		return map[string]any{"aux_task": "title_generation", "session_id": "test-session", "task_id": "test-task", "turn_id": "test-turn",
			"api_request_id": "aux-0123abcd", "platform": "cli", "model": "claude-haiku-4-5", "provider": "anthropic", "base_url": "https://api.anthropic.com",
			"api_mode": "anthropic_messages", "api_call_count": 1, "retry_count": 0, "streaming": false}
	}
	switch event {
	case hooks.PreToolCall:
		return map[string]any{"tool_name": "terminal", "args": args(), "session_id": "test-session", "task_id": "test-task", "tool_call_id": "test-call"}, true
	case hooks.PostToolCall:
		return map[string]any{"tool_name": "terminal", "args": args(), "session_id": "test-session", "task_id": "test-task", "tool_call_id": "test-call",
			"result": `{"output": "hello"}`, "duration_ms": 42}, true
	case hooks.PreLLMCall:
		return map[string]any{"session_id": "test-session", "user_message": "What is the weather?", "conversation_history": []any{}, "is_first_turn": true,
			"model": "gpt-4", "platform": "cli"}, true
	case hooks.PostLLMCall:
		return map[string]any{"session_id": "test-session", "model": "gpt-4", "platform": "cli"}, true
	case hooks.PreVerify:
		return map[string]any{"session_id": "test-session", "platform": "cli", "model": "gpt-4", "coding": true, "attempt": 0,
			"final_response": "All done — the change is applied.", "changed_paths": []any{"src/app.tsx"}}, true
	case hooks.SessionStart, hooks.SessionFinalize, hooks.SessionReset:
		return map[string]any{"session_id": "test-session"}, true
	case hooks.SessionEnd:
		return map[string]any{"session_id": "test-session", "task_id": "test-task", "turn_id": "test-turn", "completed": true, "failed": false,
			"interrupted": false, "turn_exit_reason": "text_response(stop)", "model": "gpt-4", "platform": "cli"}, true
	case hooks.SessionDelete:
		return map[string]any{"session_id": "test-session", "reason": "delete", "profile": "default"}, true
	case hooks.PreAPIRequest:
		return merge(api(), map[string]any{"message_count": 4, "tool_count": 12, "approx_input_tokens": 2048, "request_char_count": 8192,
			"max_tokens": 4096}), true
	case hooks.PostAPIRequest:
		return merge(api(), map[string]any{"api_duration": 1.234, "started_at": 1756000000.0, "ended_at": 1756000001.234, "first_chunk_at": 1756000000.512,
			"finish_reason": "stop", "message_count": 4, "response_model": "claude-sonnet-4-6", "usage": map[string]any{"input_tokens": 2048,
				"output_tokens": 512}, "context_length": 200000, "assistant_content_chars": 1200, "assistant_tool_call_count": 0, "moa_references": nil}), true
	case hooks.PreAuxiliaryCall:
		return merge(aux(), map[string]any{"message_count": 2, "tool_count": 0, "approx_input_tokens": 256, "request_char_count": 1024, "max_tokens": 64}), true
	case hooks.PostAuxiliaryCall:
		return merge(aux(), map[string]any{"api_duration": 0.42, "started_at": 1756000000.0, "ended_at": 1756000000.42, "finish_reason": "stop",
			"response_model": "claude-haiku-4-5", "usage": map[string]any{"input_tokens": 256, "output_tokens": 12}, "assistant_content_chars": 40,
			"assistant_tool_call_count": 0, "error": nil, "error_type": nil}), true
	case hooks.SubagentStop:
		return map[string]any{"parent_session_id": "parent-sess", "child_role": nil, "child_summary": "Synthetic summary for hooks test",
			"child_status": "completed", "tool_call_history": []any{map[string]any{"tool_name": "write_file", "tool_input": map[string]any{
				"argument_keys": []any{"content", "path"}, "targets": map[string]any{"path": "notes/report.txt"}}, "input_bytes": 128, "output_bytes": 32,
				"status": "ok"}}, "duration_ms": 1234}, true
	}
	return nil, false
}

// Runs every configured hook of the event once on its sample event, the fields given laid over it; with a tool, only the hooks of tool
// calls whose matcher names it. The hooks it ran, each with how it went
func (r *Registrant) Test(ctx context.Context, cfg *config.Config, event, tool string, fields map[string]any) ([]Spec, []*Run, error) {
	payload := Sample(event, tool)
	for k, v := range fields {
		payload[k] = v
	}
	var specs []Spec
	for _, spec := range r.Configured(cfg) {
		if spec.Event != event {
			continue
		}
		if point, _ := hooks.PointOf(spec.Event); tool != "" && point.Tool {
			matches, err := spec.Matcher.Matches(tool)
			if err != nil {
				return nil, nil, err
			}
			if !matches {
				continue
			}
		}
		specs = append(specs, spec)
	}
	runs := make([]*Run, 0, len(specs))
	for _, spec := range specs {
		run, err := r.once(ctx, cfg, spec, payload)
		if err != nil {
			return nil, nil, err
		}
		runs = append(runs, run)
	}
	return specs, runs, nil
}

// Runs one configured hook once on its sample event, naming the tool when one is given
func (r *Registrant) TestOne(ctx context.Context, cfg *config.Config, event, command, tool string) (*Run, error) {
	spec, ok := r.Find(cfg, event, command)
	if !ok {
		return nil, &Refusal{Message: "There is no hook " + command + " on " + event + "."}
	}
	return r.once(ctx, cfg, spec, Sample(event, tool))
}

// Checks a configured hook: whether its script can run, whether it is allowed and unchanged since, and how a run of an allowed hook that
// can run ends on its sample event
func (r *Registrant) Check(ctx context.Context, cfg *config.Config, spec Spec) (*Check, error) {
	check := &Check{Runnable: Runnable(spec.Command), Drift: -1}
	if !check.Runnable {
		check.Problems++
	}
	entry := Entry(cfg, spec.Event, spec.Command)
	if check.Allowed = len(entry) > 0; !check.Allowed {
		check.Problems++
		return check, nil
	}
	check.ApprovedAt = "?"
	if at, ok := entry["approved_at"]; ok && at != nil {
		check.ApprovedAt = fmt.Sprint(at)
	}
	if config.Truthy(entry["script_mtime_at_approval"]) {
		var err error
		if check.Drift, check.DriftWas, check.DriftNow, err = drift(spec, entry); err != nil {
			return check, err
		}
		check.Compared = true
		if check.Drift == 1 {
			check.Problems++
		}
	}
	if !check.Runnable {
		return check, nil
	}
	payload, ok := sample(spec.Event)
	if !ok {
		payload = map[string]any{"extra": map[string]any{}}
	}
	run, err := r.once(ctx, cfg, spec, payload)
	if err != nil {
		return check, err
	}
	check.Run = run
	switch stdout := strings.TrimSpace(run.Stdout); {
	case run.TimedOut, run.Error != "":
		check.Problems++
	case stdout == "":
		check.Output = "empty"
	case json.Valid([]byte(stdout)):
		check.Output = "json"
	default:
		check.Output = "invalid"
		check.Problems++
	}
	return check, nil
}

// Adds a hook to the profile's events setting, its tools and seconds left out when not given
func (r *Registrant) Add(cfg *config.Config, event, command, matcher string, timeout int) error {
	command, matcher = strings.TrimSpace(command), strings.TrimSpace(matcher)
	point, known := hooks.PointOf(event)
	switch {
	case !known || point.NoShell:
		return &Refusal{Message: "icebath has no hook event “" + event + "”."}
	case command == "":
		return &Refusal{Message: "A hook needs a command."}
	case matcher != "" && !point.Tool:
		return &Refusal{Message: "Only hooks that run before or after a tool call can name tools."}
	case timeout < 0 || timeout > MaxTimeout:
		return &Refusal{Message: "A hook can take at most " + strs.Span(MaxTimeout*time.Second) + "."}
	}
	entry := map[string]any{"command": command}
	if matcher != "" {
		entry["matcher"] = matcher
	}
	if timeout > 0 {
		entry["timeout"] = timeout
	}
	block, _ := cfg.Get(shellconfig.Events).(map[string]any)
	list, _ := block[event].([]any)
	return cfg.Set(shellconfig.Events+"."+event, append(append([]any{}, list...), entry))
}

// Takes every hook running the command out of the event's list of the profile's events setting, how many went
func (r *Registrant) Remove(cfg *config.Config, event, command string) (int, error) {
	block, _ := cfg.Get(shellconfig.Events).(map[string]any)
	list, _ := block[event].([]any)
	kept := make([]any, 0, len(list))
	for _, item := range list {
		entry, _ := item.(map[string]any)
		if text, _ := entry["command"].(string); strings.TrimSpace(text) != command {
			kept = append(kept, item)
		}
	}
	removed := len(list) - len(kept)
	if removed == 0 {
		return 0, nil
	}
	if len(kept) == 0 {
		return removed, cfg.Set(shellconfig.Events+"."+event, nil)
	}
	return removed, cfg.Set(shellconfig.Events+"."+event, kept)
}

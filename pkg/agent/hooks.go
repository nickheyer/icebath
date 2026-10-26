package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
)

// Moment in a run a hook can attach to
type HookEvent string

const (
	UserPromptSubmit HookEvent = "UserPromptSubmit"
	PreToolUse       HookEvent = "PreToolUse"
	PostToolUse      HookEvent = "PostToolUse"
	Stop             HookEvent = "Stop"
	PreCompact       HookEvent = "PreCompact"
	SessionStart     HookEvent = "SessionStart"
	SessionEnd       HookEvent = "SessionEnd"
	SubagentStop     HookEvent = "SubagentStop"
	Notification     HookEvent = "Notification"
)

// Every event a configuration may name
var HookEvents = []HookEvent{UserPromptSubmit, PreToolUse, PostToolUse, Stop, PreCompact, SessionStart, SessionEnd, SubagentStop, Notification}

// One command to run
type HookCommand struct {
	Type    string `json:"type,omitempty"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// Commands run for tools whose name matches the pattern, every tool when empty
type HookGroup struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []HookCommand `json:"hooks"`
	re      *regexp.Regexp
}

type HookConfig map[HookEvent][]HookGroup

// Reads a hooks block from JSON, nested under hooks or at the top level, and checks every entry
func LoadHooks(r io.Reader) (HookConfig, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Hooks map[HookEvent][]HookGroup `json:"hooks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("hooks: %w", err)
	}
	cfg := HookConfig(doc.Hooks)
	if cfg == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("hooks: %w", err)
		}
	}
	return cfg, cfg.Validate()
}

func LoadHooksFile(path string) (HookConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return LoadHooks(f)
}

func knownHook(event HookEvent) bool {
	for _, e := range HookEvents {
		if e == event {
			return true
		}
	}
	return false
}

// Checks event names, command kinds and matcher patterns, compiling the patterns
func (c HookConfig) Validate() error {
	for event, groups := range c {
		if !knownHook(event) {
			return fmt.Errorf("hooks: unknown event %q", event)
		}
		for i := range groups {
			g := &groups[i]
			if g.Matcher != "" {
				re, err := regexp.Compile(g.Matcher)
				if err != nil {
					return fmt.Errorf("hooks: %s matcher %q: %w", event, g.Matcher, err)
				}
				g.re = re
			}
			for _, h := range g.Hooks {
				if h.Type != "" && h.Type != "command" {
					return fmt.Errorf("hooks: %s hook type %q is not command", event, h.Type)
				}
				if strings.TrimSpace(h.Command) == "" {
					return fmt.Errorf("hooks: %s has a hook without a command", event)
				}
			}
		}
	}
	return nil
}

// Runs configured hooks as an agent hook
type HookRunner struct {
	Config HookConfig
	// Working directory of hook commands, the process directory when empty
	Dir string
	// KEY=VALUE pairs layered over the process environment
	Env []string
	// Passed to hooks as session_id and ICEBATH_SESSION_ID
	SessionID string
	// Path of the stored session log, passed to hooks as transcript_path
	Transcript string
	// Receives hook standard error and any standard hookOutput that is not JSON, discarded when nil
	Log io.Writer
}

// What one hook execution produced, published as the data of a hook event
type HookExecution struct {
	HookEvent HookEvent
	Command   string
	Tool      string
	ExitCode  int
	Duration  time.Duration
	Stdout    string
	Stderr    string
	Blocked   bool
}

type hookPayload struct {
	HookEvent      HookEvent       `json:"hook_event_name"`
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path,omitempty"`
	Cwd            string          `json:"cwd"`
	ToolName       string          `json:"tool_name,omitempty"`
	ToolInput      json.RawMessage `json:"tool_input,omitempty"`
	ToolResponse   json.RawMessage `json:"tool_response,omitempty"`
	Prompt         string          `json:"prompt,omitempty"`
	Response       string          `json:"response,omitempty"`
	StopHookActive bool            `json:"stop_hook_active,omitempty"`
	Extra          map[string]any  `json:"-"`
}

// Payload with the extra fields merged at the top level
func (p hookPayload) MarshalJSON() ([]byte, error) {
	type plain hookPayload
	base, err := json.Marshal(plain(p))
	if err != nil {
		return nil, err
	}
	if len(p.Extra) == 0 {
		return base, nil
	}
	var m map[string]any
	if err := json.Unmarshal(base, &m); err != nil {
		return nil, err
	}
	for k, v := range p.Extra {
		m[k] = v
	}
	return json.Marshal(m)
}

// Tool result as PostToolUse hooks receive it, every part in its stored JSON form
type hookToolResponse struct {
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

type hookOutput struct {
	Decision          string          `json:"decision"`
	Reason            string          `json:"reason"`
	AdditionalContext string          `json:"additionalContext"`
	UpdatedInput      json.RawMessage `json:"updatedInput"`
	HookSpecific      *struct {
		PermissionDecision string          `json:"permissionDecision"`
		Reason             string          `json:"permissionDecisionReason"`
		UpdatedInput       json.RawMessage `json:"updatedInput"`
		AdditionalContext  string          `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// What one hook asked for after it ran
type hookVerdict struct {
	block    bool
	reason   string
	context  string
	input    json.RawMessage
	decision policy.Decision
}

// Refusal raised by a hook
type HookBlocked struct {
	HookEvent HookEvent
	Reason    string
}

func (b *HookBlocked) Error() string {
	return fmt.Sprintf("hook %s blocked: %s", b.HookEvent, b.Reason)
}

func (r HookRunner) BeforeRun(ctx context.Context, input []protocol.Part) ([]protocol.Part, error) {
	v, err := r.run(ctx, UserPromptSubmit, "", hookPayload{Prompt: protocolText(input)})
	if err != nil {
		return nil, err
	}
	if v.block {
		return nil, &HookBlocked{HookEvent: UserPromptSubmit, Reason: v.reason}
	}
	if v.context != "" {
		input = append(input, protocol.Text{Text: v.context})
	}
	return input, nil
}

func (HookRunner) BeforeTurn(context.Context, *protocol.Request) error { return nil }

func (HookRunner) AfterTurn(context.Context, protocol.Response) error { return nil }

// Runs PreToolUse hooks, carrying their rewritten input, settled permission and context back as advice
func (r HookRunner) BeforeTool(ctx context.Context, call protocol.ToolCall) (Advice, error) {
	v, err := r.run(ctx, PreToolUse, call.Name, hookPayload{ToolName: call.Name, ToolInput: call.Args})
	if err != nil {
		return Advice{Call: call}, err
	}
	if v.block {
		return Advice{Call: call}, &HookBlocked{HookEvent: PreToolUse, Reason: v.reason}
	}
	if len(v.input) > 0 {
		call.Args = v.input
	}
	return Advice{Call: call, Decision: v.decision, Context: v.context}, nil
}

// Runs PostToolUse hooks, a block keeping the real hookOutput and marking the result as failed with the reason
func (r HookRunner) AfterTool(ctx context.Context, call protocol.ToolCall, res protocol.ToolResult) (protocol.ToolResult, error) {
	content, err := session.MarshalParts(res.Content)
	if err != nil {
		return res, fmt.Errorf("hooks: %w", err)
	}
	response, err := json.Marshal(hookToolResponse{Content: content, IsError: res.IsError})
	if err != nil {
		return res, err
	}
	v, err := r.run(ctx, PostToolUse, call.Name, hookPayload{ToolName: call.Name, ToolInput: call.Args, ToolResponse: response})
	if err != nil {
		return res, err
	}
	if v.block {
		res.Content = append(res.Content, protocol.Text{Text: "[hook " + string(PostToolUse) + " blocked: " + v.reason + "]"})
		res.IsError = true
		return res, nil
	}
	if v.context != "" {
		res.Content = append(res.Content, protocol.Text{Text: v.context})
	}
	return res, nil
}

// Runs Stop hooks, a block continuing the run with the reason and any context as the next user message
func (r HookRunner) OnStop(ctx context.Context, resp protocol.Response, again bool) ([]protocol.Part, error) {
	v, err := r.run(ctx, Stop, "", hookPayload{Response: protocolText(resp.Content), StopHookActive: again})
	if err != nil {
		return nil, err
	}
	if !v.block {
		return nil, nil
	}
	parts := []protocol.Part{protocol.Text{Text: v.reason}}
	if v.context != "" {
		parts = append(parts, protocol.Text{Text: v.context})
	}
	return parts, nil
}

// Runs the hooks of any event with extra top level fields, returning the context they added
func (r HookRunner) Fire(ctx context.Context, event HookEvent, fields map[string]any) (string, error) {
	if !knownHook(event) {
		return "", fmt.Errorf("hooks: unknown event %q", event)
	}
	tool := ""
	if name, ok := fields["tool_name"].(string); ok {
		tool = name
	}
	v, err := r.run(ctx, event, tool, hookPayload{Extra: fields})
	if err != nil {
		return "", err
	}
	if v.block {
		return "", &HookBlocked{HookEvent: event, Reason: v.reason}
	}
	return v.context, nil
}

// Runs every command attached to the event in order, stopping at the first block and keeping the strictest permission
func (r HookRunner) run(ctx context.Context, event HookEvent, toolName string, p hookPayload) (hookVerdict, error) {
	var v hookVerdict
	groups := r.Config[event]
	for _, g := range groups {
		if g.Matcher != "" {
			re := g.re
			if re == nil {
				compiled, err := regexp.Compile(g.Matcher)
				if err != nil {
					return v, fmt.Errorf("hooks: %s matcher %q: %w", event, g.Matcher, err)
				}
				re = compiled
			}
			if !re.MatchString(toolName) {
				continue
			}
		}
		for _, cmd := range g.Hooks {
			dir, err := r.dir()
			if err != nil {
				return v, err
			}
			p.HookEvent, p.SessionID, p.TranscriptPath, p.Cwd = event, r.SessionID, r.Transcript, dir
			if len(v.input) > 0 {
				p.ToolInput = v.input
			}
			out, err := r.exec(ctx, event, toolName, cmd, p)
			if err != nil {
				return v, err
			}
			if out.block {
				return out, nil
			}
			if out.context != "" {
				v.context = strings.TrimSpace(v.context + "\n" + out.context)
			}
			if len(out.input) > 0 {
				v.input = out.input
			}
			v.decision = policy.Stricter(v.decision, out.decision)
		}
	}
	return v, nil
}

func (r HookRunner) dir() (string, error) {
	if r.Dir != "" {
		return r.Dir, nil
	}
	return os.Getwd()
}

// Context that ends after the timeout, or with the parent alone when the timeout is zero
func deadline(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return context.WithCancel(ctx)
}

// Executes one hook, reading its hookVerdict from the exit code and any JSON it printed
func (r HookRunner) exec(ctx context.Context, event HookEvent, toolName string, cmd HookCommand, p hookPayload) (hookVerdict, error) {
	timeout := time.Duration(cmd.Timeout) * time.Second
	hctx, cancel := deadline(ctx, timeout)
	defer cancel()
	stdin, err := json.Marshal(p)
	if err != nil {
		return hookVerdict{}, err
	}
	c := proc.ShellCommand(hctx, cmd.Command)
	c.Dir = r.Dir
	c.Env = append(append(os.Environ(), "ICEBATH_SESSION_ID="+r.SessionID, "ICEBATH_HOOK_EVENT="+string(event)), r.Env...)
	c.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	started := time.Now()
	err = c.Run()
	ex := HookExecution{HookEvent: event, Command: cmd.Command, Tool: toolName, Duration: time.Since(started), Stdout: stdout.String(), Stderr: stderr.String()}
	if c.ProcessState != nil {
		ex.ExitCode = c.ProcessState.ExitCode()
	}
	if r.Log != nil && stderr.Len() > 0 {
		fmt.Fprintf(r.Log, "[hook %s] %s\n", event, strings.TrimSpace(stderr.String()))
	}
	reason := strings.TrimSpace(stderr.String())
	var exit *exec.ExitError
	switch {
	case errors.Is(hctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		ex.ExitCode = -1
		r.publish(ctx, ex, fmt.Sprintf("timed out after %s", timeout))
		return hookVerdict{}, fmt.Errorf("hook %s %q timed out after %s", event, cmd.Command, timeout)
	case errors.As(err, &exit) && exit.ExitCode() == 2:
		if reason == "" {
			reason = "blocked by hook"
		}
		ex.Blocked = true
		r.publish(ctx, ex, "blocked: "+reason)
		return hookVerdict{block: true, reason: reason}, nil
	case err != nil:
		r.publish(ctx, ex, "failed: "+err.Error())
		return hookVerdict{}, fmt.Errorf("hook %s %q failed: %w: %s", event, cmd.Command, err, reason)
	}
	v, perr := r.parse(event, cmd, bytes.TrimSpace(stdout.Bytes()))
	if perr != nil {
		r.publish(ctx, ex, "returned bad output")
		return v, perr
	}
	if v.block {
		ex.Blocked = true
		r.publish(ctx, ex, "blocked: "+v.reason)
		return v, nil
	}
	r.publish(ctx, ex, "ok")
	return v, nil
}

// Reports one execution into the event stream of the run that triggered it
func (r HookRunner) publish(ctx context.Context, ex HookExecution, summary string) {
	Observe(ctx, Event{Type: EventHook, Text: fmt.Sprintf("%s %q %s", ex.HookEvent, ex.Command, summary), Data: ex, Duration: ex.Duration})
}

// Reads the JSON a hook printed, logging plain text hookOutput instead and rejecting fields the event does not take
func (r HookRunner) parse(event HookEvent, cmd HookCommand, stdout []byte) (hookVerdict, error) {
	if len(stdout) == 0 {
		return hookVerdict{}, nil
	}
	var out hookOutput
	if err := json.Unmarshal(stdout, &out); err != nil {
		if r.Log != nil {
			fmt.Fprintf(r.Log, "[hook %s] %s\n", event, stdout)
		}
		return hookVerdict{}, nil
	}
	var v hookVerdict
	switch out.Decision {
	case "":
	case "block":
		v.block, v.reason = true, out.Reason
	case "approve":
		v.decision = policy.Allow
	default:
		return hookVerdict{}, fmt.Errorf("hook %s %q returned unsupported decision %q", event, cmd.Command, out.Decision)
	}
	v.context = out.AdditionalContext
	v.input = out.UpdatedInput
	if hs := out.HookSpecific; hs != nil {
		switch hs.PermissionDecision {
		case "":
		case "allow":
			v.decision = policy.Allow
		case "ask":
			v.decision = policy.Ask
		case "deny":
			v.block, v.reason = true, hs.Reason
		default:
			return hookVerdict{}, fmt.Errorf("hook %s %q returned unsupported permission decision %q", event, cmd.Command, hs.PermissionDecision)
		}
		if hs.AdditionalContext != "" {
			v.context = hs.AdditionalContext
		}
		if len(hs.UpdatedInput) > 0 {
			v.input = hs.UpdatedInput
		}
	}
	if (v.decision != "" || len(v.input) > 0) && event != PreToolUse {
		return hookVerdict{}, fmt.Errorf("hook %s %q returned a permission or updated input, which only PreToolUse takes", event, cmd.Command)
	}
	if v.block && v.reason == "" {
		v.reason = "blocked by hook"
	}
	if len(v.input) > 0 {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(v.input, &obj); err != nil {
			return hookVerdict{}, fmt.Errorf("hook %s %q returned updated input that is not a JSON object", event, cmd.Command)
		}
	}
	return v, nil
}

var _ Hook = HookRunner{}

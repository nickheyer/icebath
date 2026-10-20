// Lifecycle hooks run as external commands fed JSON on stdin, the way agentic harnesses configure them
package hooks

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

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
)

// Moment in a run a hook can attach to
type Event string

const (
	UserPromptSubmit Event = "UserPromptSubmit"
	PreToolUse       Event = "PreToolUse"
	PostToolUse      Event = "PostToolUse"
	Stop             Event = "Stop"
)

// Seconds a hook may run when it names no timeout
const DefaultTimeout = 60

// One command to run
type Command struct {
	Type    string `json:"type,omitempty"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// Commands run for tools whose name matches the pattern, every tool when empty
type Group struct {
	Matcher string    `json:"matcher,omitempty"`
	Hooks   []Command `json:"hooks"`
	re      *regexp.Regexp
}

type Config map[Event][]Group

// Reads a hooks block from JSON, nested under hooks or at the top level, and checks every entry
func Load(r io.Reader) (Config, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Hooks map[Event][]Group `json:"hooks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("hooks: %w", err)
	}
	cfg := Config(doc.Hooks)
	if cfg == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("hooks: %w", err)
		}
	}
	return cfg, cfg.Validate()
}

func LoadFile(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Load(f)
}

// Checks event names, command kinds and matcher patterns, compiling the patterns
func (c Config) Validate() error {
	for event, groups := range c {
		switch event {
		case UserPromptSubmit, PreToolUse, PostToolUse, Stop:
		default:
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
type Runner struct {
	Config Config
	// Working directory of hook commands, the process directory when empty
	Dir string
	// KEY=VALUE pairs layered over the process environment
	Env []string
	// Passed to hooks as session_id and ICEBATH_SESSION_ID
	SessionID string
	// Path of the stored session log, passed to hooks as transcript_path
	Transcript string
	// Receives hook standard error and any standard output that is not JSON, discarded when nil
	Log io.Writer
}

type payload struct {
	Event          Event           `json:"hook_event_name"`
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path,omitempty"`
	Cwd            string          `json:"cwd"`
	ToolName       string          `json:"tool_name,omitempty"`
	ToolInput      json.RawMessage `json:"tool_input,omitempty"`
	ToolResponse   json.RawMessage `json:"tool_response,omitempty"`
	Prompt         string          `json:"prompt,omitempty"`
	Response       string          `json:"response,omitempty"`
}

// Tool result as PostToolUse hooks receive it, every part in its stored JSON form
type toolResponse struct {
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

type output struct {
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
type verdict struct {
	block    bool
	reason   string
	context  string
	input    json.RawMessage
	decision policy.Decision
}

// Refusal raised by a hook
type Blocked struct {
	Event  Event
	Reason string
}

func (b *Blocked) Error() string {
	return fmt.Sprintf("hook %s blocked: %s", b.Event, b.Reason)
}

func (r Runner) BeforeRun(ctx context.Context, input []protocol.Part) ([]protocol.Part, error) {
	v, err := r.run(ctx, UserPromptSubmit, "", payload{Prompt: text(input)})
	if err != nil {
		return nil, err
	}
	if v.block {
		return nil, &Blocked{Event: UserPromptSubmit, Reason: v.reason}
	}
	if v.context != "" {
		input = append(input, protocol.Text{Text: v.context})
	}
	return input, nil
}

func (Runner) BeforeTurn(context.Context, *protocol.Request) error { return nil }

func (r Runner) BeforeTool(ctx context.Context, call protocol.ToolCall) (protocol.ToolCall, policy.Decision, error) {
	v, err := r.run(ctx, PreToolUse, call.Name, payload{ToolName: call.Name, ToolInput: call.Args})
	if err != nil {
		return call, "", err
	}
	if v.block {
		return call, "", &Blocked{Event: PreToolUse, Reason: v.reason}
	}
	if len(v.input) > 0 {
		call.Args = v.input
	}
	return call, v.decision, nil
}

func (r Runner) AfterTool(ctx context.Context, call protocol.ToolCall, res protocol.ToolResult) (protocol.ToolResult, error) {
	content, err := session.MarshalParts(res.Content)
	if err != nil {
		return res, fmt.Errorf("hooks: %w", err)
	}
	response, err := json.Marshal(toolResponse{Content: content, IsError: res.IsError})
	if err != nil {
		return res, err
	}
	v, err := r.run(ctx, PostToolUse, call.Name, payload{ToolName: call.Name, ToolInput: call.Args, ToolResponse: response})
	if err != nil {
		return res, err
	}
	if v.block {
		return res, &Blocked{Event: PostToolUse, Reason: v.reason}
	}
	if v.context != "" {
		res.Content = append(res.Content, protocol.Text{Text: v.context})
	}
	return res, nil
}

func (r Runner) OnStop(ctx context.Context, resp protocol.Response) ([]protocol.Part, error) {
	v, err := r.run(ctx, Stop, "", payload{Response: text(resp.Content)})
	if err != nil {
		return nil, err
	}
	if v.block {
		return []protocol.Part{protocol.Text{Text: v.reason}}, nil
	}
	return nil, nil
}

func text(parts []protocol.Part) string {
	var sb strings.Builder
	for _, p := range parts {
		if t, ok := p.(protocol.Text); ok {
			sb.WriteString(t.Text)
		}
	}
	return sb.String()
}

// Runs every command attached to the event in order, stopping at the first block and keeping the strictest permission
func (r Runner) run(ctx context.Context, event Event, toolName string, p payload) (verdict, error) {
	var v verdict
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
			p.Event, p.SessionID, p.TranscriptPath, p.Cwd = event, r.SessionID, r.Transcript, dir
			if len(v.input) > 0 {
				p.ToolInput = v.input
			}
			out, err := r.exec(ctx, event, cmd, p)
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

func (r Runner) dir() (string, error) {
	if r.Dir != "" {
		return r.Dir, nil
	}
	return os.Getwd()
}

// Executes one hook, reading its verdict from the exit code and any JSON it printed
func (r Runner) exec(ctx context.Context, event Event, cmd Command, p payload) (verdict, error) {
	timeout := time.Duration(cmd.Timeout) * time.Second
	if timeout <= 0 {
		timeout = DefaultTimeout * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stdin, err := json.Marshal(p)
	if err != nil {
		return verdict{}, err
	}
	c := exec.CommandContext(ctx, "sh", "-c", cmd.Command)
	c.Dir = r.Dir
	c.Env = append(append(os.Environ(), "ICEBATH_SESSION_ID="+r.SessionID, "ICEBATH_HOOK_EVENT="+string(event)), r.Env...)
	c.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	err = c.Run()
	if r.Log != nil && stderr.Len() > 0 {
		fmt.Fprintf(r.Log, "[hook %s] %s\n", event, strings.TrimSpace(stderr.String()))
	}
	reason := strings.TrimSpace(stderr.String())
	var exit *exec.ExitError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return verdict{}, fmt.Errorf("hook %s %q timed out after %s", event, cmd.Command, timeout)
	case errors.As(err, &exit) && exit.ExitCode() == 2:
		if reason == "" {
			reason = "blocked by hook"
		}
		return verdict{block: true, reason: reason}, nil
	case err != nil:
		return verdict{}, fmt.Errorf("hook %s %q failed: %w: %s", event, cmd.Command, err, reason)
	}
	return r.parse(event, cmd, bytes.TrimSpace(stdout.Bytes()))
}

// Reads the JSON a hook printed, logging plain text output instead and rejecting fields the event does not take
func (r Runner) parse(event Event, cmd Command, stdout []byte) (verdict, error) {
	if len(stdout) == 0 {
		return verdict{}, nil
	}
	var out output
	if err := json.Unmarshal(stdout, &out); err != nil {
		if r.Log != nil {
			fmt.Fprintf(r.Log, "[hook %s] %s\n", event, stdout)
		}
		return verdict{}, nil
	}
	var v verdict
	switch out.Decision {
	case "":
	case "block":
		v.block, v.reason = true, out.Reason
	case "approve":
		v.decision = policy.Allow
	default:
		return verdict{}, fmt.Errorf("hook %s %q returned unsupported decision %q", event, cmd.Command, out.Decision)
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
			return verdict{}, fmt.Errorf("hook %s %q returned unsupported permission decision %q", event, cmd.Command, hs.PermissionDecision)
		}
		if hs.AdditionalContext != "" {
			v.context = hs.AdditionalContext
		}
		if len(hs.UpdatedInput) > 0 {
			v.input = hs.UpdatedInput
		}
	}
	if (v.decision != "" || len(v.input) > 0) && event != PreToolUse {
		return verdict{}, fmt.Errorf("hook %s %q returned a permission or updated input, which only PreToolUse takes", event, cmd.Command)
	}
	if v.block && v.reason == "" {
		v.reason = "blocked by hook"
	}
	if len(v.input) > 0 {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(v.input, &obj); err != nil {
			return verdict{}, fmt.Errorf("hook %s %q returned updated input that is not a JSON object", event, cmd.Command)
		}
	}
	return v, nil
}

var _ agent.Hook = Runner{}

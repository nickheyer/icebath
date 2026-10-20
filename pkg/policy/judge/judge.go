// Model backed reviewer that approves, denies or escalates tool calls
package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
)

// Completes one request, satisfied by a provider
type Completer interface {
	Complete(ctx context.Context, req protocol.Request) (protocol.Response, error)
}

const system = `You review one tool call an automated agent wants to run and decide whether it may proceed.
Answer with a single JSON object and nothing else: {"decision":"allow"|"deny"|"ask","reason":"one sentence"}.
allow is for calls that are clearly safe and expected. deny is for calls that destroy data, exfiltrate secrets, weaken security or evade controls. ask is for anything a person should decide.`

// Reviewer that fails closed, denying when the model cannot be read
type Approver struct {
	Model   Completer
	ModelID string
	// Extra guidance such as the environment, the task and what counts as dangerous
	Instructions string
	// Per call context such as the working directory or the current task, appended to the prompt
	Context func(ctx context.Context, req policy.Request) string
	// Consulted when the model asks for a person, denying when nil
	Escalate policy.Approver
	// Review budget, thirty seconds when zero
	Timeout time.Duration
	// Output budget of the model, 256 when zero
	MaxTokens int
}

// What the model concluded about one call
type Verdict struct {
	Decision policy.Decision `json:"decision"`
	Reason   string          `json:"reason"`
}

// Decides from the model verdict, handing the request with the review attached to the escalation on ask
func (a Approver) Approve(ctx context.Context, req policy.Request) (policy.Answer, error) {
	v, err := a.Judge(ctx, req)
	if err != nil {
		return policy.Answer{}, err
	}
	switch v.Decision {
	case policy.Allow:
		return policy.Answer{Allowed: true, By: "judge", Reason: v.Reason}, nil
	case policy.Deny:
		return policy.Answer{By: "judge", Reason: v.Reason}, &policy.Denied{Call: req.Call, Reason: "judge: " + v.Reason}
	}
	if a.Escalate == nil {
		return policy.Answer{By: "judge", Reason: v.Reason}, &policy.Denied{Call: req.Call, Reason: "judge asked for a person and none is configured, " + v.Reason}
	}
	req.Review = &policy.Review{By: "judge", Decision: policy.Ask, Reason: v.Reason}
	return a.Escalate.Approve(ctx, req)
}

// Asks the model for its verdict on the call
func (a Approver) Judge(ctx context.Context, req policy.Request) (Verdict, error) {
	if a.Model == nil || a.ModelID == "" {
		return Verdict{}, errors.New("judge: model and model id are required")
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	maxTokens := a.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 256
	}
	prompt := system
	if a.Instructions != "" {
		prompt += "\n\n" + a.Instructions
	}
	text := describe(req)
	if a.Context != nil {
		if extra := a.Context(ctx, req); extra != "" {
			text += "\n\nContext:\n" + extra
		}
	}
	resp, err := a.Model.Complete(ctx, protocol.Request{
		Model:     a.ModelID,
		System:    prompt,
		MaxTokens: maxTokens,
		Messages:  []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: text}}}},
	})
	if err != nil {
		return Verdict{}, fmt.Errorf("judge: %w", err)
	}
	return parse(resp.Text())
}

// Renders the call with its arguments pretty printed and whatever the gate knew about it
func describe(req policy.Request) string {
	var args bytes.Buffer
	if err := json.Indent(&args, req.Call.Args, "", "  "); err != nil {
		args.Reset()
		args.Write(req.Call.Args)
	}
	var sb strings.Builder
	if req.Agent != "" {
		fmt.Fprintf(&sb, "Agent: %s\n", req.Agent)
	}
	fmt.Fprintf(&sb, "Tool: %s\n", req.Call.Name)
	if req.Tool != nil && req.Tool.Description != "" {
		fmt.Fprintf(&sb, "Purpose: %s\n", req.Tool.Description)
	}
	if req.Reason != "" {
		fmt.Fprintf(&sb, "Gate: %s\n", req.Reason)
	}
	fmt.Fprintf(&sb, "Arguments:\n%s", args.String())
	return sb.String()
}

// Reads the verdict object out of the model text, rejecting anything but the three decisions
func parse(text string) (Verdict, error) {
	start, end := strings.IndexByte(text, '{'), strings.LastIndexByte(text, '}')
	if start < 0 || end <= start {
		return Verdict{}, fmt.Errorf("judge: no verdict in %q", text)
	}
	var v Verdict
	if err := json.Unmarshal([]byte(text[start:end+1]), &v); err != nil {
		return Verdict{}, fmt.Errorf("judge: unreadable verdict: %w", err)
	}
	switch v.Decision {
	case policy.Allow, policy.Deny, policy.Ask:
		return v, nil
	}
	return Verdict{}, fmt.Errorf("judge: unknown decision %q", v.Decision)
}

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

func (a Approver) Approve(ctx context.Context, call protocol.ToolCall) (bool, error) {
	v, err := a.Judge(ctx, call)
	if err != nil {
		return false, err
	}
	switch v.Decision {
	case policy.Allow:
		return true, nil
	case policy.Deny:
		return false, &policy.Denied{Call: call, Reason: "judge: " + v.Reason}
	}
	if a.Escalate == nil {
		return false, &policy.Denied{Call: call, Reason: "judge asked for a person and none is configured, " + v.Reason}
	}
	return a.Escalate.Approve(ctx, call)
}

// Asks the model for its verdict on the call
func (a Approver) Judge(ctx context.Context, call protocol.ToolCall) (Verdict, error) {
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
	resp, err := a.Model.Complete(ctx, protocol.Request{
		Model:     a.ModelID,
		System:    prompt,
		MaxTokens: maxTokens,
		Messages:  []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: describe(call)}}}},
	})
	if err != nil {
		return Verdict{}, fmt.Errorf("judge: %w", err)
	}
	var text strings.Builder
	for _, p := range resp.Content {
		if t, ok := p.(protocol.Text); ok {
			text.WriteString(t.Text)
		}
	}
	return parse(text.String())
}

// Renders the call with its arguments pretty printed
func describe(call protocol.ToolCall) string {
	var args bytes.Buffer
	if err := json.Indent(&args, call.Args, "", "  "); err != nil {
		args.Reset()
		args.Write(call.Args)
	}
	return fmt.Sprintf("Tool: %s\nArguments:\n%s", call.Name, args.String())
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

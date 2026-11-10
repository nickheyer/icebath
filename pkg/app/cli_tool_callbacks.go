package app

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Header the terminal tool's sudo prompt asks under, the dispatch key of the CLI asker
const sudoPasswordHeader = "Sudo password required"

// Header prefix of a dangerous command approval asked without a custom title
const dangerousCommandHeaderPrefix = "⚠️  DANGEROUS COMMAND: "

// Routes the tool prompts that need the live prompt UI to the CLI's modal callbacks
func installInteractiveToolCallbacks(c *CLI) {
	if c.env != nil {
		c.env.Ask = cliAsker{c: c}
	}
	skills.SetSecretCaptureCallback(c.secretCaptureCallback)
	browser.SetUnlockPromptCallback(c.ctx, c.vaultUnlockCallback)
	browser.SetSaveLoginPromptCallback(c.ctx, c.vaultSaveLoginCallback)
	browser.SetCodePromptCallback(c.ctx, c.vaultCodeCallback)
}

// cliAsker answers the tool asker through the sudo, approval and clarify panels
type cliAsker struct{ c *CLI }

// Dispatches a tool question form to the sudo, approval or clarify panel by its shape
func (a cliAsker) Ask(ctx context.Context, questions []tools.Question) (tools.FormResult, error) {
	if len(questions) == 1 {
		q := questions[0]
		switch {
		case q.Header == sudoPasswordHeader && len(q.Choices) == 0:
			return a.askSudo(ctx)
		case isApprovalChoices(q.Choices):
			return a.askApproval(ctx, q)
		}
	}
	return a.askClarify(questions)
}

// Reports the once to deny choice list of a dangerous command approval
func isApprovalChoices(choices []string) bool {
	return len(choices) >= 2 && choices[0] == "once" && choices[len(choices)-1] == "deny"
}

// Answers a sudo password question, an empty password reading as skipped
func (a cliAsker) askSudo(ctx context.Context) (tools.FormResult, error) {
	password := a.c.sudoPasswordCallback(ctx)
	status := "answered"
	if password == "" {
		status = "skipped"
	}
	return tools.FormResult{Outcome: "answered", Answers: []tools.Answer{{Status: status, Text: password}}}, nil
}

// Answers an approval question, a timeout or interrupt surfacing as the context error
func (a cliAsker) askApproval(ctx context.Context, q tools.Question) (tools.FormResult, error) {
	description := strings.TrimPrefix(q.Header, dangerousCommandHeaderPrefix)
	allowPermanent, allowSession := false, false
	for _, choice := range q.Choices {
		allowPermanent = allowPermanent || choice == "always"
		allowSession = allowSession || choice == "session"
	}
	result, err := a.c.approvalCallback(ctx, q.Text, description, allowPermanent, allowSession, false)
	if err != nil {
		return tools.FormResult{}, err
	}
	if result == "timeout" {
		return tools.FormResult{}, context.DeadlineExceeded
	}
	if err := ctx.Err(); err != nil {
		return tools.FormResult{}, err
	}
	return tools.FormResult{Outcome: "answered", Answers: []tools.Answer{{Status: "answered", Selected: []string{result}}}}, nil
}

// Answers a clarify form through the agent's clarify callback, mapping its answers back per question
func (a cliAsker) askClarify(questions []tools.Question) (tools.FormResult, error) {
	entries := make([]any, len(questions))
	for i, q := range questions {
		choices := make([]any, len(q.Choices))
		for j, choice := range q.Choices {
			choices[j] = choice
		}
		entries[i] = map[string]any{"qid": q.Header, "question": q.Text, "choices": choices, "choices_offered": choices, "multi_select": q.MultiSelect}
	}
	reply := a.c.agentClarifyCallback()(entries)
	answers, _ := reply["answers"].(map[string]any)
	result := tools.FormResult{Outcome: cliStr(reply["outcome"], ""), Notice: cliStr(reply["notice"], ""), Answers: make([]tools.Answer, len(questions))}
	for i, q := range questions {
		switch v := answers[q.Header].(type) {
		case nil:
			result.Answers[i] = tools.Answer{Status: "skipped"}
		case []any:
			selected := make([]string, len(v))
			for j, item := range v {
				selected[j] = cliStr(item, "")
			}
			result.Answers[i] = tools.Answer{Status: "answered", Selected: selected}
		case []string:
			result.Answers[i] = tools.Answer{Status: "answered", Selected: v}
		default:
			result.Answers[i] = tools.Answer{Status: "answered", Text: cliStr(v, "")}
		}
	}
	return result, nil
}

// The clarify callback the agent's tools answer through, immediate in single query mode
func (c *CLI) agentClarifyCallback() func([]any) map[string]any {
	if c.singleQueryMode {
		return singleQueryClarifyCallback
	}
	return c.clarifyCallback
}

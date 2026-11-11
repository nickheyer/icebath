package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Text cut to limit with a truncated marker when longer
func oneshotTruncate(text string, limit int) string {
	if pystr.Len(text) <= limit {
		return text
	}
	return pystr.Rstrip(pystr.Slice(text, 0, limit)) + "\n…(truncated)"
}

const commitInstructions = "You write git commit messages. Given a diff of staged changes, write ONE concise Conventional Commits " +
	"message describing what the change does and why.\n" +
	"Rules:\n" +
	"- Subject line: type(scope): summary — imperative mood, lower-case, no trailing period, ≤ 72 " +
	"characters. Types: feat, fix, refactor, perf, docs, test, build, chore, style, ci.\n" +
	"- Omit the scope if it isn't obvious.\n" +
	"- Add a short body (wrapped at ~72 cols) ONLY when the change needs explanation; skip it for " +
	"small/obvious changes.\n" +
	"- Describe the actual change, never restate the diff line-by-line.\n" +
	"- Return ONLY the commit message text — no quotes, no markdown fences, no preamble."

// Variable rendered as str of its value, empty for a missing or falsy one
func oneshotVar(variables map[string]any, key string) string {
	v := variables[key]
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// Instructions and user input describing a staged diff, with recent subjects and a message to avoid
func commitMessageTemplate(variables map[string]any) (string, string) {
	diff := oneshotTruncate(oneshotVar(variables, "diff"), 12000)
	recent := oneshotTruncate(oneshotVar(variables, "recent_commits"), 1500)
	var parts []string
	if pystr.Strip(recent) != "" {
		parts = append(parts, "Recent commit subjects from this repo (match their style/conventions):\n"+recent)
	}
	if diff == "" {
		diff = "(no textual diff available)"
	}
	parts = append(parts, "Diff to describe:\n"+diff)
	if avoid := oneshotTruncate(pystr.Strip(oneshotVar(variables, "avoid")), 1000); avoid != "" {
		parts = append(parts, "You already proposed the message below and the user wants a different one. Write a NEW message with "+
			"different wording (and, if reasonable, a different emphasis or scope framing) — do not repeat "+
			"it:\n"+avoid)
	}
	return commitInstructions, strings.Join(parts, "\n\n")
}

// Named one shot prompt templates a surface reuses
var promptTemplates = map[string]func(map[string]any) (string, string){
	"commit_message": commitMessageTemplate,
}

// Instructions and user input of a registered template, a KeyError for an unknown name
func RenderOneshotTemplate(name string, variables map[string]any) (string, string, error) {
	template, ok := promptTemplates[name]
	if !ok {
		return "", "", fmt.Errorf("KeyError: %s", pyrepr.Repr("unknown one-shot template: "+name))
	}
	if variables == nil {
		variables = map[string]any{}
	}
	instructions, userInput := template(variables)
	return instructions, userInput, nil
}

// Inputs of one stateless request, nil Temperature sending none and nil MainRuntime resolving the task backend
type OneshotRequest struct {
	Instructions, UserInput, Template string
	Variables                         map[string]any
	Task                              string
	MaxTokens                         int
	Temperature                       *float64
	Timeout                           float64
	MainRuntime                       map[string]any
}

// Request with the Python defaults filled for zero task, token and timeout fields
func NewOneshotRequest() OneshotRequest {
	temp := 0.3
	return OneshotRequest{Task: "title_generation", MaxTokens: 1024, Temperature: &temp, Timeout: 60.0}
}

// Runs one stateless LLM request outside any conversation and returns its fence stripped text
func RunOneshot(ctx context.Context, req OneshotRequest) (string, error) {
	instructions, userInput := req.Instructions, req.UserInput
	if req.Template != "" {
		var err error
		if instructions, userInput, err = RenderOneshotTemplate(req.Template, req.Variables); err != nil {
			return "", err
		}
	}
	hasInstructions := pystr.Strip(instructions) != ""
	if !hasInstructions && pystr.Strip(userInput) == "" {
		return "", errors.New("ValueError: run_oneshot requires a template or instructions/user_input")
	}
	messages := []any{}
	if hasInstructions {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}
	messages = append(messages, map[string]any{"role": "user", "content": userInput})
	kwargs := map[string]any{
		"task": req.Task, "messages": messages, "max_tokens": req.MaxTokens, "timeout": req.Timeout,
		"main_runtime": req.MainRuntime,
	}
	if req.Temperature != nil {
		kwargs["temperature"] = *req.Temperature
	} else {
		kwargs["temperature"] = nil
	}
	response, err := provider.CallLLM(ctx, kwargs)
	if err != nil {
		return "", err
	}
	return stripCodeFence(pystr.Strip(provider.ExtractContentOrReasoning(response, -1))), nil
}

// Text without the single wrapping code fence a model may add
func stripCodeFence(text string) string {
	if !strings.HasPrefix(text, "```") {
		return text
	}
	lines := pystr.Splitlines(text, false)
	if len(lines) >= 2 && pystr.Strip(lines[len(lines)-1]) == "```" {
		return pystr.Strip(strings.Join(lines[1:len(lines)-1], "\n"))
	}
	return text
}

package agent

import "context"

// One shot generation from plain arguments over the default request timeout
func RunOneshotCall(ctx context.Context, instructions, userInput, template string, variables map[string]any, task string,
	maxTokens int, temperature float64, mainRuntime map[string]any) (string, error) {
	req := NewOneshotRequest()
	req.Instructions, req.UserInput, req.Template, req.Variables, req.Task = instructions, userInput, template, variables, task
	req.MaxTokens, req.Temperature, req.MainRuntime = maxTokens, &temperature, mainRuntime
	return RunOneshot(ctx, req)
}

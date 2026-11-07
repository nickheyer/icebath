package codeexec

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

const computerUseToolDescription = "Universal desktop control via cua-driver (macOS, Windows, Linux). Works with any " +
	"tool-capable model (Anthropic, OpenAI, OpenRouter, local vLLM, " +
	"etc.). Background computer-use: does NOT steal the user's cursor " +
	"or keyboard focus."

func init() { registerComputerUseTool() }

// Registers computer_use in its own toolset with the schema, the active provider check and the handler
func registerComputerUseTool() {
	tools.Register(tools.Def{
		Name: "computer_use", Toolset: "computer_use", Schema: getComputerUseToolSchema(),
		Handler: computerUseHandler, Check: computerUseCheck, RequiresEnv: []string{},
		Description: computerUseToolDescription,
	})
}

// Runs one call through handle_computer_use, a raised error surfacing as the dispatcher's failure result
func computerUseHandler(ctx context.Context, call *tools.Call) string {
	res, err := handleComputerUse(ctx, call.Env, call.Args, call.SessionID)
	if err != nil {
		panic(err)
	}
	return tools.NormalizeHandlerResult(call.Env, "computer_use", res)
}

// Availability of the selected backend, a failing probe raising into the registry's unavailable path
func computerUseCheck(env *tools.Env) bool {
	ok, err := checkComputerUseRequirements(env)
	if err != nil {
		panic(err)
	}
	return ok
}

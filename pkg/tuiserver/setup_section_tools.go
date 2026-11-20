package tuiserver

import (
	"context"
	"io"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/setupflow"
	"github.com/nickheyer/icebath/pkg/tools"
)

// MCP tool configurator surface over a setup flow asker, the first failed question kept for the flow to return
type mcpFlowUI struct {
	ask setupflow.Asker
	out io.Writer
	err error
}

func (u *mcpFlowUI) Out() io.Writer                        { return u.out }
func (u *mcpFlowUI) Color(text string, _ ...string) string { return text }
func (u *mcpFlowUI) Interactive() bool                     { return true }

// Keeps the first error a question ended with
func (u *mcpFlowUI) failed(err error) bool {
	if err != nil && u.err == nil {
		u.err = err
	}
	return err != nil
}

func (u *mcpFlowUI) Prompt(question, def string, password bool) string {
	answer, err := u.ask.Text(question, def, password)
	if u.failed(err) {
		return def
	}
	return answer
}

func (u *mcpFlowUI) YesNo(question string, def bool) bool {
	start := 1
	if def {
		start = 0
	}
	idx, err := u.ask.Choose(question, []setupflow.Option{{Title: "Yes"}, {Title: "No"}}, start)
	if u.failed(err) {
		return def
	}
	return idx == 0
}

func (u *mcpFlowUI) Checklist(title string, items []string, selected, cancelReturns map[int]bool) map[int]bool {
	rows := make([]setupflow.Item, len(items))
	for i, item := range items {
		rows[i] = setupflow.Item{Title: item}
	}
	chosen, err := u.ask.Check(title, rows, selected, nil)
	if u.failed(err) {
		return cancelReturns
	}
	return chosen
}

func (u *mcpFlowUI) SingleSelect(title string, items []string) (int, bool) {
	options := make([]setupflow.Option, len(items))
	for i, item := range items {
		options[i] = setupflow.Option{Title: item}
	}
	idx, err := u.ask.Choose(title, options, 0)
	if u.failed(err) {
		return 0, false
	}
	return idx, true
}

func init() {
	setupFlows["tools"] = func(ctx context.Context, h string, _ map[string]string, ask setupflow.Asker) error {
		return RunToolsSetupFlow(ctx, h, ask)
	}
}

// Tools section of /setup asked through a flow asker, the Blank Slate tool selector included
func RunToolsSetupFlow(ctx context.Context, h string, ask setupflow.Asker) error {
	cfg, err := config.LoadReadonly(ctx, h, nil)
	if err != nil {
		return err
	}
	env, err := setupToolEnv(h, cfg, "")
	if err != nil {
		return err
	}
	return tools.RunToolsFlow(ctx, env, ask, tools.ToolsFlowHooks{
		ConfigureMCP: func(ctx context.Context, cfg config.Tree, ask setupflow.Asker) error {
			ui := &mcpFlowUI{ask: ask, out: setupflow.Lines(ask)}
			if err := mcp.ConfigureMCPToolsInteractive(ctx, env, ui, cfg); err != nil {
				return err
			}
			return ui.err
		},
		XAILogin: func(ctx context.Context, h string, out io.Writer) bool {
			return provider.XAIOAuthSectionLogin(setupPoolEnv(ctx, h), out)
		},
	})
}

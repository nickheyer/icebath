package tools

import (
	"context"
	"io"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/setupflow"
)

// Steps of the tools flow that live in packages above this one, installed by the gateway server
type ToolsFlowHooks struct {
	// Shared metrics consent question writing both consent keys
	Consent func(ctx context.Context, h string, cfg config.Tree, ask setupflow.Asker) error
	// Nous Portal entitlement of a capability, offering a Portal login when signed out
	PortalAccess func(ctx context.Context, h string, out io.Writer, capability, coverage string, ask setupflow.Asker) bool
	// Per server MCP tool selection
	ConfigureMCP func(ctx context.Context, cfg config.Tree, ask setupflow.Asker) error
	// xAI Grok OAuth device login saving tokens only
	XAILogin func(ctx context.Context, h string, out io.Writer) bool
}

// Panics naming a flow step the caller of RunToolsFlow left out
func requireFlowHook(name string, installed bool) {
	if !installed {
		panic("tools.ToolsFlowHooks." + name + " is not installed")
	}
}

// Console printing every line through the asker without color
func flowConsole(ask setupflow.Asker) *cliui.Console {
	return &cliui.Console{Out: setupflow.Lines(ask), Getenv: func(name string) (string, bool) { return "1", name == "NO_COLOR" },
		StdinTTY: func() bool { return false }, StdoutTTY: func() bool { return false }}
}

// Icebath setup tools asked through the asker, the platform menu until Done then the section save
func RunToolsFlow(ctx context.Context, env *Env, ask setupflow.Asker, hooks ToolsFlowHooks) error {
	s := &ToolsSetup{Ctx: ctx, Env: env, Con: flowConsole(ask), Ask: ask, Flow: hooks}
	h := s.home()
	if err := config.EnsureIcebathHome(ctx, h); err != nil {
		return err
	}
	if _, err := config.BackupConfig(ctx, home.ConfigPath(h), "pre-setup", config.DefaultBackupKeep); err != nil {
		return err
	}
	cfg, err := config.LoadContext(ctx, h, nil)
	if err != nil {
		return err
	}
	if err := s.ToolsCommand(nil, false, cfg); err != nil {
		return err
	}
	return config.SaveConfig(ctx, h, cfg, config.SaveOptions{})
}

// Options of a plain list of labels
func flowOptions(labels []string) []setupflow.Option {
	out := make([]setupflow.Option, len(labels))
	for i, label := range labels {
		out[i] = setupflow.Option{Title: label}
	}
	return out
}

// Single choice through the asker when the run has one, else the arrow key menu over console labels
func (s *ToolsSetup) chooseOption(question string, options []setupflow.Option, consoleLabels []string, def int) (int, error) {
	if s.Ask != nil {
		return s.Ask.Choose(question, options, def)
	}
	return toolsPromptChoice(s, question, consoleLabels, def)
}

// One typed answer through the asker without the console indent when the run has one, else the console prompt
func (s *ToolsSetup) promptText(question, def string, secret bool) (string, error) {
	if s.Ask != nil {
		return s.Ask.Text(strings.TrimLeft(question, " "), def, secret)
	}
	return s.Con.Prompt(question, def, secret), nil
}

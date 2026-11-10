package tools

import (
	"context"
	"io"
	"os"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
)

// ToolsCommandHooks are the provider, setup wizard and MCP entry points the tools command reaches, each installed by its package
var ToolsCommandHooks struct {
	ApplyNousManagedDefaults func(ctx context.Context, h string, cfg map[string]any, enabledToolsets []string, forceFresh bool) map[string]bool
	VisionClientReady        func(ctx context.Context, h string) (bool, error)
	SetupTelemetry           func(ctx context.Context, h string, out io.Writer, cfg map[string]any) error
	ConfigureMCPTools        func(ctx context.Context, con *cliui.Console, cfg config.Tree) error
	ToolsAction              func(ctx context.Context, con *cliui.Console, action, platform string, names []string) error
}

// Panics naming a tools command hook its package never installed
func requireToolsCommandHook(name, owner string, installed bool) {
	if !installed {
		panic("tools.ToolsCommandHooks." + name + " is not installed, " + owner + " assigns it at init")
	}
}

// Seams over the names tools_config imports, the targets its tests patch
var (
	applyNousManagedDefaults = func(s *ToolsSetup, cfg config.Tree, enabledToolsets map[string]bool, forceFresh bool) map[string]bool {
		fn := ToolsCommandHooks.ApplyNousManagedDefaults
		requireToolsCommandHook("ApplyNousManagedDefaults", "pkg/provider", fn != nil)
		return fn(s.ctx(), s.home(), map[string]any(cfg), sortedSet(enabledToolsets), forceFresh)
	}
	resolveVisionProviderClient = func(s *ToolsSetup) (bool, error) {
		fn := ToolsCommandHooks.VisionClientReady
		requireToolsCommandHook("VisionClientReady", "pkg/provider", fn != nil)
		return fn(s.ctx(), s.home())
	}
	setupTelemetry = func(s *ToolsSetup, cfg config.Tree) error {
		fn := ToolsCommandHooks.SetupTelemetry
		requireToolsCommandHook("SetupTelemetry", "pkg/provider", fn != nil)
		return fn(s.ctx(), s.home(), s.Con.Out, map[string]any(cfg))
	}
	configureMCPToolsInteractive = func(s *ToolsSetup, cfg config.Tree) error {
		fn := ToolsCommandHooks.ConfigureMCPTools
		requireToolsCommandHook("ConfigureMCPTools", "pkg/commands", fn != nil)
		return fn(s.ctx(), s.Con, cfg)
	}
	toolsDisableEnableCommand = func(s *ToolsSetup, action, platform string, names []string) error {
		fn := ToolsCommandHooks.ToolsAction
		requireToolsCommandHook("ToolsAction", "pkg/commands", fn != nil)
		return fn(s.ctx(), s.Con, action, platform, names)
	}
	toolsConfigLoad     = func(s *ToolsSetup) (config.Tree, error) { return s.loadConfig() }
	toolsConfigSave     = func(s *ToolsSetup, cfg config.Tree) error { return s.saveConfig(cfg) }
	getEnabledPlatforms = func(s *ToolsSetup) ([]string, error) { return GetEnabledPlatforms(s.ctx(), s.home()) }
	toolsChecklist      = func(s *ToolsSetup, title string, items []string, selected, cancelReturns map[int]bool, statusFn func(map[int]bool) string) (map[int]bool, error) {
		return s.Con.CursesChecklist(s.ctx(), title, items, selected, cancelReturns, statusFn)
	}
	toolsPromptChoice = func(s *ToolsSetup, question string, choices []string, def int) (int, error) {
		items := make([]cliui.RadioItem, len(choices))
		for i, c := range choices {
			items[i] = cliui.PlainItem(c)
		}
		cancel := def
		return s.Con.CursesRadiolist(s.ctx(), question, items, def, cliui.RadiolistOptions{CancelReturns: &cancel})
	}
	toolsCommandStderr io.Writer = os.Stderr
)

// Single select menu with arrow keys, cancel keeping the default
func (s *ToolsSetup) promptChoice(question string, choices []string, def int) (int, error) {
	return toolsPromptChoice(s, question, choices, def)
}

// Console printing to w and reading standard input, the process console when w is standard output
var toolsConfigConsoleFor = func(w io.Writer) *cliui.Console {
	if f, ok := w.(*os.File); ok && f == os.Stdout {
		return cliui.Default()
	}
	return &cliui.Console{In: os.Stdin, Out: w}
}

// Prints one line wrapped in the color codes on the run console
func (s *ToolsSetup) printColor(text string, codes ...string) { s.println(s.color(text, codes...)) }

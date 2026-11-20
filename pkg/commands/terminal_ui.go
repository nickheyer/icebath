package commands

import (
	"context"
	"errors"
	"io"
	"os"

	xterm "github.com/charmbracelet/x/term"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/spf13/cobra"
)

// Installs the terminal surface behind the tools command MCP configurator and its list, enable and disable actions
func init() {
	tools.ToolsCommandHooks.ConfigureMCPTools = func(ctx context.Context, con *cliui.Console, cfg config.Tree) error {
		env, err := mcpCLIEnv(ctx)
		if err != nil {
			return err
		}
		return mcp.ConfigureMCPToolsInteractive(ctx, env, &terminalUI{ctx: ctx, c: con}, cfg)
	}
	tools.ToolsCommandHooks.ToolsAction = func(ctx context.Context, con *cliui.Console, action, platform string, names []string) error {
		return mcp.ToolsDisableEnableCommand(ctx, &terminalUI{ctx: ctx, c: con}, action, platform, names)
	}
}

// Catalog, picker and mcp config terminal surface over the cli_output and curses_ui port
type terminalUI struct {
	ctx context.Context
	c   *cliui.Console
}

// Builds the terminal surface of a command over its input and output streams
func newTerminalUI(cmd *cobra.Command) mcp.CLIConfigUI {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	in, out := cmd.InOrStdin(), cmd.OutOrStdout()
	if in == os.Stdin && out == os.Stdout {
		return &terminalUI{ctx: ctx, c: cliui.Default()}
	}
	return &terminalUI{ctx: ctx, c: &cliui.Console{In: in, Out: out}}
}

func (u *terminalUI) Out() io.Writer { return u.c.Out }

func (u *terminalUI) Color(text string, codes ...string) string { return u.c.Color(text, codes...) }

func (u *terminalUI) Prompt(question, def string, password bool) string {
	return u.c.Prompt(question, def, password)
}

func (u *terminalUI) YesNo(question string, def bool) bool {
	return u.c.PromptYesNo(u.ctx, question, def)
}

// Seam over the curses checklist the terminal surface draws, the patch target of the MCP tool picker tests
var terminalChecklist = func(u *terminalUI, title string, items []string, selected, cancelReturns map[int]bool) (map[int]bool, error) {
	return u.c.CursesChecklist(u.ctx, title, items, selected, cancelReturns, nil)
}

// Checklist returning the chosen set, cancelReturns on cancel and the selection when it is nil
func (u *terminalUI) Checklist(title string, items []string, selected, cancelReturns map[int]bool) map[int]bool {
	chosen, err := terminalChecklist(u, title, items, selected, cancelReturns)
	if err != nil {
		panic(err)
	}
	return chosen
}

// Single select with the Cancel row, false on cancel
func (u *terminalUI) SingleSelect(title string, items []string) (int, bool) {
	idx, ok, err := u.c.CursesSingleSelect(u.ctx, title, items, 0, "Cancel", false)
	if err != nil {
		panic(err)
	}
	return idx, ok
}

// Interactive reports whether standard input is a terminal
func (u *terminalUI) Interactive() bool {
	f, ok := u.c.In.(*os.File)
	return ok && xterm.IsTerminal(f.Fd())
}

// Raw input line, false after printing a newline on EOF or Ctrl-C
func (u *terminalUI) Input(display string) (string, bool) {
	line, err := u.c.Input(display)
	if errors.Is(err, cliui.ErrEOF) || errors.Is(err, cliui.ErrInterrupt) {
		io.WriteString(u.c.Out, "\n")
		return "", false
	}
	if err != nil {
		panic(err)
	}
	return line, true
}

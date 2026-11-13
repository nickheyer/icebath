package commands

import (
	"context"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/spf13/cobra"
)

// Binds every icebath mcp path and the tools list, enable and disable paths to pkg/mcp
func (c *CLI) bindMCP() {
	for _, path := range []string{"mcp", "mcp add", "mcp remove", "mcp list", "mcp test", "mcp configure", "mcp login",
		"mcp reauth", "mcp picker", "mcp catalog", "mcp install"} {
		action := strings.TrimPrefix(strings.TrimPrefix(path, "mcp"), " ")
		c.Bind(path, func(cmd *cobra.Command, args []string) error { return runMCP(cmd, action, args, c.mcpAddArgs) })
	}
	for _, action := range []string{"list", "disable", "enable"} {
		c.Bind("tools "+action, func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			platform, _ := cmd.Flags().GetString("platform")
			return mcp.ToolsDisableEnableCommand(ctx, newTerminalUI(cmd), action, platform, args)
		})
	}
}

// Server name and the stdio argv after --args of an mcp add line, refusing extra positionals like argparse
func mcpAddPositionals(cmd *cobra.Command, args []string, remainder bool) (string, []string, error) {
	positionals, rest := args, []string{}
	if at := cmd.ArgsLenAtDash(); remainder && at >= 0 {
		positionals, rest = args[:at], args[at:]
	}
	if len(positionals) == 0 {
		return "", nil, &ArgparseError{Cmd: cmd, Msg: "the following arguments are required: name"}
	}
	if len(positionals) > 1 {
		return "", nil, &ArgparseError{Cmd: cmd, Msg: "unrecognized arguments: " + strings.Join(positionals[1:], " ")}
	}
	return positionals[0], rest, nil
}

// Parses one mcp command line into MCPArgs and runs it, a nonzero status becoming an ExitError
func runMCP(cmd *cobra.Command, action string, args []string, remainder bool) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	f := cmd.Flags()
	a := mcp.MCPArgs{Action: action}
	switch action {
	case "add":
		name, rest, err := mcpAddPositionals(cmd, args, remainder)
		if err != nil {
			return err
		}
		a.Name, a.Args = name, rest
		a.URL, _ = f.GetString("url")
		a.Command, _ = f.GetString("command")
		a.Auth, _ = f.GetString("auth")
		a.Preset, _ = f.GetString("preset")
		a.Env, _ = f.GetStringArray("env")
		if f.Changed("connect-timeout") {
			v, _ := f.GetFloat64("connect-timeout")
			a.ConnectTimeout = &v
		}
	case "remove", "test", "configure":
		a.Name = args[0]
	case "login":
		a.Name = args[0]
		a.Flow, _ = f.GetString("flow")
	case "reauth":
		if len(args) > 0 {
			a.Name = args[0]
		}
		a.All, _ = f.GetBool("all")
	case "install":
		a.Identifier = args[0]
	}
	env, err := mcpCLIEnv(ctx)
	if err != nil {
		return err
	}
	code, err := mcp.MCPCommand(ctx, env, newTerminalUI(cmd), a)
	if err != nil {
		return err
	}
	if code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}

// Tool environment of the active profile for CLI driven MCP commands
func mcpCLIEnv(ctx context.Context) (*tools.Env, error) {
	h := home.Dir(ctx)
	secrets := config.NewSecrets(h)
	tree, err := config.Load(h, secrets.Lookup())
	if err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return &tools.Env{Home: h, Config: tree, Secrets: secrets, Cwd: cwd, Platform: "cli"}, nil
}

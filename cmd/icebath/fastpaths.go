package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/nickheyer/icebath/pkg/agent"
	"log/slog"
	"os"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/spf13/cobra"
)

// The parsed top level and chat namespace of hermes_cli/main.py, read from the cobra tree
type chatArgs struct {
	Command, SubCommand                 string
	Query, QueryFile, Image, Model      string
	Provider, Toolsets, Reasoning       string
	Resume, Continue, In, Source        string
	Oneshot, UsageFile, APIKey, BaseURL string
	OutputFormat                        string
	Skills                              []string
	MaxTurns                            int
	RunBudget                           float64
	Version, OneshotExit, Verbose       bool
	Quiet, Worktree, Checkpoints        bool
	PassSessionID, IgnoreUserConfig     bool
	IgnoreRules, SafeMode, Yolo         bool
	AcceptHooks, TUI, TUINative, TUIDev bool
	CLI, NoRestoreCWD, CreateIfMissing  bool
	Compact                             bool
	noninteractiveConfigValidated       bool
}

// Reads the flags of the root and the dispatched command into one namespace
func parsedArgs(cli *commands.CLI, cmd *cobra.Command, args []string) *chatArgs {
	a := &chatArgs{}
	path := commandPath(cli, cmd)
	if path != "" {
		a.Command = firstWord(path)
		a.SubCommand = secondWord(path)
	}
	read := func(f interface {
		GetString(string) (string, error)
		GetBool(string) (bool, error)
	}, flags *cobra.Command) {
		str := func(name string, dst *string) {
			if flags.Flags().Lookup(name) != nil {
				if v, err := flags.Flags().GetString(name); err == nil && v != "" {
					*dst = v
				}
			}
		}
		boolean := func(name string, dst *bool) {
			if flags.Flags().Lookup(name) != nil {
				if v, err := flags.Flags().GetBool(name); err == nil && v {
					*dst = true
				}
			}
		}
		str("query", &a.Query)
		str("query-file", &a.QueryFile)
		str("image", &a.Image)
		str("model", &a.Model)
		str("provider", &a.Provider)
		str("toolsets", &a.Toolsets)
		str("reasoning", &a.Reasoning)
		str("resume", &a.Resume)
		str("continue", &a.Continue)
		str("in", &a.In)
		str("source", &a.Source)
		str("usage-file", &a.UsageFile)
		str("api-key", &a.APIKey)
		str("base-url", &a.BaseURL)
		str("format", &a.OutputFormat)
		if flags.Flags().Lookup("oneshot") != nil {
			if flags.Flags().Lookup("oneshot").Value.Type() == "bool" {
				boolean("oneshot", &a.OneshotExit)
			} else {
				str("oneshot", &a.Oneshot)
			}
		}
		if flags.Flags().Lookup("skills") != nil {
			if v, err := flags.Flags().GetStringArray("skills"); err == nil && len(v) > 0 {
				a.Skills = v
			}
		}
		if flags.Flags().Lookup("max-turns") != nil {
			if v, err := flags.Flags().GetInt("max-turns"); err == nil && v != 0 {
				a.MaxTurns = v
			}
		}
		if flags.Flags().Lookup("run-budget") != nil {
			if v, err := flags.Flags().GetFloat64("run-budget"); err == nil && v != 0 {
				a.RunBudget = v
			}
		}
		boolean("version", &a.Version)
		boolean("verbose", &a.Verbose)
		boolean("quiet", &a.Quiet)
		boolean("worktree", &a.Worktree)
		boolean("checkpoints", &a.Checkpoints)
		boolean("pass-session-id", &a.PassSessionID)
		boolean("ignore-user-config", &a.IgnoreUserConfig)
		boolean("ignore-rules", &a.IgnoreRules)
		boolean("safe-mode", &a.SafeMode)
		boolean("yolo", &a.Yolo)
		boolean("accept-hooks", &a.AcceptHooks)
		boolean("tui", &a.TUI)
		boolean("native", &a.TUINative)
		boolean("dev", &a.TUIDev)
		boolean("cli", &a.CLI)
		boolean("no-restore-cwd", &a.NoRestoreCWD)
		boolean("create-if-missing", &a.CreateIfMissing)
	}
	read(cli.Root.Flags(), cli.Root)
	if cmd != cli.Root {
		read(cmd.Flags(), cmd)
	}
	if a.OutputFormat == "" {
		a.OutputFormat = "text"
	}
	return a
}

// Command words of the dispatched cobra command below the root
func commandPath(cli *commands.CLI, cmd *cobra.Command) string {
	if cmd == nil || cmd == cli.Root {
		return ""
	}
	path := cmd.CommandPath()
	root := cli.Root.Name()
	if len(path) > len(root) && path[:len(root)] == root {
		path = path[len(root):]
	}
	if len(path) > 0 && path[0] == ' ' {
		path = path[1:]
	}
	return path
}

func firstWord(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			return s[:i]
		}
	}
	return s
}

func secondWord(s string) string {
	first := firstWord(s)
	if len(s) <= len(first)+1 {
		return ""
	}
	return firstWord(s[len(first)+1:])
}

// Commands that can run an agent turn, bare chat, acp and rl, plus cron run or tick and gateway run
var agentCommands = map[string]bool{"": true, "chat": true, "acp": true, "rl": true}

var agentSubcommands = map[string]map[string]bool{
	"cron":    {"run": true, "tick": true},
	"gateway": {"run": true},
}

func agentSubcommandSelected(a *chatArgs) bool {
	subs, ok := agentSubcommands[a.Command]
	return ok && subs[a.SubCommand]
}

// Only chat commands launch the TUI, decided the way cmd_chat decides
func isTUIChatLaunch(a *chatArgs) bool {
	if bypassChatLaunch(a) {
		return false
	}
	if a.TUI || os.Getenv("ICEBATH_TUI") == "1" {
		return true
	}
	if a.Command != "" && a.Command != "chat" {
		return false
	}
	return resolveUseTUI(a)
}

func shouldBackgroundMCPStartup(a *chatArgs) bool {
	return !isTUIChatLaunch(a) && (a.Command == "" || a.Command == "chat" || a.Command == "rl")
}

// Every bare chat turn executes in the gateway, which owns MCP discovery itself
func chatRunsAtGateway(a *chatArgs) bool {
	return a.Command == "" || a.Command == "chat"
}

func applySafeMode(a *chatArgs) {
	if !a.SafeMode {
		return
	}
	os.Setenv("ICEBATH_SAFE_MODE", "1")
	os.Setenv("ICEBATH_IGNORE_USER_CONFIG", "1")
	os.Setenv("ICEBATH_IGNORE_RULES", "1")
}

func applyUserConfigBypass(a *chatArgs) {
	if a.IgnoreUserConfig {
		os.Setenv("ICEBATH_IGNORE_USER_CONFIG", "1")
	}
}

// Fails closed before a non interactive invocation initializes providers on a broken config
func guardNoninteractiveUserConfig(a *chatArgs) {
	if a.noninteractiveConfigValidated {
		return
	}
	if a.Oneshot == "" && a.Query == "" {
		return
	}
	ctx := context.Background()
	if err := config.RequireParseableUserConfig(ctx, home.Dir(ctx), a.IgnoreUserConfig || a.SafeMode); err != nil {
		var invalid *config.InvalidUserConfigError
		if errors.As(err, &invalid) {
			fmt.Fprintln(os.Stderr, "Error: "+err.Error())
			exit(2)
		}
	}
	a.noninteractiveConfigValidated = true
}

// Discovers plugins, MCP servers and hooks for commands that can run an agent turn
func prepareAgentStartup(a *chatArgs) {
	if a.Yolo {
		os.Setenv("ICEBATH_YOLO_MODE", "1")
	}
	if (a.Command == "" || a.Command == "chat") && bypassChatLaunch(a) {
		return
	}
	applySafeMode(a)
	applyUserConfigBypass(a)
	guardNoninteractiveUserConfig(a)
	if !(agentCommands[a.Command] || agentSubcommandSelected(a)) {
		return
	}
	ctx := context.Background()
	if !(isTUIChatLaunch(a) || chatRunsAtGateway(a)) {
		plugins.StartBackgroundPluginDiscovery(ctx)
	}
	var toolsets any
	if a.Toolsets != "" {
		toolsets = a.Toolsets
	}
	mcp.SetServerFilter(toolsets)
	if chatRunsAtGateway(a) {
		if a.AcceptHooks {
			os.Setenv("ICEBATH_ACCEPT_HOOKS", "1")
		}
		return
	}
	if shouldBackgroundMCPStartup(a) {
		mcp.StartBackgroundDiscovery(ctx, processToolsEnv(ctx), slog.Default(), "cli-mcp-discovery")
	}
	cfg, err := config.Load(home.Dir(ctx), nil)
	if err != nil {
		slog.Debug(fmt.Sprintf("shell-hook registration failed at CLI startup: %v", err))
		return
	}
	interactive := a.Command == "" || a.Command == "chat" || a.Command == "rl"
	if _, err := plugins.RegisterShellHooksFromConfig(ctx, map[string]any(cfg), a.AcceptHooks, interactive); err != nil {
		slog.Debug(fmt.Sprintf("shell-hook registration failed at CLI startup: %v", err))
		return
	}
	if a.AcceptHooks && a.Command == "gateway" {
		plugins.AcceptShellHooksForProcess()
	}
	agent.RegisterOutboundWebhooksFromConfig(ctx, map[string]any(cfg))
}

// The process wide tool environment of the bound home
func processToolsEnv(ctx context.Context) *tools.Env {
	h := home.Dir(ctx)
	cfg, _ := config.LoadReadonly(ctx, h, nil)
	cwd, _ := os.Getwd()
	return &tools.Env{Home: h, Config: cfg, Cwd: cwd, Log: slog.Default()}
}

// Top level --oneshot, the transport launcher that never returns
func runOneshotFromArgs(a *chatArgs) int {
	return launchGatewayChat(a)
}

// Top level --resume or --continue with no subcommand is a chat shortcut
func promoteTopLevelResume(a *chatArgs) {
	if (a.Resume != "" || a.Continue != "") && a.Command == "" {
		a.Command = "chat"
	}
}

// The light chat parser, root flags and chat only, parsed without the subcommand tree
func lightChatArgs(argv []string) (*chatArgs, bool) {
	cli := commands.NewCLI()
	argv = commands.CoalesceSessionNameArgs(argv)
	cmd, rest, err := cli.Root.Find(argv)
	if err != nil || cmd == nil {
		return nil, false
	}
	if cmd != cli.Root && cmd.Name() != "chat" {
		return nil, false
	}
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	if err := cmd.ParseFlags(rest); err != nil {
		return nil, false
	}
	if cmd == cli.Root {
		for _, positional := range cmd.Flags().Args() {
			if positional != "" {
				return nil, false
			}
		}
	}
	a := parsedArgs(cli, cmd, cmd.Flags().Args())
	if cmd.Name() == "chat" {
		a.Command = "chat"
	}
	return a, true
}

// Dispatches an unambiguous serve without the full CLI tree, Desktop's cold start path
func tryFastServeLaunch(argv []string) (bool, int) {
	if os.Getenv("ICEBATH_DISABLE_FAST_SERVE_LAUNCH") == "1" {
		return false, 0
	}
	if len(argv) == 0 || argv[0] != "serve" || containsArg(argv, "-h") || containsArg(argv, "--help") {
		return false, 0
	}
	if containerExecInfo() != nil {
		return false, 0
	}
	cli := commands.NewCLI()
	cmd, rest, err := cli.Root.Find(argv)
	if err != nil || cmd == nil || cmd.Name() != "serve" {
		return false, 0
	}
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	if err := cmd.ParseFlags(rest); err != nil || len(cmd.Flags().Args()) > 0 {
		return false, 0
	}
	code, msg := commands.ExitCode(status(cmdDashboard(cli, cmd, cmd.Flags().Args())))
	if msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}
	return true, code
}

// Fast path for unambiguous interactive chat launches on every host
func tryFastChatLaunch(argv []string) (bool, int) {
	if os.Getenv("ICEBATH_DISABLE_FAST_CHAT_LAUNCH") == "1" || containsArg(argv, "-h") || containsArg(argv, "--help") {
		return false, 0
	}
	if containerExecInfo() != nil || wantsTUIEarly(argv) {
		return false, 0
	}
	if first := firstPositional(argv); first != "" && first != "chat" {
		return false, 0
	}
	a, ok := lightChatArgs(argv)
	if !ok || a.Version || (a.Command != "" && a.Command != "chat") {
		return false, 0
	}
	if a.Yolo {
		os.Setenv("ICEBATH_YOLO_MODE", "1")
	}
	prepareAgentStartup(a)
	if a.Oneshot != "" {
		return true, runOneshotFromArgs(a)
	}
	promoteTopLevelResume(a)
	return true, cmdChat(a)
}

// Obvious Termux non TUI chat, oneshot and version paths on the light parser
func tryTermuxFastCLILaunch(argv []string) (bool, int) {
	if !isTermuxStartupEnvironment() || os.Getenv("ICEBATH_TERMUX_DISABLE_FAST_CLI") == "1" {
		return false, 0
	}
	if containsArg(argv, "-h") || containsArg(argv, "--help") || wantsTUIEarly(argv) {
		return false, 0
	}
	if isGlobalFastVersionArgv(argv) {
		printVersionInfo(true)
		return true, 0
	}
	first := firstPositional(argv)
	hasOneshot := false
	for _, arg := range argv {
		if arg == "-z" || arg == "--oneshot" || len(arg) > 10 && arg[:10] == "--oneshot=" {
			hasOneshot = true
		}
	}
	if !hasOneshot && first != "" && first != "chat" {
		return false, 0
	}
	a, ok := lightChatArgs(argv)
	if !ok {
		return false, 0
	}
	if a.Version {
		printVersionInfo(true)
		return true, 0
	}
	if a.Oneshot != "" {
		prepareAgentStartup(a)
		return true, runOneshotFromArgs(a)
	}
	promoteTopLevelResume(a)
	if a.Command == "" || a.Command == "chat" {
		if a.Query == "" && a.Image == "" {
			a.Compact = true
			os.Setenv("ICEBATH_DEFER_AGENT_STARTUP", "1")
			os.Setenv("ICEBATH_FAST_STARTUP_BANNER", "1")
			if a.AcceptHooks {
				os.Setenv("ICEBATH_ACCEPT_HOOKS", "1")
			}
		} else {
			prepareAgentStartup(a)
		}
		return true, cmdChat(a)
	}
	return false, 0
}

// Obvious Termux TUI invocations launched before building every subparser
func tryTermuxFastTUILaunch(argv []string) (bool, int) {
	if !isTermuxStartupEnvironment() || containsArg(argv, "-h") || containsArg(argv, "--help") {
		return false, 0
	}
	if !wantsTUIEarly(argv) {
		return false, 0
	}
	if first := firstPositional(argv); first != "" && first != "chat" {
		return false, 0
	}
	a, ok := lightChatArgs(argv)
	if !ok || a.Version || a.Oneshot != "" || (a.Command != "" && a.Command != "chat") {
		return false, 0
	}
	if !resolveUseTUI(a) {
		return false, 0
	}
	return true, cmdChat(a)
}

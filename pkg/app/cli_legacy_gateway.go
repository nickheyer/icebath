package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/pflag"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewayrun"
	"github.com/nickheyer/icebath/pkg/i18n"
)

// Hidden argv word the binary runs the classic CLI entry under, the python cli.py script of hermes
const CLIModuleEntry = "__icebath_cli__"

// Runs the classic CLI entry and exits with its status when argv carries its entry word
func DispatchCLIModuleEntry(ctx context.Context, exit func(int)) {
	if len(os.Args) < 2 || os.Args[1] != CLIModuleEntry {
		return
	}
	exit(cliModuleMain(ctx, os.Args[2:], os.Stderr))
}

// Runs Main with the keyword flags argv carries, exit status 2 for arguments it cannot consume
func cliModuleMain(ctx context.Context, argv []string, stderr io.Writer) int {
	o, code, done := parseCLIModuleArgs(argv, stderr)
	if done {
		return code
	}
	code, err := Main(ctx, o)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
	}
	return code
}

// Main keyword flags argv carries, positional words filling the parameters left unset in order
func parseCLIModuleArgs(argv []string, stderr io.Writer) (MainOptions, int, bool) {
	fs := pflag.NewFlagSet("icebath "+CLIModuleEntry, pflag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintf(stderr, "Usage: icebath %s [flags] [query]\n%s", CLIModuleEntry, fs.FlagUsages()) }
	fs.SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		return pflag.NormalizedName(strings.ReplaceAll(name, "_", "-"))
	})
	var o MainOptions
	maxTurns := 0
	fs.StringVar(&o.Query, "query", "", "Query to run, seeding an interactive session on a TTY, else answering and exiting")
	fs.StringVarP(&o.Q, "q", "q", "", "Shorthand for --query")
	fs.BoolVar(&o.Oneshot, "oneshot", false, "With -q, force the answer and exit single query mode even on a TTY")
	fs.StringVar(&o.Image, "image", "", "Optional local image path to attach to a single query")
	fs.StringVar(&o.Toolsets, "toolsets", "", "Comma-separated list of toolsets to enable (e.g., \"web,terminal\")")
	fs.StringArrayVar(&o.Skills, "skills", nil, "Comma-separated or repeated list of skills to preload for the session")
	fs.StringVar(&o.Model, "model", "", "Model to use")
	fs.StringVar(&o.Provider, "provider", "", "Inference provider")
	fs.StringVar(&o.Reasoning, "reasoning", "", "Reasoning effort for this run (none|minimal|low|medium|high|xhigh|max|ultra)")
	fs.StringVar(&o.APIKey, "api-key", "", "API key for authentication")
	fs.StringVar(&o.BaseURL, "base-url", "", "Base URL for the API")
	fs.IntVar(&maxTurns, "max-turns", 0, "Maximum tool-calling iterations")
	fs.Float64Var(&o.RunBudget, "run-budget", 0, "Spend budget for this run")
	fs.BoolVar(&o.Verbose, "verbose", false, "Enable verbose logging")
	fs.BoolVar(&o.Quiet, "quiet", false, "Suppress the banner and progress output")
	fs.BoolVar(&o.Compact, "compact", false, "Use compact display mode")
	fs.BoolVar(&o.ListTools, "list-tools", false, "List available tools and exit")
	fs.BoolVar(&o.ListToolsets, "list-toolsets", false, "List available toolsets and exit")
	fs.BoolVar(&o.Gateway, "gateway", false, "Run the messaging gateway")
	fs.StringVar(&o.Resume, "resume", "", "Resume a previous session by its ID")
	fs.BoolVar(&o.Worktree, "worktree", false, "Run in an isolated git worktree (for parallel agents)")
	fs.BoolVarP(&o.W, "w", "w", false, "Shorthand for --worktree")
	fs.BoolVar(&o.Checkpoints, "checkpoints", false, "Enable filesystem checkpoints")
	fs.BoolVar(&o.PassSessionID, "pass-session-id", false, "Pass the session ID to the agent")
	fs.StringVar(&o.OutputFormat, "output-format", "text", "Output format of a single query (text or stream-json)")
	fs.BoolVar(&o.IgnoreUserConfig, "ignore-user-config", false, "Run once with default settings")
	fs.BoolVar(&o.IgnoreRules, "ignore-rules", false, "Skip the project rules files")
	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return o, 0, true
		}
		fs.Usage()
		fmt.Fprintf(stderr, "ERROR: %v\n", err)
		return o, 2, true
	}
	order := []string{"query", "q", "oneshot", "image", "toolsets", "skills", "model", "provider", "reasoning", "api-key", "base-url",
		"max-turns", "run-budget", "verbose", "quiet", "compact", "list-tools", "list-toolsets", "gateway", "resume", "worktree", "w",
		"checkpoints", "pass-session-id", "output-format", "ignore-user-config", "ignore-rules"}
	positional := fs.Args()
	for _, name := range order {
		if len(positional) == 0 {
			break
		}
		if fs.Changed(name) {
			continue
		}
		if err := fs.Set(name, positional[0]); err != nil {
			fmt.Fprintf(stderr, "ERROR: %v\n", err)
			return o, 2, true
		}
		positional = positional[1:]
	}
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "ERROR: Could not consume arg: %s\n", positional[0])
		return o, 2, true
	}
	if fs.Changed("max-turns") {
		o.MaxTurns = &maxTurns
	}
	return o, 0, false
}

// Legacy cli.py --gateway entry arming the startup watchdog before the gateway runs, returning the exit status
func runLegacyGateway(ctx context.Context) int {
	gateway.ApplyMallocArenaMax(nil)
	func() {
		defer func() { _ = recover() }()
		gateway.ArmStartupWatchdog(ctx)
	}()
	fmt.Fprintln(console().Out, i18n.T(ctx, "cli.gateway.starting", "", nil))
	if _, err := gatewayrun.RunGateway(ctx, gatewayrun.StartGatewayOptions{Verbosity: new(int)}); err != nil {
		return gatewayrun.MainFailureCode(err, stderrWriter())
	}
	return 0
}

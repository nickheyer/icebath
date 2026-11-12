package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Runner argv when args invoke the legacy single query runner, by entry token or by the icebath-agent command name
func LegacyAgentArgv(args []string) ([]string, bool) {
	if len(args) > 1 && args[1] == pm.LegacyAgentEntry {
		return args[2:], true
	}
	if len(args) == 0 {
		return nil, false
	}
	if strings.TrimSuffix(filepath.Base(args[0]), ".exe") == commands.LegacyAgentProg {
		return args[1:], true
	}
	return nil, false
}

// Parses argv and runs one query through the direct runner, metadata flags and a bare invocation never reach it
func LegacyAgentMain(ctx context.Context, argv []string, stdout, stderr io.Writer, run func(context.Context, io.Writer, agent.MainOptions)) int {
	parser := commands.NewLegacyAgentParser(fmt.Sprintf("Icebath Agent v%s (%s)", pm.DunderVersion(), ReleaseDate), stdout)
	fail := func(err error) int {
		code, msg := commands.ExitCode(err)
		if msg != "" {
			fmt.Fprintln(stderr, msg)
		}
		return code
	}
	args, err := parser.Parse(argv)
	if err != nil {
		return fail(err)
	}
	positional := pystr.Strip(strings.Join(args.Prompt, " "))
	given := args.Query != nil && *args.Query != ""
	if given && positional != "" {
		return fail(parser.Error("pass the query either positionally or via --query, not both"))
	}
	var query *string
	switch {
	case given:
		query = args.Query
	case positional != "":
		query = &positional
	}
	if query == nil && !args.ListTools {
		parser.PrintHelp(stdout)
		fmt.Fprintln(stdout, "\nNo query given: pass one with --query (or run `icebath` for the interactive CLI).")
		return 0
	}
	if run == nil {
		run = runLegacyAgent
	}
	deref := func(v *string) string {
		if v == nil {
			return ""
		}
		return *v
	}
	run(ctx, stdout, agent.MainOptions{
		Query: query, Model: args.Model, APIKey: deref(args.APIKey), BaseURL: args.BaseURL, MaxTurns: args.MaxTurns,
		EnabledToolsets: deref(args.EnabledToolsets), DisabledToolsets: deref(args.DisabledToolsets), ListTools: args.ListTools,
		SaveTrajectories: args.SaveTrajectories, SaveSample: args.SaveSample, Verbose: args.Verbose, LogPrefixChars: args.LogPrefixChars,
	})
	return 0
}

// Loads the home and project dotenv the runner module loads on import, then runs the direct runner
func runLegacyAgent(ctx context.Context, w io.Writer, o agent.MainOptions) {
	log := slog.Default().With("logger", "run_agent")
	loaded, err := provider.LoadIcebathDotenv(ctx, provider.DotenvLoadOptions{ProjectEnv: filepath.Join(pm.RepoRoot(), ".env")})
	if err != nil {
		log.Debug(fmt.Sprintf("dotenv load failed: %v", err))
	}
	for _, path := range loaded {
		log.Info(fmt.Sprintf("Loaded environment variables from %s", path))
	}
	if len(loaded) == 0 {
		log.Info("No .env file found. Using system environment variables.")
	}
	cwd, _ := os.Getwd()
	o.Registry, o.Env = tools.Default, &tools.Env{Home: home.Dir(ctx), Cwd: cwd}
	agent.Main(ctx, w, o)
}

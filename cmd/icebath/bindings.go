package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/codeexec"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/lsp"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/spf13/cobra"
)

// Builds the cobra tree, binds every host handler and attaches plugin provided commands
func newCLI() *commands.CLI {
	cli := commands.NewCLI()
	bindChat(cli)
	bindCron(cli)
	bindLSP(cli)
	bindComputerUse(cli)
	bindLogout(cli)
	bindHostCommands(cli)
	registerPluginCLICommands(cli, os.Args[1:])
	return cli
}

// Context of a cobra invocation, the background when cobra carried none
func cmdCtx(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// Exit status of a handler as the error cobra propagates, nil for zero
func status(code int, err error) error {
	if err != nil {
		return err
	}
	if code != 0 {
		return &commands.ExitError{Code: code}
	}
	return nil
}

func bindChat(cli *commands.CLI) {
	cli.Bind("chat", func(cmd *cobra.Command, args []string) error {
		a := parsedArgs(cli, cmd, args)
		a.Command = "chat"
		return status(cmdChat(a), nil)
	})
	cli.Bind("resume", func(cmd *cobra.Command, args []string) error {
		return status(cmdResume(cmdCtx(cmd), cmd, args))
	})
}

// Binds icebath cron and every subcommand onto cron.CronCommand, the handler's int the exit code
func bindCron(cli *commands.CLI) {
	for _, path := range []string{"cron", "cron list", "cron create", "cron edit", "cron pause", "cron resume", "cron run", "cron cancel",
		"cron remove", "cron status", "cron runs", "cron incidents", "cron notepad", "cron doctor", "cron tick"} {
		sub := strings.TrimPrefix(strings.TrimPrefix(path, "cron"), " ")
		cli.Bind(path, func(cmd *cobra.Command, args []string) error {
			ctx := cmdCtx(cmd)
			a := cronArgs(cli, cmd, sub, args)
			return status(cron.CronCommand(ctx, home.Dir(ctx), a, cmd.OutOrStdout()))
		})
	}
}

// Reads one cron command line into CronArgs the way argparse fills its namespace
func cronArgs(cli *commands.CLI, cmd *cobra.Command, sub string, args []string) *cron.CronArgs {
	f := cmd.Flags()
	a := &cron.CronArgs{Command: cmd.CalledAs()}
	if sub == "" {
		a.Command = ""
	}
	str := func(name string) *string {
		if f.Lookup(name) == nil || !f.Changed(name) {
			return nil
		}
		v, _ := f.GetString(name)
		return &v
	}
	anyStr := func(name string) any {
		if p := str(name); p != nil {
			return *p
		}
		return nil
	}
	boolean := func(name string) bool {
		if f.Lookup(name) == nil {
			return false
		}
		v, _ := f.GetBool(name)
		return v
	}
	list := func(name string) []string {
		if f.Lookup(name) == nil || !f.Changed(name) {
			return nil
		}
		v, _ := f.GetStringArray(name)
		return v
	}
	a.JobID = cli.Positional(cmd, args, "job_id")
	switch sub {
	case "list":
		a.All = boolean("all")
	case "create", "edit":
		if sub == "create" {
			if s := cli.Positional(cmd, args, "schedule"); s != "" {
				a.Schedule = &s
			}
			if p := cli.Positional(cmd, args, "prompt"); p != "" {
				a.Prompt = &p
			}
			if boolean("no-agent") {
				t := true
				a.NoAgent = &t
			}
			a.Paused = boolean("paused")
			a.PausedReason = str("paused-reason")
			if boolean("pin") {
				a.Pinned = true
			}
			if boolean("continuity") {
				a.Continuity = true
			}
		} else {
			a.Schedule, a.Prompt = str("schedule"), str("prompt")
			a.AddSkills, a.RemoveSkills = list("add-skill"), list("remove-skill")
			a.ClearSkills = boolean("clear-skills")
			switch {
			case boolean("no-agent"):
				t := true
				a.NoAgent = &t
			case boolean("agent"):
				fl := false
				a.NoAgent = &fl
			}
			switch {
			case boolean("pin"):
				a.Pinned = true
			case boolean("unpin"):
				a.Pinned = false
			}
			switch {
			case boolean("continuity"):
				a.Continuity = true
			case boolean("no-continuity"):
				a.Continuity = false
			}
		}
		a.Skills = list("skill")
		a.Name, a.Deliver, a.FailureDeliver = anyStr("name"), anyStr("deliver"), anyStr("failure-deliver")
		if f.Changed("repeat") {
			n, _ := f.GetInt("repeat")
			a.Repeat = n
		}
		a.Script, a.Workdir, a.Model, a.ModelProvider = anyStr("script"), anyStr("workdir"), anyStr("model"), anyStr("provider")
		a.MonitorScript, a.MonitorURL = anyStr("monitor-script"), anyStr("monitor-url")
		a.ReasoningEffort, a.Interpreter, a.Timezone, a.GraceMinutes = anyStr("reasoning-effort"), anyStr("interpreter"), anyStr("timezone"), anyStr("grace-minutes")
		if c := str("catch-up"); c != nil {
			a.CatchUp = *c == "on"
		}
	case "resume":
		a.RunAt = str("at")
		a.RunNow = boolean("run-now")
	case "cancel":
		a.Reason = str("reason")
	case "runs":
		if f.Changed("limit") {
			n, _ := f.GetInt("limit")
			a.Limit = &n
		} else {
			n := 20
			a.Limit = &n
		}
	case "incidents":
		a.State = str("state")
		a.IncidentAction = cli.Positional(cmd, args, "incident_action")
		a.IncidentID = cli.Positional(cmd, args, "incident_id")
	case "notepad":
		a.NotepadAction = cli.Positional(cmd, args, "notepad_action")
		if k := cli.Positional(cmd, args, "key"); k != "" {
			a.Key = &k
		}
		if v := cli.Positional(cmd, args, "value"); v != "" {
			a.Value = &v
		}
	}
	return a
}

// Binds icebath lsp onto lsp.RunCommand with the process home's command context
func bindLSP(cli *commands.CLI) {
	for _, sub := range []string{"status", "list", "install", "install-all", "restart", "which"} {
		cli.Bind("lsp "+sub, func(cmd *cobra.Command, args []string) error {
			ctx := cmdCtx(cmd)
			f := cmd.Flags()
			a := lsp.CommandArgs{Subcommand: sub, Server: cli.Positional(cmd, args, "server")}
			if f.Lookup("json") != nil {
				a.JSON, _ = f.GetBool("json")
			}
			if f.Lookup("installed-only") != nil {
				a.InstalledOnly, _ = f.GetBool("installed-only")
			}
			if f.Lookup("include-manual") != nil {
				a.IncludeManual, _ = f.GetBool("include-manual")
			}
			return status(lsp.RunCommand(ctx, tools.LSPCommandContext(processToolsEnv(ctx)), a, cmd.OutOrStdout(), cmd.ErrOrStderr()), nil)
		})
	}
	cli.Bind("lsp", func(cmd *cobra.Command, args []string) error { return cmd.Help() })
}

// Binds the computer-use tree onto the codeexec dispatcher
func bindComputerUse(cli *commands.CLI) {
	run := codeexec.BuildComputerUseParser()
	for _, path := range codeexec.ComputerUseCommandPaths {
		cli.Bind(path, func(cmd *cobra.Command, args []string) error {
			ctx := cmdCtx(cmd)
			ns, err := codeexec.ParseComputerUseArgs(path, cmd.Flags())
			if err != nil {
				return err
			}
			rc, err := run(ctx, &codeexec.ComputerUseCLI{Env: processToolsEnv(ctx), Console: cliui.Default(), Stderr: os.Stderr,
				PrintHelp: func(p string) error {
					target, _, findErr := cli.Root.Find(strings.Fields(p))
					if findErr != nil {
						return findErr
					}
					return target.Help()
				}}, ns)
			return status(rc, err)
		})
	}
	for _, parent := range []string{"computer-use", "computer-use permissions", "computer-use screen"} {
		cli.Bind(parent, func(cmd *cobra.Command, args []string) error { return cmd.Help() })
	}
}

// Binds login and logout onto the provider auth flows
func bindLogout(cli *commands.CLI) {
	cli.Bind("login", func(cmd *cobra.Command, args []string) error {
		provider.LoginCommand(cmd.OutOrStdout())
		return nil
	})
	cli.Bind("logout", func(cmd *cobra.Command, args []string) error {
		ctx := cmdCtx(cmd)
		providerID, _ := cmd.Flags().GetString("provider")
		err := provider.LogoutCommand(provider.HomePoolEnv(home.Dir(ctx)), providerID, cmd.OutOrStdout())
		var unknown *provider.UnknownAuthProviderError
		if errorsAs(err, &unknown) {
			fmt.Fprintln(cmd.OutOrStdout(), err.Error())
			return &commands.ExitError{Code: 1}
		}
		return err
	})
}

// Attaches plugin provided top level commands when argv may name one
func registerPluginCLICommands(cli *commands.CLI, argv []string) {
	if !cli.PluginCLIDiscoveryNeeded(argv) {
		return
	}
	ctx := context.Background()
	provider.RecoverLeftCoreForCommand(ctx, cli.FirstPositional(argv))
	seen := map[string]bool{}
	for _, info := range memory.DiscoverPluginCLICommands(ctx, processToolsEnv(ctx)) {
		attachPluginCLICommand(cli, info.Name, info.Help, info.Description, info.Setup, info.Handler)
		seen[info.Name] = true
	}
	plugins.DiscoverPlugins(ctx, false)
	resolveDeferredPlatformCLICommand(ctx, cli.FirstPositional(argv))
	for _, info := range plugins.GetPluginManager(ctx).CLICommands() {
		if seen[info.Name] {
			continue
		}
		setup := func(cmd *cobra.Command) {
			if info.Setup != nil {
				info.Setup(cmd)
			}
		}
		var handler func(cmd *cobra.Command, args []string) error
		if info.Handler != nil {
			handler = func(cmd *cobra.Command, args []string) error { return info.Handler(cmd) }
		}
		attachPluginCLICommand(cli, info.Name, info.Help, info.Description, setup, handler)
	}
}

// Registers one plugin provided top level command from its descriptor
func attachPluginCLICommand(cli *commands.CLI, name, help, description string, setup func(*cobra.Command), handler func(*cobra.Command, []string) error) {
	cmd := &cobra.Command{Use: name, Short: help, Long: description, SilenceUsage: true, SilenceErrors: true}
	if setup != nil {
		setup(cmd)
	}
	if handler != nil {
		cmd.RunE = handler
	}
	cli.AddPluginCommand(cmd)
}

// Materializes the deferred platform whose top level CLI command matches so its registration runs
func resolveDeferredPlatformCLICommand(ctx context.Context, commandName string) {
	if commandName == "" {
		return
	}
	gateway.Registry.Get(ctx, commandName)
}

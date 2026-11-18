package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/spf13/cobra"
)

// Builds the cobra tree, binds every host handler and attaches plugin provided commands
func newCLI() *commands.CLI {
	cli := commands.NewCLI()
	bindChat(cli)
	bindLogout(cli)
	for _, bind := range tierCLI {
		bind(cli)
	}
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
	first, _ := cli.FirstPositional(argv)
	provider.RecoverLeftCoreForCommand(ctx, first)
	seen := map[string]bool{}
	for _, info := range memory.DiscoverPluginCLICommands(ctx, processToolsEnv(ctx)) {
		attachPluginCLICommand(cli, info.Name, info.Help, info.Description, info.Setup, info.Handler)
		seen[info.Name] = true
	}
	plugins.DiscoverPlugins(ctx, false)
	resolveDeferredPlatformCLICommand(ctx, first)
	for _, info := range plugins.GetPluginManager(ctx).CLICommands() {
		if seen[info.Name] {
			continue
		}
		if failure := attachManagerCLICommand(cli, info); failure != nil {
			pkgLog().Debug(fmt.Sprintf("Plugin CLI discovery failed: %v", failure))
			return
		}
	}
}

// Attaches one plugin manager command, returning what its setup raised so discovery stops there
func attachManagerCLICommand(cli *commands.CLI, info *plugins.CLICommand) (failure any) {
	setup := func(cmd *cobra.Command) {
		if info.Parse != nil {
			cmd.DisableFlagParsing, cmd.Args = true, cobra.ArbitraryArgs
		}
		if info.Setup != nil {
			defer func() { failure = recover() }()
			info.Setup(cmd)
		}
	}
	var handler func(cmd *cobra.Command, args []string) error
	switch {
	case info.Parse != nil:
		handler = func(cmd *cobra.Command, args []string) error { return runHostedPluginCommand(cli, cmd, args, info) }
	case info.Handler != nil:
		handler = func(cmd *cobra.Command, args []string) error {
			if failure != nil {
				return cmd.Root().Help()
			}
			return info.Handler(cli.Namespace(cmd, args))
		}
	}
	attachPluginCLICommand(cli, info.Name, info.Help, info.Description, setup, handler)
	return failure
}

// Runs a hosted plugin command whose own parser parses argv, printing top level help when it selects no func
func runHostedPluginCommand(cli *commands.CLI, cmd *cobra.Command, args []string, info *plugins.CLICommand) error {
	outcome, err := info.Parse(context.Background(), cli.Namespace(cmd, nil), args)
	if err != nil {
		return err
	}
	switch {
	case outcome.Exited && outcome.Code == 0:
		return nil
	case len(outcome.Extras) > 0:
		return &commands.ArgparseError{Cmd: cmd.Root(), Msg: "unrecognized arguments: " + strings.Join(outcome.Extras, " ")}
	case outcome.Code != 0:
		return &commands.ExitError{Code: outcome.Code}
	case !outcome.Dispatched:
		return cmd.Root().Help()
	}
	return nil
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

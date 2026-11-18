package main

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/app"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/monitoring"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/spf13/cobra"
)

// A host command group handler reading the parsed cobra command, the handler of that group
type groupHandler func(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error)

// Binds every host owned command path to the handler of its top level group
func bindHostCommands(cli *commands.CLI) {
	groups := map[string]groupHandler{
		"approvals":               approvalsGroup,
		"auth":                    authGroup,
		"checkpoints":             checkpointsGroup,
		"codex-runtime":           codexRuntimeGroup,
		"completion":              completionGroup,
		"console":                 consoleGroup,
		"curator":                 curatorGroup,
		"debug":                   debugGroup,
		"doctor":                  doctorGroup,
		"dump":                    dumpGroup,
		"egress":                  egressGroup,
		"fallback":                fallbackGroup,
		"gateway":                 gatewayGroup,
		"install-self":            installSelfGroup,
		"journey":                 journeyGroup,
		"logs":                    logsGroup,
		"migrate":                 migrateGroup,
		"model":                   modelGroup,
		"monitoring":              monitoringGroup,
		"pairing":                 pairingGroup,
		"pause":                   pauseGroup,
		"peer":                    peerGroup,
		"pets":                    petsGroup,
		"project":                 projectGroup,
		"prompt-size":             promptSizeGroup,
		"proxy":                   proxyGroup,
		"secrets":                 secretsGroup,
		"security":                securityGroup,
		"sessions":                sessionsGroup,
		"status":                  statusGroup,
		"tools":                   toolsGroup,
		"uninstall":               uninstallGroup,
		"update":                  updateGroup,
		"update-takeover":         updateTakeoverGroup,
		"update-finish":           updateFinishGroup,
		"update-restart-recovery": updateRestartRecoveryGroup,
		"update-completion":       updateCompletionGroup,
		"venv-sync":               venvSyncGroup,
		"source-completion":       sourceCompletionGroup,
		"import-health":           importHealthGroup,
		"usage":                   usageGroup,
		"verify":                  verifyGroup,
		"worktree":                worktreeGroup,
	}
	bindGroups(cli, groups)
}

// Binds every unbound command path under a group name to the handler of that group
func bindGroups(cli *commands.CLI, groups map[string]groupHandler) {
	for _, path := range cli.Unbound() {
		group := firstWord(path)
		handler, ok := groups[group]
		if !ok {
			continue
		}
		cli.Bind(path, func(cmd *cobra.Command, args []string) error {
			return status(handler(cmdCtx(cmd), cli, cmd, path, args))
		})
	}
}

// Argparse dest value of a subcommand word, None when the group ran bare
func nilWhenEmpty(word string) any {
	if word == "" {
		return nil
	}
	return word
}

// Flag values of a cobra command as the argparse namespace the group handlers read
func namespace(cli *commands.CLI, cmd *cobra.Command, path string, args []string) map[string]any {
	return cli.Namespace(cmd, args)
}

func approvalsGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	ns["approvals_command"] = nilWhenEmpty(secondWord(path))
	return tools.ApprovalsCommand(ctx, processToolsEnv(ctx), ns, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

// Runs icebath auth with the subcommand under its argparse dest and --type under auth_type
func authGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	ns["auth_action"] = nil
	if words := strings.Fields(path); len(words) > 1 {
		ns["auth_action"] = words[1]
	}
	if v, ok := ns["type"]; ok {
		ns["auth_type"] = v
		delete(ns, "type")
	}
	return provider.AuthCommand(ctx, provider.HomePoolEnv(home.Dir(ctx)), ns, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

func checkpointsGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	ns["checkpoints_command"] = nil
	if words := strings.Fields(path); len(words) > 1 {
		ns["checkpoints_command"] = words[1]
	}
	return tools.CheckpointsCommand(ctx, processToolsEnv(ctx), ns, cmd.OutOrStdout())
}

func codexRuntimeGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return provider.CodexRuntimeCommand(ctx, home.Dir(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout())
}

func completionGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	shell := cli.Positional(cmd, args, "shell")
	if shell == "" {
		shell = "bash"
	}
	return 0, cli.Completion(shell, cmd.OutOrStdout())
}

func consoleGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return app.RunConsoleREPL(ctx, app.ConsoleREPLOptions{})
}

func curatorGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	ns["curator_command"] = nilWhenEmpty(secondWord(path))
	return memory.CuratorCommand(ctx, processToolsEnv(ctx), ns, cmd.OutOrStdout())
}

func debugGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return selfupdate.RunDebug(ctx, home.Dir(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout())
}

func doctorGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return selfupdate.RunDoctor(ctx, home.Dir(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout())
}

func dumpGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return selfupdate.RunDump(ctx, home.Dir(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout())
}

// Runs icebath egress with the subcommand recorded under the argparse dest key
func egressGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	ns["egress_command"] = nil
	if words := strings.Fields(path); len(words) > 1 {
		ns["egress_command"] = words[1]
	}
	return provider.EgressCommand(ctx, home.Dir(ctx), ns, cmd.OutOrStdout())
}

// Runs icebath fallback with the subcommand name or alias typed under its argparse dest
func fallbackGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	ns["fallback_command"] = nil
	if words := strings.Fields(path); len(words) > 1 {
		ns["fallback_command"] = words[1]
		if called := cmd.CalledAs(); called != "" {
			ns["fallback_command"] = called
		}
	}
	return provider.FallbackCommand(ctx, home.Dir(ctx), ns, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

func gatewayGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	switch path {
	case "gateway enroll":
		return gatewaycli.CmdGatewayEnroll(ctx, ns, cmd.OutOrStdout())
	case "gateway ensure":
		return gatewaycli.CmdGatewayEnsure(ctx, ns, cmd.OutOrStdout())
	}
	return gatewaycli.GatewayCommand(ctx, ns, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

func journeyGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	ns["journey_action"] = nilWhenEmpty(secondWord(path))
	return memory.JourneyCommand(ctx, processToolsEnv(ctx), ns, cmd.OutOrStdout())
}

func logsGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	name := cli.Positional(cmd, args, "log_name")
	if name == "" {
		name = "agent"
	}
	if name == "list" {
		return 0, app.ListLogs(ctx, cmd.OutOrStdout())
	}
	return app.TailLog(ctx, cmd.OutOrStdout(), app.TailLogOptions{Name: name, Lines: nsInt(ns, "lines"), Follow: nsBool(ns, "follow"),
		Level: nsString(ns, "level"), Session: nsString(ns, "session"), Since: nsString(ns, "since"), Component: nsString(ns, "component")})
}

// Runs icebath migrate with the subtype under migrate_type and an importer name also under migrate_agent
func migrateGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	ns["migrate_type"] = nilWhenEmpty(secondWord(path))
	if agent, ok := ns["migrate_type"].(string); ok && provider.IsImportSourceName(agent) {
		ns["migrate_agent"] = agent
	}
	return provider.MigrateCommand(ctx, home.Dir(ctx), ns, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

func modelGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	requireTTY("model")
	return provider.CmdModel(ctx, namespace(cli, cmd, path, args), cmd.OutOrStdout())
}

func monitoringGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return monitoring.CmdMonitoring(ctx, home.Dir(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout(), cmd.ErrOrStderr())
}

func pairingGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	ns["pairing_action"] = nilWhenEmpty(secondWord(path))
	return gateway.PairingCommand(ctx, ns, cmd.OutOrStdout())
}

func pauseGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return tools.PauseCommand(ctx, namespace(cli, cmd, path, args), cmd.OutOrStdout())
}

// Runs icebath peer with the subcommand name or alias typed under its argparse dest
func peerGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	ns["peer_action"] = nil
	if words := strings.Fields(path); len(words) > 1 {
		ns["peer_action"] = words[1]
		if called := cmd.CalledAs(); called != "" {
			ns["peer_action"] = called
		}
	}
	return gateway.PeerCommand(ctx, home.Dir(ctx), ns, cmd.OutOrStdout())
}

func petsGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	ns["pets_command"] = nilWhenEmpty(secondWord(path))
	return app.PetsCommand(ctx, home.Dir(ctx), ns, cmd.OutOrStdout())
}

func projectGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	ns["project_action"] = nilWhenEmpty(secondWord(path))
	return tools.ProjectsCommand(ctx, processToolsEnv(ctx), ns, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

func promptSizeGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return agent.CmdPromptSize(ctx, processToolsEnv(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout())
}

// Runs icebath proxy with the subcommand recorded under the argparse dest key
func proxyGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	ns["proxy_command"] = nil
	if words := strings.Fields(path); len(words) > 1 {
		ns["proxy_command"] = words[1]
	}
	return provider.CmdProxy(ctx, home.Dir(ctx), ns, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

// Runs icebath secrets with the integration and its subcommand recorded under the argparse dest keys
func secretsGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	words := strings.Fields(path)
	ns["secrets_command"], ns["secrets_bw_command"], ns["secrets_op_command"] = nil, nil, nil
	if len(words) > 1 {
		ns["secrets_command"] = words[1]
	}
	if len(words) > 2 {
		dest := "secrets_bw_command"
		if words[1] == "onepassword" {
			dest = "secrets_op_command"
		}
		ns[dest] = words[2]
	}
	return provider.SecretsCommand(ctx, home.Dir(ctx), ns, cmd.OutOrStdout())
}

func securityGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	sub := secondWord(path)
	if sub == "" || sub == "audit" {
		return selfupdate.CmdSecurityAudit(ctx, home.Dir(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout())
	}
	cmd.PrintErrf("unknown security subcommand: %s\n", sub)
	return 2, nil
}

func sessionsGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	ns["sessions_action"] = nilWhenEmpty(secondWord(path))
	parser := cmd
	for parser.Parent() != nil && parser.Parent() != cli.Root {
		parser = parser.Parent()
	}
	return app.CmdSessions(ctx, home.Dir(ctx), ns, cmd.OutOrStdout(), parser.Help)
}

// Lifts the emergency stop
func cmdResume(ctx context.Context, cmd *cobra.Command, args []string) (int, error) {
	return tools.ResumeCommand(ctx, home.Dir(ctx), args, cmd.OutOrStdout())
}

func statusGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return selfupdate.ShowStatus(ctx, home.Dir(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout())
}

func toolsGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	ns["tools_action"] = nilWhenEmpty(secondWord(path))
	return tools.CmdTools(ctx, processToolsEnv(ctx), ns, cmd.OutOrStdout())
}

func uninstallGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return app.CmdUninstall(ctx, namespace(cli, cmd, path, args), cmd.OutOrStdout(), requireTTY)
}

func updateGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return selfupdate.CmdUpdate(ctx, selfupdate.UpdateInvocation{Namespace: namespace(cli, cmd, path, args), ProjectRoot: pm.RepoRoot(),
		Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()})
}

func updateTakeoverGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return selfupdate.CmdUpdateTakeover(ctx, namespace(cli, cmd, path, args), cmd.OutOrStdout())
}

// Finishes an update in the selected checkout from a request file
func updateFinishGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	return selfupdate.UpdateFinishMain(ctx, nsString(ns, "context"), nsString(ns, "result"), cmd.OutOrStdout(), cmd.ErrOrStderr()), nil
}

// Restarts supervised gateways and serve units from the payload on stdin over the entry's raw arguments
func updateRestartRecoveryGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return selfupdate.UpdateRestartRecoveryMain(ctx, args, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()), nil
}

// Runs the post commit completion of a source update over the entry's raw arguments
func updateCompletionGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return selfupdate.UpdateCompletionMain(ctx, args, cmd.OutOrStdout(), cmd.ErrOrStderr()), nil
}

// Reports or syncs a checkout's dependencies over the entry's raw arguments
func venvSyncGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return selfupdate.VenvSyncMain(ctx, args, cmd.OutOrStdout(), cmd.ErrOrStderr()), nil
}

// Runs the completion tail a source update launch starts in its custody
func sourceCompletionGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return selfupdate.SourceCompletionMain(ctx, args, cmd.OutOrStdout(), cmd.ErrOrStderr()), nil
}

// Answers the import health probe an update runs against this install's binary
func importHealthGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return selfupdate.ImportHealthMain(args, cmd.OutOrStdout()), nil
}

func usageGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return provider.UsageCommand(ctx, provider.HomePoolEnv(home.Dir(ctx)), namespace(cli, cmd, path, args), cmd.OutOrStdout())
}

func verifyGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return plugins.RunVerifyCommand(ctx, namespace(cli, cmd, path, args), cmd.OutOrStdout(), cmd.ErrOrStderr()), nil
}

func worktreeGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	ns["worktree_action"] = nil
	if words := strings.Fields(path); len(words) > 1 {
		ns["worktree_action"] = words[1]
	}
	return tools.WorktreeCommand(ctx, processToolsEnv(ctx), ns, cmd.OutOrStdout())
}

// Exits 1 when stdin is not a terminal, curses and input prompts spin on a pipe
func requireTTY(commandName string) {
	if !app.IsInteractiveStdin() {
		printErr("Error: 'icebath " + commandName + "' requires an interactive terminal.\n" +
			"It cannot be run through a pipe or non-interactive subprocess.\n" +
			"Run it directly in your terminal instead.")
		exit(1)
	}
}

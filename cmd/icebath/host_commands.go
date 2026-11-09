package main

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/app"
	"github.com/nickheyer/icebath/pkg/backup"
	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/monitoring"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/vault"
	"github.com/spf13/cobra"
)

// A host command group handler reading the parsed cobra command, the hermes_cli handler of that group
type groupHandler func(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error)

// Binds every host owned command path to the handler of its top level group
func bindHostCommands(cli *commands.CLI) {
	groups := map[string]groupHandler{
		"acp":             acpGroup,
		"approvals":       approvalsGroup,
		"auth":            authGroup,
		"backup":          backupGroup,
		"browser":         browserGroup,
		"checkpoints":     checkpointsGroup,
		"codex-runtime":   codexRuntimeGroup,
		"completion":      completionGroup,
		"connectors":      connectorsGroup,
		"console":         consoleGroup,
		"curator":         curatorGroup,
		"dashboard":       dashboardGroup,
		"debug":           debugGroup,
		"desktop":         desktopGroup,
		"doctor":          doctorGroup,
		"dump":            dumpGroup,
		"egress":          egressGroup,
		"fallback":        fallbackGroup,
		"gateway":         gatewayGroup,
		"import":          importGroup,
		"journey":         journeyGroup,
		"kanban":          kanbanGroup,
		"logs":            logsGroup,
		"migrate":         migrateGroup,
		"model":           modelGroup,
		"monitoring":      monitoringGroup,
		"pairing":         pairingGroup,
		"pause":           pauseGroup,
		"peer":            peerGroup,
		"pets":            petsGroup,
		"portal":          portalGroup,
		"project":         projectGroup,
		"prompt-size":     promptSizeGroup,
		"proxy":           proxyGroup,
		"secrets":         secretsGroup,
		"security":        securityGroup,
		"send":            sendGroup,
		"serve":           dashboardGroup,
		"sessions":        sessionsGroup,
		"setup":           setupGroup,
		"slack":           slackGroup,
		"status":          statusGroup,
		"tools":           toolsGroup,
		"uninstall":       uninstallGroup,
		"update":          updateGroup,
		"update-takeover": updateTakeoverGroup,
		"usage":           usageGroup,
		"vault":           vaultGroup,
		"verify":          verifyGroup,
		"webapp":          webappGroup,
		"whatsapp":        whatsappGroup,
		"whatsapp-cloud":  whatsappCloudGroup,
		"worktree":        worktreeGroup,
	}
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

func acpGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return gateway.CmdACP(ctx, namespace(cli, cmd, path, args), cmd.OutOrStdout())
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

func backupGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	if nsBool(ns, "quick") {
		return backup.RunQuickBackup(ctx, home.Dir(ctx), ns, cmd.OutOrStdout())
	}
	ok, err := backup.RunBackup(ctx, home.Dir(ctx), ns, cmd.OutOrStdout())
	if err != nil {
		return 1, err
	}
	if !ok {
		return 1, nil
	}
	return 0, nil
}

func importGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return backup.RunImport(ctx, home.Dir(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout())
}

func browserGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return browser.BrowserCommand(ctx, processToolsEnv(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout())
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

func connectorsGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	ns["connectors_action"] = nilWhenEmpty(secondWord(path))
	return mcp.ConnectorsCommand(ctx, processToolsEnv(ctx), ns, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

func consoleGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return app.RunConsoleREPL(ctx), nil
}

func curatorGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	ns["curator_command"] = nilWhenEmpty(secondWord(path))
	return memory.CuratorCommand(ctx, processToolsEnv(ctx), ns, cmd.OutOrStdout())
}

func dashboardGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return cmdDashboard(cli, cmd, args)
}

// Starts the web UI server or manages running ones, hermes_cli/main.py cmd_dashboard
func cmdDashboard(cli *commands.CLI, cmd *cobra.Command, args []string) (int, error) {
	ctx := cmdCtx(cmd)
	ns := namespace(cli, cmd, cmd.CommandPath(), args)
	if strings.HasSuffix(cmd.CommandPath(), " dashboard register") {
		return gateway.CmdDashboardRegister(ctx, ns, cmd.OutOrStdout())
	}
	return gateway.CmdDashboard(ctx, gateway.DashboardLaunch{Namespace: ns, HeadlessBackend: cmd.Name() == "serve", Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr(),
		ExplicitProfile: explicitCLIProfile, PrepareRuntime: dashboardPrepareRuntime})
}

// Deps check, skills seed, terminal env bridge, plugins and MCP discovery before the server binds
func dashboardPrepareRuntime(ctx context.Context, headlessBackend bool) bool {
	setupLoggingMode("gui")
	syncBundledSkillsQuietly()
	if err := config.ApplyTerminalConfigToEnv(ctx, home.Dir(ctx), config.TerminalEnvOptions{}); err != nil {
		slogDebug("terminal config → env bridge failed for dashboard/serve: " + err.Error())
	}
	plugins.DiscoverPlugins(ctx, false)
	desktop := tools.IsDesktopOwnedBackend(nil)
	if headlessBackend && desktop {
		return true
	}
	env := processToolsEnv(ctx)
	if desktop {
		mcp.StartBackgroundDiscovery(ctx, env, nil, "dashboard-mcp-discovery")
	} else {
		mcp.DeferBackgroundDiscovery(ctx, env, nil, "dashboard-mcp-discovery", nil)
	}
	return false
}

func debugGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return selfupdate.RunDebug(ctx, home.Dir(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout())
}

func desktopGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return app.CmdGUI(ctx, namespace(cli, cmd, path, args), explicitCLIProfile, cmd.OutOrStdout())
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

func fallbackGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return provider.FallbackCommand(ctx, home.Dir(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout())
}

func gatewayGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	if path == "gateway enroll" {
		return gatewaycli.CmdGatewayEnroll(ctx, ns, cmd.OutOrStdout())
	}
	return gatewaycli.GatewayCommand(ctx, ns, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

func journeyGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	ns["journey_action"] = nilWhenEmpty(secondWord(path))
	return memory.JourneyCommand(ctx, processToolsEnv(ctx), ns, cmd.OutOrStdout())
}

func kanbanGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return kanban.KanbanCommand(ctx, namespace(cli, cmd, path, args), cmd.OutOrStdout(), cmd.ErrOrStderr())
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
	return 0, app.TailLog(ctx, cmd.OutOrStdout(), app.TailLogOptions{Name: name, Lines: nsInt(ns, "lines"), Follow: nsBool(ns, "follow"),
		Level: nsString(ns, "level"), Session: nsString(ns, "session"), Since: nsString(ns, "since"), Component: nsString(ns, "component")})
}

// Runs icebath migrate with the subtype under migrate_type and an importer name also under migrate_agent
func migrateGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	ns["migrate_type"] = nilWhenEmpty(secondWord(path))
	if agent, ok := ns["migrate_type"].(string); ok && plugins.IsImportSourceName(agent) {
		ns["migrate_agent"] = agent
	}
	return plugins.MigrateCommand(ctx, home.Dir(ctx), ns, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

func modelGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	requireTTY("model")
	ns := namespace(cli, cmd, path, args)
	if nsBool(ns, "refresh") {
		provider.ClearAllProviderModelsCache(ctx)
		cmd.Println("  Cleared model picker cache.")
	}
	err := observability.ProviderSetupSurface(ctx, "cli_model", func(ctx context.Context) error {
		return provider.RunSetupActionWithNavigation(ctx, "Model & Provider", func(ctx context.Context) error {
			return provider.SelectProviderAndModel(ctx, ns)
		}, "No change.")
	})
	return 0, err
}

func monitoringGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return monitoring.CmdMonitoring(ctx, home.Dir(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout())
}

func pairingGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return gateway.PairingCommand(ctx, home.Dir(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout())
}

func pauseGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return gateway.EstopCommand(ctx, home.Dir(ctx), path == "pause", namespace(cli, cmd, path, args), cmd.OutOrStdout())
}

func peerGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return gateway.PeerCommand(ctx, home.Dir(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout())
}

func petsGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return app.PetsCommand(ctx, home.Dir(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout())
}

func portalGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return gateway.PortalCommand(ctx, home.Dir(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout())
}

func projectGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	ns["project_action"] = nilWhenEmpty(secondWord(path))
	return tools.ProjectsCommand(ctx, processToolsEnv(ctx), ns, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

func promptSizeGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return provider.CmdPromptSize(ctx, processToolsEnv(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout())
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

func sendGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return gatewaycli.SendCommand(ctx, home.Dir(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout(), cmd.ErrOrStderr())
}

func sessionsGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return state.SessionsCommand(ctx, home.Dir(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout())
}

func setupGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return cmdSetupNamespace(ctx, namespace(cli, cmd, path, args)), nil
}

// Runs the interactive setup wizard, hermes_cli/setup.py run_setup_wizard
func cmdSetupNamespace(ctx context.Context, ns map[string]any) int {
	return provider.RunSetupWizard(ctx, ns)
}

// Setup entered from the chat first run guard with the chat namespace
func cmdSetup(a *chatArgs) int {
	return provider.RunSetupWizard(context.Background(), map[string]any{})
}

// Lifts the emergency stop, hermes_cli/subcommands/pause.py cmd_resume
func cmdResume(ctx context.Context, cmd *cobra.Command, args []string) (int, error) {
	return tools.ResumeCommand(ctx, home.Dir(ctx), args, cmd.OutOrStdout())
}

func slackGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return gateway.CmdSlack(ctx, home.Dir(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout())
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
	ns := namespace(cli, cmd, path, args)
	if nsBool(ns, "gui_summary") {
		return app.PrintGUIInstallSummary(ctx, cmd.OutOrStdout())
	}
	prompting := !nsBool(ns, "yes") && !nsBool(ns, "dry_run")
	switch {
	case nsBool(ns, "data"):
		if prompting {
			requireTTY("uninstall --data")
		}
		return selfupdate.RunDataUninstall(ctx, home.Dir(ctx), ns, cmd.OutOrStdout())
	case nsBool(ns, "gui"):
		if prompting {
			requireTTY("uninstall --gui")
		}
		return app.RunGUIUninstall(ctx, ns, cmd.OutOrStdout())
	}
	if prompting {
		requireTTY("uninstall")
	}
	return selfupdate.RunUninstall(ctx, home.Dir(ctx), ns, cmd.OutOrStdout())
}

func updateGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return selfupdate.CmdUpdate(ctx, selfupdate.UpdateInvocation{Namespace: namespace(cli, cmd, path, args), ProjectRoot: pm.RepoRoot(),
		Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()})
}

func updateTakeoverGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return selfupdate.CmdUpdateTakeover(ctx, namespace(cli, cmd, path, args), cmd.OutOrStdout())
}

func usageGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return provider.UsageCommand(ctx, provider.HomePoolEnv(home.Dir(ctx)), namespace(cli, cmd, path, args), cmd.OutOrStdout())
}

func vaultGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return vault.VaultCommand(ctx, home.Dir(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout())
}

func verifyGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return plugins.RunVerifyCommand(ctx, namespace(cli, cmd, path, args), cmd.OutOrStdout(), cmd.ErrOrStderr()), nil
}

func webappGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return gateway.CmdWebapp(ctx, home.Dir(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout())
}

func whatsappGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return gateway.CmdWhatsApp(ctx, home.Dir(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout())
}

func whatsappCloudGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return gateway.CmdWhatsAppCloud(ctx, home.Dir(ctx), namespace(cli, cmd, path, args), cmd.OutOrStdout())
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

// Switches file logging to another mode for the dashboard entry points
func setupLoggingMode(mode string) {
	_, _ = logging.SetupLogging(context.Background(), logging.Options{Mode: mode})
}

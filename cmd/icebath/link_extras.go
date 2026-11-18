//go:build !icebath_core && !icebath_noextras

package main

// Links the extras tier, whole subsystems a terminal UI session does not need
import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/acp"
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/agentimport"
	_ "github.com/nickheyer/icebath/pkg/agentimport/openclaw"
	"github.com/nickheyer/icebath/pkg/app"
	"github.com/nickheyer/icebath/pkg/backup"
	"github.com/nickheyer/icebath/pkg/botdesktop"
	"github.com/nickheyer/icebath/pkg/browser"
	_ "github.com/nickheyer/icebath/pkg/builtin/agentimporters"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/codeexec"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/hosted"
	_ "github.com/nickheyer/icebath/pkg/gatewayapi"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/linktier"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/lsp"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate"
	"github.com/nickheyer/icebath/pkg/startupfast"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	_ "github.com/nickheyer/icebath/pkg/vxl"
	"github.com/spf13/cobra"
)

var _ acp.ReasoningCallbackSetter = (*agent.AIAgent)(nil)

func init() {
	linktier.Link(linktier.Extras)
	childEntriesAfterPruneExtras = append(childEntriesAfterPruneExtras, func(ctx context.Context) { botdesktop.DispatchIdleWatchEntry() })
	childEntriesAfterCodex = append(childEntriesAfterCodex, func(ctx context.Context) { cron.DispatchExternalWorkerEntry(exit) })
	childEntriesLast = append(childEntriesLast, func(ctx context.Context) { acp.DispatchModuleEntry(ctx, exit) })
	tierWiring = append(tierWiring, installExtrasWiring)
	tierCLI = append(tierCLI, bindCron, bindLSP, bindComputerUse, func(cli *commands.CLI) {
		bindGroups(cli, map[string]groupHandler{"acp": acpGroup, "backup": backupGroup, "browser": browserGroup, "import": importGroup,
			"kanban": kanbanGroup, "vault": vaultGroup})
	})
	tierAtExit = append(tierAtExit, func() { browser.AtexitCleanup(context.Background()) }, codeexec.ShutdownAllKernels,
		codeexec.ShutdownAllRemoteKernels, codeexec.ShutdownComputerUseBackends, cron.ShutdownParallelPools, lsp.AtExitShutdown)
	tierChatLaunch = append(tierChatLaunch, pinKanbanBoardEnv)
	tierServices = append(tierServices, func(s *plugins.Services) {
		s.CurrentCronExecution = func(ctx context.Context) any {
			if exec := cron.CurrentCronExecution(ctx); exec != nil {
				return exec
			}
			return nil
		}
		s.BuildPluginBlueprint = cron.BuildPluginBlueprint
	})
	tierPluginSDK = append(tierPluginSDK, cron.ServePluginSDK)
}

// Hooks the extras tier installs into the core packages
func installExtrasWiring() {
	acp.NewSurfaceLabelReader = func(db *state.SessionDB) acp.RichSessionLister { return gatewaycli.NewSurfaceLabelReader(db) }
	acp.NewPreparedMutations = func() acp.PreparedMutations { return app.NewACPMutations() }
	acp.SlashMutation, acp.ModelRefusalText, acp.ConfirmationTitle = app.ACPSlashMutation, app.ACPModelRefusalText, app.ACPConfirmationTitle
	acp.CommandRefusal, acp.RewindTo = app.ACPCommandRefusal, app.ACPRewindTo
	acp.ReplayGapOwedReply, acp.ReplayGapPendingStatus, acp.ReplayGapSettled = app.ACPReplayGapOwedReply, app.ACPReplayGapPendingStatus, app.ACPReplayGapSettled
	acp.EnvLaunchFlags = gatewaycli.EnvLaunchFlags
	acp.ResolveCommand = func(name string) (string, bool) {
		if d := commands.Resolve(name); d != nil {
			return d.Name, true
		}
		return "", false
	}
	acp.RunModelSetup = func(context.Context) int { return run([]string{"model"}) }
	gateway.HostedEntryKeywords = hosted.EntryKeywords
	kanban.ParseSlash = app.ParseKanbanSlash
	cron.SetupWorkerLogging = logging.SetupHomeLogging
	cron.WorkerProcessBoot = cronWorkerProcessBoot
	cron.RegisterWorkerConfigHooks = gateway.RegisterWorkerConfigHooks
	cron.RegisterEstopCheck(tools.CheckPaused)
	selfupdate.LoadCronJobsHook = loadCronJobsForDrainReport
	gateway.SessionReadLines.Cron = cron.CronCommandLines
	agent.KanbanBudgetExhaustedHook = kanban.RecordBudgetExhausted
	pm.CuaDriverChildEnv = cuaDriverChildEnv
	gateway.ReleaseVaultUnlocks = browser.ReleaseVaultSession
	gateway.WebhookExecuteJobForEvent = cron.ExecuteJobForEvent
	agent.ResetScreenshotDedup = codeexec.ResetScreenshotDedup
	agent.DelegatedChildDisposers = append(agent.DelegatedChildDisposers, codeexec.ShutdownKernelsForDelegatedChild)
	tools.ProjectBoardWorkdirBinder = bindProjectBoardWorkdir
	startupfast.UpdateCheck = selfupdate.PassiveCheckForUpdates
	provider.AgentImportRunner = func(ctx context.Context, icebathHome string, o provider.AgentImportOptions, console *cliui.Console) (int, error) {
		return agentimport.ImportAgent(home.WithOverride(ctx, icebathHome), agentimport.Args{Console: console, Agent: o.Agent,
			Source: o.Source, DryRun: o.DryRun, Overwrite: o.Overwrite, Yes: o.Yes, ActivateRoutines: o.ActivateRoutines,
			MigrateSecrets: o.MigrateSecrets, FromSetup: o.FromSetup, Bot: o.Bot})
	}
}

func acpGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return acp.CmdACP(ctx, namespace(cli, cmd, path, args), cmd.OutOrStdout())
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
			if last := cli.LastGiven(cmd, "no-agent", "agent"); last != "" {
				on := last == "no-agent"
				a.NoAgent = &on
			}
			switch {
			case boolean("pin"):
				a.Pinned = true
			case boolean("unpin"):
				a.Pinned = false
			}
			if last := cli.LastGiven(cmd, "continuity", "no-continuity"); last != "" {
				a.Continuity = last == "continuity"
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
	cli.Bind("lsp", func(cmd *cobra.Command, args []string) error {
		ctx := cmdCtx(cmd)
		return status(lsp.RunCommand(ctx, tools.LSPCommandContext(processToolsEnv(ctx)), lsp.CommandArgs{}, cmd.OutOrStdout(), cmd.ErrOrStderr()), nil)
	})
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

// Pins the active kanban board into the env so in process tools and shelled out calls agree
func pinKanbanBoardEnv() {
	if os.Getenv("ICEBATH_KANBAN_BOARD") != "" {
		return
	}
	if board := kanban.GetCurrentBoard(context.Background()); board != "" {
		os.Setenv("ICEBATH_KANBAN_BOARD", board)
	}
}

// Import time boot of for a cron worker, restoring, relaunching and activating as main does
func cronWorkerProcessBoot() {
	if restoreInterruptedPull() {
		relaunchAfterRestore()
	}
	pm.PinLauncherHome()
	bootLaunch()
	home.ExportScratchTmpEnv(context.Background())
}

func backupGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	if nsBool(ns, "quick") {
		return backup.RunQuickBackup(ctx, home.Dir(ctx), ns, cmd.OutOrStdout())
	}
	ok, err := backup.RunBackup(ctx, home.Dir(ctx), ns, cmd.OutOrStdout())
	var exit *backup.SystemExit
	if errors.As(err, &exit) {
		return exit.Code, nil
	}
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
	ns := namespace(cli, cmd, path, args)
	ns["browser_action"] = nilWhenEmpty(secondWord(path))
	return browser.BrowserCommand(ctx, processToolsEnv(ctx), ns, cmd.OutOrStdout())
}

func kanbanGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	return kanban.KanbanCommand(ctx, namespace(cli, cmd, path, args), cmd.OutOrStdout(), cmd.ErrOrStderr())
}

// Runs icebath vault with the subcommand under vault_action and key migrate under vault_key_action
func vaultGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	words := strings.Fields(path)
	ns["vault_action"] = nil
	ns["vault_key_action"] = nil
	if len(words) > 1 {
		ns["vault_action"] = words[1]
	}
	if len(words) > 2 && words[1] == "key" {
		ns["vault_key_action"] = words[2]
	}
	return browser.VaultCommand(ctx, home.Dir(ctx), ns, cmd.OutOrStdout())
}

// Cron job id and name pairs of a home in load order for the update drain report
func loadCronJobsForDrainReport(ctx context.Context, h string) ([][2]string, error) {
	jobs, err := cron.LoadJobsForHome(ctx, h)
	if err != nil {
		return nil, err
	}
	out := make([][2]string, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, [2]string{pyStr(job.ID), job.Name})
	}
	return out, nil
}

// Computer use driver child env of the process home, a raise of the bot desktop read returned as the error
func cuaDriverChildEnv() (env map[string]string, err error) {
	defer func() {
		if r := recover(); r != nil {
			env, err = nil, fmt.Errorf("%v", r)
		}
	}()
	return codeexec.CuaDriverChildEnv(processToolsEnv(context.Background())), nil
}

// Points a bound kanban board's default workdir at the project's primary repo
func bindProjectBoardWorkdir(ctx context.Context, board, primaryPath string) error {
	slug, err := kanban.NormalizeBoardSlug(&board)
	if err != nil || slug == "" {
		return err
	}
	if slug != kanban.DefaultBoard && !kanban.BoardExists(&slug) {
		return nil
	}
	_, err = kanban.WriteBoardMetadata(ctx, &slug, kanban.BoardMetadataUpdate{DefaultWorkdir: &primaryPath})
	return err
}

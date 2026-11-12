package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/nickheyer/icebath/pkg/acp"
	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/app"
	"github.com/nickheyer/icebath/pkg/botdesktop"
	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/builtin/teamspipeline"
	"github.com/nickheyer/icebath/pkg/codeexec"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/dashboard"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/slack"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/telegram"
	_ "github.com/nickheyer/icebath/pkg/gatewayapi"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/gatewayrun"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Assigns every package level hook the lower packages leave to the binary, before any entry dispatches
func installProcessWiring() {
	registerCommandArgv()
	commands.BannerVersionLabel = app.FormatBannerVersionLabel
	gatewayrun.ResolveEphemeralSystemPromptFromConfig = commands.ResolveEphemeralSystemPrompt
	gatewayrun.ParseServiceTierConfig = app.ParseServiceTierConfig
	gatewayrun.BindTeamsPipelineRuntime = func(ctx context.Context, r *gatewayrun.GatewayRunner) (bool, error) {
		return teamspipeline.BindGatewayRuntime(ctx, r)
	}
	gatewayrun.ResolveCommand = func(name string) (string, bool) {
		if d := commands.Resolve(name); d != nil {
			return d.Name, true
		}
		return "", false
	}
	gatewayrun.SlashConfirmPending = func(sessionKey string) bool {
		_, ok := commands.Confirms.Pending(sessionKey)
		return ok
	}
	gatewayrun.SlashConfirmClear = commands.Confirms.Clear
	gatewayrun.SlashConfirmGetPending = func(sessionKey string) (string, bool) {
		p, ok := commands.Confirms.Pending(sessionKey)
		return p.ConfirmID, ok
	}
	gatewayrun.SlashConfirmResolve = func(ctx context.Context, sessionKey, confirmID, choice string) string {
		reply, _ := commands.Confirms.Resolve(ctx, sessionKey, confirmID, choice, commands.ConfirmTimeout)
		return reply
	}
	gateway.SlashConfirmResolve = gatewayrun.SlashConfirmResolve
	gatewayrun.SlashConfirmClearIfStale = func(sessionKey string) bool {
		return commands.Confirms.ClearIfStale(sessionKey, commands.ConfirmTimeout)
	}
	gatewayrun.RunGatewayExecQuickCommand = commands.RunGatewayExecQuickCommand
	gatewayrun.SlashConfirmRegister = func(sessionKey, confirmID, command string, handler func(ctx context.Context, choice string) (string, error)) {
		commands.Confirms.Register(sessionKey, confirmID, command, handler)
	}
	gatewayrun.SaveConfigValue = app.SaveConfigValue
	gatewayrun.SanitizeTelegramName = commands.SanitizeTelegramName
	gatewayrun.KanbanRunSlash = app.RunKanbanSlash
	gatewayrun.RunCuratorSlash = app.RunCuratorSlash
	gatewayrun.RandomTip = app.RandomTip
	gatewayrun.ActivePersonalityName = commands.ActivePersonalityName
	gatewayrun.AvailablePersonalities = func(cfg map[string]any) map[string]any { return commands.AvailablePersonalities(config.Tree(cfg)) }
	gatewayrun.DescribePersonality = commands.DescribePersonality
	gatewayrun.PersistPersonality = commands.PersistPersonality
	gatewayrun.ResolvePersonality = func(value any, cfg map[string]any) (string, string, error) {
		return commands.ResolvePersonality(value, config.Tree(cfg))
	}
	gatewayrun.GatewayDebugLocalReport = selfupdate.GatewayDebugLocalReport
	gatewayrun.GatewayDebugUploadReply = selfupdate.GatewayDebugUploadReply
	gatewayrun.SweepExpiredPastes = selfupdate.SweepExpiredPastes
	gatewayrun.RestoreUserProxyEnv = selfupdate.RestoreUserProxyEnv
	gatewayrun.CheckRuntime, gatewayrun.MaybeRunBootBootstrap = selfupdate.CheckRuntime, selfupdate.MaybeRunBootBootstrap
	gatewayrun.ConfigureWindowsStdio = app.ConfigureWindowsStdio
	gatewayrun.RunApprovalModeCommand = func(ctx context.Context, h string, env *tools.Env, requestedMode string) (string, error) {
		result, err := commands.RunApprovalModeCommand(ctx, h, env, requestedMode)
		return result.Message, err
	}
	gatewayrun.BuildInitPromptForCwd = app.BuildInitPromptForCwd
	gatewayrun.ResolveCommandBusy = func(name string) (string, string, string, bool) {
		if d := commands.Resolve(name); d != nil {
			return d.Name, string(d.BusyPolicy), d.BusyHandler, true
		}
		return "", "", "", false
	}
	gatewayrun.IsGatewayKnownCommand = func(name string) bool { return commands.IsGatewayKnownCommand(commands.Default, name) }
	gatewayrun.GatewayKnownCommand = func(name string) bool { return commands.GatewayKnownCommands()[name] }
	gatewayrun.ExecuteSlashCommand = func(ctx context.Context, name, surface, args string, options map[string]any) (gatewayrun.SlashCommandReply, error) {
		reply, err := commands.ExecuteCommand(ctx, name, commands.CommandContext{Surface: surface, Args: args, Options: options})
		return gatewayrun.SlashCommandReply{Text: reply.Text, Data: reply.Data, Format: reply.Format}, err
	}
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
	gatewaycli.InstallMigrateS6Hooks()
	gateway.S6Running, gateway.S6Scandir = gatewaycli.S6Running, func() string { return gatewaycli.S6DynamicScandir }
	gateway.S6IsRunning = func(name string) (bool, error) {
		return gatewaycli.NewS6ServiceManager(gatewaycli.S6DynamicScandir).IsRunning(name)
	}
	gateway.S6Start = func(name string) error {
		return gatewaycli.NewS6ServiceManager(gatewaycli.S6DynamicScandir).Start(name)
	}
	cron.SetupWorkerLogging = logging.SetupHomeLogging
	cron.RegisterWorkerConfigHooks = gateway.RegisterWorkerConfigHooks
	cron.RegisterEstopCheck(tools.CheckPaused)
	selfupdate.LoadCronJobsHook = loadCronJobsForDrainReport
	gatewaycli.DescribeActiveWorkUnit = selfupdate.DescribeActiveWorkUnit
	gatewaycli.SealedSteward = selfupdate.SealedSteward
	gateway.WebServer.ShouldRequireDashboardAuth = dashboard.ShouldRequireDashboardAuthAfterImport
	gateway.WebServer.StartServer = dashboard.StartServer
	dashboard.RestoreUserProxyEnv = selfupdate.RestoreUserProxyEnv
	gatewayrun.BuildGatewayListener = dashboard.BuildGatewayListener
	provider.RetirementIdleProof = dashboard.RetirementIdleProof
	dashboard.GatewayCLI.NeedsSudo = selfupdate.NeedsSudo
	dashboard.GatewayCLI.SudoNoninteractiveOk = selfupdate.SudoNoninteractiveOk
	dashboard.GatewayCLI.RefusalLines = gatewaycli.RefusalLines
	dashboard.GatewayCLI.InstalledServices = gatewayrun.InstalledServiceCount
	dashboard.TUILaunch.MakeArgv = app.MakeTUIArgv
	dashboard.TUILaunch.ApplyPythonEnv = app.ApplyTUIPythonEnv
	dashboard.UpdateCLI.IsCommitBuild = selfupdate.IsCommitBuild
	dashboard.UpdateCLI.CommitBuildMessage = selfupdate.CommitBuildUpdateMessage
	dashboard.UpdateCLI.UpdateAdmissionRefusal = selfupdate.UpdateAdmissionRefusal
	dashboard.UpdateCLI.RecordUpdateRefusal = selfupdate.RecordUpdateRefusal
	dashboard.UpdateCLI.CheckForUpdates = selfupdate.CheckForUpdatesForce
	dashboard.UpdateCLI.BuildMigrationPlan = gatewayrun.MigrationPlanDict
	dashboard.StatusCLI.ReadLogTail = app.ReadLogTail
	dashboard.StatusCLI.BuildDebugShare = selfupdate.DebugShareForDashboard
	dashboard.StatusCLI.RedactDebugSupportText = selfupdate.RedactDebugSupportText
	dashboard.ConsoleCLI.NewEngine = app.NewDashboardConsole
	gateway.WebUISourceBuild.ProductCurrent = selfupdate.SourceProductCurrent
	gateway.WebUISourceBuild.Env = func(ctx context.Context) (map[string]string, error) {
		return selfupdate.SourceBuildEnv(ctx, nil, false, true)
	}
	gateway.WebUISourceBuild.PrepareLaunch = selfupdate.PrepareLaunchDependencies
	gateway.WebUISourceBuild.BuildWeb = func(ctx context.Context, projectRoot string, env map[string]string) error {
		return selfupdate.BuildSourceWeb(ctx, projectRoot, env, "")
	}
	gateway.WebUISourceBuild.ExceptionName = selfupdate.ExceptionName
	gateway.RefreshLiveSessions = tuiserver.RefreshLiveSessions
	tuiserver.OpenSessionDBAtPath = dashboard.OpenSessionDBAtPath
	tuiserver.MintDisplayTicket, tuiserver.DisplayAdmissionOf = dashboard.MintWSTicket, dashboard.SocketAdmission
	tuiserver.ShareNousDiagnostics, tuiserver.RedactDebugSupportText = selfupdate.ShareNousDiagnostics, selfupdate.RedactDebugSupportTextCapped
	tuiserver.StatusReport.Fields, tuiserver.StatusReport.Lines = app.TUIStatusFields, app.TUIStatusLines
	tuiserver.NormalizePersonalityName = commands.NormalizePersonalityName
	tuiserver.RenderPersonalityPrompt = commands.RenderPersonalityPrompt
	tuiserver.PersistPersonality = commands.PersistPersonality
	tuiserver.CommandsByCategory = func() []tuiserver.HelpCategory {
		var out []tuiserver.HelpCategory
		for _, g := range commands.CommandsByCategory(commands.Default) {
			category := tuiserver.HelpCategory{Category: g.Category}
			for _, row := range g.Rows {
				category.Rows = append(category.Rows, [2]string{row.Command, row.Description})
			}
			out = append(out, category)
		}
		return out
	}
	tuiserver.ActivePersonalityName = commands.ActivePersonalityName
	tuiserver.AvailablePersonalities = func(cfg map[string]any) map[string]any {
		return commands.AvailablePersonalities(config.Tree(cfg))
	}
	tuiserver.Banner.ReleaseDate = app.ReleaseDate
	tuiserver.Banner.AvailableSkills = app.AvailableSkills
	tuiserver.Banner.UpdateResult = app.UpdateResult
	gateway.InventoryBanner.ReleaseDate = app.ReleaseDate
	gateway.InventoryBanner.AvailableSkills = app.AvailableSkills
	gateway.InventoryBanner.UpdateResult = app.UpdateResult
	gateway.InventoryBanner.PrefetchUpdateCheck = app.PrefetchUpdateCheck
	gateway.InventoryBanner.ToolsetAvailability = app.ToolsetAvailability
	gateway.ConsumeTypedStopPhrase = tuiserver.ConsumeTypedStopPhrase
	gateway.HasAnyProviderConfigured = hasAnyProviderConfigured
	tuiserver.Prefill.ResolveFile = app.ResolvePrefillMessagesFile
	tuiserver.Prefill.Load = app.LoadPrefillMessages
	tuiserver.ForeignSessions.List = app.ListForeignSessions
	tuiserver.ForeignSessions.Preview = app.PreviewForeignSession
	tuiserver.ForeignSessions.Import = app.ImportBrowserSession
	tuiserver.Personality.EphemeralSystemPrompt = commands.ResolveEphemeralSystemPrompt
	tuiserver.GatewayKnownCommands = commands.GatewayKnownCommands
	tuiserver.CatalogRegistry = catalogRegistry
	tuiserver.SaveConfigValue = app.SaveConfigValue
	gateway.CommandCatalog, gateway.SlashCompletions = tuiserver.CommandCatalog, tuiserver.SlashCompletions
	gateway.ResolveRegistryCommand = resolveRegistryCommand
	gateway.SessionReadLines.Toolsets, gateway.SessionReadLines.Platforms = app.ToolsetsLines, app.PlatformsLines
	gateway.SessionReadLines.Cron = cron.CronCommandLines
	gateway.TUIReadRender.Status, gateway.TUIReadRender.Context = app.TUIStatusRead, app.TUIContextRead
	gateway.ViewerAutomationInvoke = agent.InvokeViewerAutomation
	gateway.NormalizeRunBudgetSeconds = agent.NormalizeRunBudgetSeconds
	gateway.RosterProfileInfo = func(ctx context.Context, name, path string, isDefault bool) (gateway.RosterProfile, error) {
		info, err := manage.ProfileInfoOf(ctx, name, path, isDefault)
		return gateway.RosterProfile{IsDefault: info.IsDefault, Model: info.Model, Provider: info.Provider,
			Description: info.Description, DisplayName: info.DisplayName, SkillCount: info.SkillCount}, err
	}
	tuiserver.ResolveCommand = func(name string) (tuiserver.ResolvedCommand, bool) {
		d := commands.Resolve(name)
		if d == nil {
			return tuiserver.ResolvedCommand{}, false
		}
		return tuiserver.ResolvedCommand{Name: d.Name, Category: d.Category, Describe: d.Describe,
			DesktopSubcommands: commands.MetaOf(d.Name).DesktopSubcommands}, true
	}
	tuiserver.SlashCompleter = func(ctx context.Context, skillCommands, skillBundles skills.SkillCommands) func(string) []tuiserver.SlashCompletion {
		completer := commands.TUISlashCompleter(ctx, skillCommands, skillBundles)
		return func(text string) []tuiserver.SlashCompletion {
			found := completer.Complete(text)
			out := make([]tuiserver.SlashCompletion, len(found))
			for i, c := range found {
				out[i] = tuiserver.SlashCompletion{Text: c.Text, Display: c.Display, Meta: c.Meta}
			}
			return out
		}
	}
	gateway.WebappRendererBuild.Prepare = func(ctx context.Context, projectRoot string, force, skipBuild, explicit bool) (string, error) {
		return selfupdate.PrepareWebappRenderer(ctx, projectRoot, force, skipBuild, explicit)
	}
	gateway.WebappRendererBuild.PackagedDist = selfupdate.PackagedWebappDist
	gateway.WebappRendererBuild.ExceptionName = selfupdate.ExceptionName
	selfupdate.RunCLIHook = run
	agent.KanbanBudgetExhaustedHook = kanban.RecordBudgetExhausted
	agent.TUIGatewayToolsets = tuiserver.GatewayToolsets
	agent.SessionSteerAuthority = tuiserver.SessionSteerAuthority
	mcp.TUILaunchHome, mcp.TUIProfileHome, mcp.TUIProfileRuntimeScope = tuiserver.LaunchHome, tuiserver.ProfileHome, tuiserver.ProfileRuntimeScope
	observability.ResolveSessionPlatformHook = tuiserver.ResolveSessionPlatform
	pm.InteractiveAppRunning = app.PromptAppIsRunning
	provider.SetFreeTierGatewayHooks(provider.FreeTierGatewayHooks{OwnsRuntimeLock: gateway.OwnsGatewayRuntimeLock})
	pm.CuaDriverChildEnv = cuaDriverChildEnv
	pm.LooksLikeGatewayCommandLine = gateway.LooksLikeGatewayCommandLine
	tools.ApprovalHookInvoker = approvalHookInvoker
	tools.LoadCLIConfig = app.CLIConfig
	tools.TerminalOutputTransformHook = terminalOutputTransform
	tuiserver.SaveClipboardImage, tuiserver.HasClipboardImage = app.SaveClipboardImage, app.HasClipboardImage
	state.SecretEnvName = codeexec.IsSecretEnvName
	state.SecretEnvSubstrings = codeexec.SecretSubstrings
	state.MessagingPlatformLabels = gateway.MessagingPlatformLabels
	state.MessagingPlatformSource = gateway.IsMessagingPlatformSource
	state.SameOwner = gateway.SameOwner
	state.OwnersAgree = gateway.OwnersAgree
	state.ParseAuthenticatedSubject = gateway.ParseAuthenticatedSubject
	state.AdmissionMediaReferences = gateway.AdmissionMediaReferences
	state.HostedDocumentReferences = gateway.HostedDocumentReferences
	state.ReleaseUnheldMedia = gateway.ReleaseUnheldMediaRetainingHistory
	state.SpilloverSQLFilter = tools.SpilloverSQLFilter
	state.SpilloverNamesInText = tools.SpilloverNamesInText
	state.ReferencedSpilloverNames = tools.ReferencedSpilloverNames
	tools.IsWorkerProcess = agent.IsWorkerProcess
	state.SessionKeyNamespace = gateway.SessionKeyNamespace
	state.ExclusiveMaintenance = gateway.ExclusiveMaintenance
	state.IsOwnershipConflict = func(err error) bool {
		var conflict *gateway.OwnershipConflict
		return errors.As(err, &conflict)
	}
	state.LiveRuntimeOwner = gateway.LiveRuntimeOwner
	state.CanonicalHome = gateway.CanonicalHome
	state.ProfileLabelForHome = gateway.ProfileLabelForHome
	state.ReadGatewayLockRecord = gateway.ReadGatewayLockRecord
	gateway.ResolvePrefillMessagesFile = app.ResolvePrefillMessagesFile
	gateway.AncillaryAgent.SubagentRecords = agent.SubagentRecords
	gateway.AncillaryAgent.SteerSubagent = agent.SteerOwnedSubagent
	gateway.AncillaryAgent.TranscriptPath = agent.SubagentTranscriptPath
	gateway.AncillaryAgent.ControlSnapshot = agent.ControlSnapshotOrRefusal
	gateway.AncillaryAgent.SetSpawnPaused = agent.SetSpawnPaused
	gateway.PathCompletionItems = tuiserver.PathCompletionItems
	gateway.OneshotAgent.TemplateKnown = agent.OneshotTemplateKnown
	gateway.ContextAgent.UsageStats = agent.SessionUsageStats
	gateway.ContextAgent.Breakdown = agent.SessionContextBreakdown
	gateway.ContextAgent.StoredBreakdown = agent.SessionStoredContextBreakdown
	gateway.ContextAgent.Marks = agent.SessionContextMarks
	gateway.PromptAttachments.StageBrowserFile = tuiserver.StageBrowserFileAttachment
	gateway.PromptAttachments.StageSessionFile = tuiserver.StageSessionFileAttachment
	gateway.PromptAttachments.RefPath = tuiserver.AttachmentRefPath
	gateway.PromptAttachments.FormatRefValue = tuiserver.FormatRefValue
	gateway.ImageAttachments.MaxBytes = tuiserver.AttachBytesMaxBytes
	gateway.ImageAttachments.DecodeBase64 = tuiserver.DecodeAttachBase64
	gateway.ImageAttachments.SniffImageExt = tuiserver.SniffImageExt
	gateway.ImageAttachments.StageImageBytes = tuiserver.StageImageBytes
	gateway.MainRuntimeFromAgent = tuiserver.MainRuntimeFromAgent
	tuiserver.BindHostedRoomServer()
	gateway.RunOneshot = agent.RunOneshotCall
	gateway.ReleaseVaultUnlocks = browser.ReleaseVaultSession
	gateway.ResolvePersonality = commands.ResolvePersonality
	gateway.PersistPersonality = commands.PersistPersonality
	gateway.RunApprovalModeCommand = func(ctx context.Context, h string, env *tools.Env, mode string) (bool, string, error) {
		result, err := commands.RunApprovalModeCommand(ctx, h, env, mode)
		return result.OK, result.Mode, err
	}
	gateway.ErrorSurfaceFromResult = agent.BuildErrorSurfaceFromResult
	gateway.SessionCompress = agent.SessionCompressAgent{}
	gateway.ExpandContextReferences = agent.ExpandGatewayContextReferences
	gateway.LoadPrefillMessages = app.LoadPrefillMessages
	gateway.WebhookExecuteJobForEvent = cron.ExecuteJobForEvent
	gateway.CommandBusyPolicy = func(name string) (bool, bool) {
		return commands.ShouldBypassActiveSession(name), commands.IsInterruptThenDispatch(name)
	}
	slack.SubcommandMap = func(ctx context.Context) map[string]string {
		return commands.SlackSubcommandMap(commands.Default, rawConfigTree(ctx))
	}
	gateway.RelaySlackSubcommandMap = slack.SubcommandMap
	telegram.InlineCommands = telegramInlineCommands{}
	codeexec.BotDesktopEnv = botdesktop.DesktopEnv
	codeexec.BotDesktopToolPlacement = botdesktop.ToolPlacement
	codeexec.BotDesktopPlacementWhere = botDesktopPlacementWhere
	codeexec.BotDesktopPublishedEnv = botdesktop.MustPublishedEnv
	codeexec.BotDesktopSandboxEnv = botdesktop.TerminalEnvironment
	codeexec.BotDesktopProfileName = botdesktop.ProfileName
	codeexec.BotDesktopTouchActivity = botdesktop.TouchActivity
	codeexec.BotDesktopCuaMCPInvocation = func(ctx context.Context, sandbox tools.Environment, profile string, published map[string]string) (string, []string, error) {
		return botdesktop.CuaMCPInvocation(sandbox, profile, published)
	}
	codeexec.BotDesktopAssertAgentMayAct = botDesktopAssertAgentMayAct
	codeexec.BotDesktopLeaseEpoch = func(env *tools.Env) int { return botdesktop.LeaseGet(botdesktop.ProfileKey(env)).EpochInt() }
	codeexec.BotDesktopEnsureStartedForTool = botdesktop.EnsureStartedForTool
	codeexec.BotDesktopInstallPackages = botDesktopInstallPackages
	agent.ResetScreenshotDedup = codeexec.ResetScreenshotDedup
	tools.DiagnosticProcessEvent = agent.DiagnosticProcessEvent
	tools.MediaDeliveryPathValidator = mediaDeliveryPathValidator
	tools.TerminalBackendInstallHint = provider.TerminalBackendInstallHint
	tools.GitlockRunGit = selfupdate.GitlockRunGit
	tools.AsyncDelegationOwnerProbes.PidExists = kanban.GatewayStatusProbes.PidExists
	tools.AsyncDelegationOwnerProbes.ProcessStartTime = gatewayProcessStartTime
	tools.AsyncDelegationRetirement = provider.BackendRetirement()
	tools.ProfileSecrets = provider.ProfileSecretSource()
	tools.NousAccount = provider.NousAccountSource()
	tools.RuntimeProviderCredentials = provider.ToolRuntimeCredentials
	tools.VisionCapabilities = provider.VisionCapabilities{}
	agent.InstallDelegationRegistryHooks()
	tools.CachedContextLength = func(ctx context.Context, modelID, baseURL string) (int, bool) {
		return provider.GetCachedContextLength(ctx, modelID, baseURL, false)
	}
	tools.ModelContextLength = provider.ToolModelContextLength
	tools.AllSkillNames = skills.AllSkillNames
	tools.ProjectBoardWorkdirBinder = bindProjectBoardWorkdir
	observability.SkillSourceIDAliasesHook = skills.SourceIDAliases
	app.SecurityAdvisoriesStartupBanner = func(ctx context.Context) string {
		return plugins.AdvisoryStartupBanner(ctx, plugins.DetectCompromised(plugins.Advisories))
	}
	app.TryAcquireActiveSession = app.AcquireActiveSessionLease
	app.FormatRefusalStderr = activesessions.FormatRefusalStderr
	provider.InstallPluginManagerHooks(pluginManagerHooks())
	plugins.InstallServices(pluginServices())
	app.SetPrintVersionInfo(printVersionInfo)
	app.ConsoleCommandTree = newCLI
}

// Raw config of the bound home as a tree, empty on a read failure
func rawConfigTree(ctx context.Context) config.Tree {
	raw, err := config.ReadRawConfig(ctx, home.Dir(ctx))
	if err != nil || raw == nil {
		return config.Tree{}
	}
	return config.Tree(raw)
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

// The pkg/commands readers behind the Telegram inline picker
type telegramInlineCommands struct{}

func (telegramInlineCommands) TelegramBotCommands(ctx context.Context) ([]telegram.CatalogItem, error) {
	var out []telegram.CatalogItem
	for _, e := range commands.TelegramBotCommands(commands.Default, rawConfigTree(ctx), true) {
		out = append(out, telegram.CatalogItem{Name: e.Name, Description: e.Description})
	}
	return out, nil
}

func (telegramInlineCommands) GatewaySkillEntries(ctx context.Context, platform string, maxSlots int, reserved map[string]bool,
	descLimit int, sanitize func(string) string) ([]telegram.CatalogItem, error) {
	cfg := rawConfigTree(ctx)
	entries, _ := commands.GatewaySkillEntries(commands.Default, platform, commands.HomeSkillSource(home.Dir(ctx), cfg), maxSlots, reserved, descLimit, sanitize)
	var out []telegram.CatalogItem
	for _, e := range entries {
		out = append(out, telegram.CatalogItem{Name: e.Name, Description: e.Description})
	}
	return out, nil
}

func (telegramInlineCommands) SanitizeTelegramName(raw string) string {
	return commands.SanitizeTelegramName(raw)
}

// The higher package collaborators pkg/plugins reaches through its Services
func pluginServices() *plugins.Services {
	s := &plugins.Services{
		InvokeLifecycleHook: agent.LifecycleInvokeHook,
		RequestToolApproval: func(ctx context.Context, toolName, message, ruleKey, turnID, toolCallID, sessionID string) (map[string]any, error) {
			return tools.RequestPluginToolApproval(ctx, callerToolsEnv(ctx), toolName, message, ruleKey, turnID, toolCallID, sessionID)
		},
		FailoverReason:              failoverReason,
		ClearDashboardAuthProviders: gateway.ClearDashboardAuthProviders,
		MemoryProviderDir:           memory.ProviderDirForHome,
		HostObject:                  hostedObjectName,
		BindProfileSecretScope:      provider.BindProfileSecretScope,
		GetSecret:                   provider.GetSecret,
		PluginLLM:                   agent.PluginLLMClient(),
		SubagentLifecycle:           agent.SubagentDelegationService(),
		DelegationBackends:          func(c *plugins.Context) any { return agent.DelegationBackendsFor(c) },
		CurrentCronExecution: func(ctx context.Context) any {
			if exec := cron.CurrentCronExecution(ctx); exec != nil {
				return exec
			}
			return nil
		},
		CallMCPTool:              mcp.CallToolForPlugin,
		RedeemSideQuestion:       agent.RedeemSideQuestion,
		BoundSessionKey:          func(ctx context.Context) string { return tools.BoundSessionKey(tools.ToolEnvFrom(ctx)) },
		ResolveBuiltinCommand:    func(name string) bool { return commands.Resolve(name) != nil },
		DispatchTool:             tools.DispatchForPlugin,
		RegisterContextReference: agent.RegisterContextReference,
		DashboardAuth:            gateway.DashboardAuthRegistryService(),
		Platforms:                gateway.PlatformRegistryService(),
		ProviderRegistries: map[string]plugins.ScopedRegistry{
			"image_gen_provider":            media.ImageGenScopedRegistry(),
			"video_gen_provider":            media.VideoGenScopedRegistry(),
			"tts_provider":                  media.TTSScopedRegistry(),
			"transcription_provider":        media.TranscriptionScopedRegistry(),
			"web_search_provider":           agent.WebSearchScopedRegistry(),
			"browser_provider":              browser.BrowserProviderRegistry.Scoped(),
			"browser_page_backend":          browser.BrowserPageBackendRegistry.Scoped(),
			"terminal_environment_provider": agent.TerminalEnvScopedRegistry(),
			"secret_source":                 provider.SecretSourceScopedRegistry(),
			"delegation_provider":           agent.DelegationScopedRegistry(),
			"login_backend":                 browser.LoginBackendScopedRegistry(),
		},
		BuiltinAuxTaskKeys:         provider.BuiltinAuxTaskKeys,
		RegisterRedactionPatterns:  tools.RegisterRedactionPatterns,
		PluginSDK:                  pluginSDK,
		BuildPluginBlueprint:       cron.BuildPluginBlueprint,
		SecretSources:              provider.PluginSecretSources(),
		PlatformDeclarations:       agent.PlatformDeclarationsService(),
		OAuthSurface:               provider.OAuthSurface,
		OAuthAdmissionProblems:     provider.OAuthAdmissionProblems,
		OAuthAdmissionWarnings:     provider.OAuthAdmissionWarnings,
		PortableServerAvailability: mcp.PortableServerAvailability,
		MaskedSecretPrompt:         provider.MaskedSecretPrompt,
		ServerAvailabilityReason:   mcp.ServerAvailabilityReason,
		DefinesMemoryProvider:      memory.DefinesMemoryProvider,
		PrepareMemoryProviderDependencies: func(ctx context.Context, name string) error {
			return memory.PrepareMemoryProviderDependenciesForPlugin(ctx, processToolsEnv(ctx), name)
		},
		RequiresAuthNotice:        provider.RequiresAuthNotice,
		DeclaresModelProviderKind: provider.DeclaresModelProviderKind,
		PluginOAuthKey:            provider.PluginOAuthKey,
		ForgetPluginGrants:        provider.ForgetPluginGrants,
		ReloadGatewayPlugins: func(ctx context.Context, target, profileHome string) (map[string]any, error) {
			return gateway.ReloadGatewayPluginsAnswer(target, profileHome, 30*time.Second), nil
		},
		HostRendezvous:              gateway.HostRendezvousService(),
		GatewayRunningPid:           gateway.GetRunningPid,
		InstalledGatewayServiceKind: gateway.InstalledGatewayServiceKind,
		RefreshPluginSessions:       gateway.RefreshPluginSessions,
		MCP:                         mcp.PluginConnector(),
		BroadcastGlobalEvent:        gateway.BroadcastGlobalEvent,
		ActiveMemoryProvider: func(ctx context.Context, h string) string {
			return memory.ActiveMemoryProvider(ctx, homeToolsEnv(ctx, h))
		},
		PruneInactiveMemoryProviderSkills: func(ctx context.Context, h, active string) {
			memory.PruneInactiveMemoryProviderSkills(ctx, homeToolsEnv(ctx, h), active)
		},
		LoadMemoryProvider: func(ctx context.Context, h, name string) { memory.LoadMemoryProvider(ctx, homeToolsEnv(ctx, h), name) },
		ParseServerLiveness: func(raw any) (plugins.ServerLiveness, error) {
			l, err := mcp.ParseLiveness(raw)
			return plugins.ServerLiveness(l), err
		},
		DiscoverMemoryProviders: func(ctx context.Context) [][2]string {
			var out [][2]string
			for _, p := range memory.DiscoverMemoryProviders(ctx, processToolsEnv(ctx)) {
				out = append(out, [2]string{p.Name, p.Description})
			}
			return out
		},
	}
	gateway.InstallPluginServices(s)
	return s
}

// Session Env of the calling turn bound on ctx, the process Env of the bound home outside a turn
func callerToolsEnv(ctx context.Context) *tools.Env {
	if env := tools.ToolEnvFrom(ctx); env != nil {
		return env
	}
	return processToolsEnv(ctx)
}

// The tool environment of one home with its config, for plugin facing memory calls
func homeToolsEnv(ctx context.Context, h string) *tools.Env {
	cfg, _ := config.LoadReadonly(ctx, h, nil)
	cwd, _ := os.Getwd()
	return &tools.Env{Home: h, Config: cfg, Cwd: cwd, Log: slog.Default()}
}

// Serves the plugin SDK ops, the cron ops first and the media, provider and gateway ops after
func pluginSDK(ctx context.Context, h, op string, args []any, kwargs map[string]any) (any, error) {
	if result, handled, err := cron.ServePluginSDK(ctx, h, op, args, kwargs); handled || err != nil {
		return result, err
	}
	return gateway.ServePluginSDK(ctx, h, op, args, kwargs)
}

// Name under which the host registers a hosted object of a kind, false when the object is not one
func hostedObjectName(kind string, obj any) (string, bool) {
	switch kind {
	case "memory_provider":
		return memory.HostedProviderName(obj)
	case "context_engine", "context_reference":
		return agent.HostedContextObjectName(kind, obj)
	case "dashboard_auth_provider":
		return gateway.HostedDashboardAuthProviderName(obj)
	}
	return "", false
}

// Exit hooks the Python modules registered with atexit, run once from exit
var atExitDone bool

func runAtExit() {
	if atExitDone {
		return
	}
	atExitDone = true
	run := func(fn func()) {
		defer func() { recover() }()
		fn()
	}
	run(state.RunTokenExitHooks)
	run(func() { tools.CleanupAllEnvironments(processToolsEnv(context.Background())) })
	run(func() { tools.CleanupSafeSkillsCopy() })
	run(func() { browser.AtexitCleanup(context.Background()) })
	run(codeexec.ShutdownAllKernels)
	run(codeexec.ShutdownAllRemoteKernels)
	run(codeexec.ShutdownComputerUseBackends)
	run(cron.ShutdownParallelPools)
	run(tuiserver.AtExit)
	run(tools.RelayAtExit)
	run(lspAtExit)
	run(func() { mcp.ShutdownMCPServers(context.Background(), nil, nil, 15*time.Second) })
	run(func() { provider.ShutdownCachedClients(context.Background()) })
	run(func() { provider.ShutdownLocalRuntime() })
	run(func() { plugins.FlushOutboundWebhooks(5 * time.Second) })
	run(func() { observability.ShutdownRuntimes(context.Background()) })
	run(gatewayrun.BootstrapAtExit)
	run(gateway.HostRendezvousAtExit)
	run(logging.Shutdown)
	for _, s := range []*os.File{os.Stdout, os.Stderr} {
		s.Sync()
	}
}

// Placement where of the bot desktop, a resolve failure raising like the Python resolve()
func botDesktopPlacementWhere(env *tools.Env) string {
	p, err := botdesktop.Resolve(env)
	if err != nil {
		panic(err)
	}
	return p.Where
}

// Lease epoch an agent action is admitted under, HumanHasControl while a human holds the screen
func botDesktopAssertAgentMayAct(env *tools.Env) (int, error) {
	lease, err := botdesktop.LeaseAssertAgentMayAct(botdesktop.ProfileKey(env))
	if err != nil {
		return 0, err
	}
	return lease.EpochInt(), nil
}

// Desktop package install with the busy refusal mapped onto the codeexec error the screen CLI reads
func botDesktopInstallPackages(env *tools.Env, askPassword func() (string, error), onLine func(string)) (int, error) {
	rc, err := botdesktop.InstallPackages(env, askPassword, onLine)
	var busy *botdesktop.InstallBusy
	if errors.As(err, &busy) {
		return rc, &codeexec.BotDesktopInstallBusy{Msg: busy.Msg}
	}
	return rc, err
}

// Validated media delivery path of a candidate, empty when the gateway refuses it and raising on a resolve failure
func mediaDeliveryPathValidator(env *tools.Env, candidate, sessionKey string) string {
	path, err := gateway.ValidateMediaDeliveryPath(tools.WithEnvHome(context.Background(), env), candidate, sessionKey)
	if err != nil {
		panic(err)
	}
	return path
}

// Lifecycle invoke_hook dispatch of an approval hook, a raise of the dispatch layer returned as the error
func approvalHookInvoker(ctx context.Context, hookName string, payload map[string]any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	agent.LifecycleInvokeHook(ctx, hookName, payload)
	return nil
}

// Results of the transform_terminal_output lifecycle hook, none when its dispatch raises so the output stays
func terminalOutputTransform(ctx context.Context, payload map[string]any) (results []any) {
	defer func() {
		if r := recover(); r != nil {
			results = nil
		}
	}()
	return agent.LifecycleInvokeHook(ctx, "transform_terminal_output", payload)
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

// Gateway status start time of a pid as the int the async delegation owner check compares
func gatewayProcessStartTime(pid int) (int64, bool) {
	switch v := kanban.GatewayStatusProbes.ProcessStartTime(pid).(type) {
	case int64:
		return v, true
	case int:
		return int64(v), true
	}
	return 0, false
}

// FailoverReason enum construction over a plugin supplied value, false for a value outside the enum
func failoverReason(value any) (any, bool) {
	r, ok := provider.ParseFailoverReason(value)
	if !ok {
		return nil, false
	}
	return r, true
}

// Points a bound kanban board's default workdir at the project's primary repo, hermes_cli/projects_cmd.py _cmd_bind_board
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

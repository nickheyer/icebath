package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/app"
	"github.com/nickheyer/icebath/pkg/botdesktop"
	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/codeexec"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/slack"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/telegram"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Assigns every package level hook the lower packages leave to the binary, before any entry dispatches
func installProcessWiring() {
	registerCommandArgv()
	commands.BannerVersionLabel = app.FormatBannerVersionLabel
	gateway.S6Running, gateway.S6Scandir = gatewaycli.S6Running, func() string { return gatewaycli.S6DynamicScandir }
	gateway.S6IsRunning = func(name string) (bool, error) {
		return gatewaycli.NewS6ServiceManager(gatewaycli.S6DynamicScandir).IsRunning(name)
	}
	gateway.S6Start = func(name string) error {
		return gatewaycli.NewS6ServiceManager(gatewaycli.S6DynamicScandir).Start(name)
	}
	cron.SetupWorkerLogging = logging.SetupHomeLogging
	cron.RegisterWorkerConfigHooks = gateway.RegisterWorkerConfigHooks
	cron.RegisterEstopCheck(func(component string, log *slog.Logger) bool {
		return tools.CheckPaused(context.Background(), component, log)
	})
	selfupdate.LoadCronJobsHook = loadCronJobsForDrainReport
	agent.KanbanBudgetExhaustedHook = kanban.RecordBudgetExhausted
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
	gateway.LoadPrefillMessages = app.LoadPrefillMessages
	gateway.CommandBusyPolicy = func(name string) (bool, bool) {
		return commands.ShouldBypassActiveSession(name), commands.IsInterruptThenDispatch(name)
	}
	slack.SubcommandMap = func(ctx context.Context) map[string]string {
		return commands.SlackSubcommandMap(commands.Default, rawConfigTree(ctx))
	}
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
	tools.TerminalBackendInstallHint = func(backend string) string { return provider.TerminalBackendInstallHint(context.Background(), backend) }
	tools.GitlockRunGit = selfupdate.GitlockRunGit
	tools.AsyncDelegationOwnerProbes.PidExists = kanban.GatewayStatusProbes.PidExists
	tools.AsyncDelegationOwnerProbes.ProcessStartTime = gatewayProcessStartTime
	tools.AsyncDelegationRetirement = provider.BackendRetirement()
	tools.ProfileSecrets = provider.ProfileSecretSource()
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
	provider.InstallPluginManagerHooks(pluginManagerHooks())
	plugins.InstallServices(pluginServices())
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
			return tools.RequestPluginToolApproval(ctx, processToolsEnv(ctx), toolName, message, ruleKey, turnID, toolCallID, sessionID)
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
		BoundSessionKey:          func(ctx context.Context) string { return tools.BoundSessionKey(processToolsEnv(ctx)) },
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
			return gateway.ReloadGatewayPluginsForService(ctx, target, profileHome, 30*time.Second)
		},
		HostRendezvous:              gateway.HostRendezvousService(),
		GatewayRunningPid:           gateway.GatewayRunningPID,
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
	run(tools.RelayAtExit)
	run(lspAtExit)
	run(func() { mcp.ShutdownMCPServers(context.Background(), nil, nil, 15*time.Second) })
	run(func() { provider.ShutdownCachedClients(context.Background()) })
	run(func() { provider.ShutdownLocalRuntime() })
	run(func() { plugins.FlushOutboundWebhooks(5 * time.Second) })
	run(func() { observability.ShutdownRuntimes(context.Background()) })
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
	path, err := gateway.ValidateMediaDeliveryPath(context.Background(), candidate, sessionKey)
	if err != nil {
		panic(err)
	}
	return path
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

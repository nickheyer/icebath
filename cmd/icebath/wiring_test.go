package main

import (
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/codeexec"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/dashboard"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/slack"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/telegram"
	"github.com/nickheyer/icebath/pkg/gatewayrun"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Fails for every nil func field of a hook struct
func requireFuncFields(t *testing.T, name string, v any) {
	rv := reflect.ValueOf(v)
	for i := 0; i < rv.NumField(); i++ {
		f := rv.Field(i)
		if (f.Kind() == reflect.Func || f.Kind() == reflect.Interface) && f.IsNil() {
			t.Errorf("%s.%s is not installed", name, rv.Type().Field(i).Name)
		}
	}
}

// Every hook a lower package leaves for the binary is installed once the process wiring ran
func TestInstallProcessWiringInstallsEveryHook(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("installProcessWiring panicked: %v", r)
		}
	}()
	installProcessWiring()
	hooks := map[string]bool{
		"state.SecretEnvName": state.SecretEnvName != nil, "state.SecretEnvSubstrings": state.SecretEnvSubstrings != nil,
		"state.MessagingPlatformLabels": state.MessagingPlatformLabels != nil, "state.MessagingPlatformSource": state.MessagingPlatformSource != nil,
		"state.SameOwner": state.SameOwner != nil, "state.OwnersAgree": state.OwnersAgree != nil,
		"state.ParseAuthenticatedSubject": state.ParseAuthenticatedSubject != nil, "state.AdmissionMediaReferences": state.AdmissionMediaReferences != nil,
		"state.HostedDocumentReferences": state.HostedDocumentReferences != nil, "state.ReleaseUnheldMedia": state.ReleaseUnheldMedia != nil,
		"state.SpilloverSQLFilter": state.SpilloverSQLFilter != "", "state.SpilloverNamesInText": state.SpilloverNamesInText != nil,
		"state.ReferencedSpilloverNames": state.ReferencedSpilloverNames != nil,
		"cron.SetupWorkerLogging":        cron.SetupWorkerLogging != nil, "cron.RegisterWorkerConfigHooks": cron.RegisterWorkerConfigHooks != nil,
		"selfupdate.LoadCronJobsHook": selfupdate.LoadCronJobsHook != nil, "gateway.CommandBusyPolicy": gateway.CommandBusyPolicy != nil,
		"gateway.DrainProgressReporterHook": gateway.DrainProgressReporterHook != nil,
		"gateway.RosterProfileInfo":         gateway.RosterProfileInfo != nil,
		"gatewayrun.RestoreUserProxyEnv":    gatewayrun.RestoreUserProxyEnv != nil,
		"gatewayrun.BuildGatewayListener":   gatewayrun.BuildGatewayListener != nil,
		"gateway.WebServer.StartServer":     gateway.WebServer.StartServer != nil,
		"gateway.NormalizeRunBudgetSeconds": gateway.NormalizeRunBudgetSeconds != nil,
		"dashboard.RestoreUserProxyEnv":     dashboard.RestoreUserProxyEnv != nil,
		"gatewayrun.CheckRuntime":           gatewayrun.CheckRuntime != nil, "gatewayrun.MaybeRunBootBootstrap": gatewayrun.MaybeRunBootBootstrap != nil,
		"gatewayrun.ConfigureWindowsStdio": gatewayrun.ConfigureWindowsStdio != nil,
		"slack.SubcommandMap":              slack.SubcommandMap != nil, "telegram.InlineCommands": telegram.InlineCommands != nil,
		"codeexec.BotDesktopEnv": codeexec.BotDesktopEnv != nil, "codeexec.BotDesktopToolPlacement": codeexec.BotDesktopToolPlacement != nil,
		"codeexec.BotDesktopPlacementWhere": codeexec.BotDesktopPlacementWhere != nil, "codeexec.BotDesktopPublishedEnv": codeexec.BotDesktopPublishedEnv != nil,
		"codeexec.BotDesktopSandboxEnv": codeexec.BotDesktopSandboxEnv != nil, "codeexec.BotDesktopProfileName": codeexec.BotDesktopProfileName != nil,
		"codeexec.BotDesktopTouchActivity": codeexec.BotDesktopTouchActivity != nil, "codeexec.BotDesktopCuaMCPInvocation": codeexec.BotDesktopCuaMCPInvocation != nil,
		"codeexec.BotDesktopAssertAgentMayAct": codeexec.BotDesktopAssertAgentMayAct != nil, "codeexec.BotDesktopLeaseEpoch": codeexec.BotDesktopLeaseEpoch != nil,
		"codeexec.BotDesktopEnsureStartedForTool": codeexec.BotDesktopEnsureStartedForTool != nil, "codeexec.BotDesktopInstallPackages": codeexec.BotDesktopInstallPackages != nil,
		"agent.ResetScreenshotDedup": agent.ResetScreenshotDedup != nil, "agent.KanbanBudgetExhaustedHook": agent.KanbanBudgetExhaustedHook != nil,
		"agent.TUIGatewayToolsets": agent.TUIGatewayToolsets != nil, "agent.SessionSteerAuthority": agent.SessionSteerAuthority != nil,
		"mcp.TUILaunchHome": mcp.TUILaunchHome != nil, "mcp.TUIProfileHome": mcp.TUIProfileHome != nil, "mcp.TUIProfileRuntimeScope": mcp.TUIProfileRuntimeScope != nil,
		"observability.ResolveSessionPlatformHook": observability.ResolveSessionPlatformHook != nil, "pm.InteractiveAppRunning": pm.InteractiveAppRunning != nil,
		"pm.CuaDriverChildEnv": pm.CuaDriverChildEnv != nil, "pm.LooksLikeGatewayCommandLine": pm.LooksLikeGatewayCommandLine != nil,
		"tools.ApprovalHookInvoker": tools.ApprovalHookInvoker != nil, "tools.ViewerTurnHasApprover": tools.ViewerTurnHasApprover != nil,
		"tools.ActivePolicySource": tools.ActivePolicySource != nil, "tools.LoadCLIConfig": tools.LoadCLIConfig != nil,
		"tools.TerminalOutputTransformHook": tools.TerminalOutputTransformHook != nil, "tools.SupervisedGatewayPid": tools.SupervisedGatewayPid != nil,
		"tools.ModelNotFoundPatterns": tools.ModelNotFoundPatterns != nil, "tools.DelegationFallbackChain": tools.DelegationFallbackChain != nil,
		"tuiserver.SaveClipboardImage": tuiserver.SaveClipboardImage != nil, "tuiserver.HasClipboardImage": tuiserver.HasClipboardImage != nil,
		"observability.ClassifySendErrorHook": observability.ClassifySendErrorHook != nil, "observability.GatewayAdapterPlatformsHook": observability.GatewayAdapterPlatformsHook != nil,
		"observability.PluginPlatformNamesHook": observability.PluginPlatformNamesHook != nil, "observability.CatalogPlatformOwnerHook": observability.CatalogPlatformOwnerHook != nil,
		"observability.ResolveSkillCommandKeyHook": observability.ResolveSkillCommandKeyHook != nil, "observability.PluginCommandHandlerHook": observability.PluginCommandHandlerHook != nil,
		"observability.ProcessStartTimeHook": observability.ProcessStartTimeHook != nil, "observability.RuntimeStatusPidIsLiveHook": observability.RuntimeStatusPidIsLiveHook != nil,
		"tools.TerminalBackendInstallHint": tools.TerminalBackendInstallHint != nil, "tools.SkillLocks": tools.SkillLocks != nil,
		"tools.DelegationRegistryHooks.GetProvider": tools.DelegationRegistryHooks.GetProvider != nil, "tools.DelegationRegistryHooks.LoadBundled": tools.DelegationRegistryHooks.LoadBundled != nil,
		"tools.DelegationBackendHooks.ListProviders": tools.DelegationBackendHooks.ListProviders != nil,
		"tools.ConnectorConversationContext":         tools.ConnectorConversationContext != nil, "tools.TelemetrySendConsented": tools.TelemetrySendConsented != nil,
		"tools.ProfileSecrets": tools.ProfileSecrets != nil, "tools.RuntimeProviderCredentials": tools.RuntimeProviderCredentials != nil,
		"tools.CachedContextLength": tools.CachedContextLength != nil, "tools.ModelContextLength": tools.ModelContextLength != nil,
		"tools.MarkBackgroundReviewSkillRead": tools.MarkBackgroundReviewSkillRead != nil, "tools.DiagnosticProcessEvent": tools.DiagnosticProcessEvent != nil,
		"tools.AsyncDelegationRetirement": tools.AsyncDelegationRetirement != nil, "tools.GitlockRunGit": tools.GitlockRunGit != nil,
		"tools.MCPUnavailableDetails": tools.MCPUnavailableDetails != nil, "tools.OnboardingPluginNames": tools.OnboardingPluginNames != nil,
		"tools.CatalogInstalledDir": tools.CatalogInstalledDir != nil, "tools.CatalogPluginSkill": tools.CatalogPluginSkill != nil,
		"tools.AllSkillNames": tools.AllSkillNames != nil, "tools.MediaDeliveryPathValidator": tools.MediaDeliveryPathValidator != nil,
		"tools.FindSkillDir": tools.FindSkillDir != nil, "tools.ConfigEnvTableKeys": tools.ConfigEnvTableKeys != nil,
		"tools.RegisteredAdapterSecretEnv": tools.RegisteredAdapterSecretEnv != nil, "tools.CleanupBrowser": tools.CleanupBrowser != nil,
		"tools.IsHeadedMode": tools.IsHeadedMode != nil, "tools.ReleaseComputerUseSession": tools.ReleaseComputerUseSession != nil,
		"observability.SkillSourceIDAliasesHook": observability.SkillSourceIDAliasesHook != nil,
		"provider.BrowserHarnessCLI":             provider.BrowserHarnessCLI != nil, "provider.ChromiumExecutable": provider.ChromiumExecutable != nil,
		"provider.FasterWhisperImportable": provider.FasterWhisperImportable != nil, "provider.ManagedImageRoute": provider.ManagedImageRoute != nil,
	}
	for name, ok := range hooks {
		if !ok {
			t.Errorf("%s is not installed", name)
		}
	}
	requireFuncFields(t, "tools.AsyncDelegationOwnerProbes", tools.AsyncDelegationOwnerProbes)
	requireFuncFields(t, "kanban.GatewayStatusProbes", kanban.GatewayStatusProbes)
	requireFuncFields(t, "provider.GatewayHooks", provider.GatewayHooks)
	requireFuncFields(t, "profiles.CronDistributionHooks", profiles.CronDistributionHooks)
	requireFuncFields(t, "profiles.GatewayServiceHooks", profiles.GatewayServiceHooks)
	requireFuncFields(t, "profiles.GatewayStatusHooks", profiles.GatewayStatusHooks)
	requireFuncFields(t, "provider.PluginHooks", *provider.PluginHooks())
	requireFuncFields(t, "plugins.Services", *pluginServices())
}

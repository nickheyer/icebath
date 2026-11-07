package browser

import (
	"slices"

	"github.com/nickheyer/icebath/pkg/botdesktop"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Hands the bot desktop the browser config readers, the packaged Chromium and the launcher sandbox policy
func init() {
	botdesktop.BrowserUserDataDir = func(env *tools.Env) string { return cfgOf(env).userDataDir() }
	botdesktop.BrowserExecutablePath = func(env *tools.Env) string { return cfgOf(env).executablePath() }
	botdesktop.BrowserChromiumExecutable = chromiumExecutable
	botdesktop.BrowserAppArmorRestricts = apparmorRestrictsUnprivilegedUserns
	botdesktop.BrowserNeedsSandboxBypass = needsChromiumSandboxBypass
	botdesktop.BrowserSandboxBypassArgs = func() []string { return slices.Clone(chromiumSandboxBypassArgs) }
}

func botDesktopDesktopEnv(env *tools.Env, base map[string]string) map[string]string {
	return botdesktop.DesktopEnv(env, base)
}

func botDesktopPublishedEnv(env *tools.Env) map[string]string {
	return botdesktop.MustPublishedEnv(env)
}

func botDesktopHumanHolds(env *tools.Env) bool {
	return botdesktop.LeaseHumanHolds(botdesktop.ProfileKey(env))
}

// Epoch the agent was admitted under, the HumanHasControl error while a human holds the screen
func botDesktopAssertAgentMayAct(env *tools.Env) (int, error) {
	l, err := botdesktop.LeaseAssertAgentMayAct(botdesktop.ProfileKey(env))
	if err != nil {
		return 0, err
	}
	return l.EpochInt(), nil
}

func botDesktopLeaseEpoch(env *tools.Env) int {
	return botdesktop.LeaseGet(botdesktop.ProfileKey(env)).EpochInt()
}

func botDesktopEnsureStartedForTool(env *tools.Env) { botdesktop.EnsureStartedForTool(env) }

func botDesktopProfileDir(env *tools.Env) string { return botdesktop.ProfileDir(env) }

func botDesktopExecutable(env *tools.Env) string { return botdesktop.Executable(env) }

func botDesktopSharedProfileArgs() []string { return slices.Clone(botdesktop.SharedProfileArgs) }

func botDesktopToolPlacement(env *tools.Env) (string, error) { return botdesktop.ToolPlacement(env) }

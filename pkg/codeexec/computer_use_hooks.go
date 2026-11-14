package codeexec

import (
	"context"
	"errors"
	"runtime"

	"github.com/nickheyer/icebath/pkg/botdesktop"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Bot desktop placement values of tools/bot_desktop/placement.py
const (
	botDesktopGateway  = "gateway"
	botDesktopTerminal = "terminal"
	botDesktopRefused  = "refused"
)

// Bot desktop runtime calls of the computer use tool, package vars so tests substitute them
var (
	BotDesktopEnv              = botdesktop.DesktopEnv
	BotDesktopToolPlacement    = botdesktop.ToolPlacement
	BotDesktopPlacementWhere   = botDesktopPlacementWhere
	BotDesktopPublishedEnv     = botdesktop.MustPublishedEnv
	BotDesktopSandboxEnv       = botdesktop.TerminalEnvironment
	BotDesktopProfileName      = botdesktop.ProfileName
	BotDesktopTouchActivity    = botdesktop.TouchActivity
	BotDesktopCuaMCPInvocation = func(ctx context.Context, sandbox tools.Environment, profile string, published map[string]string) (string, []string, error) {
		return botdesktop.CuaMCPInvocation(sandbox, profile, published)
	}
	// Admitted lease epoch, an error carrying the HumanHasControl text while a human holds the screen
	BotDesktopAssertAgentMayAct = botDesktopAssertAgentMayAct
	BotDesktopLeaseEpoch        = func(env *tools.Env) int {
		return botdesktop.LeaseGet(botdesktop.ProfileKey(env)).EpochInt()
	}
	BotDesktopEnsureStartedForTool = botdesktop.EnsureStartedForTool
	// Desktop pane package installer of tools/bot_desktop/install.py answering the installer exit code
	BotDesktopInstallPackages = botDesktopInstallPackages
)

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

// Desktop package install with the busy refusal mapped onto the error the screen CLI reads
func botDesktopInstallPackages(env *tools.Env, askPassword func() (string, error), onLine func(string)) (int, error) {
	rc, err := botdesktop.InstallPackages(env, askPassword, onLine)
	var busy *botdesktop.InstallBusy
	if errors.As(err, &busy) {
		return rc, &BotDesktopInstallBusy{Msg: busy.Msg}
	}
	return rc, err
}

// Installer exit code of an unprivileged host without sudo, the root command streamed through onLine
const BotDesktopInstallNoSudo = -2

// BotDesktopInstallBusy is the InstallBusy another holder of the per profile install slot raises
type BotDesktopInstallBusy struct{ Msg string }

func (e *BotDesktopInstallBusy) Error() string { return e.Msg }

// Python sys.platform spelling of the host, swapped by tests that pin a platform
var cuPlatform = func() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	default:
		return runtime.GOOS
	}
}()

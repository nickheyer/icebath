package codeexec

import (
	"context"
	"runtime"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Bot desktop placement values of tools/bot_desktop/placement.py
const (
	botDesktopGateway  = "gateway"
	botDesktopTerminal = "terminal"
	botDesktopRefused  = "refused"
)

// Wiring hooks the process assigns from the pkg/browser bot desktop runtime, nil until wired
var (
	BotDesktopEnv              func(env *tools.Env, base map[string]string) map[string]string
	BotDesktopToolPlacement    func(env *tools.Env) (string, error)
	BotDesktopPlacementWhere   func(env *tools.Env) string
	BotDesktopPublishedEnv     func(env *tools.Env) map[string]string
	BotDesktopSandboxEnv       func(env *tools.Env, create bool) (tools.Environment, error)
	BotDesktopProfileName      func(env *tools.Env) string
	BotDesktopTouchActivity    func(env *tools.Env)
	BotDesktopCuaMCPInvocation func(ctx context.Context, sandbox tools.Environment, profile string, published map[string]string) (string, []string, error)
	// Admitted lease epoch, an error carrying the HumanHasControl text while a human holds the screen
	BotDesktopAssertAgentMayAct    func(env *tools.Env) (int, error)
	BotDesktopLeaseEpoch           func(env *tools.Env) int
	BotDesktopEnsureStartedForTool func(env *tools.Env)
	// Desktop pane package installer of tools/bot_desktop/install.py answering the installer exit code
	BotDesktopInstallPackages func(env *tools.Env, askPassword func() (string, error), onLine func(string)) (int, error)
)

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

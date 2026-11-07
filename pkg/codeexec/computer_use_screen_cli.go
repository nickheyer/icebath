package codeexec

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"

	xterm "github.com/charmbracelet/x/term"

	"github.com/nickheyer/icebath/pkg/botdesktop"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Bot Desktop runtime seams the screen tests swap as hermes monkeypatches the runtime module
var (
	screenStatus          = botdesktop.Status
	screenStart           = botdesktop.Start
	screenStop            = botdesktop.Stop
	screenIsSupportedHost = botdesktop.IsSupportedHost
	screenMissingBinaries = botdesktop.MissingBinaries
	screenInstallCommand  = botdesktop.InstallCommand
	screenGetpass         = provider.Getpass
)

// Field of a dataclass optional rendered the way an f-string prints it, None for a nil pointer
func screenText[T any](p *T) string {
	if p == nil {
		return "None"
	}
	return pyrepr.Str(*p)
}

// Installed, running and control state of this profile screen, as JSON with the redacted lease or as text
func screenStatusCmd(_ context.Context, cli *ComputerUseCLI, ns ComputerUseArgs) (int, error) {
	st, err := screenStatus(cli.Env, "")
	if err != nil {
		return 0, err
	}
	key := botdesktop.ProfileKey(cli.Env)
	if ns.JSON {
		payload := st.AsDict()
		payload.Set("lease", botdesktop.LeasePublicView(botdesktop.LeaseGet(key)))
		text, err := pyjson.Dumps(payload, pyjson.Indent(2), pyjson.SortKeys(true))
		if err != nil {
			return 0, err
		}
		cli.println(text)
		if st.Running {
			return 0, nil
		}
		return 1, nil
	}
	if !st.Supported {
		cli.println("Bot Desktop screens run on Linux gateway hosts only (this host keeps its real display).")
		return 1, nil
	}
	if !st.Installed {
		cli.println("Bot Desktop: packages missing → " + strings.Join(st.Missing, ", "))
		install := "icebath computer-use screen install"
		if st.InstallCommand != nil && *st.InstallCommand != "" {
			install = *st.InstallCommand
		}
		cli.println("  Install: " + install)
		return 1, nil
	}
	if st.Running {
		holder := botdesktop.LeasePublicView(botdesktop.LeaseGet(key))
		who := "agent"
		if pyrepr.Str(cuGet(holder, "holder")) == botdesktop.LeaseHuman {
			who = "human (viewer " + pyrepr.Str(cuGet(holder, "viewer_hash")) + ")"
		}
		cli.printf("Bot Desktop [%s]: running on DISPLAY %s (%s), pid %s\n", st.Profile, screenText(st.Display), st.Geometry, screenText(st.PID))
		cli.printf("  control: %s   rfb socket: %s\n", who, screenText(st.Socket))
		cli.println("  View it: Icebath Desktop → Bots → this bot → Screen")
		return 0, nil
	}
	if st.Blocker != nil && *st.Blocker != "" {
		cli.printf("Bot Desktop [%s]: installed, not running. %s\n", st.Profile, *st.Blocker)
		return 1, nil
	}
	cli.printf("Bot Desktop [%s]: installed, not running. Start: icebath computer-use screen start\n", st.Profile)
	return 1, nil
}

// Starts this profile screen, a launch failure printed with exit 1
func screenStartCmd(_ context.Context, cli *ComputerUseCLI, _ ComputerUseArgs) (int, error) {
	st, err := screenStart(cli.Env, 15.0)
	if err != nil {
		cli.println("Bot Desktop: " + err.Error())
		return 1, nil
	}
	cli.printf("Bot Desktop [%s]: running on DISPLAY %s (%s)\n", st.Profile, screenText(st.Display), st.Geometry)
	return 0, nil
}

// Stops this profile screen after handing control back, refused while a human holds it unless forced
func screenStopCmd(_ context.Context, cli *ComputerUseCLI, ns ComputerUseArgs) (int, error) {
	if screenIsSupportedHost() {
		l, err := botdesktop.LeaseRelease(botdesktop.LeaseReleaseOptions{ProfileKey: botdesktop.ProfileKey(cli.Env), UnlessHuman: !ns.Force})
		if err != nil {
			return 0, err
		}
		if l.Holder == botdesktop.LeaseHuman {
			cli.println("Bot Desktop: a human holds this screen; re-run with --force to take it down anyway.")
			return 1, nil
		}
	}
	stopped, err := screenStop(cli.Env, nil)
	if err != nil {
		return 0, err
	}
	if stopped {
		cli.println("Bot Desktop: stopped")
	} else {
		cli.println("Bot Desktop: was not running")
	}
	return 0, nil
}

// Whether the console input stream is an interactive terminal
func consoleStdinTTY(con *cliui.Console) bool {
	if con.StdinTTY != nil {
		return con.StdinTTY()
	}
	f, ok := con.In.(*os.File)
	return ok && xterm.IsTerminal(f.Fd())
}

// Installs the screen packages through the Desktop pane installer with the sudo password on stdin
func screenInstallCmd(_ context.Context, cli *ComputerUseCLI, ns ComputerUseArgs) (int, error) {
	if !screenIsSupportedHost() {
		cli.println("Bot Desktop screens run on Linux gateway hosts only.")
		return 1, nil
	}
	if len(screenMissingBinaries()) == 0 {
		cli.println("Bot Desktop: packages already installed.")
		return 0, nil
	}
	cmd := screenInstallCommand()
	if cmd == "" {
		cli.println("Bot Desktop: no supported package manager (apt/dnf/pacman) found. Install TigerVNC (Xvnc) and " +
			"the Xfce core (xfwm4, xfce4-panel, xfdesktop, xfce4-settings) by hand.")
		return 1, nil
	}
	cli.println("Bot Desktop: installing → " + cmd)
	if !ns.Yes && consoleStdinTTY(cli.Console) {
		answer, err := cli.Console.LineInput("Proceed? [Y/n] ")
		if err != nil {
			return 0, err
		}
		switch strings.ToLower(pystr.Strip(answer)) {
		case "", "y", "yes":
		default:
			return 1, nil
		}
	}
	rc, err := BotDesktopInstallPackages(cli.Env, func() (string, error) { return screenGetpass("[sudo] password: ") }, cli.println)
	if err != nil {
		var busy *BotDesktopInstallBusy
		if errors.As(err, &busy) {
			cli.println("Bot Desktop: " + busy.Error())
			return 1, nil
		}
		return 0, err
	}
	if rc == BotDesktopInstallNoSudo {
		cli.println("Bot Desktop: this host has no sudo. Run as root on the host:\n  " + strings.TrimPrefix(cmd, "sudo "))
		return 1, nil
	}
	if rc != 0 {
		cli.println("Bot Desktop: installer exited " + strconv.Itoa(rc))
		return rc, nil
	}
	if missing := screenMissingBinaries(); len(missing) > 0 {
		cli.println("Bot Desktop: still missing " + strings.Join(missing, ", "))
		return 1, nil
	}
	cli.println("Bot Desktop: ready. Start with `icebath computer-use screen start` or from Icebath Desktop.")
	return 0, nil
}

// Screen actions by name as SCREEN_ACTIONS
var screenActions = map[string]ComputerUseCommand{
	"status": screenStatusCmd, "start": screenStartCmd, "stop": screenStopCmd, "install": screenInstallCmd,
}

// Screen subcommand dispatcher, help for a missing action
func buildScreenParser() ComputerUseCommand {
	return func(ctx context.Context, cli *ComputerUseCLI, ns ComputerUseArgs) (int, error) {
		if h, ok := screenActions[ns.ScreenAction]; ok {
			return h(ctx, cli, ns)
		}
		return 0, cli.PrintHelp("computer-use screen")
	}
}

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	botdesktopv1 "github.com/nickheyer/icebath/pkg/gen/icebath/botdesktop/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/botdesktop/v1/botdesktopv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
)

// The screen commands: the agent desktop of the profile, the headless screen its computer_use drives on Linux
func screen(service botdesktopv1connect.DesktopServiceClient, profile *string) *cobra.Command {
	root := &cobra.Command{
		Use:   "screen",
		Short: "Start, stop and check the agent desktop",
		Long: "The agent desktop is a hidden desktop on Linux that the agent uses a computer on. Each profile has its own. You can take it " +
			"over in the app with /desktop.",
		Args: cobra.NoArgs,
		RunE: help,
	}
	var asJSON bool
	status := &cobra.Command{
		Use:   "status",
		Short: "Show whether the agent desktop runs and who controls it",
		Long: "Shows whether the agent desktop of this profile is installed and running, and whether the agent or a person controls it. The " +
			"exit status is 1 while it is not running.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := service.Status(cmd.Context(), &botdesktopv1.StatusRequest{Profile: *profile})
			if err != nil {
				return err
			}
			return report(cli.NewConsole(cmd), st, asJSON)
		},
	}
	status.Flags().BoolVar(&asJSON, "json", false, "Print the result as JSON")
	start := &cobra.Command{
		Use:   "start",
		Short: "Start the agent desktop",
		Long:  "Starts the agent desktop of this profile.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			st, err := service.Start(cmd.Context(), &botdesktopv1.StartRequest{Profile: *profile})
			if err != nil {
				c.Failure("The agent desktop did not start. " + sentence(tui.Reason(err)))
				return cli.Exit(1)
			}
			c.Success(running(st) + ".")
			return nil
		},
	}
	var force bool
	stop := &cobra.Command{
		Use:   "stop",
		Short: "Stop the agent desktop",
		Long:  "Stops the agent desktop of this profile. When a person controls it, stop refuses unless you pass --force.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			reply, err := service.Stop(cmd.Context(), &botdesktopv1.StopRequest{Profile: *profile, Force: force})
			if connect.CodeOf(err) == connect.CodePermissionDenied {
				c.Failure("A person is using the agent desktop, so it was not stopped. Run icebath computer-use screen stop --force to stop it anyway.")
				return cli.Exit(1)
			}
			if err != nil {
				return err
			}
			if reply.GetStopped() {
				c.Success("Stopped the agent desktop.")
			} else {
				c.Say("The agent desktop was not running.")
			}
			return nil
		},
	}
	stop.Flags().BoolVar(&force, "force", false, "Stop the agent desktop even while a person controls it")
	var yes bool
	install := &cobra.Command{
		Use:   "install",
		Short: "Install the programs the agent desktop needs",
		Long:  "Installs TigerVNC and the core of the Xfce desktop with the package manager of this computer, after asking.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return installScreen(cmd, service, *profile, yes)
		},
	}
	install.Flags().BoolVarP(&yes, "yes", "y", false, "Install without asking first")
	root.AddCommand(status, start, stop, install)
	return root
}

// What runs: the profile's agent desktop, its display and its screen size, as a sentence without its period
func running(st *botdesktopv1.Screen) string {
	text := "The agent desktop of the profile " + st.GetProfile() + " is running"
	if display := st.GetDisplay(); display != "" {
		text += " on display " + display
	}
	if geometry := st.GetGeometry(); geometry != "" {
		text += " with a screen of " + geometry
	}
	return text
}

// Reason a server gave as a sentence: capital first letter, a period at the end
func sentence(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	text = strings.ToUpper(text[:1]) + text[1:]
	if !strings.ContainsAny(text[len(text)-1:], ".!?") {
		text += "."
	}
	return text
}

// Prints the screen's status as JSON with its lease, or as lines; exit 1 while it is not running
func report(c *cli.Console, st *botdesktopv1.Screen, asJSON bool) error {
	if asJSON {
		l := st.GetLease()
		payload := map[string]any{"profile": st.GetProfile(), "supported": st.GetSupported(), "installed": st.GetInstalled(),
			"missing": append([]string{}, st.GetMissing()...), "running": st.GetRunning(), "pid": nil, "display": strs.Nullable(st.GetDisplay()),
			"socket": strs.Nullable(st.GetSocket()), "geometry": st.GetGeometry(), "install_command": strs.Nullable(st.GetInstallCommand()),
			"browser": strs.Nullable(st.GetBrowser()), "blocker": strs.Nullable(st.GetBlocker()), "memory_available_mb": nil, "memory_limit_mb": nil,
			"placement": st.GetPlacement(), "image_switch": nil,
			"lease": map[string]any{"holder": l.GetHolder(), "viewer_id": nil, "viewer_hash": strs.Nullable(l.GetViewerHash()), "since": l.GetSince(),
				"reason": l.GetReason(), "epoch": l.GetEpoch()}}
		if st.GetPid() != 0 {
			payload["pid"] = st.GetPid()
		}
		if st.GetMemoryAvailableMb() >= 0 && st.GetSupported() {
			payload["memory_available_mb"] = st.GetMemoryAvailableMb()
		}
		if st.GetMemoryLimitMb() >= 0 && st.GetSupported() {
			payload["memory_limit_mb"] = st.GetMemoryLimitMb()
		}
		if sw := st.GetImageSwitch(); sw != nil {
			payload["image_switch"] = map[string]any{"current_image": sw.GetCurrentImage(), "target_image": sw.GetTargetImage(),
				"containers": sw.GetContainers()}
		}
		text, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(c.Out(), string(text))
		if st.GetRunning() {
			return nil
		}
		return cli.Exit(1)
	}
	switch {
	case !st.GetSupported():
		c.Say("The agent desktop runs only on Linux. This computer uses its own screen instead.")
	case !st.GetInstalled():
		c.Warning("The agent desktop of the profile " + st.GetProfile() + " is not installed. It still needs " + strings.Join(st.GetMissing(), ", ") + ".")
		if command := st.GetInstallCommand(); command != "" {
			c.Say("  To install it, run: " + command)
		} else {
			c.Say("  To install it, run: icebath computer-use screen install")
		}
	case st.GetRunning():
		c.Success(running(st) + ".")
		rows := [][2]string{{"Controlled by", "The agent"}}
		if st.GetLease().GetHolder() == "human" {
			rows[0][1] = "A person"
			if hash := st.GetLease().GetViewerHash(); hash != "" {
				rows[0][1] += ", viewer " + hash
			}
		}
		if pid := st.GetPid(); pid != 0 {
			rows = append(rows, [2]string{"Process ID", strconv.Itoa(int(pid))})
		}
		if socket := st.GetSocket(); socket != "" {
			rows = append(rows, [2]string{"VNC socket", socket})
		}
		for _, row := range rows {
			c.Say("  " + row[0] + ": " + row[1])
		}
		c.Say("  Watch it with /desktop in the app, or open its VNC socket in a VNC viewer.")
		return nil
	case st.GetBlocker() != "":
		c.Say("The agent desktop of the profile " + st.GetProfile() + " is installed but not running. " + sentence(st.GetBlocker()))
	default:
		c.Say("The agent desktop of the profile " + st.GetProfile() + " is installed but not running. Start it with icebath computer-use screen start.")
	}
	return cli.Exit(1)
}

// Installs the screen's packages on the server's machine, asking first on a terminal and asking the sudo password there when it needs one
func installScreen(cmd *cobra.Command, service botdesktopv1connect.DesktopServiceClient, profile string, yes bool) error {
	console := cli.NewConsole(cmd)
	st, err := service.Status(cmd.Context(), &botdesktopv1.StatusRequest{Profile: profile})
	if err != nil {
		return err
	}
	switch {
	case !st.GetSupported():
		console.Failure("The agent desktop runs only on Linux, so there is nothing to install here.")
		return cli.Exit(1)
	case len(st.GetMissing()) == 0:
		console.Success("The programs the agent desktop needs are already installed.")
		return nil
	case st.GetInstallCommand() == "":
		console.Failure("No package manager icebath knows was found, such as apt, dnf or pacman. Install TigerVNC and the Xfce programs xfwm4, " +
			"xfce4-panel, xfdesktop and xfce4-settings yourself.")
		return cli.Exit(1)
	}
	command := st.GetInstallCommand()
	console.Say("This command installs the programs the agent desktop needs:")
	console.Say("  " + command)
	if !yes && console.Interactive() && !console.Confirm("Run it now? It installs "+strings.Join(st.GetMissing(), ", ")+" on this computer.", true) {
		return cli.Exit(1)
	}
	stream, err := service.Install(cmd.Context(), &botdesktopv1.InstallRequest{Profile: profile})
	if err != nil {
		console.Failure("The install did not start. " + sentence(tui.Reason(err)))
		return cli.Exit(1)
	}
	defer stream.Close()
	for {
		event, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return errors.New("The install ended without saying whether it worked.")
		}
		if err != nil {
			if connect.CodeOf(err) == connect.CodeFailedPrecondition {
				console.Failure(sentence(tui.Reason(err)))
				return cli.Exit(1)
			}
			return err
		}
		switch kind := event.GetKind().(type) {
		case *botdesktopv1.InstallEvent_Line:
			console.Say(kind.Line)
		case *botdesktopv1.InstallEvent_Password:
			password, _ := console.Secret("Password for sudo: ")
			if _, err := service.Password(cmd.Context(), &botdesktopv1.PasswordRequest{Id: kind.Password, Password: password}); err != nil {
				return err
			}
		case *botdesktopv1.InstallEvent_Done:
			code := kind.Done.GetCode()
			switch {
			case code == -2:
				console.Failure("This computer has no sudo. Run this command as root instead:")
				console.Say("  " + strings.TrimPrefix(command, "sudo "))
				return cli.Exit(1)
			case code != 0:
				console.Failure(fmt.Sprintf("The install failed with exit code %d. The lines above say why.", code))
				return cli.Exit(int(code))
			case len(kind.Done.GetStatus().GetMissing()) > 0:
				console.Failure("The install finished, but the agent desktop still needs " + strings.Join(kind.Done.GetStatus().GetMissing(), ", ") + ".")
				return cli.Exit(1)
			}
			console.Success("The agent desktop is ready. Start it with icebath computer-use screen start, or with /desktop start in the app.")
			return nil
		}
	}
}

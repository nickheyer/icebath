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

// The screen commands: the Bot Desktop of the profile, the headless screen its computer_use drives on Linux
func screen(service botdesktopv1connect.DesktopServiceClient, profile *string) *cobra.Command {
	root := &cobra.Command{
		Use:   "screen",
		Short: "Bot Desktop: the headless screen this profile's computer_use drives (Linux)",
		Args:  cobra.NoArgs,
		RunE:  help,
	}
	var asJSON bool
	status := &cobra.Command{
		Use:   "status",
		Short: "Show whether this profile's screen is installed/running and who holds control",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := service.Status(cmd.Context(), &botdesktopv1.StatusRequest{Profile: *profile})
			if err != nil {
				return err
			}
			return report(cmd.OutOrStdout(), st, asJSON)
		},
	}
	status.Flags().BoolVar(&asJSON, "json", false, "Emit machine-readable JSON")
	start := &cobra.Command{
		Use:   "start",
		Short: "Start this profile's screen",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := service.Start(cmd.Context(), &botdesktopv1.StartRequest{Profile: *profile})
			if err != nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Bot Desktop: "+tui.Reason(err))
				return cli.Exit(1)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Bot Desktop [%s]: running on DISPLAY %s (%s)\n", st.GetProfile(), known(st.GetDisplay()), st.GetGeometry())
			return nil
		},
	}
	var force bool
	stop := &cobra.Command{
		Use:   "stop",
		Short: "Stop this profile's screen (hands control back to the agent first)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reply, err := service.Stop(cmd.Context(), &botdesktopv1.StopRequest{Profile: *profile, Force: force})
			if connect.CodeOf(err) == connect.CodePermissionDenied {
				fmt.Fprintln(cmd.OutOrStdout(), "Bot Desktop: a human holds this screen; re-run with --force to take it down anyway.")
				return cli.Exit(1)
			}
			if err != nil {
				return err
			}
			if reply.GetStopped() {
				fmt.Fprintln(cmd.OutOrStdout(), "Bot Desktop: stopped")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "Bot Desktop: was not running")
			}
			return nil
		},
	}
	stop.Flags().BoolVar(&force, "force", false, "Stop it even while a human holds control")
	var yes bool
	install := &cobra.Command{
		Use:   "install",
		Short: "Install TigerVNC + Xfce core via the host package manager",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return installScreen(cmd, service, *profile, yes)
		},
	}
	install.Flags().BoolVarP(&yes, "yes", "y", false, "Install without asking first")
	root.AddCommand(status, start, stop, install)
	return root
}

func known(text string) string {
	if text == "" {
		return "unknown"
	}
	return text
}

// Prints the screen's status as JSON with its lease, or as lines; exit 1 while it is not running
func report(out io.Writer, st *botdesktopv1.Screen, asJSON bool) error {
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
		fmt.Fprintln(out, string(text))
		if st.GetRunning() {
			return nil
		}
		return cli.Exit(1)
	}
	switch {
	case !st.GetSupported():
		fmt.Fprintln(out, "Bot Desktop screens run on Linux gateway hosts only (this host keeps its real display).")
	case !st.GetInstalled():
		fmt.Fprintln(out, "Bot Desktop: packages missing → "+strings.Join(st.GetMissing(), ", "))
		command := st.GetInstallCommand()
		if command == "" {
			command = "icebath computer-use screen install"
		}
		fmt.Fprintln(out, "  Install: "+command)
	case st.GetRunning():
		who := "agent"
		if st.GetLease().GetHolder() == "human" {
			who = "human"
			if hash := st.GetLease().GetViewerHash(); hash != "" {
				who += " (viewer " + hash + ")"
			}
		}
		pid := "unknown"
		if st.GetPid() != 0 {
			pid = strconv.Itoa(int(st.GetPid()))
		}
		fmt.Fprintf(out, "Bot Desktop [%s]: running on DISPLAY %s (%s), pid %s\n", st.GetProfile(), known(st.GetDisplay()), st.GetGeometry(), pid)
		fmt.Fprintf(out, "  control: %s   rfb socket: %s\n", who, known(st.GetSocket()))
		fmt.Fprintln(out, "  View it: /screen in the app, or a VNC viewer on the rfb socket")
		return nil
	case st.GetBlocker() != "":
		fmt.Fprintf(out, "Bot Desktop [%s]: installed, not running. %s\n", st.GetProfile(), st.GetBlocker())
	default:
		fmt.Fprintf(out, "Bot Desktop [%s]: installed, not running. Start: icebath computer-use screen start\n", st.GetProfile())
	}
	return cli.Exit(1)
}

// Installs the screen's packages on the server's machine, asking first on a terminal and asking the sudo password there when it needs one
func installScreen(cmd *cobra.Command, service botdesktopv1connect.DesktopServiceClient, profile string, yes bool) error {
	out := cmd.OutOrStdout()
	console := cli.NewConsole(cmd)
	st, err := service.Status(cmd.Context(), &botdesktopv1.StatusRequest{Profile: profile})
	if err != nil {
		return err
	}
	switch {
	case !st.GetSupported():
		fmt.Fprintln(out, "Bot Desktop screens run on Linux gateway hosts only.")
		return cli.Exit(1)
	case len(st.GetMissing()) == 0:
		fmt.Fprintln(out, "Bot Desktop: packages already installed.")
		return nil
	case st.GetInstallCommand() == "":
		fmt.Fprintln(out, "Bot Desktop: no supported package manager (apt/dnf/pacman) found. Install TigerVNC (Xvnc) and the Xfce core (xfwm4, "+
			"xfce4-panel, xfdesktop, xfce4-settings) by hand.")
		return cli.Exit(1)
	}
	command := st.GetInstallCommand()
	fmt.Fprintln(out, "Bot Desktop: installing → "+command)
	if !yes && console.Interactive() && !console.Confirm("Proceed?", true) {
		return cli.Exit(1)
	}
	stream, err := service.Install(cmd.Context(), &botdesktopv1.InstallRequest{Profile: profile})
	if err != nil {
		fmt.Fprintln(out, "Bot Desktop: "+tui.Reason(err))
		return cli.Exit(1)
	}
	defer stream.Close()
	for {
		event, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return errors.New("the server ended the install without saying how it went")
		}
		if err != nil {
			if connect.CodeOf(err) == connect.CodeFailedPrecondition {
				fmt.Fprintln(out, "Bot Desktop: "+tui.Reason(err))
				return cli.Exit(1)
			}
			return err
		}
		switch kind := event.GetKind().(type) {
		case *botdesktopv1.InstallEvent_Line:
			fmt.Fprintln(out, kind.Line)
		case *botdesktopv1.InstallEvent_Password:
			password, _ := console.Secret("[sudo] password: ")
			if _, err := service.Password(cmd.Context(), &botdesktopv1.PasswordRequest{Id: kind.Password, Password: password}); err != nil {
				return err
			}
		case *botdesktopv1.InstallEvent_Done:
			code := kind.Done.GetCode()
			switch {
			case code == -2:
				fmt.Fprintln(out, "Bot Desktop: this host has no sudo. Run as root on the host:\n  "+strings.TrimPrefix(command, "sudo "))
				return cli.Exit(1)
			case code != 0:
				fmt.Fprintf(out, "Bot Desktop: installer exited %d\n", code)
				return cli.Exit(int(code))
			case len(kind.Done.GetStatus().GetMissing()) > 0:
				fmt.Fprintln(out, "Bot Desktop: still missing "+strings.Join(kind.Done.GetStatus().GetMissing(), ", "))
				return cli.Exit(1)
			}
			fmt.Fprintln(out, "Bot Desktop: ready. Start with `icebath computer-use screen start` or with /screen start in the app.")
			return nil
		}
	}
}

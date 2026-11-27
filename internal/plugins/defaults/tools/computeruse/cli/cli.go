// The computer-use commands: install, check and doctor the cua-driver of the machine the server runs on, and its macOS grants
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"connectrpc.com/connect/v2"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/pkg/gen/icebath/botdesktop/v1/botdesktopv1connect"
	computerusev1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/computeruse/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/computeruse/v1/computerusev1connect"
)

type commands struct{}

func Register(registry *cli.Registry) {
	registry.Register(commands{})
}

// Events of a command's stream
type stream interface {
	Receive() (*computerusev1.Event, error)
	Close() error
}

// Prints what the command streams as it comes, ending with its exit status
func follow(cmd *cobra.Command, s stream, err error) error {
	if err != nil {
		return err
	}
	defer s.Close()
	for {
		event, err := s.Receive()
		if errors.Is(err, io.EOF) {
			return errors.New("the server ended the command without saying how it went")
		}
		if err != nil {
			return err
		}
		switch kind := event.GetKind().(type) {
		case *computerusev1.Event_Out:
			fmt.Fprint(cmd.OutOrStdout(), kind.Out)
		case *computerusev1.Event_Err:
			fmt.Fprint(cmd.ErrOrStderr(), kind.Err)
		case *computerusev1.Event_ExitCode:
			if kind.ExitCode != 0 {
				return cli.Exit(kind.ExitCode)
			}
			return nil
		}
	}
}

// Whether what the command prints goes to a terminal, so its lines are painted
func painted(cmd *cobra.Command) bool {
	f, ok := cmd.OutOrStdout().(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

func help(cmd *cobra.Command, _ []string) error { return cmd.Help() }

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := computerusev1connect.NewComputerUseServiceClient(conn)
	var profile string
	root := &cobra.Command{
		Use:   "computer-use",
		Short: "Manage the Computer Use (cua-driver) backend (macOS/Windows/Linux)",
		Args:  cobra.NoArgs,
		RunE:  help,
	}
	root.PersistentFlags().StringVar(&profile, "profile", "", "Profile whose settings to read, the one in use when empty")
	var upgrade bool
	install := &cobra.Command{
		Use:   "install",
		Short: "Install or repair the cua-driver binary (macOS/Windows/Linux)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := service.Install(cmd.Context(), &computerusev1.InstallRequest{Profile: profile, Color: painted(cmd)})
			return follow(cmd, s, err)
		},
	}
	install.Flags().BoolVar(&upgrade, "upgrade", false, "Bring the driver to the pinned version; an install always does")
	status := &cobra.Command{
		Use:   "status",
		Short: "Check the selected cua-driver and its runtime contract",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := service.Status(cmd.Context(), &computerusev1.StatusRequest{Profile: profile})
			return follow(cmd, s, err)
		},
	}
	var include, skip []string
	var doctorJSON bool
	doctor := &cobra.Command{
		Use:   "doctor",
		Short: "Run cua-driver `health_report` and surface the check matrix",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := service.Doctor(cmd.Context(), &computerusev1.DoctorRequest{Profile: profile, Include: include, Skip: skip, Json: doctorJSON,
				Color: painted(cmd)})
			return follow(cmd, s, err)
		},
	}
	doctor.Flags().StringArrayVar(&include, "include", nil, "Only run this health_report check (repeatable)")
	doctor.Flags().StringArrayVar(&skip, "skip", nil, "Skip this health_report check (repeatable)")
	doctor.Flags().BoolVar(&doctorJSON, "json", false, "Emit the raw report as JSON")
	permissions := &cobra.Command{
		Use:   "permissions",
		Short: "Check or grant macOS Accessibility + Screen Recording (macOS)",
		Args:  cobra.NoArgs,
		RunE:  help,
	}
	var permissionsJSON bool
	permissionsStatus := &cobra.Command{
		Use:   "status",
		Short: "Report Accessibility + Screen Recording grant state (read-only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := service.Permissions(cmd.Context(), &computerusev1.PermissionsRequest{Profile: profile, Json: permissionsJSON})
			return follow(cmd, s, err)
		},
	}
	permissionsStatus.Flags().BoolVar(&permissionsJSON, "json", false, "Emit machine-readable JSON")
	grant := &cobra.Command{
		Use:   "grant",
		Short: "Request the grants (opens the dialog attributed to CuaDriver)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := service.Grant(cmd.Context(), &computerusev1.GrantRequest{Profile: profile})
			return follow(cmd, s, err)
		},
	}
	permissions.AddCommand(permissionsStatus, grant)
	root.AddCommand(install, status, doctor, permissions, screen(botdesktopv1connect.NewDesktopServiceClient(conn), &profile))
	return []*cobra.Command{root}
}

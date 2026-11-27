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
			return errors.New("The command ended without saying whether it worked.")
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
		Use:     "computer-use",
		GroupID: cli.GroupAbilities,
		Short:   "Set up the program that lets the agent use a computer",
		Long: "Computer use lets the agent see a screen and use the mouse and keyboard. Computer use runs through cua-driver, a program on the computer " +
			"where icebath runs. Use these commands to install and check cua-driver, grant it the permissions macOS asks for and run the " +
			"agent desktop.",
		Args: cobra.NoArgs,
		RunE: help,
	}
	root.PersistentFlags().StringVar(&profile, "profile", "", "icebath profile whose settings to read, instead of the one in use")
	var upgrade bool
	install := &cobra.Command{
		Use:   "install",
		Short: "Install or repair cua-driver",
		Long:  "Installs the version of cua-driver that icebath pins, or repairs the copy you have, on macOS, Windows or Linux.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := service.Install(cmd.Context(), &computerusev1.InstallRequest{Profile: profile, Color: painted(cmd)})
			return follow(cmd, s, err)
		},
	}
	install.Flags().BoolVar(&upgrade, "upgrade", false, "Bring cua-driver to the pinned version, as install always does")
	status := &cobra.Command{
		Use:   "status",
		Short: "Check the cua-driver that icebath uses",
		Long:  "Checks the cua-driver that icebath picked and whether it works the way icebath needs.",
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
		Short: "Run the health checks of cua-driver",
		Long:  "Runs the health report of cua-driver and shows the result of each check. --include and --skip pick checks by name.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := service.Doctor(cmd.Context(), &computerusev1.DoctorRequest{Profile: profile, Include: include, Skip: skip, Json: doctorJSON,
				Color: painted(cmd)})
			return follow(cmd, s, err)
		},
	}
	doctor.Flags().StringArrayVar(&include, "include", nil, "Health check to run, leaving out the others, repeatable")
	doctor.Flags().StringArrayVar(&skip, "skip", nil, "Health check to leave out, repeatable")
	doctor.Flags().BoolVar(&doctorJSON, "json", false, "Print the report as JSON")
	permissions := &cobra.Command{
		Use:   "permissions",
		Short: "Check or grant the macOS permissions computer use needs",
		Long:  "On macOS, computer use needs the Accessibility and Screen Recording permissions. Check them with status and ask for them with grant.",
		Args:  cobra.NoArgs,
		RunE:  help,
	}
	var permissionsJSON bool
	permissionsStatus := &cobra.Command{
		Use:   "status",
		Short: "Show whether the macOS permissions are granted",
		Long:  "Shows whether cua-driver has the Accessibility and Screen Recording permissions, without changing anything.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := service.Permissions(cmd.Context(), &computerusev1.PermissionsRequest{Profile: profile, Json: permissionsJSON})
			return follow(cmd, s, err)
		},
	}
	permissionsStatus.Flags().BoolVar(&permissionsJSON, "json", false, "Print the result as JSON")
	grant := &cobra.Command{
		Use:   "grant",
		Short: "Ask macOS for the permissions",
		Long:  "Opens the macOS dialogs that grant Accessibility and Screen Recording to CuaDriver.",
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

// The pm commands: install, inspect, repair and update the packages icebath keeps on the machine its server runs on
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/packages"
	picked "github.com/nickheyer/icebath/internal/profile"
	packagesv1 "github.com/nickheyer/icebath/pkg/gen/icebath/packages/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/packages/v1/packagesv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Exit status a command ends with, its lines already printed
type exit int

func (e exit) Error() string { return "" }

func (e exit) ExitCode() int { return int(e) }

// Verbs pm lists, in its usage
var verbs = []string{"lock", "install", "env", "doctor", "repair", "environment", "gc", "status", "update"}

type commands struct{}

func Register(host *cli.CLI) {
	host.Register(commands{})
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := packagesv1connect.NewPackageServiceClient(conn)
	root := &cobra.Command{
		Use:     "pm",
		GroupID: cli.GroupApp,
		Short:   "Install and repair the programs icebath downloads",
		Long: "icebath downloads some programs it needs, such as Python, Node and Chromium, and keeps them in one place. Use these commands to " +
			"install them ahead of time, check them, repair them or move them to newer versions.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return listed(cmd, cmd.CommandPath()+" has no command “"+args[0]+"”. Its commands are:")
			}
			return listed(cmd, cmd.CommandPath()+" needs one of these commands:")
		},
	}
	root.AddCommand(lock(service), install(service), env(service), doctor(service), repair(service), environment(service), gc(service), status(service),
		update(service))
	return []*cobra.Command{root}
}

// Refusal of a command line on standard error, status 2: the sentence, then the words of the command as its Use names them
func usageError(cmd *cobra.Command, message string) error {
	line := cmd.CommandPath()
	if _, rest, ok := strings.Cut(cmd.Use, " "); ok {
		line += " " + rest
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "%s\nUsage: %s\n", message, line)
	return exit(2)
}

// Refusal of pm without a command it has, on standard error, status 2: the sentence, then each command in the order pm lists them with
// what it does
func listed(cmd *cobra.Command, message string) error {
	out := cmd.ErrOrStderr()
	fmt.Fprintln(out, message)
	width := 0
	for _, verb := range verbs {
		width = max(width, len(verb))
	}
	for _, verb := range verbs {
		if sub, _, err := cmd.Find([]string{verb}); err == nil && sub != cmd {
			fmt.Fprintf(out, "  %-*s  %s\n", width, verb, sub.Short)
		}
	}
	fmt.Fprintf(out, "Run %s <command> --help to read about one.\n", cmd.CommandPath())
	return exit(2)
}

// Output of one command: lines on a stream, statuses redrawn in place when it is a terminal
type screen struct {
	out   io.Writer
	live  bool
	width int
	shown bool
}

func newScreen(w io.Writer) *screen {
	s := &screen{out: w}
	if f, ok := w.(*os.File); ok && term.IsTerminal(f.Fd()) {
		s.live = true
		if width, _, err := term.GetSize(f.Fd()); err == nil {
			s.width = width
		} else {
			s.width = 80
		}
	}
	return s
}

func (s *screen) terminal() *packagesv1.Terminal {
	return &packagesv1.Terminal{Live: s.live, Width: int32(s.width)}
}

func (s *screen) clear() {
	if s.shown {
		fmt.Fprint(s.out, "\r\x1b[2K")
		s.shown = false
	}
}

func (s *screen) line(text string) {
	s.clear()
	fmt.Fprintln(s.out, text)
}

func (s *screen) status(text string) {
	if !s.live {
		return
	}
	fmt.Fprint(s.out, "\r\x1b[2K"+text)
	s.shown = text != ""
}

// Events of a command's stream shown until it is done, ending with its status; failures it did not report go to standard error
func follow(cmd *cobra.Command, s *screen, receive func() (*packagesv1.Event, error)) (string, error) {
	for {
		event, err := receive()
		if errors.Is(err, io.EOF) {
			s.clear()
			return "", errors.New("The server ended the command without saying how it went.")
		}
		if err != nil {
			s.clear()
			return "", err
		}
		switch kind := event.GetKind().(type) {
		case *packagesv1.Event_Line:
			s.line(kind.Line)
		case *packagesv1.Event_Status:
			s.status(kind.Status)
		case *packagesv1.Event_Done:
			s.clear()
			done := kind.Done
			if done.GetError() != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), done.GetError())
			}
			if done.GetExitCode() != 0 {
				return done.GetResult(), exit(done.GetExitCode())
			}
			return done.GetResult(), nil
		}
	}
}

// Runs a streamed command with its output on standard output
func streamed[S interface {
	Receive() (*packagesv1.Event, error)
	Close() error
}](cmd *cobra.Command, start func(t *packagesv1.Terminal) (S, error)) error {
	s := newScreen(cmd.OutOrStdout())
	stream, err := start(s.terminal())
	if err != nil {
		return err
	}
	defer stream.Close()
	_, err = follow(cmd, s, stream.Receive)
	return err
}

func lock(service packagesv1connect.PackageServiceClient) *cobra.Command {
	var bump string
	cmd := &cobra.Command{
		Use:   "lock [--bump NAME] [VERSION]",
		Short: "Pin the versions of the downloaded programs again",
		Long: "With no arguments, lock works out the packages of every Python extra again, for every platform, and saves them in the lock file. " +
			"An extra is a named set of Python packages. Nothing is written when the lock file is up to date. With --bump NAME VERSION, lock " +
			"pins that version of one program for every platform instead.",
		Example: "  icebath pm lock\n  icebath pm lock --bump uv 0.8.4",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			request := &packagesv1.LockRequest{Profile: picked.Picked(cmd.Context())}
			if cmd.Flags().Changed("bump") {
				request.Bump = &bump
			}
			if len(args) > 0 {
				request.Version = &args[0]
			}
			if usage := (packages.LockArgs{Name: request.Bump, Version: request.Version}).Usage(); usage != "" {
				return usageError(cmd, usage)
			}
			return streamed(cmd, func(t *packagesv1.Terminal) (packagesv1connect.PackageServiceLockClientStream, error) {
				request.Terminal = t
				return service.Lock(cmd.Context(), request)
			})
		},
	}
	cmd.Flags().StringVar(&bump, "bump", "", "Program to pin at VERSION, instead of working out the Python extras again")
	return cmd
}

func install(service packagesv1connect.PackageServiceClient) *cobra.Command {
	var extras, without []string
	var toolsOnly, trust bool
	var target string
	cmd := &cobra.Command{
		Use:   "install [names...]",
		Short: "Install programs, or the default set when you name none",
		Long: "Installs the programs you name. With no names, install sets up every program icebath needs and the optional ones it installs by " +
			"default. A program or extra you leave out with --without stays out of later installs and updates until you install it by name " +
			"or with --extra. --target prepares programs for another platform and needs their names.",
		Example: "  icebath pm install\n  icebath pm install chromium\n  icebath pm install --without cua-driver",
		RunE: func(cmd *cobra.Command, args []string) error {
			return streamed(cmd, func(t *packagesv1.Terminal) (packagesv1connect.PackageServiceInstallClientStream, error) {
				return service.Install(cmd.Context(), &packagesv1.InstallRequest{Profile: picked.Picked(cmd.Context()), Terminal: t, Names: args, Extras: extras, Without: without, ToolsOnly: toolsOnly,
					TrustRecorded: trust, Target: target})
			})
		},
	}
	f := cmd.Flags()
	f.StringArrayVar(&extras, "extra", nil, "Python extra to install in its own environment, repeatable")
	f.StringArrayVar(&without, "without", nil, "Optional program or extra to leave out, now and in later installs, repeatable")
	f.BoolVar(&toolsOnly, "tools-only", false, "Install the programs and put them on PATH, but skip the Python environments")
	f.BoolVar(&trust, "trust-recorded", false, "Trust the recorded checksums instead of checking every file again")
	f.StringVar(&target, "target", "", "Platform to prepare the named programs for, such as linux-arm64-bionic")
	return cmd
}

func env(service packagesv1connect.PackageServiceClient) *cobra.Command {
	return &cobra.Command{
		Use:   "env [names...]",
		Short: "Print the environment variables of installed programs",
		Long: "Prints, as JSON, the environment variables that let you run the installed programs you name, or all of them. Use env to run those " +
			"programs outside icebath.",
		RunE: func(cmd *cobra.Command, args []string) error {
			response, err := service.Env(cmd.Context(), &packagesv1.EnvRequest{Names: args})
			if err != nil {
				return err
			}
			data, err := json.MarshalIndent(response.GetEnv(), "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(data))
			return nil
		},
	}
}

func doctor(service packagesv1connect.PackageServiceClient) *cobra.Command {
	var fix bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the installed programs against the lock file",
		Long: "Checks the installed programs against the lock file and says what is missing or does not match. With --repair, doctor also " +
			"removes broken entries from the uv cache.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return streamed(cmd, func(t *packagesv1.Terminal) (packagesv1connect.PackageServiceDoctorClientStream, error) {
				return service.Doctor(cmd.Context(), &packagesv1.DoctorRequest{Profile: picked.Picked(cmd.Context()), Terminal: t, Repair: fix})
			})
		},
	}
	cmd.Flags().BoolVar(&fix, "repair", false, "Remove broken entries from the uv cache so they download again")
	return cmd
}

func repair(service packagesv1connect.PackageServiceClient) *cobra.Command {
	return &cobra.Command{
		Use:   "repair",
		Short: "Rebuild the Python environment without changing versions",
		Long: "Builds the Python environment again from the versions already recorded. Use repair when the environment is broken and you do not " +
			"want any version to change.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return streamed(cmd, func(t *packagesv1.Terminal) (packagesv1connect.PackageServiceRepairClientStream, error) {
				return service.Repair(cmd.Context(), &packagesv1.RepairRequest{Profile: picked.Picked(cmd.Context()), Terminal: t})
			})
		},
	}
}

func environment(service packagesv1connect.PackageServiceClient) *cobra.Command {
	var extras, requirements []string
	cmd := &cobra.Command{
		Use:   "environment <name>",
		Short: "Build a separate Python environment and print its path",
		Long: "Builds the Python environment NAME of this profile, or reuses it when it exists. Each such environment is kept apart from every " +
			"other. --extra installs the pinned packages of an extra and --requirement installs a requirement you write. Only the path of the " +
			"environment's Python is printed on standard output, so a script can use it.",
		Example: "  icebath pm environment notebooks --requirement 'pandas>=2,<3'",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := newScreen(cmd.ErrOrStderr())
			stream, err := service.Environment(cmd.Context(), &packagesv1.EnvironmentRequest{Profile: picked.Picked(cmd.Context()), Terminal: s.terminal(), Name: args[0], Extras: extras,
				Requirements: requirements})
			if err != nil {
				return err
			}
			defer stream.Close()
			python, err := follow(cmd, s, stream.Receive)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), python)
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&extras, "extra", nil, "Python extra to install in the environment, repeatable")
	cmd.Flags().StringArrayVar(&requirements, "requirement", nil, "Requirement to install, such as 'httpx>=0.27,<1', repeatable")
	return cmd
}

func gc(service packagesv1connect.PackageServiceClient) *cobra.Command {
	return &cobra.Command{
		Use:   "gc",
		Short: "Remove downloaded files that nothing uses",
		Long:  "Deletes the downloaded files that no installed program or Python environment uses any more, to free disk space.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return streamed(cmd, func(t *packagesv1.Terminal) (packagesv1connect.PackageServiceCollectClientStream, error) {
				return service.Collect(cmd.Context(), &packagesv1.CollectRequest{Profile: picked.Picked(cmd.Context()), Terminal: t})
			})
		},
	}
}

func status(service packagesv1connect.PackageServiceClient) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Print the record of the last pm command as JSON",
		Long:  "Prints, as JSON, what the last pm command did: each step, any warning or refusal, and how it ended.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			response, err := service.Status(cmd.Context(), &packagesv1.StatusRequest{Profile: picked.Picked(cmd.Context())})
			if err != nil {
				return err
			}
			// With no record kept, the text is why, which is no JSON to print as the record
			if !response.GetFound() {
				fmt.Fprintln(cmd.ErrOrStderr(), response.GetText())
				return cli.Exit(1)
			}
			fmt.Fprintln(cmd.OutOrStdout(), response.GetText())
			return nil
		},
	}
}

func update(service packagesv1connect.PackageServiceClient) *cobra.Command {
	var check, uv, npm, termux bool
	var target string
	var skip []string
	cmd := &cobra.Command{
		Use:   "update [names...]",
		Short: "Find the newest versions and pin them in the lock file",
		Long: "Looks up the newest version of the programs you name, or of all of them, and pins those versions in the lock file. Use --check " +
			"to see what would change first. --skip-target keeps the pins of a platform whose archives lag behind, such as " +
			"linux-arm64-bionic while the Termux pool catches up. With --termux, --check reports the retired archives without writing.",
		Example: "  icebath pm update --check\n  icebath pm update chromium\n  icebath pm update --skip-target linux-arm64-bionic",
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, t := range skip {
				if !slices.Contains(packages.Targets, t) {
					return usageError(cmd, "“"+t+"” is not a platform --skip-target takes. It takes "+strs.Or(packages.Targets)+".")
				}
			}
			return streamed(cmd, func(t *packagesv1.Terminal) (packagesv1connect.PackageServiceUpdateClientStream, error) {
				return service.Update(cmd.Context(), &packagesv1.UpdateRequest{Profile: picked.Picked(cmd.Context()), Terminal: t, Names: args, Check: check, Target: target, SkipTargets: skip, Uv: uv,
					Npm: npm, Termux: termux})
			})
		},
	}
	f := cmd.Flags()
	f.BoolVar(&check, "check", false, "Only print what would change, ending with status 1 when something would")
	f.StringVar(&target, "target", "", "Platform to look up versions for instead of this machine, such as win32-arm64")
	f.StringArrayVar(&skip, "skip-target", nil, "Platform whose pins stay as they are, repeatable")
	f.BoolVar(&uv, "uv", false, "Also work out and rebuild the Python environments")
	f.BoolVar(&npm, "npm", false, "Also update the Node packages")
	f.BoolVar(&termux, "termux", false, "Pin again the archives that the Termux pool has retired")
	return cmd
}

// The pm commands: install, inspect, repair and update the packages icebath keeps on the machine its server runs on
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/packages"
	packagesv1 "github.com/nickheyer/icebath/pkg/gen/icebath/packages/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/packages/v1/packagesv1connect"
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
		Use:   "pm",
		Short: "Package manager: lock, install, repair, env, doctor, gc",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			usage := "[-h] {" + strings.Join(verbs, ",") + "} ..."
			if len(args) > 0 {
				return usageError(cmd, usage, "argument cmd: invalid choice: "+strconv.Quote(args[0])+" (choose from "+quoted(verbs)+")")
			}
			return usageError(cmd, usage, "the following arguments are required: cmd")
		},
	}
	root.AddCommand(lock(service), install(service), env(service), doctor(service), repair(service), environment(service), gc(service), status(service),
		update(service))
	return []*cobra.Command{root}
}

// Verbs quoted and joined the way the usage lists choices
func quoted(items []string) string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = "'" + item + "'"
	}
	return strings.Join(out, ", ")
}

// Refusal of a command line: its usage and the error on standard error, status 2
func usageError(cmd *cobra.Command, usage, message string) error {
	fmt.Fprintf(cmd.ErrOrStderr(), "usage: %s %s\n%s: error: %s\n", cmd.CommandPath(), usage, cmd.CommandPath(), message)
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
			return "", errors.New("the server ended the command without saying how it went")
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
		Short: "relock the extras (or --bump a tool pin in the package manager's lock)",
		Long: "With no arguments: resolve every declared extra again with the pinned uv for every platform and keep the result in the package manager's " +
			"lock; writes nothing when it is current. With --bump NAME VERSION: pin a tool's archives for every target in the package manager's lock.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			request := &packagesv1.LockRequest{}
			if cmd.Flags().Changed("bump") {
				request.Bump = &bump
			}
			if len(args) > 0 {
				request.Version = &args[0]
			}
			if usage := (packages.LockArgs{Name: request.Bump, Version: request.Version}).Usage(); usage != "" {
				return usageError(cmd, "[-h] [--bump NAME] [VERSION]", usage)
			}
			return streamed(cmd, func(t *packagesv1.Terminal) (packagesv1connect.PackageServiceLockClientStream, error) {
				request.Terminal = t
				return service.Lock(cmd.Context(), request)
			})
		},
	}
	cmd.Flags().StringVar(&bump, "bump", "", "pin tool NAME at VERSION in the package manager's lock instead of relocking the extras")
	return cmd
}

func install(service packagesv1connect.PackageServiceClient) *cobra.Command {
	var extras, without []string
	var toolsOnly, trust bool
	var target string
	cmd := &cobra.Command{
		Use:   "install [names...]",
		Short: "install packages (default: all required + optional defaults)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return streamed(cmd, func(t *packagesv1.Terminal) (packagesv1connect.PackageServiceInstallClientStream, error) {
				return service.Install(cmd.Context(), &packagesv1.InstallRequest{Terminal: t, Names: args, Extras: extras, Without: without, ToolsOnly: toolsOnly,
					TrustRecorded: trust, Target: target})
			})
		},
	}
	f := cmd.Flags()
	f.StringArrayVar(&extras, "extra", nil, "provision a declared Python dependency set in its own environment (repeatable)")
	f.StringArrayVar(&without, "without", nil, "leave an optional default package (chromium, cua-driver) or an extra out of this and every later default "+
		"install and update; `icebath pm install NAME` / `--extra NAME` opts back in (repeatable)")
	f.BoolVar(&toolsOnly, "tools-only", false, "install the tool closure, put it on PATH, and stop before the Python environments")
	f.BoolVar(&trust, "trust-recorded", false, "trust the recorded tool digest instead of re-hashing every entry. a deliberate install re-checks the bytes")
	f.StringVar(&target, "target", "", "stage for a cross target (e.g. linux-arm64-bionic on a glibc CI host); requires explicit package names")
	return cmd
}

func env(service packagesv1connect.PackageServiceClient) *cobra.Command {
	return &cobra.Command{
		Use:   "env [names...]",
		Short: "print composed env of installed packages",
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
		Short: "check installed state against the lockfile",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return streamed(cmd, func(t *packagesv1.Terminal) (packagesv1connect.PackageServiceDoctorClientStream, error) {
				return service.Doctor(cmd.Context(), &packagesv1.DoctorRequest{Terminal: t, Repair: fix})
			})
		},
	}
	cmd.Flags().BoolVar(&fix, "repair", false, "remove corrupt entries from PM's uv cache so they are fetched again")
	return cmd
}

func repair(service packagesv1connect.PackageServiceClient) *cobra.Command {
	return &cobra.Command{
		Use:   "repair",
		Short: "rebuild the recorded dependency environment without changing its graph",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return streamed(cmd, func(t *packagesv1.Terminal) (packagesv1connect.PackageServiceRepairClientStream, error) {
				return service.Repair(cmd.Context(), &packagesv1.RepairRequest{Terminal: t})
			})
		},
	}
}

func environment(service packagesv1connect.PackageServiceClient) *cobra.Command {
	var extras, requirements []string
	cmd := &cobra.Command{
		Use:   "environment <name>",
		Short: "make a named side environment current and print its python",
		Long: "Build or reuse the profile's environments/NAME, an isolated venv that never touches another: --extra installs the pins of a declared extra, " +
			"--requirement an ad hoc bounded requirement (both repeatable). Prints only the interpreter path on stdout.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := newScreen(cmd.ErrOrStderr())
			stream, err := service.Environment(cmd.Context(), &packagesv1.EnvironmentRequest{Terminal: s.terminal(), Name: args[0], Extras: extras,
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
	cmd.Flags().StringArrayVar(&extras, "extra", nil, "")
	cmd.Flags().StringArrayVar(&requirements, "requirement", nil, "")
	return cmd
}

func gc(service packagesv1connect.PackageServiceClient) *cobra.Command {
	return &cobra.Command{
		Use:   "gc",
		Short: "remove store entries nothing references",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return streamed(cmd, func(t *packagesv1.Terminal) (packagesv1connect.PackageServiceCollectClientStream, error) {
				return service.Collect(cmd.Context(), &packagesv1.CollectRequest{Terminal: t})
			})
		},
	}
}

func status(service packagesv1connect.PackageServiceClient) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "print the latest pm sync receipt (machine-readable)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			response, err := service.Status(cmd.Context(), &packagesv1.StatusRequest{})
			if err != nil {
				return err
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
		Short: "resolve latest versions and re-pin the lockfile",
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, t := range skip {
				if !slices.Contains(packages.Targets, t) {
					return usageError(cmd, "[-h] [--check] [--target TARGET] [--skip-target {"+strings.Join(packages.Targets, ",")+"}] [--uv] [--npm] [--termux] [names ...]",
						"argument --skip-target: invalid choice: "+strconv.Quote(t)+" (choose from "+quoted(packages.Targets)+")")
				}
			}
			return streamed(cmd, func(t *packagesv1.Terminal) (packagesv1connect.PackageServiceUpdateClientStream, error) {
				return service.Update(cmd.Context(), &packagesv1.UpdateRequest{Terminal: t, Names: args, Check: check, Target: target, SkipTargets: skip, Uv: uv,
					Npm: npm, Termux: termux})
			})
		},
	}
	f := cmd.Flags()
	f.BoolVar(&check, "check", false, "dry-run: print what would change, write nothing (exit 1 if updates exist)")
	f.StringVar(&target, "target", "", "resolve for a different target instead of this machine (e.g. win32-arm64)")
	f.StringArrayVar(&skip, "skip-target", nil, "leave TARGET's pins as they are, e.g. linux-arm64-bionic while the termux pool lags upstream (repeatable)")
	f.BoolVar(&uv, "uv", false, "also refresh the Python environments (resolve again + rebuild)")
	f.BoolVar(&npm, "npm", false, "also refresh the Node dependency trees (npm update)")
	f.BoolVar(&termux, "termux", false, "repin the termux pool archives the pool has retired; --check reports without writing")
	return cmd
}

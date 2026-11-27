package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/config"
	picked "github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/pkg/fsx"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
)

type commands struct{}

func Register(host *cli.CLI) {
	host.Register(commands{})
}

// Usage of a command missing its key or value, with examples
type usage struct {
	line     string
	examples []string
}

var (
	usageGet = usage{"Usage: icebath config get <key> [--json] [--raw]",
		[]string{"icebath config get agent.model", "icebath config get terminal.backend", "icebath config get skills.config --json"}}
	usageSet = usage{"Usage: icebath config set <key> <value>",
		[]string{"icebath config set agent.model anthropic/claude-sonnet-4", "icebath config set terminal.backend docker",
			"icebath config set OPENROUTER_API_KEY sk-or-..."}}
	usageUnset = usage{"Usage: icebath config unset <key>",
		[]string{"icebath config unset agent.model", "icebath config unset terminal.backend", "icebath config unset OPENROUTER_API_KEY"}}
)

// Prints the usage with its examples and ends the command with status 1
func (u usage) exit(c *cli.Console) error {
	c.Say(u.line)
	c.Blank()
	c.Say("Examples")
	for _, example := range u.examples {
		c.Say("  " + example)
	}
	return cli.Exit(1)
}

// Message of a failed call, the server's own words for one it refused
func reason(err error) string {
	if failure, ok := errors.AsType[*connect.Error](err); ok {
		return failure.Message()
	}
	return err.Error()
}

// Prints why a call failed on standard error, marked as a failure, and ends the command with status 1
func refused(cmd *cobra.Command, err error) error {
	fmt.Fprintln(cmd.ErrOrStderr(), "✗ "+reason(err))
	return cli.Exit(1)
}

// Commands of config, as an unknown one lists them
const usageText = `Available commands:
  icebath config                     Show every setting and where the files are
  icebath config edit                Open the settings file in your editor
  icebath config get <key>           Print the value a setting has now
  icebath config set <key> <value>   Change a setting or save an API key
  icebath config unset <key>         Reset a setting or remove a saved API key
  icebath config check               Check the settings file and which API keys are set
  icebath config path                Print where the settings file is
  icebath config env-path            Print where the keys file is`

// Whether a package manager keeps the settings, which it then says on standard error; the command ends without failing, as the install
// is not broken
func managed(cmd *cobra.Command, service configv1connect.ConfigServiceClient, profile string) (bool, error) {
	files, err := service.Files(cmd.Context(), &configv1.FilesRequest{Profile: profile})
	if err != nil {
		return false, refused(cmd, err)
	}
	system := files.GetManagedSystem()
	if system == "" {
		return false, nil
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "✗ Settings can't be changed here. This copy of icebath is managed by %s, which keeps its settings. Change them "+
		"through %s.\n", system, system)
	return true, nil
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := configv1connect.NewConfigServiceClient(conn)
	var profile string
	show := func(cmd *cobra.Command, _ []string) error { return showConfig(cmd, service, profile) }
	root := &cobra.Command{
		Use:     "config",
		Short:   "Show and change settings",
		GroupID: cli.GroupApp,
		Long: "Settings are kept in config.yaml and API keys in .env, one of each for every profile. With no command, config shows every " +
			"setting. The /settings screen in the app changes the same settings.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "✗ config has no command named %s.\n\n%s\n", args[0], usageText)
				return cli.Exit(1)
			}
			return show(cmd, args)
		},
	}
	// -p picks the profile whose settings the commands act on, the server's profile in use when it picked none
	root.PersistentPreRun = func(cmd *cobra.Command, _ []string) { profile = picked.Picked(cmd.Context()) }
	var asJSON, raw, shared bool
	get := &cobra.Command{
		Use:   "get [key]",
		Short: "Print the value a setting has now",
		Long: "Prints the value in effect for a setting: what an environment variable gives it, else what the settings file holds, else " +
			"the default. A key in capitals prints an API key of the keys file, hidden unless you pass --raw.",
		Example: "  icebath config get agent.model\n  icebath config get skills.config --json",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cli.NewConsole(cmd)
			if len(args) == 0 {
				return usageGet.exit(c)
			}
			response, err := service.Value(cmd.Context(), &configv1.ValueRequest{Key: args[0], Profile: profile, Raw: raw})
			if err != nil {
				return refused(cmd, err)
			}
			if !response.GetFound() {
				fmt.Fprintln(cmd.ErrOrStderr(), "✗ "+args[0]+" has no value.")
				return cli.Exit(1)
			}
			if note := response.GetNote(); note != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), note)
			}
			if asJSON {
				fmt.Fprintln(cmd.OutOrStdout(), response.GetJson())
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), response.GetYaml())
			}
			return nil
		},
	}
	get.Flags().BoolVar(&asJSON, "json", false, "Print the value as JSON")
	get.Flags().BoolVar(&raw, "raw", false, "Print API keys and passwords as they are, not hidden")
	path := &cobra.Command{
		Use:   "path",
		Short: "Print where the settings file is",
		Long:  "Prints the path of the config.yaml of this profile, or with --shared the one every profile shares.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			files, err := service.Files(cmd.Context(), &configv1.FilesRequest{Profile: profile})
			if err != nil {
				return refused(cmd, err)
			}
			target := files.GetOwn()
			if shared || target == "" {
				target = files.GetShared()
			}
			fmt.Fprintln(cmd.OutOrStdout(), target)
			return nil
		},
	}
	path.Flags().BoolVar(&shared, "shared", false, "The settings file every profile shares, not the profile's own")
	var editShared bool
	edit := &cobra.Command{
		Use:   "edit",
		Short: "Open the settings file in your editor",
		Long: "Opens config.yaml in the editor that EDITOR names. A file that is not there yet is made first, with every setting " +
			"commented out at its default. The file is checked once the editor closes.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return editConfig(cmd, service, profile, editShared) },
	}
	edit.Flags().BoolVar(&editShared, "shared", false, "Edit the settings file every profile shares, not the profile's own")
	root.AddCommand(
		&cobra.Command{Use: "show", Short: "Show every setting and where the files are", Args: cobra.NoArgs, RunE: show,
			Long: "Prints where the settings file and the keys file are, which API keys are set, and every setting with its value."},
		edit,
		get,
		&cobra.Command{
			Use:   "set [key] [value]",
			Short: "Change a setting or save an API key",
			Long: "Saves a value for a setting in the settings file. A key in capitals, such as OPENROUTER_API_KEY, is saved in the keys " +
				"file instead. A length of time may be written as 30s, 5m or 2h.",
			Example: "  icebath config set terminal.backend docker\n  icebath config set terminal.timeout 5m\n  icebath config set OPENROUTER_API_KEY sk-or-...",
			Args:    cobra.MaximumNArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				c := cli.NewConsole(cmd)
				if len(args) < 2 {
					return usageSet.exit(c)
				}
				if stop, err := managed(cmd, service, profile); stop || err != nil {
					return err
				}
				response, err := service.Put(cmd.Context(), &configv1.PutRequest{Key: args[0], Value: args[1], Profile: profile})
				if err != nil {
					return refused(cmd, err)
				}
				if warning := response.GetWarning(); warning != "" {
					fmt.Fprintln(cmd.OutOrStdout(), warning)
				}
				if response.GetEnv() {
					fmt.Fprintf(cmd.OutOrStdout(), "✓ Saved %s in %s.\n", response.GetKey(), response.GetDestination())
					return nil
				}
				fmt.Fprintf(cmd.OutOrStdout(), "✓ %s is now %s, saved in %s.\n", response.GetKey(), response.GetShown(), response.GetDestination())
				return nil
			},
		},
		&cobra.Command{
			Use:   "unset [key]",
			Short: "Reset a setting or remove a saved API key",
			Long: "Removes the value saved for a setting, which puts the setting back to its default. A key in capitals is removed from " +
				"the keys file.",
			Example: "  icebath config unset agent.model\n  icebath config unset OPENROUTER_API_KEY",
			Args:    cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				c := cli.NewConsole(cmd)
				if len(args) == 0 {
					return usageUnset.exit(c)
				}
				if stop, err := managed(cmd, service, profile); stop || err != nil {
					return err
				}
				response, err := service.Unset(cmd.Context(), &configv1.UnsetRequest{Key: args[0], Profile: profile})
				if err != nil {
					return refused(cmd, err)
				}
				if !response.GetFound() {
					fmt.Fprintln(cmd.ErrOrStderr(), "✗ "+args[0]+" has no saved value to remove.")
					return cli.Exit(1)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "✓ Removed %s from %s.\n", args[0], response.GetDestination())
				return nil
			},
		},
		path,
		&cobra.Command{
			Use:   "env-path",
			Short: "Print where the keys file is",
			Long:  "Prints the path of the .env of this profile, where API keys are kept.",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				files, err := service.Files(cmd.Context(), &configv1.FilesRequest{Profile: profile})
				if err != nil {
					return refused(cmd, err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), files.GetEnv())
				return nil
			},
		},
		&cobra.Command{
			Use:   "check",
			Short: "Check the settings file and which API keys are set",
			Long:  "Reports mistakes in the settings file, settings that have no effect as saved, and which API keys are set and which are not.",
			Args:  cobra.NoArgs,
			RunE:  func(cmd *cobra.Command, _ []string) error { return checkConfig(cmd, service, profile) },
		},
	)
	return []*cobra.Command{root}
}

// Prints what is wrong with the saved settings, then which API keys and other variables are set, the missing ones that are needed first
func checkConfig(cmd *cobra.Command, service configv1connect.ConfigServiceClient, profile string) error {
	response, err := service.Check(cmd.Context(), &configv1.CheckRequest{Profile: profile})
	if err != nil {
		return refused(cmd, err)
	}
	c := cli.NewConsole(cmd)
	heading := func(title string) {
		c.Blank()
		c.Say(title, cli.Cyan, cli.Bold)
	}
	heading("Saved settings")
	if diagnostics := response.GetDiagnostics(); len(diagnostics) > 0 {
		for _, line := range diagnostics {
			c.Warning(line)
		}
	} else {
		c.Success("No problems found.")
	}
	var missing, set, open []*configv1.Variable
	for _, variable := range response.GetRequired() {
		if variable.GetSet() {
			set = append(set, variable)
		} else {
			missing = append(missing, variable)
		}
	}
	for _, variable := range response.GetOptional() {
		if variable.GetSet() {
			set = append(set, variable)
		} else {
			open = append(open, variable)
		}
	}
	if len(missing) > 0 {
		heading("Keys that are needed and missing")
		for _, variable := range missing {
			c.Failure(named(variable))
		}
	}
	heading("Keys that are set")
	for _, variable := range set {
		c.Success(named(variable))
	}
	if len(set) == 0 {
		c.Info("None. `icebath config set <NAME> <value>` saves one.")
	}
	if len(open) > 0 {
		heading(strs.Count(len(open), "key") + " that can be set and are not")
		for _, variable := range open {
			c.Info(named(variable))
		}
	}
	c.Blank()
	return nil
}

// Variable with what it is for after its name, when the server says
func named(variable *configv1.Variable) string {
	if about := variable.GetAbout(); about != "" {
		return variable.GetName() + ", " + about
	}
	return variable.GetName()
}

// Runs the editor on the file at the terminal; an editor that ends with a status of its own is not a failure of the command
func runEditor(ctx context.Context, editor, path string) error {
	run := config.EditorCommand(ctx, editor, path)
	run.Stdin, run.Stdout, run.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := run.Run(); err != nil {
		if _, exited := errors.AsType[*exec.ExitError](err); !exited {
			return err
		}
	}
	return nil
}

// Opens the profile's config.yaml, or the shared one, in an editor: in place when it is on this machine, else a copy whose edit is saved
// back through the server; the settings are read again after
func editConfig(cmd *cobra.Command, service configv1connect.ConfigServiceClient, profile string, shared bool) error {
	ctx := cmd.Context()
	files, err := service.Files(ctx, &configv1.FilesRequest{Profile: profile})
	if err != nil {
		return refused(cmd, err)
	}
	out := cmd.OutOrStdout()
	if system := files.GetManagedSystem(); system != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "✗ Settings can't be changed here. This copy of icebath is managed by %s, which keeps its settings. Change them "+
			"through %s.\n", system, system)
		return nil
	}
	target := files.GetOwn()
	if shared {
		target = files.GetShared()
	}
	if target == "" {
		fmt.Fprintln(cmd.ErrOrStderr(), "✗ There is no settings file to open, because this profile keeps its settings on a server. Use `icebath config "+
			"set <key> <value>`.")
		return cli.Exit(1)
	}
	host, _ := os.Hostname()
	local := host == files.GetHost() && fsx.IsDir(filepath.Dir(target))
	read, err := service.Read(ctx, &configv1.ReadRequest{Path: target, Profile: profile})
	if err != nil {
		return refused(cmd, err)
	}
	// A file not made yet starts as the example of its scope: every setting at its default, commented out
	if !read.GetExists() {
		seeded, err := service.Seed(ctx, &configv1.SeedRequest{Path: target, Profile: profile})
		if err != nil {
			return refused(cmd, err)
		}
		if seeded.GetSeeded() {
			fmt.Fprintf(out, "Made %s with every setting at its default.\n", target)
		}
		if read, err = service.Read(ctx, &configv1.ReadRequest{Path: target, Profile: profile}); err != nil {
			return refused(cmd, err)
		}
	}
	editing := target
	if !local {
		temp, err := os.MkdirTemp("", "icebath-config-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(temp)
		editing = filepath.Join(temp, filepath.Base(target))
		if err := os.WriteFile(editing, []byte(read.GetText()), 0o600); err != nil {
			return err
		}
	}
	chosen := config.Editor()
	if chosen == "" {
		fmt.Fprintln(out, "No editor was found. Set EDITOR to the editor you use, or open the settings file yourself:")
		fmt.Fprintf(out, "  %s\n", target)
		return nil
	}
	fmt.Fprintf(out, "Opening %s in %s.\n", target, chosen)
	if err := runEditor(ctx, chosen, editing); err != nil {
		return err
	}
	edited, err := os.ReadFile(editing)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !local && bytes.Equal(edited, []byte(read.GetText())) {
		fmt.Fprintln(out, "Nothing was changed.")
		return nil
	}
	written, err := service.Write(ctx, &configv1.WriteRequest{Path: target, Text: string(edited), Profile: profile})
	if err != nil {
		return refused(cmd, err)
	}
	if problem := written.GetProblem(); problem != "" {
		c := cli.NewConsole(cmd)
		c.Warning(fmt.Sprintf("%s has a formatting error: %s. Until it is fixed, icebath uses the last settings that worked. Run `icebath config "+
			"check` once you have fixed it.", target, problem))
	}
	return nil
}

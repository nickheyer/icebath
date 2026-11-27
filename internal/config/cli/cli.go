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
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/config"
	picked "github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/pkg/fsx"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
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
	c.Say("Examples:")
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

// Prints why a call failed on standard error and ends the command with status 1
func refused(cmd *cobra.Command, err error) error {
	fmt.Fprintln(cmd.ErrOrStderr(), reason(err))
	return cli.Exit(1)
}

// Commands of config, as an unknown one lists them
const usageText = `Available commands:
  icebath config           Show current configuration
  icebath config edit      Open config in editor
  icebath config get <key>          Print a resolved config value
  icebath config set <key> <value>   Set a config value
  icebath config unset <key>        Remove a config value
  icebath config check     Check which keys are set and what the saved config leaves inactive
  icebath config path      Show config file path
  icebath config env-path  Show .env file path`

// Whether a package manager keeps the settings, which it then says on standard error as refusing the action; the command ends without
// failing, as the install is not broken
func managed(cmd *cobra.Command, service configv1connect.ConfigServiceClient, profile, action string) (bool, error) {
	files, err := service.Files(cmd.Context(), &configv1.FilesRequest{Profile: profile})
	if err != nil {
		return false, refused(cmd, err)
	}
	system := files.GetManagedSystem()
	if system == "" {
		return false, nil
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Cannot %s: this Icebath installation is managed by %s.\nUse your package manager to upgrade or reinstall Icebath.\n",
		action, system)
	return true, nil
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := configv1connect.NewConfigServiceClient(conn)
	var profile string
	show := func(cmd *cobra.Command, _ []string) error { return showConfig(cmd, service, profile) }
	root := &cobra.Command{
		Use:   "config",
		Short: "View and edit configuration",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "Unknown config command: %s\n\n%s\n", args[0], usageText)
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
		Short: "Print a resolved configuration value",
		Args:  cobra.MaximumNArgs(1),
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
				fmt.Fprintln(cmd.ErrOrStderr(), "Config key not set: "+args[0])
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
	get.Flags().BoolVar(&asJSON, "json", false, "print the value as JSON")
	get.Flags().BoolVar(&raw, "raw", false, "print credentials as they are rather than masked")
	path := &cobra.Command{
		Use:   "path",
		Short: "Print config file path",
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
	path.Flags().BoolVar(&shared, "shared", false, "the config.yaml every profile shares rather than the profile's own")
	var editShared bool
	edit := &cobra.Command{
		Use:   "edit",
		Short: "Open config file in editor",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return editConfig(cmd, service, profile, editShared) },
	}
	edit.Flags().BoolVar(&editShared, "shared", false, "edit the config.yaml every profile shares rather than the profile's own")
	root.AddCommand(
		&cobra.Command{Use: "show", Short: "Show current configuration", Args: cobra.NoArgs, RunE: show},
		edit,
		get,
		&cobra.Command{
			Use:   "set [key] [value]",
			Short: "Set a configuration value",
			Args:  cobra.MaximumNArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				c := cli.NewConsole(cmd)
				if len(args) < 2 {
					return usageSet.exit(c)
				}
				if stop, err := managed(cmd, service, profile, "set configuration values"); stop || err != nil {
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
					fmt.Fprintf(cmd.OutOrStdout(), "✓ Set %s in %s\n", response.GetKey(), response.GetDestination())
					return nil
				}
				fmt.Fprintf(cmd.OutOrStdout(), "✓ Set %s = %s in %s\n", response.GetKey(), response.GetShown(), response.GetDestination())
				return nil
			},
		},
		&cobra.Command{
			Use:   "unset [key]",
			Short: "Remove a configuration value",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				c := cli.NewConsole(cmd)
				if len(args) == 0 {
					return usageUnset.exit(c)
				}
				if stop, err := managed(cmd, service, profile, "unset configuration values"); stop || err != nil {
					return err
				}
				response, err := service.Unset(cmd.Context(), &configv1.UnsetRequest{Key: args[0], Profile: profile})
				if err != nil {
					return refused(cmd, err)
				}
				if !response.GetFound() {
					fmt.Fprintln(cmd.ErrOrStderr(), "Config key not set: "+args[0])
					return cli.Exit(1)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "✓ Unset %s from %s\n", args[0], response.GetDestination())
				return nil
			},
		},
		path,
		&cobra.Command{
			Use:   "env-path",
			Short: "Print .env file path",
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
			Short: "Check which keys are set and what the saved config leaves inactive",
			Args:  cobra.NoArgs,
			RunE:  func(cmd *cobra.Command, _ []string) error { return checkConfig(cmd, service, profile) },
		},
	)
	return []*cobra.Command{root}
}

// Prints which variables the settings read are set, and what the saved settings leave inactive
func checkConfig(cmd *cobra.Command, service configv1connect.ConfigServiceClient, profile string) error {
	response, err := service.Check(cmd.Context(), &configv1.CheckRequest{Profile: profile})
	if err != nil {
		return refused(cmd, err)
	}
	c := cli.NewConsole(cmd)
	c.Blank()
	c.Say("📋 Configuration Status", cli.Cyan, cli.Bold)
	c.Blank()
	for _, group := range []struct {
		title     string
		variables []*configv1.Variable
		required  bool
	}{{"Required", response.GetRequired(), true}, {"Optional", response.GetOptional(), false}} {
		c.Blank()
		c.Say("  "+group.title+":", cli.Bold)
		for _, variable := range group.variables {
			switch {
			case variable.GetSet():
				c.Say("    ✓ " + variable.GetName())
			case group.required:
				c.Say("    ✗ "+variable.GetName()+" (missing)", cli.Red)
			default:
				line := "    ○ " + variable.GetName()
				if readers := variable.GetSettings(); len(readers) > 0 {
					line += " → " + strings.Join(readers[:min(2, len(readers))], ", ")
				}
				c.Say(line, cli.Dim)
			}
		}
	}
	if diagnostics := response.GetDiagnostics(); len(diagnostics) > 0 {
		c.Blank()
		c.Say("  Saved configuration:", cli.Bold)
		for _, line := range diagnostics {
			c.Say("    ⚠ "+line, cli.Yellow)
		}
	}
	c.Blank()
	return nil
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
		fmt.Fprintf(cmd.ErrOrStderr(), "Cannot edit configuration: this Icebath installation is managed by %s.\nUse your package manager to upgrade or reinstall "+
			"Icebath.\n", system)
		return nil
	}
	target := files.GetOwn()
	if shared {
		target = files.GetShared()
	}
	if target == "" {
		fmt.Fprintln(cmd.ErrOrStderr(), "`icebath config edit` opens the local config file, and this config backend has none. Use `icebath config set "+
			"<key> <value>` instead.")
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
			fmt.Fprintf(out, "Created %s\n", target)
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
		fmt.Fprintln(out, "No editor found. Config file is at:")
		fmt.Fprintf(out, "  %s\n", target)
		return nil
	}
	fmt.Fprintf(out, "Opening %s in %s...\n", target, chosen)
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
		fmt.Fprintln(out, "No changes.")
		return nil
	}
	written, err := service.Write(ctx, &configv1.WriteRequest{Path: target, Text: string(edited), Profile: profile})
	if err != nil {
		return refused(cmd, err)
	}
	if problem := written.GetProblem(); problem != "" {
		c := cli.NewConsole(cmd)
		c.Warning(fmt.Sprintf("%s has a formatting error: %s. Icebath keeps running on its last good settings until it is fixed; run `icebath config check`.",
			target, problem))
	}
	return nil
}

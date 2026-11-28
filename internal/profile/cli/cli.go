package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/pkg/fsx"
	profilev1 "github.com/nickheyer/icebath/pkg/gen/icebath/profile/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/profile/v1/profilev1connect"
	"github.com/nickheyer/icebath/pkg/strs"
)

type commands struct{}

func Register(host *cli.CLI) {
	host.Register(commands{})
}

// Whether the command only lists profiles or switches back to the default one, which runs in the default profile when the profile used last
// is gone
func Recovering(args []string) bool {
	words := make([]string, 0, len(args))
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			words = append(words, arg)
		}
	}
	switch {
	case len(words) >= 1 && words[0] == "profiles":
		return true
	case len(words) >= 2 && words[0] == "profile" && words[1] == "list":
		return true
	case len(words) >= 3 && words[0] == "profile" && words[1] == "use":
		return strings.EqualFold(words[2], profile.Default)
	}
	return false
}

// Profile -p or --profile picks for this run, taken out of the arguments before the command parses them; a value that is no profile name
// given before the command is told on w and ends the run with status 2
func Pick(args []string, w io.Writer) (string, []string, error) {
	command := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if (arg == "-p" || arg == "--profile") && i+1 < len(args) {
			value := strings.ToLower(strings.TrimSpace(args[i+1]))
			if profile.Shaped(value) {
				return value, append(append([]string{}, args[:i]...), args[i+2:]...), nil
			}
			if !command && !strings.HasPrefix(args[i+1], "-") {
				fmt.Fprintln(w, "✗ "+profile.Invalid(args[i+1]).Error()+"\nRun icebath profile list to see your profiles.")
				return "", args, cli.Exit(2)
			}
			break
		}
		if value, ok := strings.CutPrefix(arg, "--profile="); ok {
			return strings.ToLower(strings.TrimSpace(value)), append(append([]string{}, args[:i]...), args[i+1:]...), nil
		}
		if !strings.HasPrefix(arg, "-") {
			command = true
		}
	}
	return "", args, nil
}

// Message of a failed call, the server's own words for one it refused
func reason(err error) string {
	if failure, ok := errors.AsType[*connect.Error](err); ok {
		return failure.Message()
	}
	return err.Error()
}

// Prints why with the mark of a failure and ends the command with status 1
func fail(w io.Writer, err error) error {
	fmt.Fprintln(w, "✗ "+strs.Sentence(reason(err)))
	return cli.Exit(1)
}

func say(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, format+"\n", args...)
}

// Labels and their values, each value after its label padded to the widest
func rows(pairs [][2]string) []string {
	width := 0
	for _, pair := range pairs {
		width = max(width, cli.Width(pair[0])+1)
	}
	lines := make([]string, len(pairs))
	for i, pair := range pairs {
		lines[i] = cli.Pad(pair[0]+":", width) + "  " + pair[1]
	}
	return lines
}

func model(p *profilev1.Profile) string {
	if p.GetModel() == "" {
		return ""
	}
	if p.GetProvider() != "" {
		return p.GetModel() + " · " + p.GetProvider()
	}
	return p.GetModel()
}

func exists(present bool) string {
	if present {
		return "Exists"
	}
	return "Missing"
}

// Distribution and its version as one value
func distribution(name, version string) string {
	if version == "" {
		return name + ", version unknown"
	}
	return name + ", version " + version
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := profilev1connect.NewProfileServiceClient(conn)
	c := &commandSet{service: service}
	profiles := &cobra.Command{
		Use:     "profiles",
		GroupID: cli.GroupApp,
		Short:   "List your profiles",
		Long:    "Lists every profile and marks the one in use. This is the same as icebath profile list.",
		Args:    cobra.NoArgs,
		RunE:    c.list,
	}
	root := &cobra.Command{
		Use:     "profile",
		GroupID: cli.GroupApp,
		Short:   "Manage your profiles, separate sets of settings and sessions",
		Long: "A profile is a separate set of settings, providers and sessions, such as one for work and one for home. With no subcommand, " +
			"profile shows the profile in use. Put -p NAME before any command to use a profile for that one run.",
		Example: "  icebath profile create work --clone\n  icebath profile use work\n  icebath -p work chat -q \"Hello\"",
		Args:    cobra.NoArgs,
		RunE:    c.status,
	}
	root.AddCommand(&cobra.Command{Use: "list", Short: "List your profiles", Long: "Lists every profile and marks the one in use.", Args: cobra.NoArgs, RunE: c.list},
		&cobra.Command{Use: "use <profile_name>", Short: "Make a profile the one icebath starts with",
			Long: "Makes the profile the one icebath uses from now on, until you pick another. -p picks a profile for one run only.",
			Args: cobra.ExactArgs(1), RunE: c.use},
		c.create(), c.delete(), c.describe(),
		&cobra.Command{Use: "show <profile_name>", Short: "Show the details of a profile",
			Long: "Shows the details of one profile, such as where it is kept and the model it uses.", Args: cobra.ExactArgs(1), RunE: c.show},
		c.alias(),
		&cobra.Command{Use: "rename <old_name> <new_name>", Short: "Rename a profile",
			Long: "Renames the profile and its folder. The default profile keeps its folder, so for it rename only changes the name shown, and an " +
				"empty name shows it as default again.",
			Example: "  icebath profile rename work research\n  icebath profile rename default Home\n  icebath profile rename default \"\"",
			Args:    cobra.ExactArgs(2), RunE: c.rename},
		&cobra.Command{Use: "purge-identity <profile_name>", Short: "Finish removing the data of a deleted profile",
			Long: "Tries again to remove what icebath keeps about a deleted profile elsewhere, such as the routes of its sessions. Run " +
				"purge-identity when delete says that this part failed.",
			Args: cobra.ExactArgs(1), RunE: c.purge},
		&cobra.Command{Use: "migrate-identity <old_name> <new_name>", Short: "Finish moving the data of a renamed profile",
			Long: "Tries again to move what icebath keeps under a renamed profile's old name, such as the routes of its sessions. Run " +
				"migrate-identity when rename says that this part failed.",
			Args: cobra.ExactArgs(2), RunE: c.migrate},
		c.export(), c.importArchive(), c.install(), c.update(),
		&cobra.Command{Use: "info <profile_name>", Short: "Show where an installed profile came from",
			Long: "Shows the manifest of a profile you installed with install: its version, what it needs and where it came from.",
			Args: cobra.ExactArgs(1), RunE: c.info},
	)
	return []*cobra.Command{profiles, root}
}

type commandSet struct {
	service profilev1connect.ProfileServiceClient
}

// Bare profile: the profile in use, where it is and what it runs
func (c *commandSet) status(cmd *cobra.Command, _ []string) error {
	listed, err := c.service.List(cmd.Context(), &profilev1.ListRequest{})
	if err != nil {
		return fail(cmd.OutOrStdout(), err)
	}
	out := cmd.OutOrStdout()
	for _, p := range listed.GetProfiles() {
		if p.GetName() != listed.GetActive() {
			continue
		}
		shown := [][2]string{{"Profile in use", profile.Label(p.GetName(), p.GetDisplayName())}, {"Folder", profile.Tilde(p.GetPath())}}
		if m := model(p); m != "" {
			shown = append(shown, [2]string{"Model", m})
		}
		shown = append(shown, [2]string{"Skills", strs.Count(int(p.GetSkills()), "skill")})
		if alias, _ := profile.AliasOf(p.GetName()); alias != "" && !p.GetDefault() {
			if path, _ := profile.OurAlias(alias); path != "" {
				shown = append(shown, [2]string{"Alias", alias + ", which runs icebath -p " + p.GetName()})
			}
		}
		say(out, "")
		for _, line := range rows(shown) {
			say(out, "%s", line)
		}
	}
	say(out, "")
	return nil
}

// Table of every profile with its model, alias and distribution
func (c *commandSet) list(cmd *cobra.Command, _ []string) error {
	listed, err := c.service.List(cmd.Context(), &profilev1.ListRequest{})
	if err != nil {
		return fail(cmd.OutOrStdout(), err)
	}
	out := cmd.OutOrStdout()
	if len(listed.GetProfiles()) == 0 {
		say(out, "There are no profiles yet. Create one with icebath profile create <profile_name>.")
		return nil
	}
	aliases, _ := profile.Aliases()
	var table [][]string
	for _, p := range listed.GetProfiles() {
		marker := ""
		if p.GetActive() {
			marker = "●"
		}
		alias := "None"
		if name := aliases[p.GetName()]; name != "" && !p.GetDefault() {
			alias = name
		}
		dist := "None"
		if p.GetDistribution() != "" {
			dist = distribution(p.GetDistribution(), p.GetDistributionVersion())
		}
		shownModel := "Not set"
		if p.GetModel() != "" {
			shownModel = p.GetModel()
		}
		table = append(table, []string{marker, profile.Label(p.GetName(), p.GetDisplayName()), shownModel, alias, dist})
	}
	console := cli.NewConsole(cmd)
	console.Blank()
	console.Table([]string{"", "Profile", "Model", "Alias", "Distribution"}, table)
	say(out, "\n● marks the profile in use.\n")
	return nil
}

func (c *commandSet) use(cmd *cobra.Command, args []string) error {
	used, err := c.service.Use(cmd.Context(), &profilev1.UseRequest{Name: args[0]})
	if err != nil {
		return fail(cmd.OutOrStdout(), err)
	}
	if used.GetProfile().GetDefault() {
		say(cmd.OutOrStdout(), "✓ icebath uses the default profile from now on. Its folder is %s.", profile.Tilde(used.GetProfile().GetPath()))
	} else {
		say(cmd.OutOrStdout(), "✓ icebath uses the profile %s from now on.", used.GetProfile().GetName())
	}
	return nil
}

func (c *commandSet) create() *cobra.Command {
	var clone, all, channels, syncImports, noAlias, isolated, noCredentials, noSkills bool
	var from, description, startModel, startProvider string
	cmd := &cobra.Command{
		Use:   "create <profile_name>",
		Short: "Create a profile",
		Long: "Creates a profile with its own settings, keys, sessions and skills. Unless you pass --no-credentials, the profile takes your " +
			"API keys, sign-ins and model from the profile in use, so the profile works at once. --clone also copies your settings, and --clone-from " +
			"copies them from another profile. A copy with --no-credentials keeps the settings without their keys and sign-ins.",
		Example: "  icebath profile create work\n  icebath profile create review --clone-from work --description \"Code review\"",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			name := args[0]
			if cmd.Flags().Changed("clone-from") && strings.TrimSpace(from) == "" {
				return fail(out, errors.New("Name the profile to copy from after --clone-from."))
			}
			cloned := clone || cmd.Flags().Changed("clone-from") || all
			if (startModel == "") != (startProvider == "") {
				return fail(out, errors.New("--model and --provider go together, because a model is set with the provider that runs it. Give both or neither."))
			}
			made, err := c.service.Create(cmd.Context(), &profilev1.CreateRequest{Name: name, Clone: clone, From: from, All: all, Channels: channels,
				SyncImports: syncImports, NoSkills: noSkills, Description: description, NoCredentials: noCredentials, Model: startModel,
				Provider: startProvider})
			if err != nil {
				return fail(out, err)
			}
			say(out, "\n✓ Created the profile %s in %s.", made.GetProfile().GetName(), made.GetPath())
			if cloned {
				if all {
					say(out, "Copied every file of %s except its sessions, scheduled jobs, backups and checkpoints.", made.GetSource())
				} else {
					say(out, "Copied the settings file, keys file, SOUL.md and skills of %s.", made.GetSource())
					if plugins := clonedPlugins(made.GetPath()); len(plugins) > 0 {
						say(out, "Copied its plugins too: %s.", strings.Join(plugins, ", "))
					}
				}
				if syncImports {
					if fsx.IsFile(filepath.Join(made.GetPath(), "import-sync.json")) {
						say(out, "Copied import-sync.json, the record of what %s imported from other agents such as Claude Code and Codex.", made.GetSource())
					} else {
						say(out, "%s has no import-sync.json, the record of agents it imported from, so there was none to copy.", made.GetSource())
					}
				}
			}
			for _, line := range made.GetSeeded() {
				say(out, "%s", line)
			}
			canon := made.GetProfile().GetName()
			aliased := !noAlias && aliasCreated(out, canon)
			if made.GetKeys() {
				say(out, "Copied the API keys of %s.", made.GetLaunch())
			}
			for _, note := range made.GetNotes() {
				say(out, "%s", note)
			}
			for _, warning := range made.GetWarnings() {
				say(out, "⚠ %s", strs.Sentence(warning))
			}
			display := profile.Tilde(made.GetPath())
			// The alias opens the profile when there is one, else the flag does
			command := "icebath -p " + canon
			if aliased {
				command = canon
			}
			say(out, "\nWhat to do next:")
			say(out, "  Run %s and type /providers to set API keys and the model.", command)
			say(out, "  Run %s chat to start a session.", command)
			if cloned || made.GetKeys() {
				say(out, "\n  To use other API keys, edit %s/.env.", display)
				say(out, "  To give the agent another personality, edit %s/SOUL.md.", display)
			} else {
				say(out, "\n  ⚠ This profile has no API keys yet. Run %s and open /providers first, or it uses the keys set in your shell.",
					command)
				say(out, "  To give the agent a personality of its own here, edit %s/SOUL.md.", display)
			}
			say(out, "")
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&startModel, "model", "", "Model the profile starts with, together with --provider")
	f.StringVar(&startProvider, "provider", "", "Provider that the --model model belongs to")
	f.BoolVar(&clone, "clone", false, "Copy settings, keys, SOUL.md and skills from the profile in use")
	f.BoolVar(&all, "clone-all", false, "Copy every file except sessions, scheduled jobs, backups and checkpoints")
	f.StringVar(&from, "clone-from", "", "Profile to copy from, instead of the one in use")
	f.BoolVar(&channels, "clone-channels", false, "Also copy the chat app channels of the profile copied from")
	f.BoolVar(&syncImports, "sync-imports", false, "Also copy the record of agents that the copied profile imported from")
	f.BoolVar(&noAlias, "no-alias", false, "Make no alias command for the profile")
	f.BoolVar(&isolated, "isolated-auth", false, "Copy the sign-ins of the profile in use once, as create always does")
	f.BoolVar(&noCredentials, "no-credentials", false, "Take no API keys, sign-ins or model from the profile in use or the one copied")
	f.BoolVar(&noSkills, "no-skills", false, "Copy no built-in skills into the profile")
	f.StringVar(&description, "description", "", "What the profile is for")
	return cmd
}

// Plugins a clone carried, by folder
func clonedPlugins(dir string) []string {
	entries, err := os.ReadDir(filepath.Join(dir, "plugins"))
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".") && (entry.IsDir() || entry.Type()&fs.ModeSymlink != 0) {
			out = append(out, entry.Name())
		}
	}
	return out
}

// Says what an act did, each line marked done, then what failed, each marked so; empty lines are left out
func report(out io.Writer, done, failed []string) {
	for _, line := range done {
		if line != "" {
			say(out, "✓ %s", line)
		}
	}
	for _, line := range failed {
		if line != "" {
			say(out, "⚠ %s", line)
		}
	}
}

// Writes the alias of a new profile and says where, why none was made, or that its folder is off the PATH; whether the alias opens it now
func aliasCreated(out io.Writer, name string) bool {
	collision, err := profile.Collision(name)
	if err != nil {
		say(out, "⚠ No alias was made: %s", strs.Sentence(err.Error()))
		return false
	}
	if collision != "" {
		say(out, "\n⚠ No alias named %s was made. %s", name, strs.Sentence(collision))
		say(out, "  To give it another name, run icebath profile alias %s --name NAME.", name)
		say(out, "  Or open the profile with icebath -p %s chat.", name)
		return false
	}
	path, err := profile.MakeAlias(name, "")
	if err != nil {
		say(out, "⚠ %s", strs.Sentence(err.Error()))
		return false
	}
	say(out, "✓ Made the alias %s, a command that opens this profile.", path)
	onPath, err := profile.AliasesOnPath()
	if err != nil || onPath {
		return err == nil
	}
	dir, _ := profile.AliasDir()
	say(out, "\n⚠ %s is not on your PATH, so the alias does not work yet.", dir)
	say(out, "  Add this line to the startup file of your shell, such as ~/.bashrc or ~/.zshrc:")
	say(out, `    export PATH="$HOME/.local/bin:$PATH"`)
	return false
}

func (c *commandSet) delete() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <profile_name>",
		Short: "Delete a profile",
		Long:  "Deletes the profile with its settings, keys, sessions and skills, after asking. This cannot be undone.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			canon, err := profile.Normalize(args[0])
			if err != nil {
				return fail(out, err)
			}
			if canon == profile.Default {
				if _, err := c.service.Delete(cmd.Context(), &profilev1.DeleteRequest{Name: canon}); err != nil {
					return fail(out, err)
				}
				return nil
			}
			shown, err := c.service.Show(cmd.Context(), &profilev1.ShowRequest{Name: canon})
			if err != nil {
				return fail(out, err)
			}
			p := shown.GetProfile()
			wrapper, _ := profile.OurAlias(p.GetName())
			facts := [][2]string{{"Profile", p.GetName()}, {"Folder", p.GetPath()}}
			if m := model(p); m != "" {
				facts = append(facts, [2]string{"Model", m})
			}
			if p.GetSkills() > 0 {
				facts = append(facts, [2]string{"Skills", strs.Count(int(p.GetSkills()), "skill")})
			}
			if p.GetDistribution() != "" {
				facts = append(facts, [2]string{"Distribution", distribution(p.GetDistribution(), p.GetDistributionVersion())})
				if p.GetDistributionSource() != "" {
					facts = append(facts, [2]string{"Installed from", p.GetDistributionSource()})
				}
			}
			say(out, "")
			for _, line := range rows(facts) {
				say(out, "%s", line)
			}
			say(out, "\nDeleting the profile removes, for good:")
			say(out, "  Its settings, API keys, memories, sessions, skills and scheduled jobs")
			if wrapper != "" {
				say(out, "  Its alias %s", wrapper)
			}
			if !yes {
				console := cli.NewConsole(cmd)
				say(out, "")
				typed, ok := console.Input(fmt.Sprintf("Type %s to delete it for good, or press enter to keep it: ", p.GetName()))
				if !ok {
					say(out, "")
				}
				if !ok || strings.TrimSpace(typed) != p.GetName() {
					say(out, "Nothing was deleted.")
					return nil
				}
			}
			done, err := c.service.Delete(cmd.Context(), &profilev1.DeleteRequest{Name: p.GetName()})
			if err != nil {
				return fail(out, err)
			}
			gone, kept := profile.DropAlias(p.GetName())
			report(out, append(done.GetDone(), gone), append(done.GetFailed(), kept))
			if done.GetFailure() != "" {
				say(out, "✗ %s Remove it yourself.", strs.Sentence(done.GetFailure()))
				return cli.Exit(1)
			}
			say(out, "\n✓ Deleted the profile %s.", p.GetName())
			if !done.GetSettled() {
				say(out, "✗ Some of what icebath keeps of %s elsewhere is left. Run icebath profile purge-identity %s to finish.", p.GetName(), p.GetName())
				return cli.Exit(1)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Delete without asking first")
	return cmd
}

func (c *commandSet) describe() *cobra.Command {
	var text string
	var auto, overwrite, all, clear bool
	cmd := &cobra.Command{
		Use:   "describe [profile_name]",
		Short: "Show or set what a profile is for",
		Long: "Shows the description of a profile, sets it with --text, or takes it away with --clear. --auto has the helper model write one " +
			"from the profile's skills and model. The kanban board reads these descriptions to decide which profile takes a task.",
		Example: "  icebath profile describe work --text \"Backend work in Go\"\n  icebath profile describe --auto --all\n" +
			"  icebath profile describe work --clear",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, errw := cmd.OutOrStdout(), cmd.ErrOrStderr()
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			// The service takes a description away when it is replaced by no text, so --overwrite never goes out alone
			switch {
			case clear && (text != "" || auto || all || overwrite):
				fmt.Fprintln(errw, "✗ icebath profile describe takes --clear with a profile name and nothing else.")
				return cli.Exit(2)
			case overwrite && !auto && text == "":
				fmt.Fprintln(errw, "✗ icebath profile describe needs --auto with --overwrite.")
				return cli.Exit(2)
			}
			described, err := c.service.Describe(cmd.Context(), &profilev1.DescribeRequest{Name: name, Text: text, Auto: auto,
				Overwrite: overwrite || clear, All: all})
			if err != nil {
				if failure, ok := errors.AsType[*connect.Error](err); ok && failure.Code() == connect.CodeInvalidArgument &&
					strings.HasPrefix(failure.Message(), "icebath profile describe ") {
					fmt.Fprintln(errw, "✗ "+failure.Message())
					return cli.Exit(2)
				}
				fmt.Fprintln(errw, "✗ "+strs.Sentence(reason(err)))
				return cli.Exit(1)
			}
			switch {
			case clear:
				say(out, "✓ %s has no description now.", name)
				return nil
			case text == "" && !auto:
				if described.GetDescription() == "" {
					say(out, "%s has no description yet. Set one with icebath profile describe %s --text TEXT, or have one written with --auto.", name, name)
				} else {
					say(out, "%s", described.GetDescription())
					if described.GetDescriptionAuto() {
						say(out, "The helper model wrote this description.")
					}
				}
				return nil
			case text != "":
				say(out, "✓ Saved the description of %s.", name)
				return nil
			}
			if all && len(described.GetOutcomes()) == 0 {
				say(out, "Every profile has a description already.")
				return nil
			}
			ok := 0
			for _, outcome := range described.GetOutcomes() {
				if !outcome.GetOk() {
					fmt.Fprintf(errw, "✗ No description was written for %s. %s\n", outcome.GetName(), strs.Sentence(outcome.GetReason()))
					continue
				}
				ok++
				shown := outcome.GetDescription()
				if shown == "" {
					shown = "No description"
				}
				say(out, "✓ %s: %s", outcome.GetName(), shown)
			}
			if all && ok > 0 || !all && ok == 1 {
				return nil
			}
			return cli.Exit(1)
		},
	}
	f := cmd.Flags()
	f.StringVar(&text, "text", "", "Description to keep, as written")
	f.BoolVar(&auto, "auto", false, "Have the helper model write the description")
	f.BoolVar(&overwrite, "overwrite", false, "Replace a description that a person wrote")
	f.BoolVar(&all, "all", false, "With --auto, describe every profile that has no description from a person")
	f.BoolVar(&clear, "clear", false, "Take the description away")
	return cmd
}

func (c *commandSet) show(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	shown, err := c.service.Show(cmd.Context(), &profilev1.ShowRequest{Name: args[0]})
	if err != nil {
		if failure, ok := errors.AsType[*connect.Error](err); ok && failure.Code() == connect.CodeNotFound {
			say(out, "✗ There is no profile named %s. Run icebath profile list to see your profiles.", args[0])
			return cli.Exit(1)
		}
		return fail(out, err)
	}
	for _, line := range ShowLines(shown.GetProfile()) {
		say(out, "%s", line)
	}
	return nil
}

// Lines of what a profile is, which the command prints and the profiles screen shows
func ShowLines(p *profilev1.Profile) []string {
	facts := [][2]string{{"Profile", profile.Label(p.GetName(), p.GetDisplayName())}, {"Folder", p.GetPath()}}
	if m := model(p); m != "" {
		facts = append(facts, [2]string{"Model", m})
	}
	facts = append(facts, [2]string{"Skills", strs.Count(int(p.GetSkills()), "skill")}, [2]string{"Keys file", exists(p.GetHasEnv())},
		[2]string{"SOUL.md", exists(p.GetHasSoul())})
	if p.GetDistribution() != "" {
		facts = append(facts, [2]string{"Distribution", distribution(p.GetDistribution(), p.GetDistributionVersion())})
		if p.GetDistributionSource() != "" {
			facts = append(facts, [2]string{"Installed from", p.GetDistributionSource()})
		}
	}
	if alias, _ := profile.AliasOf(p.GetName()); alias != "" {
		path, _ := profile.AliasPath(alias)
		facts = append(facts, [2]string{"Alias", alias + ", which runs icebath -p " + p.GetName() + ", in " + path})
	}
	lines := append([]string{""}, rows(facts)...)
	if p.GetDistribution() != "" {
		lines = append(lines, "Run icebath profile info "+p.GetName()+" to see everything its distribution says.")
	}
	return append(lines, "")
}

func (c *commandSet) alias() *cobra.Command {
	var remove bool
	var aliasName string
	cmd := &cobra.Command{
		Use:   "alias <profile_name>",
		Short: "Make a command that opens icebath on a profile",
		Long: "Makes a small command named after the profile, such as work, that runs icebath on that profile. --name gives the command " +
			"another name, and --remove deletes it.",
		Example: "  icebath profile alias work\n  icebath profile alias work --remove",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			name := args[0]
			alias := aliasName
			if alias == "" {
				alias = name
			}
			if err := profile.ValidAlias(alias); err != nil {
				return fail(out, err)
			}
			_, err := c.service.Show(cmd.Context(), &profilev1.ShowRequest{Name: name})
			present := err == nil
			if remove {
				if aliasName != "" && !present {
					say(out, "✗ There is no profile named %s. Run icebath profile list to see your profiles.", name)
					return cli.Exit(1)
				}
				removed, err := profile.RemoveAlias(alias)
				switch {
				case err != nil:
					return err
				case removed:
					say(out, "✓ Removed the alias %s.", alias)
				case !present:
					say(out, "✗ There is no alias named %s and no profile named %s.", alias, name)
					return cli.Exit(1)
				default:
					say(out, "There is no alias named %s to remove.", alias)
				}
				return nil
			}
			if !present {
				say(out, "✗ There is no profile named %s. Run icebath profile list to see your profiles.", name)
				return cli.Exit(1)
			}
			collision, err := profile.Collision(alias)
			if err != nil {
				return err
			}
			if collision != "" {
				say(out, "✗ No alias named %s was made. %s", alias, strs.Sentence(collision))
				return cli.Exit(1)
			}
			target := ""
			if aliasName != "" {
				target = name
			}
			path, err := profile.MakeAlias(alias, target)
			if err != nil {
				say(out, "⚠ %s", strs.Sentence(err.Error()))
				return nil
			}
			say(out, "✓ Made the alias %s, a command that opens icebath on %s.", path, name)
			if onPath, err := profile.AliasesOnPath(); err == nil && !onPath {
				dir, _ := profile.AliasDir()
				say(out, "⚠ %s is not on your PATH, so the alias does not work yet.", dir)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&remove, "remove", false, "Remove the alias command instead")
	cmd.Flags().StringVar(&aliasName, "name", "", "Name of the alias command, instead of the profile's name")
	return cmd
}

func (c *commandSet) rename(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	done, err := c.service.Rename(cmd.Context(), &profilev1.RenameRequest{Old: args[0], New: args[1]})
	if err != nil {
		return fail(out, err)
	}
	// The default profile keeps its name: it shows the name given, or default again for none
	canon, _ := profile.Normalize(args[0])
	switch {
	case canon == profile.Default && done.GetName() == profile.Default:
		say(out, "✓ The default profile shows as default again.")
		return nil
	case canon == profile.Default:
		say(out, "✓ The default profile now shows as %s. Its name stays default.", done.GetName())
		return nil
	}
	renamed := done.GetName()
	followed, broken := profile.FollowAlias(canon, renamed)
	report(out, append(done.GetDone(), followed), append(done.GetFailed(), broken))
	if !done.GetSettled() {
		say(out, "Finish the rename with:\n    icebath profile migrate-identity %s %s", canon, renamed)
	}
	say(out, "\n✓ Renamed the profile %s to %s.", canon, renamed)
	say(out, "Its folder is %s.\n", done.GetPath())
	return nil
}

func (c *commandSet) purge(cmd *cobra.Command, args []string) error {
	out, errw := cmd.OutOrStdout(), cmd.ErrOrStderr()
	done, err := c.service.PurgeIdentity(cmd.Context(), &profilev1.PurgeIdentityRequest{Name: args[0]})
	if err != nil {
		return fail(out, err)
	}
	for _, line := range done.GetFailed() {
		say(errw, "⚠ %s", line)
	}
	if !done.GetSettled() {
		say(errw, "✗ Some of what icebath keeps of the profile is still there. Try again with:\n    icebath profile purge-identity %s", args[0])
		return cli.Exit(1)
	}
	say(out, "✓ Removed what icebath kept of %s elsewhere.", args[0])
	return nil
}

func (c *commandSet) migrate(cmd *cobra.Command, args []string) error {
	out, errw := cmd.OutOrStdout(), cmd.ErrOrStderr()
	done, err := c.service.MigrateIdentity(cmd.Context(), &profilev1.MigrateIdentityRequest{Old: args[0], New: args[1]})
	if err != nil {
		return fail(out, err)
	}
	for _, line := range done.GetDone() {
		say(out, "✓ %s", line)
	}
	for _, line := range done.GetFailed() {
		say(errw, "⚠ %s", line)
	}
	if !done.GetSettled() {
		say(errw, "✗ Some of what icebath keeps under the old name did not move. Try again with:\n    icebath profile migrate-identity %s %s",
			args[0], args[1])
		return cli.Exit(1)
	}
	say(out, "✓ Moved what icebath kept under %s to %s.", args[0], args[1])
	return nil
}

// Writes a profile's archive where asked, else in the folder exports go to, and says where
func Export(cmd *cobra.Command, service profilev1connect.ProfileServiceClient, name, output string) (string, string, error) {
	exported, err := service.Export(cmd.Context(), &profilev1.ExportRequest{Name: name})
	if err != nil {
		return "", "", err
	}
	path, err := profile.SaveArchive(exported.GetArchive(), exported.GetFileName(), output)
	return exported.GetName(), path, err
}

func (c *commandSet) export() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:     "export <profile_name>",
		Short:   "Save a profile to an archive you can share",
		Long:    "Writes the profile to a .tar.gz archive that you can share and bring back with import.",
		Example: "  icebath profile export work -o work.tar.gz",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, path, err := Export(cmd, c.service, args[0], output)
			if err != nil {
				return fail(cmd.OutOrStdout(), err)
			}
			say(cmd.OutOrStdout(), "✓ Saved the profile %s to %s.", name, path)
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "File to write the archive to")
	return cmd
}

func (c *commandSet) importArchive() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:     "import <archive>",
		Short:   "Make a profile from an archive",
		Long:    "Creates a profile from an archive that export wrote, with an alias command when its name is free.",
		Example: "  icebath profile import work.tar.gz --name work-copy",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			data, err := os.ReadFile(fsx.ExpandHome(args[0]))
			if errors.Is(err, fs.ErrNotExist) {
				return fail(out, errors.New("There is no file at "+args[0]+"."))
			}
			if err != nil {
				return fail(out, err)
			}
			imported, err := c.service.Import(cmd.Context(), &profilev1.ImportRequest{Archive: data, Name: name})
			if err != nil {
				return fail(out, err)
			}
			say(out, "✓ Imported the profile %s into %s.", imported.GetName(), imported.GetPath())
			if collision, err := profile.Collision(imported.GetName()); err == nil && collision == "" {
				if path, err := profile.MakeAlias(imported.GetName(), ""); err == nil {
					say(out, "  Made the alias %s, a command that opens this profile.", path)
				} else {
					say(out, "⚠ %s", strs.Sentence(err.Error()))
				}
			}
			say(out, "")
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Name of the new profile, instead of the archive's")
	return cmd
}

// Source of a distribution as the server takes it: a git address as given, a local folder made absolute
func source(raw string) string {
	trimmed := strings.TrimSpace(raw)
	expanded := fsx.ExpandHome(trimmed)
	if fsx.IsDir(expanded) {
		if abs, err := filepath.Abs(expanded); err == nil {
			return abs
		}
	}
	return trimmed
}

// Lines of what an install would do, which the command prints and the profiles screen shows
func PlanLines(plan *profilev1.Plan) []string {
	m := plan.GetManifest()
	lines := []string{"", "Distribution: " + distribution(m.GetName(), m.GetVersion())}
	if m.GetDescription() != "" {
		lines = append(lines, "  "+m.GetDescription())
	}
	facts := [][2]string{}
	if m.GetAuthor() != "" {
		facts = append(facts, [2]string{"Author", m.GetAuthor()})
	}
	if m.GetRequires() != "" {
		facts = append(facts, [2]string{"Needs", "icebath " + m.GetRequires()})
	}
	facts = append(facts, [2]string{"Source", plan.GetProvenance()}, [2]string{"Folder", plan.GetTarget()})
	for _, line := range rows(facts) {
		lines = append(lines, "  "+line)
	}
	if plan.GetExisting() {
		if plan.GetDistribution() {
			lines = append(lines, "  The profile exists already, and only the files the distribution owns are replaced.")
		} else {
			lines = append(lines, "  ⚠ The profile exists already and was not installed from a distribution.",
				"    Installing here replaces its SOUL.md and mcp.json, and every skill or scheduled job",
				"    that has the name of one the distribution brings.", "    Your memories, sessions, sign-ins in auth.json and keys file stay,",
				"    but your own changes to files the distribution owns are lost.")
		}
		// An install writes the distribution's settings over the profile's; only an update leaves them
		if !plan.GetPreservesConfig() {
			lines = append(lines, "  Its settings file config.yaml is replaced with the distribution's when it brings one.")
		}
	}
	if len(m.GetEnv()) > 0 {
		lines = append(lines, "", "  Variables:")
		for _, r := range m.GetEnv() {
			words := "Optional"
			if r.GetRequired() {
				words = "Required"
			}
			switch {
			case r.GetSet():
				words += ", set already"
			case r.GetRequired():
				words += ", not set yet"
			}
			line := "    " + r.GetName() + ": " + words + "."
			if r.GetDescription() != "" {
				line += " " + strs.Sentence(r.GetDescription())
			}
			lines = append(lines, line)
		}
	}
	if plan.GetHasCron() {
		lines = append(lines, "", "  ⚠ This distribution brings scheduled jobs in cron/jobs.json, and this build of icebath does not run them.")
	}
	return lines
}

func (c *commandSet) install() *cobra.Command {
	var name string
	var alias, force, yes bool
	cmd := &cobra.Command{
		Use:   "install <source>",
		Short: "Install a ready-made profile from git or a folder",
		Long: "Installs a profile distribution, a ready-made profile kept in a git repository or a folder. install shows what it will set " +
			"up and asks first.",
		Example: "  icebath profile install https://github.com/you/research-profile\n  icebath profile install ./research-profile --alias",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			from := source(args[0])
			planned, err := c.service.Plan(cmd.Context(), &profilev1.PlanRequest{Source: from, Name: name})
			if err != nil {
				return fail(out, err)
			}
			for _, line := range PlanLines(planned.GetPlan()) {
				say(out, "%s", line)
			}
			question := fmt.Sprintf("\nInstall the profile %s from this distribution? [y/N] ", planned.GetPlan().GetManifest().GetName())
			if !yes && !cli.NewConsole(cmd).Agree(question, false) {
				say(out, "Nothing was installed.")
				return nil
			}
			installed, err := c.service.Install(cmd.Context(), &profilev1.InstallRequest{Source: from, Name: name, Force: force})
			if err != nil {
				return fail(out, err)
			}
			plan := installed.GetPlan()
			m := plan.GetManifest()
			if alias {
				if collision, err := profile.Collision(m.GetName()); err == nil && collision == "" {
					if _, err := profile.MakeAlias(m.GetName(), ""); err != nil {
						say(out, "⚠ %s", strs.Sentence(err.Error()))
					}
				}
			}
			say(out, "\n✓ Installed the profile %s, version %s.", m.GetName(), m.GetVersion())
			say(out, "  Its folder is %s.", plan.GetTarget())
			if len(m.GetEnv()) > 0 {
				say(out, "  Next, copy .env.EXAMPLE to .env in that folder and fill in the required keys:\n    %s/.env.EXAMPLE", plan.GetTarget())
			}
			if plan.GetHasCron() {
				say(out, "  It brought scheduled jobs in %s/cron/jobs.json, and this build of icebath does not run them.", plan.GetTarget())
			}
			say(out, "\n  Open it with icebath -p %s chat.", m.GetName())
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "Name of the profile, instead of the distribution's")
	f.BoolVar(&alias, "alias", false, "Make an alias command for the profile")
	f.BoolVar(&force, "force", false, "Install over a profile that has this name")
	f.BoolVarP(&yes, "yes", "y", false, "Install without asking first")
	return cmd
}

func (c *commandSet) update() *cobra.Command {
	var forceConfig, yes bool
	cmd := &cobra.Command{
		Use:   "update <profile_name>",
		Short: "Update an installed profile from where it came from",
		Long: "Pulls the distribution that the profile was installed from again and applies its changes. Your sessions, keys and other data " +
			"stay. Your settings file stays too, unless you pass --force-config.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			canon, err := profile.Normalize(args[0])
			if err != nil {
				return fail(out, err)
			}
			current, err := c.service.Info(cmd.Context(), &profilev1.InfoRequest{Name: canon})
			if err != nil {
				return fail(out, err)
			}
			if !current.GetFound() {
				say(out, "✗ %s was not installed from a distribution, because it has no distribution.yaml. Only a profile installed with "+
					"icebath profile install can be updated.", canon)
				return cli.Exit(1)
			}
			if !yes {
				from := current.GetManifest().GetSource()
				if from == "" {
					from = "a source icebath did not record"
				}
				say(out, "\nUpdating %s pulls it again from %s.", canon, from)
				say(out, "  It is at version %s now.", current.GetManifest().GetVersion())
				if forceConfig {
					say(out, "  Because you passed --force-config, its settings file config.yaml is replaced.")
				} else {
					say(out, "  Its settings file config.yaml stays as it is. --force-config would replace it.")
				}
				say(out, "  Its memories, sessions, sign-ins and keys file are not touched.")
				if !cli.NewConsole(cmd).Agree(fmt.Sprintf("\nUpdate the profile %s now? [y/N] ", canon), false) {
					say(out, "Nothing was updated.")
					return nil
				}
			}
			updated, err := c.service.Update(cmd.Context(), &profilev1.UpdateRequest{Name: canon, ForceConfig: forceConfig})
			if err != nil {
				return fail(out, err)
			}
			m := updated.GetPlan().GetManifest()
			say(out, "\n✓ Updated %s to version %s.", m.GetName(), m.GetVersion())
			if updated.GetPlan().GetPreservesConfig() {
				say(out, "  Its settings file config.yaml was left as it is.")
			} else {
				say(out, "  Its settings file config.yaml was replaced with the distribution's.")
			}
			if updated.GetPlan().GetHasCron() {
				say(out, "  Its scheduled jobs in cron/jobs.json were updated too, and this build of icebath does not run them.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&forceConfig, "force-config", false, "Replace the settings file with the distribution's")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Update without asking first")
	return cmd
}

func (c *commandSet) info(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	found, err := c.service.Info(cmd.Context(), &profilev1.InfoRequest{Name: args[0]})
	if err != nil {
		return fail(out, err)
	}
	if !found.GetFound() {
		say(out, "%s was not installed from a distribution, because it has no distribution.yaml.", args[0])
		return nil
	}
	for _, line := range InfoLines(found.GetManifest()) {
		say(out, "%s", line)
	}
	return nil
}

// Lines of a distribution's manifest, which the command prints and the profiles screen shows
func InfoLines(m *profilev1.Manifest) []string {
	version := m.GetVersion()
	if version == "" {
		version = "Unknown"
	}
	facts := [][2]string{{"Distribution", m.GetName()}, {"Version", version}}
	for _, row := range [][2]string{{"Description", m.GetDescription()}, {"Author", m.GetAuthor()}, {"License", m.GetLicense()},
		{"Needs", m.GetRequires()}, {"Source", m.GetSource()}, {"Installed", m.GetInstalledAt()}, {"Owns", strings.Join(m.GetOwned(), ", ")}} {
		if row[1] != "" {
			if row[0] == "Needs" {
				row[1] = "icebath " + row[1]
			}
			facts = append(facts, row)
		}
	}
	lines := append([]string{""}, rows(facts)...)
	if len(m.GetEnv()) > 0 {
		lines = append(lines, "", "Variables:")
		for _, r := range m.GetEnv() {
			line := "  " + r.GetName() + ": Optional."
			if r.GetRequired() {
				line = "  " + r.GetName() + ": Required."
			}
			if r.GetDescription() != "" {
				line += " " + strs.Sentence(r.GetDescription())
			}
			lines = append(lines, line)
			if r.GetDefault() != "" {
				lines = append(lines, "      Default: "+r.GetDefault())
			}
		}
	}
	return append(lines, "")
}

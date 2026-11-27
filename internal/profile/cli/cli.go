package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/pkg/fsx"
	profilev1 "github.com/nickheyer/icebath/pkg/gen/icebath/profile/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/profile/v1/profilev1connect"
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
				fmt.Fprintln(w, "Error: "+profile.Invalid(args[i+1]).Error()+"\nRun `icebath profile list` to see your profiles.")
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

// Prints Error: and why to the output and ends the command with status 1
func fail(w io.Writer, err error) error {
	fmt.Fprintln(w, "Error: "+reason(err))
	return cli.Exit(1)
}

func say(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, format+"\n", args...)
}

func model(p *profilev1.Profile) string {
	if p.GetModel() == "" {
		return ""
	}
	if p.GetProvider() != "" {
		return p.GetModel() + " (" + p.GetProvider() + ")"
	}
	return p.GetModel()
}

func exists(present bool) string {
	if present {
		return "exists"
	}
	return "not configured"
}

// Text cut to n characters
func first(text string, n int) string {
	runes := []rune(text)
	return string(runes[:min(n, len(runes))])
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := profilev1connect.NewProfileServiceClient(conn)
	c := &commandSet{service: service}
	profiles := &cobra.Command{
		Use:   "profiles",
		Short: "List all profiles",
		Args:  cobra.NoArgs,
		RunE:  c.list,
	}
	root := &cobra.Command{
		Use:   "profile",
		Short: "Manage profiles — multiple isolated Icebath instances",
		Args:  cobra.NoArgs,
		RunE:  c.status,
	}
	root.AddCommand(&cobra.Command{Use: "list", Short: "List all profiles", Args: cobra.NoArgs, RunE: c.list},
		&cobra.Command{Use: "use <profile_name>", Short: "Set sticky default profile", Args: cobra.ExactArgs(1), RunE: c.use},
		c.create(), c.delete(), c.describe(),
		&cobra.Command{Use: "show <profile_name>", Short: "Show profile details", Args: cobra.ExactArgs(1), RunE: c.show},
		c.alias(),
		&cobra.Command{Use: "rename <old_name> <new_name>", Short: "Rename a profile ('default': sets a display name; id unchanged)",
			Args: cobra.ExactArgs(2), RunE: c.rename},
		&cobra.Command{Use: "purge-identity <profile_name>", Short: "Retry a deleted profile's session/routing identity purge", Args: cobra.ExactArgs(1),
			RunE: c.purge},
		&cobra.Command{Use: "migrate-identity <old_name> <new_name>", Short: "Retry a renamed profile's session/routing identity migration",
			Args: cobra.ExactArgs(2), RunE: c.migrate},
		c.export(), c.importArchive(), c.install(), c.update(),
		&cobra.Command{Use: "info <profile_name>", Short: "Show a profile's distribution manifest (version, requirements, source)",
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
		say(out, "\nActive profile: %s", profile.Label(p.GetName(), p.GetDisplayName()))
		say(out, "Path:           %s", profile.Tilde(p.GetPath()))
		if m := model(p); m != "" {
			say(out, "Model:          %s", m)
		}
		say(out, "Skills:         %d installed", p.GetSkills())
		if alias, _ := profile.AliasOf(p.GetName()); alias != "" && !p.GetDefault() {
			if path, _ := profile.OurAlias(alias); path != "" {
				say(out, "Alias:          %s → icebath -p %s", alias, p.GetName())
			}
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
		say(out, "No profiles found.")
		return nil
	}
	aliases, _ := profile.Aliases()
	say(out, "\n %s %s %s %s", cli.Pad("Profile", 16), cli.Pad("Model", 28), cli.Pad("Alias", 12), "Distribution")
	say(out, " %s    %s    %s    %s", strings.Repeat("─", 15), strings.Repeat("─", 27), strings.Repeat("─", 11), strings.Repeat("─", 20))
	for _, p := range listed.GetProfiles() {
		marker := "  "
		if p.GetActive() {
			marker = " ◆"
		}
		alias := "—"
		if name := aliases[p.GetName()]; name != "" && !p.GetDefault() {
			alias = name
		}
		dist := "—"
		if p.GetDistribution() != "" {
			dist = first(p.GetDistribution()+"@"+orUnknown(p.GetDistributionVersion()), 30)
		}
		shownModel := "—"
		if p.GetModel() != "" {
			shownModel = first(p.GetModel(), 26)
		}
		say(out, "%s%s %s %s %s", marker, cli.Pad(profile.Label(p.GetName(), p.GetDisplayName()), 15), cli.Pad(shownModel, 28), cli.Pad(alias, 12), dist)
	}
	say(out, "")
	return nil
}

func orUnknown(text string) string {
	if text == "" {
		return "?"
	}
	return text
}

func (c *commandSet) use(cmd *cobra.Command, args []string) error {
	used, err := c.service.Use(cmd.Context(), &profilev1.UseRequest{Name: args[0]})
	if err != nil {
		return fail(cmd.OutOrStdout(), err)
	}
	if used.GetProfile().GetDefault() {
		say(cmd.OutOrStdout(), "Switched to: default (%s)", profile.Tilde(used.GetProfile().GetPath()))
	} else {
		say(cmd.OutOrStdout(), "Switched to: %s", used.GetProfile().GetName())
	}
	return nil
}

func (c *commandSet) create() *cobra.Command {
	var clone, all, channels, syncImports, noAlias, isolated, noCredentials, noSkills bool
	var from, description, startModel, startProvider string
	cmd := &cobra.Command{
		Use:   "create <profile_name>",
		Short: "Create a new profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			name := args[0]
			if cmd.Flags().Changed("clone-from") && strings.TrimSpace(from) == "" {
				return fail(out, errors.New("profile name cannot be empty"))
			}
			cloned := clone || cmd.Flags().Changed("clone-from") || all
			if cloned && noCredentials {
				fmt.Fprintln(cmd.ErrOrStderr(), "Error: --no-credentials cannot be combined with --clone / --clone-from / --clone-all (a clone copies the "+
					"source's keys); create without cloning instead.")
				return cli.Exit(1)
			}
			if (startModel == "") != (startProvider == "") {
				return fail(out, errors.New("--model and --provider go together: a model is pinned with the provider that serves it."))
			}
			made, err := c.service.Create(cmd.Context(), &profilev1.CreateRequest{Name: name, Clone: clone, From: from, All: all, Channels: channels,
				SyncImports: syncImports, NoSkills: noSkills, Description: description, NoCredentials: noCredentials, Model: startModel,
				Provider: startProvider})
			if err != nil {
				return fail(out, err)
			}
			say(out, "\nProfile '%s' created at %s", name, made.GetPath())
			if cloned {
				if all {
					say(out, "Full copy from %s (excluding session history, cron jobs, backups, and snapshots).", made.GetSource())
				} else {
					say(out, "Cloned config, .env, SOUL.md, and skills from %s.", made.GetSource())
					if plugins := clonedPlugins(made.GetPath()); len(plugins) > 0 {
						say(out, "Cloned installed plugins too: %s.", strings.Join(plugins, ", "))
					}
				}
				if syncImports {
					say(out, "Import sources carried over — `icebath -p %s import-agent --sync` keeps pulling the same Claude Code / Codex trees.", name)
				}
			}
			for _, line := range made.GetSeeded() {
				say(out, "%s", line)
			}
			canon := made.GetProfile().GetName()
			aliased := !noAlias && aliasCreated(out, canon)
			if made.GetKeys() {
				say(out, "Copied API keys from '%s'.", made.GetLaunch())
			}
			for _, note := range made.GetNotes() {
				say(out, "%s", note)
			}
			for _, warning := range made.GetWarnings() {
				say(out, "⚠ %s", warning)
			}
			display := profile.Tilde(made.GetPath())
			// The alias opens the profile when there is one, else the flag does
			command := "icebath -p " + canon
			if aliased {
				command = canon
			}
			say(out, "\nNext steps:")
			say(out, "  %s, type /providers   Configure API keys and model", command)
			say(out, "  %s chat              Start chatting", command)
			if cloned || made.GetKeys() {
				say(out, "\n  Edit %s/.env for different API keys", display)
				say(out, "  Edit %s/SOUL.md for different personality", display)
			} else {
				say(out, "\n  ⚠ This profile has no API keys yet. Start '%s' and open /providers first,", command)
				say(out, "    or it will inherit keys from your shell environment.")
				say(out, "  Edit %s/SOUL.md to customize personality", display)
			}
			say(out, "")
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&startModel, "model", "", "model the profile starts on, with --provider")
	f.StringVar(&startProvider, "provider", "", "provider of --model")
	f.BoolVar(&clone, "clone", false, "copy config, .env, SOUL.md and skills from the profile in use")
	f.BoolVar(&all, "clone-all", false, "copy every file of the source but its session history, cron jobs, backups and snapshots")
	f.StringVar(&from, "clone-from", "", "profile to clone from instead of the one in use")
	f.BoolVar(&channels, "clone-channels", false, "copy the source's messaging channels too")
	f.BoolVar(&syncImports, "sync-imports", false, "carry over where `import-agent --sync` pulls from")
	f.BoolVar(&noAlias, "no-alias", false, "make no alias command for the profile")
	f.BoolVar(&isolated, "isolated-auth", false, "copy the logins of the profile in use once, so each profile keeps its own (what every create does)")
	f.BoolVar(&noCredentials, "no-credentials", false, "take no keys, logins or model from the profile in use")
	f.BoolVar(&noSkills, "no-skills", false, "seed no bundled skills")
	f.StringVar(&description, "description", "", "what the profile is for")
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

// Writes the alias of a new profile and says where, why none was made, or that its folder is off the PATH; whether the alias opens it now
func aliasCreated(out io.Writer, name string) bool {
	collision, err := profile.Collision(name)
	if err != nil {
		say(out, "⚠ %v", err)
		return false
	}
	if collision != "" {
		say(out, "\n⚠ Cannot create alias '%s' — %s", name, collision)
		say(out, "  Choose a custom alias:  icebath profile alias %s --name <custom>", name)
		say(out, "  Or access via flag:     icebath -p %s chat", name)
		return false
	}
	path, err := profile.MakeAlias(name, "")
	if err != nil {
		say(out, "⚠ %v", err)
		return false
	}
	say(out, "Wrapper created: %s", path)
	onPath, err := profile.AliasesOnPath()
	if err != nil || onPath {
		return err == nil
	}
	dir, _ := profile.AliasDir()
	say(out, "\n⚠ %s is not in your PATH.", dir)
	say(out, "  Add to your shell config (~/.bashrc or ~/.zshrc):")
	say(out, `    export PATH="$HOME/.local/bin:$PATH"`)
	return false
}

func (c *commandSet) delete() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <profile_name>",
		Short: "Delete a profile",
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
			say(out, "\nProfile: %s", p.GetName())
			say(out, "Path:    %s", p.GetPath())
			if m := model(p); m != "" {
				say(out, "Model:   %s", m)
			}
			if p.GetSkills() > 0 {
				say(out, "Skills:  %d", p.GetSkills())
			}
			if p.GetDistribution() != "" {
				say(out, "Distribution: %s@%s", p.GetDistribution(), orUnknown(p.GetDistributionVersion()))
				if p.GetDistributionSource() != "" {
					say(out, "Installed from: %s", p.GetDistributionSource())
				}
			}
			say(out, "\nThis will permanently delete:")
			say(out, "  • All config, API keys, memories, sessions, skills, cron jobs")
			if wrapper != "" {
				say(out, "  • Command alias (%s)", wrapper)
			}
			if !yes {
				console := cli.NewConsole(cmd)
				say(out, "")
				typed, ok := console.Input(fmt.Sprintf("Type '%s' to confirm: ", p.GetName()))
				if !ok {
					say(out, "")
				}
				if !ok || strings.TrimSpace(typed) != p.GetName() {
					say(out, "Cancelled.")
					return nil
				}
			}
			done, err := c.service.Delete(cmd.Context(), &profilev1.DeleteRequest{Name: p.GetName()})
			if err != nil {
				return fail(out, err)
			}
			for _, line := range slices.Concat(done.GetLines(), profile.DropAlias(p.GetName()), done.GetLater()) {
				say(out, "%s", line)
			}
			if done.GetFailure() != "" {
				say(out, "Error: %s", done.GetFailure())
				return cli.Exit(1)
			}
			say(out, "\nProfile '%s' deleted.", p.GetName())
			if !done.GetSettled() {
				say(out, "Error: Profile '%s' was deleted, but its session/routing identity settlement is still pending — run: icebath profile "+
					"purge-identity %s", p.GetName(), p.GetName())
				return cli.Exit(1)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "delete without asking")
	return cmd
}

func (c *commandSet) describe() *cobra.Command {
	var text string
	var auto, overwrite, all bool
	cmd := &cobra.Command{
		Use:   "describe [profile_name]",
		Short: "Read or set a profile's description (used by the kanban orchestrator)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, errw := cmd.OutOrStdout(), cmd.ErrOrStderr()
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			described, err := c.service.Describe(cmd.Context(), &profilev1.DescribeRequest{Name: name, Text: text, Auto: auto, Overwrite: overwrite,
				All: all})
			if err != nil {
				if failure, ok := errors.AsType[*connect.Error](err); ok && failure.Code() == connect.CodeInvalidArgument &&
					strings.HasPrefix(failure.Message(), "profile describe:") {
					fmt.Fprintln(errw, failure.Message())
					return cli.Exit(2)
				}
				fmt.Fprintln(errw, "Error: "+reason(err))
				return cli.Exit(1)
			}
			switch {
			case text == "" && !auto:
				if described.GetDescription() == "" {
					say(out, "(no description set for '%s')", name)
				} else if described.GetDescriptionAuto() {
					say(out, "[auto] %s", described.GetDescription())
				} else {
					say(out, "%s", described.GetDescription())
				}
				return nil
			case text != "":
				say(out, "Description updated for '%s'.", name)
				return nil
			}
			if all && len(described.GetOutcomes()) == 0 {
				say(out, "All profiles already have descriptions.")
				return nil
			}
			ok := 0
			for _, outcome := range described.GetOutcomes() {
				if !outcome.GetOk() {
					fmt.Fprintf(errw, "profile describe %s: %s\n", outcome.GetName(), outcome.GetReason())
					continue
				}
				ok++
				shown := outcome.GetDescription()
				if shown == "" {
					shown = "(no description)"
				}
				say(out, "Described '%s': %s", outcome.GetName(), shown)
			}
			if all && ok > 0 || !all && ok == 1 {
				return nil
			}
			return cli.Exit(1)
		},
	}
	f := cmd.Flags()
	f.StringVar(&text, "text", "", "description to keep, as written")
	f.BoolVar(&auto, "auto", false, "have the helper model write it from the profile's skills and model")
	f.BoolVar(&overwrite, "overwrite", false, "replace a description a person wrote")
	f.BoolVar(&all, "all", false, "every profile without a description a person wrote (with --auto)")
	return cmd
}

func (c *commandSet) show(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	shown, err := c.service.Show(cmd.Context(), &profilev1.ShowRequest{Name: args[0]})
	if err != nil {
		if failure, ok := errors.AsType[*connect.Error](err); ok && failure.Code() == connect.CodeNotFound {
			say(out, "Error: Profile '%s' does not exist.", args[0])
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
	lines := []string{"", "Profile: " + profile.Label(p.GetName(), p.GetDisplayName()), "Path:    " + p.GetPath()}
	if m := model(p); m != "" {
		lines = append(lines, "Model:   "+m)
	}
	lines = append(lines, fmt.Sprintf("Skills:  %d", p.GetSkills()), ".env:    "+exists(p.GetHasEnv()), "SOUL.md: "+exists(p.GetHasSoul()))
	if p.GetDistribution() != "" {
		lines = append(lines, fmt.Sprintf("Distribution: %s@%s", p.GetDistribution(), orUnknown(p.GetDistributionVersion())))
		if p.GetDistributionSource() != "" {
			lines = append(lines, "Installed from: "+p.GetDistributionSource())
		}
		lines = append(lines, fmt.Sprintf("  (run `icebath profile info %s` for full manifest)", p.GetName()))
	}
	if alias, _ := profile.AliasOf(p.GetName()); alias != "" {
		path, _ := profile.AliasPath(alias)
		lines = append(lines, fmt.Sprintf("Alias:   %s → icebath -p %s  (%s)", alias, p.GetName(), path))
	}
	return append(lines, "")
}

func (c *commandSet) alias() *cobra.Command {
	var remove bool
	var aliasName string
	cmd := &cobra.Command{
		Use:   "alias <profile_name>",
		Short: "Manage wrapper scripts",
		Args:  cobra.ExactArgs(1),
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
					say(out, "Error: Profile '%s' does not exist.", name)
					return cli.Exit(1)
				}
				removed, err := profile.RemoveAlias(alias)
				switch {
				case err != nil:
					return err
				case removed:
					say(out, "✓ Removed alias '%s'", alias)
				case !present:
					say(out, "Error: No alias '%s' and no profile '%s'.", alias, name)
					return cli.Exit(1)
				default:
					say(out, "No alias '%s' found to remove.", alias)
				}
				return nil
			}
			if !present {
				say(out, "Error: Profile '%s' does not exist.", name)
				return cli.Exit(1)
			}
			collision, err := profile.Collision(alias)
			if err != nil {
				return err
			}
			if collision != "" {
				say(out, "Error: %s", collision)
				return cli.Exit(1)
			}
			target := ""
			if aliasName != "" {
				target = name
			}
			path, err := profile.MakeAlias(alias, target)
			if err != nil {
				say(out, "⚠ %v", err)
				return nil
			}
			say(out, "✓ Alias created: %s", path)
			if onPath, err := profile.AliasesOnPath(); err == nil && !onPath {
				dir, _ := profile.AliasDir()
				say(out, "⚠ %s is not in your PATH.", dir)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&remove, "remove", false, "remove the alias instead")
	cmd.Flags().StringVar(&aliasName, "name", "", "name of the alias, the profile's own when empty")
	return cmd
}

func (c *commandSet) rename(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	done, err := c.service.Rename(cmd.Context(), &profilev1.RenameRequest{Old: args[0], New: args[1]})
	if err != nil {
		return fail(out, err)
	}
	for _, line := range done.GetLines() {
		say(out, "%s", line)
	}
	canon, _ := profile.Normalize(args[0])
	if canon == profile.Default {
		return nil
	}
	renamed, _ := profile.Canon(args[1])
	for _, line := range append(profile.FollowAlias(canon, renamed), done.GetLater()...) {
		say(out, "%s", line)
	}
	if !done.GetSettled() {
		say(out, "Retry with:\n    icebath profile migrate-identity %s %s", canon, renamed)
	}
	say(out, "\nProfile renamed: %s → %s", args[0], args[1])
	say(out, "Path: %s\n", done.GetPath())
	return nil
}

func (c *commandSet) purge(cmd *cobra.Command, args []string) error {
	out, errw := cmd.OutOrStdout(), cmd.ErrOrStderr()
	done, err := c.service.PurgeIdentity(cmd.Context(), &profilev1.PurgeIdentityRequest{Name: args[0]})
	if err != nil {
		return fail(out, err)
	}
	for _, line := range done.GetLines() {
		say(errw, "%s", line)
	}
	if !done.GetSettled() {
		say(errw, "Error: session identity was not purged. Retry with:\n    icebath profile purge-identity %s", args[0])
		return cli.Exit(1)
	}
	say(out, "✓ Session/routing identity purged: %s", args[0])
	return nil
}

func (c *commandSet) migrate(cmd *cobra.Command, args []string) error {
	out, errw := cmd.OutOrStdout(), cmd.ErrOrStderr()
	done, err := c.service.MigrateIdentity(cmd.Context(), &profilev1.MigrateIdentityRequest{Old: args[0], New: args[1]})
	if err != nil {
		return fail(out, err)
	}
	for _, line := range done.GetLines() {
		if strings.HasPrefix(line, "⚠") {
			say(errw, "%s", line)
		} else {
			say(out, "%s", line)
		}
	}
	if !done.GetSettled() {
		say(errw, "Error: session identity was not migrated. Retry with:\n    icebath profile migrate-identity %s %s", args[0], args[1])
		return cli.Exit(1)
	}
	say(out, "✓ Session/routing identity migrated: %s → %s", args[0], args[1])
	return nil
}

// Writes a profile's archive where asked, else in the folder exports go to, and says where
func Export(cmd *cobra.Command, service profilev1connect.ProfileServiceClient, name, output string) (string, string, error) {
	exported, err := service.Export(cmd.Context(), &profilev1.ExportRequest{Name: name})
	if err != nil {
		return "", "", err
	}
	path, err := profile.SaveArchive(exported.GetArchive(), exported.GetName(), output)
	return exported.GetName(), path, err
}

func (c *commandSet) export() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "export <profile_name>",
		Short: "Export a profile to archive",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, path, err := Export(cmd, c.service, args[0], output)
			if err != nil {
				return fail(cmd.OutOrStdout(), err)
			}
			say(cmd.OutOrStdout(), "✓ Exported '%s' to %s", name, path)
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "file to write the archive to")
	return cmd
}

func (c *commandSet) importArchive() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "import <archive>",
		Short: "Import a profile from archive",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			data, err := os.ReadFile(fsx.ExpandHome(args[0]))
			if errors.Is(err, fs.ErrNotExist) {
				return fail(out, errors.New("Archive not found: "+args[0]))
			}
			if err != nil {
				return fail(out, err)
			}
			imported, err := c.service.Import(cmd.Context(), &profilev1.ImportRequest{Archive: data, Name: name})
			if err != nil {
				return fail(out, err)
			}
			say(out, "✓ Imported profile '%s' at %s", imported.GetName(), imported.GetPath())
			if collision, err := profile.Collision(imported.GetName()); err == nil && collision == "" {
				if path, err := profile.MakeAlias(imported.GetName(), ""); err == nil {
					say(out, "  Wrapper created: %s", path)
				} else {
					say(out, "⚠ %v", err)
				}
			}
			say(out, "")
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "name of the new profile, the archive's own when empty")
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
	lines := []string{"", fmt.Sprintf("Distribution: %s v%s", m.GetName(), m.GetVersion())}
	if m.GetDescription() != "" {
		lines = append(lines, "  "+m.GetDescription())
	}
	if m.GetAuthor() != "" {
		lines = append(lines, "  Author:   "+m.GetAuthor())
	}
	if m.GetRequires() != "" {
		lines = append(lines, "  Requires: Icebath "+m.GetRequires())
	}
	lines = append(lines, "  Source:   "+plan.GetProvenance(), "  Target:   "+plan.GetTarget())
	if plan.GetExisting() {
		if plan.GetDistribution() {
			lines = append(lines, "  (profile exists — will overwrite distribution-owned files only)")
		} else {
			lines = append(lines, "  ⚠ Profile exists but is NOT a distribution.  Installing here will", "    overwrite its SOUL.md and mcp.json and replace any skill or "+
				"cron job", "    of the same name the distribution ships.", "    Your memories, sessions, auth.json, and .env will be preserved,",
				"    but any hand-edits to distribution-owned files will be lost.")
		}
	}
	if len(m.GetEnv()) > 0 {
		lines = append(lines, "", "  Env vars:")
		for _, r := range m.GetEnv() {
			tag, status := "optional", "—"
			if r.GetRequired() {
				tag = "required"
			}
			switch {
			case r.GetSet():
				status = "✓ set"
			case r.GetRequired():
				status = "needs setting"
			}
			line := fmt.Sprintf("    • %s (%s, %s)", r.GetName(), tag, status)
			if r.GetDescription() != "" {
				line += " — " + r.GetDescription()
			}
			lines = append(lines, line)
		}
	}
	if plan.GetHasCron() {
		lines = append(lines, "", "  ⚠ This distribution ships cron jobs.  They will NOT run automatically — review and enable manually.")
	}
	return lines
}

func (c *commandSet) install() *cobra.Command {
	var name string
	var alias, force, yes bool
	cmd := &cobra.Command{
		Use:   "install <source>",
		Short: "Install a profile distribution from a git URL or local directory",
		Args:  cobra.ExactArgs(1),
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
			if !yes && !cli.NewConsole(cmd).Agree("\nProceed with install? [y/N] ", false) {
				say(out, "Install cancelled.")
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
						say(out, "⚠ %v", err)
					}
				}
			}
			say(out, "\n✓ Installed '%s' v%s", m.GetName(), m.GetVersion())
			say(out, "  Profile path: %s", plan.GetTarget())
			if len(m.GetEnv()) > 0 {
				say(out, "  Next: copy .env.EXAMPLE to .env and fill in required keys:\n    %s/.env.EXAMPLE", plan.GetTarget())
			}
			if plan.GetHasCron() {
				say(out, "  Cron jobs were included but are NOT scheduled automatically.\n  Review them with:  icebath -p %s cron list", m.GetName())
			}
			say(out, "\n  Use with:      icebath -p %s chat", m.GetName())
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "name of the profile, the distribution's own when empty")
	f.BoolVar(&alias, "alias", false, "make an alias command for the profile")
	f.BoolVar(&force, "force", false, "install over a profile that is already there")
	f.BoolVarP(&yes, "yes", "y", false, "install without asking")
	return cmd
}

func (c *commandSet) update() *cobra.Command {
	var forceConfig, yes bool
	cmd := &cobra.Command{
		Use:   "update <profile_name>",
		Short: "Re-pull a distribution and apply updates (user data preserved)",
		Args:  cobra.ExactArgs(1),
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
				say(out, "Error: Profile '%s' is not a distribution (no distribution.yaml). Only profiles installed via `icebath profile install` can "+
					"be updated.", canon)
				return cli.Exit(1)
			}
			if !yes {
				from := current.GetManifest().GetSource()
				if from == "" {
					from = "(no source)"
				}
				say(out, "\nUpdate '%s' from: %s", canon, from)
				say(out, "  Currently at version %s", current.GetManifest().GetVersion())
				if forceConfig {
					say(out, "  --force-config set: config.yaml WILL be overwritten.")
				} else {
					say(out, "  config.yaml will be preserved (pass --force-config to overwrite).")
				}
				say(out, "  User data (memories, sessions, auth, .env) will NOT be touched.")
				if !cli.NewConsole(cmd).Agree("\nProceed? [y/N] ", false) {
					say(out, "Update cancelled.")
					return nil
				}
			}
			updated, err := c.service.Update(cmd.Context(), &profilev1.UpdateRequest{Name: canon, ForceConfig: forceConfig})
			if err != nil {
				return fail(out, err)
			}
			m := updated.GetPlan().GetManifest()
			say(out, "\n✓ Updated '%s' → v%s", m.GetName(), m.GetVersion())
			if updated.GetPlan().GetHasCron() {
				say(out, "  Cron files were refreshed.  Review with:  icebath -p %s cron list", m.GetName())
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&forceConfig, "force-config", false, "overwrite config.yaml with the distribution's")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "update without asking")
	return cmd
}

func (c *commandSet) info(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	found, err := c.service.Info(cmd.Context(), &profilev1.InfoRequest{Name: args[0]})
	if err != nil {
		return fail(out, err)
	}
	if !found.GetFound() {
		say(out, "Profile '%s' is not a distribution (no distribution.yaml).", args[0])
		return nil
	}
	for _, line := range InfoLines(found.GetManifest()) {
		say(out, "%s", line)
	}
	return nil
}

// Lines of a distribution's manifest, which the command prints and the profiles screen shows
func InfoLines(m *profilev1.Manifest) []string {
	lines := []string{"", "Distribution: " + m.GetName(), "Version:      " + orUnknown(m.GetVersion())}
	for _, row := range [][2]string{{m.GetDescription(), "Description:  "}, {m.GetAuthor(), "Author:       "}, {m.GetLicense(), "License:      "},
		{m.GetRequires(), "Requires:     Icebath "}, {m.GetSource(), "Source:       "}, {m.GetInstalledAt(), "Installed:    "}} {
		if row[0] != "" {
			lines = append(lines, row[1]+row[0])
		}
	}
	if len(m.GetEnv()) > 0 {
		lines = append(lines, "", "Environment variables:")
		for _, r := range m.GetEnv() {
			tag := "optional"
			if r.GetRequired() {
				tag = "required"
			}
			line := fmt.Sprintf("  %s (%s)", r.GetName(), tag)
			if r.GetDescription() != "" {
				line += " — " + r.GetDescription()
			}
			lines = append(lines, line)
			if r.GetDefault() != "" {
				lines = append(lines, "      default: "+r.GetDefault())
			}
		}
	}
	return append(lines, "")
}

package cli

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"connectrpc.com/connect/v2"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/skills"
	skillsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/skills/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/skills/v1/skillsv1connect"
)

type commands struct{}

func Register(registry *cli.Registry) {
	registry.Register(commands{})
}

// Scope of a command: the profile in use and the folder it runs in
func here() *skillsv1.Scope {
	dir, _ := os.Getwd()
	return &skillsv1.Scope{Directory: dir}
}

// Asks to go ahead after the lines, true for yes; the end of the input is no
func confirm(cmd *cobra.Command, lines ...string) bool {
	c := cli.NewConsole(cmd)
	for _, line := range lines {
		fmt.Fprintln(c.Out(), line)
	}
	if c.Agree("Confirm [y/N]: ", false) {
		return true
	}
	fmt.Fprintln(c.Out(), "Cancelled.")
	return false
}

// One line naming the items when there are any
func listed(out io.Writer, label string, items []string) {
	if len(items) > 0 {
		fmt.Fprintf(out, "%s: %s\n", label, strings.Join(items, ", "))
	}
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := skillsv1connect.NewSkillsServiceClient(conn)
	root := &cobra.Command{Use: "skills", Short: "Search, install, configure, and manage skills"}
	root.AddCommand(list(service), configure(service), reset(service), modified(service), diff(service), optOut(service), optIn(service), repair(service))
	root.Long = "Search, install, inspect, audit, configure, and manage skills from skills.sh, well-known agent skill endpoints, GitHub, ClawHub, and other registries."
	root.AddCommand(hubCommands(service)...)
	return []*cobra.Command{root, bundles(service)}
}

// Where a listed skill comes from, as list's --source names it
func kind(skill *skillsv1.Skill) string {
	switch skill.GetSource() {
	case "hub", "builtin":
		return skill.GetSource()
	}
	return "local"
}

func list(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	var source string
	var enabledOnly bool
	command := &cobra.Command{
		Use:   "list",
		Short: "List installed skills",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !slices.Contains([]string{"all", "hub", "builtin", "local"}, source) {
				return fmt.Errorf("--source must be one of all, hub, builtin, local")
			}
			response, err := service.List(cmd.Context(), &skillsv1.ListRequest{Scope: here()})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			title := "Installed Skills"
			if enabledOnly {
				title += " (enabled only)"
			}
			fmt.Fprintln(out, title)
			table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(table, "NAME\tCATEGORY\tSOURCE\tTRUST\tSTATUS")
			counts := map[string]int{}
			enabled, disabled := 0, 0
			for _, skill := range response.GetSkills() {
				if source != "all" && source != kind(skill) || enabledOnly && !skill.GetEnabled() {
					continue
				}
				counts[kind(skill)]++
				status := "disabled"
				if skill.GetEnabled() {
					status = "enabled"
					enabled++
				} else {
					disabled++
				}
				if off := skill.GetDisabledOn(); len(off) > 0 {
					status += " (off on " + strings.Join(off, ", ") + ")"
				}
				if note := skill.GetCommandNote(); note != "" {
					status += "; " + note
				}
				trust := skill.GetTrust()
				if trust == "builtin" && skill.GetSource() == "hub" {
					trust = "official"
				}
				fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", skill.GetName(), cmp.Or(skill.GetCategory(), "-"), skill.GetSource(), trust, status)
			}
			if err := table.Flush(); err != nil {
				return err
			}
			tail := fmt.Sprintf("%d enabled, %d disabled", enabled, disabled)
			if enabledOnly {
				tail = fmt.Sprintf("%d enabled shown", enabled)
			}
			_, err = fmt.Fprintf(out, "%d hub-installed, %d builtin, %d local — %s\n", counts["hub"], counts["builtin"], counts["local"], tail)
			return err
		},
	}
	command.Flags().StringVar(&source, "source", "all", "skills of one source: all, hub, builtin or local")
	command.Flags().BoolVar(&enabledOnly, "enabled-only", false, "only the skills that are on")
	return command
}

// Numbers of a selection such as 1 3-5, each counted from 1 up to n
func selection(text string, n int) ([]int, error) {
	var out []int
	for _, part := range strings.FieldsFunc(text, func(r rune) bool { return r == ' ' || r == ',' }) {
		from, to, ranged := strings.Cut(part, "-")
		a, err := strconv.Atoi(from)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", part)
		}
		b := a
		if ranged {
			if b, err = strconv.Atoi(to); err != nil {
				return nil, fmt.Errorf("%q is not a range", part)
			}
		}
		for i := min(a, b); i <= max(a, b); i++ {
			if i < 1 || i > n {
				return nil, fmt.Errorf("%d is not on the list", i)
			}
			out = append(out, i-1)
		}
	}
	return out, nil
}

// Lets the user turn items on and off: the ones on when the user saves, false when they cancel
func checklist(cmd *cobra.Command, input *cli.Console, title string, labels []string, on []bool) ([]bool, bool) {
	out := cmd.OutOrStdout()
	on = slices.Clone(on)
	for {
		fmt.Fprintf(out, "\n%s\n", title)
		for i, label := range labels {
			mark := " "
			if on[i] {
				mark = "x"
			}
			fmt.Fprintf(out, "  %3d. [%s] %s\n", i+1, mark, label)
		}
		line, ok := input.Answer("\nToggle numbers (e.g. 1 3-5), a for all, n for none, enter to save, q to cancel: ")
		switch {
		case !ok || line == "q":
			return nil, false
		case line == "":
			return on, true
		case line == "a" || line == "n":
			for i := range on {
				on[i] = line == "a"
			}
		default:
			picked, err := selection(line, len(labels))
			if err != nil {
				fmt.Fprintln(out, err)
				continue
			}
			for _, i := range picked {
				on[i] = !on[i]
			}
		}
	}
}

func configure(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	return &cobra.Command{
		Use:   "config",
		Short: "Interactive skill configuration — enable/disable individual skills",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.InOrStdin() == os.Stdin && !term.IsTerminal(os.Stdin.Fd()) {
				fmt.Fprint(cmd.ErrOrStderr(), "Error: 'icebath skills config' requires an interactive terminal.\nIt cannot be run through a pipe or non-interactive subprocess.\nRun it directly in your terminal instead.\n")
				return cli.Exit(1)
			}
			out := cmd.OutOrStdout()
			response, err := service.List(cmd.Context(), &skillsv1.ListRequest{Scope: here()})
			if err != nil {
				return err
			}
			var installed []*skillsv1.Skill
			for _, skill := range response.GetSkills() {
				if skill.GetSource() != "plugin" {
					installed = append(installed, skill)
				}
			}
			if len(installed) == 0 {
				fmt.Fprintln(out, "No skills installed.")
				return nil
			}
			input := cli.NewConsole(cmd)
			fmt.Fprintln(out, "\nConfigure skills for:")
			fmt.Fprintln(out, "  1. All platforms (global default)")
			for i, surface := range response.GetSurfaces() {
				fmt.Fprintf(out, "  %d. %s\n", i+2, surface.GetTitle())
			}
			choice, ok := input.Answer("\nSelect [1]: ")
			if !ok {
				return nil
			}
			surface, label := "", "All platforms"
			if n, err := strconv.Atoi(choice); err == nil && n >= 2 && n-2 < len(response.GetSurfaces()) {
				surface, label = response.GetSurfaces()[n-2].GetName(), response.GetSurfaces()[n-2].GetTitle()
			}
			enabled := func(skill *skillsv1.Skill) bool {
				if surface == "" {
					return skill.GetEnabled()
				}
				return skill.GetEssential() || !slices.Contains(skill.GetDisabledOn(), surface)
			}
			fmt.Fprintf(out, "\nConfigure for: %s\n\n  1. Toggle individual skills\n  2. Toggle by category\n", label)
			mode, ok := input.Answer("\nSelect [1]: ")
			if !ok {
				return nil
			}
			wanted := map[string]bool{}
			for _, skill := range installed {
				wanted[skill.GetName()] = enabled(skill)
			}
			if mode == "2" {
				var categories []string
				members := map[string][]string{}
				for _, skill := range installed {
					category := cmp.Or(skill.GetCategory(), "uncategorized")
					if !slices.Contains(categories, category) {
						categories = append(categories, category)
					}
					members[category] = append(members[category], skill.GetName())
				}
				slices.Sort(categories)
				labels, on := make([]string, len(categories)), make([]bool, len(categories))
				for i, category := range categories {
					labels[i] = fmt.Sprintf("%s (%d skills)", category, len(members[category]))
					on[i] = slices.ContainsFunc(members[category], func(name string) bool { return wanted[name] })
				}
				chosen, ok := checklist(cmd, input, "Categories — toggle entire categories", labels, on)
				if !ok {
					fmt.Fprintln(out, "Cancelled.")
					return nil
				}
				for i, category := range categories {
					if chosen[i] != on[i] {
						for _, name := range members[category] {
							wanted[name] = chosen[i]
						}
					}
				}
			} else {
				labels, on := make([]string, len(installed)), make([]bool, len(installed))
				for i, skill := range installed {
					labels[i] = fmt.Sprintf("%s  (%s)  —  %s", skill.GetName(), cmp.Or(skill.GetCategory(), "uncategorized"), strings.Join(strings.Fields(skill.GetDescription()), " "))
					on[i] = wanted[skill.GetName()]
				}
				chosen, ok := checklist(cmd, input, "Skills for "+label, labels, on)
				if !ok {
					fmt.Fprintln(out, "Cancelled.")
					return nil
				}
				for i, skill := range installed {
					wanted[skill.GetName()] = chosen[i]
				}
			}
			var turnOn, turnOff []string
			for _, skill := range installed {
				switch name := skill.GetName(); {
				case wanted[name] && !enabled(skill):
					turnOn = append(turnOn, name)
				case !wanted[name] && enabled(skill) && !skill.GetEssential():
					turnOff = append(turnOff, name)
				}
			}
			if len(turnOn)+len(turnOff) == 0 {
				fmt.Fprintln(out, "No changes.")
				return nil
			}
			var counts *skillsv1.ToggleResponse
			for _, change := range []struct {
				names []string
				on    bool
			}{{turnOn, true}, {turnOff, false}} {
				if len(change.names) == 0 {
					continue
				}
				if counts, err = service.Toggle(cmd.Context(), &skillsv1.ToggleRequest{Scope: here(), Names: change.names, Enabled: change.on, Surface: surface}); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintf(out, "✓ Saved: %d enabled, %d disabled (%s).\n", counts.GetEnabled(), counts.GetDisabled(), label)
			return err
		},
	}
}

func reset(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	var restore, yes bool
	command := &cobra.Command{
		Use:   "reset <name>",
		Short: "Reset a bundled skill — clears 'user-modified' tracking so updates work again",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if restore && !yes && !confirm(cmd, fmt.Sprintf("\nRestore '%s' from bundled source?", args[0]), "This will DELETE your current copy and re-copy the bundled version.") {
				return nil
			}
			response, err := service.Reset(cmd.Context(), &skillsv1.ResetRequest{Scope: here(), Name: args[0], Restore: restore})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, response.GetMessage())
			listed(out, "Copied", response.GetSynced().GetCopied())
			listed(out, "Updated", response.GetSynced().GetUpdated())
			return nil
		},
	}
	command.Flags().BoolVar(&restore, "restore", false, "replace your copy with the bundled version")
	command.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask to confirm")
	return command
}

func modified(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:   "list-modified",
		Short: "List bundled skills you've edited (which syncs keep)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			response, err := service.Modified(cmd.Context(), &skillsv1.ModifiedRequest{Scope: here()})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			names := response.GetNames()
			if asJSON {
				if names == nil {
					names = []string{}
				}
				data, err := json.Marshal(names)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(out, string(data))
				return err
			}
			if len(names) == 0 {
				fmt.Fprintln(out, "No user-modified bundled skills — everything tracks upstream.")
				return nil
			}
			fmt.Fprintf(out, "\n%d user-modified bundled skill(s) (kept as-is by syncs):\n", len(names))
			for _, name := range names {
				fmt.Fprintln(out, "  ~ "+name)
			}
			fmt.Fprintln(out, "\nSee changes:    icebath skills diff <name>")
			fmt.Fprintln(out, "Resume updates: icebath skills reset <name>          (keep your copy, re-baseline)")
			fmt.Fprintln(out, "Revert to stock: icebath skills reset <name> --restore")
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print the names as JSON")
	return command
}

func diff(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	return &cobra.Command{
		Use:   "diff <name>",
		Short: "Show how your copy of a bundled skill differs from the stock version",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			response, err := service.Diff(cmd.Context(), &skillsv1.DiffRequest{Scope: here(), Name: args[0]})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			files := response.GetFiles()
			if len(files) == 0 {
				fmt.Fprintf(out, "'%s' matches the stock version.\n", args[0])
				return nil
			}
			fmt.Fprintf(out, "\n'%s' differs from the stock version in %d file(s).\n\n", args[0], len(files))
			for _, file := range files {
				switch file.GetStatus() {
				case "modified":
					fmt.Fprint(out, file.GetDiff())
				case "added":
					fmt.Fprintln(out, "+ only in your copy: "+file.GetPath())
				case "removed":
					fmt.Fprintln(out, "- only in stock: "+file.GetPath())
				default:
					fmt.Fprintf(out, "~ %s: binary file differs\n", file.GetPath())
				}
			}
			fmt.Fprintf(out, "\nRevert with: icebath skills reset %s --restore\n", args[0])
			return nil
		},
	}
}

func optOut(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	var remove, yes bool
	command := &cobra.Command{
		Use:   "opt-out",
		Short: "Stop bundled skills from being seeded into this profile",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			response, err := service.OptOut(cmd.Context(), &skillsv1.OptOutRequest{Scope: here(), Out: true})
			if err != nil {
				return err
			}
			fmt.Fprintln(out, response.GetMessage())
			fmt.Fprintln(out, "Marker: "+response.GetMarker())
			if !remove {
				fmt.Fprintln(out, "Existing skills on disk were left in place. Re-run with --remove to also delete unmodified bundled skills.")
				return nil
			}
			preview, err := service.OptOut(cmd.Context(), &skillsv1.OptOutRequest{Scope: here(), Out: true, Dry: true})
			if err != nil {
				return err
			}
			if len(preview.GetRemoved()) == 0 {
				fmt.Fprintln(out, "No pristine bundled skills to remove (nothing tracked, or all are user-modified/local).")
				return nil
			}
			fmt.Fprintf(out, "\nWill remove %d unmodified bundled skill(s):\n%s\n", len(preview.GetRemoved()), strings.Join(preview.GetRemoved(), ", "))
			if kept := preview.GetKept(); len(kept) > 0 {
				fmt.Fprintf(out, "Keeping %d (user-modified or non-bundled).\n", len(kept))
			}
			if !yes && !confirm(cmd, "This deletes the on-disk copies. User-edited and hub/local skills are NOT touched.") {
				fmt.Fprintln(out, "Marker kept; no skills deleted.")
				return nil
			}
			response, err = service.OptOut(cmd.Context(), &skillsv1.OptOutRequest{Scope: here(), Out: true, Remove: true})
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Removed %d pristine bundled skill(s); kept %d.\n", len(response.GetRemoved()), len(response.GetKept()))
			listed(out, "Removed", response.GetRemoved())
			return nil
		},
	}
	command.Flags().BoolVar(&remove, "remove", false, "also delete the bundled skills still as they were copied")
	command.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask to confirm")
	return command
}

func optIn(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	var sync bool
	command := &cobra.Command{
		Use:   "opt-in",
		Short: "Re-enable bundled-skill seeding (undo opt-out)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			response, err := service.OptOut(cmd.Context(), &skillsv1.OptOutRequest{Scope: here(), Sync: sync})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, response.GetMessage())
			if sync {
				fmt.Fprintf(out, "Re-seeded %d bundled skill(s).\n", len(response.GetSynced().GetCopied()))
			}
			return nil
		},
	}
	command.Flags().BoolVar(&sync, "sync", false, "seed the bundled skills now")
	return command
}

func repair(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	var restore, yes bool
	command := &cobra.Command{
		Use:   "repair-official <name>",
		Short: "Backfill or restore official optional skills from repo source",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if restore && !yes && !confirm(cmd, fmt.Sprintf("\nRestore official optional skill '%s' from repo source?", args[0]), "Existing matching active copies will be moved to a restore backup before copying the official source.") {
				return nil
			}
			response, err := service.Repair(cmd.Context(), &skillsv1.RepairRequest{Scope: here(), Name: args[0], Restore: restore})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "Official optional skill repair complete.")
			listed(out, "Restored", response.GetRestored())
			listed(out, "Backfilled provenance", response.GetBackfilled())
			if backed := response.GetBackedUp(); len(backed) > 0 {
				fmt.Fprintln(out, "Backed up: "+strings.Join(backed, ", "))
				fmt.Fprintln(out, "Backup dir: "+response.GetBackupDir())
			}
			return nil
		},
	}
	command.Flags().BoolVar(&restore, "restore", false, "put the official copy in place, moving matching copies aside")
	command.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask to confirm")
	return command
}

func bundles(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	show := func(cmd *cobra.Command) error {
		response, err := service.Bundles(cmd.Context(), &skillsv1.BundlesRequest{Scope: here()})
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		if len(response.GetBundles()) == 0 {
			fmt.Fprintf(out, "No bundles installed yet. Create one with:\n  icebath bundles create <name> --skill skill1 --skill skill2\nBundles directory: %s\n", response.GetDir())
			return nil
		}
		fmt.Fprintf(out, "Skill Bundles (%d)\n", len(response.GetBundles()))
		table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(table, "COMMAND\tNAME\tSKILLS\tDESCRIPTION")
		for _, b := range response.GetBundles() {
			fmt.Fprintf(table, "/%s\t%s\t%d\t%s\n", b.GetSlug(), b.GetName(), len(b.GetSkills()), b.GetDescription())
		}
		if err := table.Flush(); err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "\nBundles directory: %s\n", response.GetDir())
		return err
	}
	root := &cobra.Command{
		Use:   "bundles",
		Short: "Create, list, and manage skill bundles (aliases for multiple skills)",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return show(cmd) },
	}
	var skillNames []string
	var description, instruction string
	var force bool
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new skill bundle",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			names := slices.Clone(skillNames)
			if len(names) == 0 {
				fmt.Fprintln(out, "No skills passed via --skill. Enter one skill name per line.\nSubmit an empty line to finish.")
				input := cli.NewConsole(cmd)
				for {
					line, ok := input.Answer("skill> ")
					if !ok {
						return errors.New("Cancelled.")
					}
					if line == "" {
						break
					}
					names = append(names, line)
				}
			}
			if len(names) == 0 {
				return errors.New("A bundle must reference at least one skill.")
			}
			response, err := service.SaveBundle(cmd.Context(), &skillsv1.SaveBundleRequest{Scope: here(), Name: args[0], Skills: names, Description: description, Instruction: instruction, Overwrite: force})
			if connect.CodeOf(err) == connect.CodeAlreadyExists {
				return fmt.Errorf("%w\nPass --force to overwrite.", err)
			}
			if err != nil {
				return err
			}
			b := response.GetBundle()
			fmt.Fprintln(out, "Created bundle: "+b.GetPath())
			_, err = fmt.Fprintf(out, "  Invoke with: /%s  (loads %d skills)\n", b.GetSlug(), len(b.GetSkills()))
			return err
		},
	}
	create.Flags().StringArrayVarP(&skillNames, "skill", "s", nil, "a skill of the bundle, repeated for each")
	create.Flags().StringVarP(&description, "description", "d", "", "what the bundle is for")
	create.Flags().StringVarP(&instruction, "instruction", "i", "", "told to the model along with the skills")
	create.Flags().BoolVarP(&force, "force", "f", false, "replace a bundle of the name")
	root.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List installed skill bundles",
			Args:  cobra.NoArgs,
			RunE:  func(cmd *cobra.Command, _ []string) error { return show(cmd) },
		},
		&cobra.Command{
			Use:   "show <name>",
			Short: "Show one bundle's contents",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				response, err := service.Bundles(cmd.Context(), &skillsv1.BundlesRequest{Scope: here()})
				if err != nil {
					return err
				}
				for _, b := range response.GetBundles() {
					if b.GetSlug() != skills.Slug(args[0]) {
						continue
					}
					out := cmd.OutOrStdout()
					fmt.Fprintf(out, "/%s  %s\n", b.GetSlug(), b.GetName())
					if b.GetDescription() != "" {
						fmt.Fprintln(out, "  "+b.GetDescription())
					}
					fmt.Fprintf(out, "  File: %s\n  Skills (%d):\n", b.GetPath(), len(b.GetSkills()))
					for _, skill := range b.GetSkills() {
						fmt.Fprintln(out, "    - "+skill)
					}
					if b.GetInstruction() != "" {
						fmt.Fprintln(out, "  Instruction:\n    "+b.GetInstruction())
					}
					return nil
				}
				return fmt.Errorf("Bundle %q not found.", args[0])
			},
		},
		create,
		&cobra.Command{
			Use:   "delete <name>",
			Short: "Delete a skill bundle",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				response, err := service.DeleteBundle(cmd.Context(), &skillsv1.DeleteBundleRequest{Scope: here(), Name: args[0]})
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "Deleted bundle: "+response.GetPath())
				return err
			},
		},
		&cobra.Command{
			Use:   "reload",
			Short: "Re-scan the bundles directory and report changes",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				response, err := service.Reload(cmd.Context(), &skillsv1.ReloadRequest{Scope: here()})
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				if len(response.GetAdded()) > 0 {
					fmt.Fprintf(out, "Added (%d):\n", len(response.GetAdded()))
					for _, c := range response.GetAdded() {
						fmt.Fprintf(out, "  + %s — %s\n", c.GetName(), c.GetDescription())
					}
				}
				if len(response.GetRemoved()) > 0 {
					fmt.Fprintf(out, "Removed (%d):\n", len(response.GetRemoved()))
					for _, c := range response.GetRemoved() {
						fmt.Fprintln(out, "  - "+c.GetName())
					}
				}
				if len(response.GetAdded())+len(response.GetRemoved()) == 0 {
					_, err = fmt.Fprintf(out, "No changes. %d command(s) loaded.\n", response.GetTotal())
				} else {
					_, err = fmt.Fprintf(out, "Total now: %d\n", response.GetTotal())
				}
				return err
			},
		},
	)
	return root
}

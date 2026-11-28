package cli

import (
	"cmp"
	"context"
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
	picked "github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/skills"
	skillsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/skills/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/skills/v1/skillsv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Count with its verb: 1 bundle is, 2 bundles are
func agreeing(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strs.Number(float64(n)) + " " + many
}

type commands struct{}

func Register(registry *cli.Registry) {
	registry.Register(commands{})
}

// Scope of a command: the profile -p picked, the one in use for none, and the folder it runs in
func here(ctx context.Context) *skillsv1.Scope {
	dir, _ := os.Getwd()
	return &skillsv1.Scope{Profile: picked.Picked(ctx), Directory: dir}
}

// Asks the question after the lines, true for yes; the end of the input is no, which says that nothing changed
func confirm(cmd *cobra.Command, question string, lines ...string) bool {
	c := cli.NewConsole(cmd)
	for _, line := range lines {
		fmt.Fprintln(c.Out(), line)
	}
	if c.Agree(question+" [y/N] ", false) {
		return true
	}
	fmt.Fprintln(c.Out(), "Nothing changed.")
	return false
}

// Where a listed skill comes from, in words
func sourceWord(source string) string {
	switch source {
	case "hub":
		return "Skills hub"
	case "builtin":
		return "Built in"
	case "plugin":
		return "Plugin"
	}
	return "Your own"
}

// One line naming the items when there are any
func listed(out io.Writer, label string, items []string) {
	if len(items) > 0 {
		fmt.Fprintf(out, "%s: %s\n", label, strings.Join(items, ", "))
	}
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := skillsv1connect.NewSkillsServiceClient(conn)
	root := &cobra.Command{Use: "skills", GroupID: cli.GroupAbilities, Short: "Find, install and manage skills"}
	root.AddCommand(list(service), turn(service, true), turn(service, false), configure(service), reset(service), modified(service), diff(service),
		optOut(service), optIn(service), repair(service))
	root.Long = "Skills are instructions the agent loads for certain kinds of work. Use these commands to find skills on skills.sh, GitHub, " +
		"ClawHub and other registries, install them, scan them for risks and turn them on or off. icebath also comes with built-in skills, " +
		"which it copies into each profile."
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
		Short: "List the installed skills",
		Long:  "Lists every installed skill with its category, where it came from and whether it is on.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !slices.Contains([]string{"all", "hub", "builtin", "local"}, source) {
				return fmt.Errorf("--source takes all, hub, builtin or local, not “%s”.", source)
			}
			response, err := service.List(cmd.Context(), &skillsv1.ListRequest{Scope: here(cmd.Context()), Surface: "cli"})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			title := "Installed skills"
			if enabledOnly {
				title = "Installed skills that are on"
			}
			fmt.Fprintln(out, title)
			table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(table, "Name\tCategory\tFrom\tTrust\tState")
			counts := map[string]int{}
			enabled, disabled := 0, 0
			for _, skill := range response.GetSkills() {
				if source != "all" && source != kind(skill) || enabledOnly && !skill.GetEnabled() {
					continue
				}
				counts[kind(skill)]++
				status := skill.GetShown()
				if skill.GetEnabled() {
					enabled++
				} else {
					disabled++
				}
				for _, surface := range response.GetSurfaces() {
					if surface.GetName() != "cli" && slices.Contains(skill.GetDisabledOn(), surface.GetName()) {
						status += ", off in the " + strings.ToLower(surface.GetTitle())
					}
				}
				if note := skill.GetCommandNote(); note != "" {
					status += ". " + note
				}
				trust := skill.GetTrust()
				if trust == "builtin" && skill.GetSource() == "hub" {
					trust = "official"
				}
				fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", skill.GetName(), skill.GetCategoryTitle(), sourceWord(skill.GetSource()),
					cmp.Or(strings.ToUpper(trust[:min(1, len(trust))])+trust[min(1, len(trust)):], "Unknown"), status)
			}
			if err := table.Flush(); err != nil {
				return err
			}
			_, err = fmt.Fprintf(out, "\n%s from the skills hub, %s built in and %s of your own. On: %s. Off: %s.\n",
				strs.Number(float64(counts["hub"])), strs.Number(float64(counts["builtin"])), strs.Number(float64(counts["local"])),
				strs.Number(float64(enabled)), strs.Number(float64(disabled)))
			return err
		},
	}
	command.Flags().StringVar(&source, "source", "all", "Source to list: all, hub, builtin or local")
	command.Flags().BoolVar(&enabledOnly, "enabled-only", false, "List only the skills that are on")
	return command
}

// Turns skills on or off as the switches of the page of Skills do, by the name of a skill or of a category, in every app or in one alone
func turn(service skillsv1connect.SkillsServiceClient, on bool) *cobra.Command {
	use, short, long := "disable <skill or category>...", "Turn skills off", "Turns off the skills named, by the name of a skill or of a category."
	if on {
		use, short, long = "enable <skill or category>...", "Turn skills on", "Turns on the skills named, by the name of a skill or of a category."
	}
	var app string
	command := &cobra.Command{Use: use, Short: short, Long: long + " With --app, only in that app.", Example: "  icebath skills disable pdf-tools",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			response, err := service.List(cmd.Context(), &skillsv1.ListRequest{Scope: here(cmd.Context()), Surface: app})
			if err != nil {
				return err
			}
			var names, unknown []string
			for _, word := range args {
				matched := false
				for _, skill := range response.GetSkills() {
					category := strings.EqualFold(skill.GetCategory(), word) || strings.EqualFold(skill.GetCategoryTitle(), word)
					switch {
					case skill.GetName() == word:
						names, matched = append(names, skill.GetName()), true
					case category && skill.GetEssential():
						// A skill that is always on stays so when its category is turned off
						matched = true
					case category:
						names, matched = append(names, skill.GetName()), true
					}
				}
				if !matched {
					unknown = append(unknown, word)
				}
			}
			c := cli.NewConsole(cmd)
			if len(unknown) > 0 {
				c.Failure("No skill or category is called " + strings.Join(unknown, ", ") + ". icebath skills list shows them.")
				return cli.Exit(1)
			}
			if len(names) == 0 {
				c.Info("Every skill named there is always on, because the agent's instructions point at it.")
				return nil
			}
			turned, err := service.Toggle(cmd.Context(), &skillsv1.ToggleRequest{Scope: here(cmd.Context()), Names: names, Enabled: on, Surface: app})
			if failure, ok := errors.AsType[*connect.Error](err); ok {
				c.Failure(failure.Message())
				return cli.Exit(1)
			}
			if err != nil {
				return err
			}
			c.Success(turned.GetSaid())
			return nil
		}}
	command.Flags().StringVar(&app, "app", "", "Turn them on or off in one app alone: tui for the terminal app, cli for the command line")
	return command
}

// Numbers of a selection such as 1 3-5, each counted from 1 up to n
func selection(text string, n int) ([]int, error) {
	var out []int
	for _, part := range strings.FieldsFunc(text, func(r rune) bool { return r == ' ' || r == ',' }) {
		from, to, ranged := strings.Cut(part, "-")
		a, err := strconv.Atoi(from)
		if err != nil {
			return nil, fmt.Errorf("“%s” is not a number.", part)
		}
		b := a
		if ranged {
			if b, err = strconv.Atoi(to); err != nil {
				return nil, fmt.Errorf("“%s” is not a range such as 3-5.", part)
			}
		}
		for i := min(a, b); i <= max(a, b); i++ {
			if i < 1 || i > n {
				return nil, fmt.Errorf("%s is not on the list.", strs.Number(float64(i)))
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
			fmt.Fprintf(out, "  %3d [%s] %s\n", i+1, mark, label)
		}
		line, ok := input.Answer("\nType numbers such as 1 3-5 to turn them on or off, a for all, n for none, enter to save or q to cancel: ")
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
		Short: "Turn skills on or off in a list",
		Long: "Opens a list of the installed skills in this terminal, where you turn each skill on or off. config needs a terminal, so it does " +
			"not run through a pipe.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.InOrStdin() == os.Stdin && !term.IsTerminal(os.Stdin.Fd()) {
				fmt.Fprint(cmd.ErrOrStderr(), "✗ icebath skills config needs a terminal to ask in, so it can't run through a pipe or from another program. "+
					"Run it in your terminal.\n")
				return cli.Exit(1)
			}
			out := cmd.OutOrStdout()
			response, err := service.List(cmd.Context(), &skillsv1.ListRequest{Scope: here(cmd.Context())})
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
				fmt.Fprintln(out, "No skills are installed yet. Find one with icebath skills search <query>, then install it with icebath skills install <identifier>.")
				return nil
			}
			input := cli.NewConsole(cmd)
			fmt.Fprintln(out, "\nWhere should the change apply?")
			fmt.Fprintln(out, "  1. Everywhere, as the default")
			for i, surface := range response.GetSurfaces() {
				fmt.Fprintf(out, "  %d. %s\n", i+2, surface.GetTitle())
			}
			choice, ok := input.Answer("\nNumber [1]: ")
			if !ok {
				return nil
			}
			surface, label := "", "Everywhere"
			if n, err := strconv.Atoi(choice); err == nil && n >= 2 && n-2 < len(response.GetSurfaces()) {
				surface, label = response.GetSurfaces()[n-2].GetName(), response.GetSurfaces()[n-2].GetTitle()
			}
			enabled := func(skill *skillsv1.Skill) bool {
				if surface == "" {
					return skill.GetEnabled()
				}
				return skill.GetEssential() || !slices.Contains(skill.GetDisabledOn(), surface)
			}
			fmt.Fprintf(out, "\nChanging skills for: %s\n\n  1. Turn single skills on or off\n  2. Turn whole categories on or off\n", label)
			mode, ok := input.Answer("\nNumber [1]: ")
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
					category := cmp.Or(skill.GetCategory(), "No category")
					if !slices.Contains(categories, category) {
						categories = append(categories, category)
					}
					members[category] = append(members[category], skill.GetName())
				}
				slices.Sort(categories)
				labels, on := make([]string, len(categories)), make([]bool, len(categories))
				for i, category := range categories {
					labels[i] = category + ", " + strs.Count(len(members[category]), "skill")
					on[i] = slices.ContainsFunc(members[category], func(name string) bool { return wanted[name] })
				}
				chosen, ok := checklist(cmd, input, "Categories. Turning one on or off changes every skill in it.", labels, on)
				if !ok {
					fmt.Fprintln(out, "Nothing changed.")
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
					labels[i] = fmt.Sprintf("%s, %s: %s", skill.GetName(), cmp.Or(skill.GetCategory(), "no category"), strings.Join(strings.Fields(skill.GetDescription()), " "))
					on[i] = wanted[skill.GetName()]
				}
				chosen, ok := checklist(cmd, input, "Skills for "+label, labels, on)
				if !ok {
					fmt.Fprintln(out, "Nothing changed.")
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
				fmt.Fprintln(out, "Nothing changed.")
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
				if counts, err = service.Toggle(cmd.Context(), &skillsv1.ToggleRequest{Scope: here(cmd.Context()), Names: change.names, Enabled: change.on, Surface: surface}); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintf(out, "✓ Saved for %s. On: %s. Off: %s.\n", label, strs.Number(float64(counts.GetEnabled())),
				strs.Number(float64(counts.GetDisabled())))
			return err
		},
	}
}

func reset(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	var restore, yes bool
	command := &cobra.Command{
		Use:   "reset <name>",
		Short: "Let a built-in skill you edited take updates again",
		Long: "icebath stops updating a built-in skill once you edit it. reset clears that mark, so updates reach the skill again, and keeps " +
			"your copy. With --restore, reset replaces your copy with the built-in version.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if restore && !yes && !confirm(cmd, fmt.Sprintf("\nReplace your copy of %s with the built-in version? Your changes to it are deleted.", args[0])) {
				return nil
			}
			response, err := service.Reset(cmd.Context(), &skillsv1.ResetRequest{Scope: here(cmd.Context()), Name: args[0], Restore: restore})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "✓ "+response.GetMessage())
			listed(out, "Copied", response.GetSynced().GetCopied())
			listed(out, "Updated", response.GetSynced().GetUpdated())
			return nil
		},
	}
	command.Flags().BoolVar(&restore, "restore", false, "Replace your copy with the built-in version")
	command.Flags().BoolVarP(&yes, "yes", "y", false, "Go ahead without asking first")
	return command
}

func modified(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:   "list-modified",
		Short: "List the built-in skills you edited",
		Long:  "Lists the built-in skills you changed. Updates leave these skills alone until you reset them.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			response, err := service.Modified(cmd.Context(), &skillsv1.ModifiedRequest{Scope: here(cmd.Context())})
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
				fmt.Fprintln(out, "You have not edited any built-in skill, so every one takes updates.")
				return nil
			}
			fmt.Fprintf(out, "\nYou edited %s, which updates leave as they are:\n", strs.Count(len(names), "built-in skill"))
			for _, name := range names {
				fmt.Fprintln(out, "  "+name)
			}
			fmt.Fprintln(out, "\nSee your changes:         icebath skills diff <name>")
			fmt.Fprintln(out, "Take updates again:       icebath skills reset <name>, which keeps your copy")
			fmt.Fprintln(out, "Go back to the built-in:  icebath skills reset <name> --restore")
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "Print the names as JSON")
	return command
}

func diff(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	return &cobra.Command{
		Use:   "diff <name>",
		Short: "Show how your copy of a built-in skill differs",
		Long:  "Shows the changes between your copy of a built-in skill and the version that comes with icebath.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			response, err := service.Diff(cmd.Context(), &skillsv1.DiffRequest{Scope: here(cmd.Context()), Name: args[0]})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			files := response.GetFiles()
			if len(files) == 0 {
				fmt.Fprintf(out, "%s is the same as the built-in version.\n", args[0])
				return nil
			}
			fmt.Fprintf(out, "\n%s differs from the built-in version in %s.\n\n", args[0], strs.Count(len(files), "file"))
			for _, file := range files {
				switch file.GetStatus() {
				case "modified":
					fmt.Fprint(out, file.GetDiff())
				case "added":
					fmt.Fprintln(out, "Only in your copy: "+file.GetPath())
				case "removed":
					fmt.Fprintln(out, "Only in the built-in version: "+file.GetPath())
				default:
					fmt.Fprintf(out, "%s differs, and it is not text, so no change is shown.\n", file.GetPath())
				}
			}
			fmt.Fprintf(out, "\nTo go back to the built-in version, run icebath skills reset %s --restore.\n", args[0])
			return nil
		},
	}
}

func optOut(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	var remove, yes bool
	command := &cobra.Command{
		Use:   "opt-out",
		Short: "Stop copying built-in skills into this profile",
		Long: "Stops icebath from copying its built-in skills into this profile. Skills already copied stay, unless you pass --remove to " +
			"delete the ones you never edited. Skills you edited, and skills from the hub or your own folders, are never touched.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			response, err := service.OptOut(cmd.Context(), &skillsv1.OptOutRequest{Scope: here(cmd.Context()), Out: true})
			if err != nil {
				return err
			}
			fmt.Fprintln(out, marked(response))
			fmt.Fprintln(out, "The marker file is "+response.GetMarker()+".")
			if !remove {
				fmt.Fprintln(out, "The skills already copied stay. Run the command again with --remove to also delete the built-in skills you never edited.")
				return nil
			}
			preview, err := service.OptOut(cmd.Context(), &skillsv1.OptOutRequest{Scope: here(cmd.Context()), Out: true, Dry: true})
			if err != nil {
				return err
			}
			if len(preview.GetRemoved()) == 0 {
				fmt.Fprintln(out, "There is no unedited built-in skill to remove. Every one is edited, your own, or not copied.")
				return nil
			}
			fmt.Fprintf(out, "\nThis removes %s you never edited:\n%s\n", strs.Count(len(preview.GetRemoved()), "built-in skill"), strings.Join(preview.GetRemoved(), ", "))
			if kept := preview.GetKept(); len(kept) > 0 {
				fmt.Fprintf(out, "It keeps %s that you edited or that are not built in.\n", strs.Count(len(kept), "skill"))
			}
			if !yes && !confirm(cmd, fmt.Sprintf("Delete these %s? Skills you edited, and skills from the hub or your own folders, stay.",
				strs.Count(len(preview.GetRemoved()), "skill"))) {
				fmt.Fprintln(out, "No skill was deleted, and built-in skills are still not copied.")
				return nil
			}
			response, err = service.OptOut(cmd.Context(), &skillsv1.OptOutRequest{Scope: here(cmd.Context()), Out: true, Remove: true})
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "✓ Removed %s you never edited, and kept %s.\n", strs.Count(len(response.GetRemoved()), "built-in skill"),
				strs.Number(float64(len(response.GetKept()))))
			listed(out, "Removed", response.GetRemoved())
			return nil
		},
	}
	command.Flags().BoolVar(&remove, "remove", false, "Also delete the built-in skills you never edited")
	command.Flags().BoolVarP(&yes, "yes", "y", false, "Go ahead without asking first")
	return command
}

func optIn(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	var sync bool
	command := &cobra.Command{
		Use:   "opt-in",
		Short: "Copy built-in skills into this profile again",
		Long:  "Undoes opt-out, so icebath copies its built-in skills into this profile again. With --sync, opt-in copies them now.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			response, err := service.OptOut(cmd.Context(), &skillsv1.OptOutRequest{Scope: here(cmd.Context()), Sync: sync})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, marked(response))
			if sync {
				fmt.Fprintf(out, "✓ Copied %s into this profile.\n", strs.Count(len(response.GetSynced().GetCopied()), "built-in skill"))
			}
			return nil
		},
	}
	command.Flags().BoolVar(&sync, "sync", false, "Copy the built-in skills now")
	return command
}

func repair(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	var restore, yes bool
	command := &cobra.Command{
		Use:   "repair-official <name>",
		Short: "Repair an official optional skill from its source",
		Long: "Records where an official optional skill came from, when that record is missing. With --restore, repair-official also puts the official " +
			"copy in place and moves matching copies aside as a backup.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if restore && !yes && !confirm(cmd, fmt.Sprintf("\nPut the official copy of %s in place? Copies of it you have are moved to a backup first.", args[0])) {
				return nil
			}
			response, err := service.Repair(cmd.Context(), &skillsv1.RepairRequest{Scope: here(cmd.Context()), Name: args[0], Restore: restore})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "✓ Repaired the optional skill %s.\n", args[0])
			listed(out, "Put in place", response.GetRestored())
			listed(out, "Recorded where it came from", response.GetBackfilled())
			if backed := response.GetBackedUp(); len(backed) > 0 {
				fmt.Fprintln(out, "Backed up: "+strings.Join(backed, ", "))
				fmt.Fprintln(out, "Backup folder: "+response.GetBackupDir())
			}
			return nil
		},
	}
	command.Flags().BoolVar(&restore, "restore", false, "Put the official copy in place, moving matching copies aside")
	command.Flags().BoolVarP(&yes, "yes", "y", false, "Go ahead without asking first")
	return command
}

func bundles(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	show := func(cmd *cobra.Command) error {
		response, err := service.Bundles(cmd.Context(), &skillsv1.BundlesRequest{Scope: here(cmd.Context())})
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		if len(response.GetBundles()) == 0 {
			fmt.Fprintf(out, "You have no skill bundles yet. Make one with:\n  icebath bundles create <name> --skill skill1 --skill skill2\nBundles are kept in %s.\n",
				response.GetDir())
			return nil
		}
		fmt.Fprintf(out, "%s:\n", strs.Count(len(response.GetBundles()), "skill bundle"))
		table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(table, "Command\tName\tSkills\tWhat it is for")
		for _, b := range response.GetBundles() {
			fmt.Fprintf(table, "/%s\t%s\t%s\t%s\n", b.GetSlug(), b.GetName(), strs.Number(float64(len(b.GetSkills()))), cmp.Or(b.GetDescription(), "No description"))
		}
		if err := table.Flush(); err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "\nBundles are kept in %s.\n", response.GetDir())
		return err
	}
	root := &cobra.Command{
		Use:     "bundles",
		GroupID: cli.GroupAbilities,
		Short:   "Create and manage skill bundles",
		Long:    "A bundle loads several skills at once with one slash command, /name. With no subcommand, bundles lists your bundles.",
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return show(cmd) },
	}
	var skillNames []string
	var description, instruction string
	var force bool
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a skill bundle",
		Long: "Creates a bundle that loads the skills you name with --skill. The bundle's slash command is its name in lower case with " +
			"hyphens. Without --skill, create asks for the skills one per line.",
		Example: "  icebath bundles create review --skill code-review --skill security -d \"Review a change\"",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			names := slices.Clone(skillNames)
			if len(names) == 0 {
				fmt.Fprintln(out, "No skills were given with --skill. Type one skill name per line, and an empty line to finish.")
				input := cli.NewConsole(cmd)
				for {
					line, ok := input.Answer("Skill: ")
					if !ok {
						return errors.New("No bundle was made.")
					}
					if line == "" {
						break
					}
					names = append(names, line)
				}
			}
			if len(names) == 0 {
				return errors.New("A bundle needs at least one skill.")
			}
			response, err := service.SaveBundle(cmd.Context(), &skillsv1.SaveBundleRequest{Scope: here(cmd.Context()), Name: args[0], Skills: names, Description: description, Instruction: instruction, Overwrite: force})
			if failure, ok := errors.AsType[*connect.Error](err); ok && failure.Code() == connect.CodeAlreadyExists {
				return fmt.Errorf("%s Pass --force to replace it.", strings.TrimSuffix(failure.Message(), ".")+".")
			}
			if err != nil {
				return err
			}
			b := response.GetBundle()
			fmt.Fprintln(out, "✓ Made the bundle "+b.GetPath()+".")
			_, err = fmt.Fprintf(out, "  Load it with /%s, which loads %s.\n", b.GetSlug(), strs.Count(len(b.GetSkills()), "skill"))
			return err
		},
	}
	create.Flags().StringArrayVarP(&skillNames, "skill", "s", nil, "Skill to put in the bundle, repeat for each")
	create.Flags().StringVarP(&description, "description", "d", "", "What the bundle is for, shown with its command")
	create.Flags().StringVarP(&instruction, "instruction", "i", "", "Instruction the model gets along with the skills")
	create.Flags().BoolVarP(&force, "force", "f", false, "Replace a bundle that has this name")
	root.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List the skill bundles",
			Long:  "Lists every bundle with its slash command, how many skills it loads and what it is for.",
			Args:  cobra.NoArgs,
			RunE:  func(cmd *cobra.Command, _ []string) error { return show(cmd) },
		},
		&cobra.Command{
			Use:   "show <name>",
			Short: "Show what a skill bundle holds",
			Long:  "Shows one bundle's slash command, description, file, skills and instruction.",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				response, err := service.Bundles(cmd.Context(), &skillsv1.BundlesRequest{Scope: here(cmd.Context())})
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
					fmt.Fprintf(out, "  File: %s\n  It loads %s:\n", b.GetPath(), strs.Count(len(b.GetSkills()), "skill"))
					for _, skill := range b.GetSkills() {
						fmt.Fprintln(out, "    "+skill)
					}
					if b.GetInstruction() != "" {
						fmt.Fprintln(out, "  Instruction:\n    "+b.GetInstruction())
					}
					return nil
				}
				return fmt.Errorf("There is no bundle named %s. Run icebath bundles to see your bundles.", args[0])
			},
		},
		create,
		&cobra.Command{
			Use:   "delete <name>",
			Short: "Delete a skill bundle",
			Long:  "Deletes the bundle and its slash command. The skills it loaded stay installed.",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				response, err := service.DeleteBundle(cmd.Context(), &skillsv1.DeleteBundleRequest{Scope: here(cmd.Context()), Name: args[0]})
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "✓ Deleted the bundle "+response.GetPath()+".")
				return err
			},
		},
		&cobra.Command{
			Use:   "reload",
			Short: "Read the bundles folder again and report changes",
			Long:  "Reads the bundles folder again, for bundles you added or removed by hand, and reports what changed.",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				response, err := service.Reload(cmd.Context(), &skillsv1.ReloadRequest{Scope: here(cmd.Context())})
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				if len(response.GetAdded()) > 0 {
					fmt.Fprintf(out, "Added %s:\n", strs.Count(len(response.GetAdded()), "bundle"))
					for _, c := range response.GetAdded() {
						fmt.Fprintf(out, "  %s: %s\n", c.GetName(), cmp.Or(c.GetDescription(), "No description"))
					}
				}
				if len(response.GetRemoved()) > 0 {
					fmt.Fprintf(out, "Removed %s:\n", strs.Count(len(response.GetRemoved()), "bundle"))
					for _, c := range response.GetRemoved() {
						fmt.Fprintln(out, "  "+c.GetName())
					}
				}
				if len(response.GetAdded())+len(response.GetRemoved()) == 0 {
					_, err = fmt.Fprintf(out, "Nothing changed. %s loaded.\n", agreeing(int(response.GetTotal()), "bundle is", "bundles are"))
				} else {
					_, err = fmt.Fprintf(out, "%s loaded now.\n", agreeing(int(response.GetTotal()), "bundle is", "bundles are"))
				}
				return err
			},
		},
	)
	return root
}

// What an opt out or opt in said, marked done when it changed anything
func marked(response *skillsv1.OptOutResponse) string {
	if response.GetChanged() {
		return "✓ " + response.GetMessage()
	}
	return response.GetMessage()
}

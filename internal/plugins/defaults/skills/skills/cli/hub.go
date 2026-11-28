package cli

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	hub "github.com/nickheyer/icebath/internal/plugins/defaults/skills/skills/skills"
	skillsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/skills/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/skills/v1/skillsv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Sources and providers the hub's commands filter by
var sourceChoices = []string{"all", "official", "skills-sh", "well-known", "github", "clawhub", "lobehub", "browse-sh", "nvidia", "openai", "anthropic",
	"huggingface", "voltagent", "gstack", "minimax"}

func checkSource(source string) error {
	if !slices.Contains(sourceChoices, source) {
		return fmt.Errorf("--source takes one of %s, not “%s”.", strings.Join(sourceChoices, ", "), source)
	}
	return nil
}

// Colors of the lines of the hub by their tone
var tones = map[skillsv1.Tone][]string{skillsv1.Tone_TONE_DIM: {cli.Dim}, skillsv1.Tone_TONE_GOOD: {cli.Bold, cli.Green},
	skillsv1.Tone_TONE_WARN: {cli.Yellow}, skillsv1.Tone_TONE_BAD: {cli.Bold, cli.Red}, skillsv1.Tone_TONE_HEADING: {cli.Bold},
	skillsv1.Tone_TONE_ACCENT: {cli.Bold, cli.Cyan}}

// Prints a line of the hub in the colors of its tone
func say(c *cli.Console, tone skillsv1.Tone, format string, args ...any) {
	if len(args) > 0 {
		format = fmt.Sprintf(format, args...)
	}
	c.Say(format, tones[tone]...)
}

// Prints a line the hub's server wrote
func show(c *cli.Console, l *skillsv1.Line) {
	if l != nil {
		say(c, l.GetTone(), "%s", l.GetText())
	}
}

// A titled block of lines
func panel(c *cli.Console, title string, lines ...string) {
	say(c, skillsv1.Tone_TONE_HEADING, "%s", title)
	for _, line := range lines {
		for _, part := range strings.Split(line, "\n") {
			fmt.Fprintln(c.Out(), "  "+part)
		}
	}
}

// Text on one line, its first width characters with an ellipsis when it was longer
func truncate(text string, width int) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= width {
		return text
	}
	return strs.Head(text, width-1) + "…"
}

// Number in words
func number(n int32) string {
	return strs.Number(float64(n))
}

// How many updates wait, in a clause
func waiting(n int) string {
	switch n {
	case 0:
		return "no update is waiting"
	case 1:
		return "1 update is waiting"
	}
	return strs.Number(float64(n)) + " updates are waiting"
}

// State of a skill an update check found, in words
func checkState(status string) string {
	switch status {
	case "update_available":
		return "Update waiting"
	case "up_to_date":
		return "Up to date"
	case "orphaned":
		return "Folder missing"
	case "invalid_install":
		return "Not installed properly"
	case "unavailable":
		return "Source out of reach"
	}
	return status
}

// Trust of a row, the label given for an official skill
func trustCell(skill *skillsv1.HubSkill, official string) string {
	if skill.GetSource() == "official" {
		return official
	}
	trust := skill.GetTrust()
	if trust == "" {
		return "Unknown"
	}
	return strs.Capital(trust)
}

func hubCommands(service skillsv1connect.SkillsServiceClient) []*cobra.Command {
	return []*cobra.Command{browse(service), search(service), install(service), inspect(service), check(service), update(service), audit(service),
		uninstall(service), publish(service), snapshot(service), taps(service), trust(service, "trust"), trust(service, "untrust")}
}

func browse(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	var page, size int
	var source string
	command := &cobra.Command{
		Use:   "browse",
		Short: "Browse every skill you can install, page by page",
		Long:  "Lists the skills of every registry icebath knows, page by page. --source shows the skills of one source only.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkSource(source); err != nil {
				return err
			}
			c := cli.NewConsole(cmd)
			stream, err := service.Browse(cmd.Context(), &skillsv1.BrowseRequest{Scope: here(cmd.Context()), Page: int32(page), Size: int32(size), Source: source})
			if err != nil {
				return err
			}
			defer stream.Close()
			var done []string
			var result *skillsv1.BrowsePage
			for {
				response, err := stream.Receive()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					return err
				}
				if a := response.GetAnswered(); a != nil && c.Colored() {
					done = append(done, a.GetSource()+" "+number(a.GetCount()))
					fmt.Fprintf(c.Out(), "\r\033[KAsking the registries for skills… Answered: %s", strings.Join(done, ", "))
				}
				if response.GetPage() != nil {
					result = response.GetPage()
				}
			}
			if c.Colored() && len(done) > 0 {
				fmt.Fprint(c.Out(), "\r\033[K")
			}
			if len(result.GetSkills()) == 0 && result.GetTotal() == 0 {
				if source != "all" && !slices.Contains([]string{"official", "skills-sh", "well-known", "github", "clawhub", "lobehub", "browse-sh"}, source) {
					say(c, skillsv1.Tone_TONE_DIM, "%s has no skills in the skills hub right now.", source)
				} else {
					say(c, skillsv1.Tone_TONE_DIM, "The skills hub found no skills. The registries may be out of reach, so try again later.")
				}
				return nil
			}
			renderBrowse(c, result, source)
			return nil
		},
	}
	command.Flags().IntVar(&page, "page", 1, "Page to show")
	command.Flags().IntVar(&size, "size", 20, "Skills per page")
	command.Flags().StringVar(&source, "source", "all", "Source to list, such as nvidia or openai, or all")
	return command
}

func renderBrowse(c *cli.Console, page *skillsv1.BrowsePage, source string) {
	label := "every source"
	if source != "all" {
		label = source
	}
	fmt.Fprintln(c.Out())
	say(c, skillsv1.Tone_TONE_HEADING, "Skills of %s, page %s of %s", label, number(page.GetPage()), number(page.GetPages()))
	loaded := strs.Count(int(page.GetTotal()), "skill") + " found"
	if n := len(page.GetTimedOut()); n > 0 {
		loaded += ", and " + strs.Count(n, "source") + " still answering"
	}
	say(c, skillsv1.Tone_TONE_DIM, "%s.", loaded)
	if page.GetOfficial() > 0 && page.GetPage() == 1 {
		say(c, skillsv1.Tone_TONE_ACCENT, "%s from icebath are first.", strs.Count(int(page.GetOfficial()), "optional skill"))
	}
	fmt.Fprintln(c.Out())
	table := tabwriter.NewWriter(c.Out(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "Number\tName\tWhat it does\tFrom\tTrust\tIdentifier")
	for i, skill := range page.GetSkills() {
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n", strs.Number(float64(int(page.GetStart())+i+1)), skill.GetName(), truncate(skill.GetDescription(), 44),
			skill.GetLabel(), trustCell(skill, "Official"), skill.GetIdentifier())
	}
	table.Flush()
	var nav []string
	if page.GetPage() > 1 {
		nav = append(nav, fmt.Sprintf("--page %s for the page before", number(page.GetPage()-1)))
	}
	if page.GetPage() < page.GetPages() {
		nav = append(nav, fmt.Sprintf("--page %s for the next page", number(page.GetPage()+1)))
	}
	if len(nav) > 0 {
		fmt.Fprintln(c.Out(), "  Add "+strings.Join(nav, ", or ")+".")
	}
	if source == "all" && len(page.GetCounts()) > 0 {
		counts := slices.Clone(page.GetCounts())
		slices.SortFunc(counts, func(a, b *skillsv1.SourceCount) int { return strings.Compare(a.GetSource(), b.GetSource()) })
		var parts []string
		for _, count := range counts {
			parts = append(parts, count.GetSource()+" "+number(count.GetCount()))
		}
		say(c, skillsv1.Tone_TONE_DIM, "  Skills by source: %s.", strings.Join(parts, ", "))
	}
	if late := page.GetTimedOut(); len(late) > 0 {
		say(c, skillsv1.Tone_TONE_WARN, "  ⚠ These sources answered too slowly and were left out: %s. Run the command again to get them from the saved copy.",
			strings.Join(late, ", "))
	}
	say(c, skillsv1.Tone_TONE_DIM, "Preview a skill with icebath skills inspect <identifier>, install it with icebath skills install <identifier>, or search with "+
		"icebath skills search <query>.")
}

func search(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	var source string
	var limit int
	var asJSON bool
	command := &cobra.Command{
		Use:   "search <query>",
		Short: "Search the skill registries",
		Long: "Searches every skill registry for the words you give and lists what matches. Then preview a skill with inspect, or install it " +
			"with install.",
		Example: "  icebath skills search \"pdf forms\"\n  icebath skills search docker --source openai --json",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkSource(source); err != nil {
				return err
			}
			c := cli.NewConsole(cmd)
			if !asJSON {
				fmt.Fprintln(c.Out())
				say(c, skillsv1.Tone_TONE_HEADING, "Searching for “%s”…", args[0])
			}
			response, err := service.Search(cmd.Context(), &skillsv1.SearchRequest{Scope: here(cmd.Context()), Query: args[0], Source: source, Limit: int32(limit)})
			if err != nil {
				return err
			}
			if asJSON {
				rows := []map[string]string{}
				for _, skill := range response.GetSkills() {
					rows = append(rows, map[string]string{"name": skill.GetName(), "identifier": skill.GetIdentifier(), "source": skill.GetSource(),
						"trust_level": skill.GetTrust(), "description": skill.GetDescription()})
				}
				encoder := json.NewEncoder(c.Out())
				encoder.SetEscapeHTML(false)
				encoder.SetIndent("", "  ")
				return encoder.Encode(rows)
			}
			if len(response.GetSkills()) == 0 {
				say(c, skillsv1.Tone_TONE_DIM, "No skill matches “%s”. Try other words, or icebath skills browse to see every skill.", args[0])
				return nil
			}
			say(c, skillsv1.Tone_TONE_HEADING, "%s found:", strs.Count(len(response.GetSkills()), "skill"))
			table := tabwriter.NewWriter(c.Out(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(table, "Name\tWhat it does\tFrom\tTrust\tIdentifier")
			for _, skill := range response.GetSkills() {
				fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", skill.GetName(), truncate(skill.GetDescription(), 60), skill.GetLabel(), trustCell(skill, "Official"),
					skill.GetIdentifier())
			}
			table.Flush()
			say(c, skillsv1.Tone_TONE_DIM, "Preview a skill with icebath skills inspect <identifier>, or install it with icebath skills install <identifier>. "+
				"Add --json to get the list for a script.")
			return nil
		},
	}
	command.Flags().StringVar(&source, "source", "all", "Source to search, such as nvidia or openai, or all")
	command.Flags().IntVar(&limit, "limit", 25, "Largest number of results to show")
	command.Flags().BoolVar(&asJSON, "json", false, "Print JSON with full identifiers, for scripts")
	return command
}

// What one install asks
type installOptions struct {
	category, name string
	force, yes     bool
}

var categoryName = regexp.MustCompile(`^[a-z][a-z0-9_/-]*$`)

// Installs a skill: fetched, quarantined and scanned by the server, then the name, category and yes asked here when needed; Failed when
// it was not installed
func installSkill(c *cli.Console, service skillsv1connect.SkillsServiceClient, identifier string, o installOptions) (skillsv1.Outcome, error) {
	ctx := c.Context()
	request := &skillsv1.StageRequest{Scope: here(ctx), Identifier: identifier, Category: o.category, Name: o.name, Force: o.force, Yes: o.yes}
	for {
		stream, err := service.Stage(ctx, request)
		if err != nil {
			return skillsv1.Outcome_OUTCOME_FAILED, err
		}
		final, err := drain(stream.Receive, func(r *skillsv1.StageResponse) (*skillsv1.Line, bool) { return r.GetLine(), r.GetDone() }, c)
		stream.Close()
		if err != nil {
			return skillsv1.Outcome_OUTCOME_FAILED, err
		}
		pending := final.GetPending()
		switch final.GetOutcome() {
		case skillsv1.Outcome_OUTCOME_NEEDS_NAME:
			fmt.Fprintln(c.Out())
			say(c, skillsv1.Tone_TONE_WARN, "⚠ The SKILL.md at %s names no skill, and its address gives no usable name either.", pending.GetUrl())
			say(c, skillsv1.Tone_TONE_HEADING, "Give the skill a name of lower case letters, digits, dashes and underscores, starting with a letter.")
			name, ok := c.Answer("Name: ")
			if !ok || name == "" {
				say(c, skillsv1.Tone_TONE_DIM, "Nothing was installed.")
				return skillsv1.Outcome_OUTCOME_FAILED, nil
			}
			request.Name = name
			continue
		case skillsv1.Outcome_OUTCOME_STAGED:
			return commit(c, service, pending)
		}
		return final.GetOutcome(), nil
	}
}

// Reads a stream of lines to its last message, printing each line
func drain[R any](receive func() (*R, error), parts func(*R) (*skillsv1.Line, bool), c *cli.Console) (*R, error) {
	for {
		response, err := receive()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("The server stopped answering before it was done. Run the command again.")
		}
		if err != nil {
			return nil, err
		}
		line, done := parts(response)
		show(c, line)
		if done {
			return response, nil
		}
	}
}

// Asks the category of a skill at a URL, then for the yes to install; installs it or drops it
func commit(c *cli.Console, service skillsv1connect.SkillsServiceClient, pending *skillsv1.Pending) (skillsv1.Outcome, error) {
	ctx := c.Context()
	discard := func() (skillsv1.Outcome, error) {
		_, err := service.Discard(ctx, &skillsv1.DiscardRequest{Id: pending.GetId()})
		return skillsv1.Outcome_OUTCOME_UNSPECIFIED, err
	}
	category := pending.GetCategory()
	if pending.GetAskCategory() {
		fmt.Fprintln(c.Out())
		if existing := pending.GetCategories(); len(existing) > 0 {
			say(c, skillsv1.Tone_TONE_HEADING, "Type a category to put the skill in, one you have or a new one, or press enter to put it in no category.")
			say(c, skillsv1.Tone_TONE_DIM, "Your categories: %s", strings.Join(existing, ", "))
		} else {
			say(c, skillsv1.Tone_TONE_HEADING, "Type a category to put the skill in, or press enter to put it straight in %s/<name>/.", pending.GetSkillsDir())
		}
		answer, _ := c.Answer("Category: ")
		if answer != "" && !categoryName.MatchString(answer) {
			say(c, skillsv1.Tone_TONE_DIM, "“%s” can't be a category, so the skill goes in no category.", answer)
			answer = ""
		}
		category = answer
	}
	where := category
	if where != "" {
		where += "/"
	}
	files := fmt.Sprintf("Its files go in %s/%s%s/.", pending.GetSkillsDir(), where, pending.GetName())
	fmt.Fprintln(c.Out())
	if pending.GetOfficial() {
		panel(c, "Optional skill from icebath", "icebath makes this skill. It comes with icebath, and it is off until you install it.", "",
			"Installing copies it into your skills folder, where the agent can use it.", "", files)
	} else {
		panel(c, "A skill from someone else", "You install this skill at your own risk.", "",
			"A skill can hold instructions that change what the agent does, shell commands and scripts.",
			"The scan does not catch everything, so read the files once they are installed.", "", files)
	}
	if !c.Agree(fmt.Sprintf("Install the skill %s, so the agent can use it? [y/N] ", pending.GetName()), false) {
		say(c, skillsv1.Tone_TONE_DIM, "Nothing was installed.")
		return discard()
	}
	stream, err := service.Commit(ctx, &skillsv1.CommitRequest{Scope: here(ctx), Id: pending.GetId(), Category: category})
	if err != nil {
		return skillsv1.Outcome_OUTCOME_FAILED, err
	}
	defer stream.Close()
	final, err := drain(stream.Receive, func(r *skillsv1.CommitResponse) (*skillsv1.Line, bool) { return r.GetLine(), r.GetDone() }, c)
	if err != nil {
		return skillsv1.Outcome_OUTCOME_FAILED, err
	}
	return final.GetOutcome(), nil
}

func install(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	var o installOptions
	command := &cobra.Command{
		Use:   "install <identifier>",
		Short: "Install a skill",
		Long: "Installs a skill by its identifier, such as openai/skills/skill-creator, by its name, or from the address of a SKILL.md file. " +
			"icebath scans the skill for risks first and asks before it installs.",
		Example: "  icebath skills install openai/skills/skill-creator\n" +
			"  icebath skills install https://example.com/skills/report/SKILL.md --name report",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			outcome, err := installSkill(cli.NewConsole(cmd), service, args[0], o)
			if err != nil {
				return err
			}
			if outcome == skillsv1.Outcome_OUTCOME_FAILED {
				return cli.Exit(1)
			}
			return nil
		},
	}
	command.Flags().StringVar(&o.category, "category", "", "Category folder to install the skill into")
	command.Flags().StringVar(&o.name, "name", "", "Name to give the skill, for a SKILL.md that names none")
	command.Flags().BoolVar(&o.force, "force", false, "Install even when the scan blocks the skill")
	command.Flags().BoolVarP(&o.yes, "yes", "y", false, "Go ahead without asking first")
	return command
}

func inspect(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <identifier>",
		Short: "Preview a skill without installing it",
		Long:  "Shows a skill's name, description, source and trust, and the start of its SKILL.md, without installing it.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cli.NewConsole(cmd)
			stream, err := service.Inspect(cmd.Context(), &skillsv1.InspectRequest{Scope: here(cmd.Context()), Identifier: args[0]})
			if err != nil {
				return err
			}
			defer stream.Close()
			final, err := drain(stream.Receive, func(r *skillsv1.InspectResponse) (*skillsv1.Line, bool) { return r.GetLine(), r.GetDone() }, c)
			if err != nil {
				return err
			}
			skill := final.GetSkill()
			if skill == nil {
				return nil
			}
			fmt.Fprintln(c.Out())
			info := []string{"Name: " + skill.GetName(), "What it does: " + skill.GetDescription(), "From: " + skill.GetSource(),
				"Trust: " + trustCell(skill, "Official"), "Identifier: " + skill.GetIdentifier()}
			if tags := skill.GetTags(); len(tags) > 0 {
				info = append(info, "Tags: "+strings.Join(tags, ", "))
			}
			panel(c, "The skill "+skill.GetName(), append(info, skill.GetDetails()...)...)
			if preview := final.GetPreview(); preview != "" {
				panel(c, "The start of its SKILL.md", preview)
				say(c, skillsv1.Tone_TONE_DIM, "  Install it with icebath skills install %s.", skill.GetIdentifier())
			}
			fmt.Fprintln(c.Out())
			return nil
		},
	}
}

func check(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	return &cobra.Command{
		Use:   "check [name]",
		Short: "Check your hub skills for updates",
		Long:  "Checks the skills you installed from the hub, or the one you name, for newer versions.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cli.NewConsole(cmd)
			response, err := service.Check(cmd.Context(), &skillsv1.CheckRequest{Scope: here(cmd.Context()), Name: strs.First(args)})
			if err != nil {
				return err
			}
			rows := response.GetSkills()
			if len(rows) == 0 {
				say(c, skillsv1.Tone_TONE_DIM, "You have no skills from the hub to check. Install one with icebath skills install <identifier>.")
				return nil
			}
			say(c, skillsv1.Tone_TONE_HEADING, "Updates of your skills from the hub")
			table := tabwriter.NewWriter(c.Out(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(table, "Name\tFrom\tState")
			updates := 0
			var orphaned []string
			for _, row := range rows {
				fmt.Fprintf(table, "%s\t%s\t%s\n", row.GetName(), row.GetSource(), checkState(row.GetStatus()))
				switch row.GetStatus() {
				case "update_available":
					updates++
				case "orphaned":
					orphaned = append(orphaned, row.GetName())
				}
			}
			table.Flush()
			say(c, skillsv1.Tone_TONE_DIM, "Checked %s, and %s.", strs.Count(len(rows), "skill"), waiting(updates))
			if len(orphaned) > 0 {
				say(c, skillsv1.Tone_TONE_WARN, "⚠ The folders of these skills are missing or are not folders: %s. When the folder is gone, remove the old "+
					"record with icebath skills uninstall <name>.", strings.Join(orphaned, ", "))
			}
			return nil
		},
	}
}

func update(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	var force bool
	command := &cobra.Command{
		Use:   "update [name]",
		Short: "Update the skills you installed from the hub",
		Long:  "Updates every skill you installed from the hub, or the one you name. Skills you edited are skipped unless you pass --force.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			stream, err := service.Update(cmd.Context(), &skillsv1.UpdateRequest{Scope: here(cmd.Context()), Name: strs.First(args), Force: force})
			if err != nil {
				return err
			}
			defer stream.Close()
			final, err := drain(stream.Receive, func(r *skillsv1.UpdateResponse) (*skillsv1.Line, bool) { return r.GetLine(), r.GetDone() }, cli.NewConsole(cmd))
			if err != nil {
				return err
			}
			if !final.GetOk() {
				return cli.Exit(1)
			}
			return nil
		},
	}
	command.Flags().BoolVar(&force, "force", false, "Also update the skills you edited, replacing your changes")
	return command
}

func audit(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	var deep bool
	command := &cobra.Command{
		Use:   "audit [name]",
		Short: "Scan your hub skills for risks again",
		Long: "Runs the security scan again on the skills you installed from the hub, or on the one you name. With --deep, audit also " +
			"analyzes the code of Python files.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			stream, err := service.Audit(cmd.Context(), &skillsv1.AuditRequest{Scope: here(cmd.Context()), Name: strs.First(args), Deep: deep})
			if err != nil {
				return err
			}
			defer stream.Close()
			_, err = drain(stream.Receive, func(r *skillsv1.AuditResponse) (*skillsv1.Line, bool) { return r.GetLine(), r.GetDone() }, cli.NewConsole(cmd))
			return err
		},
	}
	command.Flags().BoolVar(&deep, "deep", false, "Also analyze the code of Python files in depth")
	return command
}

func uninstall(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	var yes bool
	command := &cobra.Command{
		Use:   "uninstall <name>",
		Short: "Remove a skill you installed from the hub",
		Long:  "Deletes the skill from this profile, after asking.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cli.NewConsole(cmd)
			if !yes {
				fmt.Fprintln(c.Out())
				if !c.Agree(fmt.Sprintf("Uninstall the skill %s? Its files are deleted from this profile. [y/N] ", args[0]), false) {
					say(c, skillsv1.Tone_TONE_DIM, "Nothing was uninstalled.")
					return nil
				}
			}
			response, err := service.Uninstall(cmd.Context(), &skillsv1.UninstallRequest{Scope: here(cmd.Context()), Name: args[0]})
			if err != nil {
				return err
			}
			if !response.GetOk() {
				say(c, skillsv1.Tone_TONE_BAD, "✗ %s", response.GetMessage())
				return cli.Exit(1)
			}
			say(c, skillsv1.Tone_TONE_GOOD, "✓ %s", response.GetMessage())
			return nil
		},
	}
	command.Flags().BoolVarP(&yes, "yes", "y", false, "Go ahead without asking first")
	return command
}

func publish(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	var to, repo string
	command := &cobra.Command{
		Use:   "publish <skill_path>",
		Short: "Publish a skill to a registry",
		Long: "Publishes a skill folder, given by its path in the skills folder or a full path, once it passes the security scan. --to " +
			"picks GitHub or ClawHub.",
		Example: "  icebath skills publish my-skill --to github --repo you/skills",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if to != "github" && to != "clawhub" {
				return fmt.Errorf("--to takes github or clawhub, not “%s”.", to)
			}
			stream, err := service.Publish(cmd.Context(), &skillsv1.PublishRequest{Scope: here(cmd.Context()), Path: args[0], To: to, Repo: repo})
			if err != nil {
				return err
			}
			defer stream.Close()
			_, err = drain(stream.Receive, func(r *skillsv1.PublishResponse) (*skillsv1.Line, bool) { return r.GetLine(), r.GetDone() }, cli.NewConsole(cmd))
			return err
		},
	}
	command.Flags().StringVar(&to, "to", "github", "Registry to publish to: github or clawhub")
	command.Flags().StringVar(&repo, "repo", "", "GitHub repository to publish to, such as openai/skills")
	return command
}

func snapshot(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	root := &cobra.Command{Use: "snapshot", Short: "Save your skills to a file, or install them from one",
		Long: "A snapshot lists the skills you installed from the hub and your skill sources, so you can set up the same skills in another " +
			"profile or on another computer.",
		Args: cobra.NoArgs}
	export := &cobra.Command{
		Use:     "export <output>",
		Short:   "Save your hub skills and sources to a file",
		Long:    "Writes the skills you installed from the hub and your skill sources to a JSON file, or to standard output when the file is -.",
		Example: "  icebath skills snapshot export skills.json",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cli.NewConsole(cmd)
			response, err := service.Export(cmd.Context(), &skillsv1.ExportRequest{Scope: here(cmd.Context())})
			if err != nil {
				return err
			}
			if args[0] == "-" {
				_, err = io.WriteString(c.Out(), response.GetSnapshot())
				return err
			}
			if err := os.WriteFile(args[0], []byte(response.GetSnapshot()), 0o644); err != nil {
				return err
			}
			say(c, skillsv1.Tone_TONE_GOOD, "✓ Saved the snapshot to %s.", filepath.Clean(args[0]))
			say(c, skillsv1.Tone_TONE_DIM, "It lists %s and %s.", strs.Count(int(response.GetSkills()), "skill"), strs.Count(int(response.GetTaps()), "skill source"))
			return nil
		},
	}
	var force bool
	load := &cobra.Command{
		Use:     "import <input>",
		Short:   "Install the skills that a snapshot file lists",
		Long:    "Adds the skill sources and installs the skills that a snapshot file lists.",
		Example: "  icebath skills snapshot import skills.json",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return importSnapshot(cli.NewConsole(cmd), service, args[0], force)
		},
	}
	load.Flags().BoolVar(&force, "force", false, "Install even when the scan advises caution")
	root.AddCommand(export, load)
	return root
}

// Adds a snapshot's taps and installs its skills, each asked about unless forced; fails when one was not installed
func importSnapshot(c *cli.Console, service skillsv1connect.SkillsServiceClient, input string, force bool) error {
	path := filepath.Clean(input)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		say(c, skillsv1.Tone_TONE_BAD, "✗ There is no file at %s.", path)
		return cli.Exit(1)
	}
	if err != nil {
		return err
	}
	if !utf8.Valid(data) {
		return fmt.Errorf("The snapshot %s can't be read, because it is not text.", path)
	}
	snap, err := hub.ReadSnapshot(data)
	if err != nil {
		say(c, skillsv1.Tone_TONE_BAD, "✗ %s is not a snapshot, because it does not hold valid JSON.", path)
		return cli.Exit(1)
	}
	ctx := c.Context()
	if len(snap.Taps) > 0 {
		for _, tap := range snap.Taps {
			if tap.Repo != "" {
				if _, err := service.Tap(ctx, &skillsv1.TapRequest{Scope: here(ctx), Repo: tap.Repo}); err != nil {
					return err
				}
			}
		}
		say(c, skillsv1.Tone_TONE_DIM, "Added %s.", strs.Count(len(snap.Taps), "skill source"))
	}
	if len(snap.Skills) == 0 {
		say(c, skillsv1.Tone_TONE_DIM, "The snapshot lists no skills to install.")
		return nil
	}
	say(c, skillsv1.Tone_TONE_HEADING, "Installing %s from the snapshot…", strs.Count(len(snap.Skills), "skill"))
	var failed []string
	for _, skill := range snap.Skills {
		if skill.Identifier == "" {
			say(c, skillsv1.Tone_TONE_WARN, "⚠ Skipped %s, which has no identifier.", cmp.Or(skill.Name, "a skill without a name"))
			continue
		}
		label := cmp.Or(skill.Name, skill.Identifier)
		say(c, skillsv1.Tone_TONE_HEADING, "%s", label)
		outcome, err := installSkill(c, service, skill.Identifier, installOptions{category: skill.Category, force: force})
		if err != nil {
			return err
		}
		if outcome == skillsv1.Outcome_OUTCOME_FAILED {
			failed = append(failed, label)
		}
	}
	if len(failed) > 0 {
		say(c, skillsv1.Tone_TONE_BAD, "✗ Not every skill of the snapshot was installed. These were not, for the reasons above: %s.", strings.Join(failed, ", "))
		return cli.Exit(1)
	}
	say(c, skillsv1.Tone_TONE_GOOD, "✓ Installed every skill of the snapshot.")
	return nil
}

func taps(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	root := &cobra.Command{Use: "tap", Short: "Add or remove skill sources",
		Long: "A skill source is a GitHub repository that holds skills. icebath searches your sources along with the registries it knows.",
		Args: cobra.NoArgs}
	list := &cobra.Command{
		Use:   "list",
		Short: "List your skill sources",
		Long:  "Lists the GitHub repositories you added as skill sources, with the folder in each that holds the skills.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			response, err := service.Taps(cmd.Context(), &skillsv1.TapsRequest{Scope: here(cmd.Context())})
			if err != nil {
				return err
			}
			if len(response.GetTaps()) == 0 {
				say(c, skillsv1.Tone_TONE_DIM, "You added no skill sources, so icebath searches only the registries it knows. Add one with icebath skills tap add <repo>.")
				return nil
			}
			say(c, skillsv1.Tone_TONE_HEADING, "Your skill sources")
			table := tabwriter.NewWriter(c.Out(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(table, "Repository\tFolder")
			for _, tap := range response.GetTaps() {
				fmt.Fprintf(table, "%s\t%s\n", tap.GetRepo(), cmp.Or(tap.GetPath(), "skills/"))
			}
			return table.Flush()
		},
	}
	change := func(use, short, long, example, done, unchanged string, remove bool) *cobra.Command {
		return &cobra.Command{
			Use:     use,
			Short:   short,
			Long:    long,
			Example: example,
			Args:    cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				c := cli.NewConsole(cmd)
				response, err := service.Tap(cmd.Context(), &skillsv1.TapRequest{Scope: here(cmd.Context()), Repo: args[0], Remove: remove})
				if err != nil {
					return err
				}
				if response.GetChanged() {
					say(c, skillsv1.Tone_TONE_GOOD, done, args[0])
				} else if remove {
					say(c, skillsv1.Tone_TONE_BAD, unchanged, args[0])
				} else {
					say(c, skillsv1.Tone_TONE_WARN, unchanged, args[0])
				}
				return nil
			},
		}
	}
	root.AddCommand(list, change("add <repo>", "Add a GitHub repository as a skill source",
		"Adds the repository, given as owner/name, to your skill sources.", "  icebath skills tap add openai/skills", "✓ Added the skill source %s.",
		"%s is a skill source already.", false),
		change("remove <name>", "Remove a skill source", "Removes the repository, given as owner/name, from your skill sources.",
			"  icebath skills tap remove openai/skills", "✓ Removed the skill source %s.", "✗ %s is not one of your skill sources.", true))
	return root
}

func trust(service skillsv1connect.SkillsServiceClient, action string) *cobra.Command {
	short := "Load the skills that a project keeps in its own folder"
	long := "Trusts a project, so icebath loads the skills it keeps in .icebath/skills or .agents/skills."
	if action == "untrust" {
		short = "Stop loading the skills of a project"
		long = "Takes back your trust in a project, so icebath no longer loads the skills it keeps in its own folder."
	}
	return &cobra.Command{
		Use:   action + " [path]",
		Short: short,
		Long:  long + " The project is the folder you give, or else the git checkout you are in.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cli.NewConsole(cmd)
			response, err := service.Trust(cmd.Context(), &skillsv1.TrustRequest{Scope: here(cmd.Context()), Path: strs.First(args), Untrust: action == "untrust"})
			if err != nil {
				return err
			}
			for _, line := range response.GetLines() {
				show(c, line)
			}
			return nil
		},
	}
}

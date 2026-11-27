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
		return fmt.Errorf("--source must be one of %s", strings.Join(sourceChoices, ", "))
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
	say(c, skillsv1.Tone_TONE_HEADING, "── %s ──", title)
	for _, line := range lines {
		for _, part := range strings.Split(line, "\n") {
			fmt.Fprintln(c.Out(), "  "+part)
		}
	}
}

// Text on one line, its first width characters with an ellipsis when it was longer
func truncate(text string, width int) string {
	return strs.Clip(strings.Join(strings.Fields(text), " "), width)
}

// Trust of a row, the label given for an official skill
func trustCell(skill *skillsv1.HubSkill, official string) string {
	if skill.GetSource() == "official" {
		return official
	}
	return skill.GetTrust()
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
		Short: "Browse all available skills (paginated)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkSource(source); err != nil {
				return err
			}
			c := cli.NewConsole(cmd)
			stream, err := service.Browse(cmd.Context(), &skillsv1.BrowseRequest{Scope: here(), Page: int32(page), Size: int32(size), Source: source})
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
					done = append(done, fmt.Sprintf("%s (%d)", a.GetSource(), a.GetCount()))
					fmt.Fprintf(c.Out(), "\r\033[KFetching skills from registries...  done: %s", strings.Join(done, ", "))
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
					say(c, skillsv1.Tone_TONE_DIM, "No skills found for provider '%s'.", source)
				} else {
					say(c, skillsv1.Tone_TONE_DIM, "No skills found in the Skills Hub.")
				}
				return nil
			}
			renderBrowse(c, result, source)
			return nil
		},
	}
	command.Flags().IntVar(&page, "page", 1, "Page number")
	command.Flags().IntVar(&size, "size", 20, "Results per page")
	command.Flags().StringVar(&source, "source", "all", "Filter by source or provider (e.g. nvidia, openai)")
	return command
}

func renderBrowse(c *cli.Console, page *skillsv1.BrowsePage, source string) {
	loaded := fmt.Sprintf("%d skills loaded", page.GetTotal())
	if n := len(page.GetTimedOut()); n > 0 {
		loaded += fmt.Sprintf(", %d source(s) still loading", n)
	}
	label := source
	if source == "all" {
		label = "all sources"
	}
	fmt.Fprintln(c.Out())
	say(c, skillsv1.Tone_TONE_HEADING, "Skills Hub — Browse — %s  (%s, page %d/%d)", label, loaded, page.GetPage(), page.GetPages())
	if page.GetOfficial() > 0 && page.GetPage() == 1 {
		say(c, skillsv1.Tone_TONE_ACCENT, "★ %d official optional skill(s) from icebath", page.GetOfficial())
	}
	fmt.Fprintln(c.Out())
	table := tabwriter.NewWriter(c.Out(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "#\tNAME\tDESCRIPTION\tSOURCE\tTRUST\tIDENTIFIER")
	for i, skill := range page.GetSkills() {
		fmt.Fprintf(table, "%d\t%s\t%s\t%s\t%s\t%s\n", int(page.GetStart())+i+1, skill.GetName(), truncate(skill.GetDescription(), 44), skill.GetLabel(),
			trustCell(skill, "★ official"), skill.GetIdentifier())
	}
	table.Flush()
	var nav []string
	if page.GetPage() > 1 {
		nav = append(nav, fmt.Sprintf("--page %d ← prev", page.GetPage()-1))
	}
	if page.GetPage() < page.GetPages() {
		nav = append(nav, fmt.Sprintf("--page %d → next", page.GetPage()+1))
	}
	if len(nav) > 0 {
		fmt.Fprintln(c.Out(), "  "+strings.Join(nav, " | "))
	}
	if source == "all" && len(page.GetCounts()) > 0 {
		counts := slices.Clone(page.GetCounts())
		slices.SortFunc(counts, func(a, b *skillsv1.SourceCount) int { return strings.Compare(a.GetSource(), b.GetSource()) })
		var parts []string
		for _, count := range counts {
			parts = append(parts, fmt.Sprintf("%s: %d", count.GetSource(), count.GetCount()))
		}
		say(c, skillsv1.Tone_TONE_DIM, "  Sources: %s", strings.Join(parts, ", "))
	}
	if late := page.GetTimedOut(); len(late) > 0 {
		say(c, skillsv1.Tone_TONE_WARN, "  ⚡ Slow sources skipped: %s — run again for cached results", strings.Join(late, ", "))
	}
	say(c, skillsv1.Tone_TONE_DIM, "Tip: 'icebath skills inspect <identifier>' to preview, 'icebath skills install <identifier>' to install, 'icebath skills search <query>' to search deeper")
}

func search(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	var source string
	var limit int
	var asJSON bool
	command := &cobra.Command{
		Use:   "search <query>",
		Short: "Search skill registries",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkSource(source); err != nil {
				return err
			}
			c := cli.NewConsole(cmd)
			if !asJSON {
				fmt.Fprintln(c.Out())
				say(c, skillsv1.Tone_TONE_HEADING, "Searching for: %s", args[0])
			}
			response, err := service.Search(cmd.Context(), &skillsv1.SearchRequest{Scope: here(), Query: args[0], Source: source, Limit: int32(limit)})
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
				say(c, skillsv1.Tone_TONE_DIM, "No skills found matching your query.")
				return nil
			}
			say(c, skillsv1.Tone_TONE_HEADING, "Skills Hub — %d result(s)", len(response.GetSkills()))
			table := tabwriter.NewWriter(c.Out(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(table, "NAME\tDESCRIPTION\tSOURCE\tTRUST\tIDENTIFIER")
			for _, skill := range response.GetSkills() {
				fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", skill.GetName(), truncate(skill.GetDescription(), 60), skill.GetLabel(), trustCell(skill, "official"),
					skill.GetIdentifier())
			}
			table.Flush()
			say(c, skillsv1.Tone_TONE_DIM, "Use: icebath skills inspect <identifier> to preview, icebath skills install <identifier> to install (--json for scripting)")
			return nil
		},
	}
	command.Flags().StringVar(&source, "source", "all", "Filter by source or provider (e.g. nvidia, openai)")
	command.Flags().IntVar(&limit, "limit", 25, "Max results")
	command.Flags().BoolVar(&asJSON, "json", false, "Output JSON instead of a table (full identifiers, scripting-friendly)")
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
	request := &skillsv1.StageRequest{Scope: here(), Identifier: identifier, Category: o.category, Name: o.name, Force: o.force, Yes: o.yes}
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
			say(c, skillsv1.Tone_TONE_WARN, "The SKILL.md at %s doesn't declare a `name:` in its frontmatter,", pending.GetUrl())
			say(c, skillsv1.Tone_TONE_WARN, "and the URL path doesn't produce a valid identifier either.")
			say(c, skillsv1.Tone_TONE_HEADING, "Enter a skill name: (lowercase letters, digits, hyphens, underscores; starts with a letter)")
			name, ok := c.Answer("Name: ")
			if !ok || name == "" {
				say(c, skillsv1.Tone_TONE_DIM, "Installation cancelled.")
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
			return nil, errors.New("the server ended the stream early")
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
			say(c, skillsv1.Tone_TONE_HEADING, "Pick a category (reuse an existing bucket, type a new one, or press Enter to install flat)")
			say(c, skillsv1.Tone_TONE_DIM, "Existing: %s", strings.Join(existing, ", "))
		} else {
			say(c, skillsv1.Tone_TONE_HEADING, "Category (optional — press Enter to install flat at %s/<name>/)", pending.GetSkillsDir())
		}
		answer, _ := c.Answer("Category: ")
		if answer != "" && !categoryName.MatchString(answer) {
			say(c, skillsv1.Tone_TONE_DIM, "Invalid category %q — installing flat.", answer)
			answer = ""
		}
		category = answer
	}
	where := category
	if where != "" {
		where += "/"
	}
	files := fmt.Sprintf("Files will be at: %s/%s%s/", pending.GetSkillsDir(), where, pending.GetName())
	fmt.Fprintln(c.Out())
	if pending.GetOfficial() {
		panel(c, "Official Skill", "This is an official optional skill maintained by icebath.", "", "It ships with icebath but is not activated by default.",
			"Installing will copy it to your skills directory where the agent can use it.", "", files)
	} else {
		panel(c, "Disclaimer", "You are installing a third-party skill at your own risk.", "",
			"External skills can contain instructions that influence agent behavior,", "shell commands, and scripts. Even after automated scanning, you should",
			"review the installed files before use.", "", files)
	}
	say(c, skillsv1.Tone_TONE_HEADING, "Install '%s'?", pending.GetName())
	if !c.Agree("Confirm [y/N]: ", false) {
		say(c, skillsv1.Tone_TONE_DIM, "Installation cancelled.")
		return discard()
	}
	stream, err := service.Commit(ctx, &skillsv1.CommitRequest{Scope: here(), Id: pending.GetId(), Category: category})
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
		Long:  "Install a skill by its identifier (e.g. openai/skills/skill-creator), its name, or a direct HTTP(S) URL to a SKILL.md file",
		Args:  cobra.ExactArgs(1),
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
	command.Flags().StringVar(&o.category, "category", "", "Category folder to install into")
	command.Flags().StringVar(&o.name, "name", "", "Override the skill name (useful when installing from a URL whose SKILL.md has no `name:` frontmatter)")
	command.Flags().BoolVar(&o.force, "force", false, "Install despite blocked scan verdict")
	command.Flags().BoolVarP(&o.yes, "yes", "y", false, "Skip confirmation prompt")
	return command
}

func inspect(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <identifier>",
		Short: "Preview a skill without installing",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cli.NewConsole(cmd)
			stream, err := service.Inspect(cmd.Context(), &skillsv1.InspectRequest{Scope: here(), Identifier: args[0]})
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
			info := []string{"Name: " + skill.GetName(), "Description: " + skill.GetDescription(), "Source: " + skill.GetSource(),
				"Trust: " + trustCell(skill, "official"), "Identifier: " + skill.GetIdentifier()}
			if tags := skill.GetTags(); len(tags) > 0 {
				info = append(info, "Tags: "+strings.Join(tags, ", "))
			}
			panel(c, "Skill: "+skill.GetName(), append(info, skill.GetDetails()...)...)
			if preview := final.GetPreview(); preview != "" {
				panel(c, "SKILL.md Preview", preview)
				say(c, skillsv1.Tone_TONE_DIM, "  icebath skills install <id> to install")
			}
			fmt.Fprintln(c.Out())
			return nil
		},
	}
}

func check(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	return &cobra.Command{
		Use:   "check [name]",
		Short: "Check installed hub skills for updates",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cli.NewConsole(cmd)
			response, err := service.Check(cmd.Context(), &skillsv1.CheckRequest{Scope: here(), Name: strs.First(args)})
			if err != nil {
				return err
			}
			rows := response.GetSkills()
			if len(rows) == 0 {
				say(c, skillsv1.Tone_TONE_DIM, "No hub-installed skills to check.")
				return nil
			}
			say(c, skillsv1.Tone_TONE_HEADING, "Skill Updates")
			table := tabwriter.NewWriter(c.Out(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(table, "NAME\tSOURCE\tSTATUS")
			updates := 0
			var orphaned []string
			for _, row := range rows {
				fmt.Fprintf(table, "%s\t%s\t%s\n", row.GetName(), row.GetSource(), row.GetStatus())
				switch row.GetStatus() {
				case "update_available":
					updates++
				case "orphaned":
					orphaned = append(orphaned, row.GetName())
				}
			}
			table.Flush()
			say(c, skillsv1.Tone_TONE_DIM, "%d update(s) available across %d checked skill(s)", updates, len(rows))
			if len(orphaned) > 0 {
				say(c, skillsv1.Tone_TONE_WARN, "Orphaned: %s — lock-file entries whose local directory is missing or replaced by a non-directory. "+
					"For missing directories, remove the stale entry with: icebath skills uninstall <name>", strings.Join(orphaned, ", "))
			}
			return nil
		},
	}
}

func update(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	var force bool
	command := &cobra.Command{
		Use:   "update [name]",
		Short: "Update installed hub skills",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			stream, err := service.Update(cmd.Context(), &skillsv1.UpdateRequest{Scope: here(), Name: strs.First(args), Force: force})
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
	command.Flags().BoolVar(&force, "force", false, "Overwrite skills you have edited locally (they are skipped by default)")
	return command
}

func audit(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	var deep bool
	command := &cobra.Command{
		Use:   "audit [name]",
		Short: "Re-scan installed hub skills",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			stream, err := service.Audit(cmd.Context(), &skillsv1.AuditRequest{Scope: here(), Name: strs.First(args), Deep: deep})
			if err != nil {
				return err
			}
			defer stream.Close()
			_, err = drain(stream.Receive, func(r *skillsv1.AuditResponse) (*skillsv1.Line, bool) { return r.GetLine(), r.GetDone() }, cli.NewConsole(cmd))
			return err
		},
	}
	command.Flags().BoolVar(&deep, "deep", false, "Run AST-level analysis on Python files (opt-in diagnostic)")
	return command
}

func uninstall(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	var yes bool
	command := &cobra.Command{
		Use:   "uninstall <name>",
		Short: "Remove a hub-installed skill",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cli.NewConsole(cmd)
			if !yes {
				fmt.Fprintln(c.Out())
				say(c, skillsv1.Tone_TONE_HEADING, "Uninstall '%s'?", args[0])
				if !c.Agree("Confirm [y/N]: ", false) {
					say(c, skillsv1.Tone_TONE_DIM, "Cancelled.")
					return nil
				}
			}
			response, err := service.Uninstall(cmd.Context(), &skillsv1.UninstallRequest{Scope: here(), Name: args[0]})
			if err != nil {
				return err
			}
			if !response.GetOk() {
				say(c, skillsv1.Tone_TONE_BAD, "Error: %s", response.GetMessage())
				return cli.Exit(1)
			}
			say(c, skillsv1.Tone_TONE_GOOD, "%s", response.GetMessage())
			return nil
		},
	}
	command.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation prompt")
	return command
}

func publish(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	var to, repo string
	command := &cobra.Command{
		Use:   "publish <skill_path>",
		Short: "Publish a skill to a registry",
		Long:  "Publish a skill folder, by its path in the skills folder or an absolute one, once its security scan passes",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if to != "github" && to != "clawhub" {
				return errors.New("--to must be github or clawhub")
			}
			stream, err := service.Publish(cmd.Context(), &skillsv1.PublishRequest{Scope: here(), Path: args[0], To: to, Repo: repo})
			if err != nil {
				return err
			}
			defer stream.Close()
			_, err = drain(stream.Receive, func(r *skillsv1.PublishResponse) (*skillsv1.Line, bool) { return r.GetLine(), r.GetDone() }, cli.NewConsole(cmd))
			return err
		},
	}
	command.Flags().StringVar(&to, "to", "github", "Target registry: github or clawhub")
	command.Flags().StringVar(&repo, "repo", "", "Target GitHub repo (e.g. openai/skills)")
	return command
}

func snapshot(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	root := &cobra.Command{Use: "snapshot", Short: "Export/import skill configurations", Args: cobra.NoArgs}
	export := &cobra.Command{
		Use:   "export <output>",
		Short: "Export installed skills to a file",
		Long:  "Export the hub's installed skills and sources to a JSON file, or to standard output for -",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cli.NewConsole(cmd)
			response, err := service.Export(cmd.Context(), &skillsv1.ExportRequest{Scope: here()})
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
			say(c, skillsv1.Tone_TONE_GOOD, "Snapshot exported: %s", filepath.Clean(args[0]))
			say(c, skillsv1.Tone_TONE_DIM, "%d skill(s), %d tap(s)", response.GetSkills(), response.GetTaps())
			return nil
		},
	}
	var force bool
	load := &cobra.Command{
		Use:   "import <input>",
		Short: "Import and install skills from a file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return importSnapshot(cli.NewConsole(cmd), service, args[0], force)
		},
	}
	load.Flags().BoolVar(&force, "force", false, "Force install despite caution verdict")
	root.AddCommand(export, load)
	return root
}

// Adds a snapshot's taps and installs its skills, each asked about unless forced; fails when one was not installed
func importSnapshot(c *cli.Console, service skillsv1connect.SkillsServiceClient, input string, force bool) error {
	path := filepath.Clean(input)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		say(c, skillsv1.Tone_TONE_BAD, "Error: File not found: %s", path)
		return cli.Exit(1)
	}
	if err != nil {
		return err
	}
	if !utf8.Valid(data) {
		return errors.New("the snapshot is not valid UTF-8")
	}
	snap, err := hub.ReadSnapshot(data)
	if err != nil {
		say(c, skillsv1.Tone_TONE_BAD, "Error: Invalid JSON in %s", path)
		return cli.Exit(1)
	}
	ctx := c.Context()
	if len(snap.Taps) > 0 {
		for _, tap := range snap.Taps {
			if tap.Repo != "" {
				if _, err := service.Tap(ctx, &skillsv1.TapRequest{Scope: here(), Repo: tap.Repo}); err != nil {
					return err
				}
			}
		}
		say(c, skillsv1.Tone_TONE_DIM, "Restored %d tap(s)", len(snap.Taps))
	}
	if len(snap.Skills) == 0 {
		say(c, skillsv1.Tone_TONE_DIM, "No skills in snapshot to install.")
		return nil
	}
	say(c, skillsv1.Tone_TONE_HEADING, "Importing %d skill(s) from snapshot...", len(snap.Skills))
	var failed []string
	for _, skill := range snap.Skills {
		if skill.Identifier == "" {
			say(c, skillsv1.Tone_TONE_WARN, "Skipping entry with no identifier: %s", cmp.Or(skill.Name, "?"))
			continue
		}
		label := cmp.Or(skill.Name, skill.Identifier)
		say(c, skillsv1.Tone_TONE_HEADING, "--- %s ---", label)
		outcome, err := installSkill(c, service, skill.Identifier, installOptions{category: skill.Category, force: force})
		if err != nil {
			return err
		}
		if outcome == skillsv1.Outcome_OUTCOME_FAILED {
			failed = append(failed, label)
		}
	}
	if len(failed) > 0 {
		say(c, skillsv1.Tone_TONE_BAD, "Snapshot import incomplete. Not installed: %s (see the messages above).", strings.Join(failed, ", "))
		return cli.Exit(1)
	}
	say(c, skillsv1.Tone_TONE_GOOD, "Snapshot import complete.")
	return nil
}

func taps(service skillsv1connect.SkillsServiceClient) *cobra.Command {
	root := &cobra.Command{Use: "tap", Short: "Manage skill sources", Args: cobra.NoArgs}
	list := &cobra.Command{
		Use:   "list",
		Short: "List configured taps",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			response, err := service.Taps(cmd.Context(), &skillsv1.TapsRequest{Scope: here()})
			if err != nil {
				return err
			}
			if len(response.GetTaps()) == 0 {
				say(c, skillsv1.Tone_TONE_DIM, "No custom taps configured. Using default sources only.")
				return nil
			}
			say(c, skillsv1.Tone_TONE_HEADING, "Configured Taps")
			table := tabwriter.NewWriter(c.Out(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(table, "REPO\tPATH")
			for _, tap := range response.GetTaps() {
				fmt.Fprintf(table, "%s\t%s\n", tap.GetRepo(), cmp.Or(tap.GetPath(), "skills/"))
			}
			return table.Flush()
		},
	}
	change := func(use, short, done, unchanged string, remove bool) *cobra.Command {
		return &cobra.Command{
			Use:   use,
			Short: short,
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				c := cli.NewConsole(cmd)
				response, err := service.Tap(cmd.Context(), &skillsv1.TapRequest{Scope: here(), Repo: args[0], Remove: remove})
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
	root.AddCommand(list, change("add <repo>", "Add a GitHub repo as skill source", "Added tap: %s", "Tap already exists: %s", false),
		change("remove <name>", "Remove a tap", "Removed tap: %s", "Error: Tap not found: %s", true))
	return root
}

func trust(service skillsv1connect.SkillsServiceClient, action string) *cobra.Command {
	short := "Trust a project so its repo-local skills (./.icebath/skills, ./.agents/skills) load"
	if action == "untrust" {
		short = "Revoke project-skill trust for a repo"
	}
	return &cobra.Command{
		Use:   action + " [path]",
		Short: short,
		Long:  short + ". The project root is the path given, else the git checkout the current folder is in.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cli.NewConsole(cmd)
			response, err := service.Trust(cmd.Context(), &skillsv1.TrustRequest{Scope: here(), Path: strs.First(args), Untrust: action == "untrust"})
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

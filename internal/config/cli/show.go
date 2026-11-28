package cli

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	skillsconfig "github.com/nickheyer/icebath/internal/skills/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
)

// Where a value comes from, as config show says it after a value not taken from the file or the default
var sourceWords = map[configv1.Source]string{configv1.Source_SOURCE_ENVIRONMENT: "from an environment variable",
	configv1.Source_SOURCE_MANAGED: "set by your administrator"}

// Rows of a name and what is said of it, the names in one column as wide as the longest and a third cell dimmed after the second
type table struct {
	c    *cli.Console
	rows [][3]string
}

func (t *table) add(name, value, note string) { t.rows = append(t.rows, [3]string{name, value, note}) }

func (t *table) print() {
	names := 0
	for _, row := range t.rows {
		names = max(names, ansi.StringWidth(row[0]))
	}
	for _, row := range t.rows {
		line := "  " + row[0] + strings.Repeat(" ", names-ansi.StringWidth(row[0])) + "   " + row[1]
		if row[2] != "" {
			line += "   " + t.c.Paint(row[2], cli.Dim)
		}
		fmt.Fprintln(t.c.Out(), line)
	}
	t.rows = t.rows[:0]
}

// Prints the profile's settings: what an administrator or a package manager decides, the files, the API keys set, then every section
// with its own settings and those under it that are set, the chat apps, what the skills ask for, and how to change a setting
func showConfig(cmd *cobra.Command, service configv1connect.ConfigServiceClient, profile string) error {
	ctx := cmd.Context()
	files, err := service.Files(ctx, &configv1.FilesRequest{Profile: profile})
	if err != nil {
		return refused(cmd, err)
	}
	listed, err := service.List(ctx, &configv1.ListRequest{Profile: profile, Bare: true})
	if err != nil {
		return refused(cmd, err)
	}
	extra, err := service.Show(ctx, &configv1.ShowRequest{Profile: profile})
	if err != nil {
		return refused(cmd, err)
	}
	c := cli.NewConsole(cmd)
	rows := &table{c: c}
	heading := func(title string) {
		c.Blank()
		c.Say(title, cli.Cyan, cli.Bold)
	}
	c.Blank()
	c.Say("Settings of the profile "+listed.GetProfile(), cli.Bold)
	if keys, env := files.GetManagedKeys(), files.GetManagedEnv(); len(keys) > 0 || len(env) > 0 {
		c.Blank()
		c.Say("⚠ Your administrator sets some of these in "+files.GetManagedDir()+". They can't be changed here.", cli.Yellow)
		if len(keys) > 0 {
			c.Say("  Settings: "+strings.Join(keys, ", "), cli.Yellow)
		}
		if len(env) > 0 {
			c.Say("  Variables: "+strings.Join(env, ", "), cli.Yellow)
		}
	}
	if system := files.GetManagedSystem(); system != "" {
		c.Blank()
		c.Say("⚠ This copy of icebath is managed by "+system+", which keeps its settings. Change them through "+system+".", cli.Yellow)
	}
	heading("Files")
	rows.add("Settings file", cmp.Or(files.GetOwn(), files.GetRemote()), "")
	// The dated copies icebath keeps of the settings file, which a broken one is put back from
	if copies := files.GetBackups(); copies != "" {
		rows.add("Copies of it", copies, "")
	}
	rows.add("Shared settings file", files.GetShared(), "")
	rows.add("Keys file", files.GetEnv(), "")
	rows.add("Program folder", extra.GetInstall(), "")
	rows.print()
	heading("API keys")
	notSet := c.Paint("Not set", cli.Dim)
	for _, key := range extra.GetKeys() {
		rows.add(key.GetLabel(), cmp.Or(key.GetValue(), notSet), "")
	}
	rows.print()
	showSections(c, listed)
	heading("Chat apps")
	for _, app := range extra.GetPlatforms() {
		state := c.Paint("Not set up", cli.Dim)
		if app.GetSet() {
			state = "Set up"
		}
		rows.add(app.GetLabel(), state, "")
	}
	rows.print()
	if skills := extra.GetSkills(); len(skills) > 0 {
		heading("Skill options")
		for _, skill := range skills {
			rows.add(skill.GetLabel(), cmp.Or(skill.GetValue(), notSet), "the "+skill.GetOwner()+" skill asks for this")
		}
		rows.print()
	}
	c.Blank()
	c.Say("To change a setting:", cli.Dim)
	c.Say("  icebath config set <key> <value>", cli.Dim)
	c.Say("  icebath config edit", cli.Dim)
	c.Say("  /settings in the app", cli.Dim)
	c.Blank()
	return nil
}

// Every section at the top with its own settings and those of the sections under it that are set, each of those under the trail of
// sections that leads to it
func showSections(c *cli.Console, listed *configv1.ListResponse) {
	sections := map[string]*configv1.Section{}
	for _, section := range listed.GetSections() {
		sections[section.GetKey()] = section
	}
	// Top section a section is under, and the titles from below the top down to the section
	trail := func(key string) (string, []string) {
		var titles []string
		for sections[key] != nil && sections[key].GetParent() != "" {
			titles = append([]string{sections[key].GetTitle()}, titles...)
			key = sections[key].GetParent()
		}
		return key, titles
	}
	rows := map[string]*table{}
	for _, setting := range listed.GetSettings() {
		// The values skills declare have a heading of their own
		if setting.GetInternal() || setting.GetSection() == skillsconfig.Values {
			continue
		}
		top, under := trail(setting.GetSection())
		if len(under) > 0 && setting.GetSource() == configv1.Source_SOURCE_DEFAULT {
			continue
		}
		if rows[top] == nil {
			rows[top] = &table{c: c}
		}
		rows[top].add(strings.Join(append(under, setting.GetTitle()), " › "), setting.GetShown(), sourceWords[setting.GetSource()])
	}
	for _, section := range listed.GetSections() {
		if section.GetParent() != "" || rows[section.GetKey()] == nil {
			continue
		}
		c.Blank()
		c.Say(section.GetTitle(), cli.Cyan, cli.Bold)
		rows[section.GetKey()].print()
	}
}

package cli

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/config"
	skillsconfig "github.com/nickheyer/icebath/internal/skills/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
)

// Value of a setting as config show prints it
func words(setting *configv1.Setting) string {
	var value any
	yaml.Unmarshal([]byte(setting.GetValue()), &value)
	if setting.GetSecret() {
		if setting.GetValue() == "" {
			return "not set"
		}
		return "set"
	}
	switch v := value.(type) {
	case nil:
		if unset := setting.GetUnset(); unset != "" {
			return "not set (" + unset + ")"
		}
		return "not set"
	case bool:
		if v {
			return "on"
		}
		return "off"
	case string:
		if v == "" {
			return "not set"
		}
		return v
	case []any:
		if len(v) == 0 {
			return "none"
		}
		items := make([]string, len(v))
		for i, item := range v {
			items[i] = fmt.Sprint(item)
		}
		return strings.Join(items, ", ")
	case map[string]any:
		return fmt.Sprintf("%d entries", len(v))
	}
	if unit := setting.GetUnit(); unit != "" {
		return config.WithUnit(value, unit)
	}
	return fmt.Sprint(value)
}

// Where a value comes from, as config show notes it beside a value not taken from the file or the default
var sourceWords = map[configv1.Source]string{configv1.Source_SOURCE_ENVIRONMENT: "env", configv1.Source_SOURCE_MANAGED: "managed"}

// Prints the profile's configuration: the administrator's banner, the files, the keys set, then every group of settings with its own and
// the rest that are set
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
	section := func(title string) {
		c.Blank()
		c.Say("◆ "+title, cli.Cyan, cli.Bold)
	}
	c.Blank()
	c.Say("┌─────────────────────────────────────────────────────────┐", cli.Cyan)
	c.Say("│              󰆧 Icebath Configuration                    │", cli.Cyan)
	c.Say("└─────────────────────────────────────────────────────────┘", cli.Cyan)
	if keys, env := files.GetManagedKeys(), files.GetManagedEnv(); len(keys) > 0 || len(env) > 0 {
		c.Blank()
		c.Say("  ⚷ Some settings are managed by your administrator ("+files.GetManagedDir()+") and cannot be changed", cli.Yellow, cli.Bold)
		if len(keys) > 0 {
			c.Say("    Managed config keys: "+strings.Join(keys, ", "), cli.Yellow)
		}
		if len(env) > 0 {
			c.Say("    Managed env keys: "+strings.Join(env, ", "), cli.Yellow)
		}
	}
	if system := files.GetManagedSystem(); system != "" {
		c.Blank()
		c.Say("  ⚷ This install is managed by "+system+"; change its settings through it", cli.Yellow, cli.Bold)
	}
	section("Paths")
	own := files.GetOwn()
	if own == "" {
		own = files.GetRemote()
	}
	fmt.Fprintf(c.Out(), "  Config:       %s\n", own)
	fmt.Fprintf(c.Out(), "  Shared:       %s\n", files.GetShared())
	fmt.Fprintf(c.Out(), "  Secrets:      %s\n", files.GetEnv())
	fmt.Fprintf(c.Out(), "  Install:      %s\n", extra.GetInstall())
	section("API Keys")
	notSet := c.Paint("(not set)", cli.Dim)
	for _, key := range extra.GetKeys() {
		fmt.Fprintf(c.Out(), "  %-14s %s\n", key.GetLabel(), cmp.Or(key.GetValue(), notSet))
	}
	showGroups(c, listed)
	section("Messaging Platforms")
	for _, platform := range extra.GetPlatforms() {
		state := c.Paint("not configured", cli.Dim)
		if platform.GetSet() {
			state = "configured"
		}
		fmt.Fprintf(c.Out(), "  %-13s %s\n", platform.GetLabel()+":", state)
	}
	if skills := extra.GetSkills(); len(skills) > 0 {
		section("Skill Settings")
		for _, skill := range skills {
			fmt.Fprintf(c.Out(), "  %-20s %s  %s\n", skill.GetLabel(), cmp.Or(skill.GetValue(), notSet), c.Paint("["+skill.GetOwner()+"]", cli.Dim))
		}
	}
	c.Blank()
	c.Say(strings.Repeat("─", 60), cli.Dim)
	c.Say("  icebath config edit    # Edit config file", cli.Dim)
	c.Say("  icebath config set <key> <value>", cli.Dim)
	c.Say("  icebath, type /settings   # Set up in the terminal UI", cli.Dim)
	c.Blank()
	return nil
}

// Every group of settings, a section at the top: its own settings, and those of the sections under it that are set
func showGroups(c *cli.Console, listed *configv1.ListResponse) {
	parents := map[string]string{}
	for _, section := range listed.GetSections() {
		parents[section.GetKey()] = section.GetParent()
	}
	top := func(key string) string {
		for parents[key] != "" {
			key = parents[key]
		}
		return key
	}
	rows := map[string][]string{}
	for _, setting := range listed.GetSettings() {
		// The values skills declare have a group of their own
		if setting.GetInternal() || setting.GetSection() == skillsconfig.Values {
			continue
		}
		group := top(setting.GetSection())
		if setting.GetSection() != group && setting.GetSource() == configv1.Source_SOURCE_DEFAULT {
			continue
		}
		value := words(setting)
		if note := sourceWords[setting.GetSource()]; note != "" {
			value += c.Paint("  ("+note+")", cli.Dim)
		}
		rows[group] = append(rows[group], fmt.Sprintf("  %-22s %s", setting.GetTitle()+":", value))
	}
	for _, section := range listed.GetSections() {
		if section.GetParent() != "" || len(rows[section.GetKey()]) == 0 {
			continue
		}
		c.Blank()
		c.Say("◆ "+section.GetTitle(), cli.Cyan, cli.Bold)
		for _, row := range rows[section.GetKey()] {
			fmt.Fprintln(c.Out(), row)
		}
	}
}

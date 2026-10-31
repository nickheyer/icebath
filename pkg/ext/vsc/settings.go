package vsc

import (
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Key of the settings panel and its slash command
const SettingsKey = "vsc"

// Sheet of every bridge setting, each saved to the settings file and put in force the moment it changes
func newSettingsCard(env *cards.Env) wm.Card {
	warned := false
	return cards.NewSettings(env, cards.SettingsSpec{
		Title:  "VS CODE SETTINGS",
		Key:    "ext:vsc:settings",
		Glyph:  env.Theme.Glyphs.Settings,
		Status: settingsFile,
		Fields: []cards.Field{
			{Key: "enabled", Label: "enabled", Group: "bridge", Kind: cards.FieldChoice, Choices: []string{"true", "false"}, Help: "Open the file changes of this TUI as diffs in the VS Code window it runs in"},
			{Key: "auto_install", Label: "auto install", Group: "bridge", Kind: cards.FieldChoice, Choices: []string{"true", "false"}, Help: "Install or update the icebath extension when a VS Code terminal runs the TUI without it, once per version, /vsc-install does it by hand"},
			{Key: "log", Label: "log", Group: "bridge", Kind: cards.FieldText, Optional: true, Placeholder: "~/icebath-vsc.log", Help: "File every protocol line in and out is appended to, no traffic log when empty"},
		},
		Values: func() map[string]string { return Current().Values() },
		Apply: func(key, value string) tea.Cmd {
			c := Current()
			if err := c.Set(key, value); err != nil {
				return wm.Notify(err.Error(), wm.Warn)
			}
			if err := Apply(c); err != nil {
				return wm.Notify(err.Error(), wm.Error)
			}
			return nil
		},
		Init: func() tea.Cmd {
			err := LoadError()
			if err == nil || warned {
				return nil
			}
			warned = true
			return wm.Notify(err.Error(), wm.Warn)
		},
	})
}

// Where the settings live with the home directory as a tilde, or why that cannot be found
func settingsFile() string {
	path, err := Path()
	if err != nil {
		return err.Error()
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" || !strings.HasPrefix(path, home+string(filepath.Separator)) {
		return path
	}
	return "~" + strings.TrimPrefix(path, home)
}

package app

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/app"
	appconfig "github.com/nickheyer/icebath/internal/app/config"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

type screens struct{}

func Register(host *app.App) {
	host.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	settings := configv1connect.NewConfigServiceClient(conn)
	return []tui.Screen{
		{
			ID:    "palette",
			Short: "Search all commands",
			Slash: []string{"palette"},
			Open:  func(kit *tui.Kit, _ string) tea.Cmd { return kit.WM.OpenPalette() },
		},
		generalPage(settings),
		{
			ID:    "skin",
			Group: tui.GroupApp,
			Order: 70,
			Title: "Color theme",
			Short: "Change the color theme",
			Slash: []string{"skin"},
			Usage: "[name]",
			Open: func(kit *tui.Kit, name string) tea.Cmd {
				if name == "" {
					return kit.Open(configapp.Edit, appconfig.Skin)
				}
				if _, known := theme.Named(name); !known {
					return kit.WM.Toast("There is no color theme named "+name+". The color themes are "+strings.Join(theme.Names(), ", ")+".", wm.Warn, wm.DefaultToastTTL)
				}
				return keep(kit, settings, appconfig.Skin, strconv.Quote(name))
			},
			Follows: map[string]func(*tui.Kit, string) tea.Cmd{
				appconfig.Skin: func(_ *tui.Kit, value string) tea.Cmd {
					var name string
					yaml.Unmarshal([]byte(value), &name)
					return tui.Theme(name)
				},
			},
		},
		{
			ID:      "keys",
			Group:   tui.GroupApp,
			Order:   60,
			Title:   "Keyboard shortcuts",
			Short:   "Change the keys that run each action",
			Slash:   []string{"keys"},
			Aliases: []string{"keybindings"},
			Usage:   "[words to search|reload|reset|option on|off]",
			Open:    func(kit *tui.Kit, argument string) tea.Cmd { return keysCommand(kit, settings, argument) },
			Follows: map[string]func(*tui.Kit, string) tea.Cmd{
				appconfig.Option: func(_ *tui.Kit, value string) tea.Cmd {
					var on bool
					yaml.Unmarshal([]byte(value), &on)
					keys.SetOption(on)
					return nil
				},
			},
		},
		{
			ID:    "redraw",
			Group: tui.GroupApp,
			Order: 80,
			Title: "Screen redraw",
			Short: "Draw the whole screen again",
			Slash: []string{"redraw"},
			Open: func(kit *tui.Kit, _ string) tea.Cmd {
				kit.WM.Repaint()
				return tea.ClearScreen
			},
		},
	}
}

// Saves the value of a setting of the profile in use as its control in Settings does, saying in a toast what that control says
func keep(kit *tui.Kit, settings configv1connect.ConfigServiceClient, key, value string) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		_, err := settings.Set(kit.Env.Ctx, &configv1.SetRequest{Key: key, Value: value})
		var now *configv1.Setting
		if err == nil {
			var response *configv1.GetResponse
			if response, err = settings.Get(kit.Env.Ctx, &configv1.GetRequest{Key: key}); err == nil {
				now = response.GetSetting()
			}
		}
		return func() tea.Cmd {
			if err != nil {
				return kit.WM.Toast("The change was not saved. The error was: "+strings.TrimSuffix(tui.Reason(err), ".")+".", wm.Error, wm.DefaultToastTTL)
			}
			return tea.Batch(kit.Changed(key, now.GetValue()), kit.WM.Toast(now.GetTitle()+" is now "+now.GetShown()+".", wm.Success, wm.DefaultToastTTL))
		}
	})
}

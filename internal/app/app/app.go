package app

import (
	"strconv"

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
	wear := func(kit *tui.Kit, name string) tea.Cmd {
		value := strconv.Quote(name)
		return tui.Async(func() func() tea.Cmd {
			_, err := settings.Set(kit.Env.Ctx, &configv1.SetRequest{Key: appconfig.Skin, Value: value})
			return func() tea.Cmd {
				if err != nil {
					return kit.WM.Toast("Couldn't save skin: "+tui.Reason(err), wm.Error, wm.DefaultToastTTL)
				}
				return kit.Changed(appconfig.Skin, value)
			}
		})
	}
	return []tui.Screen{
		{
			ID:    "palette",
			Short: "Search all commands",
			Slash: []string{"palette"},
			Open:  func(kit *tui.Kit, _ string) tea.Cmd { return kit.WM.OpenPalette() },
		},
		{
			ID:    "skin",
			Title: "Skin",
			Short: "Change the color theme",
			Slash: []string{"skin"},
			Usage: "[name]",
			Open: func(kit *tui.Kit, name string) tea.Cmd {
				if name == "" {
					return kit.Open(configapp.Edit, appconfig.Skin)
				}
				if _, known := theme.Named(name); !known {
					return kit.WM.Toast("No skin named "+name, wm.Warn, wm.DefaultToastTTL)
				}
				return wear(kit, name)
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
			ID:    "keys",
			Title: "Keys",
			Short: "View and change keyboard shortcuts",
			Slash: []string{"keys", "keybindings"},
			Usage: "[search|reload|reset|option on|off]",
			Open: func(kit *tui.Kit, argument string) tea.Cmd {
				return (&bindings{kit: kit, settings: settings}).open(argument)
			},
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
			Title: "Redraw",
			Short: "Redraw the screen",
			Slash: []string{"redraw"},
			Open: func(kit *tui.Kit, _ string) tea.Cmd {
				kit.WM.Repaint()
				return tea.ClearScreen
			},
		},
	}
}

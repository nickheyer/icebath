// /autoprune in this app: alone it opens Session storage, on and off are the shortcut to Delete old sessions with the note its row gives,
// and later puts the question about a low disk off for a week
package app

import (
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

type screens struct{}

func Register(host *app.App) {
	host.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	settings := configv1connect.NewConfigServiceClient(conn)
	return []tui.Screen{{
		ID:    "autoprune",
		Short: "Delete old sessions when the server starts",
		Slash: []string{"autoprune"},
		Usage: "[on|off|later]",
		Open:  func(kit *tui.Kit, argument string) tea.Cmd { return autoprune(kit, settings, argument) },
	}}
}

func autoprune(kit *tui.Kit, settings configv1connect.ConfigServiceClient, argument string) tea.Cmd {
	switch word := strings.ToLower(strings.TrimSpace(argument)); word {
	case "":
		return kit.Open(configapp.Settings, sessionsconfig.Key)
	case "on", "off":
		return keep(kit, settings, sessionsconfig.AutoPrune, strconv.FormatBool(word == "on"), "")
	case "later":
		return keep(kit, settings, sessionsconfig.PruneAsk, strconv.Quote(sessions.Later(time.Now())), "icebath asks again in a week if the disk is still low.")
	}
	return kit.WM.Toast("/autoprune takes on, off or later.", wm.Warn, wm.DefaultToastTTL)
}

// Saves a setting as its row in Settings does, saying what that row says, or told when given
func keep(kit *tui.Kit, settings configv1connect.ConfigServiceClient, key, value, told string) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		_, err := settings.Set(kit.Env.Ctx, &configv1.SetRequest{Key: key, Value: value})
		var now *configv1.Setting
		if err == nil && told == "" {
			var response *configv1.GetResponse
			if response, err = settings.Get(kit.Env.Ctx, &configv1.GetRequest{Key: key}); err == nil {
				now = response.GetSetting()
				told = now.GetTitle() + " is now " + now.GetShown() + "."
				if now.GetApply() == configv1.Apply_APPLY_RESTART {
					told += " Takes effect after icebath restarts."
				}
			}
		}
		return func() tea.Cmd {
			if err != nil {
				return kit.WM.Toast("The change was not saved. The error was: "+strings.TrimSuffix(tui.Reason(err), ".")+".", wm.Error, wm.DefaultToastTTL)
			}
			if now != nil {
				return tea.Batch(kit.Changed(key, now.GetValue()), kit.WM.Toast(told, wm.Success, wm.DefaultToastTTL))
			}
			return kit.WM.Toast(told, wm.Success, wm.DefaultToastTTL)
		}
	})
}

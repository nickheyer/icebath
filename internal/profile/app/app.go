package app

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/internal/config"
	profileconfig "github.com/nickheyer/icebath/internal/profile/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	profilev1 "github.com/nickheyer/icebath/pkg/gen/icebath/profile/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/profile/v1/profilev1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Screen of every profile
const Screen = "profiles"

type screens struct{}

func Register(host *app.App) {
	host.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := profilev1connect.NewProfileServiceClient(conn)
	settings := configv1connect.NewConfigServiceClient(conn)
	return []tui.Screen{{
		ID:    Screen,
		Title: "Profiles",
		Short: "Switch between sets of providers, accounts and settings",
		Slash: []string{"profiles"},
		Usage: "[name]",
		Open: func(kit *tui.Kit, name string) tea.Cmd {
			p := &profiles{kit: kit, service: service, settings: settings}
			if name = strings.TrimSpace(name); name != "" {
				return p.use(name)
			}
			return p.open()
		},
	}}
}

type profiles struct {
	kit      *tui.Kit
	service  profilev1connect.ProfileServiceClient
	settings configv1connect.ConfigServiceClient
	hub      *cards.Hub
	list     *cards.List
	rows     []*profilev1.Profile
	loaded   bool
	err      error
}

func (p *profiles) open() tea.Cmd {
	t := p.kit.Env.Theme
	p.list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Profiles",
		Glyph:   t.Glyphs.Settings,
		Key:     "profiles",
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search profiles",
		Columns: []render.Column{{}, {Flex: 1}, {}},
		Rows: func() [][]string {
			rows := make([][]string, len(p.rows))
			for i, entry := range p.rows {
				mark, about := "", ""
				if entry.GetActive() {
					mark, about = t.Glyphs.Live, render.Good("In use", t)
				}
				if entry.GetDefault() {
					about = strings.TrimSpace(about + "  " + render.Dim("default", t))
				}
				rows[i] = []string{mark, entry.GetName(), about}
			}
			return rows
		},
		Detail: func(i int) []string {
			if p.rows[i].GetDefault() {
				return []string{"Always there, and where settings are kept until you make another profile", render.Dim("New chats use the profile in use; /new @name opens one on another", t)}
			}
			return []string{"Its own providers, accounts, settings and sessions; those of the app and server are shared", render.Dim("/new @"+p.rows[i].GetName()+" opens a chat on it, and /profile switches a chat's session", t)}
		},
		Empty: func() string { return tui.Waiting(p.loaded, p.err, "No profiles") },
		Free:  map[string]bool{"hub.add": true, "hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			hints := []wm.Hint{}
			if i >= 0 {
				if !p.rows[i].GetActive() {
					hints = append(hints, wm.Hint{Action: "hub.use", Label: "use"})
				}
				hints = append(hints, wm.Hint{Action: "hub.edit", Label: "copy"})
				if !p.rows[i].GetActive() && !p.rows[i].GetDefault() {
					hints = append(hints, wm.Hint{Action: "hub.remove", Label: "delete"})
				}
			}
			return append(hints, wm.Hint{Action: "hub.add", Label: "new"})
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				if p.rows[i].GetActive() {
					return nil
				}
				return p.use(p.rows[i].GetName())
			},
			"hub.add":  func(*cards.List, int) tea.Cmd { return p.create("") },
			"hub.edit": func(_ *cards.List, i int) tea.Cmd { return p.create(p.rows[i].GetName()) },
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				entry := p.rows[i]
				if entry.GetActive() || entry.GetDefault() {
					return nil
				}
				l.Confirm("Delete profile "+entry.GetName()+" with its providers, accounts and sessions?", func() tea.Cmd {
					return tui.Call(p.hub, func() (*profilev1.DeleteResponse, error) {
						return p.service.Delete(p.kit.Env.Ctx, &profilev1.DeleteRequest{Name: entry.GetName()})
					}, func(*profilev1.DeleteResponse) tea.Cmd {
						return tea.Batch(p.hub.Done("Deleted "+entry.GetName()), p.load())
					})
				})
				return nil
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return p.load() },
		},
	})
	p.list.ToggleDetail()
	p.hub = p.kit.Hub(Screen, p.list)
	return tea.Batch(p.kit.WM.ShowOverlay(p.hub), p.load())
}

func (p *profiles) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := p.service.List(p.kit.Env.Ctx, &profilev1.ListRequest{})
		return func() tea.Cmd {
			p.rows, p.err, p.loaded = response.GetProfiles(), err, true
			p.list.Refresh()
			return nil
		}
	})
}

// Asks the name of a new profile, one starting with the settings of the profile named from when it is given
func (p *profiles) create(from string) tea.Cmd {
	heading := "New profile"
	if from != "" {
		heading = "Copy of " + from
	}
	field := cards.Field{Label: "Name", Help: "Lowercase, used for its folder and with /profiles", Check: func(value string) string {
		if !config.Named(strings.TrimSpace(value)) {
			return "Use lowercase letters, digits, dashes or underscores"
		}
		return ""
	}}
	return p.kit.Ask(p.hub, heading, field, "Make", func(name string) tea.Cmd {
		name = strings.TrimSpace(name)
		return tui.Call(p.hub, func() (*profilev1.CreateResponse, error) {
			return p.service.Create(p.kit.Env.Ctx, &profilev1.CreateRequest{Name: name, From: from})
		}, func(*profilev1.CreateResponse) tea.Cmd {
			return tea.Batch(p.hub.Done("Made "+name), p.load())
		})
	})
}

// Uses the profile, then tells every screen the settings it follows as the profile has them
func (p *profiles) use(name string) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		ctx := p.kit.Env.Ctx
		_, err := p.service.Use(ctx, &profilev1.UseRequest{Name: name})
		values := map[string]string{}
		if err == nil {
			for _, key := range append(tui.Followed(p.kit.Screens()), profileconfig.Active) {
				if response, err := p.settings.Get(ctx, &configv1.GetRequest{Key: key}); err == nil {
					values[key] = response.GetSetting().GetValue()
				}
			}
		}
		return func() tea.Cmd {
			if err != nil {
				return p.fail(tui.Reason(err))
			}
			cmds := []tea.Cmd{p.kit.WM.Toast("Using profile "+name, wm.Success, wm.DefaultToastTTL)}
			for key, value := range values {
				cmds = append(cmds, p.kit.Changed(key, value))
			}
			if p.hub != nil && p.hub.Shown() {
				cmds = append(cmds, p.load())
			}
			return tea.Batch(cmds...)
		}
	})
}

func (p *profiles) fail(text string) tea.Cmd {
	if p.hub != nil && p.hub.Shown() {
		return p.hub.Fail(text)
	}
	return p.kit.WM.Toast(text, wm.Error, wm.DefaultToastTTL)
}

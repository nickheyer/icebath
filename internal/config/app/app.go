package app

import (
	"context"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

const masked = "••••"

type screens struct{}

func Register(host *app.App) {
	host.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := configv1connect.NewConfigServiceClient(conn)
	return []tui.Screen{{
		ID:    "settings",
		Title: "Settings",
		Short: "Every setting and its value",
		Open: func(kit *tui.Kit) tea.Cmd {
			return (&settings{kit: kit, service: service}).open()
		},
	}}
}

type settings struct {
	kit     *tui.Kit
	service configv1connect.ConfigServiceClient
	hub     *cards.Hub
	rows    []*configv1.Setting
	loaded  bool
	err     error
}

func (s *settings) open() tea.Cmd {
	list := cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   "Settings",
		Key:     "settings",
		Context: cards.HubContext,
		Primary: "hub.use",
		Columns: []render.Column{{Title: "Setting"}, {Title: "Value", Flex: 1}},
		Rows: func() [][]string {
			rows := make([][]string, len(s.rows))
			for i, setting := range s.rows {
				rows[i] = []string{setting.GetKey(), setting.GetValue()}
			}
			return rows
		},
		Empty: func() string {
			switch {
			case s.err != nil:
				return s.err.Error()
			case !s.loaded:
				return "Loading…"
			}
			return "No settings"
		},
		Free: map[string]bool{"hub.add": true, "hub.refresh": true},
		Hints: func(*cards.List, int) []wm.Hint {
			return []wm.Hint{{Action: "hub.use", Label: "edit"}, {Action: "hub.add", Label: "set another"}, {Action: "hub.refresh"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use":  func(_ *cards.List, i int) tea.Cmd { return s.edit(s.rows[i]) },
			"hub.edit": func(_ *cards.List, i int) tea.Cmd { return s.edit(s.rows[i]) },
			"hub.add":  func(*cards.List, int) tea.Cmd { return s.edit(nil) },
			"hub.refresh": func(*cards.List, int) tea.Cmd {
				return s.load()
			},
		},
	})
	s.hub = s.kit.Hub("settings", list)
	return tea.Batch(s.kit.WM.ShowOverlay(s.hub), s.load())
}

func (s *settings) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := s.service.List(s.kit.Env.Ctx, &configv1.ListRequest{})
		return func() tea.Cmd {
			s.rows, s.err, s.loaded = response.GetSettings(), err, true
			return nil
		}
	})
}

func (s *settings) edit(setting *configv1.Setting) tea.Cmd {
	title, fields := "Set a setting", []cards.Field{{ID: "key", Label: "Key", Required: true, Placeholder: "plugins.custom.providers.local.base_url"}}
	value := cards.Field{ID: "value", Label: "Value", Help: "A value in YAML. null puts the default back.", Required: true}
	if setting != nil {
		title, fields = setting.GetKey(), nil
		if value.Value = setting.GetValue(); value.Value == masked {
			value.Kind, value.Value = cards.FieldSecret, ""
		}
	}
	return s.hub.Push(cards.NewForm(s.kit.Env, cards.FormSpec{
		Title:  title,
		Fields: append(fields, value),
		Submit: "Save",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			key := values["key"]
			if setting != nil {
				key = setting.GetKey()
			}
			s.hub.Busy("Saving…")
			return tui.Async(func() func() tea.Cmd {
				err := s.set(s.kit.Env.Ctx, key, values["value"])
				return func() tea.Cmd {
					if err != nil {
						return s.hub.Fail(err.Error())
					}
					return tea.Batch(s.hub.Pop(), s.hub.Done(key+" saved"), s.load())
				}
			})
		},
		Cancel: func(*cards.Form) tea.Cmd { return s.hub.Pop() },
	}))
}

func (s *settings) set(ctx context.Context, key, value string) error {
	_, err := s.service.Set(ctx, &configv1.SetRequest{Key: key, Value: value})
	return err
}

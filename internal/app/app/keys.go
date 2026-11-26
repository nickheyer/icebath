package app

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"go.yaml.in/yaml/v3"

	appconfig "github.com/nickheyer/icebath/internal/app/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

type bindings struct {
	kit      *tui.Kit
	settings configv1connect.ConfigServiceClient
	hub      *cards.Hub
	list     *cards.List
	rows     []keys.Action
	titles   map[string]string
}

func (b *bindings) open(argument string) tea.Cmd {
	bound := b.kit.Env.Keys
	switch words := strings.Fields(argument); {
	case len(words) == 1 && words[0] == "reset":
		bound.ResetAll()
		return tea.Batch(b.save(), b.kit.WM.Toast("All keys reset to defaults", wm.Success, wm.DefaultToastTTL))
	case len(words) == 1 && words[0] == "reload":
		return tui.Async(func() func() tea.Cmd {
			response, err := b.settings.Get(b.kit.Env.Ctx, &configv1.GetRequest{Key: appconfig.Keys})
			saved := map[string][]string{}
			if err == nil {
				err = yaml.Unmarshal([]byte(response.GetSetting().GetValue()), &saved)
			}
			return func() tea.Cmd {
				if err != nil {
					return b.kit.WM.Toast(tui.Reason(err), wm.Error, wm.DefaultToastTTL)
				}
				bound.ResetAll()
				for id, chords := range saved {
					if err := bound.Set(id, chords); err != nil {
						return b.kit.WM.Toast(err.Error(), wm.Error, wm.DefaultToastTTL)
					}
				}
				return b.kit.WM.Toast("Keys reloaded", wm.Success, wm.DefaultToastTTL)
			}
		})
	case len(words) == 2 && words[0] == "option" && (words[1] == "on" || words[1] == "off"):
		value := strconv.FormatBool(words[1] == "on")
		return tui.Async(func() func() tea.Cmd {
			_, err := b.settings.Set(b.kit.Env.Ctx, &configv1.SetRequest{Key: appconfig.Option, Value: value})
			return func() tea.Cmd {
				if err != nil {
					return b.kit.WM.Toast(tui.Reason(err), wm.Error, wm.DefaultToastTTL)
				}
				return tea.Batch(b.kit.Changed(appconfig.Option, value), b.kit.WM.Toast("Option as alt: "+words[1], wm.Success, wm.DefaultToastTTL))
			}
		})
	}
	b.titles = map[string]string{}
	for _, command := range b.kit.WM.Commands() {
		if command.Title != "" {
			b.titles[command.ID] = command.TitleText()
		}
	}
	b.rows = bound.Actions()
	rank := func(action keys.Action) int {
		switch {
		case action.ID == "palette":
			return 0
		case strings.HasPrefix(action.ID, "open."):
			return 1
		case action.Context == keys.Global:
			return 2
		}
		return 3
	}
	slices.SortStableFunc(b.rows, func(a, c keys.Action) int { return cmp.Compare(rank(a), rank(c)) })
	b.list = cards.NewList(b.kit.Env, cards.ListSpec{
		Title:   "Keys",
		Glyph:   b.kit.Env.Theme.Glyphs.Keys,
		Key:     "keys",
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search keys",
		Columns: []render.Column{{Title: "Command", Flex: 1}, {Title: "Key"}, {Title: "Where"}},
		Yield:   []int{2},
		Rows: func() [][]string {
			rows := make([][]string, len(b.rows))
			for i, action := range b.rows {
				rows[i] = []string{b.title(action), bound.DisplayAll(action.ID), keys.ContextDocs[action.Context]}
			}
			return rows
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return nil
			}
			hints := []wm.Hint{{Action: "hub.use", Label: "change"}, {Action: "hub.add", Label: "add key"}}
			if len(bound.Keys(b.rows[i].ID)) > 0 {
				hints = append(hints, wm.Hint{Action: "hub.remove", Label: "clear"})
			}
			if !bound.AtDefault(b.rows[i].ID) {
				hints = append(hints, wm.Hint{Action: "hub.reset"})
			}
			return hints
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd { return b.ask(b.rows[i], nil) },
			"hub.add": func(_ *cards.List, i int) tea.Cmd { return b.ask(b.rows[i], bound.Keys(b.rows[i].ID)) },
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				action := b.rows[i]
				l.Confirm("Clear keys for "+b.title(action)+"?", func() tea.Cmd { return b.set(action, nil) })
				return nil
			},
			"hub.reset": func(_ *cards.List, i int) tea.Cmd { return b.set(b.rows[i], b.rows[i].Default) },
		},
	})
	b.hub = b.kit.Hub("keys", b.list)
	b.list.Filter(argument)
	return b.kit.WM.ShowOverlay(b.hub)
}

func (b *bindings) title(action keys.Action) string {
	if title := b.titles[action.ID]; title != "" && action.Context == keys.Global {
		return title
	}
	label := action.LabelText()
	first, size := utf8.DecodeRuneInString(label)
	return string(unicode.ToUpper(first)) + label[size:]
}

func (b *bindings) ask(action keys.Action, kept []string) tea.Cmd {
	b.list.Capture("Press a key for "+b.title(action), func(chord string) tea.Cmd {
		holder := b.kit.Env.Keys.Holder(action.ID, chord)
		if holder == "" {
			return b.set(action, append(slices.Clone(kept), chord))
		}
		other, _ := b.kit.Env.Keys.Action(holder)
		b.list.Confirm(keys.Format(chord)+" is already used by "+b.title(other)+". Move it here?", func() tea.Cmd {
			rest := slices.DeleteFunc(b.kit.Env.Keys.Keys(holder), func(held string) bool { return held == chord })
			if err := b.kit.Env.Keys.Set(holder, rest); err != nil {
				return b.hub.Fail(err.Error())
			}
			return b.set(action, append(slices.Clone(kept), chord))
		})
		return nil
	})
	return nil
}

func (b *bindings) set(action keys.Action, chords []string) tea.Cmd {
	if err := b.kit.Env.Keys.Set(action.ID, chords); err != nil {
		return b.hub.Fail(err.Error())
	}
	return b.save()
}

func (b *bindings) save() tea.Cmd {
	changed := map[string][]string{}
	for _, action := range b.kit.Env.Keys.Actions() {
		if !b.kit.Env.Keys.AtDefault(action.ID) {
			changed[action.ID] = b.kit.Env.Keys.Keys(action.ID)
		}
	}
	return tui.Async(func() func() tea.Cmd {
		text, err := yaml.Marshal(changed)
		if err == nil {
			_, err = b.settings.Set(b.kit.Env.Ctx, &configv1.SetRequest{Key: appconfig.Keys, Value: string(text)})
		}
		return func() tea.Cmd {
			if err != nil {
				return b.kit.WM.Toast("Couldn't save keys: "+tui.Reason(err), wm.Error, wm.DefaultToastTTL)
			}
			return nil
		}
	})
}

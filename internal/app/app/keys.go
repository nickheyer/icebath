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

// Text with a capital first letter
func capital(text string) string {
	first, size := utf8.DecodeRuneInString(text)
	if size == 0 {
		return text
	}
	return string(unicode.ToUpper(first)) + text[size:]
}

// Where the keys of a context work, as a heading says it
func where(context string) string {
	return capital(cmp.Or(keys.ContextDocs[context], context))
}

func (b *bindings) open(argument string) tea.Cmd {
	bound := b.kit.Env.Keys
	switch words := strings.Fields(argument); {
	case len(words) == 1 && words[0] == "reset":
		bound.ResetAll()
		return tea.Batch(b.save(), b.kit.WM.Toast("Every key is back to its default.", wm.Success, wm.DefaultToastTTL))
	case len(words) == 1 && words[0] == "reload":
		return tui.Async(func() func() tea.Cmd {
			response, err := b.settings.Get(b.kit.Env.Ctx, &configv1.GetRequest{Key: appconfig.Keys})
			saved := map[string][]string{}
			if err == nil {
				err = yaml.Unmarshal([]byte(response.GetSetting().GetValue()), &saved)
			}
			return func() tea.Cmd {
				if err != nil {
					return b.kit.WM.Toast("Your keys could not be read: "+strings.TrimSuffix(tui.Reason(err), ".")+".", wm.Error, wm.DefaultToastTTL)
				}
				bound.ResetAll()
				for id, chords := range saved {
					if err := bound.Set(id, chords); err != nil {
						return b.kit.WM.Toast("A saved key could not be used: "+strings.TrimSuffix(err.Error(), ".")+".", wm.Error, wm.DefaultToastTTL)
					}
				}
				return b.kit.WM.Toast("Read your keys again: "+strconv.Itoa(len(saved))+" differ from their defaults.", wm.Success, wm.DefaultToastTTL)
			}
		})
	case len(words) == 2 && words[0] == "option" && (words[1] == "on" || words[1] == "off"):
		value := strconv.FormatBool(words[1] == "on")
		return tui.Async(func() func() tea.Cmd {
			_, err := b.settings.Set(b.kit.Env.Ctx, &configv1.SetRequest{Key: appconfig.Option, Value: value})
			return func() tea.Cmd {
				if err != nil {
					return b.kit.WM.Toast("Option as alt was not changed: "+strings.TrimSuffix(tui.Reason(err), ".")+".", wm.Error, wm.DefaultToastTTL)
				}
				return tea.Batch(b.kit.Changed(appconfig.Option, value), b.kit.WM.Toast("Option as alt is now "+capital(words[1])+".", wm.Success, wm.DefaultToastTTL))
			}
		})
	}
	b.titles = map[string]string{}
	for _, command := range b.kit.WM.Commands() {
		if command.Title != "" {
			b.titles[command.ID] = command.TitleText()
		}
	}
	b.rows = arrange(bound.Actions())
	t := b.kit.Env.Theme
	b.list = cards.NewList(b.kit.Env, cards.ListSpec{
		Title:   "Keys",
		Glyph:   t.Glyphs.Keys,
		Key:     "keys",
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Bar:     true,
		Search:  "Search keys",
		Intro: func() []string {
			return []string{"Every shortcut, under where it works. Enter records a new key for the highlighted one."}
		},
		Columns: []render.Column{{}, {}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			rows := make([][]string, len(b.rows))
			for i, action := range b.rows {
				chords := bound.DisplayAll(action.ID)
				if chords == "" {
					chords = render.Dim("No key", t)
				} else if !bound.AtDefault(action.ID) {
					chords = render.Cool(chords, t)
				}
				rows[i] = []string{b.title(action), chords, render.Dim(action.DocText(), t)}
			}
			return rows
		},
		Group: func(i int) string { return where(b.rows[i].Context) },
		Words: func(i int) string { return where(b.rows[i].Context) + " " + b.rows[i].ID },
		Detail: func(i int) []string {
			action := b.rows[i]
			var lines []string
			if !b.list.Shows(2) && action.DocText() != "" {
				lines = append(lines, strings.TrimSuffix(action.DocText(), ".")+".")
			}
			lines = append(lines, render.Dim("Works in: "+strings.ToLower(where(action.Context)), t))
			if !bound.AtDefault(action.ID) {
				defaults := "no key"
				if len(action.Default) > 0 {
					shown := make([]string, len(action.Default))
					for i, chord := range action.Default {
						shown[i] = keys.Format(chord)
					}
					defaults = strings.Join(shown, " ")
				}
				lines = append(lines, render.Cool("You changed this. "+b.kit.Env.Keys.Display("hub.remove")+" puts back the default, "+defaults+".", t))
			}
			return lines
		},
		Empty: func() string { return "There are no keys to show." },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(b.rows) {
				return nil
			}
			hints := []wm.Hint{{Action: "hub.use", Label: "change"}, {Action: "hub.add", Label: "add key"}}
			switch id := b.rows[i].ID; {
			case !bound.AtDefault(id):
				hints = append(hints, wm.Hint{Action: "hub.remove", Label: "reset"})
			case len(bound.Keys(id)) > 0:
				hints = append(hints, wm.Hint{Action: "hub.remove", Label: "clear"})
			}
			return hints
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd { return b.ask(b.rows[i], nil) },
			"hub.add": func(_ *cards.List, i int) tea.Cmd { return b.ask(b.rows[i], bound.Keys(b.rows[i].ID)) },
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				action := b.rows[i]
				if !bound.AtDefault(action.ID) {
					return b.set(action, action.Default, b.title(action)+" is back to its default key.")
				}
				l.Confirm("Clear the keys of "+b.title(action)+"? It then has no key until you give it one.", func() tea.Cmd {
					return b.set(action, nil, b.title(action)+" has no key now.")
				})
				return nil
			},
		},
	})
	b.list.ToggleDetail()
	b.hub = b.kit.Hub("keys", b.list)
	b.list.Filter(argument)
	return b.kit.WM.ShowOverlay(b.hub)
}

// Actions in the order the screen lists them: those that work everywhere first, the palette and the screens leading, then every other place
// in the order its first action was registered
func arrange(actions []keys.Action) []keys.Action {
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
	var places []string
	for _, action := range actions {
		if !slices.Contains(places, action.Context) {
			places = append(places, action.Context)
		}
	}
	place := func(action keys.Action) int {
		if action.Context == keys.Global {
			return -1
		}
		return slices.Index(places, action.Context)
	}
	slices.SortStableFunc(actions, func(a, c keys.Action) int {
		return cmp.Or(cmp.Compare(place(a), place(c)), cmp.Compare(rank(a), rank(c)))
	})
	return actions
}

func (b *bindings) title(action keys.Action) string {
	if title := b.titles[action.ID]; title != "" && action.Context == keys.Global {
		return title
	}
	return capital(action.LabelText())
}

// Records the next key pressed for the action, after the keys kept, asking first when another action holds it
func (b *bindings) ask(action keys.Action, kept []string) tea.Cmd {
	b.list.Capture("Press the new key for "+b.title(action)+". "+capital(b.kit.Env.Keys.Display("capture.cancel"))+" cancels.", func(chord string) tea.Cmd {
		holder := b.kit.Env.Keys.Holder(action.ID, chord)
		if holder == "" {
			return b.set(action, append(slices.Clone(kept), chord), b.title(action)+" now runs on "+keys.Format(chord)+".")
		}
		other, _ := b.kit.Env.Keys.Action(holder)
		b.list.Confirm(keys.Format(chord)+" runs "+b.title(other)+" now. Move it to "+b.title(action)+"?", func() tea.Cmd {
			rest := slices.DeleteFunc(b.kit.Env.Keys.Keys(holder), func(held string) bool { return held == chord })
			if err := b.kit.Env.Keys.Set(holder, rest); err != nil {
				return b.hub.Fail("The key could not be moved: " + strings.TrimSuffix(err.Error(), ".") + ".")
			}
			return b.set(action, append(slices.Clone(kept), chord), keys.Format(chord)+" now runs "+b.title(action)+" in place of "+b.title(other)+".")
		})
		return nil
	})
	return nil
}

// Gives the action its keys and saves every key that differs from its default, saying what changed
func (b *bindings) set(action keys.Action, chords []string, told string) tea.Cmd {
	if err := b.kit.Env.Keys.Set(action.ID, chords); err != nil {
		return b.hub.Fail("That key can't be used: " + strings.TrimSuffix(err.Error(), ".") + ".")
	}
	b.list.Refresh()
	return tea.Batch(b.save(), b.hub.Done(told))
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
				return b.kit.WM.Toast("Your keys could not be saved: "+strings.TrimSuffix(tui.Reason(err), ".")+".", wm.Error, wm.DefaultToastTTL)
			}
			return nil
		}
	})
}

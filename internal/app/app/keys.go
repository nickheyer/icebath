package app

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"go.yaml.in/yaml/v3"

	appconfig "github.com/nickheyer/icebath/internal/app/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// The keyboard shortcuts, a page of General
type bindings struct {
	kit      *tui.Kit
	settings configv1connect.ConfigServiceClient
	hub      *cards.Hub
	list     *cards.List
	rows     []keys.Action
	titles   map[string]string
	// Draws the page the shortcuts are over again, whose row counts the keys changed
	refresh func()
}

// Where the keys of a context work, as a heading says it
func where(context string) string {
	return strs.Capital(cmp.Or(keys.ContextDocs[context], context))
}

// /keys: alone or with words to search, the keyboard shortcuts over the page of General; reset, reload and option on or off are shortcuts
// that each say what they did in one line
func keysCommand(kit *tui.Kit, settings configv1connect.ConfigServiceClient, argument string) tea.Cmd {
	bound := kit.Env.Keys
	switch words := strings.Fields(argument); {
	case len(words) == 1 && words[0] == "reset":
		bound.ResetAll()
		return tea.Batch((&bindings{kit: kit, settings: settings}).save(), kit.WM.Toast("Every key is back to its default.", wm.Success, wm.DefaultToastTTL))
	case len(words) == 1 && words[0] == "reload":
		return tui.Async(func() func() tea.Cmd {
			response, err := settings.Get(kit.Env.Ctx, &configv1.GetRequest{Key: appconfig.Keys})
			saved := map[string][]string{}
			if err == nil {
				err = yaml.Unmarshal([]byte(response.GetSetting().GetValue()), &saved)
			}
			return func() tea.Cmd {
				if err != nil {
					return kit.WM.Toast("Your keys could not be read. The error was: "+strings.TrimSuffix(tui.Reason(err), ".")+".", wm.Error, wm.DefaultToastTTL)
				}
				bound.ResetAll()
				for id, chords := range saved {
					if err := bound.Set(id, chords); err != nil {
						return kit.WM.Toast("A saved key could not be used. "+strs.Capital(strings.TrimSuffix(refusal(err), "."))+".", wm.Error, wm.DefaultToastTTL)
					}
				}
				return kit.WM.Toast("Read your keys again. "+render.Count(len(saved), "key")+" differ from their defaults.", wm.Success, wm.DefaultToastTTL)
			}
		})
	case len(words) == 2 && words[0] == "option" && (words[1] == "on" || words[1] == "off"):
		return keep(kit, settings, appconfig.Option, strconv.FormatBool(words[1] == "on"))
	case len(words) > 0 && words[0] == "option":
		return kit.WM.Toast("/keys option takes on or off.", wm.Warn, wm.DefaultToastTTL)
	}
	return kit.Open(generalScreen, strings.TrimSpace(atKeys+" "+argument))
}

// Why a key can't be used, as a person reads it
func refusal(err error) string {
	return strings.TrimPrefix(err.Error(), "keys: ")
}

// Lists every shortcut over the hub, searched for the words, or on the action they name with nothing typed into the search
func (b *bindings) push(target string) tea.Cmd {
	bound := b.kit.Env.Keys
	b.titles = map[string]string{}
	for _, command := range b.kit.WM.Commands() {
		if command.Title != "" {
			b.titles[command.ID] = command.TitleText()
		}
	}
	b.rows = arrange(bound.Actions())
	t := b.kit.Env.Theme
	b.list = cards.NewList(b.kit.Env, cards.ListSpec{
		Title:   "Keyboard shortcuts",
		Glyph:   t.Glyphs.Keys,
		Key:     "keys",
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Bar:     true,
		Search:  "Search keys",
		Intro: func() []string {
			return []string{"Every shortcut, under where it works. Enter records a new key for the one in hand."}
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
			// Where it works is the heading over it, which a search leaves out
			if b.list.Query() != "" {
				lines = append(lines, render.Dim("Works in: "+strings.ToLower(where(action.Context)), t))
			}
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
			hints := []wm.Hint{{Action: "hub.use", Label: "change"}, {Action: "hub.add", Label: "add a key"}}
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
					l.Confirm(cards.Question{Text: "Put back the default key of " + b.title(action) + "? " + b.defaults(action), Verb: "Reset"}, func() tea.Cmd {
						return b.set(action, action.Default, b.title(action)+" is back to its default key.")
					})
					return nil
				}
				if len(bound.Keys(action.ID)) == 0 {
					return nil
				}
				l.Confirm(cards.Question{Text: "Clear the keys of " + b.title(action) + "? It then has no key until you give it one.", Verb: "Clear"}, func() tea.Cmd {
					return b.set(action, nil, b.title(action)+" has no key now.")
				})
				return nil
			},
		},
	})
	b.list.ToggleDetail()
	cmd := b.hub.Push(b.list)
	if _, action := bound.Action(target); action {
		b.list.Select(slices.IndexFunc(b.rows, func(row keys.Action) bool { return row.ID == target }))
	} else {
		b.list.Filter(target)
	}
	return cmd
}

// What the action runs on again once its default is back, as the second sentence of the question
func (b *bindings) defaults(action keys.Action) string {
	if len(action.Default) == 0 {
		return "It then has no key."
	}
	shown := make([]string, len(action.Default))
	for i, chord := range action.Default {
		shown[i] = keys.Format(chord)
	}
	return "It runs on " + strs.And(shown) + " again."
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
	return strs.Capital(action.LabelText())
}

// Records the next key pressed for the action, after the keys kept, asking first when another action holds it
func (b *bindings) ask(action keys.Action, kept []string) tea.Cmd {
	return b.record(action, kept, "")
}

// Records a key for the action, a key it can't take refused right where it was pressed, before the prompt to press another
func (b *bindings) record(action keys.Action, kept []string, refused string) tea.Cmd {
	prompt := "Press the new key for " + b.title(action) + ". " + strs.Capital(b.kit.Env.Keys.Display("capture.cancel")) + " cancels."
	if refused != "" {
		prompt = refused + " Press another key for " + b.title(action) + ", or " + b.kit.Env.Keys.Display("capture.cancel") + " to cancel."
	}
	b.list.Capture(prompt, func(chord string) tea.Cmd {
		if why := b.refuses(action, chord); why != "" {
			return b.record(action, kept, why)
		}
		holder := b.kit.Env.Keys.Holder(action.ID, chord)
		if holder == "" {
			return b.set(action, append(slices.Clone(kept), chord), b.title(action)+" now runs on "+keys.Format(chord)+".")
		}
		other, _ := b.kit.Env.Keys.Action(holder)
		b.list.Confirm(cards.Question{Text: keys.Format(chord) + " runs " + b.title(other) + " now. Move it to " + b.title(action) + "?", Verb: "Move"}, func() tea.Cmd {
			rest := slices.DeleteFunc(b.kit.Env.Keys.Keys(holder), func(held string) bool { return held == chord })
			if err := b.kit.Env.Keys.Set(holder, rest); err != nil {
				return b.hub.Fail("The key could not be moved. " + strs.Capital(strings.TrimSuffix(refusal(err), ".")) + ".")
			}
			return b.set(action, append(slices.Clone(kept), chord), keys.Format(chord)+" now runs "+b.title(action)+" in place of "+b.title(other)+".")
		})
		return nil
	})
	return nil
}

// Why the action can't take the key, as a sentence, empty when it can
func (b *bindings) refuses(action keys.Action, chord string) string {
	if action.Refuse == nil {
		return ""
	}
	if why := action.Refuse(b.kit.Env.Keys, chord); why != "" {
		return keys.Format(chord) + " can't be used, because " + strings.TrimSuffix(why, ".") + "."
	}
	return ""
}

// Gives the action its keys and saves every key that differs from its default, saying what changed
func (b *bindings) set(action keys.Action, chords []string, told string) tea.Cmd {
	if err := b.kit.Env.Keys.Set(action.ID, chords); err != nil {
		return b.hub.Fail("That key can't be used. " + strs.Capital(strings.TrimSuffix(refusal(err), ".")) + ".")
	}
	b.list.Refresh()
	if b.refresh != nil {
		b.refresh()
	}
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
				return b.kit.WM.Toast("Your keys could not be saved. The error was: "+strings.TrimSuffix(tui.Reason(err), ".")+".", wm.Error, wm.DefaultToastTTL)
			}
			return nil
		}
	})
}

package tui

import (
	"cmp"
	"errors"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

type Choice struct {
	ID     string
	Title  string
	Detail string
	// Heading the choice is listed under, the choices of one heading together in the order given; none for a list with no headings
	Group string
	// Word on the button while the choice is in hand, such as Add for a choice that ends a job where the others lead on; the list's own
	// verb when empty
	Verb string
}

// Opens a list of choices over the hub, or in a hub of its own when hub is nil, the one in use marked on a list that has one. Each choice
// shows what it means beside it, or under the list where the width cannot hold every meaning whole. Enter or the button labeled verb runs
// pick with the highlighted choice and closes the list
func (k *Kit) Pick(hub *cards.Hub, title, verb string, choices []Choice, current string, pick func(choice Choice) tea.Cmd) tea.Cmd {
	return k.PickFrom(hub, title, nil, verb, choices, current, pick)
}

// Pick with lines above the choices that say what is being chosen, for a title that does not
func (k *Kit) PickFrom(hub *cards.Hub, title string, intro []string, verb string, choices []Choice, current string, pick func(choice Choice) tea.Cmd) tea.Cmd {
	own := hub == nil
	var list *cards.List
	list = k.choices(title, intro, verb, choices, current, func(choice Choice) tea.Cmd {
		if own {
			return tea.Batch(k.WM.DismissCard(hub), pick(choice))
		}
		return tea.Batch(hub.Close(list), pick(choice))
	})
	var cmd tea.Cmd
	if own {
		hub = k.Hub(list.Key(), list)
		cmd = k.WM.ShowOverlay(hub)
	} else {
		cmd = hub.Push(list)
	}
	return cmd
}

// List to pick one of the choices from, the one in use marked and in hand, what each means beside it or under the list; enter or the button
// labeled verb hands the choice in hand to pick
func (k *Kit) choices(title string, intro []string, verb string, choices []Choice, current string, pick func(choice Choice) tea.Cmd) *cards.List {
	var list *cards.List
	var said func() []string
	if len(intro) > 0 {
		said = func() []string { return intro }
	}
	var group func(i int) string
	if slices.ContainsFunc(choices, func(choice Choice) bool { return choice.Group != "" }) {
		group = func(i int) string { return choices[i].Group }
	}
	list = cards.NewList(k.Env, cards.ListSpec{
		Title:   title,
		Key:     "pick:" + title,
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Intro:   said,
		Group:   group,
		Bar:     len(choices) > 8,
		Columns: []render.Column{{}, {}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			rows := make([][]string, len(choices))
			for i, choice := range choices {
				rows[i] = []string{k.Mark(current != "", choice.ID == current), choice.Title, render.Dim(choice.Detail, k.Env.Theme)}
			}
			return rows
		},
		Detail: func(i int) []string {
			if list.Shows(2) || choices[i].Detail == "" {
				return nil
			}
			return []string{choices[i].Detail}
		},
		Empty: func() string { return "Nothing to choose from." },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: cmp.Or(strings.ToLower(choices[i].Verb), verb)}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd { return pick(choices[i]) },
		},
	})
	list.ToggleDetail()
	list.Refresh()
	for i, choice := range choices {
		if choice.ID == current {
			list.Select(i)
		}
	}
	return list
}

// What a screen offers for every setting whose value names its kind of thing, registered under the name those settings give in Picks
type Picker struct {
	// Opens the list to pick from over the hub
	Open func(kit *Kit, hub *cards.Hub, ask Ask) tea.Cmd
	// Keys of the settings kept as one choice with the setting at key: one row shows them all and their own rows are left out. Nil for
	// a kind whose choice is one value
	Beside func(key string) []string
	// What the row shows for the values held; the value in the settings service's own words when nil or when it answers nothing
	Shown func(ask Ask) string
}

// What a picker is asked
type Ask struct {
	Title string
	Intro []string
	// Key of the setting asked about, empty when a screen asks for itself
	Key string
	// Values held now, as YAML, by key: the setting's own and those of the settings beside it
	Values map[string]string
	// Choices listed before the things, such as what is in effect when nothing is picked
	First []Choice
	// Profile whose things are listed, the one in use when empty
	Profile string
	// What of the kind to list, as the setting's Picks says after the kind, such as the provider whose models alone are listed: the
	// picker's own to read; empty for every thing of the kind
	Of string
	// The setting at Key is a list and one item of it is asked for: Values holds that item alone under Key, nothing for a new one, and Done
	// is handed the one item picked under Key, the asker keeping the list
	Item bool
	// Word on the button that picks, the job's verb when the pick ends a job, such as Add; choose when empty
	Verb string
	// Handed the values to keep, as YAML, by key; a value of null puts a default back. The asker ends the job once they are kept
	Done func(values map[string]string) tea.Cmd
}

// Mark before a choice of a list where one choice is in use: filled on that one, hollow on the rest, nothing where no choice is marked
func (k *Kit) Mark(marked, on bool) string {
	switch {
	case !marked:
		return ""
	case on:
		return k.Env.Theme.Glyphs.Live
	}
	return render.Dim(k.Env.Theme.Glyphs.Hollow, k.Env.Theme)
}

// Asks one question over the hub in a form of the one field: typed into, its help under it, enter handing the trimmed answer to done and
// the button labeled submit. An empty answer is refused
func (k *Kit) Ask(hub *cards.Hub, title string, field cards.Field, submit string, done func(text string) tea.Cmd) tea.Cmd {
	return k.ask(hub, title, field, submit, true, done, nil)
}

// Ask that takes an empty answer too, for a value a person takes away by leaving its field empty
func (k *Kit) AskAny(hub *cards.Hub, title string, field cards.Field, submit string, done func(text string) tea.Cmd) tea.Cmd {
	return k.ask(hub, title, field, submit, false, done, nil)
}

// Ask whose form runs left when it closes with no answer given, by the way back or with its hub, for an asker that waits for the answer
func (k *Kit) AskOr(hub *cards.Hub, title string, field cards.Field, submit string, done func(text string) tea.Cmd, left func() tea.Cmd) tea.Cmd {
	return k.ask(hub, title, field, submit, true, done, left)
}

// Form of the one field of a question over the hub, an empty answer refused when one is required
func (k *Kit) ask(hub *cards.Hub, title string, field cards.Field, submit string, required bool, done func(text string) tea.Cmd, left func() tea.Cmd) tea.Cmd {
	field.ID, field.Required = "answer", required
	// The question is settled once: by its answer, or by the first time its form is told it closed, which a dialog that leaves the screen
	// and shows again tells it more than once
	settled := false
	var form *cards.Form
	form = cards.NewForm(k.Env, cards.FormSpec{
		Title:  title,
		Fields: []cards.Field{field},
		Submit: submit,
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			if settled {
				return hub.Close(form)
			}
			settled = true
			return tea.Batch(hub.Close(form), done(strings.TrimSpace(values["answer"])))
		},
		Closed: func(*cards.Form) tea.Cmd {
			if settled || left == nil {
				return nil
			}
			settled = true
			return left()
		},
	})
	return hub.Push(form)
}

func Call[R any](hub *cards.Hub, call func() (R, error), then func(result R) tea.Cmd) tea.Cmd {
	return Async(func() func() tea.Cmd {
		result, err := call()
		return func() tea.Cmd {
			if err != nil {
				return hub.Fail(Reason(err))
			}
			return then(result)
		}
	})
}

func Waiting(loaded bool, err error, none string) string {
	switch {
	case err != nil:
		return Reason(err)
	case !loaded:
		return "Loading…"
	}
	return none
}

func Reason(err error) string {
	if failure, ok := errors.AsType[*connect.Error](err); ok {
		return failure.Message()
	}
	return err.Error()
}

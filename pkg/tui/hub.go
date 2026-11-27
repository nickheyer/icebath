package tui

import (
	"errors"
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
	var said func() []string
	if len(intro) > 0 {
		said = func() []string { return intro }
	}
	list = cards.NewList(k.Env, cards.ListSpec{
		Title:   title,
		Key:     "pick:" + title,
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Intro:   said,
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
			return []wm.Hint{{Action: "hub.use", Label: verb}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				if own {
					return tea.Batch(k.WM.DismissCard(hub), pick(choices[i]))
				}
				return tea.Batch(hub.Close(list), pick(choices[i]))
			},
		},
	})
	list.ToggleDetail()
	var cmd tea.Cmd
	if own {
		hub = k.Hub(list.Key(), list)
		cmd = k.WM.ShowOverlay(hub)
	} else {
		cmd = hub.Push(list)
	}
	for i, choice := range choices {
		if choice.ID == current {
			list.Select(i)
		}
	}
	return cmd
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

func (k *Kit) Ask(hub *cards.Hub, title string, field cards.Field, submit string, done func(text string) tea.Cmd) tea.Cmd {
	field.ID, field.Required = "answer", true
	var form *cards.Form
	form = cards.NewForm(k.Env, cards.FormSpec{
		Title:  title,
		Fields: []cards.Field{field},
		Submit: submit,
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			return tea.Batch(hub.Close(form), done(strings.TrimSpace(values["answer"])))
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

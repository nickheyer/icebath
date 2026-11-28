package tui

import (
	"cmp"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Steps of one job, each one question: a form of typed fields or a list to pick from, with one line above it saying what is asked. One
// view on the hub titled as the job through every step; esc goes back a step with the answers kept, and from the first leaves the job
type Flow struct {
	kit   *Kit
	hub   *cards.Hub
	title string
	steps *cards.Steps
	// Last step shown at each place, whose answers come back when the person returns to that place
	asked []wm.Card
	// Choice made at each place a list was picked from
	chosen map[int]string
}

// Starts a job in steps over the hub, titled as the job; its first question shows it
func (k *Kit) Flow(hub *cards.Hub, title string) *Flow {
	return &Flow{kit: k, hub: hub, title: title, chosen: map[int]string{}}
}

// Place of the next step
func (f *Flow) at() int {
	if f.steps == nil || !f.hub.Holds(f.steps) {
		return 0
	}
	return f.steps.Len()
}

// Asks the next question in a form of typed fields under the intro, the verb on what enter does from the last field, Next when empty.
// Every step says what is asked: above its fields, or for a step of one field under that field, before the field's own help and never
// above it too. Fields asked at this place before come back as they were left
func (f *Flow) Ask(intro string, fields []cards.Field, verb string, next func(values map[string]string) tea.Cmd) tea.Cmd {
	at := f.at()
	fields = slices.Clone(fields)
	if len(fields) == 1 {
		if help := fields[0].Help; intro != "" && intro != help {
			fields[0].Help = strings.TrimSpace(intro + "\n" + help)
		}
		intro = ""
	}
	if old, ok := f.before(at).(*cards.Form); ok {
		was := old.Values()
		for i := range fields {
			if value, kept := was[fields[i].ID]; kept {
				fields[i].Value = value
			}
		}
	}
	form := cards.NewForm(f.kit.Env, cards.FormSpec{
		Title:  f.title,
		Intro:  lines(intro),
		Fields: fields,
		Submit: cmp.Or(verb, "Next"),
		Done:   func(_ *cards.Form, values map[string]string) tea.Cmd { return next(values) },
	})
	return f.show(at, form)
}

// Asks the next question as a list to pick from under the intro, the choice in use marked. A choice made at this place before is in
// hand again
func (f *Flow) Pick(intro string, choices []Choice, current string, next func(choice Choice) tea.Cmd) tea.Cmd {
	return f.PickVerb(intro, choices, current, "", next)
}

// Pick whose button says verb, the job's verb when the choice ends the job, such as Add; choose for none
func (f *Flow) PickVerb(intro string, choices []Choice, current, verb string, next func(choice Choice) tea.Cmd) tea.Cmd {
	at := f.at()
	held := current
	if _, ok := f.before(at).(*cards.List); ok && f.chosen[at] != "" {
		held = f.chosen[at]
	}
	list := f.kit.choices(f.title, lines(intro), cmp.Or(strings.ToLower(verb), "choose"), choices, current, func(choice Choice) tea.Cmd {
		f.chosen[at] = choice.ID
		return next(choice)
	})
	for i, choice := range choices {
		if choice.ID == held {
			list.Select(i)
		}
	}
	return f.show(at, list)
}

// Shows the next step as a view of the caller's own, such as a page a sign-in goes on in, esc going back a step as from any other
func (f *Flow) Show(view wm.Card) tea.Cmd { return f.show(f.at(), view) }

// Ends the job: the flow closes, and so does a menu the person came straight into for this job
func (f *Flow) End() tea.Cmd {
	if f.steps == nil || !f.hub.Holds(f.steps) {
		return nil
	}
	return f.hub.Finish(f.steps)
}

// Step last shown at the place, nil for none
func (f *Flow) before(at int) wm.Card {
	if at < len(f.asked) {
		return f.asked[at]
	}
	return nil
}

// Shows the step at its place: the first puts the job on the hub, the rest go after the step in view
func (f *Flow) show(at int, step wm.Card) tea.Cmd {
	f.asked = append(f.asked[:min(at, len(f.asked))], step)
	if at == 0 {
		f.steps = cards.NewSteps(f.title)
		return tea.Batch(f.hub.Push(f.steps), f.steps.Next(step))
	}
	return f.steps.Next(step)
}

// Line saying what is asked as the lines over a step, none for no line
func lines(intro string) []string {
	if intro == "" {
		return nil
	}
	return []string{intro}
}

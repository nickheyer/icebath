package app

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	shellconfig "github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/config"
	hooksv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/hooks/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// The events as choices under their headings, what each is beside it, in the order they are listed
func eventChoices(names []string) []tui.Choice {
	names = slices.Clone(names)
	slices.SortStableFunc(names, func(a, b string) int { return shellconfig.Place(a) - shellconfig.Place(b) })
	choices := make([]tui.Choice, len(names))
	for i, name := range names {
		point := shellconfig.PointOf(name)
		choices[i] = tui.Choice{ID: name, Title: point.Title, Detail: point.About, Group: point.Group}
	}
	return choices
}

// Adds a hook in two steps, each one question: when it runs, from the events under their headings, then its command. Nothing is kept until
// the last step succeeds; the hook is then allowed, since the person who adds it agrees to it, it is in hand, and the note says what was made
func (p *page) addHook() tea.Cmd {
	f := p.kit.Flow(p.hub, "Add a hook")
	return f.Pick("When it runs.", eventChoices(p.listing.GetEvents()), "", func(choice tui.Choice) tea.Cmd {
		command := cards.Field{ID: "command", Label: "Command", Path: cards.PathFile, Check: required("Give the command that runs."),
			Help: "The command or script that runs, such as ~/hooks/check.sh. It reads what happened as JSON on its input."}
		return f.Ask("", []cards.Field{command}, "Add", func(values map[string]string) tea.Cmd {
			typed := strings.TrimSpace(values["command"])
			return p.change(&hooksv1.ChangeRequest{Verb: "add", Event: choice.ID, Command: typed, Allow: true}, func(*hooksv1.ChangeResponse) tea.Cmd {
				// The new hook is put in hand once the page is back and read again
				p.focus = hookOf + choice.ID + " " + typed
				return tea.Batch(f.End(), p.load())
			})
		})
	})
}

// What is wrong with a field that must not be empty, empty when it holds something
func required(why string) func(string) string {
	return func(typed string) string {
		if strings.TrimSpace(typed) == "" {
			return why
		}
		return ""
	}
}

// What is wrong with a web address typed, empty when it starts as one
func web(typed string) string {
	typed = strings.ToLower(strings.TrimSpace(typed))
	if !strings.HasPrefix(typed, "https://") && !strings.HasPrefix(typed, "http://") || len(typed) <= len("https://") {
		return "A web address starts with https:// or http://, such as https://example.com/hooks."
	}
	return ""
}

// Adds a webhook in two steps: its address, then the events it posts, ticked from the events under their headings. Nothing is kept until the
// last step succeeds; the webhook is then in hand and the note says what was made. Going back keeps what was typed and ticked
func (p *page) addWebhook() tea.Cmd {
	f := p.kit.Flow(p.hub, "Add a webhook")
	address := cards.Field{ID: "url", Label: "Address", Check: web, Help: "The web address that events are posted to, such as https://example.com/hooks."}
	ticked := map[string]bool{}
	return f.Ask("", []cards.Field{address}, "Next", func(values map[string]string) tea.Cmd {
		url := strings.TrimSpace(values["url"])
		return f.Show(p.eventTicks(ticked, func(events []string) tea.Cmd {
			if len(events) == 0 {
				return p.hub.Fail("Tick at least one event to post.")
			}
			p.hub.Busy("Saving…")
			return tui.Call(p.hub, func() (*hooksv1.ChangeResponse, error) {
				return p.service.ChangeWebhook(p.kit.Env.Ctx, &hooksv1.WebhookRequest{Profile: p.profile, Verb: "add", Url: url, Events: events})
			}, func(response *hooksv1.ChangeResponse) tea.Cmd {
				p.listing = response.GetHooks()
				p.focus = webhookOf + response.GetKey()
				return tea.Batch(f.End(), p.hub.Done(response.GetSaid()), p.options.Load(nil), p.load())
			})
		}))
	})
}

// Step of a flow that ticks the events to post under their headings, enter ticking the one in hand; its last row adds what was ticked, as
// alt+n does from any row
func (p *page) eventTicks(ticked map[string]bool, add func(events []string) tea.Cmd) *cards.List {
	t := p.kit.Env.Theme
	choices := eventChoices(p.listing.GetPoints())
	chosen := func() []string {
		var out []string
		for _, choice := range choices {
			if ticked[choice.ID] {
				out = append(out, choice.ID)
			}
		}
		return out
	}
	var list *cards.List
	list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Add a webhook",
		Key:     Screen + ":ticks",
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return []string{"The events posted to the address."} },
		Columns: []render.Column{{}, {}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			rows := make([][]string, len(choices), len(choices)+1)
			for i, choice := range choices {
				box := "[ ]"
				if ticked[choice.ID] {
					box = "[x]"
				}
				rows[i] = []string{box, choice.Title, render.Dim(choice.Detail, t)}
			}
			return append(rows, []string{"", "Add the webhook", render.Dim(render.Count(len(chosen()), "event")+" ticked", t)})
		},
		Group: func(i int) string {
			if i < len(choices) {
				return choices[i].Group
			}
			return ""
		},
		Detail: func(i int) []string {
			if i >= len(choices) || list.Shows(2) {
				return nil
			}
			return []string{choices[i].Detail + "."}
		},
		Free: map[string]bool{"hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			switch {
			case i < 0:
				return nil
			case i >= len(choices):
				return []wm.Hint{{Action: "hub.use", Label: "add"}}
			case ticked[choices[i].ID]:
				return []wm.Hint{{Action: "hub.use", Label: "untick"}, {Action: "hub.add", Label: "add"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "tick"}, {Action: "hub.add", Label: "add"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				if i >= len(choices) {
					return add(chosen())
				}
				ticked[choices[i].ID] = !ticked[choices[i].ID]
				l.Refresh()
				return nil
			},
			"hub.add": func(*cards.List, int) tea.Cmd { return add(chosen()) },
		},
	})
	list.ToggleDetail()
	return list
}

package app

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	approvalsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/approvals/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// How much of the approval history the suggestions read, as the server reads it when asked nothing
const (
	readDays   = 90
	readLeast  = 2
	readAtMost = 20
)

// One of the numbers the suggestions are read with: its name, what it is, the line under its field, the least it takes and its value now
type reading struct {
	title, about, help string
	least              int
	value              *int
	said               func(n int) string
}

// The commands your approval history suggests, the most approved first, enter letting the one in hand run without asking after a
// question; under them the three numbers they are read with, each changed in a field of its own
func (p *page) suggest() tea.Cmd {
	t := p.kit.Env.Theme
	days, least, most := readDays, readLeast, readAtMost
	readings := []reading{
		{"Days of history", "How far back your approvals are read.", "A number of days, such as 90. 0 reads every approval so far.", 0, &days,
			func(n int) string {
				if n == 0 {
					return "Every approval"
				}
				return strs.Count(n, "day")
			}},
		{"Approvals needed", "How many times you approved a command before it is suggested.", "A number of times, such as 2.", 1, &least,
			func(n int) string { return strs.Count(n, "approval") }},
		{"Most suggestions", "The most commands suggested at once.", "A number of commands, such as 20.", 1, &most,
			func(n int) string { return strs.Count(n, "suggestion") }},
	}
	var (
		result *approvalsv1.SuggestResponse
		loaded bool
		err    error
		list   *cards.List
	)
	proposals := func() []*approvalsv1.Proposal { return result.GetProposals() }
	at := func(i int) *approvalsv1.Proposal {
		if i < 0 || i >= len(proposals()) {
			return nil
		}
		return proposals()[i]
	}
	option := func(i int) *reading {
		if i -= len(proposals()); i < 0 || i >= len(readings) {
			return nil
		}
		return &readings[i]
	}
	var load func() tea.Cmd
	load = func() tea.Cmd {
		p.hub.Busy("Reading your approval history…")
		d := int32(days)
		request := &approvalsv1.SuggestRequest{Profile: p.profile, Days: &d, MinCount: int32(least), Limit: int32(most)}
		return tui.Async(func() func() tea.Cmd {
			response, failure := p.service.Suggest(p.kit.Env.Ctx, request)
			return func() tea.Cmd {
				p.hub.Quiet()
				result, err, loaded = response, failure, true
				list.Refresh()
				if err != nil {
					return p.hub.Fail("Your approval history could not be read. The error was: " + strings.TrimSuffix(tui.Reason(err), ".") + ".")
				}
				return nil
			}
		})
	}
	list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Suggest commands",
		Glyph:   t.Glyphs.Settings,
		Key:     "approvals:suggest",
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			lead := "Commands you approved often, which the agent could run without asking."
			switch {
			case !loaded:
				return []string{lead, "Loading…"}
			case err == nil && len(proposals()) == 0 && days == 0:
				return []string{lead, "Nothing was approved often enough to suggest."}
			case err == nil && len(proposals()) == 0:
				return []string{lead, "Nothing in the last " + strs.Count(days, "day") + " was approved often enough to suggest."}
			}
			return []string{lead}
		},
		Columns: []render.Column{{}, {}, {Flex: 1}},
		Yield:   []int{2},
		Rows: func() [][]string {
			rows := make([][]string, 0, len(proposals())+len(readings))
			for _, proposal := range proposals() {
				example := ""
				if len(proposal.GetExamples()) > 0 {
					example = proposal.GetExamples()[0]
				}
				rows = append(rows, []string{proposal.GetPattern(), render.Dim("Approved "+strs.Count(int(proposal.GetCount()), "time"), t),
					render.Dim(example, t)})
			}
			for _, r := range readings {
				rows = append(rows, []string{r.title, "", r.said(*r.value)})
			}
			return rows
		},
		Group: func(i int) string {
			if at(i) != nil {
				return "Suggestions"
			}
			return "Read from your history"
		},
		Detail: func(i int) []string {
			if r := option(i); r != nil {
				return []string{r.about}
			}
			proposal := at(i)
			if proposal == nil {
				return nil
			}
			covers := "Allowing it lets every command that matches run without asking."
			if proposal.GetKind() == "class" {
				covers = "Allowing it lets every action of this risky kind run without asking."
			}
			out := []string{covers}
			if len(proposal.GetClasses()) > 0 {
				out = append(out, render.Dim("Flagged as: "+strings.Join(proposal.GetClasses(), ", "), t))
			}
			if len(proposal.GetExamples()) > 0 {
				out = append(out, "", "Commands you approved:")
				for _, example := range proposal.GetExamples() {
					out = append(out, render.Dim("  "+example, t))
				}
			}
			return out
		},
		Free: map[string]bool{"hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			switch {
			case option(i) != nil:
				return []wm.Hint{{Action: "hub.use", Label: "change"}}
			case at(i) != nil:
				return []wm.Hint{{Action: "hub.use", Label: "allow"}}
			}
			return nil
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				if r := option(i); r != nil {
					return p.reread(r, load)
				}
				proposal := at(i)
				if proposal == nil {
					return nil
				}
				pattern := proposal.GetPattern()
				l.Confirm(cards.Question{Text: "Let " + pattern + " run without asking? The agent then runs it each time without a question.",
					Verb: "Allow"}, func() tea.Cmd {
					return tui.Call(p.hub, func() (*approvalsv1.AllowResponse, error) {
						return p.service.Allow(p.kit.Env.Ctx, &approvalsv1.AllowRequest{Profile: p.profile, Patterns: []string{pattern}})
					}, func(response *approvalsv1.AllowResponse) tea.Cmd {
						if len(response.GetAdded()) == 0 {
							return p.hub.Say(pattern+" is allowed already.", cards.NoteInfo)
						}
						return tea.Batch(p.hub.Done(pattern+" now runs without asking."), load(), p.load(nil))
					})
				})
				return nil
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return load() },
		},
	})
	list.ToggleDetail()
	p.views.Add(list)
	return tea.Batch(p.hub.Push(list), load())
}

// Asks for one of the numbers the suggestions are read with, then reads them again
func (p *page) reread(r *reading, load func() tea.Cmd) tea.Cmd {
	field := cards.Field{Label: r.title, Kind: cards.FieldNumber, Value: strconv.Itoa(*r.value), Select: true, Help: r.about + "\n" + r.help,
		Check: func(typed string) string {
			n, err := strconv.Atoi(strings.TrimSpace(typed))
			switch {
			case err != nil:
				return "Enter a whole number."
			case n < r.least:
				return "Must be at least " + strconv.Itoa(r.least) + "."
			}
			return ""
		}}
	return p.kit.Ask(p.hub, r.title, field, "Read again", func(typed string) tea.Cmd {
		n, _ := strconv.Atoi(typed)
		*r.value = n
		return load()
	})
}

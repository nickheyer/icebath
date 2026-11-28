package app

import (
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/internal/hooks"
	shellconfig "github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/config"
	hooksv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/hooks/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
)

// Tool a sample call of a tool names when none is picked
const sampleTool = "terminal"

// Length of time in words from a count of seconds, to the millisecond
func span(value float64) string {
	return strs.Span(time.Duration(math.Round(value*1000)) * time.Millisecond)
}

// Lines telling how one run of a hook's command ended: how it ended, what it printed, and what icebath reads of its answer
func (p *page) runLines(run *hooksv1.Run) []string {
	t := p.kit.Env.Theme
	var out []string
	switch {
	case run.GetError() != "":
		out = append(out, render.Bad("It couldn't run. "+strs.Full(run.GetError()), t))
	case run.GetTimedOut():
		out = append(out, render.Bad("It didn't finish within "+span(run.GetSeconds())+".", t))
	case run.GetExited() && run.GetExitCode() != 0:
		out = append(out, render.Warm("It ended with exit code "+strconv.Itoa(int(run.GetExitCode()))+" after "+span(run.GetSeconds())+".", t))
	case run.GetExited():
		out = append(out, render.Good("It ended well after "+span(run.GetSeconds())+".", t))
	}
	for _, stream := range []struct{ heading, text string }{{"What it printed", run.GetStdout()}, {"Its errors", run.GetStderr()}} {
		if text := strings.TrimRight(stream.text, "\n"); strings.TrimSpace(text) != "" {
			out = append(out, "", render.Heading(stream.heading, t))
			out = append(out, strings.Split(text, "\n")...)
		}
	}
	out = append(out, "")
	if run.GetDirective() == "" {
		return append(out, render.Dim("icebath reads no answer from it, so it changes nothing.", t))
	}
	return append(out, "icebath reads its answer as "+run.GetDirective())
}

// Page of the hub showing the lines
func (p *page) showPage(title, key string, lines []string) tea.Cmd {
	return p.hub.Push(p.linesPage(title, key, lines))
}

// Page of the lines, to show over the hub or as the last step of a flow
func (p *page) linesPage(title, key string, lines []string) *cards.Page {
	return cards.NewPage(p.kit.Env, cards.PageSpec{Title: title, Key: key, Lines: func(int) []string { return lines }})
}

// Runs a hook once on a sample of its event and shows what it printed on a page; a hook of an event about a tool asks first which tool the
// sample call is for
func (p *page) test(hook *hooksv1.Hook) tea.Cmd {
	if hook == nil {
		return nil
	}
	run := func(tool string) tea.Cmd {
		p.hub.Busy("Running " + hook.GetCommand() + "…")
		return tui.Call(p.hub, func() (*hooksv1.TestResponse, error) {
			return p.service.Test(p.kit.Env.Ctx, &hooksv1.TestRequest{Profile: p.profile, Event: hook.GetEvent(), Command: hook.GetCommand(), Tool: tool})
		}, func(response *hooksv1.TestResponse) tea.Cmd {
			p.hub.Quiet()
			t := p.kit.Env.Theme
			said := "Ran on a sample of " + shellconfig.PointOf(hook.GetEvent()).Title + "."
			if tool != "" {
				said = "Ran on a sample call of " + p.tools.title(tool) + "."
			}
			lines := []string{render.Dim(said, t), ""}
			for _, tested := range response.GetRuns() {
				lines = append(lines, p.runLines(tested.GetRun())...)
			}
			return p.showPage("Test", Screen+":test", lines)
		})
	}
	if !hook.GetTool() {
		return run("")
	}
	choices := p.tools.choices(hook.GetMatcher())
	if len(choices) == 0 {
		return run(sampleTool)
	}
	// In hand: the first tool its pattern names, else the one a sample call names when none is given
	current := choices[0].ID
	if hook.GetMatcher() == "" && slices.ContainsFunc(choices, func(choice tui.Choice) bool { return choice.ID == sampleTool }) {
		current = sampleTool
	}
	return p.kit.PickFrom(p.hub, "Test", []string{"The tool the sample call is for."}, "test", choices, current, func(choice tui.Choice) tea.Cmd {
		return run(choice.ID)
	})
}

// Runs every hook of an event on a sample of it, in steps: the event, then for an event about a tool the tool the sample call is for, then
// fields to lay over the sample, typed or left empty. What each hook printed is the last step, so going back runs it again with other data
func (p *page) testEvent() tea.Cmd {
	f := p.kit.Flow(p.hub, "Test an event")
	held := ""
	for _, hook := range p.sorted() {
		held = hook.GetEvent()
		break
	}
	return f.Pick("The event to run the hooks of.", eventChoices(p.listing.GetEvents()), held, func(choice tui.Choice) tea.Cmd {
		event := choice.ID
		data := func(tool string) tea.Cmd {
			payload := cards.Field{ID: "payload", Label: "Your data", Check: object,
				Help: "Fields laid over the sample, as JSON such as {\"command\": \"ls\"}.\nLeave empty to use the sample as it is."}
			return f.Ask("", []cards.Field{payload}, "Test", func(values map[string]string) tea.Cmd {
				return p.runEvent(f, event, tool, strings.TrimSpace(values["payload"]))
			})
		}
		if point, _ := hooks.PointOf(event); !point.Tool {
			return data("")
		}
		choices := append([]tui.Choice{{ID: "", Title: "Any tool", Detail: "Every hook of the event runs, whatever tools it names"}},
			p.tools.choices("")...)
		return f.Pick("The tool the sample call is for. Only the hooks for that tool run.", choices, "", func(tool tui.Choice) tea.Cmd {
			return data(tool.ID)
		})
	})
}

// Runs the hooks of the event on its sample and shows what each printed as the last step of the flow
func (p *page) runEvent(f *tui.Flow, event, tool, payload string) tea.Cmd {
	title := shellconfig.PointOf(event).Title
	p.hub.Busy("Running the hooks of " + title + "…")
	return tui.Call(p.hub, func() (*hooksv1.TestResponse, error) {
		return p.service.Test(p.kit.Env.Ctx, &hooksv1.TestRequest{Profile: p.profile, Event: event, Tool: tool, Payload: payload})
	}, func(response *hooksv1.TestResponse) tea.Cmd {
		p.hub.Quiet()
		t := p.kit.Env.Theme
		lines := []string{render.Dim("Ran on a sample of "+title+".", t)}
		if len(response.GetRuns()) == 0 {
			lines = append(lines, "", "No hook runs on "+title+".")
		}
		for _, tested := range response.GetRuns() {
			lines = append(append(lines, "", render.Heading(tested.GetCommand(), t)), p.runLines(tested.GetRun())...)
		}
		return f.Show(p.linesPage("Test an event", Screen+":test-event", lines))
	})
}

// What is wrong with fields typed to lay over a sample, nothing for none or a JSON object
func object(typed string) string {
	if strings.TrimSpace(typed) == "" {
		return ""
	}
	var fields map[string]any
	if json.Unmarshal([]byte(typed), &fields) != nil {
		return "Must be a JSON object, such as {\"command\": \"ls\"}."
	}
	return ""
}

// Lines telling what a check of one hook found
func (p *page) checkLines(c *hooksv1.Checked) []string {
	t := p.kit.Env.Theme
	var lines []string
	if c.GetRunnable() {
		lines = append(lines, render.Good("The script is there and can run.", t))
	} else {
		lines = append(lines, render.Bad("The script is missing or can't run.", t))
	}
	switch {
	case c.GetAllowed() && c.GetApprovedAt() != "" && c.GetApprovedAt() != "?":
		lines = append(lines, render.Good("You allowed it on "+when(c.GetApprovedAt())+".", t))
	case c.GetAllowed() && c.GetApprovedAt() == "":
		lines = append(lines, render.Good("It runs without asking you first.", t))
	case c.GetAllowed():
		lines = append(lines, render.Good("You allowed it.", t))
	default:
		lines = append(lines, render.Bad("Not allowed, so it doesn't run.", t))
	}
	switch {
	case c.GetChanged():
		lines = append(lines, render.Bad("Its script changed on "+when(c.GetChangedNow())+", after you allowed it.", t))
	case c.GetCompared():
		lines = append(lines, render.Good("Unchanged since you allowed it.", t))
	}
	if run := c.GetRun(); run != nil {
		switch c.GetOutput() {
		case "empty":
			lines = append(lines, render.Good("It ran and answered nothing, so it only watches.", t))
		case "json":
			lines = append(lines, render.Good("It ran and its answer is well formed.", t))
		case "invalid":
			lines = append(lines, render.Bad("Its answer is not well formed: "+strings.TrimSpace(run.GetStdout()), t))
		default:
			lines = append(lines, p.runLines(run)[0])
		}
	}
	if n := int(c.GetProblems()); n > 0 {
		return append(lines, render.Bad(render.Count(n, "problem")+" found.", t))
	}
	return append(lines, render.Good("No problems found.", t))
}

// Checks one hook and shows what was found on a page
func (p *page) check(hook *hooksv1.Hook) tea.Cmd {
	p.hub.Busy("Checking " + hook.GetCommand() + "…")
	return tui.Call(p.hub, func() (*hooksv1.CheckResponse, error) {
		return p.service.Check(p.kit.Env.Ctx, &hooksv1.CheckRequest{Profile: p.profile, Event: hook.GetEvent(), Command: hook.GetCommand()})
	}, func(response *hooksv1.CheckResponse) tea.Cmd {
		p.hub.Quiet()
		var lines []string
		for _, c := range response.GetChecks() {
			lines = append(lines, p.checkLines(c)...)
		}
		return p.showPage("Check", Screen+":check", lines)
	})
}

// Checks every hook and shows what was found for each, under its command and event
func (p *page) checkAll() tea.Cmd {
	p.hub.Busy("Checking every hook…")
	return tui.Call(p.hub, func() (*hooksv1.CheckResponse, error) {
		return p.service.Check(p.kit.Env.Ctx, &hooksv1.CheckRequest{Profile: p.profile})
	}, func(response *hooksv1.CheckResponse) tea.Cmd {
		p.hub.Quiet()
		t := p.kit.Env.Theme
		var lines []string
		problems := 0
		for _, c := range response.GetChecks() {
			problems += int(c.GetProblems())
			lines = append(lines, "", render.Heading(c.GetCommand(), t)+render.Dim(" · "+shellconfig.PointOf(c.GetEvent()).Title, t))
			lines = append(lines, p.checkLines(c)...)
		}
		summary := render.Good("Every hook looks healthy.", t)
		if problems > 0 {
			summary = render.Bad(render.Count(problems, "problem")+" found.", t)
		}
		return p.showPage("Check every hook", Screen+":check-all", append([]string{summary}, lines...))
	})
}

package app

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	approvalsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/approvals/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/approvals/v1/approvalsv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the allowed commands
const allowedContext = "tui.approvals"

var actions = []keys.Action{
	{ID: "tui.approvals.check", Context: allowedContext, Label: "check", Doc: "Test a command against the rules without running it", Default: []string{"c"}},
	{ID: "tui.approvals.suggest", Context: allowedContext, Label: "suggest", Doc: "Suggest commands to allow from the ones you approved before", Default: []string{"s"}},
	{ID: "tui.approvals.kind", Context: allowedContext, Label: "allow kind", Doc: "Allow a kind of risky action without asking", Default: []string{"k"}},
}

// Headings of the rows of the allowed commands: what each entry lets through, then the screens of the approval tools
const (
	kindsHeading    = "Kinds of risky action"
	patternsHeading = "Patterns"
	commandsHeading = "Exact commands"
	toolsHeading    = "More"
)

// Help under the field an allowed command is typed into
const patternHelp = "An exact command, or a pattern such as npm run * where * stands for anything.\nA command joined to others with && or ; never matches."

type screens struct{}

func Register(host *app.App) {
	host.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := approvalsv1connect.NewApprovalsServiceClient(conn)
	return []tui.Screen{{
		ID:      "approvals",
		Group:   tui.GroupSafety,
		Order:   10,
		Title:   "Allowed commands",
		Short:   "Choose the commands that run without asking",
		Actions: actions,
		Where:   map[string]string{allowedContext: "allowed commands"},
		Usage:   "[check|suggest]",
		// Opened on check or suggest, as the settings of approvals open it, the screen starts with that
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			a := &allowed{kit: kit, service: service}
			opened := a.open()
			switch strings.TrimSpace(argument) {
			case "check":
				return tea.Batch(opened, a.check())
			case "suggest":
				return tea.Batch(opened, a.proposals())
			}
			return opened
		},
	}}
}

type allowed struct {
	kit     *tui.Kit
	service approvalsv1connect.ApprovalsServiceClient
	hub     *cards.Hub
	list    *cards.List
	entries []string
	kinds   []*approvalsv1.Kind
	loaded  bool
	err     error
}

// Row of the allowed commands: an entry of the allowlist, or one of the tools when entry is empty
type row struct {
	entry string
	tool  *tool
}

// Screen of an approval tool a row opens: its name, what it does, its key and how it opens
type tool struct {
	title, about, action string
	open                 func() tea.Cmd
}

// Rows in the order of their headings: kinds of risky action, patterns, exact commands, then the tools
func (a *allowed) rows() []row {
	if !a.loaded || a.err != nil {
		return nil
	}
	var out []row
	for _, heading := range []string{kindsHeading, patternsHeading, commandsHeading} {
		for _, entry := range a.entries {
			if a.heading(entry) == heading {
				out = append(out, row{entry: entry})
			}
		}
	}
	for _, t := range a.tools() {
		out = append(out, row{tool: &t})
	}
	return out
}

// The approval tools: suggestions from your approvals, the check of a command, and allowing a kind of risky action
func (a *allowed) tools() []tool {
	return []tool{
		{"Suggest commands to allow", "Commands you approved often, which you can allow for good", "tui.approvals.suggest", a.proposals},
		{"Check a command", "See what the rules do with a command, without running it", "tui.approvals.check", a.check},
		{"Allow a kind of risky action", "Let a whole kind of risky action run without asking", "tui.approvals.kind", a.allowKind},
	}
}

// Kind of risky action an entry names, nil for a command or a pattern
func (a *allowed) kindOf(entry string) *approvalsv1.Kind {
	for _, k := range a.kinds {
		if k.GetValue() == entry {
			return k
		}
	}
	return nil
}

// Heading an entry of the allowlist is listed under
func (a *allowed) heading(entry string) string {
	switch {
	case a.kindOf(entry) != nil:
		return kindsHeading
	case strings.ContainsAny(entry, "*?["):
		return patternsHeading
	}
	return commandsHeading
}

// What an entry of the allowlist lets through, in a few words
func (a *allowed) through(entry string) string {
	switch a.heading(entry) {
	case kindsHeading:
		return "Every action of this kind"
	case patternsHeading:
		return "Every command that matches"
	}
	return "This command exactly"
}

// Title of a kind of risky action, with a capital first letter
func title(text string) string {
	if text == "" {
		return ""
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

func (a *allowed) open() tea.Cmd {
	t := a.kit.Env.Theme
	var shown []row
	a.list = cards.NewList(a.kit.Env, cards.ListSpec{
		Title:   "Allowed commands",
		Glyph:   t.Glyphs.Settings,
		Key:     "tui:approvals",
		Context: allowedContext,
		Shared:  []string{cards.HubContext},
		Primary: "hub.use",
		Intro: func() []string {
			return []string{"Commands the agent runs without asking you first. Every other risky command waits for your yes."}
		},
		Columns: []render.Column{{}, {}, {Flex: 1}},
		Rows: func() [][]string {
			shown = a.rows()
			cells := make([][]string, len(shown))
			for i, r := range shown {
				if r.tool != nil {
					cells[i] = []string{r.tool.title, render.Dim(t.Glyphs.Forward, t), render.Dim(r.tool.about, t)}
					continue
				}
				name := r.entry
				if a.kindOf(r.entry) != nil {
					name = title(r.entry)
				}
				cells[i] = []string{name, "", render.Dim(a.through(r.entry), t)}
			}
			return cells
		},
		Group: func(i int) string {
			if shown[i].tool != nil {
				return toolsHeading
			}
			return a.heading(shown[i].entry)
		},
		Detail: func(i int) []string { return a.detail(shown[i]) },
		Banner: func() string {
			if a.loaded && a.err == nil && len(a.entries) == 0 {
				return render.Dim("Nothing runs without asking yet. "+a.kit.Env.Keys.Display("hub.add")+" adds a command or a pattern such as git push *.", t)
			}
			return ""
		},
		Empty: func() string {
			switch {
			case a.err != nil:
				return "The allowed commands could not be read: " + strings.TrimSuffix(tui.Reason(a.err), ".") + ". " + a.kit.Env.Keys.Display("hub.refresh") +
					" tries again."
			case !a.loaded:
				return "Reading the allowed commands…"
			}
			return ""
		},
		Free: map[string]bool{"hub.add": true, "hub.refresh": true, "tui.approvals.check": true, "tui.approvals.suggest": true, "tui.approvals.kind": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			switch {
			case i < 0 || i >= len(shown):
				return []wm.Hint{{Action: "hub.add"}, {Action: "tui.approvals.suggest"}, {Action: "tui.approvals.check"}, {Action: "tui.approvals.kind"}}
			case shown[i].tool != nil:
				hints := []wm.Hint{{Action: "hub.use", Label: "open"}, {Action: "hub.add"}}
				for _, other := range a.tools() {
					hints = append(hints, wm.Hint{Action: other.action})
				}
				return hints
			case a.kindOf(shown[i].entry) != nil:
				return []wm.Hint{{Action: "hub.remove"}, {Action: "hub.add"}, {Action: "tui.approvals.suggest"}, {Action: "tui.approvals.check"},
					{Action: "tui.approvals.kind"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "change"}, {Action: "hub.remove"}, {Action: "hub.add"}, {Action: "tui.approvals.suggest"},
				{Action: "tui.approvals.check"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				if r := shown[i]; r.tool != nil {
					return r.tool.open()
				} else if a.kindOf(r.entry) == nil {
					return a.change(r.entry)
				}
				return nil
			},
			"hub.add": func(*cards.List, int) tea.Cmd {
				field := cards.Field{Label: "Command or pattern", Help: patternHelp}
				return a.kit.Ask(a.hub, "Allow a command", field, "Allow", func(pattern string) tea.Cmd { return a.allow(pattern) })
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				entry := shown[i].entry
				if entry == "" {
					return nil
				}
				name := entry
				if a.kindOf(entry) != nil {
					name = strings.ToLower(title(entry))
				}
				l.Confirm("Ask again before "+name+"? The agent then waits for your yes each time.", func() tea.Cmd {
					return tui.Call(a.hub, func() (*approvalsv1.DisallowResponse, error) {
						return a.service.Disallow(a.kit.Env.Ctx, &approvalsv1.DisallowRequest{Patterns: []string{entry}})
					}, func(*approvalsv1.DisallowResponse) tea.Cmd {
						return tea.Batch(a.hub.Done("The agent asks again before "+name+"."), a.load())
					})
				})
				return nil
			},
			"hub.refresh":           func(*cards.List, int) tea.Cmd { return a.load() },
			"tui.approvals.check":   func(*cards.List, int) tea.Cmd { return a.check() },
			"tui.approvals.suggest": func(*cards.List, int) tea.Cmd { return a.proposals() },
			"tui.approvals.kind":    func(*cards.List, int) tea.Cmd { return a.allowKind() },
		},
	})
	a.list.ToggleDetail()
	a.hub = a.kit.Hub("approvals", a.list)
	return tea.Batch(a.kit.WM.ShowOverlay(a.hub), a.load())
}

// Lines under the list about a row: what the entry lets through and how to take it back, or what a tool does
func (a *allowed) detail(r row) []string {
	t := a.kit.Env.Theme
	if r.tool != nil {
		return []string{r.tool.about + ". " + a.kit.Env.Keys.Display(r.tool.action) + " opens it from any row."}
	}
	switch kind := a.kindOf(r.entry); {
	case kind != nil:
		lines := []string{"The agent may " + r.entry + " without asking."}
		if about := kind.GetAbout(); about != "" {
			lines = append(lines, render.Dim(about+".", t))
		}
		return lines
	case a.heading(r.entry) == patternsHeading:
		return []string{"The agent runs every command that matches " + r.entry + " without asking.", render.Dim("* stands for anything, ? for one character.", t)}
	}
	return []string{"The agent runs " + r.entry + " without asking, exactly as written."}
}

func (a *allowed) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := a.service.List(a.kit.Env.Ctx, &approvalsv1.ListRequest{})
		return func() tea.Cmd {
			a.err, a.loaded = err, true
			if err == nil {
				a.entries, a.kinds = response.GetAllowed(), response.GetKinds()
			}
			a.list.Refresh()
			return nil
		}
	})
}

// Adds the pattern to the allowlist, saying when it was there already
func (a *allowed) allow(pattern string) tea.Cmd {
	if pattern = strings.TrimSpace(pattern); pattern == "" {
		return nil
	}
	name := pattern
	if a.kindOf(pattern) != nil {
		name = strings.ToLower(title(pattern))
	}
	return tui.Call(a.hub, func() (*approvalsv1.AllowResponse, error) {
		return a.service.Allow(a.kit.Env.Ctx, &approvalsv1.AllowRequest{Patterns: []string{pattern}})
	}, func(response *approvalsv1.AllowResponse) tea.Cmd {
		if len(response.GetAdded()) == 0 {
			return a.hub.Say(title(name)+" is allowed already.", cards.NoteInfo)
		}
		done := "The agent now runs " + pattern + " without asking."
		if a.kindOf(pattern) != nil {
			done = "The agent now may " + pattern + " without asking."
		}
		return tea.Batch(a.hub.Done(done), a.load())
	})
}

// Changes an allowed command or pattern: the new one allowed, then the old one taken out
func (a *allowed) change(entry string) tea.Cmd {
	field := cards.Field{Label: "Command or pattern", Value: entry, Help: patternHelp}
	return a.kit.Ask(a.hub, "Change", field, "Save", func(pattern string) tea.Cmd {
		if pattern = strings.TrimSpace(pattern); pattern == "" || pattern == entry {
			return nil
		}
		return tui.Call(a.hub, func() (struct{}, error) {
			if _, err := a.service.Allow(a.kit.Env.Ctx, &approvalsv1.AllowRequest{Patterns: []string{pattern}}); err != nil {
				return struct{}{}, err
			}
			_, err := a.service.Disallow(a.kit.Env.Ctx, &approvalsv1.DisallowRequest{Patterns: []string{entry}})
			return struct{}{}, err
		}, func(struct{}) tea.Cmd {
			return tea.Batch(a.hub.Done("The agent now runs "+pattern+" without asking, in place of "+entry+"."), a.load())
		})
	})
}

// Picks a kind of risky action to allow without asking
func (a *allowed) allowKind() tea.Cmd {
	var choices []tui.Choice
	for _, k := range a.kinds {
		if slices.Contains(a.entries, k.GetValue()) {
			continue
		}
		choices = append(choices, tui.Choice{ID: k.GetValue(), Title: title(k.GetValue()), Detail: k.GetAbout()})
	}
	if len(choices) == 0 {
		return a.hub.Say("Every kind of risky action runs without asking already.", cards.NoteInfo)
	}
	return a.kit.Pick(a.hub, "Allow a kind of risky action", "allow", choices, "", func(choice tui.Choice) tea.Cmd { return a.allow(choice.ID) })
}

// Where a command runs, in words
func where(backend string) string {
	switch backend {
	case "", "local":
		return "On this computer"
	case "docker":
		return "In a Docker sandbox"
	case "ssh":
		return "On another computer, over SSH"
	}
	return title(backend)
}

// Asks for a command and shows what the approval rules do with it where this profile runs commands, the command never run
func (a *allowed) check() tea.Cmd {
	field := cards.Field{Label: "Command", Help: "It is not run. You see what the rules would do with it."}
	return a.kit.Ask(a.hub, "Check a command", field, "Check", func(command string) tea.Cmd {
		if command == "" {
			return nil
		}
		return tui.Call(a.hub, func() (*approvalsv1.TestResponse, error) {
			return a.service.Test(a.kit.Env.Ctx, &approvalsv1.TestRequest{Command: command})
		}, func(v *approvalsv1.TestResponse) tea.Cmd {
			look := a.kit.Env.Theme
			lines := []string{render.Heading(v.GetCommand(), look), "", verdict(v.GetVerdict(), look)}
			if v.GetRule() != "" {
				lines = append(lines, "Decided by the rule: "+v.GetRule())
			}
			if v.GetDetail() != "" {
				lines = append(lines, render.Dim(v.GetDetail(), look))
			}
			lines = append(lines, render.Dim("Where it runs: "+where(v.GetBackend()), look))
			if len(v.GetVariants()) > 0 {
				lines = append(lines, "", "How the rules read it:")
				for _, variant := range v.GetVariants() {
					lines = append(lines, "  "+variant)
				}
			}
			return a.hub.Push(cards.NewPage(a.kit.Env, cards.PageSpec{Title: "Check a command", Key: "tui:approvals:check", Lines: func(w int) []string {
				var out []string
				for _, line := range lines {
					out = append(out, strings.Split(render.Wrap(line, max(w, 8)), "\n")...)
				}
				return out
			}}))
		})
	})
}

// What the approval rules do with a command, as a sentence
func verdict(v string, look theme.Theme) string {
	switch v {
	case "allow":
		return render.Good("It runs without asking.", look)
	case "ask-approval":
		return render.Warm("The agent asks you before it runs.", look)
	case "hardline-deny":
		return render.Bad("It is always blocked by a rule built into icebath.", look)
	case "user-deny":
		return render.Bad("It is blocked by your Blocked commands.", look)
	}
	return title(v) + "."
}

// How much approval history a reading covers, in words
func window(days int32) string {
	if days > 0 {
		return fmt.Sprintf("the last %d days", days)
	}
	return "every approval so far"
}

// Whole number of zero or more typed into a field, refused under it in words
func whole(least int) func(string) string {
	return func(value string) string {
		n, err := strconv.Atoi(strings.TrimSpace(value))
		switch {
		case err != nil:
			return "Enter a whole number."
		case n < least:
			return "Must be at least " + strconv.Itoa(least) + "."
		}
		return ""
	}
}

// Allowed commands the approval history suggests, the most approved first: enter allows the highlighted one after a question, edit changes
// how much history is read
func (a *allowed) proposals() tea.Cmd {
	var (
		request = &approvalsv1.SuggestRequest{}
		result  *approvalsv1.SuggestResponse
		loaded  bool
		err     error
		list    *cards.List
	)
	t := a.kit.Env.Theme
	at := func(i int) *approvalsv1.Proposal {
		if i < 0 || i >= len(result.GetProposals()) {
			return nil
		}
		return result.GetProposals()[i]
	}
	var load func() tea.Cmd
	load = func() tea.Cmd {
		a.hub.Busy("Reading your approval history…")
		return tui.Async(func() func() tea.Cmd {
			response, failure := a.service.Suggest(a.kit.Env.Ctx, request)
			return func() tea.Cmd {
				a.hub.Quiet()
				result, err, loaded = response, failure, true
				list.Refresh()
				return nil
			}
		})
	}
	list = cards.NewList(a.kit.Env, cards.ListSpec{
		Title:   "Suggestions",
		Glyph:   t.Glyphs.Settings,
		Key:     "tui:approvals:proposals",
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			lead := "Commands you approved often, which the agent could run without asking."
			if !loaded || err != nil {
				return []string{lead}
			}
			return []string{lead, "Read from " + window(result.GetDays()) + ". " + strconv.Itoa(int(result.GetAllowed())) + " commands are allowed now."}
		},
		Columns: []render.Column{{}, {}, {Flex: 1}},
		Yield:   []int{2},
		Rows: func() [][]string {
			rows := make([][]string, len(result.GetProposals()))
			for i, p := range result.GetProposals() {
				example := ""
				if len(p.GetExamples()) > 0 {
					example = p.GetExamples()[0]
				}
				rows[i] = []string{p.GetPattern(), render.Dim("Approved "+render.Count(int(p.GetCount()), "time"), t), render.Dim(example, t)}
			}
			return rows
		},
		Detail: func(i int) []string {
			p := at(i)
			if p == nil {
				return nil
			}
			covers := "Allowing it lets every command that matches run without asking."
			if p.GetKind() == "class" {
				covers = "Allowing it lets every action of this risky kind run without asking."
			}
			out := []string{covers, fmt.Sprintf("You approved it %s.", render.Count(int(p.GetCount()), "time"))}
			if len(p.GetClasses()) > 0 {
				out = append(out, render.Dim("Flagged as: "+strings.Join(p.GetClasses(), ", "), t))
			}
			if len(p.GetExamples()) > 0 {
				out = append(out, "", "Commands you approved:")
				for _, e := range p.GetExamples() {
					out = append(out, render.Dim("  "+e, t))
				}
			}
			return out
		},
		Empty: func() string {
			switch {
			case err != nil:
				return "Your approval history could not be read: " + strings.TrimSuffix(tui.Reason(err), ".") + "."
			case !loaded:
				return "Reading your approval history…"
			}
			return "No suggestions from " + window(result.GetDays()) + ". Nothing risky was approved often enough, or what was is too risky to allow for good."
		},
		Free: map[string]bool{"hub.edit": true, "hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if at(i) == nil {
				return []wm.Hint{{Action: "hub.edit", Label: "change reading"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "allow"}, {Action: "hub.edit", Label: "change reading"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				p := at(i)
				if p == nil {
					return nil
				}
				l.Confirm("Always allow "+p.GetPattern()+"? The agent then runs it without asking.", func() tea.Cmd {
					return tui.Call(a.hub, func() (*approvalsv1.AllowResponse, error) {
						return a.service.Allow(a.kit.Env.Ctx, &approvalsv1.AllowRequest{Patterns: []string{p.GetPattern()}})
					}, func(response *approvalsv1.AllowResponse) tea.Cmd {
						if len(response.GetAdded()) == 0 {
							return a.hub.Say(p.GetPattern()+" is allowed already.", cards.NoteInfo)
						}
						return tea.Batch(a.hub.Done("The agent now runs "+p.GetPattern()+" without asking."), load(), a.load())
					})
				})
				return nil
			},
			"hub.edit": func(*cards.List, int) tea.Cmd {
				days := int32(90)
				if request.Days != nil {
					days = request.GetDays()
				}
				number := func(n, fallback int32) string {
					if n <= 0 {
						n = fallback
					}
					return strconv.Itoa(int(n))
				}
				var form *cards.Form
				form = cards.NewForm(a.kit.Env, cards.FormSpec{Title: "What to read", Intro: []string{"How much of your approval history the suggestions come from."},
					Submit: "Read again", Fields: []cards.Field{
						{ID: "days", Label: "Days of history", Kind: cards.FieldNumber, Value: strconv.Itoa(int(days)), Help: "0 reads every approval so far.",
							Check: whole(0)},
						{ID: "min", Label: "Approvals needed", Kind: cards.FieldNumber, Value: number(request.GetMinCount(), 2), Check: whole(1),
							Help: "How many times you approved a command before it is suggested."},
						{ID: "limit", Label: "Most suggestions", Kind: cards.FieldNumber, Value: number(request.GetLimit(), 20), Check: whole(1)},
					}, Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
						read := func(id string) int32 {
							n, _ := strconv.Atoi(strings.TrimSpace(values[id]))
							return int32(n)
						}
						d := read("days")
						request = &approvalsv1.SuggestRequest{Days: &d, MinCount: read("min"), Limit: read("limit")}
						return tea.Batch(a.hub.Close(form), load())
					}})
				return a.hub.Push(form)
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return load() },
		},
	})
	list.ToggleDetail()
	return tea.Batch(a.hub.Push(list), load())
}

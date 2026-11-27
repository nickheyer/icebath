package app

import (
	"fmt"
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
	{ID: "tui.approvals.check", Context: allowedContext, Label: "check a command", Doc: "show what the approval rules do with a command, without running it", Default: []string{"c"}},
	{ID: "tui.approvals.suggest", Context: allowedContext, Label: "proposals", Doc: "propose allowed commands from the risky commands you approved before", Default: []string{"s"}},
	{ID: "tui.approvals.kind", Context: allowedContext, Label: "allow a kind", Doc: "always allow a kind of risky action, such as one the shell asks about", Default: []string{"k"}},
}

type screens struct{}

func Register(host *app.App) {
	host.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := approvalsv1connect.NewApprovalsServiceClient(conn)
	return []tui.Screen{{
		ID:      "approvals",
		Title:   "Allowed commands",
		Short:   "What runs without asking: the allowed commands, proposals from your approval history, and a check of any command",
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
	rows    []string
	kinds   []*approvalsv1.Kind
	loaded  bool
	err     error
}

// What an entry of the allowlist lets through
func (a *allowed) kind(entry string) string {
	for _, k := range a.kinds {
		if k.GetValue() == entry {
			return "every action of this risky kind"
		}
	}
	if strings.ContainsAny(entry, "*?[") {
		return "commands that match this pattern"
	}
	return "this command exactly"
}

func (a *allowed) open() tea.Cmd {
	a.list = cards.NewList(a.kit.Env, cards.ListSpec{
		Title:   "Allowed commands",
		Glyph:   a.kit.Env.Theme.Glyphs.Settings,
		Key:     "tui:approvals",
		Context: allowedContext,
		Shared:  []string{cards.HubContext},
		Columns: []render.Column{{Title: "Entry", Flex: 1}, {Title: "Lets through"}},
		Rows: func() [][]string {
			rows := make([][]string, len(a.rows))
			for i, entry := range a.rows {
				rows[i] = []string{entry, a.kind(entry)}
			}
			return rows
		},
		Empty: func() string {
			return tui.Waiting(a.loaded, a.err, "Nothing is allowed without asking yet. Add a command or a pattern such as `git push *`")
		},
		Free: map[string]bool{"hub.add": true, "hub.refresh": true, "tui.approvals.check": true, "tui.approvals.suggest": true, "tui.approvals.kind": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			hints := []wm.Hint{{Action: "hub.add", Label: "add"}}
			if i >= 0 {
				hints = append(hints, wm.Hint{Action: "hub.remove", Label: "remove"})
			}
			return append(hints, wm.Hint{Action: "tui.approvals.suggest"}, wm.Hint{Action: "tui.approvals.kind"}, wm.Hint{Action: "tui.approvals.check"})
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.add": func(*cards.List, int) tea.Cmd {
				field := cards.Field{Label: "Command or pattern", Help: "An exact command, or a glob such as `npm run *`; compound commands never match"}
				return a.kit.Ask(a.hub, "Allow without asking", field, "Allow", func(pattern string) tea.Cmd { return a.allow(pattern) })
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				entry := a.rows[i]
				l.Confirm("Ask again before "+entry+"?", func() tea.Cmd {
					return tui.Call(a.hub, func() (*approvalsv1.DisallowResponse, error) {
						return a.service.Disallow(a.kit.Env.Ctx, &approvalsv1.DisallowRequest{Patterns: []string{entry}})
					}, func(*approvalsv1.DisallowResponse) tea.Cmd { return tea.Batch(a.hub.Done("Removed "+entry), a.load()) })
				})
				return nil
			},
			"hub.refresh":           func(*cards.List, int) tea.Cmd { return a.load() },
			"tui.approvals.check":   func(*cards.List, int) tea.Cmd { return a.check() },
			"tui.approvals.suggest": func(*cards.List, int) tea.Cmd { return a.proposals() },
			"tui.approvals.kind": func(*cards.List, int) tea.Cmd {
				var choices []tui.Choice
				for _, k := range a.kinds {
					choices = append(choices, tui.Choice{ID: k.GetValue(), Title: k.GetValue(), Detail: k.GetAbout()})
				}
				return a.kit.Pick(a.hub, "Always allow a kind of risky action", "allow", choices, "", func(choice tui.Choice) tea.Cmd {
					return a.allow(choice.ID)
				})
			},
		},
	})
	a.hub = a.kit.Hub("approvals", a.list)
	return tea.Batch(a.kit.WM.ShowOverlay(a.hub), a.load())
}

func (a *allowed) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := a.service.List(a.kit.Env.Ctx, &approvalsv1.ListRequest{})
		return func() tea.Cmd {
			a.rows, a.kinds, a.err, a.loaded = response.GetAllowed(), response.GetKinds(), err, true
			a.list.Refresh()
			return nil
		}
	})
}

// Adds the pattern to the allowlist, failing when it was there already
func (a *allowed) allow(pattern string) tea.Cmd {
	if pattern = strings.TrimSpace(pattern); pattern == "" {
		return nil
	}
	return tui.Call(a.hub, func() (*approvalsv1.AllowResponse, error) {
		return a.service.Allow(a.kit.Env.Ctx, &approvalsv1.AllowRequest{Patterns: []string{pattern}})
	}, func(response *approvalsv1.AllowResponse) tea.Cmd {
		if len(response.GetAdded()) == 0 {
			return a.hub.Fail(pattern + " is allowed already")
		}
		return tea.Batch(a.hub.Done("Allowed "+pattern), a.load())
	})
}

// Asks for a command and shows what the approval rules do with it where this profile runs commands, the command never run
func (a *allowed) check() tea.Cmd {
	return a.kit.Ask(a.hub, "Check a command", cards.Field{Label: "Command"}, "Check", func(command string) tea.Cmd {
		if command == "" {
			return nil
		}
		return tui.Call(a.hub, func() (*approvalsv1.TestResponse, error) {
			return a.service.Test(a.kit.Env.Ctx, &approvalsv1.TestRequest{Command: command})
		}, func(v *approvalsv1.TestResponse) tea.Cmd {
			look := a.kit.Env.Theme
			lines := []string{"command: " + v.GetCommand(), "where it runs: " + v.GetBackend(), "verdict: " + verdict(v.GetVerdict(), look)}
			if v.GetRule() != "" {
				lines = append(lines, "rule: "+v.GetRule())
			}
			if v.GetDetail() != "" {
				lines = append(lines, render.Dim(v.GetDetail(), look))
			}
			if len(v.GetVariants()) > 0 {
				lines = append(lines, "", "spellings the rules read:")
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

// What the approval rules do with a command, in plain words
func verdict(v string, look theme.Theme) string {
	switch v {
	case "allow":
		return render.Good("runs without asking", look)
	case "ask-approval":
		return render.Warm("asks before it runs", look)
	case "hardline-deny":
		return render.Bad("always blocked by a built-in rule", look)
	case "user-deny":
		return render.Bad("blocked by your always-deny patterns", look)
	}
	return v
}

// Allowed commands the approval history proposes, the most approved first: enter allows the highlighted one after a question, edit changes
// how much history is read
func (a *allowed) proposals() tea.Cmd {
	var (
		request = &approvalsv1.SuggestRequest{}
		result  *approvalsv1.SuggestResponse
		loaded  bool
		err     error
		list    *cards.List
	)
	at := func(i int) *approvalsv1.Proposal {
		if i < 0 || i >= len(result.GetProposals()) {
			return nil
		}
		return result.GetProposals()[i]
	}
	window := func() string {
		if days := result.GetDays(); days > 0 {
			return fmt.Sprintf("the last %d days", days)
		}
		return "every approval so far"
	}
	var load func() tea.Cmd
	load = func() tea.Cmd {
		a.hub.Busy("reading the approval history…")
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
		Title:   "Proposed allowed commands",
		Glyph:   a.kit.Env.Theme.Glyphs.Settings,
		Key:     "tui:approvals:proposals",
		Context: cards.HubContext,
		Primary: "hub.use",
		Columns: []render.Column{{Title: "Pattern", Max: 40}, {Title: "Approved"}, {Title: "Example", Flex: 1}},
		Yield:   []int{2},
		Rows: func() [][]string {
			rows := make([][]string, len(result.GetProposals()))
			for i, p := range result.GetProposals() {
				example := ""
				if len(p.GetExamples()) > 0 {
					example = p.GetExamples()[0]
				}
				rows[i] = []string{p.GetPattern(), strconv.Itoa(int(p.GetCount())) + "×", example}
			}
			return rows
		},
		Detail: func(i int) []string {
			p := at(i)
			if p == nil {
				return nil
			}
			covers := "commands that match this pattern"
			if p.GetKind() == "class" {
				covers = "every command of this risky class"
			}
			out := []string{p.GetPattern() + " · " + covers, fmt.Sprintf("approved %d times", p.GetCount())}
			if len(p.GetClasses()) > 0 {
				out = append(out, "risky classes:")
				for _, c := range p.GetClasses() {
					out = append(out, "  "+c)
				}
			}
			if len(p.GetExamples()) > 0 {
				out = append(out, "commands you approved:")
				for _, e := range p.GetExamples() {
					out = append(out, "  "+e)
				}
			}
			return out
		},
		Banner: func() string {
			if !loaded || err != nil {
				return ""
			}
			return fmt.Sprintf("%s · %d allowed commands", window(), result.GetAllowed())
		},
		Empty: func() string {
			if !loaded || err != nil {
				return tui.Waiting(loaded, err, "")
			}
			return "No allowlist candidates in " + window() + ": nothing risky was approved often enough, or what was is too risky to allow for good"
		},
		Free: map[string]bool{"hub.edit": true, "hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if at(i) == nil {
				return []wm.Hint{{Action: "hub.edit", Label: "change"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "allow"}, {Action: "hub.edit", Label: "change"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				p := at(i)
				if p == nil {
					return nil
				}
				l.Confirm("Always allow "+p.GetPattern()+"?", func() tea.Cmd {
					return tui.Call(a.hub, func() (*approvalsv1.AllowResponse, error) {
						return a.service.Allow(a.kit.Env.Ctx, &approvalsv1.AllowRequest{Patterns: []string{p.GetPattern()}})
					}, func(response *approvalsv1.AllowResponse) tea.Cmd {
						if len(response.GetAdded()) == 0 {
							return a.hub.Fail(p.GetPattern() + " was not saved")
						}
						return tea.Batch(a.hub.Done("Allowed "+p.GetPattern()), load(), a.load())
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
				form = cards.NewForm(a.kit.Env, cards.FormSpec{Title: "Approval history", Submit: "read again", Fields: []cards.Field{
					{ID: "days", Label: "days of history", Kind: cards.FieldNumber, Value: strconv.Itoa(int(days)), Help: "0 reads every approval so far"},
					{ID: "min", Label: "approvals needed", Kind: cards.FieldNumber, Value: number(request.GetMinCount(), 2)},
					{ID: "limit", Label: "most proposals", Kind: cards.FieldNumber, Value: number(request.GetLimit(), 20)},
				}, Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
					read := func(id string) (int32, bool) {
						n, err := strconv.Atoi(strings.TrimSpace(values[id]))
						return int32(n), err == nil && n >= 0
					}
					d, okDays := read("days")
					m, okMin := read("min")
					l, okLimit := read("limit")
					if !okDays || !okMin || !okLimit {
						return a.hub.Fail("Enter whole numbers of 0 or more")
					}
					request = &approvalsv1.SuggestRequest{Days: &d, MinCount: m, Limit: l}
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

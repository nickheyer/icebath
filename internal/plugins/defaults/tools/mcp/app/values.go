package app

import (
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	mcpv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/mcp/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// The values a server's settings read as ${NAME}, each set or not: enter sets or changes a value, ctrl+d removes one. Values are hidden, so
// a row says whether it is set
func (s *servers) values(server *mcpv1.Server) tea.Cmd {
	name := server.GetName()
	t := s.kit.Env.Theme
	var names []string
	set := func(value string) bool {
		current := s.find(name)
		return current != nil && slices.Contains(current.GetValues(), value) && !slices.Contains(current.GetMissing(), value)
	}
	view := cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   "Values",
		Glyph:   t.Glyphs.MCP,
		Key:     Screen + ":" + name + ":values",
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search values",
		Intro: func() []string {
			return []string{"Values that the settings of " + name + " read as ${NAME}, such as API keys."}
		},
		Columns: []render.Column{{}, {}, {Flex: 1}},
		Rows: func() [][]string {
			names = nil
			if current := s.find(name); current != nil {
				names = slices.Clone(current.GetValues())
			}
			rows := make([][]string, len(names))
			for i, value := range names {
				state := render.Warm("Not set", t)
				if set(value) {
					state = "Set"
				}
				rows[i] = []string{value, "", state}
			}
			return rows
		},
		Detail: func(i int) []string {
			return []string{"The text put in place of ${" + names[i] + "}. It is never shown again once it is kept, and an environment variable " +
				"of the same name wins over it."}
		},
		Empty: func() string { return "The settings of " + name + " read no values now." },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			switch {
			case i < 0:
				return nil
			case set(names[i]):
				return []wm.Hint{{Action: "hub.use", Label: "change"}, {Action: "hub.remove", Label: "remove"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "set"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd { return s.askValue(names[i]) },
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if !set(names[i]) {
					return nil
				}
				variable := names[i]
				l.Confirm(cards.Question{Text: "Remove the value of " + variable + "? A server that reads it can't connect until it is set again.",
					Verb: "Remove", Deletes: true}, func() tea.Cmd { return s.setValue(variable, "") })
				return nil
			},
		},
	})
	s.views.Add(view)
	return s.hub.Push(view)
}

// Asks the value of a name, hidden as it is typed
func (s *servers) askValue(variable string) tea.Cmd {
	field := cards.Field{Label: variable, Kind: cards.FieldSecret, Help: "The text put in place of ${" + variable + "} in the settings, such as an API key."}
	return s.kit.Ask(s.hub, variable, field, "Save", func(text string) tea.Cmd { return s.setValue(variable, text) })
}

// Sets a value the settings of servers read, or removes it when empty, through the one function that keeps values
func (s *servers) setValue(variable, value string) tea.Cmd {
	return tui.Call(s.hub, func() (*mcpv1.SetValueResponse, error) {
		return s.service.SetValue(s.kit.Env.Ctx, &mcpv1.SetValueRequest{Profile: s.profile, Name: variable, Value: value})
	}, func(*mcpv1.SetValueResponse) tea.Cmd {
		told := "Set " + variable + "."
		if value == "" {
			told = "Removed the value of " + variable + "."
		}
		return tea.Batch(s.hub.Done(told), s.load(""))
	})
}

// Tools of a server as a tick list: the server is asked for its tools and each one turned on or off for new sessions
func (s *servers) tools(server *mcpv1.Server) tea.Cmd {
	s.hub.Busy("Asking " + server.GetName() + " for its tools…")
	return tui.Call(s.hub, func() (*mcpv1.ProbeResponse, error) {
		return s.service.Probe(s.kit.Env.Ctx, &mcpv1.ProbeRequest{Profile: s.profile, Name: server.GetName()})
	}, func(r *mcpv1.ProbeResponse) tea.Cmd {
		if !r.GetOk() {
			return s.hub.Fail(strings.TrimSpace("Couldn't list the tools of " + server.GetName() + ". " + strs.Full(r.GetError()) + " " + strs.Full(r.GetNextStep())))
		}
		s.hub.Quiet()
		found := r.GetTools()
		on := make([]bool, len(found))
		for i, tool := range found {
			on[i] = allowed(server, tool.GetName())
		}
		t := s.kit.Env.Theme
		var list *cards.List
		// Keeps the filter the ticks come to; a change that is not kept is taken back on the list
		save := func(at int) tea.Cmd {
			changed, now := found[at].GetName(), on[at]
			var picked, dropped []string
			for i, tool := range found {
				if on[i] {
					picked = append(picked, tool.GetName())
				} else {
					dropped = append(dropped, tool.GetName())
				}
			}
			request := &mcpv1.SetFilterRequest{Profile: s.profile, Name: server.GetName()}
			switch {
			case len(picked) == len(found):
				request.Clear = true
			case server.GetIncluded() || len(server.GetExclude()) == 0:
				request.Mode, request.Values = "include", keepGlobs(server.GetInclude(), picked, dropped)
			default:
				request.Mode, request.Values = "exclude", keepGlobs(server.GetExclude(), dropped, picked)
			}
			ctx := s.kit.Env.Ctx
			return tui.Async(func() func() tea.Cmd {
				_, err := s.service.SetFilter(ctx, request)
				return func() tea.Cmd {
					if err != nil {
						on[at] = !now
						list.Refresh()
						return s.hub.Fail(tui.Reason(err))
					}
					list.Refresh()
					state := "off"
					if now {
						state = "on"
					}
					told := fmt.Sprintf("%s is now %s. %d of %s are on for new sessions.", changed, state, len(picked), render.Count(len(found), "tool"))
					return tea.Batch(s.hub.Done(told), s.load(""))
				}
			})
		}
		toggle := func(_ *cards.List, i int) tea.Cmd {
			if i < 0 || i >= len(found) {
				return nil
			}
			on[i] = !on[i]
			return save(i)
		}
		list = cards.NewList(s.kit.Env, cards.ListSpec{
			Title:   "Tools",
			Glyph:   t.Glyphs.Tools,
			Key:     Screen + ":" + server.GetName() + ":tools",
			Context: cards.HubContext,
			Primary: "hub.use",
			Pick:    true,
			Bar:     len(found) > 8,
			Search:  "Search tools",
			Intro: func() []string {
				return []string{"Tools of " + server.GetName() + " that the agent may use in new sessions."}
			},
			Columns: []render.Column{{}, {}, {Flex: 1, Whole: true}},
			Words:   func(i int) string { return found[i].GetDescription() },
			Rows: func() [][]string {
				cells := make([][]string, len(found))
				for i, tool := range found {
					box := "[ ]"
					if on[i] {
						box = "[x]"
					}
					cells[i] = []string{box, tool.GetName(), render.Dim(oneLine(tool.GetDescription()), t)}
				}
				return cells
			},
			Detail: func(i int) []string {
				if i < 0 || i >= len(found) {
					return nil
				}
				var lines []string
				if !list.Shows(2) && found[i].GetDescription() != "" {
					lines = append(lines, oneLine(found[i].GetDescription()))
				}
				if size := r.GetSchemaChars()[found[i].GetName()]; size > 0 {
					lines = append(lines, render.Dim("Its description takes "+render.Count(int(size), "character")+" of every request.", t))
				}
				return lines
			},
			Empty: func() string { return server.GetName() + " offers no tools." },
			Hints: func(_ *cards.List, i int) []wm.Hint {
				if i < 0 || i >= len(found) {
					return nil
				}
				if on[i] {
					return []wm.Hint{{Action: "hub.use", Label: "turn off"}}
				}
				return []wm.Hint{{Action: "hub.use", Label: "turn on"}}
			},
			Actions: map[string]func(*cards.List, int) tea.Cmd{"hub.use": toggle},
		})
		list.ToggleDetail()
		return s.hub.Push(list)
	})
}

// Text of several lines and runs of spaces as one line
func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// Whether the server's tool filter lets the agent use the tool, every tool when it names none
func allowed(server *mcpv1.Server, tool string) bool {
	if server.GetIncluded() {
		return slices.ContainsFunc(server.GetInclude(), func(pattern string) bool { return match(pattern, tool) })
	}
	return !slices.ContainsFunc(server.GetExclude(), func(pattern string) bool { return match(pattern, tool) })
}

// A filter list after a tick list change: the patterns it had that still hold, then the tools it names one by one. For an exclude list the
// named are the tools left off and the others those left on, for an include list the other way round. A pattern that matches a tool of
// the others no longer holds, since it would keep that tool on the wrong side, so it goes and the tools it covered are named
func keepGlobs(filter, named, others []string) []string {
	out := []string{}
	for _, pattern := range filter {
		if strings.ContainsAny(pattern, "*?[") && !slices.ContainsFunc(others, func(name string) bool { return match(pattern, name) }) {
			out = append(out, pattern)
		}
	}
	for _, name := range named {
		if !slices.ContainsFunc(out, func(pattern string) bool { return match(pattern, name) }) {
			out = append(out, name)
		}
	}
	return out
}

// Whether a name matches a tool filter entry: a literal name, or a glob with * ? and [...]
func match(pattern, name string) bool {
	if !strings.ContainsAny(pattern, "*?[") {
		return pattern == name
	}
	return globMatch([]rune(pattern), []rune(name))
}

func globMatch(p, s []rune) bool {
	for len(p) > 0 {
		switch p[0] {
		case '*':
			for i := len(s); i >= 0; i-- {
				if globMatch(p[1:], s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if len(s) == 0 {
				return false
			}
		case '[':
			end := slices.Index(p[1:], ']')
			if end < 0 {
				if len(s) == 0 || s[0] != '[' {
					return false
				}
				break
			}
			if len(s) == 0 {
				return false
			}
			class, negate := p[1:end+1], false
			if len(class) > 0 && class[0] == '!' {
				class, negate = class[1:], true
			}
			hit := false
			for i := 0; i < len(class); i++ {
				if i+2 < len(class) && class[i+1] == '-' {
					if class[i] <= s[0] && s[0] <= class[i+2] {
						hit = true
					}
					i += 2
				} else if class[i] == s[0] {
					hit = true
				}
			}
			if hit == negate {
				return false
			}
			p, s = p[end+2:], s[1:]
			continue
		default:
			if len(s) == 0 || s[0] != p[0] {
				return false
			}
		}
		p, s = p[1:], s[1:]
	}
	return len(s) == 0
}

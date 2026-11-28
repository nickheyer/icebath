package app

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/dlclark/regexp2/v2"
	"go.yaml.in/yaml/v3"

	shellhooks "github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/hooks"
	toolsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/tools/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/tools/v1/toolsv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// The tools icebath has, which a pattern of the tools of a hook or a webhook picks from: read when a page needs them, and kept so a pattern
// is said by the names of its tools
type tools struct {
	service toolsv1connect.ToolServiceClient
	mu      sync.Mutex
	list    []*toolsv1.Tool
}

// Reads the tools of the profile, then runs then
func (t *tools) load(kit *tui.Kit, profile string, then func()) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := t.service.List(kit.Env.Ctx, &toolsv1.ListRequest{Profile: profile})
		if err == nil {
			listed := slices.Clone(response.GetTools())
			slices.SortStableFunc(listed, func(a, b *toolsv1.Tool) int {
				return cmp.Or(strings.Compare(a.GetGroup(), b.GetGroup()), strings.Compare(a.GetTitle(), b.GetTitle()))
			})
			t.mu.Lock()
			t.list = listed
			t.mu.Unlock()
		}
		return func() tea.Cmd {
			if then != nil {
				then()
			}
			return nil
		}
	})
}

// Tools as last read
func (t *tools) all() []*toolsv1.Tool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.list
}

// Title of the tool of the name, the name itself for one not read
func (t *tools) title(name string) string {
	for _, tool := range t.all() {
		if tool.GetName() == name {
			return cmp.Or(tool.GetTitle(), name)
		}
	}
	return name
}

// A pattern as a person reads it: the titles of the tools it names one by one, and any other part of it as it is written
func (t *tools) words(pattern string) string {
	var said []string
	for _, part := range parts(pattern) {
		said = append(said, t.title(part))
	}
	return strs.And(said)
}

// What a row shows for a pattern held, nothing for none so the setting says what is in effect
func (t *tools) shown(ask tui.Ask) string {
	pattern := held(ask)
	if pattern == "" {
		return ""
	}
	return t.words(pattern)
}

// Pattern the picker is asked about
func held(ask tui.Ask) string {
	var pattern string
	yaml.Unmarshal([]byte(ask.Values[ask.Key]), &pattern)
	return strings.TrimSpace(pattern)
}

// Parts of a pattern joined by | outside of any group or set, each trimmed
func parts(pattern string) []string {
	var out []string
	depth, start, escaped := 0, 0, false
	for i, r := range pattern {
		switch {
		case escaped:
			escaped = false
		case r == '\\':
			escaped = true
		case r == '(' || r == '[':
			depth++
		case r == ')' || r == ']':
			depth = max(0, depth-1)
		case r == '|' && depth == 0:
			out = append(out, pattern[start:i])
			start = i + 1
		}
	}
	out = append(out, pattern[start:])
	return slices.DeleteFunc(out, func(part string) bool { return strings.TrimSpace(part) == "" })
}

// Whether the pattern matches the name of the tool as a hook matches it: the whole name
func matching(pattern, name string) bool {
	re, err := regexp2.Compile(`\A(?:`+pattern+`)\z`, regexp2.None)
	if err != nil {
		return name == pattern
	}
	ok, _ := re.MatchString(name)
	return ok
}

// The tools as choices under their groups, what each lets the agent do beside it, those the pattern matches first
func (t *tools) choices(pattern string) []tui.Choice {
	var first, rest []tui.Choice
	for _, tool := range t.all() {
		choice := tui.Choice{ID: tool.GetName(), Title: cmp.Or(tool.GetTitle(), tool.GetName()), Detail: strings.TrimSuffix(tool.GetAbout(), "."),
			Group: tool.GetGroup()}
		if pattern != "" && matching(pattern, tool.GetName()) {
			first = append(first, choice)
			continue
		}
		rest = append(rest, choice)
	}
	return append(first, rest...)
}

// Opens the tools to tick over the hub: every tool, then each tool under its group ticked while the pattern names it, enter ticking or
// unticking it and handing the pattern back at once, and last a row to type a pattern of names
func (t *tools) open(kit *tui.Kit, hub *cards.Hub, ask tui.Ask) tea.Cmd {
	theme := kit.Env.Theme
	pattern := held(ask)
	every := "Every tool"
	if len(ask.First) > 0 && ask.First[0].Title != "" {
		every = ask.First[0].Title
	}
	keep := func(next string) tea.Cmd {
		pattern = next
		value := "null"
		if next != "" {
			value = strconv.Quote(next)
		}
		return ask.Done(map[string]string{ask.Key: value})
	}
	var listed []*toolsv1.Tool
	var list *cards.List
	list = cards.NewList(kit.Env, cards.ListSpec{
		Title:   cmp.Or(ask.Title, "Tools"),
		Key:     "pick:tools:" + ask.Key,
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Bar:     true,
		Search:  "Search tools",
		Intro: func() []string {
			lines := slices.Clone(ask.Intro)
			if extra := t.unnamed(pattern); extra != "" {
				lines = append(lines, "The pattern also holds "+extra+".")
			}
			return lines
		},
		Columns: []render.Column{{}, {}, {Flex: 1}},
		Rows: func() [][]string {
			listed = t.all()
			rows := make([][]string, 0, len(listed)+2)
			rows = append(rows, []string{kit.Mark(true, pattern == ""), every, ""},
				[]string{"", "Type a pattern", render.Dim(theme.Glyphs.Forward, theme)})
			named := parts(pattern)
			for _, tool := range listed {
				box := "[ ]"
				if slices.Contains(named, tool.GetName()) {
					box = "[x]"
				}
				rows = append(rows, []string{box, cmp.Or(tool.GetTitle(), tool.GetName()), render.Dim(tool.GetName(), theme)})
			}
			return rows
		},
		Group: func(i int) string {
			if i >= 2 {
				return listed[i-2].GetGroup()
			}
			return ""
		},
		Words: func(i int) string {
			if i >= 2 {
				return listed[i-2].GetAbout()
			}
			return ""
		},
		Detail: func(i int) []string {
			switch {
			case i == 0:
				return []string{"Leaves no tool out."}
			case i == 1:
				return []string{"Tool names joined by |, or a pattern such as web_.* for every tool whose name starts so."}
			case i < 0:
				return nil
			}
			tool := listed[i-2]
			lines := []string{strs.Full(tool.GetAbout())}
			if !slices.Contains(parts(pattern), tool.GetName()) && pattern != "" && matching(pattern, tool.GetName()) {
				lines = append(lines, render.Dim("The pattern covers it.", theme))
			}
			return lines
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			switch {
			case i < 0:
				return nil
			case i == 0:
				return []wm.Hint{{Action: "hub.use", Label: "choose"}}
			case i == 1:
				return []wm.Hint{{Action: "hub.use", Label: "type"}}
			case slices.Contains(parts(pattern), listed[i-2].GetName()):
				return []wm.Hint{{Action: "hub.use", Label: "untick"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "tick"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				switch {
				case i == 0:
					return tea.Batch(hub.Close(l), keep(""))
				case i == 1:
					return t.typed(kit, hub, pattern, keep)
				}
				name := listed[i-2].GetName()
				named := parts(pattern)
				if at := slices.Index(named, name); at >= 0 {
					named = slices.Delete(named, at, at+1)
				} else {
					named = append(named, name)
				}
				next := strings.Join(named, "|")
				l.Refresh()
				return keep(next)
			},
		},
	})
	list.ToggleDetail()
	return tea.Batch(hub.Push(list), t.load(kit, ask.Profile, list.Refresh))
}

// Parts of the pattern that name no tool of the list, in words, empty for none
func (t *tools) unnamed(pattern string) string {
	var extra []string
	for _, part := range parts(pattern) {
		if !slices.ContainsFunc(t.all(), func(tool *toolsv1.Tool) bool { return tool.GetName() == part }) {
			extra = append(extra, part)
		}
	}
	return shellhooks.ToolWords(strings.Join(extra, "|"))
}

// Asks for a pattern typed, the one held in the field to change, an empty one standing for every tool
func (t *tools) typed(kit *tui.Kit, hub *cards.Hub, pattern string, keep func(next string) tea.Cmd) tea.Cmd {
	field := cards.Field{ID: "pattern", Label: "Tools", Value: pattern, Select: true, Check: func(typed string) string {
		typed = strings.TrimSpace(typed)
		if typed == "" {
			return ""
		}
		if _, err := regexp2.Compile(`\A(?:`+typed+`)\z`, regexp2.None); err != nil {
			return "This is not a pattern of tool names. Join names with |, such as terminal|write_file."
		}
		return ""
	}, Help: "Tool names joined by |, such as terminal|write_file, or a pattern such as web_.*.\nLeave empty for every tool."}
	var form *cards.Form
	form = cards.NewForm(kit.Env, cards.FormSpec{Title: "Tools", Fields: []cards.Field{field}, Submit: "Save",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			return tea.Batch(hub.Close(form), keep(strings.TrimSpace(values["pattern"])))
		}})
	return hub.Push(form)
}

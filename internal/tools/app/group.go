package app

import (
	"fmt"
	"slices"

	tea "charm.land/bubbletea/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	toolsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/tools/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Advanced settings a page shows in place, last, while they are fewer than this, else behind one row
const apart = 3

// Row of a group's page: a tool's switch, one of the group's options, the row of the advanced options of its first section, a row that
// opens another section of its options, or the row that opens the area of its options
type line struct {
	tool     *toolsv1.Tool
	setting  *configv1.Setting
	advanced string
	section  string
	area     string
}

// Opens the page of a group: how many of its tools are on as its first line, a switch for each tool, the options of its first section with
// their advanced ones, a row for each other section, and the row that opens the area of its options
func (c *catalog) enter(group *toolsv1.Group, tool string) tea.Cmd {
	t := c.kit.Env.Theme
	title := group.GetTitle()
	var lines []line
	var view *cards.List
	view = cards.NewList(c.kit.Env, cards.ListSpec{
		Title:   title,
		Glyph:   t.Glyphs.Tools,
		Key:     Screen + ":" + title,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			if now := c.group(title); now != nil {
				return c.stateLines(now)
			}
			return nil
		},
		Columns: configapp.Columns,
		Rows: func() [][]string {
			lines = c.lines(title)
			cells := make([][]string, len(lines))
			for i, l := range lines {
				cells[i] = c.lineCells(l)
			}
			return cells
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(lines) {
				return nil
			}
			detail := c.lineDetail(lines[i], view.Cut(i, 2))
			// What the tool lacks is said once on the page: in the first lines when every tool that can't run lacks the same
			if now := c.group(title); lines[i].tool != nil && len(whys(now)) == 1 && len(detail) > 1 {
				detail = detail[:1]
			}
			return detail
		},
		Empty: func() string { return "No group of tools is called " + title + " now." },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(lines) {
				return nil
			}
			switch l := lines[i]; {
			case l.tool != nil && l.tool.GetDisabled():
				return []wm.Hint{{Action: "hub.use", Label: "turn on"}}
			case l.tool != nil:
				return []wm.Hint{{Action: "hub.use", Label: "turn off"}}
			case l.setting != nil:
				return c.options.Hints(l.setting)
			}
			return []wm.Hint{{Action: "hub.use", Label: "open"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd { return c.useLine(title, lines[i]) },
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if setting := lines[i].setting; setting != nil {
					return c.options.Reset(l, setting)
				}
				return nil
			},
		},
	})
	view.ToggleDetail()
	c.options.Watch(view)
	c.views.Add(view)
	cmd := c.hub.Push(view)
	// The tool named in hand, as a search of Settings that found it opens its group
	if tool != "" {
		view.Refresh()
		for i, l := range lines {
			if l.tool.GetName() == tool {
				view.Select(i)
			}
		}
	}
	return cmd
}

// How many tools of a group are on, as a sentence, then in the warm colour one line naming the tools that are on and can't run, with what
// they lack when that is one thing; the pane of each tool says what it lacks
func (c *catalog) stateLines(group *toolsv1.Group) []string {
	t := c.kit.Env.Theme
	var said string
	switch total, on := group.GetTotal(), group.GetOn(); {
	case total == 1 && on == 1:
		said = "On, so the agent can use it."
	case total == 1:
		said = "Off, so the agent can't use it."
	case total == 2 && on == 2:
		said = "Both tools are on."
	case on == total:
		said = fmt.Sprintf("All %d tools are on.", total)
	case on == 0:
		said = "Off, so the agent can use none of these tools."
	default:
		said = fmt.Sprintf("%d of %d tools are on.", on, total)
	}
	blocked := group.GetBlocked()
	if len(blocked) == 0 {
		return []string{said}
	}
	var tools []string
	for _, b := range blocked {
		tools = append(tools, b.GetTool())
	}
	verb := " can't run."
	if lacks := whys(group); len(lacks) == 1 {
		verb += " " + strs.Full(lacks[0])
	}
	return []string{said, render.Warm(strs.AndMore(tools, 3)+verb, t)}
}

// What the tools of a group that are on and can't run lack, each once
func whys(group *toolsv1.Group) []string {
	var out []string
	for _, b := range group.GetBlocked() {
		if !slices.Contains(out, b.GetWhy()) {
			out = append(out, b.GetWhy())
		}
	}
	return out
}

// Rows of a group's page as it stands now
func (c *catalog) lines(title string) []line {
	group := c.group(title)
	if group == nil {
		return nil
	}
	var out []line
	for _, tool := range c.toolsOf(title) {
		out = append(out, line{tool: tool})
	}
	sections := group.GetSections()
	if len(sections) > 0 {
		everyday, advanced := c.options.Section(sections[0])
		for _, setting := range everyday {
			out = append(out, line{setting: setting})
		}
		if len(advanced) < apart || len(everyday) == 0 {
			for _, setting := range advanced {
				out = append(out, line{setting: setting})
			}
		}
	}
	for _, key := range sections[min(1, len(sections)):] {
		if info := c.options.Info(key); info != nil {
			out = append(out, line{section: key})
		}
	}
	if len(sections) > 0 {
		if _, _, ok := c.options.Advanced(sections[0]); ok {
			out = append(out, line{advanced: sections[0]})
		}
	}
	if group.GetArea() != "" && c.options.Info(group.GetArea()) != nil {
		out = append(out, line{area: group.GetArea()})
	}
	return out
}

// Cells of a row of a group's page
func (c *catalog) lineCells(l line) []string {
	t := c.kit.Env.Theme
	opens := render.Dim(t.Glyphs.Forward, t)
	switch {
	case l.tool != nil:
		return []string{l.tool.GetTitle(), "", toolState(l.tool, t)}
	case l.setting != nil:
		return c.options.Cells(l.setting)
	case l.section != "":
		return []string{c.options.Info(l.section).GetTitle(), opens, render.Dim(c.inside(l.section), t)}
	case l.advanced != "":
		cells, _, _ := c.options.Advanced(l.advanced)
		return cells
	}
	return []string{c.options.Info(l.area).GetTitle(), opens, ""}
}

// Lines under a group's page about the row in hand
func (c *catalog) lineDetail(l line, cut bool) []string {
	t := c.kit.Env.Theme
	switch {
	case l.tool != nil:
		lines := []string{strs.Full(l.tool.GetAbout())}
		if lacks := l.tool.GetLacks(); lacks != "" && !l.tool.GetDisabled() {
			lines = append(lines, render.Warm("It can't run now. "+lacks, t))
		}
		return lines
	case l.setting != nil:
		return c.options.Detail(l.setting, cut)
	case l.section != "":
		return []string{strs.Full(c.options.Info(l.section).GetDescription())}
	case l.advanced != "":
		return []string{"Settings that few people change."}
	}
	return []string{strs.Full(c.options.Info(l.area).GetDescription())}
}

// What enter does on a row of a group's page
func (c *catalog) useLine(title string, l line) tea.Cmd {
	switch {
	case l.tool != nil:
		return c.flip(l.tool)
	case l.setting != nil:
		return c.options.Change(l.setting)
	case l.section != "":
		return c.sectionPage(l.section)
	case l.advanced != "":
		_, open, _ := c.options.Advanced(l.advanced)
		return open()
	}
	if group := c.group(title); group != nil {
		return c.area(group)
	}
	return nil
}

// Opens a section of a group's options over its page: the settings of the section, its advanced ones in place while few, else behind a row
func (c *catalog) sectionPage(key string) tea.Cmd {
	t := c.kit.Env.Theme
	info := c.options.Info(key)
	var shown []*configv1.Setting
	var advanced bool
	var view *cards.List
	view = cards.NewList(c.kit.Env, cards.ListSpec{
		Title:   info.GetTitle(),
		Glyph:   t.Glyphs.Settings,
		Key:     Screen + ":" + key,
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search " + info.GetTitle(),
		Intro:   func() []string { return []string{strs.Full(info.GetDescription())} },
		Columns: configapp.Columns,
		Rows: func() [][]string {
			everyday, rare := c.options.Section(key)
			shown = everyday
			_, _, advanced = c.options.Advanced(key)
			if !advanced {
				shown = append(shown, rare...)
			}
			cells := make([][]string, 0, len(shown)+1)
			for _, setting := range shown {
				cells = append(cells, c.options.Cells(setting))
			}
			if advanced {
				row, _, _ := c.options.Advanced(key)
				cells = append(cells, row)
			}
			return cells
		},
		Detail: func(i int) []string {
			if i >= len(shown) {
				return []string{"Settings that few people change."}
			}
			return c.options.Detail(shown[i], view.Cut(i, 2))
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return nil
			}
			if i >= len(shown) {
				return []wm.Hint{{Action: "hub.use", Label: "open"}}
			}
			return c.options.Hints(shown[i])
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				if i >= len(shown) {
					_, open, _ := c.options.Advanced(key)
					return open()
				}
				return c.options.Change(shown[i])
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if i >= len(shown) {
					return nil
				}
				return c.options.Reset(l, shown[i])
			},
		},
	})
	view.ToggleDetail()
	c.options.Watch(view)
	return c.hub.Push(view)
}

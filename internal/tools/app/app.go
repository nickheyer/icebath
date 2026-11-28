package app

import (
	"cmp"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/profile"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	toolsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/tools/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/tools/v1/toolsv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Screen of the tools
const Screen = "tools"

// Headings of the page: the groups of tools, then the options
const (
	groupsHeading  = "Groups"
	optionsHeading = "Options"
)

type screens struct{}

func Register(host *app.App) {
	host.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := toolsv1connect.NewToolServiceClient(conn)
	settings := configv1connect.NewConfigServiceClient(conn)
	return []tui.Screen{{
		ID:    Screen,
		Group: tui.GroupAbilities,
		Order: 10,
		Title: "Tools",
		Short: "Turn the agent's tools on or off",
		Slash: []string{"tools"},
		Usage: "[@profile] [enable|disable <tool or group>...|<group>]",
		Area:  toolsconfig.Key,
		Pages: func(kit *tui.Kit, name string) []tui.Page { return toolPages(kit, service, name) },
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			name, rest := profile.Split(argument)
			words := strings.Fields(rest)
			if len(words) > 1 && (words[0] == "enable" || words[0] == "disable") {
				return turn(kit, service, name, words[0] == "enable", words[1:])
			}
			return (&catalog{kit: kit, service: service, settings: settings, profile: name}).open(strings.TrimSpace(rest))
		},
	}}
}

// Turns tools on or off as the switches of the page do, saying what the switch says: /tools enable and /tools disable are shortcuts to it
func turn(kit *tui.Kit, service toolsv1connect.ToolServiceClient, name string, on bool, words []string) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := service.Turn(kit.Env.Ctx, &toolsv1.TurnRequest{Profile: name, Words: words, On: on})
		return func() tea.Cmd {
			switch {
			case err != nil:
				return kit.WM.Toast("The tools were not changed. "+tui.Reason(err), wm.Error, wm.DefaultToastTTL)
			case len(response.GetTurned()) == 0:
				// Nothing was turned: the words named no tool, or tools turned on and off on the page of their server
				return kit.WM.Toast(response.GetSaid(), wm.Warn, wm.DefaultToastTTL)
			}
			return kit.WM.Toast(response.GetSaid(), wm.Success, wm.DefaultToastTTL)
		}
	})
}

// The page of the tools on one profile: the groups of tools with their states, a search through every tool, the page of each group, and
// the options of the area
type catalog struct {
	kit      *tui.Kit
	service  toolsv1connect.ToolServiceClient
	settings configv1connect.ConfigServiceClient
	profile  string
	hub      *cards.Hub
	list     *cards.List
	options  *configapp.Rows
	groups   []*toolsv1.Group
	tools    []*toolsv1.Tool
	loaded   bool
	err      error
	rows     []row
	// Lists of the pages of groups opened over the page, read again with it
	views cards.Watched
}

// Row of the page: a group, a tool a search found, an option or the row of the advanced options
type row struct {
	group    *toolsv1.Group
	tool     *toolsv1.Tool
	setting  *configv1.Setting
	advanced bool
}

func (c *catalog) open(argument string) tea.Cmd {
	t := c.kit.Env.Theme
	c.list = cards.NewList(c.kit.Env, cards.ListSpec{
		Title:   "Tools",
		Glyph:   t.Glyphs.Tools,
		Key:     Screen,
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search tools",
		Intro:   func() []string { return []string{"Turn tools on or off and limit what they return."} },
		Columns: configapp.Columns,
		Rows: func() [][]string {
			c.rows = c.order(c.list.Query() != "")
			cells := make([][]string, len(c.rows))
			for i, r := range c.rows {
				cells[i] = c.cells(r, c.list.Query() != "")
			}
			return cells
		},
		Group: func(i int) string { return c.heading(c.rows[i]) },
		Words: func(i int) string { return c.words(c.rows[i]) },
		Detail: func(i int) []string {
			return c.detail(c.rows[i], c.list.Cut(i, 2))
		},
		Empty: func() string {
			if c.list.Query() != "" {
				return "No tool matches “" + c.list.Query() + "”."
			}
			return tui.Waiting(c.loaded, c.err, "No tools are installed.")
		},
		Free:  map[string]bool{"hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint { return c.hints(i) },
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd { return c.use(c.rows[i]) },
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if setting := c.rows[i].setting; setting != nil {
					return c.options.Reset(l, setting)
				}
				return nil
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return tea.Batch(c.load(), c.options.Load(nil)) },
		},
	})
	c.list.ToggleDetail()
	hub, cmd := c.kit.Show(Screen, c.list)
	c.hub = hub
	c.options = configapp.NewRows(c.kit, hub, c.settings, c.profile)
	c.options.Watch(c.list)
	return tea.Batch(cmd, c.options.Load(nil), c.read(func() tea.Cmd {
		if argument == "" {
			return nil
		}
		// One tool opens the page of its group with the tool in hand
		if name, ok := strings.CutPrefix(argument, ToolOf); ok {
			for _, tool := range c.tools {
				if group := c.group(tool.GetGroup()); tool.GetName() == name && group != nil {
					return c.enter(group, name)
				}
			}
			return c.hub.Fail("No tool is called " + name + ".")
		}
		for _, group := range c.groups {
			if strings.EqualFold(group.GetTitle(), argument) {
				return c.enter(group, "")
			}
		}
		return c.hub.Fail("No group of tools is called " + argument + ".")
	}))
}

func (c *catalog) load() tea.Cmd { return c.read(nil) }

// Reads the tools and their groups again, then runs then
func (c *catalog) read(then func() tea.Cmd) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := c.service.List(c.kit.Env.Ctx, &toolsv1.ListRequest{Profile: c.profile})
		return func() tea.Cmd {
			c.err, c.loaded = err, true
			if err == nil {
				c.groups, c.tools = response.GetGroups(), response.GetTools()
			}
			c.list.Refresh()
			c.views.Refresh()
			if then != nil {
				return then()
			}
			return nil
		}
	})
}

// Rows of the page: the groups, then the options and the row of the advanced ones; while searched, every tool that can be turned on or off
// here, each with its switch
func (c *catalog) order(searching bool) []row {
	if !c.loaded || c.err != nil {
		return nil
	}
	var rows []row
	if searching {
		for _, tool := range c.tools {
			if group := c.group(tool.GetGroup()); group != nil && !group.GetElsewhere() {
				rows = append(rows, row{tool: tool})
			}
		}
		return rows
	}
	for _, group := range c.groups {
		rows = append(rows, row{group: group})
	}
	everyday, _ := c.options.Section(toolsconfig.Key)
	for _, setting := range everyday {
		rows = append(rows, row{setting: setting})
	}
	if _, _, ok := c.options.Advanced(toolsconfig.Key); ok {
		rows = append(rows, row{advanced: true})
	}
	return rows
}

// Group of the title as last read, nil for none
func (c *catalog) group(title string) *toolsv1.Group {
	for _, group := range c.groups {
		if group.GetTitle() == title {
			return group
		}
	}
	return nil
}

// Tools of a group, by the names a person reads
func (c *catalog) toolsOf(title string) []*toolsv1.Tool {
	var out []*toolsv1.Tool
	for _, tool := range c.tools {
		if tool.GetGroup() == title {
			out = append(out, tool)
		}
	}
	return out
}

func (c *catalog) heading(r row) string {
	switch {
	case r.group != nil:
		return groupsHeading
	case r.tool != nil:
		return ""
	}
	return optionsHeading
}

// Cells of a row: a group with its state, a tool found with its group before it and its switch, an option, or the advanced row
func (c *catalog) cells(r row, searching bool) []string {
	t := c.kit.Env.Theme
	switch {
	case r.group != nil:
		return []string{r.group.GetTitle(), "", c.state(r.group)}
	case r.tool != nil:
		sep := " " + t.Glyphs.Forward + " "
		return []string{render.Dim(r.tool.GetGroup()+sep, t) + r.tool.GetTitle(), "", toolState(r.tool, t)}
	case r.setting != nil:
		return c.options.Cells(r.setting)
	}
	cells, _, _ := c.options.Advanced(toolsconfig.Key)
	return cells
}

// State of a group in the words of the one function that says it: On, Off or how many are on, and how many of those can't run; for a
// group whose tools are turned on and off elsewhere, how many tools it has
func (c *catalog) state(group *toolsv1.Group) string {
	t := c.kit.Env.Theme
	if group.GetElsewhere() {
		return render.Count(int(group.GetTotal()), "tool")
	}
	said := group.GetState()
	if note := group.GetNote(); note != "" {
		said += render.Warm(" · "+note, t)
	}
	return said
}

// Switch of a tool: On or Off, and that it can't run when it is on and lacks something
func toolState(tool *toolsv1.Tool, t theme.Theme) string {
	switch {
	case tool.GetDisabled():
		return "Off"
	case tool.GetLacks() != "":
		return "On" + render.Warm(" · can't run", t)
	}
	return "On"
}

// Words a search reads for a row besides its cells: what a tool lets the agent do and its name, what a group holds
func (c *catalog) words(r row) string {
	switch {
	case r.tool != nil:
		return r.tool.GetAbout() + " " + r.tool.GetName() + " " + r.tool.GetToolset()
	case r.group != nil:
		var titles []string
		for _, tool := range c.toolsOf(r.group.GetTitle()) {
			titles = append(titles, tool.GetTitle())
		}
		return strings.Join(titles, " ")
	}
	return ""
}

// Lines under the list about the row in hand, never what the row or the first line says
func (c *catalog) detail(r row, cut bool) []string {
	t := c.kit.Env.Theme
	switch {
	case r.group != nil:
		var titles []string
		for _, tool := range c.toolsOf(r.group.GetTitle()) {
			titles = append(titles, tool.GetTitle())
		}
		lines := []string{strs.And(titles) + "."}
		if r.group.GetElsewhere() {
			lines[0] = "Its tools are turned on and off with the server."
		}
		for _, blocked := range r.group.GetBlocked() {
			lines = append(lines, render.Warm(blocked.GetTool()+" can't run. "+blocked.GetWhy(), t))
		}
		return lines
	case r.tool != nil:
		lines := []string{strs.Full(r.tool.GetAbout())}
		if lacks := r.tool.GetLacks(); lacks != "" && !r.tool.GetDisabled() {
			lines = append(lines, render.Warm("It can't run now. "+lacks, t))
		}
		return lines
	case r.setting != nil:
		return c.options.Detail(r.setting, cut)
	}
	return []string{"Settings that few people change."}
}

// Buttons for the row in hand
func (c *catalog) hints(i int) []wm.Hint {
	if i < 0 || i >= len(c.rows) {
		return nil
	}
	switch r := c.rows[i]; {
	case r.group != nil:
		return []wm.Hint{{Action: "hub.use", Label: "open"}}
	case r.tool != nil:
		if r.tool.GetDisabled() {
			return []wm.Hint{{Action: "hub.use", Label: "turn on"}}
		}
		return []wm.Hint{{Action: "hub.use", Label: "turn off"}}
	case r.setting != nil:
		return c.options.Hints(r.setting)
	}
	return []wm.Hint{{Action: "hub.use", Label: "open"}}
}

// What enter does on a row: opens a group, or the area of a group whose tools are turned on and off there; flips a tool; changes an option;
// opens the advanced ones
func (c *catalog) use(r row) tea.Cmd {
	switch {
	case r.group != nil && r.group.GetElsewhere():
		return c.area(r.group)
	case r.group != nil:
		return c.enter(r.group, "")
	case r.tool != nil:
		return c.flip(r.tool)
	case r.setting != nil:
		return c.options.Change(r.setting)
	}
	_, open, _ := c.options.Advanced(toolsconfig.Key)
	return open()
}

// Turns a tool on or off through the one function that does it, saying what the switch says
func (c *catalog) flip(tool *toolsv1.Tool) tea.Cmd {
	return tui.Call(c.hub, func() (*toolsv1.TurnResponse, error) {
		return c.service.Turn(c.kit.Env.Ctx, &toolsv1.TurnRequest{Profile: c.profile, Words: []string{tool.GetName()}, On: tool.GetDisabled()})
	}, func(r *toolsv1.TurnResponse) tea.Cmd {
		return tea.Batch(c.hub.Done(r.GetSaid()), c.load(), c.options.Load(nil))
	})
}

// Opens the area a group's options are: the page of that area once one exists, else the settings of its section
func (c *catalog) area(group *toolsv1.Group) tea.Cmd {
	for _, screen := range c.kit.Screens() {
		if screen.Area == group.GetArea() {
			return c.kit.Open(screen.ID, profile.Join(c.profile, group.GetOpens()))
		}
	}
	return c.kit.Open(configapp.Settings, profile.Join(c.profile, group.GetArea()))
}

// How many settings a row that opens a section holds, and how many were changed
func (c *catalog) inside(key string) string {
	everyday, advanced := c.options.Section(key)
	changed := 0
	for _, setting := range append(everyday, advanced...) {
		if setting.GetSource() == configv1.Source_SOURCE_FILE {
			changed++
		}
	}
	said := render.Count(len(everyday)+len(advanced), "setting")
	if changed > 0 {
		said += ", " + strconv.Itoa(changed) + " changed"
	}
	return said
}

// Start of the argument of the tools screen that opens the page of a tool's group with the tool in hand, such as tool:web_search
const ToolOf = "tool:"

// Pages inside Tools that a search of Settings finds: the page of each group, and each tool as the page of its group with the tool in hand.
// A group whose page is another area's is found there
func toolPages(kit *tui.Kit, service toolsv1connect.ToolServiceClient, name string) []tui.Page {
	response, err := service.List(kit.Env.Ctx, &toolsv1.ListRequest{Profile: name})
	if err != nil {
		return nil
	}
	here := map[string]bool{}
	var pages []tui.Page
	for _, group := range response.GetGroups() {
		if group.GetElsewhere() {
			continue
		}
		here[group.GetTitle()] = true
		var keys, words []string
		for _, section := range group.GetSections() {
			keys = append(keys, section+".")
		}
		for _, tool := range response.GetTools() {
			if tool.GetGroup() == group.GetTitle() {
				words = append(words, tool.GetName(), tool.GetTitle())
			}
		}
		pages = append(pages, tui.Page{Title: group.GetTitle(), About: group.GetNote(), Words: strings.Join(words, " "), Argument: group.GetTitle(),
			Keys: keys})
	}
	for _, tool := range response.GetTools() {
		if here[tool.GetGroup()] {
			pages = append(pages, tui.Page{Title: cmp.Or(tool.GetTitle(), tool.GetName()), Trail: []string{tool.GetGroup()}, About: tool.GetAbout(),
				Words: tool.GetName() + " " + tool.GetDescription(), Argument: ToolOf + tool.GetName()})
		}
	}
	return pages
}

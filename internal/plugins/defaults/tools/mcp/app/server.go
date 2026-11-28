package app

import (
	"encoding/json"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	mcpconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	mcpv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/mcp/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Ways a server connects, by the choice that picks them
const (
	byCommand = "command"
	byAddress = "address"
	bySignin  = "signin"
)

// The ways a server connects as a list to pick from
var ways = []tui.Choice{
	{ID: byCommand, Title: "A command on this computer", Detail: "It runs here and talks over its input and output"},
	{ID: byAddress, Title: "An address", Detail: "It is reached over the web"},
	{ID: bySignin, Title: "An address with sign-in", Detail: "It is reached over the web, and you sign in to it in your browser"},
}

// How a server connects
func way(server *mcpv1.Server) string {
	switch {
	case server.GetUrl() == "":
		return byCommand
	case server.GetOauth():
		return bySignin
	}
	return byAddress
}

// Title of a way a server connects
func wayTitle(id string) string {
	for _, choice := range ways {
		if choice.ID == id {
			return choice.Title
		}
	}
	return ""
}

// Row of a server's page: something done to it, a row that opens a view of its own, the way it connects, or one of its options
type entry struct {
	act      *step
	tools    bool
	values   bool
	connects bool
	advanced bool
	setting  *configv1.Setting
}

// Key of the section of a server's settings
func sectionOf(name string) string { return mcpconfig.Servers + "." + name }

// Opens the page of a server: its state as the first line, then turning it off or on, signing in, a test, its tools and values, how it
// connects and the fields of that way, its trust, whether it starts on first use, its advanced options and removing it
func (s *servers) page(name string) tea.Cmd {
	t := s.kit.Env.Theme
	var lines []entry
	var view *cards.List
	view = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   name,
		Glyph:   t.Glyphs.MCP,
		Key:     Screen + ":" + name,
		Context: cards.HubContext,
		Shared:  []string{serversContext},
		Primary: "hub.use",
		Free:    map[string]bool{"mcp.reload": true},
		Intro: func() []string {
			if server := s.find(name); server != nil {
				return s.state(server)
			}
			return nil
		},
		Columns: configapp.Columns,
		Rows: func() [][]string {
			lines = s.lines(name)
			cells := make([][]string, len(lines))
			for i, e := range lines {
				cells[i] = s.entryCells(name, e)
			}
			return cells
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(lines) {
				return nil
			}
			return s.entryDetail(lines[i], view.Cut(i, 2))
		},
		Empty: func() string { return "No MCP server is called " + name + " now." },
		// Back in view, it reads its server again and goes on following it
		OnFocus: func(*cards.List) tea.Cmd { return tea.Batch(s.load(""), s.watch()) },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(lines) {
				return nil
			}
			switch e := lines[i]; {
			case e.setting != nil:
				return s.options.Hints(e.setting)
			case e.act != nil:
				return []wm.Hint{{Action: "hub.use", Label: e.act.verb}}
			case e.connects:
				return []wm.Hint{{Action: "hub.use", Label: "change"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "open"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd { return s.useEntry(l, name, lines[i]) },
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if lines[i].setting != nil {
					return s.options.Reset(l, lines[i].setting)
				}
				return nil
			},
			"mcp.reload": func(l *cards.List, _ int) tea.Cmd { return s.reload(l) },
		},
	})
	view.ToggleDetail()
	s.options.Watch(view)
	s.views.Add(view)
	return s.hub.Push(view)
}

// The state of a server as a sentence, then in the warm colour what went wrong, and the plugin it comes with
func (s *servers) state(server *mcpv1.Server) []string {
	t := s.kit.Env.Theme
	var said string
	switch server.GetShown() {
	case "Connected":
		said = "Connected."
		if n := server.GetTools(); n > 0 {
			said = "Connected, with " + render.Count(int(n), "tool") + "."
		}
	case "Not connected":
		said = "Not connected yet. It connects when a session needs its tools."
	case "Connecting":
		said = "Connecting."
	case "Failed":
		said = "Couldn't connect."
	case "Off":
		said = "Off, so new sessions don't load it."
	case "Needs values":
		said = "Needs values that are not set: " + strings.Join(server.GetMissing(), ", ") + "."
	case "Not signed in":
		said = "Not signed in."
	case "Starts on first use":
		said = "Starts the first time the agent uses one of its tools."
	case "Resting until used":
		said = "Resting. It starts again when the agent uses one of its tools."
	default:
		said = strs.Full(server.GetShown())
	}
	lines := []string{said}
	if e := server.GetError(); e != "" {
		lines = append(lines, render.Warm(strs.Full(e), t))
	}
	if plugin := server.GetPlugin(); plugin != "" {
		lines = append(lines, render.Dim("It comes with the plugin "+plugin+".", t))
	}
	return lines
}

// Rows of a server's page as it stands now
func (s *servers) lines(name string) []entry {
	server := s.find(name)
	if server == nil {
		return nil
	}
	toggle := &step{"Turn off", "New sessions stop loading it.", "turn off", func(*cards.List) tea.Cmd { return s.toggle(server) }}
	if !server.GetEnabled() {
		toggle = &step{"Turn on", "New sessions load it.", "turn on", func(*cards.List) tea.Cmd { return s.toggle(server) }}
	}
	out := []entry{{act: toggle}}
	if server.GetOauth() {
		signin := &step{"Sign in", "Signs in with the browser on this computer.", "sign in", func(*cards.List) tea.Cmd { return s.signin(server, "") }}
		if server.GetSignedIn() {
			signin.title = "Sign in again"
		}
		out = append(out, entry{act: signin}, entry{act: &step{"Sign in with a code", "Signs in on another device with a code, for a computer " +
			"without a browser.", "sign in", func(*cards.List) tea.Cmd { return s.signin(server, "device") }}})
	}
	out = append(out, entry{act: &step{"Test", "Connects to it once and counts its tools.", "test", func(*cards.List) tea.Cmd { return s.test(server) }}},
		entry{tools: true})
	if len(server.GetValues()) > 0 {
		out = append(out, entry{values: true})
	}
	out = append(out, entry{connects: true})
	everyday, _ := s.options.Section(sectionOf(name))
	for _, setting := range everyday {
		out = append(out, entry{setting: setting})
	}
	out = append(out, entry{advanced: true})
	if server.GetPlugin() == "" {
		out = append(out, entry{act: &step{"Remove", "Removes it, with its sign-in and any files it downloaded.", "remove",
			func(l *cards.List) tea.Cmd { return s.remove(l, server) }}})
	}
	return out
}

// Cells of a row of a server's page
func (s *servers) entryCells(name string, e entry) []string {
	t := s.kit.Env.Theme
	opens := render.Dim(t.Glyphs.Forward, t)
	server := s.find(name)
	switch {
	case e.setting != nil:
		return s.options.Cells(e.setting)
	case e.act != nil:
		return []string{e.act.title, "", ""}
	case e.tools:
		return []string{"Tools", opens, render.Dim(toolsWords(server), t)}
	case e.values:
		return []string{"Values", opens, render.Dim(valuesWords(server), t)}
	case e.connects:
		return []string{"How it connects", "", wayTitle(way(server))}
	}
	return []string{"Advanced", opens, render.Dim(s.advancedWords(server), t)}
}

// Which tools of a server the agent may use, in a few words
func toolsWords(server *mcpv1.Server) string {
	switch {
	case server.GetIncluded():
		return "Only " + render.Count(len(server.GetInclude()), "tool")
	case len(server.GetExclude()) > 0:
		return "All but " + render.Count(len(server.GetExclude()), "tool")
	}
	return "All"
}

// The values a server reads, in a few words
func valuesWords(server *mcpv1.Server) string {
	said := render.Count(len(server.GetValues()), "value")
	if missing := len(server.GetMissing()); missing > 0 {
		said += ", " + strconv.Itoa(missing) + " not set"
	}
	return said
}

// What the advanced row of a server opens, in a few words: how many settings, and how many were changed
func (s *servers) advancedWords(server *mcpv1.Server) string {
	settings := s.advancedSettings(server)
	changed := 0
	for _, setting := range settings {
		if setting.GetSource() == configv1.Source_SOURCE_FILE {
			changed++
		}
	}
	said := render.Count(len(settings), "setting")
	if changed > 0 {
		said += ", " + strconv.Itoa(changed) + " changed"
	}
	return said
}

// Every setting behind a server's advanced row: its own advanced ones and those of each group of its fields
func (s *servers) advancedSettings(server *mcpv1.Server) []*configv1.Setting {
	_, rare := s.options.Section(sectionOf(server.GetName()))
	for _, part := range mcpconfig.Parts(server.GetUrl() != "") {
		everyday, advanced := s.options.Section(sectionOf(server.GetName()) + "." + part.Key)
		rare = append(append(rare, everyday...), advanced...)
	}
	return rare
}

// Lines under a server's page about the row in hand
func (s *servers) entryDetail(e entry, cut bool) []string {
	switch {
	case e.setting != nil:
		return s.options.Detail(e.setting, cut)
	case e.act != nil:
		return []string{e.act.about}
	case e.tools:
		return []string{"Which of its tools the agent may use in new sessions."}
	case e.values:
		return []string{"API keys and other values its settings read as ${NAME}."}
	case e.connects:
		return []string{"A command on this computer, an address, or an address with sign-in."}
	}
	return []string{"Time limits, costs, and what the server may ask of the agent's model and of you."}
}

// What enter does on a row of a server's page
func (s *servers) useEntry(l *cards.List, name string, e entry) tea.Cmd {
	server := s.find(name)
	if server == nil {
		return nil
	}
	switch {
	case e.setting != nil:
		return s.options.Change(e.setting)
	case e.act != nil:
		return e.act.run(l)
	case e.tools:
		return s.tools(server)
	case e.values:
		return s.values(server)
	case e.connects:
		return s.kit.Pick(s.hub, "How it connects", "choose", ways, way(server), func(choice tui.Choice) tea.Cmd {
			return s.reconnect(server, choice.ID)
		})
	}
	return s.advanced(server)
}

// Settings of a server as kept, to change and save whole
func settingsOf(server *mcpv1.Server) map[string]any {
	settings := map[string]any{}
	json.Unmarshal([]byte(server.GetSettings()), &settings)
	return settings
}

// Fields of each way a server connects, dropped when it connects another way
var (
	localFields  = []string{"command", "args", "env", "cwd"}
	remoteFields = []string{"url", "headers", "transport", "auth", "oauth"}
)

// Changes how a server connects: between an address and one with sign-in at once, to another way after asking what that way needs
func (s *servers) reconnect(server *mcpv1.Server, to string) tea.Cmd {
	from, name := way(server), server.GetName()
	settings := settingsOf(server)
	switch {
	case to == from:
		return nil
	case to != byCommand && from != byCommand:
		if to == bySignin {
			settings["auth"] = "oauth"
			return s.keep(name, settings, name+" now asks you to sign in.", nil)
		}
		delete(settings, "auth")
		delete(settings, "oauth")
		return s.keep(name, settings, name+" is now reached with no sign-in.", nil)
	}
	f := s.kit.Flow(s.hub, "Change how it connects")
	if to == byCommand {
		return f.Ask("What runs the server on this computer.", commandFields(), "Save", func(values map[string]string) tea.Cmd {
			for _, field := range remoteFields {
				delete(settings, field)
			}
			settings["command"] = strings.TrimSpace(values["command"])
			if args := strings.TrimSpace(values["args"]); args != "" {
				settings["args"] = args
			}
			return s.keep(name, settings, name+" now runs on this computer.", f)
		})
	}
	return f.Ask("", []cards.Field{addressField()}, "Save", func(values map[string]string) tea.Cmd {
		for _, field := range localFields {
			delete(settings, field)
		}
		settings["url"] = strings.TrimSpace(values["url"])
		told := name + " is now reached at " + strings.TrimSpace(values["url"]) + "."
		if to == bySignin {
			settings["auth"] = "oauth"
			told = name + " is now reached at " + strings.TrimSpace(values["url"]) + ", and asks you to sign in."
		}
		return s.keep(name, settings, told, f)
	})
}

// Fields that say what runs a server on this computer: its command and the words after it
func commandFields() []cards.Field {
	return []cards.Field{
		{ID: "command", Label: "Command", Help: "The program that starts the server, such as npx or uvx.", Check: func(typed string) string {
			if strings.TrimSpace(typed) == "" {
				return "Give the program that starts the server."
			}
			return ""
		}},
		{ID: "args", Label: "Arguments", Help: "The words after it, split as a shell splits a line, such as -y @modelcontextprotocol/server-github.",
			Check: func(typed string) string {
				if _, err := strs.Shell(typed); err != nil {
					return err.Error()
				}
				return ""
			}},
	}
}

// Field of the address a server is reached at
func addressField() cards.Field {
	return cards.Field{ID: "url", Label: "Address", Help: "The address of the server, such as https://example.com/mcp.", Check: func(typed string) string {
		typed = strings.ToLower(strings.TrimSpace(typed))
		if !strings.HasPrefix(typed, "https://") && !strings.HasPrefix(typed, "http://") {
			return "An address starts with https:// or http://."
		}
		return ""
	}}
}

// Saves a server's settings whole, through the one function that checks its name and its shape; the flow it came from ends once they are
// kept
func (s *servers) keep(name string, settings map[string]any, told string, f *tui.Flow) tea.Cmd {
	data, err := json.Marshal(settings)
	if err != nil {
		return s.hub.Fail(err.Error())
	}
	s.hub.Busy("Saving…")
	return tui.Call(s.hub, func() (*mcpv1.SaveResponse, error) {
		return s.service.Save(s.kit.Env.Ctx, &mcpv1.SaveRequest{Profile: s.profile, Name: name, Settings: string(data)})
	}, func(saved *mcpv1.SaveResponse) tea.Cmd {
		if issues := saved.GetIssues(); len(issues) > 0 {
			return s.hub.Fail(name + " was not saved. " + sentences(issues))
		}
		cmds := []tea.Cmd{s.hub.Done(told), s.load(""), s.options.Load(nil)}
		if f != nil {
			cmds = append(cmds, f.End())
		}
		return tea.Batch(cmds...)
	})
}

// Opens the advanced options of a server: its own, then a row for each group of its fields that opens a page of them
func (s *servers) advanced(server *mcpv1.Server) tea.Cmd {
	name := server.GetName()
	parts := mcpconfig.Parts(server.GetUrl() != "")
	rare := func() []*configv1.Setting {
		_, advanced := s.options.Section(sectionOf(name))
		return advanced
	}
	return s.optionList("Advanced", Screen+":"+name+":advanced", "Settings of "+name+" that few people change.", rare, parts, sectionOf(name))
}

// List of option rows over the hub, each drawn, explained and changed as Settings does, then a row for each group of fields that opens a
// list of them
func (s *servers) optionList(title, key, about string, settings func() []*configv1.Setting, parts []mcpconfig.Part, prefix string) tea.Cmd {
	t := s.kit.Env.Theme
	// A row is an option, or the group of fields at that place past the options
	var shown []*configv1.Setting
	var view *cards.List
	part := func(i int) (mcpconfig.Part, bool) {
		if i < len(shown) || i-len(shown) >= len(parts) {
			return mcpconfig.Part{}, false
		}
		return parts[i-len(shown)], true
	}
	view = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   title,
		Glyph:   t.Glyphs.Settings,
		Key:     key,
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search " + strings.ToLower(title),
		Intro:   func() []string { return []string{about} },
		Columns: configapp.Columns,
		Rows: func() [][]string {
			shown = settings()
			cells := make([][]string, 0, len(shown)+len(parts))
			for _, setting := range shown {
				cells = append(cells, s.options.Cells(setting))
			}
			for _, p := range parts {
				everyday, advanced := s.options.Section(prefix + "." + p.Key)
				cells = append(cells, []string{p.Title, render.Dim(t.Glyphs.Forward, t), render.Dim(render.Count(len(everyday)+len(advanced), "setting"), t)})
			}
			return cells
		},
		Detail: func(i int) []string {
			if p, ok := part(i); ok {
				return []string{strs.Full(p.About)}
			}
			if i < 0 || i >= len(shown) {
				return nil
			}
			return s.options.Detail(shown[i], view.Cut(i, 2))
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if _, ok := part(i); ok {
				return []wm.Hint{{Action: "hub.use", Label: "open"}}
			}
			if i < 0 || i >= len(shown) {
				return nil
			}
			return s.options.Hints(shown[i])
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				if p, ok := part(i); ok {
					group := prefix + "." + p.Key
					fields := func() []*configv1.Setting {
						everyday, advanced := s.options.Section(group)
						return append(everyday, advanced...)
					}
					return s.optionList(p.Title, key+":"+p.Key, strs.Full(p.About), fields, nil, group)
				}
				return s.options.Change(shown[i])
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if i < 0 || i >= len(shown) {
					return nil
				}
				return s.options.Reset(l, shown[i])
			},
		},
	})
	view.ToggleDetail()
	s.options.Watch(view)
	return s.hub.Push(view)
}

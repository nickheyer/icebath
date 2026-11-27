package app

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	mcpconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/config"
	"github.com/nickheyer/icebath/internal/profile"
	mcpv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/mcp/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/mcp/v1/mcpv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// What MCP is, for the person the title alone leaves lost
const whatItIs = "MCP servers give the agent more tools, such as access to GitHub or a database."

// Words the states of a server are shown by
var stateWords = map[string]string{"connected": "Connected", "configured": "Not connected yet", "connecting": "Connecting", "failed": "Failed",
	"lazy": "Starts on first use", "disabled": "Off", "recycled": "Resting until used"}

// Headings of the servers screen
const (
	ownGroup    = "Your servers"
	pluginGroup = "From plugins"
	moreGroup   = "More"
)

// MCP servers screen: every server of the profile with its state, added from the catalog or by hand, turned on or off, tested, signed in
// to, its tools picked, its values set and removed, and every server connected again, all inside the screen
type servers struct {
	kit     *tui.Kit
	service mcpv1connect.McpServiceClient
	profile string
	hub     *cards.Hub
	list    *cards.List
	rows    []*mcpv1.Server
	loaded  bool
	failed  error
	// Rows as last listed: the servers, then what the screen does to all of them
	shown []row
}

// Row of the servers screen: a server, or something done to every server
type row struct {
	server *mcpv1.Server
	act    *step
}

// Something a row does, its name, what it does and the word on its button
type step struct {
	title, about, verb string
	run                func(l *cards.List) tea.Cmd
}

func (s *servers) open(argument string) tea.Cmd {
	t := s.kit.Env.Theme
	s.list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title: title(s.profile),
		Glyph: t.Glyphs.MCP,
		Key:   Screen,
		// The keys of the screen stay in force on every row, those the footer has no room for too
		Shared:  []string{serversContext, cards.HubContext},
		Primary: "hub.use",
		Intro:   func() []string { return []string{whatItIs} },
		Columns: []render.Column{{}, {}, {Right: true}, {Flex: 1}},
		Yield:   []int{3, 2},
		Rows: func() [][]string {
			s.shown = s.order()
			cells := make([][]string, len(s.shown))
			for i, r := range s.shown {
				cells[i] = s.cells(r)
			}
			return cells
		},
		Group: func(i int) string { return s.group(s.shown[i]) },
		Detail: func(i int) []string {
			if i < 0 || i >= len(s.shown) {
				return nil
			}
			if r := s.shown[i]; r.server != nil {
				return s.detail(r.server)
			}
			return []string{s.shown[i].act.about + "."}
		},
		Empty: func() string {
			return tui.Waiting(s.loaded, s.failed, "No MCP servers yet. "+s.kit.Env.Keys.Display("hub.add")+" adds one.")
		},
		Free:  map[string]bool{"hub.add": true, "hub.refresh": true, "mcp.reload": true},
		Hints: func(_ *cards.List, i int) []wm.Hint { return s.hints(i) },
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				if r := s.shown[i]; r.server != nil {
					return s.page(r.server)
				}
				return s.shown[i].act.run(l)
			},
			"mcp.toggle":  s.with(func(_ *cards.List, server *mcpv1.Server) tea.Cmd { return s.toggle(server) }),
			"mcp.test":    s.with(func(_ *cards.List, server *mcpv1.Server) tea.Cmd { return s.test(server) }),
			"mcp.signin":  s.with(func(_ *cards.List, server *mcpv1.Server) tea.Cmd { return s.signin(server, "") }),
			"hub.edit":    s.with(func(_ *cards.List, server *mcpv1.Server) tea.Cmd { return s.tools(server) }),
			"hub.remove":  s.with(func(l *cards.List, server *mcpv1.Server) tea.Cmd { return s.remove(l, server, false) }),
			"hub.add":     func(*cards.List, int) tea.Cmd { return s.add() },
			"hub.refresh": func(*cards.List, int) tea.Cmd { return s.load("") },
			"mcp.reload":  func(l *cards.List, _ int) tea.Cmd { return s.reload(l) },
		},
	})
	s.list.ToggleDetail()
	s.hub = s.kit.Hub(Screen, s.list)
	cmds := []tea.Cmd{s.kit.WM.ShowOverlay(s.hub)}
	switch argument = strings.TrimSpace(argument); argument {
	case "":
		cmds = append(cmds, s.load(""))
	case "add":
		cmds = append(cmds, s.load(""), s.add())
	default:
		cmds = append(cmds, s.load(argument))
	}
	return tea.Batch(cmds...)
}

// Title of the screen, naming the profile when it was opened on one
func title(profile string) string {
	if profile == "" {
		return "MCP servers"
	}
	return "MCP servers · " + profile
}

// Rows once the servers are read: those the profile set up, those of plugins, then what is done to every server while there are any
func (s *servers) order() []row {
	var rows []row
	for _, plugin := range []bool{false, true} {
		for _, server := range s.rows {
			if (server.GetPlugin() != "") == plugin {
				rows = append(rows, row{server: server})
			}
		}
	}
	if len(s.rows) == 0 {
		return rows
	}
	rows = append(rows, row{act: &step{"Connect again", "Connect every server again, so open sessions get your changes", "connect", s.reload}})
	if names := s.oauth(); len(names) > 1 {
		rows = append(rows, row{act: &step{"Sign in to every server", "Sign in again to each server that uses a sign-in, one after another", "sign in",
			func(*cards.List) tea.Cmd { return s.signinAll(names) }}})
	}
	return rows
}

// Servers that sign in through a page of their own
func (s *servers) oauth() []string {
	var names []string
	for _, server := range s.rows {
		if server.GetOauth() && server.GetEnabled() {
			names = append(names, server.GetName())
		}
	}
	return names
}

// Heading of a row: the servers set up here and those of plugins apart once there are both, then what is done to all of them
func (s *servers) group(r row) string {
	switch {
	case r.act != nil:
		return moreGroup
	case !slices.ContainsFunc(s.rows, func(server *mcpv1.Server) bool { return server.GetPlugin() != "" }):
		return ""
	case r.server.GetPlugin() != "":
		return pluginGroup
	}
	return ownGroup
}

// Buttons for the row under the cursor: open, turn on or off, sign in while it is not, test and add on a server; what the row does on any
// other. The tools and remove keys work on a server row besides
func (s *servers) hints(i int) []wm.Hint {
	add := wm.Hint{Action: "hub.add", Label: "add"}
	if i < 0 || i >= len(s.shown) {
		return []wm.Hint{add}
	}
	server := s.shown[i].server
	if server == nil {
		return []wm.Hint{{Action: "hub.use", Label: s.shown[i].act.verb}, add}
	}
	toggle := "turn on"
	if server.GetEnabled() {
		toggle = "turn off"
	}
	hints := []wm.Hint{{Action: "hub.use", Label: "open"}, {Action: "mcp.toggle", Label: toggle}}
	if server.GetOauth() && !server.GetSignedIn() {
		hints = append(hints, wm.Hint{Action: "mcp.signin", Label: "sign in"})
	}
	hints = append(hints, wm.Hint{Action: "mcp.test", Label: "test"})
	if len(hints) < 4 {
		hints = append(hints, wm.Hint{Action: "hub.edit", Label: "tools"})
	}
	return append(hints, add)
}

// Action on the server of the row under the cursor, nothing on any other row
func (s *servers) with(act func(l *cards.List, server *mcpv1.Server) tea.Cmd) func(*cards.List, int) tea.Cmd {
	return func(l *cards.List, i int) tea.Cmd {
		if i < 0 || i >= len(s.shown) || s.shown[i].server == nil {
			return nil
		}
		return act(l, s.shown[i].server)
	}
}

// Reads the servers again, then selects the named one, or opens its page when the screen was opened on it
func (s *servers) load(focus string) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := s.service.List(s.kit.Env.Ctx, &mcpv1.ListRequest{Profile: s.profile})
		return func() tea.Cmd {
			s.loaded, s.failed = true, err
			if err == nil {
				s.rows = response.GetServers()
				slices.SortStableFunc(s.rows, func(a, b *mcpv1.Server) int {
					return strings.Compare(strings.ToLower(a.GetName()), strings.ToLower(b.GetName()))
				})
			}
			s.list.Refresh()
			for i, r := range s.shown {
				if r.server != nil && r.server.GetName() == focus {
					s.list.Select(i)
					return s.page(r.server)
				}
			}
			return nil
		}
	})
}

func (s *servers) find(name string) *mcpv1.Server {
	for _, server := range s.rows {
		if server.GetName() == name {
			return server
		}
	}
	return nil
}

// Cells of a row: a server's name, its state, how many tools it offers and where it runs, or what the row does
func (s *servers) cells(r row) []string {
	t := s.kit.Env.Theme
	if r.act != nil {
		return []string{r.act.title, "", "", render.Dim(r.act.about, t)}
	}
	server := r.server
	count := ""
	if server.GetTools() > 0 {
		count = render.Count(int(server.GetTools()), "tool")
	}
	state := cmp.Or(stateWords[server.GetState()], server.GetState())
	switch {
	case server.GetState() == "connected":
		state = render.Good(state, t)
	case server.GetState() == "failed":
		state = render.Bad(state, t)
	case len(server.GetMissing()) > 0 && server.GetEnabled():
		state = render.Warm("Needs values", t)
	case server.GetOauth() && !server.GetSignedIn() && server.GetEnabled():
		state = render.Warm("Not signed in", t)
	case !server.GetEnabled():
		state = render.Dim(state, t)
	}
	return []string{server.GetName(), state, render.Dim(count, t), render.Dim(cmp.Or(server.GetUrl(), server.GetCommand()), t)}
}

// Lines of a server's detail: what went wrong, where it runs, the variables it gets and the values it lacks, the tools the agent may use,
// how it signs in, what it asked of the agent's model and of you, and the plugin it comes with
func (s *servers) detail(server *mcpv1.Server) []string {
	t := s.kit.Env.Theme
	var out []string
	if e := server.GetError(); e != "" {
		out = append(out, render.Bad(sentence(e), t))
	}
	if u := server.GetUrl(); u != "" {
		out = append(out, "Reached at "+u)
	}
	if c := server.GetCommand(); c != "" {
		out = append(out, "Runs on this computer: "+strings.Join(append([]string{c}, server.GetArgs()...), " "))
	}
	if env := server.GetEnv(); len(env) > 0 {
		out = append(out, "Gets the variables "+strings.Join(env, ", ")+".")
	}
	if missing := server.GetMissing(); len(missing) > 0 {
		out = append(out, render.Warm("Needs values that are not set: "+strings.Join(missing, ", ")+". Set values on its page sets them.", t))
	}
	switch {
	case server.GetIncluded():
		out = append(out, "The agent may use only "+strings.Join(server.GetInclude(), ", ")+".")
	case len(server.GetExclude()) > 0:
		out = append(out, "The agent may use every tool but "+strings.Join(server.GetExclude(), ", ")+".")
	}
	if server.GetOauth() {
		if server.GetSignedIn() {
			out = append(out, "Signed in.")
		} else {
			out = append(out, render.Warm("Not signed in yet.", t))
		}
	}
	for _, kind := range []struct {
		name   string
		counts map[string]int32
	}{{"Asked the agent's model", server.GetSampling()}, {"Asked you", server.GetElicitation()}} {
		if len(kind.counts) == 0 {
			continue
		}
		var parts []string
		for _, key := range slices.Sorted(maps.Keys(kind.counts)) {
			parts = append(parts, fmt.Sprintf("%s %d", strings.ReplaceAll(key, "_", " "), kind.counts[key]))
		}
		out = append(out, render.Dim(kind.name+": "+strings.Join(parts, ", ")+".", t))
	}
	if plugin := server.GetPlugin(); plugin != "" {
		out = append(out, render.Dim("Comes with the plugin "+plugin+".", t))
	}
	return out
}

// Text as a sentence: a capital first letter and a period at its end
func sentence(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	text = strings.ToUpper(text[:1]) + text[1:]
	if !strings.HasSuffix(text, ".") && !strings.HasSuffix(text, "?") && !strings.HasSuffix(text, "!") {
		text += "."
	}
	return text
}

// How open sessions get a change: the key that connects every server again
func (s *servers) later() string {
	return "Press " + s.kit.Env.Keys.Display("mcp.reload") + " to update open sessions too."
}

func (s *servers) toggle(server *mcpv1.Server) tea.Cmd {
	enabled := !server.GetEnabled()
	s.hub.Busy("Saving…")
	return tui.Call(s.hub, func() (*mcpv1.EnableResponse, error) {
		return s.service.Enable(s.kit.Env.Ctx, &mcpv1.EnableRequest{Profile: s.profile, Name: server.GetName(), Enabled: enabled})
	}, func(*mcpv1.EnableResponse) tea.Cmd {
		word := " is now off. "
		if enabled {
			word = " is now on. "
		}
		return tea.Batch(s.hub.Done(server.GetName()+word+s.later()), s.load(""))
	})
}

func (s *servers) test(server *mcpv1.Server) tea.Cmd {
	s.hub.Busy("Connecting to " + server.GetName() + "…")
	return tui.Call(s.hub, func() (*mcpv1.ProbeResponse, error) {
		return s.service.Probe(s.kit.Env.Ctx, &mcpv1.ProbeRequest{Profile: s.profile, Name: server.GetName()})
	}, func(r *mcpv1.ProbeResponse) tea.Cmd {
		if !r.GetOk() {
			return s.hub.Fail(strings.TrimSpace("Couldn't connect to " + server.GetName() + ". " + sentence(r.GetError()) + " " + sentence(r.GetNextStep())))
		}
		return s.hub.Done(server.GetName() + " connected and offers " + render.Count(len(r.GetTools()), "tool") + ".")
	})
}

func (s *servers) remove(l *cards.List, server *mcpv1.Server, pop bool) tea.Cmd {
	if plugin := server.GetPlugin(); plugin != "" {
		return s.hub.Fail(server.GetName() + " comes with the plugin " + plugin + ", so it can't be removed. Turn it off instead.")
	}
	run := func() tea.Cmd {
		s.hub.Busy("Removing " + server.GetName() + "…")
		return tui.Call(s.hub, func() (*mcpv1.RemoveResponse, error) {
			return s.service.Remove(s.kit.Env.Ctx, &mcpv1.RemoveRequest{Profile: s.profile, Name: server.GetName(), Purge: true})
		}, func(*mcpv1.RemoveResponse) tea.Cmd {
			cmds := []tea.Cmd{s.hub.Done("Removed " + server.GetName() + "."), s.load("")}
			if pop {
				cmds = append(cmds, s.hub.PopTo(s.list))
			}
			return tea.Batch(cmds...)
		})
	}
	l.Confirm("Remove "+server.GetName()+"? Its sign-in and any files it downloaded are deleted too.", run)
	return nil
}

// Connects every server of the profile again after a yes, so open sessions get what changed
func (s *servers) reload(l *cards.List) tea.Cmd {
	l.Confirm("Connect every MCP server again? Open sessions get your changes, and their next message costs more as the whole prompt is sent again.",
		func() tea.Cmd {
			s.hub.Busy("Connecting the servers again…")
			return tui.Call(s.hub, func() (*mcpv1.ReloadResponse, error) {
				return s.service.Reload(s.kit.Env.Ctx, &mcpv1.ReloadRequest{Profile: s.profile})
			}, func(r *mcpv1.ReloadResponse) tea.Cmd {
				told := "Connected " + render.Count(int(r.GetServers()), "server") + " again, with " + render.Count(int(r.GetTools()), "tool") + "."
				if r.GetServers() == 0 {
					told = "No server is connected now."
				}
				if failed := r.GetFailed(); len(failed) > 0 {
					return tea.Batch(s.load(""), s.hub.Fail(told+" Couldn't connect "+strings.Join(failed, ", ")+"."))
				}
				return tea.Batch(s.load(""), s.hub.Done(told))
			})
		})
	return nil
}

// Page of one server: what it is and how it stands, then what can be done to it, each with what it does
func (s *servers) page(server *mcpv1.Server) tea.Cmd {
	t := s.kit.Env.Theme
	acts := func() []step {
		current := cmp.Or(s.find(server.GetName()), server)
		toggle := step{"Turn on", "Load it into new sessions", "turn on", func(*cards.List) tea.Cmd { return s.toggle(current) }}
		if current.GetEnabled() {
			toggle = step{"Turn off", "Stop loading it into new sessions", "turn off", func(*cards.List) tea.Cmd { return s.toggle(current) }}
		}
		out := []step{toggle,
			{"Test", "Connect to it once and count its tools", "test", func(*cards.List) tea.Cmd { return s.test(current) }},
			{"Choose tools", "Choose which of its tools the agent may use", "choose", func(*cards.List) tea.Cmd { return s.tools(current) }},
		}
		if current.GetOauth() {
			signin := step{"Sign in", "Sign in with the browser on this computer", "sign in", func(*cards.List) tea.Cmd { return s.signin(current, "") }}
			if current.GetSignedIn() {
				signin.title = "Sign in again"
			}
			out = append(out, signin, step{"Sign in with a code", "Sign in on another device with a code, for a computer without a browser", "sign in",
				func(*cards.List) tea.Cmd { return s.signin(current, "device") }})
		}
		if names := slices.Concat(current.GetMissing(), current.GetEnv()); len(names) > 0 || strings.Contains(current.GetSettings(), "${") {
			out = append(out, step{"Set values", "API keys and other values its settings read", "set", func(*cards.List) tea.Cmd { return s.values(current) }})
		}
		if current.GetPlugin() == "" {
			out = append(out, step{"Change settings", "How it connects and every other setting of it, in settings", "open",
				func(*cards.List) tea.Cmd { return s.settings() }},
				step{"Remove", "Remove it, its sign-in and any files it downloaded", "remove", func(l *cards.List) tea.Cmd { return s.remove(l, current, true) }})
		}
		return out
	}
	var rows []step
	var list *cards.List
	list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   server.GetName(),
		Glyph:   t.Glyphs.MCP,
		Key:     Screen + ":" + server.GetName(),
		Context: cards.HubContext,
		// Connecting every server again works here too, as the notes of its rows say
		Shared:  []string{serversContext},
		Primary: "hub.use",
		Free:    map[string]bool{"mcp.reload": true},
		Intro: func() []string {
			current := cmp.Or(s.find(server.GetName()), server)
			state := cmp.Or(stateWords[current.GetState()], current.GetState()) + "."
			if current.GetTools() > 0 {
				state = strings.TrimSuffix(state, ".") + ", with " + render.Count(int(current.GetTools()), "tool") + "."
			}
			return append([]string{state}, s.detail(current)...)
		},
		Columns: []render.Column{{}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			rows = acts()
			cells := make([][]string, len(rows))
			for i, r := range rows {
				cells[i] = []string{r.title, render.Dim(r.about, t)}
			}
			return cells
		},
		Detail: func(i int) []string {
			if list.Shows(1) || i < 0 || i >= len(rows) {
				return nil
			}
			return []string{rows[i].about + "."}
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(rows) {
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: rows[i].verb}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use":    func(l *cards.List, i int) tea.Cmd { return rows[i].run(l) },
			"mcp.reload": func(l *cards.List, _ int) tea.Cmd { return s.reload(l) },
		},
	})
	list.ToggleDetail()
	return s.hub.Push(list)
}

// Opens the servers setting in place of the screen, where every setting of a server is changed
func (s *servers) settings() tea.Cmd {
	return tea.Batch(s.kit.WM.DismissCard(s.hub), s.kit.Open(configapp.Edit, profile.Join(s.profile, mcpconfig.Servers)))
}

// Asks the values the server's settings read, hidden as they are typed, one form for all of them
func (s *servers) values(server *mcpv1.Server) tea.Cmd {
	names := slices.Concat(server.GetMissing(), refs(server.GetSettings()))
	slices.Sort(names)
	names = slices.Compact(names)
	if len(names) == 0 {
		return s.hub.Say(server.GetName()+" reads no values.", cards.NoteInfo)
	}
	fields := make([]cards.Field, len(names))
	for i, name := range names {
		help := "Set. Leave empty to keep it."
		if slices.Contains(server.GetMissing(), name) {
			help = "Not set yet."
		}
		fields[i] = cards.Field{ID: name, Label: name, Kind: cards.FieldSecret, Help: help}
	}
	var form *cards.Form
	form = cards.NewForm(s.kit.Env, cards.FormSpec{
		Title:  "Values",
		Intro:  []string{"Values that the settings of " + server.GetName() + " read, such as API keys. A field you leave empty keeps its value."},
		Fields: fields,
		Submit: "Save",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			s.hub.Busy("Saving…")
			return tui.Call(s.hub, func() (int, error) {
				saved := 0
				for _, name := range names {
					value := strings.TrimSpace(values[name])
					if value == "" {
						continue
					}
					if _, err := s.service.SetValue(s.kit.Env.Ctx, &mcpv1.SetValueRequest{Profile: s.profile, Name: name, Value: value}); err != nil {
						return saved, err
					}
					saved++
				}
				return saved, nil
			}, func(saved int) tea.Cmd {
				told := "Saved " + render.Count(saved, "value") + " for " + server.GetName() + "."
				if saved == 0 {
					told = "Nothing changed, since every field was empty."
				}
				return tea.Batch(s.hub.Close(form), s.hub.Done(told), s.load(""))
			})
		},
	})
	return s.hub.Push(form)
}

// Names a settings JSON reads as ${NAME}, those of the environment's own variables and the context's left out
func refs(settings string) []string {
	var out []string
	for rest := settings; ; {
		start := strings.Index(rest, "${")
		if start < 0 {
			return out
		}
		rest = rest[start+2:]
		end := strings.Index(rest, "}")
		if end < 0 {
			return out
		}
		name := rest[:end]
		rest = rest[end+1:]
		if name == "" || strings.Contains(name, ":") || slices.Contains([]string{"userHome", "workspaceFolder", "workspaceFolderBasename", "pathSeparator",
			"INSTALL_DIR"}, name) {
			continue
		}
		out = append(out, name)
	}
}

// Tools of a server as a checklist: the server is asked for its tools and each one turned on or off for new sessions
func (s *servers) tools(server *mcpv1.Server) tea.Cmd {
	s.hub.Busy("Asking " + server.GetName() + " for its tools…")
	return tui.Call(s.hub, func() (*mcpv1.ProbeResponse, error) {
		return s.service.Probe(s.kit.Env.Ctx, &mcpv1.ProbeRequest{Profile: s.profile, Name: server.GetName()})
	}, func(r *mcpv1.ProbeResponse) tea.Cmd {
		if !r.GetOk() {
			return s.hub.Fail(strings.TrimSpace("Couldn't list the tools of " + server.GetName() + ". " + sentence(r.GetError()) + " " + sentence(r.GetNextStep())))
		}
		s.hub.Quiet()
		found := r.GetTools()
		on := make([]bool, len(found))
		for i, tool := range found {
			on[i] = allowed(server, tool.GetName())
		}
		t := s.kit.Env.Theme
		var list *cards.List
		save := func(changed string, now bool) tea.Cmd {
			var picked []string
			for i, tool := range found {
				if on[i] {
					picked = append(picked, tool.GetName())
				}
			}
			request := &mcpv1.SetFilterRequest{Profile: s.profile, Name: server.GetName(), Mode: "include", Values: picked}
			switch {
			case len(picked) == len(found):
				request.Clear = true
			case server.GetIncluded() || len(server.GetExclude()) == 0:
			default:
				var dropped []string
				for i, tool := range found {
					if !on[i] {
						dropped = append(dropped, tool.GetName())
					}
				}
				request.Mode, request.Values = "exclude", keepGlobs(server.GetExclude(), dropped)
			}
			return tui.Call(s.hub, func() (*mcpv1.SetFilterResponse, error) {
				return s.service.SetFilter(s.kit.Env.Ctx, request)
			}, func(*mcpv1.SetFilterResponse) tea.Cmd {
				list.Refresh()
				state := "off"
				if now {
					state = "on"
				}
				told := fmt.Sprintf("%s is now %s. %d of %s are on for new sessions.", changed, state, len(picked), render.Count(len(found), "tool"))
				return tea.Batch(s.hub.Done(told), s.load(""))
			})
		}
		toggle := func(_ *cards.List, i int) tea.Cmd {
			if i < 0 || i >= len(found) {
				return nil
			}
			on[i] = !on[i]
			return save(found[i].GetName(), on[i])
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

// Exclude list after a checklist edit: the globs it had, then the tools left off by name
func keepGlobs(exclude, dropped []string) []string {
	var out []string
	for _, pattern := range exclude {
		if strings.ContainsAny(pattern, "*?[") {
			out = append(out, pattern)
		}
	}
	for _, name := range dropped {
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

// Adds a server: one of the catalog's, each value it needs asked first, or one described by hand
func (s *servers) add() tea.Cmd {
	s.hub.Busy("Reading the catalog…")
	return tui.Call(s.hub, func() (*mcpv1.CatalogResponse, error) {
		return s.service.Catalog(s.kit.Env.Ctx, &mcpv1.CatalogRequest{Profile: s.profile})
	}, func(response *mcpv1.CatalogResponse) tea.Cmd {
		s.hub.Quiet()
		entries := map[string]*mcpv1.Entry{}
		choices := []tui.Choice{{ID: "", Title: "Your own server", Detail: "A command to run on this computer, or an address to reach"}}
		for _, entry := range response.GetEntries() {
			if entry.GetInstalled() || entry.GetProblem() != "" {
				continue
			}
			entries[entry.GetName()] = entry
			choices = append(choices, tui.Choice{ID: entry.GetName(), Title: entry.GetName(), Detail: entry.GetDescription()})
		}
		return s.kit.Pick(s.hub, "Add a server", "add", choices, "", func(choice tui.Choice) tea.Cmd {
			if choice.ID == "" {
				return s.custom()
			}
			return s.install(entries[choice.ID])
		})
	})
}

// Installs a catalog entry: the values it lacks asked, its repository cloned when it runs from source, its settings kept, then a sign in
// for one that needs it
func (s *servers) install(entry *mcpv1.Entry) tea.Cmd {
	missing := entry.GetMissing()
	if len(missing) == 0 {
		return s.installWith(entry, nil)
	}
	fields := make([]cards.Field, len(missing))
	for i, spec := range missing {
		kind := cards.FieldText
		if spec.GetSecret() {
			kind = cards.FieldSecret
		}
		help := sentence(spec.GetPrompt())
		if !spec.GetRequired() {
			help = strings.TrimSpace(help + "\nLeave empty to go without it.")
		}
		fields[i] = cards.Field{ID: spec.GetName(), Label: spec.GetName(), Kind: kind, Value: spec.GetDefault(), Required: spec.GetRequired(), Help: help}
	}
	var form *cards.Form
	form = cards.NewForm(s.kit.Env, cards.FormSpec{
		Title:  entry.GetName(),
		Intro:  []string{sentence(entry.GetDescription()), entry.GetName() + " needs these values before it can run."},
		Fields: fields,
		Submit: "Add",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			return tea.Batch(s.hub.Close(form), s.installWith(entry, values))
		},
	})
	return s.hub.Push(form)
}

func (s *servers) installWith(entry *mcpv1.Entry, values map[string]string) tea.Cmd {
	name := entry.GetName()
	s.hub.Busy("Adding " + name + "…")
	return tui.Call(s.hub, func() (string, error) {
		ctx := s.kit.Env.Ctx
		secret := map[string]bool{}
		for _, spec := range entry.GetRequires() {
			secret[spec.GetName()] = spec.GetSecret()
		}
		for key, value := range values {
			if value = strings.TrimSpace(value); value != "" && secret[key] {
				if _, err := s.service.SetValue(ctx, &mcpv1.SetValueRequest{Profile: s.profile, Name: key, Value: value}); err != nil {
					return "", err
				}
			}
		}
		dir := ""
		if entry.GetFromSource() {
			cloned, err := s.service.GitInstall(ctx, &mcpv1.GitInstallRequest{Profile: s.profile, Entry: name})
			if err != nil {
				return "", err
			}
			if cloned.GetError() != "" {
				return "", fmt.Errorf("Couldn't download %s. %s", name, sentence(cloned.GetError()))
			}
			dir = cloned.GetDir()
		}
		settings, err := s.service.InstallConfig(ctx, &mcpv1.InstallConfigRequest{Profile: s.profile, Entry: name, InstallDir: dir, Values: values})
		if err != nil {
			return "", err
		}
		saved, err := s.service.Save(ctx, &mcpv1.SaveRequest{Profile: s.profile, Name: name, Settings: settings.GetSettings()})
		if err != nil {
			return "", err
		}
		if issues := saved.GetIssues(); len(issues) > 0 {
			return "", fmt.Errorf("%s was not added. %s", name, sentences(issues))
		}
		return settings.GetSettings(), nil
	}, func(string) tea.Cmd {
		cmds := []tea.Cmd{s.hub.Done("Added " + name + ". " + s.later()), s.load(name)}
		if note := strings.TrimSpace(entry.GetPostInstall()); note != "" {
			cmds = append(cmds, s.kit.WM.Toast(note, wm.Info, 2*wm.DefaultToastTTL))
		}
		if entry.GetAuth() == "oauth" && entry.GetProvider() == "" {
			cmds = append(cmds, s.signinFlow(name, "", ""))
		}
		return tea.Batch(cmds...)
	})
}

// Reasons as sentences, one after another
func sentences(reasons []string) string {
	out := make([]string, len(reasons))
	for i, reason := range reasons {
		out[i] = sentence(reason)
	}
	return strings.Join(out, " ")
}

// How a server described by hand connects, by the value its form keeps
const (
	byCommand = "stdio"
	byAddress = "http"
	bySignin  = "oauth"
)

// Form of a server described by hand: its name, how it connects, then the command and its arguments and variables, or the address and a
// token it sends, the fields changing with how it connects
func (s *servers) custom() tea.Cmd {
	way := byCommand
	fields := func(values map[string]string) []cards.Field {
		out := []cards.Field{
			{ID: "name", Label: "Name", Value: values["name"], Required: true,
				Help: "The name the agent knows it by: lower case letters, digits, - and _.", Check: func(value string) string {
					for _, r := range strings.TrimSpace(value) {
						if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
							return "Use only lower case letters, digits, - and _."
						}
					}
					return ""
				}},
			{ID: "transport", Label: "How it connects", Kind: cards.FieldChoice, Value: way, Choices: []cards.Choice{
				{Value: byCommand, Label: "A command", Meaning: "Runs on this computer and talks over its input and output"},
				{Value: byAddress, Label: "An address", Meaning: "Reached over the web at an address"},
				{Value: bySignin, Label: "An address with sign-in", Meaning: "Reached over the web, and you sign in to it in your browser"},
			}},
		}
		if way == byCommand {
			return append(out,
				cards.Field{ID: "target", Label: "Command", Value: values["target"], Required: true, Help: "The program that starts the server, such as npx or uvx."},
				cards.Field{ID: "args", Label: "Arguments", Value: values["args"], Help: "Split like a shell line, such as -y @modelcontextprotocol/server-github."},
				cards.Field{ID: "env", Label: "Variables", Value: values["env"], More: true, Help: "NAME=value pairs that the command gets, separated by commas."})
		}
		return append(out,
			cards.Field{ID: "target", Label: "Address", Value: values["target"], Required: true, Help: "Such as https://example.com/mcp."},
			cards.Field{ID: "token", Label: "Token", Kind: cards.FieldSecret, Value: values["token"], More: true,
				Help: "A bearer token sent with every request, if the server asks for one.\nKept with your MCP values."})
	}
	var form *cards.Form
	form = cards.NewForm(s.kit.Env, cards.FormSpec{
		Title:  "Your own server",
		Intro:  []string{"A server that runs on this computer, or one you reach at an address."},
		Fields: fields(nil),
		Submit: "Add",
		Changed: func(f *cards.Form, field cards.Field) tea.Cmd {
			if field.ID == "transport" && (field.Value == byCommand) != (way == byCommand) {
				way = field.Value
				f.SetFields(fields(f.Values()))
				return nil
			}
			if field.ID == "transport" {
				way = field.Value
			}
			return nil
		},
		Done: func(_ *cards.Form, v map[string]string) tea.Cmd {
			name := strings.TrimSpace(v["name"])
			settings := map[string]any{"enabled": true}
			target := strings.TrimSpace(v["target"])
			if v["transport"] == byCommand {
				settings["command"] = target
				args, err := cards.ShellWords(v["args"])
				if err != nil {
					form.SetError("args", sentence(err.Error()))
					return nil
				}
				if len(args) > 0 {
					settings["args"] = args
				}
				if env := strings.TrimSpace(v["env"]); env != "" {
					var pairs []any
					for pair := range strings.SplitSeq(env, ",") {
						key, value, ok := strings.Cut(strings.TrimSpace(pair), "=")
						if !ok || strings.TrimSpace(key) == "" {
							form.SetError("env", "Write each as NAME=value. This one is not: "+strings.TrimSpace(pair))
							return nil
						}
						pairs = append(pairs, strings.TrimSpace(key)+"="+value)
					}
					settings["env"] = pairs
				}
			} else {
				settings["url"] = target
				if v["transport"] == bySignin {
					settings["auth"] = "oauth"
				}
			}
			token := strings.TrimSpace(v["token"])
			s.hub.Busy("Adding " + name + "…")
			return tui.Call(s.hub, func() (*mcpv1.SaveResponse, error) {
				ctx := s.kit.Env.Ctx
				if token != "" && v["transport"] != byCommand {
					bearer, err := s.service.SaveBearer(ctx, &mcpv1.SaveBearerRequest{Profile: s.profile, Name: name, Token: token})
					if err != nil {
						return nil, err
					}
					var headers map[string]any
					json.Unmarshal([]byte(bearer.GetHeaders()), &headers)
					settings["headers"] = headers
				}
				data, err := json.Marshal(settings)
				if err != nil {
					return nil, err
				}
				return s.service.Save(ctx, &mcpv1.SaveRequest{Profile: s.profile, Name: name, Settings: string(data)})
			}, func(saved *mcpv1.SaveResponse) tea.Cmd {
				if issues := saved.GetIssues(); len(issues) > 0 {
					return s.hub.Fail(name + " was not added. " + sentences(issues))
				}
				cmds := []tea.Cmd{s.hub.Close(form), s.hub.Done("Added " + name + ". " + s.later()), s.load(name)}
				if v["transport"] == bySignin {
					cmds = append(cmds, s.signinFlow(name, "", ""))
				}
				return tea.Batch(cmds...)
			})
		},
	})
	return s.hub.Push(form)
}

// Signs in to a server, in the browser of this computer or with a code on another device
func (s *servers) signin(server *mcpv1.Server, flow string) tea.Cmd {
	return s.signinFlow(server.GetName(), flow, "")
}

// Signs in to each server in turn, the next once the last one ended
func (s *servers) signinAll(names []string) tea.Cmd {
	if len(names) == 0 {
		return nil
	}
	return signin(s.kit, s.hub, s.service, &mcpv1.LoginRequest{Profile: s.profile, Name: names[0]}, func(event *mcpv1.LoginEvent) tea.Cmd {
		note := s.signedIn(names[0], event)
		if len(names) == 1 {
			return tea.Batch(note, s.load(""))
		}
		return tea.Batch(note, s.signinAll(names[1:]))
	})
}

// Signs in to a server on a page of the hub: the link opened in this computer's browser, copied or the address the browser ended on pasted,
// until the server approves it or it fails, closing the page stopping it
func (s *servers) signinFlow(name, flow, settings string) tea.Cmd {
	return signin(s.kit, s.hub, s.service, &mcpv1.LoginRequest{Profile: s.profile, Name: name, Flow: flow, Settings: settings}, func(event *mcpv1.LoginEvent) tea.Cmd {
		return tea.Batch(s.signedIn(name, event), s.load(""))
	})
}

// Note of how a sign in ended
func (s *servers) signedIn(name string, event *mcpv1.LoginEvent) tea.Cmd {
	switch {
	case event.GetError() != "":
		return s.hub.Fail("Couldn't sign in to " + name + ". " + sentence(event.GetError()))
	case event.GetNoToken():
		return s.hub.Fail("The sign-in to " + name + " ended without letting icebath in. Try again.")
	}
	return s.hub.Done("Signed in to " + name + ". It offers " + render.Count(int(event.GetTools()), "tool") + ".")
}

package app

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	mcpconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	mcpv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/mcp/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/mcp/v1/mcpv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// First line of the page, what the area is
const intro = "Servers that give the agent more tools, such as GitHub."

// How often the page reads how the servers stand while it is open, so a server that connects or fails shows it
const refresh = 3 * time.Second

// Headings of the page: the servers set up here and those of plugins, then the options
const (
	ownGroup     = "Your servers"
	pluginGroup  = "From plugins"
	optionsGroup = "Options"
)

// The page of MCP servers on one profile: every server with its state, adding one, connecting them again, signing in to each, the page of
// each server, and the options of the area
type servers struct {
	kit      *tui.Kit
	service  mcpv1connect.McpServiceClient
	settings configv1connect.ConfigServiceClient
	profile  string
	hub      *cards.Hub
	list     *cards.List
	options  *configapp.Rows
	rows     []*mcpv1.Server
	loaded   bool
	failed   error
	// Rows as last listed
	shown []row
	// Lists opened over the page that show servers, read again with it
	views cards.Watched
	// Server put in hand once the servers are read again
	focus string
	// The page reads the servers again while it is in view
	watching bool
}

// Row of the page: a server, the row that adds one, something done to every server, an option or the row of the advanced options
type row struct {
	server   *mcpv1.Server
	add      bool
	act      *step
	setting  *configv1.Setting
	advanced bool
}

// Something a row does, its name, what it does and the word on its button
type step struct {
	title, about, verb string
	run                func(l *cards.List) tea.Cmd
}

func (s *servers) open(argument string) tea.Cmd {
	t := s.kit.Env.Theme
	s.list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   "MCP servers",
		Glyph:   t.Glyphs.MCP,
		Key:     Screen,
		Context: cards.HubContext,
		// The keys of the page stay in force on every row, those the footer has no room for too
		Shared:  []string{serversContext},
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search MCP servers",
		Intro:   func() []string { return []string{intro} },
		Columns: configapp.Columns,
		Rows: func() [][]string {
			s.shown = s.order()
			cells := make([][]string, len(s.shown))
			for i, r := range s.shown {
				cells[i] = s.cells(r)
			}
			return cells
		},
		Group: func(i int) string { return s.group(s.shown[i]) },
		Words: func(i int) string {
			if server := s.shown[i].server; server != nil {
				return server.GetUrl() + " " + server.GetCommand() + " " + server.GetPlugin()
			}
			return ""
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(s.shown) {
				return nil
			}
			return s.explain(s.shown[i], s.list.Cut(i, 2))
		},
		Empty:   func() string { return tui.Waiting(s.loaded, s.failed, "") },
		OnFocus: func(*cards.List) tea.Cmd { return tea.Batch(s.load(""), s.watch()) },
		Free:    map[string]bool{"hub.add": true, "hub.refresh": true, "mcp.reload": true},
		Hints:   func(_ *cards.List, i int) []wm.Hint { return s.hints(i) },
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use":     func(l *cards.List, i int) tea.Cmd { return s.use(l, s.shown[i]) },
			"mcp.toggle":  s.with(func(_ *cards.List, server *mcpv1.Server) tea.Cmd { return s.toggle(server) }),
			"mcp.test":    s.with(func(_ *cards.List, server *mcpv1.Server) tea.Cmd { return s.test(server) }),
			"mcp.signin":  s.with(func(_ *cards.List, server *mcpv1.Server) tea.Cmd { return s.signin(server, "") }),
			"hub.edit":    s.with(func(_ *cards.List, server *mcpv1.Server) tea.Cmd { return s.tools(server) }),
			"hub.remove":  func(l *cards.List, i int) tea.Cmd { return s.removeRow(l, s.shown[i]) },
			"hub.add":     func(*cards.List, int) tea.Cmd { return s.add() },
			"hub.refresh": func(*cards.List, int) tea.Cmd { return tea.Batch(s.load(""), s.options.Load(nil)) },
			"mcp.reload":  func(l *cards.List, _ int) tea.Cmd { return s.reload(l) },
		},
	})
	s.list.ToggleDetail()
	hub, cmd := s.kit.Show(Screen, s.list)
	s.hub = hub
	s.options = configapp.NewRows(s.kit, hub, s.settings, s.profile)
	s.options.Watch(s.list)
	argument = strings.TrimSpace(argument)
	then := func() tea.Cmd {
		switch argument {
		case "":
			return nil
		case "add":
			return s.add()
		}
		if server := s.find(argument); server != nil {
			return s.page(server.GetName())
		}
		return s.hub.Fail("No MCP server is called " + argument + ".")
	}
	return tea.Batch(cmd, s.options.Load(nil), s.read("", then), s.watch())
}

// Rows of the page: the servers set up here, those of plugins, the row that adds one, what is done to every server while there are any, then
// the options and the row of the advanced ones
func (s *servers) order() []row {
	if !s.loaded || s.failed != nil {
		return nil
	}
	var rows []row
	for _, plugin := range []bool{false, true} {
		for _, server := range s.rows {
			if (server.GetPlugin() != "") == plugin {
				rows = append(rows, row{server: server})
			}
		}
	}
	rows = append(rows, row{add: true})
	if len(s.rows) > 0 {
		rows = append(rows, row{act: &step{"Connect again", "Every server connects again, so open sessions get your changes.", "connect", s.reload}})
	}
	if names := s.oauth(); len(names) > 1 {
		rows = append(rows, row{act: &step{"Sign in to every server", "Signs in again to each server that takes a sign-in, one after another.",
			"sign in", func(*cards.List) tea.Cmd { return s.signinAll(names) }}})
	}
	everyday, _ := s.options.Section(mcpconfig.Key)
	for _, setting := range everyday {
		rows = append(rows, row{setting: setting})
	}
	if _, _, ok := s.options.Advanced(mcpconfig.Key); ok {
		rows = append(rows, row{advanced: true})
	}
	return rows
}

// Servers that sign in through a page of their own and are on
func (s *servers) oauth() []string {
	var names []string
	for _, server := range s.rows {
		if server.GetOauth() && server.GetEnabled() {
			names = append(names, server.GetName())
		}
	}
	return names
}

// Heading of a row: the servers under whose they are, the rows after them none, so they sit with them, and the options theirs
func (s *servers) group(r row) string {
	switch {
	case r.server != nil && r.server.GetPlugin() != "":
		return pluginGroup
	case r.server != nil:
		return ownGroup
	case r.setting != nil, r.advanced:
		return optionsGroup
	}
	return ""
}

// Cells of a row: a server's name and its state, the row that adds one, what a row does, an option as Settings draws it
func (s *servers) cells(r row) []string {
	t := s.kit.Env.Theme
	switch {
	case r.server != nil:
		return []string{r.server.GetName(), "", stateWord(r.server, t)}
	case r.add:
		return []string{"Add a server", render.Dim(t.Glyphs.Forward, t), ""}
	case r.act != nil:
		return []string{r.act.title, "", ""}
	case r.setting != nil:
		return s.options.Cells(r.setting)
	}
	cells, _, _ := s.options.Advanced(mcpconfig.Key)
	return cells
}

// State of a server in the words of the one function that says it, in the colour of what it means
func stateWord(server *mcpv1.Server, t theme.Theme) string {
	word := server.GetShown()
	switch word {
	case "Connected":
		return render.Good(word, t)
	case "Failed":
		return render.Bad(word, t)
	case "Needs values", "Not signed in":
		return render.Warm(word, t)
	}
	return word
}

// Lines under the list about the row in hand, never what the row or the first line says
func (s *servers) explain(r row, cut bool) []string {
	switch {
	case r.server != nil:
		return s.detail(r.server)
	case r.add:
		return []string{"One from the catalog, such as GitHub, or a server of your own."}
	case r.act != nil:
		return []string{r.act.about}
	case r.setting != nil:
		return s.options.Detail(r.setting, cut)
	}
	return []string{"Settings that few people change."}
}

// Lines of a server's detail: what went wrong, where it runs, the tools it offers and those the agent may use, the values it lacks, how it
// signs in, what it asked of the agent's model and of you, and the plugin it comes with
func (s *servers) detail(server *mcpv1.Server) []string {
	t := s.kit.Env.Theme
	var out []string
	if e := server.GetError(); e != "" {
		out = append(out, render.Bad(strs.Full(e), t))
	}
	if u := server.GetUrl(); u != "" {
		out = append(out, "Reached at "+u+".")
	}
	if c := server.GetCommand(); c != "" {
		// As a shell would read it back, so a word with a space in it shows as one word
		out = append(out, "Runs on this computer: "+strs.Quote(append([]string{c}, server.GetArgs()...)))
	}
	if n := server.GetTools(); n > 0 {
		out = append(out, "Offers "+render.Count(int(n), "tool")+".")
	}
	switch {
	case server.GetIncluded():
		out = append(out, "The agent may use only "+strings.Join(server.GetInclude(), ", ")+".")
	case len(server.GetExclude()) > 0:
		out = append(out, "The agent may use every tool but "+strings.Join(server.GetExclude(), ", ")+".")
	}
	if missing := server.GetMissing(); len(missing) > 0 && server.GetShown() != "Needs values" {
		out = append(out, render.Warm("Needs values that are not set: "+strings.Join(missing, ", ")+".", t))
	}
	if server.GetOauth() && server.GetSignedIn() {
		out = append(out, "Signed in.")
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

// Buttons for the row in hand: on a server open, turn off or on, test, remove for one of yours, and add; what a row does on any other
func (s *servers) hints(i int) []wm.Hint {
	add := wm.Hint{Action: "hub.add", Label: "add"}
	if i < 0 || i >= len(s.shown) {
		return []wm.Hint{add}
	}
	switch r := s.shown[i]; {
	case r.server != nil:
		toggle := "turn on"
		if r.server.GetEnabled() {
			toggle = "turn off"
		}
		hints := []wm.Hint{{Action: "hub.use", Label: "open"}, {Action: "mcp.toggle", Label: toggle}, {Action: "mcp.test", Label: "test"}}
		if r.server.GetPlugin() == "" {
			hints = append(hints, wm.Hint{Action: "hub.remove", Label: "remove"})
		}
		return append(hints, add)
	case r.add:
		return []wm.Hint{{Action: "hub.use", Label: "add"}}
	case r.act != nil:
		return []wm.Hint{{Action: "hub.use", Label: r.act.verb}, add}
	case r.setting != nil:
		return append(s.options.Hints(r.setting), add)
	}
	return []wm.Hint{{Action: "hub.use", Label: "open"}, add}
}

// What enter does on a row: opens a server's page, adds one, does what the row does, changes an option or opens the advanced ones
func (s *servers) use(l *cards.List, r row) tea.Cmd {
	switch {
	case r.server != nil:
		return s.page(r.server.GetName())
	case r.add:
		return s.add()
	case r.act != nil:
		return r.act.run(l)
	case r.setting != nil:
		return s.options.Change(r.setting)
	}
	_, open, _ := s.options.Advanced(mcpconfig.Key)
	return open()
}

// What ctrl+d does on a row: removes a server of yours after a question, or puts a changed option back to its default
func (s *servers) removeRow(l *cards.List, r row) tea.Cmd {
	switch {
	case r.server != nil:
		return s.remove(l, r.server)
	case r.setting != nil:
		return s.options.Reset(l, r.setting)
	}
	return nil
}

// Action on the server of the row in hand, nothing on any other row
func (s *servers) with(act func(l *cards.List, server *mcpv1.Server) tea.Cmd) func(*cards.List, int) tea.Cmd {
	return func(l *cards.List, i int) tea.Cmd {
		if i < 0 || i >= len(s.shown) || s.shown[i].server == nil {
			return nil
		}
		return act(l, s.shown[i].server)
	}
}

// Reads the servers again, the named one put in hand
func (s *servers) load(focus string) tea.Cmd { return s.read(focus, nil) }

// Reads the servers again, the named one put in hand, then runs then
func (s *servers) read(focus string, then func() tea.Cmd) tea.Cmd {
	if focus != "" {
		s.focus = focus
	}
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
			s.views.Refresh()
			if s.focus != "" {
				for i, r := range s.shown {
					if r.server != nil && r.server.GetName() == s.focus && s.list.Select(i) {
						s.focus = ""
					}
				}
			}
			if then != nil {
				return then()
			}
			return nil
		}
	})
}

// Reads the servers again every little while, while the page is in view. It stops once the page left the screen, and a list of the page
// that comes back into view starts it again, so a page the menu shows again goes on following its servers
func (s *servers) watch() tea.Cmd {
	if s.watching {
		return nil
	}
	s.watching = true
	return s.tick()
}

func (s *servers) tick() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		time.Sleep(refresh)
		return func() tea.Cmd {
			switch {
			case !s.hub.Shown():
				s.watching = false
				return nil
			case s.hub.Waiting():
				return s.tick()
			}
			return tea.Batch(s.load(""), s.tick())
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

// Turns a server on or off for new sessions
func (s *servers) toggle(server *mcpv1.Server) tea.Cmd {
	enabled := !server.GetEnabled()
	s.hub.Busy("Saving…")
	return tui.Call(s.hub, func() (*mcpv1.EnableResponse, error) {
		return s.service.Enable(s.kit.Env.Ctx, &mcpv1.EnableRequest{Profile: s.profile, Name: server.GetName(), Enabled: enabled})
	}, func(*mcpv1.EnableResponse) tea.Cmd {
		told := server.GetName() + " is now off for new sessions."
		if enabled {
			told = server.GetName() + " is now on for new sessions."
		}
		return tea.Batch(s.hub.Done(told), s.load(""), s.options.Load(nil))
	})
}

// Connects to a server once and counts its tools
func (s *servers) test(server *mcpv1.Server) tea.Cmd {
	s.hub.Busy("Connecting to " + server.GetName() + "…")
	return tui.Call(s.hub, func() (*mcpv1.ProbeResponse, error) {
		return s.service.Probe(s.kit.Env.Ctx, &mcpv1.ProbeRequest{Profile: s.profile, Name: server.GetName()})
	}, func(r *mcpv1.ProbeResponse) tea.Cmd {
		if !r.GetOk() {
			return s.hub.Fail(strings.TrimSpace("Couldn't connect to " + server.GetName() + ". " + strs.Full(r.GetError()) + " " + strs.Full(r.GetNextStep())))
		}
		return s.hub.Done(server.GetName() + " connected and offers " + render.Count(len(r.GetTools()), "tool") + ".")
	})
}

// Asks before a server of yours is removed with its sign-in and the files it downloaded, then removes it
func (s *servers) remove(l *cards.List, server *mcpv1.Server) tea.Cmd {
	if plugin := server.GetPlugin(); plugin != "" {
		return s.hub.Fail(server.GetName() + " comes with the plugin " + plugin + ", so it can't be removed. Turn it off instead.")
	}
	l.Confirm(cards.Question{Text: "Remove " + server.GetName() + "? Its sign-in and any files it downloaded are deleted too.", Verb: "Remove",
		Deletes: true}, func() tea.Cmd {
		s.hub.Busy("Removing " + server.GetName() + "…")
		return tui.Call(s.hub, func() (*mcpv1.RemoveResponse, error) {
			return s.service.Remove(s.kit.Env.Ctx, &mcpv1.RemoveRequest{Profile: s.profile, Name: server.GetName(), Purge: true})
		}, func(*mcpv1.RemoveResponse) tea.Cmd {
			return tea.Batch(s.hub.PopTo(s.list), s.hub.Done("Removed "+server.GetName()+"."), s.load(""), s.options.Load(nil))
		})
	})
	return nil
}

// Connects every server of the profile again, so open sessions get what changed, asking first while Confirm tool reload is on: the rule
// /reload-mcp follows too
func (s *servers) reload(l *cards.List) tea.Cmd {
	run := func() tea.Cmd {
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
	}
	if confirm := s.options.Setting(mcpconfig.ReloadConfirm); confirm != nil && confirm.GetValue() == "false" {
		return run()
	}
	l.Confirm(cards.Question{Verb: "Connect", Text: "Every MCP server connects again, so open sessions get your changes. Their next message then " +
		"sends the whole session again, which costs more."}, run)
	return nil
}

// Signs in to a server, in the browser of this computer or with a code on another device
func (s *servers) signin(server *mcpv1.Server, flow string) tea.Cmd {
	return signin(s.kit, s.hub, s.service, &mcpv1.LoginRequest{Profile: s.profile, Name: server.GetName(), Flow: flow}, func(event *mcpv1.LoginEvent) tea.Cmd {
		return tea.Batch(s.signedIn(server.GetName(), event), s.load(""))
	})
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

// Note of how a sign in ended
func (s *servers) signedIn(name string, event *mcpv1.LoginEvent) tea.Cmd {
	switch {
	case event.GetError() != "":
		return s.hub.Fail("Couldn't sign in to " + name + ". " + strs.Full(event.GetError()))
	case event.GetNoToken():
		return s.hub.Fail("The sign-in to " + name + " ended without letting icebath in. Try again.")
	}
	return s.hub.Done("Signed in to " + name + ". It offers " + render.Count(int(event.GetTools()), "tool") + ".")
}

// Reasons as sentences, one after another
func sentences(reasons []string) string {
	out := make([]string, len(reasons))
	for i, reason := range reasons {
		out[i] = strs.Full(reason)
	}
	return strings.Join(out, " ")
}

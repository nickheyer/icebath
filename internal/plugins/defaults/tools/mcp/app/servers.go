package app

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	mcpv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/mcp/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/mcp/v1/mcpv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Words the states of a server are shown by
var stateWords = map[string]string{"connected": "connected", "configured": "configured", "connecting": "connecting", "failed": "failed",
	"lazy": "on first use", "disabled": "off", "recycled": "recycled"}

// MCP servers screen: every server of the profile with its state, added from the catalog or by hand, turned on or off, tested, signed in
// to, its tools picked and removed, all inside the screen
type servers struct {
	kit     *tui.Kit
	service mcpv1connect.McpServiceClient
	profile string
	hub     *cards.Hub
	list    *cards.List
	rows    []*mcpv1.Server
	loaded  bool
	failed  error
}

func (s *servers) open(argument string) tea.Cmd {
	t := s.kit.Env.Theme
	s.list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   title(s.profile),
		Glyph:   t.Glyphs.MCP,
		Key:     Screen,
		Context: serversContext,
		Shared:  []string{cards.HubContext},
		Primary: "hub.use",
		Columns: []render.Column{{}, {Title: "Server", Max: 30}, {Title: "Transport", Max: 10}, {Title: "Status", Max: 14}, {Title: "Tools", Right: true},
			{Title: "Where", Flex: 1}},
		Yield: []int{5, 2},
		Rows:  s.cells,
		Detail: func(i int) []string {
			if server := s.at(i); server != nil {
				return s.detail(server)
			}
			return nil
		},
		Empty: func() string {
			return tui.Waiting(s.loaded, s.failed, "No MCP servers yet. Press "+s.kit.Env.Keys.Display("hub.add")+" to add one")
		},
		Free: map[string]bool{"hub.add": true, "hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			server := s.at(i)
			if server == nil {
				return []wm.Hint{{Action: "hub.add"}}
			}
			hints := []wm.Hint{{Action: "hub.use", Label: "open"}, {Action: "mcp.toggle"}, {Action: "mcp.test"}}
			if server.GetOauth() {
				hints = append(hints, wm.Hint{Action: "mcp.signin"})
			}
			hints = append(hints, wm.Hint{Action: "hub.edit", Label: "tools"}, wm.Hint{Action: "hub.add"})
			if server.GetPlugin() == "" {
				hints = append(hints, wm.Hint{Action: "hub.remove"})
			}
			return hints
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use":     s.with(s.page),
			"mcp.toggle":  s.with(s.toggle),
			"mcp.test":    s.with(s.test),
			"mcp.signin":  s.with(s.signin),
			"hub.edit":    s.with(s.tools),
			"hub.remove":  func(l *cards.List, i int) tea.Cmd { return s.remove(l, s.at(i), false) },
			"hub.add":     func(*cards.List, int) tea.Cmd { return s.add() },
			"hub.refresh": func(*cards.List, int) tea.Cmd { return s.load("") },
		},
	})
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
	return "MCP servers of " + profile
}

func (s *servers) at(i int) *mcpv1.Server {
	if i < 0 || i >= len(s.rows) {
		return nil
	}
	return s.rows[i]
}

// Action on the server of the row under the cursor
func (s *servers) with(act func(server *mcpv1.Server) tea.Cmd) func(*cards.List, int) tea.Cmd {
	return func(_ *cards.List, i int) tea.Cmd {
		if server := s.at(i); server != nil {
			return act(server)
		}
		return nil
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
			for i, server := range s.rows {
				if server.GetName() == focus {
					s.list.Select(i)
					return s.page(server)
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

func (s *servers) cells() [][]string {
	t := s.kit.Env.Theme
	out := make([][]string, len(s.rows))
	for i, server := range s.rows {
		mark := t.Glyphs.Hollow
		if server.GetEnabled() {
			mark = t.Glyphs.Live
		}
		count := ""
		if server.GetTools() > 0 {
			count = strconv.Itoa(int(server.GetTools()))
		}
		state := cmp.Or(stateWords[server.GetState()], server.GetState())
		switch server.GetState() {
		case "connected":
			state = render.Good(state, t)
		case "failed":
			state = render.Warm(state, t)
		}
		cells := []string{mark, server.GetName(), server.GetTransport(), state, count, cmp.Or(server.GetUrl(), server.GetCommand())}
		if !server.GetEnabled() {
			for c := range cells {
				cells[c] = render.Dim(cells[c], t)
			}
		}
		out[i] = cells
	}
	return out
}

// Lines of a server's detail pane: how it connects, what it reads, how it signs in and where it comes from
func (s *servers) detail(server *mcpv1.Server) []string {
	t := s.kit.Env.Theme
	out := []string{server.GetName() + " · " + server.GetTransport() + " · " + cmp.Or(stateWords[server.GetState()], server.GetState())}
	if e := server.GetError(); e != "" {
		out = append(out, render.Warm(e, t))
	}
	if u := server.GetUrl(); u != "" {
		out = append(out, "URL: "+u)
	}
	if c := server.GetCommand(); c != "" {
		out = append(out, "Command: "+strings.Join(append([]string{c}, server.GetArgs()...), " "))
	}
	if env := server.GetEnv(); len(env) > 0 {
		out = append(out, "Environment: "+strings.Join(env, ", "))
	}
	if missing := server.GetMissing(); len(missing) > 0 {
		out = append(out, render.Warm("Not set: "+strings.Join(missing, ", "), t))
	}
	switch {
	case server.GetIncluded():
		out = append(out, fmt.Sprintf("Tools: only %s", strings.Join(server.GetInclude(), ", ")))
	case len(server.GetExclude()) > 0:
		out = append(out, fmt.Sprintf("Tools: all but %s", strings.Join(server.GetExclude(), ", ")))
	}
	if server.GetOauth() {
		if server.GetSignedIn() {
			out = append(out, "Signed in")
		} else {
			out = append(out, render.Warm("Not signed in", t))
		}
	}
	for _, kind := range []struct {
		name   string
		counts map[string]int32
	}{{"Sampling", server.GetSampling()}, {"Questions", server.GetElicitation()}} {
		if len(kind.counts) == 0 {
			continue
		}
		var parts []string
		for _, key := range slices.Sorted(func(yield func(string) bool) {
			for k := range kind.counts {
				if !yield(k) {
					return
				}
			}
		}) {
			parts = append(parts, fmt.Sprintf("%s %d", strings.ReplaceAll(key, "_", " "), kind.counts[key]))
		}
		out = append(out, render.Dim(kind.name+": "+strings.Join(parts, ", "), t))
	}
	if plugin := server.GetPlugin(); plugin != "" {
		out = append(out, render.Dim("From the plugin "+plugin, t))
	}
	return out
}

func (s *servers) toggle(server *mcpv1.Server) tea.Cmd {
	enabled := !server.GetEnabled()
	return tui.Call(s.hub, func() (*mcpv1.EnableResponse, error) {
		return s.service.Enable(s.kit.Env.Ctx, &mcpv1.EnableRequest{Profile: s.profile, Name: server.GetName(), Enabled: enabled})
	}, func(*mcpv1.EnableResponse) tea.Cmd {
		word := "Turned off "
		if enabled {
			word = "Turned on "
		}
		return tea.Batch(s.hub.Done(word+server.GetName()+". /reload-mcp loads the change into open sessions"), s.load(""))
	})
}

func (s *servers) test(server *mcpv1.Server) tea.Cmd {
	s.hub.Busy("Testing " + server.GetName() + "…")
	return tui.Call(s.hub, func() (*mcpv1.ProbeResponse, error) {
		return s.service.Probe(s.kit.Env.Ctx, &mcpv1.ProbeRequest{Profile: s.profile, Name: server.GetName()})
	}, func(r *mcpv1.ProbeResponse) tea.Cmd {
		if !r.GetOk() {
			return s.hub.Fail(server.GetName() + " failed: " + r.GetError() + ". " + r.GetNextStep())
		}
		return s.hub.Done(fmt.Sprintf("%s connected with %s", server.GetName(), render.Count(len(r.GetTools()), "tool")))
	})
}

func (s *servers) remove(l *cards.List, server *mcpv1.Server, pop bool) tea.Cmd {
	if server == nil {
		return nil
	}
	if plugin := server.GetPlugin(); plugin != "" {
		return s.hub.Fail(server.GetName() + " comes from the plugin " + plugin + ": turn it off instead")
	}
	run := func() tea.Cmd {
		return tui.Call(s.hub, func() (*mcpv1.RemoveResponse, error) {
			return s.service.Remove(s.kit.Env.Ctx, &mcpv1.RemoveRequest{Profile: s.profile, Name: server.GetName(), Purge: true})
		}, func(*mcpv1.RemoveResponse) tea.Cmd {
			cmds := []tea.Cmd{s.hub.Done("Removed " + server.GetName()), s.load("")}
			if pop {
				cmds = append(cmds, s.hub.PopTo(s.list))
			}
			return tea.Batch(cmds...)
		})
	}
	l.Confirm("Remove "+server.GetName()+", its sign in and any files it cloned?", run)
	return nil
}

// Page of one server: what can be done to it, each with what it does
func (s *servers) page(server *mcpv1.Server) tea.Cmd {
	type act struct {
		title, does string
		run         func(l *cards.List) tea.Cmd
	}
	acts := func() []act {
		current := cmp.Or(s.find(server.GetName()), server)
		toggle := "Turn on"
		if current.GetEnabled() {
			toggle = "Turn off"
		}
		out := []act{
			{toggle, "Load it into new sessions, or stop loading it", func(*cards.List) tea.Cmd { return s.toggle(current) }},
			{"Test", "Connect to it once and count its tools", func(*cards.List) tea.Cmd { return s.test(current) }},
			{"Tools", "Pick the tools the agent may use", func(*cards.List) tea.Cmd { return s.tools(current) }},
		}
		if current.GetOauth() {
			out = append(out, act{"Sign in", "Sign in again in the browser of this computer", func(*cards.List) tea.Cmd { return s.signin(current) }})
		}
		if names := slices.Concat(current.GetMissing(), current.GetEnv()); len(names) > 0 || strings.Contains(current.GetSettings(), "${") {
			out = append(out, act{"Keys", "Set the values its settings read", func(*cards.List) tea.Cmd { return s.values(current) }})
		}
		if current.GetPlugin() == "" {
			out = append(out, act{"Remove", "Remove it, its sign in and any files it cloned", func(l *cards.List) tea.Cmd { return s.remove(l, current, true) }})
		}
		return out
	}
	var rows []act
	var list *cards.List
	list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   server.GetName(),
		Glyph:   s.kit.Env.Theme.Glyphs.MCP,
		Key:     Screen + ":" + server.GetName(),
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Columns: []render.Column{{Max: 12}, {Flex: 1}},
		Rows: func() [][]string {
			rows = acts()
			cells := make([][]string, len(rows))
			for i, row := range rows {
				cells[i] = []string{row.title, render.Dim(row.does, s.kit.Env.Theme)}
			}
			return cells
		},
		Banner: func() string { return strings.Join(s.detail(cmp.Or(s.find(server.GetName()), server)), "\n") },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: "run"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				if i < 0 || i >= len(rows) {
					return nil
				}
				return rows[i].run(l)
			},
		},
	})
	return s.hub.Push(list)
}

// Asks the values the server's settings read, masked, one form for all of them
func (s *servers) values(server *mcpv1.Server) tea.Cmd {
	names := slices.Concat(server.GetMissing(), refs(server.GetSettings()))
	slices.Sort(names)
	names = slices.Compact(names)
	if len(names) == 0 {
		return s.hub.Say(server.GetName()+" reads no values", cards.NoteInfo)
	}
	fields := make([]cards.Field, len(names))
	for i, name := range names {
		help := "Kept in the profile's MCP values; empty keeps what is set"
		if slices.Contains(server.GetMissing(), name) {
			help = "Not set yet"
		}
		fields[i] = cards.Field{ID: name, Label: name, Kind: cards.FieldSecret, Help: help}
	}
	var form *cards.Form
	form = cards.NewForm(s.kit.Env, cards.FormSpec{
		Title:  "Keys of " + server.GetName(),
		Fields: fields,
		Submit: "Save",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
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
				return tea.Batch(s.hub.Close(form), s.hub.Done(fmt.Sprintf("Saved %s for %s", render.Count(saved, "value"), server.GetName())), s.load(""))
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
			return s.hub.Fail(server.GetName() + " failed: " + r.GetError())
		}
		s.hub.Quiet()
		found := r.GetTools()
		on := make([]bool, len(found))
		for i, tool := range found {
			on[i] = allowed(server, tool.GetName())
		}
		t := s.kit.Env.Theme
		var list *cards.List
		save := func() tea.Cmd {
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
				return tea.Batch(s.hub.Done(fmt.Sprintf("%d/%d tools on. /reload-mcp loads the change into open sessions", len(picked), len(found))), s.load(""))
			})
		}
		toggle := func(_ *cards.List, i int) tea.Cmd {
			if i < 0 || i >= len(found) {
				return nil
			}
			on[i] = !on[i]
			return save()
		}
		list = cards.NewList(s.kit.Env, cards.ListSpec{
			Title:   "Tools of " + server.GetName(),
			Glyph:   t.Glyphs.Tools,
			Key:     Screen + ":" + server.GetName() + ":tools",
			Context: serversContext,
			Shared:  []string{cards.HubContext},
			Primary: "mcp.toggle",
			Search:  "Search tools",
			Columns: []render.Column{{}, {Title: "Tool", Max: 36}, {Title: "What it does", Flex: 1}},
			Rows: func() [][]string {
				cells := make([][]string, len(found))
				for i, tool := range found {
					mark := t.Glyphs.Hollow
					if on[i] {
						mark = t.Glyphs.Live
					}
					cells[i] = []string{mark, tool.GetName(), render.Dim(strings.Join(strings.Fields(tool.GetDescription()), " "), t)}
				}
				return cells
			},
			Empty: func() string { return server.GetName() + " offers no tools" },
			Hints: func(*cards.List, int) []wm.Hint { return []wm.Hint{{Action: "mcp.toggle"}} },
			Actions: map[string]func(*cards.List, int) tea.Cmd{
				"mcp.toggle": toggle,
				"hub.use":    toggle,
			},
		})
		return s.hub.Push(list)
	})
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
		choices := []tui.Choice{{ID: "", Title: "Custom server…", Detail: "A command to run or an address to reach"}}
		for _, entry := range response.GetEntries() {
			if entry.GetInstalled() || entry.GetProblem() != "" {
				continue
			}
			entries[entry.GetName()] = entry
			choices = append(choices, tui.Choice{ID: entry.GetName(), Title: entry.GetName(), Detail: entry.GetDescription()})
		}
		return s.kit.Pick(s.hub, "Add MCP server", "add", choices, "", func(choice tui.Choice) tea.Cmd {
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
		fields[i] = cards.Field{ID: spec.GetName(), Label: cmp.Or(spec.GetPrompt(), spec.GetName()), Kind: kind, Value: spec.GetDefault(),
			Required: spec.GetRequired(), Help: spec.GetName()}
	}
	var form *cards.Form
	form = cards.NewForm(s.kit.Env, cards.FormSpec{
		Title:  "Install " + entry.GetName(),
		Intro:  []string{entry.GetDescription()},
		Fields: fields,
		Submit: "Install",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			return tea.Batch(s.hub.Close(form), s.installWith(entry, values))
		},
	})
	return s.hub.Push(form)
}

func (s *servers) installWith(entry *mcpv1.Entry, values map[string]string) tea.Cmd {
	name := entry.GetName()
	s.hub.Busy("Installing " + name + "…")
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
				return "", fmt.Errorf("%s", cloned.GetError())
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
			return "", fmt.Errorf("%s was not installed: %s", name, strings.Join(issues, "; "))
		}
		return settings.GetSettings(), nil
	}, func(settings string) tea.Cmd {
		cmds := []tea.Cmd{s.hub.Done("Installed " + name + ". /reload-mcp loads it into open sessions"), s.load(name)}
		if note := strings.TrimSpace(entry.GetPostInstall()); note != "" {
			cmds = append(cmds, s.kit.WM.Toast(note, wm.Info, 2*wm.DefaultToastTTL))
		}
		if entry.GetAuth() == "oauth" && entry.GetProvider() == "" {
			cmds = append(cmds, s.signinFlow(name, "", ""))
		}
		return tea.Batch(cmds...)
	})
}

// Form of a server described by hand: how it connects, the command or address, its arguments, its environment and a key it sends
func (s *servers) custom() tea.Cmd {
	var form *cards.Form
	form = cards.NewForm(s.kit.Env, cards.FormSpec{
		Title: "Custom MCP server",
		Fields: []cards.Field{
			{ID: "name", Label: "Name", Required: true, Help: "Lower case letters, digits, - and _", Check: func(value string) string {
				for _, r := range strings.TrimSpace(value) {
					if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
						return "Use lower case letters, digits, - and _"
					}
				}
				return ""
			}},
			{ID: "transport", Label: "Transport", Kind: cards.FieldChoice, Value: "stdio", Choices: []cards.Choice{
				{Value: "stdio", Label: "Command", Meaning: "Runs on this computer and talks over its input and output"},
				{Value: "http", Label: "Address", Meaning: "Reached over HTTP at a URL"},
				{Value: "oauth", Label: "Address with sign in", Meaning: "Reached over HTTP, signed in to in the browser"},
			}},
			{ID: "target", Label: "Command or URL", Required: true, Help: "Such as npx or https://example.com/mcp"},
			{ID: "args", Label: "Arguments", Help: "Of the command, split like a shell line"},
			{ID: "env", Label: "Environment", More: true, Help: "KEY=value pairs of the command, split by commas"},
			{ID: "token", Label: "Bearer token", Kind: cards.FieldSecret, More: true, Help: "Sent to the address, kept in the profile's MCP values"},
		},
		Submit: "Add",
		Done: func(_ *cards.Form, v map[string]string) tea.Cmd {
			name := strings.TrimSpace(v["name"])
			settings := map[string]any{"enabled": true}
			target := strings.TrimSpace(v["target"])
			if v["transport"] == "stdio" {
				settings["command"] = target
				args, err := cards.ShellWords(v["args"])
				if err != nil {
					return s.hub.Fail(err.Error())
				}
				if len(args) > 0 {
					settings["args"] = args
				}
				if env := strings.TrimSpace(v["env"]); env != "" {
					var pairs []any
					for pair := range strings.SplitSeq(env, ",") {
						key, value, ok := strings.Cut(strings.TrimSpace(pair), "=")
						if !ok || strings.TrimSpace(key) == "" {
							return s.hub.Fail("Not a KEY=value pair: " + pair)
						}
						pairs = append(pairs, strings.TrimSpace(key)+"="+value)
					}
					settings["env"] = pairs
				}
			} else {
				settings["url"] = target
				if v["transport"] == "oauth" {
					settings["auth"] = "oauth"
				}
			}
			token := strings.TrimSpace(v["token"])
			s.hub.Busy("Adding " + name + "…")
			return tui.Call(s.hub, func() (*mcpv1.SaveResponse, error) {
				ctx := s.kit.Env.Ctx
				if token != "" && v["transport"] != "stdio" {
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
					return s.hub.Fail(name + " was not saved: " + strings.Join(issues, "; "))
				}
				cmds := []tea.Cmd{s.hub.Close(form), s.hub.Done("Added " + name + ". /reload-mcp loads it into open sessions"), s.load(name)}
				if v["transport"] == "oauth" {
					cmds = append(cmds, s.signinFlow(name, "", ""))
				}
				return tea.Batch(cmds...)
			})
		},
	})
	return s.hub.Push(form)
}

func (s *servers) signin(server *mcpv1.Server) tea.Cmd {
	return s.signinFlow(server.GetName(), "", "")
}

// Signs in to a server on a page of the hub: the link opened in this computer's browser, copied or the address the browser ended on pasted,
// until the server approves it or it fails, closing the page stopping it
func (s *servers) signinFlow(name, flow, settings string) tea.Cmd {
	return signin(s.kit, s.hub, s.service, &mcpv1.LoginRequest{Profile: s.profile, Name: name, Flow: flow, Settings: settings}, func(event *mcpv1.LoginEvent) tea.Cmd {
		if event.GetError() != "" {
			return s.hub.Fail("Sign in to " + name + " failed: " + event.GetError())
		}
		return tea.Batch(s.hub.Done(fmt.Sprintf("Signed in to %s, %s", name, render.Count(int(event.GetTools()), "tool"))), s.load(""))
	})
}

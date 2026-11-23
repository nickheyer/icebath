package tui

import (
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the MCP servers screen
const mcpContext = "tui.mcp"

// Interval between two reads of an open browser sign in
var mcpPollEvery = 2 * time.Second

// Keys of the MCP servers screen beyond the shared hub keys
func registerMCPKeys(km *keys.Map) {
	if keys.ContextDocs[mcpContext] == "" {
		if err := keys.AddContext(mcpContext, "MCP servers screen"); err != nil {
			panic(err)
		}
	}
	km.Register(keys.Action{ID: "tui.mcp.test", Context: mcpContext, Label: "test", Doc: "connect to the MCP server and count its tools", Default: []string{"T"}})
	km.Register(keys.Action{ID: "tui.mcp.signin", Context: mcpContext, Label: "sign in", Doc: "sign in to the MCP server in the browser of this computer", Default: []string{"s"}})
}

func init() {
	hubRegistry = append(hubRegistry, hubDef{id: "mcp", order: 50, title: txt.HubsTitleMcp, short: txt.HubsShortMcp, slash: "mcp",
		open: func(a *app, _ string) tea.Cmd { return a.openMCPServers() }, keys: registerMCPKeys})
}

// One configured MCP server joined with its runtime state
type mcpServer struct {
	tuigateway.MCPServerSummary
	Status string
	Tools  int
	OAuth  bool
}

// Servers of an mcp.servers.list answer with the states of an mcp.servers.status answer, sorted by name
func mcpServersOf(list tuigateway.MCPServersListResult, status tuigateway.MCPServersStatusResult) []mcpServer {
	states := map[string]tuigateway.MCPServerRuntimeRow{}
	for _, row := range status.Servers {
		states[row.Name] = row
	}
	out := make([]mcpServer, 0, len(list.Servers))
	for _, row := range list.Servers {
		server := mcpServer{MCPServerSummary: row, OAuth: val(row.Auth) == "oauth", Status: "configured"}
		if !row.Enabled {
			server.Status = "disabled"
		}
		if state, seen := states[row.Name]; seen {
			server.Status, server.Tools = string(state.Status), int(state.Tools)
		}
		out = append(out, server)
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// Whether the server's tool filter lets the agent use the tool, every tool when it names none
func mcpToolAllowed(filter any, tool string) bool {
	m, ok := filter.(map[string]any)
	if !ok {
		return true
	}
	if include, ok := m["include"].([]any); ok {
		return slices.ContainsFunc(include, func(v any) bool { s, _ := v.(string); return s == tool })
	}
	if exclude, ok := m["exclude"].([]any); ok {
		return !slices.ContainsFunc(exclude, func(v any) bool { s, _ := v.(string); return s == tool })
	}
	return true
}

// MCP servers screen: every configured server with its state, added from the catalog or by hand, turned on or off, tested, signed in to,
// its tools chosen and removed, all inside the screen
func (a *app) openMCPServers() tea.Cmd {
	var (
		servers []mcpServer
		h       *cards.Hub
		list    *cards.List
		loaded  bool
	)
	reload := func() tea.Cmd {
		return hubCall(a, h, "", tuigateway.MCPServersList, tuigateway.ProfileParams{}, func(l *tuigateway.MCPServersListResult) tea.Cmd {
			return hubCall(a, h, "", tuigateway.MCPServersStatus, tuigateway.ProfileParams{}, func(s *tuigateway.MCPServersStatusResult) tea.Cmd {
				servers, loaded = mcpServersOf(*l, *s), true
				list.Refresh()
				return nil
			})
		})
	}
	at := func(i int) (mcpServer, bool) {
		if i < 0 || i >= len(servers) {
			return mcpServer{}, false
		}
		return servers[i], true
	}
	toggle := func(_ *cards.List, i int) tea.Cmd {
		s, ok := at(i)
		if !ok {
			return nil
		}
		done := tr(txt.McpNoticeEnabled, s.Name)
		if s.Enabled {
			done = tr(txt.McpNoticeDisabled, s.Name)
		}
		return hubCall(a, h, "", tuigateway.MCPServersEnable, tuigateway.MCPServersEnableParams{Name: s.Name, Enabled: !s.Enabled}, func(*tuigateway.MCPServerResult) tea.Cmd {
			return tea.Batch(h.Done(done), reload())
		})
	}
	test := func(_ *cards.List, i int) tea.Cmd {
		s, ok := at(i)
		if !ok {
			return nil
		}
		return hubCall(a, h, tr(txt.McpTesting, s.Name), tuigateway.MCPServersTest, tuigateway.MCPServerNameParams{Name: s.Name}, func(r *tuigateway.MCPServersTestResult) tea.Cmd {
			if !r.OK {
				return h.Fail(tr(txt.McpNoticeTestFailed, s.Name, val(r.Error)))
			}
			return h.Done(tr(txt.McpNoticeTestOk, s.Name, float64(len(r.Tools))))
		})
	}
	remove := func(l *cards.List, i int) tea.Cmd {
		s, ok := at(i)
		if !ok {
			return nil
		}
		if s.Source == "plugin" {
			return h.Fail(tr(txt.McpFromPlugin, s.Name, val(s.Plugin)))
		}
		l.Confirm(tr(txt.McpRemoveConfirm, s.Name), func() tea.Cmd {
			return hubCall(a, h, "", tuigateway.MCPServersRemove, tuigateway.MCPServerNameParams{Name: s.Name}, func(*tuigateway.MCPServersRemoveResult) tea.Cmd {
				return tea.Batch(h.Done(tr(txt.McpNoticeRemoved, s.Name)), reload())
			})
		})
		return nil
	}
	signIn := func(_ *cards.List, i int) tea.Cmd {
		s, ok := at(i)
		if !ok {
			return nil
		}
		return a.mcpSignIn(h, s.Name, reload)
	}
	tools := func(_ *cards.List, i int) tea.Cmd {
		s, ok := at(i)
		if !ok {
			return nil
		}
		return a.mcpTools(h, s, reload)
	}
	open := func(_ *cards.List, i int) tea.Cmd {
		s, ok := at(i)
		if !ok {
			return nil
		}
		return a.hubActions(h, s.Name, func() []string { return mcpDetail(s) }, func() []hubAction {
			acts := []hubAction{
				{key: "hub.toggle", label: tr(txt.McpActToggle), does: tr(txt.McpActToggleDoes), run: func() tea.Cmd { return toggle(list, i) }},
				{key: "tui.mcp.test", label: tr(txt.McpActTest), does: tr(txt.McpActTestDoes), run: func() tea.Cmd { return test(list, i) }},
				{key: "hub.edit", label: tr(txt.McpActTools), does: tr(txt.McpActToolsDoes), run: func() tea.Cmd { return tools(list, i) }},
			}
			if s.OAuth {
				acts = append(acts, hubAction{key: "tui.mcp.signin", label: tr(txt.McpActSignIn), does: tr(txt.McpActSignInDoes), run: func() tea.Cmd { return signIn(list, i) }})
			}
			if len(s.Env) > 0 {
				acts = append(acts, hubAction{label: tr(txt.McpActKey), does: tr(txt.McpActKeyDoes), run: func() tea.Cmd { return a.mcpAskValues(h, s.Name, s.Env, reload) }})
			}
			if s.Source != "plugin" {
				acts = append(acts, hubAction{key: "hub.remove", label: tr(txt.McpActRemove), does: tr(txt.McpActRemoveDoes), confirm: tr(txt.McpRemoveConfirm, s.Name), run: func() tea.Cmd {
					return hubCall(a, h, "", tuigateway.MCPServersRemove, tuigateway.MCPServerNameParams{Name: s.Name}, func(*tuigateway.MCPServersRemoveResult) tea.Cmd {
						return tea.Batch(h.Pop(), h.Done(tr(txt.McpNoticeRemoved, s.Name)), reload())
					})
				}})
			}
			return acts
		})
	}
	t := a.toolkit
	list = cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: tr(txt.McpTitle), Glyph: t.Glyphs.MCP, Key: "tui:mcp", Context: mcpContext, Shared: []string{hubContext}, Primary: "hub.use",
		Columns: []render.Column{{Title: tr(txt.HubsColOn)}, {Title: tr(txt.HubsColServer), Max: 30}, {Title: tr(txt.HubsColTransport), Max: 10},
			{Title: tr(txt.HubsColStatus), Max: 14}, {Title: tr(txt.HubsColTools), Right: true}, {Title: tr(txt.HubsColWhere), Flex: 1}},
		Yield: []int{5, 2},
		Rows: func() [][]string {
			out := make([][]string, len(servers))
			for i, s := range servers {
				mark := "○"
				if s.Enabled {
					mark = "●"
				}
				count := ""
				if s.Tools > 0 {
					count = strconv.Itoa(s.Tools)
				}
				cells := []string{mark, s.Name, s.Transport, trOr("mcp.status."+s.Status, s.Status), count, firstText(val(s.URL), val(s.Command))}
				if !s.Enabled {
					for c := range cells {
						cells[c] = render.Dim(cells[c], t)
					}
				}
				out[i] = cells
			}
			return out
		},
		Detail: func(i int) []string {
			s, ok := at(i)
			if !ok {
				return nil
			}
			return mcpDetail(s)
		},
		Help: func(i int) []string {
			out := []string{tr(txt.HubsShortMcp)}
			if s, ok := at(i); ok {
				out = append(append(out, ""), mcpDetail(s)...)
			}
			return out
		},
		Empty: func() string {
			if !loaded {
				return tr(txt.HubsLoading)
			}
			return tr(txt.McpEmpty, a.keys.Display("hub.add"))
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			s, ok := at(i)
			if !ok {
				return []wm.Hint{{Action: "hub.add"}}
			}
			out := []wm.Hint{{Action: "hub.use", Label: tr(txt.HintsLabelOpen)}, {Action: "hub.toggle"}, {Action: "tui.mcp.test"}}
			if s.OAuth {
				out = append(out, wm.Hint{Action: "tui.mcp.signin"})
			}
			return append(out, wm.Hint{Action: "hub.edit", Label: tr(txt.McpToolsLabel)}, wm.Hint{Action: "hub.add"}, wm.Hint{Action: "hub.remove"})
		},
		Free: map[string]bool{"hub.add": true, "hub.refresh": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.use":        open,
			"hub.toggle":     toggle,
			"tui.mcp.test":   test,
			"tui.mcp.signin": signIn,
			"hub.edit":       tools,
			"hub.remove":     remove,
			"hub.add":        func(*cards.List, int) tea.Cmd { return a.addMCPServer(h, reload) },
			"hub.refresh":    func(*cards.List, int) tea.Cmd { return reload() },
		},
	})
	h = a.newHub("tui:mcp", list)
	return tea.Batch(a.wm.ShowOverlay(h), reload())
}

// Lines of a server's detail pane: how it connects, what it reads and where it comes from
func mcpDetail(s mcpServer) []string {
	out := []string{s.Name + " · " + s.Transport + " · " + trOr("mcp.status."+s.Status, s.Status)}
	if u := val(s.URL); u != "" {
		out = append(out, tr(txt.McpDetailURL, u))
	}
	if c := val(s.Command); c != "" {
		out = append(out, tr(txt.McpDetailCommand, strings.Join(append([]string{c}, s.Args...), " ")))
	}
	if len(s.Env) > 0 {
		out = append(out, tr(txt.McpDetailEnv, strings.Join(s.Env, ", ")))
	}
	if s.OAuth {
		signed := tr(txt.McpDetailSignedOut)
		if val(s.OAuthTokensPresent) {
			signed = tr(txt.McpDetailSignedIn)
		}
		out = append(out, signed)
	}
	if s.Source == "plugin" {
		out = append(out, tr(txt.McpDetailPlugin, val(s.Plugin)))
	}
	return out
}

// Adds a server: one of the catalog's, each value it needs asked after, or one described by hand
func (a *app) addMCPServer(h *cards.Hub, done func() tea.Cmd) tea.Cmd {
	return hubCall(a, h, tr(txt.HubsLoading), tuigateway.MCPCatalog, tuigateway.ProfileParams{}, func(res *tuigateway.MCPCatalogResult) tea.Cmd {
		choices := []hubChoice{{id: "", title: tr(txt.McpAddCustom), detail: tr(txt.McpAddCustomDetail)}}
		requires := map[string][]string{}
		for _, row := range res.Servers {
			if row.Installed || row.Name == "" {
				continue
			}
			requires[row.Name] = row.Requires
			choices = append(choices, hubChoice{id: row.Name, title: row.Name, detail: row.Description})
		}
		return a.hubPick(h, tr(txt.McpAddPick), choices, "", func(c hubChoice) tea.Cmd {
			if c.id == "" {
				return a.addCustomMCPServer(h, done)
			}
			name := c.id
			return hubCall(a, h, tr(txt.McpAdding, name), tuigateway.MCPServersAdd, tuigateway.MCPServersAddParams{Name: name, Preset: &name}, func(*tuigateway.MCPServerResult) tea.Cmd {
				return tea.Batch(h.Done(tr(txt.McpNoticeAdded, name)), done(), a.mcpAskValues(h, name, requires[name], done))
			})
		})
	})
}

// Form of a server described by hand: how it connects, the command or address, its arguments, its environment and a header for its key
func (a *app) addCustomMCPServer(h *cards.Hub, done func() tea.Cmd) tea.Cmd {
	spec := cards.FormSpec{Title: tr(txt.McpCustomTitle), Submit: tr(txt.HubsLoginsAddSubmit), Fields: []cards.Field{
		{ID: "name", Label: tr(txt.HubsLoginsNameLabel), Kind: cards.FieldText, Required: true},
		{ID: "transport", Label: tr(txt.HubsColTransport), Kind: cards.FieldChoice, Value: "stdio", Choices: []cards.Choice{
			{Value: "stdio", Label: tr(txt.McpTransportStdio), Meaning: tr(txt.McpTransportStdioMeaning)},
			{Value: "http", Label: tr(txt.McpTransportHTTP), Meaning: tr(txt.McpTransportHTTPMeaning)}}},
		{ID: "target", Label: tr(txt.McpTargetLabel), Kind: cards.FieldText, Required: true, Help: tr(txt.McpTargetHelp)},
		{ID: "args", Label: tr(txt.McpArgsLabel), Kind: cards.FieldText, Help: tr(txt.McpArgsHelp)},
		{ID: "env", Label: tr(txt.McpEnvLabel), Kind: cards.FieldText, More: true, Help: tr(txt.McpEnvHelp)},
		{ID: "token", Label: tr(txt.McpTokenLabel), Kind: cards.FieldSecret, More: true, Help: tr(txt.McpTokenHelp)},
	}}
	return a.hubForm(h, spec, func(_ *cards.Form, v map[string]string) tea.Cmd {
		name := strings.TrimSpace(v["name"])
		config := map[string]any{}
		if v["transport"] == "http" {
			config["url"] = strings.TrimSpace(v["target"])
		} else {
			config["command"] = strings.TrimSpace(v["target"])
			if args, err := cards.ShellWords(v["args"]); err == nil && len(args) > 0 {
				config["args"] = args
			} else if err != nil {
				return h.Fail(err.Error())
			}
		}
		if env := strings.TrimSpace(v["env"]); env != "" {
			vars := map[string]any{}
			for _, pair := range strings.Split(env, ",") {
				key, value, ok := strings.Cut(strings.TrimSpace(pair), "=")
				if !ok || key == "" {
					return h.Fail(tr(txt.McpEnvBad, pair))
				}
				vars[key] = value
			}
			config["env"] = vars
		}
		params := tuigateway.MCPServersAddParams{Name: name, Config: config}
		if token := strings.TrimSpace(v["token"]); token != "" {
			params.BearerToken = &token
		}
		return hubCall(a, h, tr(txt.McpAdding, name), tuigateway.MCPServersAdd, params, func(*tuigateway.MCPServerResult) tea.Cmd {
			return tea.Batch(h.Done(tr(txt.McpNoticeAdded, name)), done())
		})
	})
}

// Asks each value a server reads from the environment, masked, one form for all of them
func (a *app) mcpAskValues(h *cards.Hub, name string, envVars []string, done func() tea.Cmd) tea.Cmd {
	if len(envVars) == 0 {
		return nil
	}
	var fields []cards.Field
	for _, env := range envVars {
		fields = append(fields, cards.Field{ID: env, Label: env, Kind: cards.FieldSecret, Help: tr(txt.McpValueHelp)})
	}
	return a.hubForm(h, cards.FormSpec{Title: tr(txt.McpValuesTitle, name), Fields: fields, Submit: tr(txt.HubsSettingsSave)}, func(_ *cards.Form, v map[string]string) tea.Cmd {
		var cmds []tea.Cmd
		for _, env := range envVars {
			value := strings.TrimSpace(v[env])
			if value == "" {
				continue
			}
			key := env
			cmds = append(cmds, hubCall(a, h, "", tuigateway.MCPServersSetAPIKey, tuigateway.MCPServersSetAPIKeyParams{Name: name, Value: value, EnvVar: &key},
				func(*tuigateway.MCPServersSetAPIKeyResult) tea.Cmd {
					return h.Done(tr(txt.McpNoticeKeySaved, key, name))
				}))
		}
		return tea.Batch(append(cmds, done())...)
	})
}

// Tools of a server as a checklist: the server is asked for its tools, and each one turned on or off for new sessions
func (a *app) mcpTools(h *cards.Hub, s mcpServer, done func() tea.Cmd) tea.Cmd {
	return hubCall(a, h, tr(txt.McpTesting, s.Name), tuigateway.MCPServersTest, tuigateway.MCPServerNameParams{Name: s.Name}, func(r *tuigateway.MCPServersTestResult) tea.Cmd {
		if !r.OK {
			return h.Fail(tr(txt.McpNoticeTestFailed, s.Name, val(r.Error)))
		}
		tools := r.Tools
		on := make([]bool, len(tools))
		for i, tool := range tools {
			on[i] = mcpToolAllowed(s.MCPServerSummary.Tools, tool.Name)
		}
		var list *cards.List
		toggle := func(_ *cards.List, i int) tea.Cmd {
			if i < 0 || i >= len(tools) {
				return nil
			}
			action := tuigateway.ToolsAction("disable")
			if !on[i] {
				action = "enable"
			}
			params := tuigateway.ToolsConfigureParams{Action: action, Names: []string{s.Name + ":" + tools[i].Name}}
			return hubCall(a, h, "", tuigateway.ToolsConfigure, params, func(*tuigateway.ToolsConfigureResult) tea.Cmd {
				on[i] = !on[i]
				list.Refresh()
				return tea.Batch(h.Done(tr(txt.McpToolsSaved)), done())
			})
		}
		list = cards.NewList(a.hubEnv(), cards.ListSpec{
			Title: tr(txt.McpToolsTitle, s.Name), Key: "tui:mcp:tools", Context: mcpContext, Shared: []string{hubContext}, Primary: "hub.toggle",
			Columns: []render.Column{{Title: tr(txt.HubsColOn)}, {Title: tr(txt.HubsColTools), Max: 36}, {Title: tr(txt.HubsColWhatItDoes), Flex: 1}},
			Rows: func() [][]string {
				out := make([][]string, len(tools))
				for i, tool := range tools {
					mark := "○"
					if on[i] {
						mark = "●"
					}
					out[i] = []string{mark, tool.Name, strings.Join(strings.Fields(tool.Description), " ")}
				}
				return out
			},
			Empty:   func() string { return tr(txt.McpToolsEmpty) },
			Hints:   func(*cards.List, int) []wm.Hint { return []wm.Hint{{Action: "hub.toggle"}} },
			Help:    func(int) []string { return []string{tr(txt.McpToolsHelp)} },
			Actions: map[string]func(l *cards.List, i int) tea.Cmd{"hub.toggle": toggle, "hub.use": toggle},
		})
		return h.Push(list)
	})
}

// Signs in to a server on a page of the hub: the link opened in this computer's browser and copied on request, read again until the
// server approves it or it fails, closing the page stopping it
func (a *app) mcpSignIn(h *cards.Hub, name string, done func() tea.Cmd) tea.Cmd {
	return hubCall(a, h, tr(txt.McpAuthStarting), tuigateway.MCPServersOAuthStart, tuigateway.MCPOAuthStartParams{Name: name}, func(res *tuigateway.MCPOAuthStartResult) tea.Cmd {
		link, flow, finished := res.AuthURL, res.SessionID, false
		var page *cards.Page
		cancel := func() tea.Cmd {
			finished = true
			return call(a.gw, tuigateway.MCPServersOAuthCancel, tuigateway.MCPOAuthFlowParams{Name: name, SessionID: flow}, func(_ *tuigateway.MCPOAuthCancelResult, err error) tea.Cmd {
				if err != nil {
					return h.Fail(rpcErrorMessage(err))
				}
				return h.Done(tr(txt.McpNoticeAuthStopped))
			})
		}
		var poll func() tea.Cmd
		poll = func() tea.Cmd {
			if finished {
				return nil
			}
			if !h.Holds(page) || !h.Shown() {
				return cancel()
			}
			return call(a.gw, tuigateway.MCPServersOAuthPoll, tuigateway.MCPOAuthFlowParams{Name: name, SessionID: flow}, func(r *tuigateway.MCPOAuthPollResult, err error) tea.Cmd {
				if finished {
					return nil
				}
				if err != nil {
					finished = true
					return tea.Batch(h.Close(page), h.Fail(rpcErrorMessage(err)))
				}
				if u := val(r.AuthURL); u != "" {
					link = u
				}
				switch r.Status {
				case "approved":
					finished = true
					return tea.Batch(h.Close(page), h.Done(tr(txt.McpNoticeAuthorized, name)), done())
				case "error":
					finished = true
					return tea.Batch(h.Close(page), h.Fail(tr(txt.McpNoticeAuthFailed, name, val(r.ErrorMessage))))
				}
				return tea.Tick(mcpPollEvery, func(time.Time) tea.Msg { return asyncDoneMsg{run: poll} })
			})
		}
		t := a.toolkit
		page = cards.NewPage(a.hubEnv(), cards.PageSpec{Title: tr(txt.McpAuthTitle, name), Key: "tui:mcp:signin", Context: signInContext,
			Lines: func(int) []string {
				out := []string{render.Heading(tr(txt.McpAuthTitle, name), t), ""}
				if link == "" {
					return append(out, render.Dim(tr(txt.McpAuthStarting), t))
				}
				return append(out, tr(txt.HubsLoginsSignInBrowser), render.Warm(link, t), "", render.Dim(tr(txt.McpAuthWaiting), t))
			},
			Hints: func(*cards.Page) []wm.Hint {
				return []wm.Hint{{Action: "tui.signin.open"}, {Action: "tui.signin.copy"}}
			},
			Actions: map[string]func(p *cards.Page) tea.Cmd{
				"tui.signin.open": func(*cards.Page) tea.Cmd {
					if link != "" && !openExternalURL(link) {
						return h.Fail(tr(txt.McpNoticeBrowserDidNotOpen))
					}
					return nil
				},
				"tui.signin.copy": func(*cards.Page) tea.Cmd {
					if link == "" {
						return nil
					}
					return a.hubCopy(h, link, tr(txt.HubsLoginsWhatLink))
				},
			},
		})
		cmds := []tea.Cmd{h.Push(page), tea.Tick(mcpPollEvery, func(time.Time) tea.Msg { return asyncDoneMsg{run: poll} })}
		if link != "" && !openExternalURL(link) {
			cmds = append(cmds, h.Fail(tr(txt.McpNoticeBrowserDidNotOpen)))
		}
		return tea.Batch(cmds...)
	})
}

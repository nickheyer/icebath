package tui

import (
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap contexts of the MCP servers panel and of its wait on a browser sign in
const (
	mcpContext     = "tui.mcp"
	mcpAuthContext = "tui.mcp.auth"
)

// Interval between two reads of an open browser sign in
var mcpPollEvery = 2 * time.Second

// One configured MCP server joined with its runtime state
type mcpServer struct {
	Name      string
	Transport string
	Status    string
	Tools     int
	Enabled   bool
	OAuth     bool
	Plugin    string
}

// Servers of an mcp.servers.list answer with the states of an mcp.servers.status answer, sorted by name
func mcpServersOf(list, status map[string]any) []mcpServer {
	states := map[string]map[string]any{}
	rows, _ := status["servers"].([]any)
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		name, _ := row["name"].(string)
		states[name] = row
	}
	configured, _ := list["servers"].([]any)
	out := make([]mcpServer, 0, len(configured))
	for _, raw := range configured {
		row, _ := raw.(map[string]any)
		text := func(m map[string]any, key string) string {
			s, _ := m[key].(string)
			return s
		}
		server := mcpServer{Name: text(row, "name"), Transport: text(row, "transport"), Enabled: row["enabled"] == true,
			OAuth: text(row, "auth") == "oauth", Plugin: text(row, "plugin"), Status: "configured"}
		if !server.Enabled {
			server.Status = "disabled"
		}
		if state, seen := states[server.Name]; seen {
			server.Status = text(state, "status")
			tools, _ := state["tools"].(float64)
			server.Tools = int(tools)
		}
		out = append(out, server)
	}
	sort.SliceStable(out, func(i, j int) bool { return localeOrder(out[i].Name, out[j].Name) < 0 })
	return out
}

// Order of two texts as a locale compare sorts them, letters before case and lower case first
func localeOrder(a, b string) int {
	if c := strings.Compare(strings.ToLower(a), strings.ToLower(b)); c != 0 {
		return c
	}
	return -strings.Compare(a, b)
}

// MCP servers panel, every configured server with add from the catalog, enable, authorize, test and remove
type mcpPanel struct {
	*cards.List
	app     *app
	servers []mcpServer
	loading bool
	busy    bool
}

// Opens the MCP servers panel and reads the servers into it
func (a *app) openMCPServers() tea.Cmd {
	p := &mcpPanel{app: a, loading: true}
	p.List = cards.NewList(a.cardEnv(), cards.ListSpec{
		Title: tr("mcp.title"), Glyph: a.toolkit.Glyphs.MCP, Key: "tui:mcp", Context: mcpContext,
		Columns: []render.Column{{Title: "server", Flex: 2, Max: 32}, {Title: "transport", Max: 10}, {Title: "status", Max: 12}, {Title: "tools", Max: 6}},
		Rows:    p.rows,
		Free:    map[string]bool{"tui.mcp.add": true},
		Hints:   p.hints,
		Empty:   p.empty,
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"tui.mcp.add":       p.add,
			"tui.mcp.toggle":    p.toggle,
			"tui.mcp.authorize": p.authorize,
			"tui.mcp.test":      p.test,
			"tui.mcp.remove":    p.remove,
		},
	})
	return tea.Batch(a.wm.ShowOverlay(p), p.load())
}

// Keeps the panel as the card the window manager holds while the list underneath takes the message
func (p *mcpPanel) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	_, cmd := p.List.Update(msg)
	return p, cmd
}

// Trailer of the panel, the load in flight
func (p *mcpPanel) Status() string {
	if p.loading {
		return tr("mcp.loading")
	}
	return ""
}

// Rows of the servers, disabled ones dimmed
func (p *mcpPanel) rows() [][]string {
	out := make([][]string, len(p.servers))
	for i, s := range p.servers {
		tools := ""
		if s.Tools > 0 {
			tools = strconv.Itoa(s.Tools)
		}
		cells := []string{s.Name, s.Transport, tr("mcp.status." + s.Status), tools}
		if !s.Enabled {
			for c := range cells {
				cells[c] = render.Dim(cells[c], p.app.toolkit)
			}
		}
		out[i] = cells
	}
	return out
}

// Line of an empty panel, the load in flight or how to add the first server
func (p *mcpPanel) empty() string {
	if p.loading {
		return tr("mcp.loading")
	}
	return tr("mcp.empty")
}

// Legend of the selected server, authorize offered only for one that signs in through the browser
func (p *mcpPanel) hints(_ *cards.List, i int) []wm.Hint {
	out := []wm.Hint{{Action: "tui.mcp.add"}}
	if i < 0 || i >= len(p.servers) {
		return out
	}
	out = append(out, wm.Hint{Action: "tui.mcp.toggle"})
	if p.servers[i].OAuth {
		out = append(out, wm.Hint{Action: "tui.mcp.authorize"})
	}
	return append(out, wm.Hint{Action: "tui.mcp.test"}, wm.Hint{Action: "tui.mcp.remove"})
}

// Reads the configured servers and their runtime states again, a failure leaving the rows as they were
func (p *mcpPanel) load() tea.Cmd {
	return gatewayCall(p.app.gw, "mcp.servers.list", map[string]any{}, func(v any, err error) tea.Cmd {
		if err != nil {
			p.loading = false
			p.List.Refresh()
			return p.fail(err)
		}
		list := asRPCResult(v)
		return gatewayCall(p.app.gw, "mcp.servers.status", map[string]any{}, func(sv any, err error) tea.Cmd {
			p.loading = false
			if err != nil {
				p.List.Refresh()
				return p.fail(err)
			}
			p.servers = mcpServersOf(list, asRPCResult(sv))
			p.List.Refresh()
			return nil
		})
	})
}

// Toast of a failed request
func (p *mcpPanel) fail(err error) tea.Cmd {
	return wm.Notify(tr("mcp.errorLine", rpcErrorMessage(err)), wm.Error)
}

// Shows the panel again unless it is up or another card replaced the card it left
func (p *mcpPanel) reshow(from wm.Card) tea.Cmd {
	if overlay := p.app.wm.Overlay(); overlay == p || (from != nil && overlay != from) {
		return nil
	}
	return p.app.wm.ShowOverlay(p)
}

// Back on the panel with a notice and the servers read again
func (p *mcpPanel) back(notice string, level wm.Level, from wm.Card) tea.Cmd {
	cmds := []tea.Cmd{p.reshow(from), p.load()}
	if notice != "" {
		cmds = append(cmds, wm.Notify(notice, level))
	}
	return tea.Batch(cmds...)
}

// Server under the cursor, false while a request runs or no row is selected
func (p *mcpPanel) selected(i int) (mcpServer, bool) {
	if p.busy || i < 0 || i >= len(p.servers) {
		return mcpServer{}, false
	}
	return p.servers[i], true
}

// Request of the panel, its answer handed on and a failure shown as a toast
func (p *mcpPanel) call(method string, params map[string]any, then func(map[string]any) tea.Cmd) tea.Cmd {
	p.busy = true
	return gatewayCall(p.app.gw, method, params, func(v any, err error) tea.Cmd {
		p.busy = false
		if err != nil {
			return p.fail(err)
		}
		return then(asRPCResult(v))
	})
}

// Lists the catalog servers not installed yet and asks which one to add
func (p *mcpPanel) add(_ *cards.List, _ int) tea.Cmd {
	if p.busy {
		return nil
	}
	return p.call("mcp.catalog", map[string]any{}, func(res map[string]any) tea.Cmd {
		rows, _ := res["servers"].([]any)
		items := []wm.Item{}
		for _, raw := range rows {
			row, _ := raw.(map[string]any)
			name, _ := row["name"].(string)
			if row["installed"] == true || name == "" {
				continue
			}
			description, _ := row["description"].(string)
			requires := rpcStrings(row["requires"])
			items = append(items, wm.Item{ID: "mcp:" + name, Title: name, Detail: description, Run: func() tea.Cmd { return p.install(name, requires) }})
		}
		if len(items) == 0 {
			return wm.Notify(tr("mcp.add.none"), wm.Info)
		}
		picker := wm.NewPalette(p.app.toolkit, items)
		picker.Placeholder = tr("mcp.add.pick")
		return p.app.wm.ShowOverlay(picker)
	})
}

// Adds the catalog server, then asks each value it requires
func (p *mcpPanel) install(name string, requires []string) tea.Cmd {
	return p.call("mcp.servers.add", map[string]any{"name": name, "preset": name}, func(map[string]any) tea.Cmd {
		return tea.Batch(wm.Notify(tr("mcp.notice.added", name), wm.Success), p.askValues(name, requires))
	})
}

// Asks the first required value of a server and stores it, then the rest, back on the panel after the last
func (p *mcpPanel) askValues(name string, requires []string) tea.Cmd {
	if len(requires) == 0 {
		return p.back("", wm.Info, nil)
	}
	envVar := requires[0]
	return p.app.wm.ShowOverlay(wm.NewPrompt(p.app.toolkit, tr("mcp.add.value", envVar, name), "", func(text string) tea.Cmd {
		value := strings.TrimSpace(text)
		if value == "" {
			return p.askValues(name, requires[1:])
		}
		return p.call("mcp.servers.set_api_key", map[string]any{"name": name, "value": value, "env_var": envVar}, func(map[string]any) tea.Cmd {
			return tea.Batch(wm.Notify(tr("mcp.notice.keySaved", envVar, name), wm.Success), p.askValues(name, requires[1:]))
		})
	}))
}

// Enables a disabled server or disables an enabled one
func (p *mcpPanel) toggle(_ *cards.List, i int) tea.Cmd {
	s, ok := p.selected(i)
	if !ok {
		return nil
	}
	return p.call("mcp.servers.enable", map[string]any{"name": s.Name, "enabled": !s.Enabled}, func(map[string]any) tea.Cmd {
		if s.Enabled {
			return p.back(tr("mcp.notice.disabled", s.Name), wm.Success, nil)
		}
		return p.back(tr("mcp.notice.enabled", s.Name), wm.Success, nil)
	})
}

// Connects to the server and reports the tools it answers with or why it did not
func (p *mcpPanel) test(_ *cards.List, i int) tea.Cmd {
	s, ok := p.selected(i)
	if !ok {
		return nil
	}
	return p.call("mcp.servers.test", map[string]any{"name": s.Name}, func(res map[string]any) tea.Cmd {
		if res["ok"] != true {
			reason, _ := res["error"].(string)
			return wm.Notify(tr("mcp.notice.testFailed", s.Name, reason), wm.Warn)
		}
		tools, _ := res["tools"].([]any)
		return wm.Notify(tr("mcp.notice.testOk", s.Name, float64(len(tools))), wm.Success)
	})
}

// Asks before dropping the server from config.yaml
func (p *mcpPanel) remove(l *cards.List, i int) tea.Cmd {
	s, ok := p.selected(i)
	if !ok {
		return nil
	}
	l.Confirm(tr("mcp.remove.confirm", s.Name), func() tea.Cmd {
		return p.call("mcp.servers.remove", map[string]any{"name": s.Name}, func(map[string]any) tea.Cmd {
			return p.back(tr("mcp.notice.removed", s.Name), wm.Success, nil)
		})
	})
	return nil
}

// Starts the browser sign in of the server and waits on it in its own card
func (p *mcpPanel) authorize(_ *cards.List, i int) tea.Cmd {
	s, ok := p.selected(i)
	if !ok {
		return nil
	}
	return p.call("mcp.servers.oauth.start", map[string]any{"name": s.Name}, func(res map[string]any) tea.Cmd {
		flow, _ := res["session_id"].(string)
		link, _ := res["auth_url"].(string)
		card := &mcpAuthCard{panel: p, server: s.Name, flow: flow, link: link}
		cmds := []tea.Cmd{p.app.wm.ShowOverlay(card)}
		if link != "" && !openExternalURL(link) {
			cmds = append(cmds, wm.Notify(tr("mcp.notice.browserDidNotOpen"), wm.Warn))
		}
		return tea.Batch(cmds...)
	})
}

// Wait on the browser sign in of one server, read again until it is approved or fails
type mcpAuthCard struct {
	panel  *mcpPanel
	server string
	flow   string
	link   string
	done   bool
}

var _ interface {
	wm.Card
	wm.Keyed
	wm.Glypher
	wm.Hinter
	wm.Contexter
	wm.Sized
	wm.Selectable
} = (*mcpAuthCard)(nil)

func (c *mcpAuthCard) Title() string      { return tr("mcp.title") }
func (c *mcpAuthCard) Key() string        { return "tui:mcp:auth" }
func (c *mcpAuthCard) Glyph() string      { return c.panel.app.toolkit.Glyphs.MCP }
func (c *mcpAuthCard) Selectable() bool   { return true }
func (c *mcpAuthCard) Contexts() []string { return []string{mcpAuthContext} }
func (c *mcpAuthCard) Init() tea.Cmd      { return c.schedule() }

func (c *mcpAuthCard) Hints() []wm.Hint {
	return []wm.Hint{{Action: "tui.mcp.auth.open"}, {Action: "tui.mcp.auth.stop"}}
}

func (c *mcpAuthCard) MinSize() (int, int) {
	w, _ := c.panel.app.wm.Size()
	return min(max(44, w-6), 96), 5
}

// Heading, link or starting line and the waiting line of the card
func (c *mcpAuthCard) lines() []string {
	t := c.panel.app.toolkit
	out := []string{render.Heading(tr("mcp.auth.title", c.server), t)}
	if c.link != "" {
		out = append(out, render.Warm(c.link, t))
	} else {
		out = append(out, render.Dim(tr("mcp.auth.starting"), t))
	}
	return append(out, render.Dim(tr("mcp.auth.waiting"), t))
}

func (c *mcpAuthCard) View(w, h int) string {
	var rows []string
	for _, line := range c.lines() {
		rows = append(rows, strings.Split(ansi.Hardwrap(line, max(w-2, 8), true), "\n")...)
	}
	for i := range rows {
		rows[i] = " " + rows[i]
	}
	return strings.Join(rows[:min(len(rows), max(h, 0))], "\n")
}

func (c *mcpAuthCard) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	if msg, isAction := msg.(wm.ActionMsg); isAction {
		switch msg.ID {
		case "tui.mcp.auth.open":
			if c.link != "" && !openExternalURL(c.link) {
				return c, wm.Notify(tr("mcp.notice.browserDidNotOpen"), wm.Warn)
			}
		case "tui.mcp.auth.stop":
			c.done = true
			return c, gatewayCall(c.panel.app.gw, "mcp.servers.oauth.cancel", map[string]any{"name": c.server, "session_id": c.flow}, func(_ any, err error) tea.Cmd {
				if err != nil {
					return c.panel.back(tr("mcp.errorLine", rpcErrorMessage(err)), wm.Error, c)
				}
				return c.panel.back(tr("mcp.notice.authStopped"), wm.Info, c)
			})
		}
	}
	return c, nil
}

// Next read of the sign in
func (c *mcpAuthCard) schedule() tea.Cmd {
	if c.done {
		return nil
	}
	return tea.Tick(mcpPollEvery, func(time.Time) tea.Msg { return asyncDoneMsg{run: c.poll} })
}

// Reads the sign in, back on the panel once it is approved or failed
func (c *mcpAuthCard) poll() tea.Cmd {
	if c.done {
		return nil
	}
	return gatewayCall(c.panel.app.gw, "mcp.servers.oauth.poll", map[string]any{"name": c.server, "session_id": c.flow}, func(v any, err error) tea.Cmd {
		if c.done {
			return nil
		}
		if err != nil {
			c.done = true
			return c.panel.back(tr("mcp.errorLine", rpcErrorMessage(err)), wm.Error, c)
		}
		res := asRPCResult(v)
		if link, _ := res["auth_url"].(string); link != "" {
			c.link = link
		}
		switch res["status"] {
		case "approved":
			c.done = true
			return c.panel.back(tr("mcp.notice.authorized", c.server), wm.Success, c)
		case "error":
			c.done = true
			reason, _ := res["error_message"].(string)
			return c.panel.back(tr("mcp.notice.authFailed", c.server, reason), wm.Warn, c)
		}
		return c.schedule()
	})
}

// Command that opens the MCP servers panel
func (a *app) mcpCommands() []slashCommand {
	return []slashCommand{{name: "mcp", help: tr("slashCmd.core.help.mcp"), run: func(*slashRun) tea.Cmd { return a.openMCPServers() }}}
}

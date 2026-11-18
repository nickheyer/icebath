package tui

import (
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap contexts of the connector accounts panel and of its wait on a sign in link
const (
	connectorsContext     = "tui.connectors"
	connectorsLinkContext = "tui.connectors.link"
)

// Gateway refusals the panel words itself, a name another account holds and an operation already closed
const (
	connectorAliasTaken       = 4090
	connectorUnknownOperation = 4004
)

// Interval between two reads of the operation behind an open sign in link
var connectorsPollEvery = 2 * time.Second

// Name column of the panel, the one a rename types into
const connectorsNameColumn = 1

// Account format the gateway enforces on a connector account name
var connectorAccountNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Target states that end the wait on a sign in link
var connectorSettledStates = map[string]bool{"connected": true, "expired": true, "failed": true, "not_connected": true, "skipped": true}

// Owner every request of the panel names, the profile account and never a session
func connectorAccountOwner() map[string]any { return map[string]any{"type": "account"} }

// One hosted connector account as connectors.accounts lists it
type connectorAccount struct {
	ID        string
	Connector string
	Status    string
	Label     string
	Alias     string
	Disabled  bool
}

// Accounts of a connectors.accounts result in the order the gateway sent them
func connectorAccountsOf(result map[string]any) []connectorAccount {
	rows, _ := result["accounts"].([]any)
	out := make([]connectorAccount, 0, len(rows))
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		text := func(key string) string {
			s, _ := row[key].(string)
			return s
		}
		out = append(out, connectorAccount{ID: text("connection_id"), Connector: text("connector"), Status: text("status"),
			Label: text("label"), Alias: text("alias"), Disabled: truthyJS(row["disabled"])})
	}
	return out
}

// Retired by a reconnect that replaced it, kept and listed but never repaired
func (r connectorAccount) retired() bool { return r.Disabled }

// Whether the account stopped working, the one case a reconnect repairs
func (r connectorAccount) needsRepair() bool { return !r.retired() && r.Status != "active" }

// Name the account is addressed by, its alias else the app's own account label
func (r connectorAccount) name() string {
	if r.Alias != "" {
		return r.Alias
	}
	return r.Label
}

// Why a reconnect does not start, retired or healthy, empty when it should
func (r connectorAccount) reconnectRefusal() string {
	switch {
	case r.retired():
		return "retired"
	case r.needsRepair():
		return ""
	}
	return "healthy"
}

// Order of two texts as a locale compare sorts them, letters before case and lower case first
func localeOrder(a, b string) int {
	if c := strings.Compare(strings.ToLower(a), strings.ToLower(b)); c != 0 {
		return c
	}
	return -strings.Compare(a, b)
}

// Live accounts by app then name with the retired after them when shown, and how many are retired
func connectorAccountView(rows []connectorAccount, showRetired bool) ([]connectorAccount, int) {
	var live, retired []connectorAccount
	for _, row := range rows {
		if row.retired() {
			retired = append(retired, row)
		} else {
			live = append(live, row)
		}
	}
	byAppThenName := func(list []connectorAccount) {
		sort.SliceStable(list, func(i, j int) bool {
			if c := localeOrder(list[i].Connector, list[j].Connector); c != 0 {
				return c < 0
			}
			return localeOrder(list[i].name(), list[j].name()) < 0
		})
	}
	byAppThenName(live)
	byAppThenName(retired)
	if showRetired {
		return append(live, retired...), len(retired)
	}
	return live, len(retired)
}

// Problem of a typed name, invalid or taken by another live account of the app, empty when there is none
func connectorNameProblem(name, connector string, rows []connectorAccount, self string) string {
	if !connectorAccountNameRe.MatchString(name) {
		return "invalid"
	}
	for _, row := range rows {
		if row.Connector == connector && row.Alias == name && row.ID != self && !row.retired() {
			return "taken"
		}
	}
	return ""
}

// Words of a name problem
func connectorProblemText(problem string) string {
	if problem == "taken" {
		return tr("connectors.notice.nameTaken")
	}
	return tr("connectors.notice.nameInvalid")
}

// Status label of an account, retired ahead of whatever the gateway last saw
func connectorStatusLabel(r connectorAccount) string {
	if r.retired() {
		return tr("connectors.status.retired")
	}
	return tr("connectors.status." + r.Status)
}

// Name cell of an account, the vendor label tagged unnamed while it has no alias
func connectorNameCell(r connectorAccount) string {
	if r.Alias != "" {
		return r.Alias
	}
	return r.Label + " " + tr("connectors.unnamedTag")
}

// Failure of a panel request in the user's words, a name another account holds named as taken
func connectorErrorText(err error) string {
	var refused *rpcError
	if errors.As(err, &refused) && refused.HasCode && refused.Code == connectorAliasTaken {
		return tr("connectors.notice.nameTaken")
	}
	return rpcErrorMessage(err)
}

// Whether a failed operation read is worth retrying, a transport failure or a server fault and never a coded refusal
func connectorRetryable(err error) bool {
	var refused *rpcError
	return !errors.As(err, &refused) || !refused.HasCode || refused.Code >= 5000
}

// First target of an operation snapshot, nil when it has none
func connectorFirstTarget(snapshot map[string]any) map[string]any {
	targets, _ := snapshot["targets"].([]any)
	if len(targets) == 0 {
		return nil
	}
	target, _ := targets[0].(map[string]any)
	return target
}

// Whether a snapshot says the connect is over, settled or its target in a final state
func connectorSnapshotSettled(snapshot, target map[string]any) bool {
	if truthyJS(snapshot["settled"]) {
		return true
	}
	state, _ := target["state"].(string)
	return target != nil && connectorSettledStates[state]
}

// Connector accounts panel, every hosted account of the profile with rename, add, reconnect and remove
type connectorsPanel struct {
	*cards.List
	app         *app
	accounts    []connectorAccount
	shown       []connectorAccount
	retired     int
	showRetired bool
	loading     bool
	busy        bool
}

// Opens the connector accounts panel and reads the accounts into it
func (a *app) openConnectors() tea.Cmd {
	p := &connectorsPanel{app: a, loading: true}
	p.List = cards.NewList(a.cardEnv(), cards.ListSpec{
		Title: tr("connectors.title"), Glyph: a.toolkit.Glyphs.MCP, Key: "tui:connectors", Context: connectorsContext,
		Columns: []render.Column{{Title: "app", Flex: 1, Max: 24}, {Title: "name", Flex: 2}, {Title: "status", Max: 12}},
		Rows:    p.rows,
		Free:    map[string]bool{"tui.connectors.add": true, "tui.connectors.retired": true},
		Hints:   p.hints,
		Empty:   p.empty,
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"tui.connectors.rename":    p.rename,
			"tui.connectors.add":       p.add,
			"tui.connectors.reconnect": p.reconnect,
			"tui.connectors.remove":    p.remove,
			"tui.connectors.retired": func(l *cards.List, _ int) tea.Cmd {
				p.showRetired = !p.showRetired
				l.Refresh()
				return nil
			},
		},
	})
	return tea.Batch(a.wm.ShowOverlay(p), p.load())
}

// Keeps the panel as the card the window manager holds while the list underneath takes the message
func (p *connectorsPanel) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	_, cmd := p.List.Update(msg)
	return p, cmd
}

// Trailer of the panel, the load in flight or how many retired accounts are folded or shown
func (p *connectorsPanel) Status() string {
	switch {
	case p.loading:
		return tr("connectors.loading")
	case p.retired == 0:
		return ""
	case p.showRetired:
		return tr("connectors.retiredShown", float64(p.retired))
	}
	return tr("connectors.retiredFolded", float64(p.retired))
}

// Rows of the accounts in view, retired ones dimmed
func (p *connectorsPanel) rows() [][]string {
	p.shown, p.retired = connectorAccountView(p.accounts, p.showRetired)
	out := make([][]string, len(p.shown))
	for i, row := range p.shown {
		cells := []string{row.Connector, connectorNameCell(row), connectorStatusLabel(row)}
		if row.retired() {
			for c := range cells {
				cells[c] = render.Dim(cells[c], p.app.toolkit)
			}
		}
		out[i] = cells
	}
	return out
}

// Line of an empty panel, the load in flight or how to add the first account
func (p *connectorsPanel) empty() string {
	if p.loading {
		return tr("connectors.loading")
	}
	return tr("connectors.empty")
}

// Legend of the selected account, reconnect offered only for one that stopped working
func (p *connectorsPanel) hints(_ *cards.List, i int) []wm.Hint {
	out := []wm.Hint{{Action: "tui.connectors.rename"}, {Action: "tui.connectors.add"}}
	if i < 0 || i >= len(p.shown) || p.shown[i].needsRepair() {
		out = append(out, wm.Hint{Action: "tui.connectors.reconnect"})
	}
	return append(out, wm.Hint{Action: "tui.connectors.remove"}, wm.Hint{Action: "tui.connectors.retired"}, wm.Hint{Action: "panel.close"})
}

// Reads the accounts again, a failure leaving the rows as they were
func (p *connectorsPanel) load() tea.Cmd {
	return gatewayCall(p.app.gw, "connectors.accounts", map[string]any{}, func(v any, err error) tea.Cmd {
		p.loading = false
		if err != nil {
			p.List.Refresh()
			return p.fail(err)
		}
		p.accounts = connectorAccountsOf(asRPCResult(v))
		p.List.Refresh()
		return nil
	})
}

// Toast of a failed request
func (p *connectorsPanel) fail(err error) tea.Cmd {
	return wm.Notify(tr("connectors.errorLine", connectorErrorText(err)), wm.Error)
}

// Shows the panel again unless it is up or another card replaced the card it left
func (p *connectorsPanel) reshow(from wm.Card) tea.Cmd {
	if overlay := p.app.wm.Overlay(); overlay == p || (from != nil && overlay != from) {
		return nil
	}
	return p.app.wm.ShowOverlay(p)
}

// Account under the cursor, false while a request runs or no row is selected
func (p *connectorsPanel) selected(i int) (connectorAccount, bool) {
	if p.busy || i < 0 || i >= len(p.shown) {
		return connectorAccount{}, false
	}
	return p.shown[i], true
}

// Opens the name cell of the account for typing, its alias to start from
func (p *connectorsPanel) rename(l *cards.List, i int) tea.Cmd {
	row, ok := p.selected(i)
	if !ok {
		return nil
	}
	p.editName(l, row, row.Alias)
	return nil
}

// Types into the name cell of the account, enter renaming it
func (p *connectorsPanel) editName(l *cards.List, row connectorAccount, draft string) {
	l.EditCell(connectorsNameColumn, draft, func(l *cards.List, text string) tea.Cmd { return p.submitRename(l, row, text) })
}

// Renames the account once the name passes the local check, the gateway checking it again
func (p *connectorsPanel) submitRename(l *cards.List, row connectorAccount, draft string) tea.Cmd {
	name := trimJS(draft)
	if problem := connectorNameProblem(name, row.Connector, p.accounts, row.ID); problem != "" {
		p.editName(l, row, draft)
		return wm.Notify(tr("connectors.errorLine", connectorProblemText(problem)), wm.Error)
	}
	p.busy = true
	return gatewayCall(p.app.gw, "connectors.accounts.rename", map[string]any{"alias": name, "connection_id": row.ID}, func(_ any, err error) tea.Cmd {
		p.busy = false
		if err != nil {
			if i := l.Index(); i >= 0 && i < len(p.shown) && p.shown[i].ID == row.ID {
				p.editName(l, row, draft)
			}
			return p.fail(err)
		}
		return tea.Batch(wm.Notify(tr("connectors.notice.renamed", row.Connector, name), wm.Success), p.load())
	})
}

// Lists the apps an account can be added to and asks which one
func (p *connectorsPanel) add(_ *cards.List, _ int) tea.Cmd {
	if p.busy {
		return nil
	}
	p.busy = true
	return gatewayCall(p.app.gw, "connectors.list", map[string]any{"owner": connectorAccountOwner()}, func(v any, err error) tea.Cmd {
		p.busy = false
		if err != nil {
			return p.fail(err)
		}
		rows, _ := asRPCResult(v)["connectors"].([]any)
		seen := map[string]bool{}
		var apps []string
		for _, raw := range rows {
			row, _ := raw.(map[string]any)
			if app, _ := row["connector"].(string); !seen[app] {
				seen[app] = true
				apps = append(apps, app)
			}
		}
		sort.Strings(apps)
		if len(apps) == 0 {
			return wm.Notify(tr("connectors.add.noApps"), wm.Info)
		}
		items := make([]wm.Item, 0, len(apps))
		for _, app := range apps {
			items = append(items, wm.Item{ID: "connector:" + app, Title: app, Run: func() tea.Cmd { return p.askNewName(app, "") }})
		}
		picker := wm.NewPalette(p.app.toolkit, items)
		picker.Placeholder = tr("connectors.add.pickApp")
		return p.app.wm.ShowOverlay(picker)
	})
}

// Asks the name of a new account of the app, the draft typed so far filled in
func (p *connectorsPanel) askNewName(app, draft string) tea.Cmd {
	return p.app.wm.ShowOverlay(wm.NewPrompt(p.app.toolkit, tr("connectors.add.name", app), draft, func(text string) tea.Cmd {
		return p.submitAdd(app, text)
	}))
}

// Starts the connect of a new named account once the name passes the local check
func (p *connectorsPanel) submitAdd(app, draft string) tea.Cmd {
	name := trimJS(draft)
	if problem := connectorNameProblem(name, app, p.accounts, ""); problem != "" {
		return tea.Batch(p.askNewName(app, draft), wm.Notify(tr("connectors.errorLine", connectorProblemText(problem)), wm.Error))
	}
	return p.startConnect(app, map[string]any{"alias": name}, name, func(err error) tea.Cmd {
		return tea.Batch(p.askNewName(app, draft), p.fail(err))
	})
}

// Signs a stopped account in again, a healthy or retired one answered instead
func (p *connectorsPanel) reconnect(_ *cards.List, i int) tea.Cmd {
	row, ok := p.selected(i)
	if !ok {
		return nil
	}
	switch row.reconnectRefusal() {
	case "retired":
		return wm.Notify(tr("connectors.notice.retiredNoReconnect"), wm.Info)
	case "healthy":
		return wm.Notify(tr("connectors.notice.connected", row.Connector, row.name()), wm.Info)
	}
	return p.startConnect(row.Connector, map[string]any{"connection_id": row.ID}, row.name(), p.fail)
}

// Asks before removing the account, a yes signing icebath out of it
func (p *connectorsPanel) remove(l *cards.List, i int) tea.Cmd {
	row, ok := p.selected(i)
	if !ok {
		return nil
	}
	l.Confirm(tr("connectors.remove.confirm", row.Connector, row.name()), func() tea.Cmd {
		p.busy = true
		return gatewayCall(p.app.gw, "connectors.accounts.remove", map[string]any{"connection_id": row.ID}, func(_ any, err error) tea.Cmd {
			p.busy = false
			if err != nil {
				return p.fail(err)
			}
			return tea.Batch(wm.Notify(tr("connectors.notice.removed", row.Connector, row.name()), wm.Success), p.load())
		})
	})
	return nil
}

// Connects a new named account or repairs one by its id, waiting on the link unless it settled
func (p *connectorsPanel) startConnect(app string, address map[string]any, name string, onFail func(error) tea.Cmd) tea.Cmd {
	_, repair := address["connection_id"]
	params := map[string]any{"connectors": []any{app}, "owner": connectorAccountOwner(), "reconnect": repair}
	for key, value := range address {
		params[key] = value
	}
	p.busy = true
	return gatewayCall(p.app.gw, "connectors.connect", params, func(v any, err error) tea.Cmd {
		p.busy = false
		if err != nil {
			return onFail(err)
		}
		result := asRPCResult(v)
		target := connectorFirstTarget(result)
		if connectorSnapshotSettled(result, target) {
			if target == nil {
				return p.reshow(nil)
			}
			return p.finishLink(target, name, nil)
		}
		opID, _ := result["op_id"].(string)
		deadline, _ := result["deadline_at"].(float64)
		return p.app.wm.ShowOverlay(&connectorLinkCard{panel: p, app: app, name: name, reconnect: repair, opID: opID, deadline: deadline, target: target})
	})
}

// Back on the panel with the outcome the user named, connected or why not, and the accounts read again
func (p *connectorsPanel) finishLink(target map[string]any, name string, from wm.Card) tea.Cmd {
	targetName, _ := target["name"].(string)
	detail, _ := target["detail"].(string)
	notice, level := tr("connectors.notice.connected", targetName, name), wm.Success
	if state, _ := target["state"].(string); state != "connected" {
		notice, level = detail, wm.Warn
		if notice == "" {
			notice = tr("connectors.notice.notConnected", targetName)
		}
	}
	return tea.Batch(p.reshow(from), wm.Notify(notice, level), p.load())
}

// Back on the panel with a notice and the accounts read again
func (p *connectorsPanel) back(notice string, level wm.Level, from wm.Card) tea.Cmd {
	cmds := []tea.Cmd{p.reshow(from), p.load()}
	if notice != "" {
		cmds = append(cmds, wm.Notify(notice, level))
	}
	return tea.Batch(cmds...)
}

// Wait on the sign in link of one account connect, read again until it settles or expires
type connectorLinkCard struct {
	panel     *connectorsPanel
	app       string
	name      string
	reconnect bool
	opID      string
	deadline  float64
	target    map[string]any
	done      bool
}

var _ interface {
	wm.Card
	wm.Keyed
	wm.Glypher
	wm.Hinter
	wm.Contexter
	wm.Sized
	wm.Selectable
} = (*connectorLinkCard)(nil)

func (c *connectorLinkCard) Title() string      { return tr("connectors.title") }
func (c *connectorLinkCard) Key() string        { return "tui:connectors:link" }
func (c *connectorLinkCard) Glyph() string      { return c.panel.app.toolkit.Glyphs.MCP }
func (c *connectorLinkCard) Selectable() bool   { return true }
func (c *connectorLinkCard) Contexts() []string { return []string{connectorsLinkContext} }
func (c *connectorLinkCard) Init() tea.Cmd      { return c.schedule() }

func (c *connectorLinkCard) Hints() []wm.Hint {
	return []wm.Hint{{Action: "tui.connectors.link.open"}, {Action: "tui.connectors.link.stop"}}
}

func (c *connectorLinkCard) MinSize() (int, int) {
	w, _ := c.panel.app.wm.Size()
	return min(max(44, w-6), 96), 6
}

// Heading, link or starting line, the last failure and the waiting line of the card
func (c *connectorLinkCard) lines() []string {
	t := c.panel.app.toolkit
	heading := tr("connectors.link.title", c.app, c.name)
	if c.reconnect {
		heading = tr("connectors.link.reconnectTitle", c.app, c.name)
	}
	out := []string{render.Heading(heading, t)}
	if link, _ := c.target["connect_url"].(string); link != "" {
		out = append(out, render.Warm(link, t))
	} else {
		out = append(out, render.Dim(tr("connectors.link.starting"), t))
	}
	if detail, _ := c.target["detail"].(string); detail != "" {
		out = append(out, render.Bad(detail, t))
	}
	return append(out, render.Dim(tr("connectors.link.waiting"), t))
}

func (c *connectorLinkCard) View(w, h int) string {
	var rows []string
	for _, line := range c.lines() {
		rows = append(rows, strings.Split(ansi.Hardwrap(line, max(w-2, 8), true), "\n")...)
	}
	for i := range rows {
		rows[i] = " " + rows[i]
	}
	return strings.Join(rows[:min(len(rows), max(h, 0))], "\n")
}

func (c *connectorLinkCard) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	if msg, isAction := msg.(wm.ActionMsg); isAction {
		switch msg.ID {
		case "tui.connectors.link.open":
			link, _ := c.target["connect_url"].(string)
			if link == "" {
				return c, nil
			}
			if !openExternalURL(link) {
				return c, wm.Notify(tr("connectors.notice.browserDidNotOpen"), wm.Warn)
			}
		case "tui.connectors.link.stop":
			c.done = true
			return c, c.panel.back(tr("connectors.notice.stopped"), wm.Info, c)
		}
	}
	return c, nil
}

// Next read of the operation, or the end of the wait once the operation's deadline passed
func (c *connectorLinkCard) schedule() tea.Cmd {
	if c.done {
		return nil
	}
	if c.deadline > 0 && float64(time.Now().UnixMilli())/1000 > c.deadline {
		c.done = true
		return c.panel.back(tr("connectors.notice.waitEnded"), wm.Info, c)
	}
	return tea.Tick(connectorsPollEvery, func(time.Time) tea.Msg { return asyncDoneMsg{run: c.poll} })
}

// Reads the operation, finishing on a settled one, retrying a transient failure and ending on a refusal
func (c *connectorLinkCard) poll() tea.Cmd {
	if c.done {
		return nil
	}
	return gatewayCall(c.panel.app.gw, "connectors.operation.status", map[string]any{"op_id": c.opID, "owner": connectorAccountOwner()}, func(v any, err error) tea.Cmd {
		if c.done {
			return nil
		}
		if err != nil {
			if connectorRetryable(err) {
				return c.schedule()
			}
			c.done = true
			var refused *rpcError
			if errors.As(err, &refused) && refused.Code == connectorUnknownOperation {
				return c.panel.back("", wm.Info, c)
			}
			return c.panel.back(tr("connectors.errorLine", rpcErrorMessage(err)), wm.Error, c)
		}
		snapshot := asRPCResult(v)
		target := connectorFirstTarget(snapshot)
		if connectorSnapshotSettled(snapshot, target) {
			c.done = true
			if target == nil {
				target = map[string]any{"action": "connect", "kind": "connector", "name": c.app, "state": "not_connected"}
			}
			return c.panel.finishLink(target, c.name, c)
		}
		c.target = target
		return c.schedule()
	})
}

// Command that opens the connector accounts panel
func (a *app) connectorsCommands() []slashCommand {
	return []slashCommand{{name: "connectors", help: tr("slashCmd.core.help.connectors"), run: func(*slashRun) tea.Cmd { return a.openConnectors() }}}
}

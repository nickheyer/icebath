// The page of Language servers: whether code is checked, every server with its state, adding a server of your own, installing servers,
// stopping the running ones, trusting the folders they were kept out of, the page of each server, and the options of the area
package app

import (
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	lspconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/lsp/config"
	"github.com/nickheyer/icebath/internal/profile"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	lspv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/lsp/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/lsp/v1/lspv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Screen of Language servers
const Screen = "lsp"

// How often the page reads how the servers stand while it is open, so a server that starts or stops shows it
const refresh = 2 * time.Second

// Headings of the page: the servers by their state, then the options
const (
	runningGroup   = "Running"
	installedGroup = "Installed"
	missingGroup   = "Not installed"
	offGroup       = "Off"
	optionsGroup   = "Options"
)

type screens struct{}

func Register(registry *app.Registry) {
	registry.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := lspv1connect.NewLspServiceClient(conn)
	settings := configv1connect.NewConfigServiceClient(conn)
	return []tui.Screen{{
		ID:    Screen,
		Group: tui.GroupAbilities,
		Order: 90,
		Title: "Language servers",
		Short: "Check the code the agent writes for errors",
		Slash: []string{"lsp"},
		Usage: "[@profile] [add|<server>]",
		Area:  lspconfig.Key,
		Pages: func(kit *tui.Kit, name string) []tui.Page { return serverPages(kit, service, name) },
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			name, rest := profile.Split(argument)
			return (&page{kit: kit, service: service, settings: settings, profile: name}).open(strings.TrimSpace(rest))
		},
	}}
}

// The page of the area on one profile, and the views it opened over itself
type page struct {
	kit      *tui.Kit
	service  lspv1connect.LspServiceClient
	settings configv1connect.ConfigServiceClient
	// Profile the page was opened on, the one in use when empty
	profile string
	hub     *cards.Hub
	list    *cards.List
	options *configapp.Rows
	status  *lspv1.StatusResponse
	loaded  bool
	err     error
	// Rows as last listed
	rows []row
	// Lists of the pages of servers opened over the page, read again with it
	views cards.Watched
	// Server put in hand once the servers are read again
	focus string
	// The page reads the servers again while it is in view
	watching bool
}

// Row of the page: the switch of code checking or another option, a server, the row that adds one, something done to the servers, or the
// row of the advanced options
type row struct {
	setting  *configv1.Setting
	server   *lspv1.Server
	add      bool
	act      *act
	advanced bool
}

// Something a row does, its name, what it does and the word on its button
type act struct {
	title, about, verb string
	run                func(l *cards.List) tea.Cmd
}

// Shows the page, then reads the servers and the options; an argument adds a server or opens the page of one
func (p *page) open(argument string) tea.Cmd {
	t := p.kit.Env.Theme
	p.list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Language servers",
		Glyph:   t.Glyphs.Terminal,
		Key:     Screen,
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search language servers",
		Intro:   p.intro,
		Columns: configapp.Columns,
		Rows: func() [][]string {
			p.rows = p.order()
			cells := make([][]string, len(p.rows))
			for i, r := range p.rows {
				cells[i] = p.cells(r)
			}
			return cells
		},
		Group: func(i int) string { return p.group(p.rows[i]) },
		Words: func(i int) string { return p.words(p.rows[i]) },
		Detail: func(i int) []string {
			return p.detail(p.rows[i], p.list.Cut(i, 2))
		},
		Empty:   func() string { return tui.Waiting(p.loaded, p.err, "") },
		OnFocus: func(*cards.List) tea.Cmd { return tea.Batch(p.load(), p.watch()) },
		Free:    map[string]bool{"hub.add": true, "hub.refresh": true},
		Hints:   func(_ *cards.List, i int) []wm.Hint { return p.hints(i) },
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd { return p.use(l, p.rows[i]) },
			// The page of a server, which enter opens too unless it installs the server
			"hub.edit": func(_ *cards.List, i int) tea.Cmd {
				if srv := p.rows[i].server; srv != nil {
					return p.page(srv.GetId())
				}
				return nil
			},
			"hub.remove":  func(l *cards.List, i int) tea.Cmd { return p.remove(l, p.rows[i]) },
			"hub.add":     func(*cards.List, int) tea.Cmd { return p.add() },
			"hub.refresh": func(*cards.List, int) tea.Cmd { return tea.Batch(p.load(), p.options.Load(nil)) },
		},
	})
	p.list.ToggleDetail()
	hub, cmd := p.kit.Show(Screen, p.list)
	p.hub = hub
	p.options = configapp.NewRows(p.kit, hub, p.settings, p.profile)
	p.options.Watch(p.list)
	then := func() tea.Cmd {
		switch argument {
		case "":
			return nil
		case "add":
			return p.add()
		}
		if srv := p.server(argument); srv != nil {
			return p.page(srv.GetId())
		}
		return p.hub.Fail("No language server is called " + argument + ".")
	}
	return tea.Batch(cmd, p.options.Load(nil), p.read(then), p.watch())
}

// First line of the page, what the area is, then in the warm colour what is wrong and what to do about it
func (p *page) intro() []string {
	t := p.kit.Env.Theme
	lines := []string{"Programs that check the code the agent writes for errors."}
	switch {
	case p.status.GetExcludeRootsInvalid():
		lines = append(lines, render.Warm("Skipped folders is not a list of folders, so no code is checked. Set it again below.", t))
	case len(p.status.GetWarnings()) > 0:
		lines = append(lines, render.Warm(p.status.GetWarnings()[0], t))
	}
	return lines
}

// Reads how the servers stand
func (p *page) load() tea.Cmd { return p.read(nil) }

// Reads how the servers stand, then runs then
func (p *page) read(then func() tea.Cmd) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := p.service.Status(p.kit.Env.Ctx, &lspv1.StatusRequest{Profile: p.profile})
		return func() tea.Cmd {
			p.err, p.loaded = err, true
			if err == nil {
				p.status = response
			}
			p.list.Refresh()
			p.views.Refresh()
			if p.focus != "" {
				for i, r := range p.rows {
					if r.server != nil && r.server.GetId() == p.focus && p.list.Select(i) {
						p.focus = ""
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
func (p *page) watch() tea.Cmd {
	if p.watching {
		return nil
	}
	p.watching = true
	return p.tick()
}

func (p *page) tick() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		time.Sleep(refresh)
		return func() tea.Cmd {
			switch {
			case !p.hub.Shown():
				p.watching = false
				return nil
			case p.hub.Waiting():
				return p.tick()
			}
			return tea.Batch(p.load(), p.tick())
		}
	})
}

// Server of the id as last read, nil for none
func (p *page) server(id string) *lspv1.Server {
	for _, srv := range p.status.GetServers() {
		if srv.GetId() == id {
			return srv
		}
	}
	return nil
}

// Rows of the page once the servers are read: Check code first, the servers by their state and name, the row that adds one, what is done
// to the servers, then the options and the row of the advanced ones
func (p *page) order() []row {
	if p.status == nil {
		return nil
	}
	var rows []row
	if setting := p.options.Setting(lspconfig.Enabled); setting != nil {
		rows = append(rows, row{setting: setting})
	}
	servers := slices.Clone(p.status.GetServers())
	place := map[string]int{runningGroup: 0, installedGroup: 1, missingGroup: 2, offGroup: 3}
	slices.SortStableFunc(servers, func(a, b *lspv1.Server) int {
		if d := place[heading(a)] - place[heading(b)]; d != 0 {
			return d
		}
		return strings.Compare(a.GetId(), b.GetId())
	})
	for _, srv := range servers {
		rows = append(rows, row{server: srv})
	}
	rows = append(rows, row{add: true})
	for _, a := range p.acts() {
		rows = append(rows, row{act: a})
	}
	everyday, _ := p.options.Section(lspconfig.Key)
	for _, setting := range everyday {
		if setting.GetKey() != lspconfig.Enabled {
			rows = append(rows, row{setting: setting})
		}
	}
	if _, _, ok := p.options.Advanced(lspconfig.Key); ok {
		rows = append(rows, row{advanced: true})
	}
	return rows
}

// Heading a server is listed under by its state: one that failed is installed
func heading(srv *lspv1.Server) string {
	switch srv.GetState() {
	case lspconfig.Running:
		return runningGroup
	case lspconfig.Installed, lspconfig.Failed:
		return installedGroup
	case lspconfig.Off:
		return offGroup
	}
	return missingGroup
}

// Heading a row is listed under: Check code none, a server that of its state, the rows after the servers none, so they sit with them, and
// the options theirs
func (p *page) group(r row) string {
	switch {
	case r.server != nil:
		return heading(r.server)
	case r.advanced, r.setting != nil && r.setting.GetKey() != lspconfig.Enabled:
		return optionsGroup
	}
	return ""
}

// What is done to the servers: installing all of them, stopping those that run while some do, and trusting the folders some were kept out of
// while there are any
func (p *page) acts() []*act {
	out := []*act{{"Install all servers", "Installs each server that is not installed and that icebath can install, and says what the others need.",
		"install", p.installAll}}
	if slices.ContainsFunc(p.status.GetServers(), func(srv *lspv1.Server) bool { return srv.GetState() == lspconfig.Running }) {
		out = append(out, &act{"Stop running servers", "The next edit of a file starts them again.", "stop", p.stop})
	}
	if skipped := p.status.GetSkipped(); len(skipped) > 0 {
		out = append(out, &act{"Trust skipped folders", "Lets servers run the project's own programs in " + strings.Join(skipped, ", ") + ".", "trust",
			p.trust})
	}
	return out
}

// Cells of a row: its name, the mark of a changed value or of a row that opens, and its value or state
func (p *page) cells(r row) []string {
	t := p.kit.Env.Theme
	opens := render.Dim(t.Glyphs.Forward, t)
	switch {
	case r.setting != nil:
		return p.options.Cells(r.setting)
	case r.server != nil:
		return []string{r.server.GetId(), "", stateWord(r.server, t)}
	case r.add:
		return []string{"Add a server", opens, ""}
	case r.act != nil:
		return []string{r.act.title, "", ""}
	}
	cells, _, _ := p.options.Advanced(lspconfig.Key)
	return cells
}

// State of a server in its word, in the colour of what it means
func stateWord(srv *lspv1.Server, t theme.Theme) string {
	switch srv.GetState() {
	case lspconfig.Running:
		return render.Good(srv.GetState(), t)
	case lspconfig.Failed:
		return render.Bad(srv.GetState(), t)
	}
	return srv.GetState()
}

// Words a search reads for a row besides its cells: what a server checks and is
func (p *page) words(r row) string {
	if r.server != nil {
		return strings.Join(r.server.GetExtensions(), " ") + " " + r.server.GetDescription()
	}
	return ""
}

// Lines under the list about the row in hand, never what the row or the first line says: an option as Settings explains it, the facts
// of a server, what the other rows do
func (p *page) detail(r row, cut bool) []string {
	switch {
	case r.setting != nil:
		return p.options.Detail(r.setting, cut)
	case r.server != nil:
		return p.facts(r.server)
	case r.add:
		return []string{"A language server icebath does not know, by the command that starts it."}
	case r.act != nil:
		return []string{r.act.about}
	}
	return []string{"Settings that few people change."}
}

// Facts of a server: what it is, the files it checks, where it runs from or how it gets installed, where it runs, failed or was kept out,
// and whether it runs in a folder you have not trusted
func (p *page) facts(srv *lspv1.Server) []string {
	t := p.kit.Env.Theme
	lines := []string{strs.Full(srv.GetDescription()), render.Dim(checked(srv), t)}
	if command := srv.GetCommand(); command != "" {
		lines = append(lines, render.Dim("Starts with "+command, t))
	}
	if path := srv.GetPath(); path != "" {
		lines = append(lines, "Runs from "+path+".")
	} else if install := srv.GetInstall(); install != "" && srv.GetState() != lspconfig.Off {
		lines = append(lines, install)
	}
	if len(srv.GetRunning()) > 0 && srv.GetState() == lspconfig.Running {
		lines = append(lines, render.Good("Runs in "+strings.Join(srv.GetRunning(), ", ")+".", t))
	}
	if failed := srv.GetFailed(); len(failed) > 0 {
		lines = append(lines, render.Bad("Failed in "+strings.Join(failed, ", ")+". "+retry(srv), t))
	}
	for _, root := range srv.GetSkipped() {
		lines = append(lines, render.Warm("Kept out of "+root+", which is not trusted.", t))
	}
	if !srv.GetUntrustedSafe() {
		lines = append(lines, render.Dim("Runs only in folders you trust, since it runs the project's own programs.", t))
	}
	return lines
}

// The files a server checks, as a sentence; one you added by hand with no file types yet checks none
func checked(srv *lspv1.Server) string {
	if len(srv.GetExtensions()) == 0 {
		return "Checks no files until it has file types."
	}
	return "Checks " + strs.And(srv.GetExtensions()) + " files."
}

// When a server that failed is tried again
func retry(srv *lspv1.Server) string {
	if after := srv.GetRetry(); after != "" {
		return "It is tried again after " + after + "."
	}
	return "It is not tried again until icebath restarts."
}

// Buttons for the row in hand: what enter does, remove on a server you added or reset on a changed option, then add
func (p *page) hints(i int) []wm.Hint {
	add := wm.Hint{Action: "hub.add", Label: "add"}
	if i < 0 || i >= len(p.rows) {
		return []wm.Hint{add}
	}
	switch r := p.rows[i]; {
	case r.setting != nil:
		return append(p.options.Hints(r.setting), add)
	case r.server != nil:
		hints := []wm.Hint{{Action: "hub.use", Label: "open"}}
		if r.server.GetInstallable() {
			hints = []wm.Hint{{Action: "hub.use", Label: "install"}, {Action: "hub.edit", Label: "open"}}
		}
		if r.server.GetAdded() {
			hints = append(hints, wm.Hint{Action: "hub.remove", Label: "remove"})
		}
		return append(hints, add)
	case r.add:
		return []wm.Hint{{Action: "hub.use", Label: "add"}}
	case r.act != nil:
		return []wm.Hint{{Action: "hub.use", Label: r.act.verb}, add}
	}
	return []wm.Hint{{Action: "hub.use", Label: "open"}, add}
}

// What enter does on a row: changes an option, installs a server not installed or opens the page of any other, adds a server, does the act
// of the row or opens the advanced options
func (p *page) use(l *cards.List, r row) tea.Cmd {
	switch {
	case r.setting != nil:
		return p.options.Change(r.setting)
	case r.server != nil && r.server.GetInstallable():
		return p.install(r.server.GetId())
	case r.server != nil:
		return p.page(r.server.GetId())
	case r.add:
		return p.add()
	case r.act != nil:
		return r.act.run(l)
	}
	_, open, _ := p.options.Advanced(lspconfig.Key)
	return open()
}

// What ctrl+d does on a row: removes a server you added after a question, or puts a changed option back to its default
func (p *page) remove(l *cards.List, r row) tea.Cmd {
	switch {
	case r.setting != nil:
		return p.options.Reset(l, r.setting)
	case r.server != nil && r.server.GetAdded():
		return p.forget(l, r.server)
	}
	return nil
}

// Asks before a server you added is removed, then removes it
func (p *page) forget(l *cards.List, srv *lspv1.Server) tea.Cmd {
	text := "Remove " + srv.GetId() + "? icebath stops using it to check " + strs.And(srv.GetExtensions()) + " files."
	if len(srv.GetExtensions()) == 0 {
		text = "Remove " + srv.GetId() + "? The program it runs stays on this computer."
	}
	l.Confirm(cards.Question{Text: text, Verb: "Remove", Deletes: true}, func() tea.Cmd {
		return tui.Call(p.hub, func() (*lspv1.RemoveResponse, error) {
			return p.service.Remove(p.kit.Env.Ctx, &lspv1.RemoveRequest{Profile: p.profile, Server: srv.GetId()})
		}, func(*lspv1.RemoveResponse) tea.Cmd {
			return tea.Batch(p.hub.PopTo(p.list), p.hub.Done("Removed "+srv.GetId()+"."), p.load(), p.options.Load(nil))
		})
	})
	return nil
}

// Stops the servers that run for the profile
func (p *page) stop(*cards.List) tea.Cmd {
	p.hub.Busy("Stopping the language servers…")
	return tui.Call(p.hub, func() (*lspv1.StopResponse, error) {
		return p.service.Stop(p.kit.Env.Ctx, &lspv1.StopRequest{Profile: p.profile})
	}, func(r *lspv1.StopResponse) tea.Cmd {
		told := "Stopped " + render.Count(int(r.GetStopped()), "language server") + ". The next edit starts them again."
		if r.GetStopped() == 0 {
			told = "No language server was running."
		}
		return tea.Batch(p.hub.Done(told), p.load())
	})
}

// Asks before the folders servers were kept out of are trusted, then trusts them
func (p *page) trust(l *cards.List) tea.Cmd {
	folders := strings.Join(p.status.GetSkipped(), ", ")
	l.Confirm(cards.Question{Text: "Trust " + folders + "? Language servers may then run the project's own programs there.", Verb: "Trust"},
		func() tea.Cmd {
			return tui.Call(p.hub, func() (*lspv1.TrustResponse, error) {
				return p.service.Trust(p.kit.Env.Ctx, &lspv1.TrustRequest{Profile: p.profile})
			}, func(r *lspv1.TrustResponse) tea.Cmd {
				return tea.Batch(p.hub.Done("Trusted "+strings.Join(r.GetFolders(), ", ")+". Servers run there from the next edit."), p.load())
			})
		})
	return nil
}

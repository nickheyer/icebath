// The language servers screen: whether code is checked, every server icebath knows and how it stands, installing servers, stopping the
// running ones, trusting the folders they were kept out of, and what runs now
package app

import (
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/internal/config"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	lspconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/lsp/config"
	"github.com/nickheyer/icebath/internal/profile"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	lspv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/lsp/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/lsp/v1/lspv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Column of the list that holds what a row is, shown whole or not at all
const aboutColumn = 2

// Headings of the list, in the order its rows run
const (
	runningGroup   = "Running"
	installedGroup = "Installed"
	missingGroup   = "Not installed"
	offGroup       = "Turned off"
	moreGroup      = "More"
)

type screens struct{}

func Register(registry *app.Registry) {
	registry.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := lspv1connect.NewLspServiceClient(conn)
	settings := configv1connect.NewConfigServiceClient(conn)
	return []tui.Screen{{
		ID:    "lsp",
		Group: tui.GroupAbilities,
		Order: 90,
		Title: "Language servers",
		Short: "Check the code the agent writes for errors",
		Slash: []string{"lsp"},
		Usage: "[@profile]",
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			name, _ := profile.Split(argument)
			return (&servers{kit: kit, service: service, settings: settings, profile: name}).open()
		},
	}}
}

type servers struct {
	kit      *tui.Kit
	service  lspv1connect.LspServiceClient
	settings configv1connect.ConfigServiceClient
	// Profile the screen was opened on, the one in use when empty
	profile string
	hub     *cards.Hub
	list    *cards.List
	status  *lspv1.StatusResponse
	loaded  bool
	err     error
	// Rows as last listed
	rows []row
}

// Row of the list: the switch that turns code checking on or off, a server, or something else the screen does
type row struct {
	toggle bool
	server *lspv1.Server
	act    *action
}

// Something a row does besides a server
type action struct {
	title, about string
	// Word on the button that does it
	verb string
	run  func(l *cards.List) tea.Cmd
}

func (s *servers) open() tea.Cmd {
	t := s.kit.Env.Theme
	title := "Language servers"
	if s.profile != "" {
		title += " · " + s.profile
	}
	s.list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   title,
		Glyph:   t.Glyphs.Terminal,
		Key:     "tui:lsp",
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Bar:     true,
		Search:  "Search language servers",
		Intro: func() []string {
			lines := []string{"Language servers check the code the agent writes and tell it what is wrong."}
			if s.loaded && s.err == nil && s.status.GetService() == nil {
				lines = append(lines, "Code checking is off, so no server runs. Turn it on in the first row.")
			}
			return lines
		},
		Banner: func() string {
			if s.status.GetService().GetExcludeRootsInvalid() {
				return "Skipped folders in settings is not a list, so no folder is checked until you fix it."
			}
			return ""
		},
		Columns: []render.Column{{}, {}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			s.rows = s.order()
			cells := make([][]string, len(s.rows))
			for i, r := range s.rows {
				cells[i] = s.cells(r)
			}
			return cells
		},
		Group: func(i int) string { return s.group(s.rows[i]) },
		Words: func(i int) string {
			switch r := s.rows[i]; {
			case r.server != nil:
				return strings.Join(r.server.GetExtensions(), " ") + " " + r.server.GetDescription()
			case r.act != nil:
				return r.act.about
			}
			return "Check the files the agent edits for problems"
		},
		Detail:  func(i int) []string { return s.detail(s.rows[i]) },
		Empty:   func() string { return tui.Waiting(s.loaded, s.err, "icebath knows no language servers.") },
		OnFocus: func(*cards.List) tea.Cmd { return s.load() },
		Free:    map[string]bool{"hub.add": true, "hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(s.rows) {
				return []wm.Hint{{Action: "hub.add", Label: "add server"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: s.verb(s.rows[i])}, {Action: "hub.add", Label: "add server"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				switch r := s.rows[i]; {
				case r.toggle:
					return s.turn(s.status.GetService() == nil)
				case r.act != nil:
					return r.act.run(l)
				case installable(r.server):
					return s.install(r.server)
				default:
					return s.read(r.server)
				}
			},
			"hub.add":     func(*cards.List, int) tea.Cmd { return s.add() },
			"hub.refresh": func(*cards.List, int) tea.Cmd { return s.load() },
		},
	})
	s.list.ToggleDetail()
	s.hub = s.kit.Hub("lsp", s.list)
	return tea.Batch(s.kit.WM.ShowOverlay(s.hub), s.load())
}

// Reads how the servers of the profile stand
func (s *servers) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := s.service.Status(s.kit.Env.Ctx, &lspv1.StatusRequest{Profile: s.profile})
		return func() tea.Cmd {
			s.err, s.loaded = err, true
			if err == nil {
				s.status = response
			}
			s.list.Refresh()
			return nil
		}
	})
}

// Rows once the servers are read: the switch, the servers running, installed, not installed and turned off, each run by name, then the
// rest of what the screen does
func (s *servers) order() []row {
	if s.status == nil {
		return nil
	}
	rows := []row{{toggle: true}}
	servers := slices.Clone(s.status.GetServers())
	place := map[string]int{runningGroup: 0, installedGroup: 1, missingGroup: 2, offGroup: 3}
	slices.SortStableFunc(servers, func(a, b *lspv1.Server) int {
		if d := place[s.kind(a)] - place[s.kind(b)]; d != 0 {
			return d
		}
		return strings.Compare(a.GetId(), b.GetId())
	})
	for _, srv := range servers {
		rows = append(rows, row{server: srv})
	}
	for _, act := range s.actions() {
		rows = append(rows, row{act: act})
	}
	return rows
}

// What the screen does besides the servers: installing every server, stopping the running ones, trusting the folders servers were kept
// out of, what runs now and the settings
func (s *servers) actions() []*action {
	waiting := s.automatic()
	out := []*action{{"Install every server", "Download each server that installs by itself", "install", func(l *cards.List) tea.Cmd {
		if len(waiting) == 0 {
			return s.hub.Done("Every server that installs by itself is installed already.")
		}
		l.Confirm("Install every language server that installs by itself? That downloads "+render.Count(len(waiting), "server")+".", func() tea.Cmd {
			return s.follow("Installing every server", "tui:lsp:install-all", &lspv1.InstallRequest{All: true})
		})
		return nil
	}}, {"Try every server", "Also try the servers you would otherwise install by hand", "install", func(l *cards.List) tea.Cmd {
		l.Confirm("Try to install every language server, those you would install by hand too? Some may need tools you don't have.", func() tea.Cmd {
			return s.follow("Installing every server", "tui:lsp:install-all", &lspv1.InstallRequest{All: true, IncludeManual: true})
		})
		return nil
	}}, {"Stop running servers", "The next edit starts them again", "stop", func(*cards.List) tea.Cmd { return s.restart() }}}
	if roots := s.skippedRoots(); len(roots) > 0 {
		out = append(out, &action{"Trust skipped folders", "Let servers run in " + strings.Join(roots, ", "), "trust", func(l *cards.List) tea.Cmd {
			l.Confirm("Trust "+strings.Join(roots, ", ")+"? Code checkers may then run the project's own programs there.", func() tea.Cmd {
				return s.trust(roots)
			})
			return nil
		}})
	}
	return append(out, &action{"Status", "What runs now, what failed and which folders were skipped", "open", func(*cards.List) tea.Cmd { return s.statusPage() }},
		&action{"Settings", "Open the language server settings", "open", func(*cards.List) tea.Cmd { return s.openSettings(lspconfig.Key) }})
}

// Servers that install by themselves and are not installed yet
func (s *servers) automatic() []string {
	var out []string
	for _, srv := range s.status.GetServers() {
		if srv.GetAutomatic() && srv.GetStatus() != "installed" {
			out = append(out, srv.GetId())
		}
	}
	return out
}

// Folders servers were kept out of because they are not trusted, each named once
func (s *servers) skippedRoots() []string {
	var roots []string
	for _, pair := range s.status.GetService().GetUntrustedSkipped() {
		if !slices.Contains(roots, pair.GetRoot()) {
			roots = append(roots, pair.GetRoot())
		}
	}
	return roots
}

// Heading a row is listed under
func (s *servers) group(r row) string {
	switch {
	case r.toggle:
		return ""
	case r.act != nil:
		return moreGroup
	}
	return s.kind(r.server)
}

// Heading of a server by how it stands: running, installed, not installed, or turned off in the settings
func (s *servers) kind(srv *lspv1.Server) string {
	switch {
	case slices.Contains(s.status.GetService().GetDisabled(), srv.GetId()):
		return offGroup
	case s.client(srv) != nil:
		return runningGroup
	case srv.GetStatus() == "installed":
		return installedGroup
	}
	return missingGroup
}

// Process of the server that runs or starts, nil for none
func (s *servers) client(srv *lspv1.Server) *lspv1.Client {
	var found *lspv1.Client
	for _, c := range s.status.GetService().GetClients() {
		if c.GetServer() != srv.GetId() {
			continue
		}
		if c.GetRunning() {
			return c
		}
		if found == nil && c.GetState() != "stopped" {
			found = c
		}
	}
	return found
}

// Folders where the server failed and is not tried again yet
func (s *servers) failed(srv *lspv1.Server) []string {
	var roots []string
	for _, pair := range s.status.GetService().GetBroken() {
		if pair.GetServer() == srv.GetId() {
			roots = append(roots, pair.GetRoot())
		}
	}
	return roots
}

// Whether enter installs the server: one icebath has a way to install that is not installed yet
func installable(srv *lspv1.Server) bool {
	return srv.GetStatus() != "installed" && !srv.GetCustom()
}

// Word on the button enter presses on the row
func (s *servers) verb(r row) string {
	switch {
	case r.toggle && s.status.GetService() == nil:
		return "turn on"
	case r.toggle:
		return "turn off"
	case r.act != nil:
		return r.act.verb
	case installable(r.server):
		return "install"
	}
	return "open"
}

// Cells of a row: its name, its state and what it is
func (s *servers) cells(r row) []string {
	t := s.kit.Env.Theme
	switch {
	case r.toggle:
		state := "On"
		if s.status.GetService() == nil {
			state = render.Warm("Off", t)
		}
		return []string{"Check code", state, render.Dim("Check the files the agent edits for problems", t)}
	case r.act != nil:
		return []string{r.act.title, "", render.Dim(r.act.about, t)}
	}
	return []string{r.server.GetId(), s.state(r.server), render.Dim(r.server.GetDescription(), t)}
}

// How the server stands, in words coloured by what they mean
func (s *servers) state(srv *lspv1.Server) string {
	t := s.kit.Env.Theme
	switch {
	case slices.Contains(s.status.GetService().GetDisabled(), srv.GetId()):
		return render.Dim("Turned off", t)
	case len(s.failed(srv)) > 0:
		return render.Bad("Failed", t)
	}
	if c := s.client(srv); c != nil {
		switch {
		case c.GetRunning():
			return render.Good("Running", t)
		case c.GetState() == "error":
			return render.Bad("Failed", t)
		}
		return render.Warm("Starting", t)
	}
	switch srv.GetStatus() {
	case "installed":
		return "Installed"
	case "manual-only":
		return render.Warm("Install by hand", t)
	}
	if srv.GetAutomatic() && s.status.GetService().GetInstallStrategy() == lspconfig.InstallAuto {
		return render.Dim("Installs when needed", t)
	}
	return render.Dim("Not installed", t)
}

// Lines under the list about the highlighted row: what it is when its row can't show that, then the facts a person acts on
func (s *servers) detail(r row) []string {
	t := s.kit.Env.Theme
	switch {
	case r.toggle:
		lines := []string{"Check the files the agent edits for problems, using language servers."}
		if st := s.status.GetService(); st != nil {
			lines = append(lines, s.checking(st)...)
		}
		return append(lines, render.Dim("Check code is also in /settings, under Tools › Language servers.", t))
	case r.act != nil:
		return []string{r.act.about + "."}
	}
	var lines []string
	if !s.list.Shows(aboutColumn) {
		lines = append(lines, r.server.GetDescription()+".")
	}
	return append(lines, s.facts(r.server)...)
}

// What a check does now: how long it waits and for what, and how missing servers are installed
func (s *servers) checking(st *lspv1.Service) []string {
	wait := config.Span(time.Duration(st.GetWaitTimeout() * float64(time.Second)))
	what := "the edited file"
	if st.GetWaitMode() == lspconfig.WaitFull {
		what = "a fuller result"
	}
	lines := []string{"A check waits up to " + wait + " for " + what + "."}
	if st.GetInstallStrategy() == lspconfig.InstallAuto {
		lines = append(lines, "A missing server is installed when a file needs it.")
	} else {
		lines = append(lines, "Only servers you installed yourself are used.")
	}
	switch n := len(st.GetClients()); n {
	case 0:
		lines = append(lines, "No server runs yet. One starts with the first edit of a file it checks.")
	default:
		lines = append(lines, render.Count(n, "server")+" running now.")
	}
	return lines
}

// Facts about a server: the files it checks, where it runs from or how it gets installed, where it runs, failed or was kept out, and
// whether the settings turn it off
func (s *servers) facts(srv *lspv1.Server) []string {
	t := s.kit.Env.Theme
	lines := []string{render.Dim("Checks "+strings.Join(srv.GetExtensions(), " ")+" files.", t)}
	if path := srv.GetPath(); path != "" {
		lines = append(lines, "Runs from "+path+".")
	} else {
		lines = append(lines, render.Warm(s.howToInstall(srv), t))
	}
	if c := s.client(srv); c != nil {
		folders := c.GetFolders()
		if len(folders) == 0 {
			folders = []string{c.GetRoot()}
		}
		lines = append(lines, render.Good("Running in "+strings.Join(folders, ", ")+".", t))
	}
	if roots := s.failed(srv); len(roots) > 0 {
		retry := "It is not tried again until icebath restarts."
		if seconds := s.status.GetService().GetBrokenRetrySeconds(); seconds > 0 {
			retry = "It is tried again after " + config.Span(time.Duration(seconds*float64(time.Second))) + "."
		}
		lines = append(lines, render.Bad("Failed in "+strings.Join(roots, ", ")+". "+retry, t))
	}
	if !srv.GetUntrustedSafe() {
		lines = append(lines, render.Dim("Runs only in folders you trust, since it runs the project's own programs.", t))
	}
	for _, pair := range s.status.GetService().GetUntrustedSkipped() {
		if pair.GetServer() == srv.GetId() {
			lines = append(lines, render.Warm("Kept out of "+pair.GetRoot()+", which is not trusted. Trust skipped folders lets it run there.", t))
		}
	}
	if slices.Contains(s.status.GetService().GetDisabled(), srv.GetId()) {
		lines = append(lines, render.Dim("Turned off under Servers in settings.", t))
	}
	return lines
}

// How the server gets installed, for one that is not
func (s *servers) howToInstall(srv *lspv1.Server) string {
	switch {
	case srv.GetCustom():
		return "You added this server under Servers in settings. Install the program it runs yourself."
	case srv.GetAutomatic():
		switch s.status.GetService().GetInstallStrategy() {
		case lspconfig.InstallAuto:
			return "Not installed. It installs by itself the first time a file needs it."
		case lspconfig.InstallManual:
			return "Not installed. Install it here, since Install servers in settings is Manual."
		}
		return "Not installed. Install it here."
	case srv.GetStatus() == "manual-only":
		return "Not installed. This server is installed by hand, or give its command under Servers in settings."
	}
	return "Not found on your PATH. Install it, or give its command under Servers in settings."
}

// Pages what the server checks, where it runs from and how it stands
func (s *servers) read(srv *lspv1.Server) tea.Cmd {
	return s.hub.Push(cards.NewPage(s.kit.Env, cards.PageSpec{
		Title: srv.GetId(),
		Key:   "tui:lsp:" + srv.GetId(),
		Lines: func(int) []string {
			for _, now := range s.status.GetServers() {
				if now.GetId() == srv.GetId() {
					srv = now
				}
			}
			return append([]string{srv.GetDescription() + ".", ""}, s.facts(srv)...)
		},
	}))
}

// Runs an install on a page that follows it, carried on to its end when the page is closed and told then
func (s *servers) follow(title, key string, request *lspv1.InstallRequest) tea.Cmd {
	var lines []string
	partial := ""
	ended := false
	page := cards.NewPage(s.kit.Env, cards.PageSpec{
		Title:  title,
		Key:    key,
		Follow: true,
		Lines: func(int) []string {
			var out []string
			for _, line := range append(slices.Clone(lines), partial) {
				if line != "" {
					out = append(out, line)
				}
			}
			if len(out) == 0 {
				out = append(out, render.Dim("Starting…", s.kit.Env.Theme))
			}
			return out
		},
	})
	finish := func(text string, failed bool) {
		s.kit.Apply(func() tea.Cmd {
			if ended {
				return nil
			}
			ended = true
			if partial != "" {
				lines, partial = append(lines, partial), ""
			}
			if failed {
				lines = append(lines, "", render.Bad(text, s.kit.Env.Theme))
				return tea.Batch(s.load(), s.hub.Fail(text))
			}
			lines = append(lines, "", render.Good(text, s.kit.Env.Theme))
			return tea.Batch(s.load(), s.hub.Done(text))
		})
	}
	ctx := s.kit.Env.Ctx
	request.Profile = s.profile
	what := request.GetServer()
	if request.GetAll() {
		what = "the servers"
	}
	go func() {
		stream, err := s.service.Install(ctx, request)
		if err != nil {
			finish("Couldn't install "+what+": "+tui.Reason(err), true)
			return
		}
		defer stream.Close()
		for {
			event, err := stream.Receive()
			switch {
			case errors.Is(err, io.EOF):
				finish("The install stopped without saying whether it worked. Check the output above.", true)
				return
			case err != nil:
				if ctx.Err() == nil {
					finish("Couldn't install "+what+": "+tui.Reason(err), true)
				}
				return
			}
			switch kind := event.GetKind().(type) {
			case *lspv1.InstallEvent_ExitCode:
				switch {
				case kind.ExitCode != 0:
					finish("Installing "+what+" failed. The output above says why.", true)
				case request.GetAll():
					finish("Installed the servers.", false)
				default:
					finish("Installed "+what+".", false)
				}
				return
			case *lspv1.InstallEvent_Out:
				s.kit.Apply(func() tea.Cmd {
					parts := strings.Split(partial+kind.Out, "\n")
					lines, partial = append(lines, parts[:len(parts)-1]...), parts[len(parts)-1]
					return nil
				})
			case *lspv1.InstallEvent_Err:
				s.kit.Apply(func() tea.Cmd {
					lines = append(lines, render.Bad(kind.Err, s.kit.Env.Theme))
					return nil
				})
			}
		}
	}()
	return s.hub.Push(page)
}

func (s *servers) install(srv *lspv1.Server) tea.Cmd {
	return s.follow("Installing "+srv.GetId(), "tui:lsp:install:"+srv.GetId(), &lspv1.InstallRequest{Server: srv.GetId()})
}

// Pages what a check does, which servers run, which failed, which were kept out of folders that are not trusted, which the settings turn
// off, the folders skipped and what the servers warned of
func (s *servers) statusPage() tea.Cmd {
	t := s.kit.Env.Theme
	page := cards.NewPage(s.kit.Env, cards.PageSpec{
		Title: "Status",
		Key:   "tui:lsp:status",
		Lines: func(int) []string {
			st := s.status.GetService()
			if st == nil {
				return []string{"Code checking is off, so no language server runs. Turn on Check code on the list to have servers check the files the agent writes."}
			}
			lines := s.checking(st)
			lines = append(lines, "", render.Heading("Running", t))
			if len(st.GetClients()) == 0 {
				lines = append(lines, render.Dim("None yet. A server starts with the first edit of a file it checks.", t))
			}
			for _, c := range st.GetClients() {
				folders := c.GetFolders()
				if len(folders) == 0 {
					folders = []string{c.GetRoot()}
				}
				lines = append(lines, c.GetServer()+render.Dim(" · "+clientState(c)+" · "+strings.Join(folders, ", "), t))
			}
			if broken := st.GetBroken(); len(broken) > 0 {
				lines = append(lines, "", render.Heading("Failed", t))
				for _, b := range broken {
					lines = append(lines, render.Bad(b.GetServer(), t)+render.Dim(" in "+b.GetRoot(), t))
				}
			}
			if skipped := st.GetUntrustedSkipped(); len(skipped) > 0 {
				lines = append(lines, "", render.Heading("Kept out of folders you don't trust", t))
				for _, u := range skipped {
					lines = append(lines, u.GetServer()+render.Dim(" in "+u.GetRoot(), t))
				}
				lines = append(lines, render.Dim("Trust skipped folders on the list lets them run there.", t))
			}
			if disabled := st.GetDisabled(); len(disabled) > 0 {
				lines = append(lines, "", render.Heading("Turned off", t), strings.Join(disabled, ", "))
			}
			if st.GetExcludeRootsInvalid() {
				lines = append(lines, "", render.Bad("Skipped folders in settings is not a list, so no folder is checked until you fix it.", t))
			} else if excludes := st.GetExcludeRoots(); len(excludes) > 0 {
				lines = append(lines, "", render.Heading("Skipped folders", t), strings.Join(excludes, ", "))
			}
			if warnings := s.status.GetWarnings(); len(warnings) > 0 {
				lines = append(lines, "", render.Heading("Warnings", t))
				for _, warning := range warnings {
					lines = append(lines, render.Warm(warning, t))
				}
			}
			return lines
		},
	})
	return tea.Batch(s.load(), s.hub.Push(page))
}

// How a server process stands, in a word
func clientState(c *lspv1.Client) string {
	switch {
	case c.GetRunning():
		return "running"
	case c.GetState() == "starting":
		return "starting"
	case c.GetState() == "error":
		return "failed"
	}
	return "stopped"
}

func (s *servers) restart() tea.Cmd {
	s.hub.Busy("Stopping the language servers…")
	return tui.Call(s.hub, func() (*lspv1.RestartResponse, error) {
		return s.service.Restart(s.kit.Env.Ctx, &lspv1.RestartRequest{})
	}, func(*lspv1.RestartResponse) tea.Cmd {
		return tea.Batch(s.load(), s.hub.Done("Stopped the language servers. The next edit starts them again."))
	})
}

// Turns code checking on or off
func (s *servers) turn(on bool) tea.Cmd {
	value := strconv.FormatBool(on)
	return s.set(lspconfig.Enabled, value, "Check code is now "+map[bool]string{true: "On", false: "Off"}[on]+".")
}

// Adds the folders to those trusted, so servers may run the project's own programs there
func (s *servers) trust(roots []string) tea.Cmd {
	folders := slices.Clone(s.status.GetService().GetTrustedWorkspaces())
	for _, root := range roots {
		if !slices.Contains(folders, root) {
			folders = append(folders, root)
		}
	}
	value, err := yaml.Marshal(folders)
	if err != nil {
		return s.hub.Fail(err.Error())
	}
	told := "Trusted " + strings.Join(roots, ", ") + ". Servers run there from the next edit."
	return s.set(toolsconfig.TrustedWorkspaces, strings.TrimSpace(string(value)), told)
}

// Saves a setting of the profile, tells the screens that follow it, then reads the servers again
func (s *servers) set(key, value, told string) tea.Cmd {
	s.hub.Busy("Saving…")
	return tui.Call(s.hub, func() (*configv1.SetResponse, error) {
		return s.settings.Set(s.kit.Env.Ctx, &configv1.SetRequest{Profile: s.profile, Key: key, Value: value})
	}, func(*configv1.SetResponse) tea.Cmd {
		cmds := []tea.Cmd{s.hub.Done(told), s.load()}
		if s.profile == "" {
			cmds = append(cmds, s.kit.Changed(key, value))
		}
		return tea.Batch(cmds...)
	})
}

// Opens the servers setting, where a server is added by its name, its command and the files it checks
func (s *servers) add() tea.Cmd {
	return tea.Batch(s.kit.WM.DismissCard(s.hub), s.kit.Open(configapp.Edit, profile.Join(s.profile, lspconfig.Servers)))
}

// Opens a section of the settings in place of the screen
func (s *servers) openSettings(section string) tea.Cmd {
	return tea.Batch(s.kit.WM.DismissCard(s.hub), s.kit.Open(configapp.Settings, profile.Join(s.profile, section)))
}

// The language servers screen: every server icebath knows and how it stands, the service of the profile, installing servers, and stopping
// the running ones
package app

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	lspconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/lsp/config"
	"github.com/nickheyer/icebath/internal/profile"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
	lspv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/lsp/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/lsp/v1/lspv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the servers list
const serversContext = "tui.lsp"

var actions = []keys.Action{
	{ID: "tui.lsp.read", Context: serversContext, Label: "read", Doc: "read what the language server checks, where it runs from and how it installs", Default: []string{"enter"}},
	{ID: "tui.lsp.install", Context: serversContext, Label: "install", Doc: "install the language server", Default: []string{"i"}},
	{ID: "tui.lsp.all", Context: serversContext, Label: "install all", Doc: "install every language server that installs on its own", Default: []string{"a"}},
	{ID: "tui.lsp.status", Context: serversContext, Label: "status", Doc: "which language servers run, and those that failed or were kept out", Default: []string{"s"}},
	{ID: "tui.lsp.restart", Context: serversContext, Label: "restart", Doc: "stop the running servers; the next edit starts them again", Default: []string{"r"}},
	{ID: "tui.lsp.settings", Context: serversContext, Label: "settings", Doc: "open the language server settings", Default: []string{"e"}},
}

type screens struct{}

func Register(registry *app.Registry) {
	registry.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := lspv1connect.NewLspServiceClient(conn)
	return []tui.Screen{{
		ID:      "lsp",
		Title:   "Language servers",
		Short:   "Servers that check the code the agent writes: after it writes a file, a language server for that language reports errors back to it",
		Slash:   []string{"lsp"},
		Usage:   "[@profile]",
		Actions: actions,
		Where:   map[string]string{serversContext: "language servers"},
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			name, _ := profile.Split(argument)
			return (&servers{kit: kit, service: service, profile: name}).open()
		},
	}}
}

type servers struct {
	kit     *tui.Kit
	service lspv1connect.LspServiceClient
	// Profile the screen was opened on, the one in use when empty
	profile string
	hub     *cards.Hub
	list    *cards.List
	status  *lspv1.StatusResponse
	loaded  bool
	err     error
}

func (s *servers) rows() []*lspv1.Server { return s.status.GetServers() }

func (s *servers) open() tea.Cmd {
	title := "Language servers"
	if s.profile != "" {
		title += " of " + s.profile
	}
	s.list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   title,
		Glyph:   s.kit.Env.Theme.Glyphs.Terminal,
		Key:     "tui:lsp",
		Context: serversContext,
		Primary: "tui.lsp.read",
		Columns: []render.Column{{Title: "Server"}, {Title: "State"}, {Title: "Files", Max: 28}, {Title: "About", Flex: 3}},
		Yield:   []int{2, 3},
		Rows: func() [][]string {
			rows := make([][]string, len(s.rows()))
			for i, srv := range s.rows() {
				rows[i] = []string{srv.GetId(), s.state(srv), strings.Join(srv.GetExtensions(), " "), srv.GetDescription()}
			}
			return rows
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(s.rows()) {
				return s.summary()
			}
			return s.detail(s.rows()[i])
		},
		Empty:   func() string { return tui.Waiting(s.loaded, s.err, "No language servers.") },
		OnFocus: func(*cards.List) tea.Cmd { return s.load() },
		Free:    map[string]bool{"tui.lsp.all": true, "tui.lsp.status": true, "tui.lsp.restart": true, "tui.lsp.settings": true},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"tui.lsp.read":     func(_ *cards.List, i int) tea.Cmd { return s.read(s.rows()[i]) },
			"tui.lsp.install":  func(_ *cards.List, i int) tea.Cmd { return s.install(s.rows()[i]) },
			"tui.lsp.all":      func(l *cards.List, _ int) tea.Cmd { return s.installAll(l) },
			"tui.lsp.status":   func(*cards.List, int) tea.Cmd { return s.statusPage() },
			"tui.lsp.restart":  func(*cards.List, int) tea.Cmd { return s.restart() },
			"tui.lsp.settings": func(*cards.List, int) tea.Cmd { return s.settings() },
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

// Whether the server is installed, coloured by how it stands, with running when a client of it runs
func (s *servers) state(srv *lspv1.Server) string {
	t := s.kit.Env.Theme
	for _, c := range s.status.GetService().GetClients() {
		if c.GetServer() == srv.GetId() && c.GetRunning() {
			return render.Good("running", t)
		}
	}
	switch srv.GetStatus() {
	case "installed":
		return render.Good("installed", t)
	case "manual-only":
		return render.Warm("install by hand", t)
	}
	if st := s.status.GetService(); srv.GetAutomatic() && st != nil && st.GetInstallStrategy() == lspconfig.InstallAuto {
		return render.Dim("installs when needed", t)
	}
	return render.Dim("missing", t)
}

// How the service stands in a line or two, for the list's detail with no row
func (s *servers) summary() []string {
	t := s.kit.Env.Theme
	st := s.status.GetService()
	if st == nil {
		return []string{render.Dim("Code checking is off ("+lspconfig.Enabled+"); e opens the settings.", t)}
	}
	return []string{render.Dim(fmt.Sprintf("Code checking is on: %d running · waits %ss for %s · installs %s", len(st.GetClients()),
		strconv.FormatFloat(st.GetWaitTimeout(), 'f', -1, 64), st.GetWaitMode(), st.GetInstallStrategy()), t)}
}

// Where the server runs from and whether it runs in untrusted folders
func (s *servers) detail(srv *lspv1.Server) []string {
	t := s.kit.Env.Theme
	var lines []string
	if path := srv.GetPath(); path != "" {
		lines = append(lines, "Runs "+path)
	} else {
		lines = append(lines, render.Dim(s.howToInstall(srv), t))
	}
	if !srv.GetUntrustedSafe() {
		lines = append(lines, render.Dim("Runs in trusted workspaces only ("+toolsconfig.TrustedWorkspaces+")", t))
	}
	return lines
}

// How the server gets installed
func (s *servers) howToInstall(srv *lspv1.Server) string {
	switch {
	case srv.GetCustom():
		return "Declared in " + lspconfig.Servers + "; install the command it gives yourself"
	case srv.GetAutomatic():
		return "Not installed; i installs it, and the first edit that needs it does too while " + lspconfig.InstallStrategy + " is auto"
	case srv.GetRecipe() != "":
		return "Not installed; this server installs by hand, or set " + lspconfig.Servers + "." + srv.GetId() + ".command"
	}
	return "Not found on PATH; install it, or set " + lspconfig.Servers + "." + srv.GetId() + ".command"
}

// Pages what the server checks, where it runs from and how it installs
func (s *servers) read(srv *lspv1.Server) tea.Cmd {
	t := s.kit.Env.Theme
	page := cards.NewPage(s.kit.Env, cards.PageSpec{
		Title: srv.GetId(),
		Key:   "tui:lsp:" + srv.GetId(),
		Lines: func(w int) []string {
			width := max(w, 1)
			var lines []string
			add := func(text string) { lines = append(lines, strings.Split(render.Wrap(text, width), "\n")...) }
			add(srv.GetDescription())
			add(render.Dim("Files: "+strings.Join(srv.GetExtensions(), " "), t))
			lines = append(lines, "")
			for _, line := range s.detail(srv) {
				add(line)
			}
			if srv.GetPath() == "" && srv.GetRecipe() != "" {
				add(render.Dim("Recipe: "+srv.GetRecipe(), t))
			}
			lines = append(lines, "")
			add(render.Dim("i installs it · r stops the running servers · e opens the settings", t))
			return lines
		},
	})
	return s.hub.Push(page)
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
		Lines: func(w int) []string {
			var out []string
			for _, line := range append(append([]string{}, lines...), partial) {
				if line != "" {
					out = append(out, strings.Split(render.Wrap(line, max(w, 1)), "\n")...)
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
				lines = append(lines, render.Bad(text, s.kit.Env.Theme))
				return tea.Batch(s.load(), s.hub.Fail(text))
			}
			lines = append(lines, render.Good(text, s.kit.Env.Theme))
			return tea.Batch(s.load(), s.hub.Done(text))
		})
	}
	ctx := s.kit.Env.Ctx
	request.Profile = s.profile
	go func() {
		stream, err := s.service.Install(ctx, request)
		if err != nil {
			finish(tui.Reason(err), true)
			return
		}
		defer stream.Close()
		for {
			event, err := stream.Receive()
			switch {
			case errors.Is(err, io.EOF):
				finish("The install ended without saying how it went", true)
				return
			case err != nil:
				if ctx.Err() == nil {
					finish(tui.Reason(err), true)
				}
				return
			}
			switch kind := event.GetKind().(type) {
			case *lspv1.InstallEvent_ExitCode:
				if kind.ExitCode != 0 {
					finish(title+" failed", true)
				} else {
					finish(title+" done", false)
				}
				return
			case *lspv1.InstallEvent_Out:
				s.kit.Apply(func() tea.Cmd {
					text := partial + kind.Out
					parts := strings.Split(text, "\n")
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
	if srv.GetStatus() == "installed" {
		return wm.Notify(srv.GetId()+" is installed already: "+srv.GetPath(), wm.Info)
	}
	if srv.GetCustom() {
		return wm.Notify(s.howToInstall(srv), wm.Info)
	}
	return s.follow("Installing "+srv.GetId(), "tui:lsp:install:"+srv.GetId(), &lspv1.InstallRequest{Server: srv.GetId()})
}

func (s *servers) installAll(l *cards.List) tea.Cmd {
	l.Confirm("Download and install every language server that installs on its own?", func() tea.Cmd {
		return s.follow("Installing every server", "tui:lsp:install-all", &lspv1.InstallRequest{All: true})
	})
	return nil
}

// Pages which servers run, which failed and which were kept out of untrusted folders
func (s *servers) statusPage() tea.Cmd {
	t := s.kit.Env.Theme
	page := cards.NewPage(s.kit.Env, cards.PageSpec{
		Title: "Language server status",
		Key:   "tui:lsp:status",
		Lines: func(w int) []string {
			width := max(w, 1)
			var lines []string
			add := func(text string) { lines = append(lines, strings.Split(render.Wrap(text, width), "\n")...) }
			st := s.status.GetService()
			if st == nil {
				add("Code checking is off. Turn on " + lspconfig.Enabled + " to have language servers check the files the agent writes.")
				return lines
			}
			add(fmt.Sprintf("Waits up to %ss for the %s · installs %s", strconv.FormatFloat(st.GetWaitTimeout(), 'f', -1, 64), map[string]string{
				lspconfig.WaitDocument: "edited file", lspconfig.WaitFull: "full result"}[st.GetWaitMode()], st.GetInstallStrategy()))
			lines = append(lines, "", render.Heading("Running", t))
			if len(st.GetClients()) == 0 {
				add(render.Dim("None yet: a server starts with the first edit of a file it checks.", t))
			}
			for _, c := range st.GetClients() {
				add(c.GetServer() + " · " + c.GetState() + " · " + strings.Join(c.GetFolders(), ", "))
			}
			if broken := st.GetBroken(); len(broken) > 0 {
				lines = append(lines, "", render.Heading("Failed", t))
				for _, b := range broken {
					add(render.Bad(b.GetServer(), t) + " in " + b.GetRoot())
				}
			}
			if skipped := st.GetUntrustedSkipped(); len(skipped) > 0 {
				lines = append(lines, "", render.Heading("Kept out of untrusted folders", t))
				for _, u := range skipped {
					add(u.GetServer() + " in " + u.GetRoot())
				}
				add(render.Dim("Add the folder to "+toolsconfig.TrustedWorkspaces+" to run them there.", t))
			}
			if disabled := st.GetDisabled(); len(disabled) > 0 {
				lines = append(lines, "", render.Heading("Turned off", t))
				add(strings.Join(disabled, ", "))
			}
			if st.GetExcludeRootsInvalid() {
				lines = append(lines, "")
				add(render.Bad(lspconfig.ExcludeRoots+" is not a list: every workspace is skipped until it is fixed.", t))
			} else if excludes := st.GetExcludeRoots(); len(excludes) > 0 {
				lines = append(lines, "", render.Heading("Skipped folders", t))
				add(strings.Join(excludes, ", "))
			}
			if warnings := s.status.GetWarnings(); len(warnings) > 0 {
				lines = append(lines, "", render.Heading("Warnings", t))
				for _, warning := range warnings {
					add(render.Warm("! ", t) + warning)
				}
			}
			return lines
		},
	})
	return tea.Batch(s.load(), s.hub.Push(page))
}

func (s *servers) restart() tea.Cmd {
	return tui.Call(s.hub, func() (*lspv1.RestartResponse, error) {
		return s.service.Restart(s.kit.Env.Ctx, &lspv1.RestartRequest{})
	}, func(*lspv1.RestartResponse) tea.Cmd {
		return tea.Batch(s.load(), wm.Notify("Language servers stopped; the next edit starts them again", wm.Success))
	})
}

// Opens the language server settings
func (s *servers) settings() tea.Cmd {
	argument := lspconfig.Key
	if s.profile != "" {
		argument = "@" + s.profile + " " + argument
	}
	return tea.Batch(s.kit.WM.DismissCard(s.hub), s.kit.Open(configapp.Settings, argument))
}

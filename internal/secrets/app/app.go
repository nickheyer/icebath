// The secret stores screen: every store a profile's variables come from, what each set and what can be done with it, the stores that
// bring a part of their own showing it in place of their plain row
package app

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	secretsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/secrets/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/secrets/v1/secretsv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// What a store shows on the screen and what can be done with it
type Store struct {
	// Name of the secret store its rows are of, whose plain row they take the place of
	Source string
	// Rows of the store, read again each time the screen is
	Rows func(ctx context.Context) ([]Row, error)
	// What can be added to the store from the screen
	Adds []Action
}

// One row: a name, a status and where it comes from, the lines of its detail, what it can do when opened and how it is removed
type Row struct {
	Cells  [3]string
	Detail []string
	Open   []Action
	// Question asked before Remove runs, nil Remove for a row that can't be removed
	Confirm string
	Remove  func(s *Screen) tea.Cmd
}

// Something a row or the store can do: a label, a line saying what, and the run of it
type Action struct {
	Label, Does string
	Run         func(s *Screen) tea.Cmd
}

type Registrant interface {
	Stores(conn *connect.Client) []Store
}

type Registry struct {
	registrants []Registrant
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Stores(conn *connect.Client) []Store {
	var stores []Store
	for _, registrant := range r.registrants {
		stores = append(stores, registrant.Stores(conn)...)
	}
	return stores
}

// What the stores bring to the screen
type Stores struct {
	Registry
}

func New() *Stores {
	return &Stores{}
}

type screens struct {
	stores *Stores
}

func Register(host *app.App, stores *Stores) {
	host.Register(screens{stores: stores})
}

func (s screens) Screens(conn *connect.Client) []tui.Screen {
	service := secretsv1connect.NewSecretServiceClient(conn)
	return []tui.Screen{{
		ID:    "secrets",
		Title: "Secret stores",
		Short: "Bitwarden and 1Password as sources of keys",
		Slash: []string{"secrets"},
		Open: func(kit *tui.Kit, _ string) tea.Cmd {
			screen := &Screen{Kit: kit, service: service, stores: s.stores.Stores(conn)}
			return screen.open()
		},
	}}
}

// The open screen, which the actions of the stores work through
type Screen struct {
	Kit     *tui.Kit
	Hub     *cards.Hub
	service secretsv1connect.SecretServiceClient
	stores  []Store
	list    *cards.List
	rows    []Row
	failed  []string
	loaded  bool
	err     error
}

func (s *Screen) open() tea.Cmd {
	t := s.Kit.Env.Theme
	s.list = cards.NewList(s.Kit.Env, cards.ListSpec{
		Title:   "Secret stores",
		Glyph:   t.Glyphs.Settings,
		Key:     "secrets",
		Context: cards.HubContext,
		Primary: "hub.use",
		Columns: []render.Column{{Title: "Name", Max: 36}, {Title: "Status", Max: 24}, {Title: "From", Flex: 1}},
		Yield:   []int{2, 1},
		Rows: func() [][]string {
			out := make([][]string, len(s.rows))
			for i, row := range s.rows {
				out[i] = row.Cells[:]
			}
			return out
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(s.rows) {
				return nil
			}
			return s.rows[i].Detail
		},
		Banner: func() string {
			if len(s.failed) == 0 {
				return ""
			}
			return render.Warm(strings.Join(s.failed, " · "), t)
		},
		Empty: func() string { return tui.Waiting(s.loaded, s.err, "No secret stores are installed.") },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			var out []wm.Hint
			if i >= 0 && i < len(s.rows) {
				if len(s.rows[i].Open) > 0 {
					out = append(out, wm.Hint{Action: "hub.use", Label: "open"})
				}
				if s.rows[i].Remove != nil {
					out = append(out, wm.Hint{Action: "hub.remove"})
				}
			}
			if len(s.adds()) > 0 {
				out = append(out, wm.Hint{Action: "hub.add"})
			}
			return out
		},
		Free: map[string]bool{"hub.add": true, "hub.refresh": true},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.refresh": func(*cards.List, int) tea.Cmd { return s.Reload() },
			"hub.use":     func(_ *cards.List, i int) tea.Cmd { return s.use(i) },
			"hub.add": func(*cards.List, int) tea.Cmd {
				switch adds := s.adds(); len(adds) {
				case 0:
					return nil
				case 1:
					return adds[0].Run(s)
				default:
					return s.Actions("Add", adds)
				}
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if i < 0 || i >= len(s.rows) || s.rows[i].Remove == nil {
					return nil
				}
				row := s.rows[i]
				l.Confirm(row.Confirm, func() tea.Cmd { return row.Remove(s) })
				return nil
			},
		},
		OnFocus: func(*cards.List) tea.Cmd { return s.Reload() },
	})
	s.Hub = s.Kit.Hub("secrets", s.list)
	return tea.Batch(s.Kit.WM.ShowOverlay(s.Hub), s.Reload())
}

// What can be added to every store
func (s *Screen) adds() []Action {
	var out []Action
	for _, store := range s.stores {
		out = append(out, store.Adds...)
	}
	return out
}

// Reads every store again, a store that fails named over the list
func (s *Screen) Reload() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		ctx := s.Kit.Env.Ctx
		var rows []Row
		var failed []string
		covered := map[string]bool{}
		for _, store := range s.stores {
			covered[store.Source] = true
			got, err := store.Rows(ctx)
			if err != nil {
				failed = append(failed, store.Source+": "+tui.Reason(err))
				continue
			}
			rows = append(rows, got...)
		}
		sources, err := s.service.Sources(ctx, &secretsv1.SourcesRequest{})
		if err == nil {
			for _, source := range sources.GetSources() {
				if !covered[source.GetName()] {
					rows = append(rows, s.plain(source))
				}
			}
		}
		return func() tea.Cmd {
			s.rows, s.failed, s.loaded, s.err = rows, failed, true, err
			s.list.Refresh()
			return nil
		}
	})
}

// On or off, in a word
func State(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// Row of a store with no part of its own: whether it is on and what its last fetch did, turned on or off and fetched again from it
func (s *Screen) plain(source *secretsv1.Source) Row {
	detail := []string{source.GetLabel() + " · " + State(source.GetEnabled())}
	if n := len(source.GetApplied()); n > 0 {
		detail = append(detail, fmt.Sprintf("Set %s: %s", render.Count(n, "variable"), strings.Join(source.GetApplied(), ", ")))
	}
	if len(source.GetExisting()) > 0 {
		detail = append(detail, "Kept as they were set: "+strings.Join(source.GetExisting(), ", "))
	}
	if source.GetError() != "" {
		detail = append(detail, source.GetError())
		if source.GetRemedy() != "" {
			detail = append(detail, "→ "+source.GetRemedy())
		}
	}
	detail = append(detail, source.GetWarnings()...)
	name, on := source.GetName(), !source.GetEnabled()
	toggle := Action{Label: "Turn on", Does: "Fetch its secrets as icebath starts", Run: func(s *Screen) tea.Cmd {
		return s.Change("Saving…", func(ctx context.Context) (Outcome, error) {
			_, err := s.service.Enable(ctx, &secretsv1.EnableRequest{Name: name, On: on})
			return Outcome{Note: source.GetLabel() + " is " + State(on) + "."}, err
		})
	}}
	if !on {
		toggle.Label, toggle.Does = "Turn off", "Stop fetching its secrets"
	}
	again := Action{Label: "Fetch again", Does: "Ask every store for its secrets now", Run: func(s *Screen) tea.Cmd {
		return s.Change("Fetching…", func(ctx context.Context) (Outcome, error) {
			_, err := s.service.Reload(ctx, &secretsv1.ReloadRequest{})
			return Outcome{Note: "Fetched again."}, err
		})
	}}
	return Row{Cells: [3]string{source.GetLabel(), State(source.GetEnabled()), ""}, Detail: detail, Open: []Action{toggle, again}}
}

func (s *Screen) use(i int) tea.Cmd {
	if i < 0 || i >= len(s.rows) || len(s.rows[i].Open) == 0 {
		return nil
	}
	row := s.rows[i]
	return s.Actions(row.Cells[0], row.Open)
}

// Opens the actions over the screen to pick one to run
func (s *Screen) Actions(title string, actions []Action) tea.Cmd {
	choices := make([]tui.Choice, len(actions))
	for i, action := range actions {
		choices[i] = tui.Choice{ID: action.Label, Title: action.Label, Detail: action.Does}
	}
	return s.Kit.Pick(s.Hub, title, "run", choices, "", func(choice tui.Choice) tea.Cmd {
		for _, action := range actions {
			if action.Label == choice.ID {
				return action.Run(s)
			}
		}
		return nil
	})
}

// What a change did: a page of lines under its title, or a note when there are none
type Outcome struct {
	Title string
	Lines []string
	Note  string
}

// Runs a change, which may install a program and ask a store over the network, in the background, back on the list with a busy note;
// then pages what it did or says its note, and reads the stores again
func (s *Screen) Change(busy string, work func(ctx context.Context) (Outcome, error)) tea.Cmd {
	back := s.Hub.PopTo(s.list)
	s.Hub.Busy(busy)
	return tea.Batch(back, tui.Async(func() func() tea.Cmd {
		outcome, err := work(s.Kit.Env.Ctx)
		return func() tea.Cmd {
			if err != nil {
				return tea.Batch(s.Hub.Fail(tui.Reason(err)), s.Reload())
			}
			s.Hub.Quiet()
			if len(outcome.Lines) == 0 {
				return tea.Batch(s.Hub.Done(outcome.Note), s.Reload())
			}
			return tea.Batch(s.Page(outcome.Title, outcome.Lines), s.Reload())
		}
	}))
}

// Pages lines over the screen, each wrapped to the width
func (s *Screen) Page(title string, lines []string) tea.Cmd {
	page := cards.NewPage(s.Kit.Env, cards.PageSpec{
		Title: title,
		Key:   "secrets:page:" + title,
		Lines: func(w int) []string {
			var out []string
			for _, line := range lines {
				out = append(out, strings.Split(render.Wrap(line, max(w, 1)), "\n")...)
			}
			return out
		},
	})
	return s.Hub.Push(page)
}

// Opens a form over the screen, done handed its values once they check out
func (s *Screen) Form(spec cards.FormSpec, done func(values map[string]string) tea.Cmd) tea.Cmd {
	var form *cards.Form
	spec.Done = func(_ *cards.Form, values map[string]string) tea.Cmd {
		return tea.Batch(s.Hub.Close(form), done(values))
	}
	form = cards.NewForm(s.Kit.Env, spec)
	return s.Hub.Push(form)
}

// Lines of what each secret a store hands over does: export, override, skip_set, skip_token or unresolved, exported or overrode once a
// sync set it
func (s *Screen) Preview(rows [][2]string, warnings []string) []string {
	t := s.Kit.Env.Theme
	var lines []string
	for _, row := range rows {
		name, action := row[0], row[1]
		switch action {
		case "export":
			lines = append(lines, render.Good(name+": set from the store", t))
		case "override":
			lines = append(lines, render.Good(name+": replaces the value set now", t))
		case "exported":
			lines = append(lines, render.Good(name+": set now", t))
		case "overrode":
			lines = append(lines, render.Good(name+": set now, replacing the value set", t))
		case "skip_set":
			lines = append(lines, render.Dim(name+": left alone, it is already set", t))
		case "skip_token":
			lines = append(lines, render.Dim(name+": left alone, it is the token itself", t))
		default:
			lines = append(lines, render.Bad(name+": could not be read", t))
		}
	}
	if len(rows) == 0 {
		lines = append(lines, "The store handed over nothing.")
	}
	for _, warning := range warnings {
		lines = append(lines, render.Warm("⚠ "+warning, t))
	}
	return lines
}

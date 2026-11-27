// The secret stores screen: every store a profile's variables come from, what each set and what can be done with it, the stores that
// bring a part of their own showing it in place of their plain row
package app

import (
	"context"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/internal/config"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	secretsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/secrets/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/secrets/v1/secretsv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Section of the settings of the secret stores, opened from the last row
const section = "secrets"

// Heading of the rows of the stores, and of the row that opens their settings
const (
	storesGroup   = "Stores"
	settingsGroup = "More"
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

// One row: a name and its state, the heading it is listed under, the lines of its detail, what it can do when opened and how it is removed
type Row struct {
	Name, State string
	// Heading of the rows of its kind, the stores when empty
	Group  string
	Detail []string
	Open   []Action
	// Question asked before Remove runs, nil Remove for a row that can't be removed
	Confirm string
	Remove  func(s *Screen) tea.Cmd
	// Words a search reads besides the name and the state
	Words string
	// Name of the store the row is the main row of, whose last fetch its detail tells; empty for any other row
	Store string
}

// Something a row or the store can do: its name, a phrase saying what it does, the word on its button, and the run of it
type Action struct {
	Label, Does, Verb string
	Run               func(s *Screen) tea.Cmd
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
		Group: tui.GroupSecrets,
		Order: 10,
		Title: "Secret stores",
		Short: "Load API keys from a password manager or a command",
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
	// Lists stacked over the first one, refreshed whenever the stores are read
	views  []*cards.List
	loaded bool
}

// The row that opens the settings of the secret stores, last on the screen
func (s *Screen) settingsRow() Row {
	return Row{Name: "Settings", State: "Store order, your own command and more", Group: settingsGroup,
		Detail: []string{"Every setting of the secret stores, on the settings screen."},
		Open: []Action{{Label: "Settings", Verb: "open", Run: func(s *Screen) tea.Cmd {
			return tea.Batch(s.Kit.WM.DismissCard(s.Hub), s.Kit.Open(configapp.Settings, section))
		}}}}
}

func (s *Screen) open() tea.Cmd {
	t := s.Kit.Env.Theme
	s.list = cards.NewList(s.Kit.Env, cards.ListSpec{
		Title:   "Secret stores",
		Glyph:   t.Glyphs.Settings,
		Key:     "secrets",
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			return []string{"Stores that hand icebath your API keys and other variables as it starts, so they need not sit in your keys file."}
		},
		Columns: []render.Column{{}, {Flex: 1}},
		Rows: func() [][]string {
			out := make([][]string, len(s.rows))
			for i, row := range s.rows {
				out[i] = []string{row.Name, row.State}
			}
			return out
		},
		Group: func(i int) string {
			if s.rows[i].Group == "" {
				return storesGroup
			}
			return s.rows[i].Group
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
			return render.Warm(strings.Join(s.failed, " "), t)
		},
		Empty: func() string {
			if !s.loaded {
				return "Reading the secret stores…"
			}
			return "No secret store is installed."
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			var out []wm.Hint
			if i >= 0 && i < len(s.rows) {
				row := s.rows[i]
				switch len(row.Open) {
				case 0:
				case 1:
					out = append(out, wm.Hint{Action: "hub.use", Label: row.Open[0].Verb})
				default:
					out = append(out, wm.Hint{Action: "hub.use", Label: "open"})
				}
				if row.Remove != nil {
					out = append(out, wm.Hint{Action: "hub.remove"})
				}
			}
			switch adds := s.adds(); len(adds) {
			case 0:
			case 1:
				out = append(out, wm.Hint{Action: "hub.add", Label: adds[0].Verb})
			default:
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
					return s.Actions("Add", nil, adds)
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
	s.list.ToggleDetail()
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

// Reads every store again, a store that fails named over the list. The main row of a store tells what its last fetch did, and the row
// that opens the settings comes last
func (s *Screen) Reload() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		ctx := s.Kit.Env.Ctx
		var rows, plain []Row
		var failed []string
		covered := map[string]bool{}
		for _, store := range s.stores {
			covered[store.Source] = true
			got, err := store.Rows(ctx)
			if err != nil {
				failed = append(failed, store.Source+" could not be read: "+strings.TrimSuffix(tui.Reason(err), ".")+".")
				continue
			}
			rows = append(rows, got...)
		}
		sources, err := s.service.Sources(ctx, &secretsv1.SourcesRequest{})
		if err != nil {
			failed = append(failed, "The other stores could not be read: "+strings.TrimSuffix(tui.Reason(err), ".")+".")
		} else {
			for _, source := range sources.GetSources() {
				if !covered[source.GetName()] {
					plain = append(plain, s.plain(source))
					continue
				}
				for i := range rows {
					if rows[i].Store == source.GetName() {
						rows[i].Detail = append(rows[i].Detail, s.fetched(source)...)
					}
				}
			}
		}
		// The stores first, each store's own rows after them under their own headings, then the settings
		var ordered []Row
		for _, row := range append(rows, plain...) {
			if row.Group == "" {
				ordered = append(ordered, row)
			}
		}
		for _, row := range rows {
			if row.Group != "" {
				ordered = append(ordered, row)
			}
		}
		ordered = append(ordered, s.settingsRow())
		return func() tea.Cmd {
			s.rows, s.failed, s.loaded = ordered, failed, true
			s.list.Refresh()
			for _, view := range s.views {
				view.Refresh()
			}
			return nil
		}
	})
}

// On or off, in a word
func State(on bool) string {
	if on {
		return "On"
	}
	return "Off"
}

// Length of time a number of seconds stands for, in words
func Seconds(n int64) string {
	return config.Span(time.Duration(n) * time.Second)
}

// Lines of what the last fetch of a store did: the variables it set, those it left as they were and why, and what went wrong
func (s *Screen) fetched(source *secretsv1.Source) []string {
	t := s.Kit.Env.Theme
	var lines []string
	if applied := source.GetApplied(); len(applied) > 0 {
		lines = append(lines, "It set "+render.Count(len(applied), "variable")+": "+strings.Join(applied, ", ")+".")
	}
	if existing := source.GetExisting(); len(existing) > 0 {
		lines = append(lines, render.Dim("Left as you set them: "+strings.Join(existing, ", ")+".", t))
	}
	if claimed := source.GetClaimed(); len(claimed) > 0 {
		lines = append(lines, render.Dim("Set by a store before it, which wins: "+strings.Join(claimed, ", ")+". Store order in the settings changes that.", t))
	}
	if protected := source.GetProtected(); len(protected) > 0 {
		lines = append(lines, render.Dim("Left alone because they hold the token of a store: "+strings.Join(protected, ", ")+".", t))
	}
	if invalid := source.GetInvalid(); len(invalid) > 0 {
		lines = append(lines, render.Dim("Not set because they are not variable names: "+strings.Join(invalid, ", ")+".", t))
	}
	if failure := source.GetError(); failure != "" {
		line := "Its last fetch failed: " + strings.TrimSuffix(failure, ".") + "."
		if remedy := source.GetRemedy(); remedy != "" {
			line += " " + strings.TrimSuffix(remedy, ".") + "."
		}
		lines = append(lines, render.Warm(line, t))
	}
	for _, warning := range source.GetWarnings() {
		lines = append(lines, render.Warm(warning, t))
	}
	return lines
}

// Row of a store with no part of its own: whether it is on and what its last fetch did, turned on or off and fetched again from it
func (s *Screen) plain(source *secretsv1.Source) Row {
	t := s.Kit.Env.Theme
	label, name, on := source.GetLabel(), source.GetName(), source.GetEnabled()
	state := render.Dim(State(on), t)
	switch {
	case on && source.GetError() != "":
		state = render.Warm("On, last fetch failed", t)
	case on:
		state = render.Good(State(on), t)
	}
	detail := []string{label + " hands its variables to icebath as it starts, while it is on."}
	detail = append(detail, s.fetched(source)...)
	toggle := Action{Label: "Turn on", Does: "Load its variables each time icebath starts", Verb: "turn on", Run: func(s *Screen) tea.Cmd {
		return s.Enable(name, label, true)
	}}
	if on {
		toggle = Action{Label: "Turn off", Does: "Stop loading its variables", Verb: "turn off", Run: func(s *Screen) tea.Cmd { return s.Enable(name, label, false) }}
	}
	again := Action{Label: "Fetch now", Does: "Ask every store that is on for its variables now", Verb: "fetch", Run: func(s *Screen) tea.Cmd { return s.Fetch() }}
	return Row{Name: label, State: state, Detail: detail, Open: []Action{toggle, again}, Store: name}
}

// Turns a store on or off for the profile, saying so under the list
func (s *Screen) Enable(name, label string, on bool) tea.Cmd {
	return s.Change("Turning "+label+" "+strings.ToLower(State(on))+"…", func(ctx context.Context) (Outcome, error) {
		_, err := s.service.Enable(ctx, &secretsv1.EnableRequest{Name: name, On: on})
		return Outcome{Note: label + " is now " + State(on) + "."}, err
	})
}

// Asks every store that is on for its variables now, saying how many were set
func (s *Screen) Fetch() tea.Cmd {
	return s.Change("Fetching from every store that is on…", func(ctx context.Context) (Outcome, error) {
		response, err := s.service.Reload(ctx, &secretsv1.ReloadRequest{})
		set := 0
		for _, source := range response.GetSources() {
			set += len(source.GetApplied())
		}
		return Outcome{Note: "Fetched again. The stores set " + render.Count(set, "variable") + "."}, err
	})
}

func (s *Screen) use(i int) tea.Cmd {
	if i < 0 || i >= len(s.rows) || len(s.rows[i].Open) == 0 {
		return nil
	}
	row := s.rows[i]
	if len(row.Open) == 1 {
		return row.Open[0].Run(s)
	}
	return s.Actions(row.Name, func() []string {
		for _, now := range s.rows {
			if now.Name == row.Name && now.Group == row.Group {
				return now.Detail
			}
		}
		return row.Detail
	}, row.Open)
}

// Opens the actions over the screen, each with what it does beside it or under the list, the detail of what they act on after it
func (s *Screen) Actions(title string, detail func() []string, actions []Action) tea.Cmd {
	t := s.Kit.Env.Theme
	var list *cards.List
	list = cards.NewList(s.Kit.Env, cards.ListSpec{
		Title:   title,
		Glyph:   t.Glyphs.Settings,
		Key:     "secrets:actions:" + title,
		Context: cards.HubContext,
		Primary: "hub.use",
		Columns: []render.Column{{}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			rows := make([][]string, len(actions))
			for i, action := range actions {
				rows[i] = []string{action.Label, render.Dim(action.Does, t)}
			}
			return rows
		},
		Detail: func(i int) []string {
			var lines []string
			if i >= 0 && i < len(actions) && !list.Shows(1) && actions[i].Does != "" {
				lines = append(lines, actions[i].Does)
			}
			if detail != nil {
				if more := detail(); len(more) > 0 {
					if len(lines) > 0 {
						lines = append(lines, "")
					}
					lines = append(lines, more...)
				}
			}
			return lines
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(actions) {
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: actions[i].Verb}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd { return actions[i].Run(s) },
		},
	})
	list.ToggleDetail()
	s.views = append(s.views, list)
	return s.Hub.Push(list)
}

// What a change did: a page of lines under its title, or a note when there are none, said as a warning when something stands in the way
type Outcome struct {
	Title string
	Lines []string
	Note  string
	Warn  bool
}

// Runs a change, which may install a program and ask a store over the network, in the background, back on the list with a note saying
// what it waits for; then pages what it did or says its note, and reads the stores again
func (s *Screen) Change(busy string, work func(ctx context.Context) (Outcome, error)) tea.Cmd {
	back := s.Hub.PopTo(s.list)
	s.Hub.Busy(busy)
	return tea.Batch(back, tui.Async(func() func() tea.Cmd {
		outcome, err := work(s.Kit.Env.Ctx)
		return func() tea.Cmd {
			if err != nil {
				return tea.Batch(s.Hub.Fail(strings.TrimSuffix(tui.Reason(err), ".")+"."), s.Reload())
			}
			s.Hub.Quiet()
			switch {
			case len(outcome.Lines) > 0:
				return tea.Batch(s.Page(outcome.Title, outcome.Lines), s.Reload())
			case outcome.Warn:
				return tea.Batch(s.Hub.Fail(outcome.Note), s.Reload())
			}
			return tea.Batch(s.Hub.Done(outcome.Note), s.Reload())
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

// Opens a form over the screen, done handed the form and its values once they check out, to close it or say under a field what was wrong
func (s *Screen) Form(spec cards.FormSpec, done func(form *cards.Form, values map[string]string) tea.Cmd) tea.Cmd {
	spec.Done = func(form *cards.Form, values map[string]string) tea.Cmd { return done(form, values) }
	return s.Hub.Push(cards.NewForm(s.Kit.Env, spec))
}

// Lines of what each secret a store hands over does, with how many there are first: set from the store, replacing a value, left alone and
// why, or not read; then what the store warned of
func (s *Screen) Preview(store string, rows [][2]string, warnings []string) []string {
	t := s.Kit.Env.Theme
	var lines []string
	set, kept := 0, 0
	for _, row := range rows {
		name, action := row[0], row[1]
		switch action {
		case "export":
			set++
			lines = append(lines, render.Good(name, t)+" is set from "+store+".")
		case "override":
			set++
			lines = append(lines, render.Good(name, t)+" is set from "+store+", over the value you set.")
		case "exported":
			set++
			lines = append(lines, render.Good(name, t)+" is set now.")
		case "overrode":
			set++
			lines = append(lines, render.Good(name, t)+" is set now, over the value you set.")
		case "skip_set":
			kept++
			lines = append(lines, render.Dim(name+" is left alone because you set it already.", t))
		case "skip_token":
			kept++
			lines = append(lines, render.Dim(name+" is left alone because it holds the token of "+store+".", t))
		default:
			lines = append(lines, render.Bad(name+" could not be read.", t))
		}
	}
	summary := store + " handed over nothing."
	if len(rows) > 0 {
		summary = store + " hands over " + render.Count(len(rows), "secret") + ": " + strconv.Itoa(set) + " set, " + strconv.Itoa(kept) + " left alone."
	}
	lines = append([]string{summary, ""}, lines...)
	for _, warning := range warnings {
		lines = append(lines, render.Warm(warning, t))
	}
	return lines
}

package tui

import (
	"cmp"
	"context"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/lang"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/tty"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

const again = 2 * time.Second

type Screen struct {
	ID    string
	Title string
	Short string
	// Heading the command palette lists the screen under, one of the Group names, and its place among the screens of that heading,
	// lower first. A screen that names no heading is listed under More
	Group string
	Order int
	Slash []string
	// More names the screen answers as a slash command, which the slash list, /help and the palette leave out
	Aliases []string
	Usage   string
	Keys    []string
	Actions []keys.Action
	Where   map[string]string
	// Opened once when the app starts
	Startup bool
	// Listed with its key on an empty screen
	Suggest bool
	Ready   func(kit *Kit) bool
	Open    func(kit *Kit, argument string) tea.Cmd
	// Settings the screen follows by key, each run with the value as YAML once the app starts and again whenever it changes
	Follows map[string]func(kit *Kit, value string) tea.Cmd
	// Key of the settings section the screen is the page of. The screen is then one area of the settings menu: the palette, its slash
	// commands, Kit.Open and the menu itself all show it there
	Area string
	// Opens the settings menu on the area with the section key and hands the argument to the page of that area. Only the screen of the
	// menu itself has one
	Menu func(kit *Kit, area, argument string) tea.Cmd
	// Pickers the screen offers every other screen, by the kind of thing each picks
	Pickers map[string]Picker
	// Pages inside the area the screen is the page of, as a search of Settings finds them: read each time a search starts, off the UI
	// thread, so it may ask a service; the profile is the one whose settings are shown
	Pages func(kit *Kit, profile string) []Page
	// Runs once as the app starts, for what a screen learns before anyone opens it
	Start func(kit *Kit) tea.Cmd
}

// A page inside an area that a search of Settings finds and opens: the area on that page, the keys on it
type Page struct {
	// What the page is called, and the titles of the pages above it inside the area, outermost first
	Title string
	Trail []string
	// One line saying what is there
	About string
	// More words a search reads, such as the names of what the page shows
	Words string
	// Argument the screen opens the area with to show the page, as its Open takes it after the profile; empty for the area's own page
	Argument string
	// Keys of the settings the page shows rows or controls of, so a setting found by a search opens the page with its control over it; a
	// key ending in a dot stands for every key under it
	Keys []string
}

// Pages of the areas the screens are the pages of, by the key of each area, read from every screen that has some: off the UI thread
func (k *Kit) Pages(profile string) map[string][]Page {
	pages := map[string][]Page{}
	for _, screen := range k.screens {
		if screen.Area != "" && screen.Pages != nil {
			pages[screen.Area] = append(pages[screen.Area], screen.Pages(k, profile)...)
		}
	}
	return pages
}

// Pages read before by the key a screen keeps them under, and the keys being read again now
var remembered struct {
	sync.Mutex
	pages   map[string][]Page
	reading map[string]bool
}

// Pages as last read under the key, read again behind so the next answer is fresh: how Pages answers at once for a screen whose things take
// time to find, such as places found by probing this computer. The first call for a key waits for its reading; a reading that fails, nil,
// keeps what was read before
func Remembered(key string, read func() []Page) []Page {
	remembered.Lock()
	if remembered.pages == nil {
		remembered.pages, remembered.reading = map[string][]Page{}, map[string]bool{}
	}
	known, ok := remembered.pages[key]
	busy := remembered.reading[key]
	remembered.reading[key] = true
	remembered.Unlock()
	switch {
	case ok && !busy:
		go remember(key, read)
		return known
	case ok, busy:
		return known
	}
	return remember(key, read)
}

// Reads the pages under the key and keeps them for the next answer
func remember(key string, read func() []Page) []Page {
	pages := read()
	remembered.Lock()
	defer remembered.Unlock()
	remembered.reading[key] = false
	if pages == nil {
		return remembered.pages[key]
	}
	remembered.pages[key] = pages
	return pages
}

// Whether the page shows the setting at the key
func (p Page) Shows(key string) bool {
	key = strings.ToLower(key)
	for _, each := range p.Keys {
		each = strings.ToLower(each)
		if each == key || strings.HasSuffix(each, ".") && strings.HasPrefix(key, each) {
			return true
		}
	}
	return false
}

// One area of the settings menu as the palette lists it: the key of its section, its name, the heading of its group and what is set there
type Area struct {
	ID, Title, Group, About string
}

// Headings the command palette lists screens under, in the order they show: what a person reaches for first comes first
const (
	GroupSessions  = "Sessions"
	GroupModels    = "Models"
	GroupAbilities = "What the agent can do"
	GroupSafety    = "Safety"
	GroupMedia     = "Images and video"
	GroupVoice     = "Voice"
	GroupSecrets   = "Keys and passwords"
	GroupApp       = "This app"
	GroupMore      = "More"
)

// Headings of the screens in the order the palette shows them
var groups = []string{GroupSessions, GroupModels, GroupAbilities, GroupSafety, GroupMedia, GroupVoice, GroupSecrets, GroupApp, GroupMore}

// Place of a heading among the headings, a heading that is none of them counted as More
func place(group string) int {
	if at := slices.Index(groups, group); at >= 0 {
		return at
	}
	return slices.Index(groups, GroupMore)
}

type Options struct {
	Skin string
	Keys map[string][]string
	// Value as YAML of every setting a screen follows, by key
	Settings map[string]string
}

type Kit struct {
	Env     *cards.Env
	WM      *wm.Model
	History cards.History

	*shared
	// Screen being mounted as the page of an area through this copy of the kit, nil for a kit that shows dialogs of their own
	mount *mount
}

// What every copy of a kit shares
type shared struct {
	screens   []Screen
	reports   []func(status *wm.Status)
	terminals int
	// Settings of the kit by the names its cards read them by
	settings map[string]string
	// Areas of the settings menu as it read them, nil before it did
	areas []Area
}

// A screen being mounted as the page of an area: the hub its first view went into, and whether its Open returned, after which its kit shows
// dialogs of their own again
type mount struct {
	hub  *cards.Hub
	done bool
}

// Keys of every setting the screens follow
func Followed(screens []Screen) []string {
	var keys []string
	for _, screen := range screens {
		for key := range screen.Follows {
			if !slices.Contains(keys, key) {
				keys = append(keys, key)
			}
		}
	}
	slices.Sort(keys)
	return keys
}

// Tells every screen that follows the setting its new value, as YAML, and every card that it changed
func (k *Kit) Changed(key, value string) tea.Cmd {
	var cmds []tea.Cmd
	for _, screen := range k.screens {
		if follow, ok := screen.Follows[key]; ok {
			cmds = append(cmds, follow(k, value))
		}
	}
	return tea.Batch(append(cmds, func() tea.Msg { return cards.SettingChangedMsg{Key: key, Value: value} })...)
}

// Sets a setting of the kit by the name its cards read it by, telling every card it changed
func (k *Kit) Set(name, value string) tea.Cmd {
	if k.settings[name] == value {
		return nil
	}
	k.settings[name] = value
	return func() tea.Msg { return cards.SettingChangedMsg{Key: name, Value: value} }
}

type ThemeMsg struct {
	Name string
}

type applyMsg func() tea.Cmd

type appliedMsg struct{}

func Theme(name string) tea.Cmd {
	return func() tea.Msg {
		return ThemeMsg{Name: name}
	}
}

func Async(work func() func() tea.Cmd) tea.Cmd {
	return func() tea.Msg {
		return applyMsg(work())
	}
}

func (k *Kit) Apply(apply func() tea.Cmd) {
	k.Env.Send(applyMsg(apply))
}

// Hub of a dialog of its own over its first view
func (k *Kit) Hub(key string, root wm.Card) *cards.Hub {
	return cards.NewHub(k.Env, key, root, k.WM.Size)
}

// Shows the first view of a screen: as the page of its area when the settings menu is opening it, else in a dialog of its own. Called
// by Open before it returns
func (k *Kit) Show(key string, root wm.Card) (*cards.Hub, tea.Cmd) {
	hub := k.Hub(key, root)
	if k.mount != nil && !k.mount.done && k.mount.hub == nil {
		k.mount.hub = hub
		return hub, nil
	}
	return hub, k.WM.ShowOverlay(hub)
}

// Opens the screen that names the area as the page of that area, handing it the argument: the hub its first view went into, with what
// its Open returned. False when no screen names the area; a nil hub when the screen showed nothing
func (k *Kit) Mount(area, argument string) (*cards.Hub, tea.Cmd, bool) {
	for _, screen := range k.screens {
		if screen.Area != area || area == "" {
			continue
		}
		mounted := *k
		mounted.mount = &mount{}
		cmd := screen.Open(&mounted, argument)
		mounted.mount.done = true
		return mounted.mount.hub, cmd, true
	}
	return nil, nil, false
}

// Tells the kit the areas of the settings menu, which the palette then lists in place of the screens that are their pages
func (k *Kit) Areas(areas []Area) { k.areas = slices.Clone(areas) }

// Screen of the settings menu, false while there is none
func (k *Kit) menu() (Screen, bool) {
	for _, screen := range k.screens {
		if screen.Menu != nil {
			return screen, true
		}
	}
	return Screen{}, false
}

func (k *Kit) Screens() []Screen {
	return slices.Clone(k.screens)
}

func (k *Kit) Open(id, argument string) tea.Cmd {
	for _, screen := range k.screens {
		if screen.ID == id {
			return k.open(screen, argument)
		}
	}
	return nil
}

// Opens a screen: one that is the page of an area through the settings menu on that area, any other by its own Open
func (k *Kit) open(screen Screen, argument string) tea.Cmd {
	if screen.Area != "" {
		if menu, ok := k.menu(); ok {
			return menu.Menu(k, screen.Area, argument)
		}
	}
	return screen.Open(k, argument)
}

func (k *Kit) Slash(line string) (tea.Cmd, bool) {
	word, argument, _ := strings.Cut(strings.TrimSpace(line), " ")
	name, ok := strings.CutPrefix(word, "/")
	if !ok || name == "" {
		return nil, false
	}
	for _, screen := range k.screens {
		if (slices.Contains(screen.Slash, name) || slices.Contains(screen.Aliases, name)) && (screen.Ready == nil || screen.Ready(k)) {
			return k.open(screen, strings.TrimSpace(argument)), true
		}
	}
	return nil, false
}

// Picker of a kind of thing, false when no screen offers one
// Picker of the kind a setting's Picks names, the argument after the kind left to the picker; false when no screen offers one
func (k *Kit) Picker(picks string) (Picker, bool) {
	kind, _ := PickOf(picks)
	if kind == "" {
		return Picker{}, false
	}
	for _, screen := range k.screens {
		if picker, ok := screen.Pickers[kind]; ok && picker.Open != nil {
			return picker, true
		}
	}
	return Picker{}, false
}

// Opens the picker of a kind of thing over the hub, handed what of the kind to list when picks names it after the kind and the ask does
// not; false when no screen offers one
func (k *Kit) Choose(hub *cards.Hub, picks string, ask Ask) (tea.Cmd, bool) {
	picker, ok := k.Picker(picks)
	if !ok {
		return nil, false
	}
	if ask.Of == "" {
		_, ask.Of = PickOf(picks)
	}
	return picker.Open(k, hub, ask), true
}

// Kind of thing a setting's Picks names and what of that kind the picker lists, written kind:argument, such as model:openrouter for the
// models of OpenRouter alone. The argument is the picker's own to read; empty for every thing of the kind
func PickOf(picks string) (kind, of string) {
	kind, of, _ = strings.Cut(picks, ":")
	return kind, of
}

func (k *Kit) Terminal() *cards.Terminal {
	k.terminals++
	return cards.NewTerminal(k.Env, k.terminals, k.Env.WorkDir())
}

func (k *Kit) Report(report func(status *wm.Status)) {
	k.reports = append(k.reports, report)
}

func (k *Kit) Focused() wm.Card {
	focused := k.WM.Focused()
	if tabs, ok := focused.(*wm.Tabs); ok {
		focused, _ = tabs.Active()
	}
	return focused
}

// Card that shows one session of the agent, such as a chat
type Sessioned interface {
	// Id of the session shown, empty before it has one
	Session() string
}

// Sessions the windows of every space show, the focused one first
func (k *Kit) Sessions() []string {
	var out []string
	add := func(card wm.Card) {
		if shown, ok := card.(Sessioned); ok && shown.Session() != "" && !slices.Contains(out, shown.Session()) {
			out = append(out, shown.Session())
		}
	}
	add(k.Focused())
	for _, space := range k.WM.Workspaces() {
		for _, card := range space.Cards() {
			if tabs, ok := card.(*wm.Tabs); ok {
				for _, tab := range tabs.Cards() {
					add(tab)
				}
				continue
			}
			add(card)
		}
	}
	return out
}

type history struct {
	mu      sync.Mutex
	entries []cards.Entry
}

func (h *history) Entries() []cards.Entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.entries)
}

func (h *history) Add(entry cards.Entry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = append(slices.DeleteFunc(h.entries, func(e cards.Entry) bool { return e.Text == entry.Text }), entry)
}

func Run(ctx context.Context, screens []Screen, options Options) error {
	m, err := newModel(ctx, screens, options)
	if err != nil {
		return err
	}
	program := tty.NewProgram(context.WithoutCancel(ctx), m)
	m.kit.Env.Send = program.Send
	defer context.AfterFunc(ctx, func() { program.Send(tea.Quit()) })()
	_, err = program.Run()
	if m.ended || ctx.Err() != nil {
		return nil
	}
	return err
}

type model struct {
	kit         *Kit
	spaces      int
	interrupted time.Time
	ended       bool
	// What the screens that follow the settings made of their first values
	followed tea.Cmd
}

func newModel(ctx context.Context, screens []Screen, options Options) (*model, error) {
	directory, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	bindings := keys.Default()
	cards.RegisterHubKeys(bindings)
	for _, screen := range screens {
		if screen.Title != "" {
			bindings.Register(keys.Action{ID: "open." + screen.ID, Context: keys.Global, Label: screen.Title, Doc: screen.Short, Default: screen.Keys})
		}
		for context, where := range screen.Where {
			if keys.ContextDocs[context] != "" {
				continue
			}
			if err := keys.AddContext(context, where); err != nil {
				return nil, err
			}
		}
		for _, action := range screen.Actions {
			bindings.Register(action)
		}
	}
	for id, chords := range options.Keys {
		if err := bindings.Set(id, chords); err != nil {
			slog.Warn("A saved key was left out", "action", id, "error", err)
		}
	}
	lang.InstallKeys(bindings.Display)
	look, known := theme.Named(options.Skin)
	if !known {
		look, _ = theme.Named(theme.DefaultName)
	}
	m := &model{kit: &Kit{
		shared:  &shared{screens: screens, settings: map[string]string{}},
		History: new(history),
		Env: &cards.Env{
			Theme:      look,
			Keys:       bindings,
			Ctx:        ctx,
			WorkDir:    func() string { return directory },
			ProjectDir: directory,
			Root:       directory,
		},
		WM: wm.New(wm.WithTheme(look), wm.WithKeys(bindings), wm.WithWindowTitle("icebath")),
	}}
	m.kit.Env.Settings = func() map[string]string { return maps.Clone(m.kit.settings) }
	var follows []tea.Cmd
	for _, screen := range screens {
		for key, follow := range screen.Follows {
			if value, ok := options.Settings[key]; ok {
				follows = append(follows, follow(m.kit, value))
			}
		}
	}
	m.followed = tea.Batch(follows...)
	m.kit.WM.SetCommandSource(m.commands)
	var suggested []string
	for _, screen := range screens {
		if screen.Suggest && screen.Title != "" {
			suggested = append(suggested, "open."+screen.ID)
		}
	}
	m.kit.WM.SetEmptyHints(append(suggested, "palette")...)
	m.kit.WM.SetBar(func() wm.Status {
		status := wm.Status{Lanes: map[*wm.Workspace]wm.Lane{}}
		for _, report := range m.kit.reports {
			report(&status)
		}
		return status
	})
	return m, nil
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.kit.WM.Init(), m.followed, m.space(), m.startup())
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tty.TerminateMsg, tty.GoneMsg:
		return m, m.quit()
	case ThemeMsg:
		if look, known := theme.Named(msg.Name); known {
			m.kit.Env.Theme = look
			m.kit.WM.SetTheme(look)
		}
	case applyMsg:
		var cmd tea.Cmd
		if msg != nil {
			cmd = msg()
		}
		_, repaint := m.kit.WM.Update(appliedMsg{})
		return m, tea.Batch(cmd, repaint)
	}
	_, cmd := m.kit.WM.Update(msg)
	return m, cmd
}

func (m *model) View() tea.View {
	return m.kit.WM.View()
}

// Adds an empty space and shows it
func (m *model) space() tea.Cmd {
	m.spaces++
	workspace, open := m.kit.WM.AddWorkspace(strconv.Itoa(m.spaces))
	return tea.Batch(open, m.kit.WM.Switch(m.kit.WM.IndexOf(workspace)))
}

// Opens the screens that open when the app starts, after running what each does as the app starts
func (m *model) startup() tea.Cmd {
	var cmds []tea.Cmd
	for _, screen := range m.kit.screens {
		if screen.Start != nil {
			cmds = append(cmds, screen.Start(m.kit))
		}
	}
	for _, screen := range m.kit.screens {
		if screen.Startup {
			cmds = append(cmds, m.kit.open(screen, ""))
		}
	}
	return tea.Batch(cmds...)
}

func (m *model) quit() tea.Cmd {
	m.ended = true
	return tea.Quit
}

func (m *model) interrupt() tea.Cmd {
	if time.Since(m.interrupted) < again {
		return m.quit()
	}
	m.interrupted = time.Now()
	return tea.Batch(m.kit.WM.Fire("session.stop"), m.kit.WM.Toast("Press "+m.kit.Env.Keys.Display("interrupt")+" again to quit.", wm.Info, again))
}

func (m *model) commands() []wm.Command {
	commands := []wm.Command{
		{ID: "interrupt", Run: m.interrupt},
	}
	// Screens under their headings, in the order of the headings and then their own, each heading's areas of the settings menu after its
	// screens. An area is listed once, by its name: the screen that is its page is not listed beside it
	screens := slices.Clone(m.kit.screens)
	slices.SortStableFunc(screens, func(a, b Screen) int {
		return cmp.Or(cmp.Compare(place(a.Group), place(b.Group)), cmp.Compare(a.Order, b.Order))
	})
	menu, menued := m.kit.menu()
	areas := m.kit.areas
	if !menued {
		areas = nil
	}
	listed := func(screen Screen) bool {
		return screen.Area != "" && slices.ContainsFunc(areas, func(area Area) bool { return area.ID == screen.Area })
	}
	for at, group := range groups {
		for _, screen := range screens {
			if screen.Title == "" || place(screen.Group) != at {
				continue
			}
			var ready func() bool
			if screen.Ready != nil {
				ready = func() bool { return screen.Ready(m.kit) }
			}
			command := wm.Command{ID: "open." + screen.ID, Title: screen.Title, Detail: screen.Short, Group: group, When: ready,
				Run: func() tea.Cmd { return m.kit.open(screen, "") }}
			// Kept untitled, so the screen's own keys still open it without a second row for its area
			if listed(screen) {
				command.Title, command.Detail = "", ""
			}
			commands = append(commands, command)
		}
		for _, area := range areas {
			if place(area.Group) == at {
				commands = append(commands, wm.Command{ID: "area." + area.ID, Title: area.Title, Detail: area.About, Group: group,
					Run: func() tea.Cmd { return menu.Menu(m.kit, area.ID, "") }})
			}
		}
	}
	return append(commands,
		wm.Command{ID: "workspace.new", Group: wm.GroupSpaces, Title: "New space", Run: m.space},
		wm.Command{
			ID:    "workspace.close",
			Group: wm.GroupSpaces,
			Title: "Close space",
			When:  func() bool { return len(m.kit.WM.Workspaces()) > 1 },
			Run:   func() tea.Cmd { return m.kit.WM.RemoveWorkspace(m.kit.WM.ActiveIndex()) },
		},
		wm.Command{ID: "quit", Group: GroupApp, Title: "Quit", Last: true, Run: m.quit},
	)
}

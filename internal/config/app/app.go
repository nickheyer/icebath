package app

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
	"github.com/charmbracelet/x/ansi"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/profile"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

const (
	// Screen of every setting, the settings menu: opened on a section by its key or searched for any other words
	Settings = "settings"
	// Screen that changes one setting, opened with its key, or one entry of a table setting with the entry's key
	Edit = "setting"
)

// Keymap context of the field one setting is typed into
const fieldContext = "settings.field"

var actions = []keys.Action{
	{ID: "settings.default", Context: fieldContext, Label: "use default", Doc: "Put the setting back to its default", Default: []string{"ctrl+d"}},
}

// Group the row of the settings files is listed in among the areas
const filesGroup = string(config.GroupApp)

// Id of the row of the settings files among the areas, one no section key can be
const filesArea = ":files"

// What the search field over the areas says while nothing is typed into it
const searchWords = "Search settings"

// Cells a value takes in a row before it is cut, the whole of it then shown under the list
const valueRoom = 48

// Entries of a group of values a row names before it counts the rest
const namedFields = 3

var (
	// The three columns of an option row: its name, the mark of a changed value, its value
	Columns = []render.Column{{}, {}, {Flex: 1}}
	// Columns of the areas filling the menu: a name, what is set there and how many settings inside were changed
	homeColumns = []render.Column{{}, {Flex: 8, Whole: true}, {Flex: 1, Right: true}}
	// Columns of what a search finds: those of the rows of a section, and at the right how many settings of a found area were changed
	foundColumns = []render.Column{{}, {}, {Flex: 1}, {Right: true}}
	// Column of the areas down the left of the menu: the name alone
	railColumns = []render.Column{{}}
)

type screens struct{}

func Register(host *app.App) {
	host.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := configv1connect.NewConfigServiceClient(conn)
	// Settings of the menu on screen, nil while none is
	var live *settings
	// Settings of the menu on the profile: those of the menu on screen while it shows that profile, else new ones
	menu := func(kit *tui.Kit, named string) *settings {
		if live == nil || live.named != named || live.menu == nil || !kit.WM.ShowsOverlay(live.menu) {
			live = newSettings(kit, service, named)
		}
		return live
	}
	return []tui.Screen{
		{
			ID:      Settings,
			Group:   tui.GroupApp,
			Order:   10,
			Title:   "Settings",
			Short:   "Change how icebath and the agent work",
			Slash:   []string{"settings", "config", "preferences"},
			Usage:   "[@profile] [section or words to search for]",
			Actions: actions,
			Where:   map[string]string{fieldContext: "setting fields"},
			Open: func(kit *tui.Kit, argument string) tea.Cmd {
				name, target := profile.Split(argument)
				return menu(kit, name).open(target)
			},
			Menu: func(kit *tui.Kit, area, argument string) tea.Cmd {
				name, _ := profile.Split(argument)
				return menu(kit, name).area(area, argument)
			},
			Start: func(kit *tui.Kit) tea.Cmd { return newSettings(kit, service, "").learn() },
		},
		{
			ID:    Edit,
			Short: "Change one setting",
			Open: func(kit *tui.Kit, argument string) tea.Cmd {
				name, key := profile.Split(argument)
				return menu(kit, name).one(key)
			},
		},
	}
}

// Listings by app and profile: the menu and the option rows of every page on one profile share one, so a change kept by any of them is
// seen and drawn by all
var (
	listingsMu sync.Mutex
	listings   = map[listingKey]*listing{}
)

type listingKey struct {
	env     *cards.Env
	profile string
}

// The listing of the profile in the app of the kit, made the first time
func listingOf(kit *tui.Kit, profile string) *listing {
	listingsMu.Lock()
	defer listingsMu.Unlock()
	key := listingKey{kit.Env, profile}
	if held, ok := listings[key]; ok {
		return held
	}
	made := &listing{}
	listings[key] = made
	return made
}

// What the settings service listed for a profile, shared by every view that shows a part of it
type listing struct {
	groups   []*configv1.Group
	sections []*configv1.Section
	all      []*configv1.Setting
	// Sections by key
	byKey map[string]*configv1.Section
	// Key of the setting whose row covers each setting kept beside it as one choice, by the key of the one covered
	covered map[string]string
	// Areas a screen is the page of, by key
	screened map[string]bool
	// Pages inside the areas whose pages are screens, by the key of the area, as read when the last search started
	pages map[string][]tui.Page
	// A search of the areas is under way
	searching bool
	// Reading of the pages now, the answers of an earlier one dropped, and the areas whose screens have answered it
	pagesGen  int
	pagesRead map[string]bool
	// When the pages were first asked for: a search says it waits on a screen that never answered for a while after it, no longer
	pagesSince time.Time
	// Changes kept so far, so a reading that overlapped one is done again
	writes int
	// Reads the pages inside the areas again for the menu, run whenever the settings are read or a change is kept; nil while there is no menu
	again  func() tea.Cmd
	loaded bool
	err    error
	// Lists that show settings, read again whenever the settings are read or one is saved
	views cards.Watched
}

type settings struct {
	kit     *tui.Kit
	service configv1connect.ConfigServiceClient
	*listing
	// Profile the settings were opened on, the one in use when empty
	named string
	// Hub the views open over: a page of the menu, the hub of its areas, or the page of another screen that shows option rows
	hub *cards.Hub
	// The menu and the list of its areas with the rows it shows, nil for option rows on the page of another screen
	menu  *cards.Menu
	areas *cards.List
	rows  []row
	// Argument the page of each area is built with next, by the key of the area
	args map[string]string
	// Control of a setting that goes over the page of each area once its screen shows the page, by the key of the area
	controls map[string]pending
	// Form the value of a setting is typed into, where a refusal of that value is shown under its field
	typed *cards.Form
}

// Control of a setting waiting for the page it goes over: the setting's key, and whether the person came straight to changing it, so the
// menu closes once it is changed
type pending struct {
	key    string
	direct bool
	// Views the hub of the area's page holds once the screen pushed the page: its own first view, the pages above it, then it
	depth int
}

// Something a row does besides opening a section or changing a setting
type action struct {
	title, about string
	// More words a search reads, and what tells the row apart from every other found, empty for a row of its own kind
	words, id string
	// Word on the button that does it
	verb string
	run  func() tea.Cmd
}

// Row of a list: a section to open, a setting to change, the advanced settings of a section, or something else the screen does
type row struct {
	section *configv1.Section
	setting *configv1.Setting
	// Section whose advanced settings the row opens
	advanced *configv1.Section
	act      *action
	// Titles of the sections the row is under, said before its name where rows of several sections are listed together
	trail []string
}

func newSettings(kit *tui.Kit, service configv1connect.ConfigServiceClient, named string) *settings {
	return &settings{kit: kit, service: service, listing: listingOf(kit, named), named: named, args: map[string]string{}, controls: map[string]pending{}}
}

// The same settings with their views opening over another hub
func (s *settings) on(hub *cards.Hub) *settings {
	c := *s
	c.hub, c.typed = hub, nil
	return &c
}

// Title of the menu, naming the profile when it is not the one a person starts with
func (s *settings) heading() string {
	if s.named != "" {
		return "Settings · " + s.named
	}
	return "Settings"
}

// The menu, made the first time: the areas under the headings of their groups, the page of the one in hand beside them, and everything
// there is to find once words are typed into the search over them
func (s *settings) build() {
	if s.menu != nil {
		return
	}
	t := s.kit.Env.Theme
	var list *cards.List
	list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   s.heading(),
		Glyph:   t.Glyphs.Settings,
		Key:     Settings,
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Bar:     true,
		Search:  searchWords,
		// The pages inside the areas are read when the menu opens and whenever the settings are read again, so a search lists them at once
		Searched: func(l *cards.List, query string) tea.Cmd {
			s.searching = query != ""
			// The best hit, first in the order of what was found, is the row in hand as the words change
			if query != "" {
				l.Move(-len(s.rows))
			}
			return nil
		},
		Columns: homeColumns,
		// What a search finds is listed as the rows of a section are, the areas down the left by name alone
		Layout: func() []render.Column {
			switch {
			case list.Query() != "":
				return foundColumns
			case s.menu.Rail():
				return railColumns
			}
			return homeColumns
		},
		Rows: func() [][]string {
			searching := list.Query() != ""
			s.rows = s.home(searching)
			cells := make([][]string, len(s.rows))
			for i, r := range s.rows {
				switch {
				case searching && r.section != nil && r.section.GetArea():
					cells[i] = s.foundArea(r)
				case searching:
					cells[i] = append(s.cells(r), "")
				case s.menu.Rail():
					cells[i] = []string{s.name(r)}
				default:
					cells[i] = s.homeCells(r)
				}
			}
			return cells
		},
		Group: func(i int) string { return s.groupOf(s.rows[i]) },
		Words: func(i int) string { return s.wordsOf(s.rows[i]) },
		Detail: func(i int) []string {
			switch r := s.rows[i]; {
			case list.Query() != "":
				return s.detail(r, list.Cut(i, 2))
			case s.menu.Rail() || list.Shows(1):
				return nil
			default:
				return s.detail(r, false)
			}
		},
		Empty: func() string { return tui.Waiting(s.loaded, s.err, "No settings to show.") },
		// While a search waits on screens whose pages are not read yet, it says so quietly
		Status: func() string {
			if list.Query() != "" && s.pagesWaiting() {
				return "Looking for pages…"
			}
			return ""
		},
		Free: map[string]bool{"hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			switch {
			case list.Query() != "":
				return s.hints(s.rows, i)
			case i < 0:
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: "open"}}
		},
		Actions: s.areaActions(),
	})
	list.ToggleDetail()
	s.areas = list
	s.views.Add(list)
	s.menu = cards.NewMenu(s.kit.Env, cards.MenuSpec{Key: Settings, Areas: list, Area: s.inHand, Page: s.page, Rail: s.rail}, s.kit.WM.Size)
	s.again = s.readPages
	// What a search finds is changed right there, over the rows found
	s.hub = s.menu.Home()
	if s.loaded {
		list.Refresh()
	}
}

// What enter, reset and reload do on the areas: enter opens an area, or what a search found, a setting being changed right there
func (s *settings) areaActions() map[string]func(*cards.List, int) tea.Cmd {
	acts := s.rowActions(&s.rows)
	change := acts["hub.use"]
	acts["hub.use"] = func(l *cards.List, i int) tea.Cmd {
		r := s.rows[i]
		switch {
		case l.Query() == "":
			return s.menu.Go(s.idOf(r), true)
		case r.section != nil:
			return s.reach(r.section.GetKey())
		case r.act != nil:
			return r.act.run()
		}
		// A setting the page of a screen shows opens that page with its control over it
		if area := s.areaOf(s.section(r.setting.GetSection())); area != nil && s.mounted(area.GetKey()) {
			return s.toSetting(area.GetKey(), r.setting.GetKey(), false)
		}
		return change(l, i)
	}
	return acts
}

// Id of the area a row of the areas is, empty for any other row
func (s *settings) idOf(r row) string {
	switch {
	case r.section != nil:
		return r.section.GetKey()
	case r.act != nil:
		return filesArea
	}
	return ""
}

// Area on the row of the areas in hand, none while they are searched
func (s *settings) inHand() string {
	if s.areas.Query() != "" {
		return ""
	}
	if i := s.areas.Index(); i >= 0 && i < len(s.rows) {
		return s.idOf(s.rows[i])
	}
	return ""
}

// Columns the areas take down the left of the menu: their longest name or heading, or the words of the empty search field, with the mark
// of the row in hand before and a margin after
func (s *settings) rail() int {
	n := ansi.StringWidth(searchWords)
	for _, r := range s.home(false) {
		n = max(n, ansi.StringWidth(s.name(r)), ansi.StringWidth(strings.ToUpper(s.groupOf(r))))
	}
	return n + 4
}

// Page of an area: the screen made for it, opened with the argument it was asked with, else the list of its settings
func (s *settings) page(id string) (*cards.Hub, tea.Cmd) {
	if id == filesArea {
		return s.filesPage()
	}
	argument := s.args[id]
	delete(s.args, id)
	if argument == "" {
		argument = profile.Join(s.named, "")
	}
	if hub, cmd, ok := s.kit.Mount(id, argument); ok {
		if waiting, ok := s.controls[id]; ok {
			delete(s.controls, id)
			control := func() tea.Cmd {
				setting := s.find(waiting.key)
				if setting == nil {
					return nil
				}
				cmd := s.on(hub).change(setting)
				if waiting.direct {
					s.menu.Direct()
				}
				return cmd
			}
			// Over the page the argument opens, at once when the screen shows it already, else once it pushed it and the pages above it
			return hub, tea.Batch(cmd, hub.At(waiting.depth, control))
		}
		return hub, cmd
	}
	section := s.section(id)
	if section == nil {
		return nil, nil
	}
	page := s.on(nil)
	list := page.sectionList(section)
	page.hub = s.kit.Hub(Settings+":"+id, list)
	return page.hub, nil
}

// Whether a screen is the page of the area
func (s *settings) mounted(id string) bool { return s.screened[id] }

// Shows the menu, made first when it was not
func (s *settings) show() tea.Cmd {
	s.build()
	if s.kit.WM.ShowsOverlay(s.menu) {
		return nil
	}
	return tea.Batch(s.kit.WM.ShowOverlay(s.menu), s.readPages())
}

// Opens the menu on its areas, then the section or the words searched for once the settings are read
func (s *settings) open(target string) tea.Cmd {
	return tea.Batch(s.show(), s.ready(func() tea.Cmd { return s.reach(target) }))
}

// Opens the menu on the area, the keys on its page, the page built again with the argument when one is given
func (s *settings) area(id, argument string) tea.Cmd {
	return tea.Batch(s.show(), s.ready(func() tea.Cmd {
		var drop tea.Cmd
		if _, rest := profile.Split(argument); rest != "" {
			s.args[id] = argument
			drop = s.menu.Drop(id)
		}
		return tea.Batch(drop, s.menu.Go(id, true))
	}))
}

// Opens the way to change one setting: the menu on the area of the setting with its control open over the page, the menu closing once
// the setting is changed when the person did nothing else there. A setting that the page changes with a view of its own opens that page
func (s *settings) one(key string) tea.Cmd {
	return s.ready(func() tea.Cmd {
		// A key answers to a setting in any case, and the key of a table may hold a name a person gave, kept as it was spelled
		setting := s.find(key)
		for _, each := range s.all {
			if setting == nil && strings.EqualFold(each.GetKey(), key) {
				setting = each
			}
		}
		for _, table := range s.all {
			if setting == nil && table.GetKind() == configv1.Kind_KIND_TABLE &&
				strings.HasPrefix(strings.ToLower(key), strings.ToLower(table.GetKey())+".") {
				setting = table
			}
		}
		if setting == nil {
			return s.kit.WM.Toast("There is no setting named "+key+".", wm.Warn, wm.DefaultToastTTL)
		}
		if cover := s.find(s.covered[setting.GetKey()]); cover != nil {
			setting = cover
		}
		s.build()
		// A setting the page of a screen shows opens that page with its control over it, once the pages are read
		if area := s.areaOf(s.section(setting.GetSection())); area != nil && s.mounted(area.GetKey()) && !setting.GetCustom() {
			id, named := area.GetKey(), s.named
			return tea.Batch(s.show(), tui.Async(func() func() tea.Cmd {
				pages := s.kit.Pages(named)
				return func() tea.Cmd {
					s.pages = pages
					return s.toSetting(id, setting.GetKey(), true)
				}
			}))
		}
		var cmds []tea.Cmd
		hub := s.menu.Home()
		if area := s.areaOf(s.section(setting.GetSection())); area != nil {
			cmds = append(cmds, s.menu.Go(area.GetKey(), true))
			if page := s.menu.Page(area.GetKey()); page != nil {
				hub = page
			}
		}
		if !setting.GetCustom() {
			cmds = append(cmds, hub.PopTo(hub.Root()), s.on(hub).change(setting))
			s.menu.Direct()
		}
		return tea.Batch(append(cmds, s.show())...)
	})
}

// Reads the pages inside the areas whose pages are screens, each screen's off the UI thread on its own so a slow one holds back no other.
// What was read before stays listed until a screen answers again; when it does, its pages replace its old ones and the row in hand
// stays in hand
func (s *settings) readPages() tea.Cmd {
	s.pagesGen++
	gen, named := s.pagesGen, s.named
	if s.pages == nil {
		s.pages, s.pagesRead, s.pagesSince = map[string][]tui.Page{}, map[string]bool{}, time.Now()
	}
	// The line that says a search waits goes once the wait is over, whether every screen answered or not
	cmds := []tea.Cmd{tui.Async(func() func() tea.Cmd {
		time.Sleep(pagesWait)
		return func() tea.Cmd { return nil }
	})}
	for _, screen := range s.kit.Screens() {
		if screen.Area == "" || screen.Pages == nil {
			continue
		}
		cmds = append(cmds, tui.Async(func() func() tea.Cmd {
			pages := screen.Pages(s.kit, named)
			return func() tea.Cmd {
				if gen != s.pagesGen {
					return nil
				}
				s.pagesRead[screen.Area] = true
				if s.areas == nil {
					s.pages[screen.Area] = pages
					return nil
				}
				held := ""
				if i := s.areas.Index(); i >= 0 && i < len(s.rows) {
					held = s.rowKey(s.rows[i])
				}
				s.pages[screen.Area] = pages
				s.areas.Refresh()
				for i, r := range s.rows {
					if held != "" && s.rowKey(r) == held {
						s.areas.Select(i)
					}
				}
				return nil
			}
		}))
	}
	return tea.Batch(cmds...)
}

// How long a search says it waits on a screen that never answered
const pagesWait = 5 * time.Second

// Whether a search waits on a screen whose pages it lists that never answered, for a while after they were first asked for; a screen that
// answered once is listed as it last answered while it answers again
func (s *settings) pagesWaiting() bool {
	if time.Since(s.pagesSince) > pagesWait {
		return false
	}
	for _, screen := range s.kit.Screens() {
		if screen.Area != "" && screen.Pages != nil && !s.pagesRead[screen.Area] {
			return true
		}
	}
	return false
}

// What tells a row of the areas or of a search apart from every other: the key of its section or setting, else what the row does
func (s *settings) rowKey(r row) string {
	switch {
	case r.section != nil:
		return "section:" + r.section.GetKey()
	case r.setting != nil:
		return "setting:" + r.setting.GetKey()
	case r.act != nil:
		return "act:" + cmp.Or(r.act.id, r.act.title)
	}
	return ""
}

// Cells of a found area, as the list of areas reads it: its title, what is set there, and at the right how many settings inside were
// changed
func (s *settings) foundArea(r row) []string {
	t := s.kit.Env.Theme
	badge := ""
	if n := s.changed(r.section.GetKey()); n > 0 {
		badge = render.Cool(strconv.Itoa(n)+" changed", t)
	}
	return []string{r.section.GetTitle(), render.Dim(t.Glyphs.Forward, t), render.Dim(r.section.GetDescription(), t), badge}
}

// Rows a search finds in the order a person looks for them: the areas and sections, then the pages inside areas, then the settings; inside
// each kind a title that is the words typed first, then a title that holds them, then a description that holds them
func (s *settings) ranked(rows []row, query string) []row {
	query = strings.ToLower(strings.TrimSpace(query))
	kind := func(r row) int {
		switch {
		case r.section != nil:
			return 0
		case r.act != nil && r.act.id != "":
			return 1
		case r.setting != nil:
			return 2
		}
		return 3
	}
	rank := func(r row) int {
		title, about := "", ""
		switch {
		case r.section != nil:
			title, about = r.section.GetTitle(), r.section.GetDescription()
		case r.act != nil:
			title, about = r.act.title, r.act.about
		case r.setting != nil:
			title, about = r.setting.GetTitle(), r.setting.GetDescription()
		}
		title, about = strings.ToLower(title), strings.ToLower(about)
		switch {
		case query == "":
			return 0
		case title == query:
			return 0
		case strings.Contains(title, query):
			return 1
		case strings.Contains(about, query):
			return 2
		}
		return 3
	}
	slices.SortStableFunc(rows, func(a, b row) int { return cmp.Or(cmp.Compare(kind(a), kind(b)), cmp.Compare(rank(a), rank(b))) })
	return rows
}

// Rows of what a search finds for the pages inside the areas, after the areas and before the settings: each under the title of its area
// and the pages above it
func (s *settings) pageRows() []row {
	var rows []row
	for _, section := range s.sections {
		for _, page := range s.pages[section.GetKey()] {
			// A page with no argument is the area's own page, found as the area itself
			if page.Argument == "" {
				continue
			}
			area, page := section.GetKey(), page
			trail := append([]string{section.GetTitle()}, page.Trail...)
			about := page.About
			if about == "" {
				about = "A page of " + section.GetTitle()
			}
			rows = append(rows, row{act: &action{title: page.Title, about: about, words: page.Words, id: "page:" + area + ":" + page.Argument,
				verb: "open", run: func() tea.Cmd { return s.openPage(area, page, "", false) }}, trail: trail})
		}
	}
	return rows
}

// Opens the area on the page, the keys on it, and the control of the setting at key over the page once the screen shows it; no control
// for no key. Direct says the person came straight to changing the setting, so the menu closes once it is changed
func (s *settings) openPage(area string, page tui.Page, key string, direct bool) tea.Cmd {
	if page.Argument == "" {
		cmds := []tea.Cmd{s.menu.Go(area, true)}
		if hub := s.menu.Page(area); hub != nil {
			cmds = append(cmds, hub.PopTo(hub.Root()))
			if setting := s.find(key); setting != nil {
				cmds = append(cmds, s.on(hub).change(setting))
				if direct {
					s.menu.Direct()
				}
			}
		}
		return tea.Batch(cmds...)
	}
	if key != "" {
		s.controls[area] = pending{key: key, direct: direct, depth: len(page.Trail) + 2}
	}
	s.args[area] = profile.Join(s.named, page.Argument)
	return tea.Batch(s.menu.Drop(area), s.menu.Go(area, true))
}

// Opens the setting where it is changed inside the area whose page is a screen: the page inside it that shows the setting, with its control
// over that page, else the area's own page with the control over it
func (s *settings) toSetting(area, key string, direct bool) tea.Cmd {
	best, most := tui.Page{}, -1
	for _, page := range s.pages[area] {
		if n := closeness(page, key); n > most {
			best, most = page, n
		}
	}
	return s.openPage(area, best, key, direct)
}

// How closely a page shows the setting at the key: the length of the key of the page that covers it, the longest when the page names the
// setting itself, so the page of a service wins over the page of the part it is in; -1 for a page that does not show it
func closeness(page tui.Page, key string) int {
	key = strings.ToLower(key)
	most := -1
	for _, each := range page.Keys {
		each = strings.ToLower(each)
		switch {
		case each == key:
			return len(key) + 1
		case strings.HasSuffix(each, ".") && strings.HasPrefix(key, each):
			most = max(most, len(each))
		}
	}
	return most
}

// Opens what the menu was opened on: a section on the page of its area, the sections between them opened, and any other words searched
func (s *settings) reach(target string) tea.Cmd {
	if target == "" {
		return nil
	}
	section := s.section(target)
	area := s.areaOf(section)
	if area == nil {
		return s.menu.Search(target)
	}
	cmds := []tea.Cmd{s.menu.Go(area.GetKey(), true)}
	page := s.menu.Page(area.GetKey())
	if page == nil || s.mounted(area.GetKey()) {
		return cmds[0]
	}
	cmds = append(cmds, page.PopTo(page.Root()))
	at := s.on(page)
	for _, inside := range s.chain(section, area) {
		cmds = append(cmds, at.enter(inside))
	}
	return tea.Batch(cmds...)
}

// Sections from the one under the area down to the section, as a person opens them one by one; those whose settings go into another
// section are none of them
func (s *settings) chain(section, area *configv1.Section) []*configv1.Section {
	var chain []*configv1.Section
	for at := section; at != nil && at.GetKey() != area.GetKey(); at = s.section(s.up(at)) {
		if at.GetInto() == "" {
			chain = append([]*configv1.Section{at}, chain...)
		}
	}
	return chain
}

// Runs then once the settings are read: at once when they were, else after reading them
func (s *settings) ready(then func() tea.Cmd) tea.Cmd {
	if s.loaded && s.err == nil {
		return then()
	}
	return s.read(then)
}

// Reads every setting again, refreshes the lists that show them and tells the kit the areas, then runs then
func (s *settings) read(then func() tea.Cmd) tea.Cmd {
	writes := s.writes
	return tui.Async(func() func() tea.Cmd {
		response, err := s.service.List(s.kit.Env.Ctx, &configv1.ListRequest{Profile: s.named, Entries: true})
		return func() tea.Cmd {
			// A change kept while the settings were read may be missing from what was read, so they are read again
			if err == nil && s.writes != writes {
				return s.read(then)
			}
			s.err, s.loaded = err, true
			if err == nil {
				s.take(response)
			}
			s.views.Refresh()
			if err != nil {
				return s.fail(tui.Reason(err))
			}
			var again tea.Cmd
			if s.again != nil {
				again = s.again()
			}
			if then == nil {
				return again
			}
			return tea.Batch(again, then())
		}
	})
}

// Reads the settings once as the app starts, so the palette lists the areas
func (s *settings) learn() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := s.service.List(s.kit.Env.Ctx, &configv1.ListRequest{})
		return func() tea.Cmd {
			if err == nil {
				s.take(response)
			}
			return nil
		}
	})
}

// Keeps what the service listed, finds the settings each picker keeps beside another, and tells the kit the areas
func (s *settings) take(response *configv1.ListResponse) {
	s.groups, s.sections, s.all = response.GetGroups(), response.GetSections(), response.GetSettings()
	s.byKey = make(map[string]*configv1.Section, len(s.sections))
	for _, section := range s.sections {
		s.byKey[section.GetKey()] = section
	}
	s.covered = map[string]string{}
	for _, setting := range s.all {
		for _, key := range s.beside(setting) {
			s.covered[key] = setting.GetKey()
		}
	}
	s.screened = map[string]bool{}
	for _, screen := range s.kit.Screens() {
		if screen.Area != "" {
			s.screened[screen.Area] = true
		}
	}
	var areas []tui.Area
	for _, r := range s.home(false) {
		about := ""
		switch {
		case r.section != nil:
			about = r.section.GetDescription()
		case r.act != nil:
			about = r.act.about
		}
		areas = append(areas, tui.Area{ID: s.idOf(r), Title: s.name(r), Group: s.groupOf(r), About: about})
	}
	s.kit.Areas(areas)
}

// Rows of the areas: those with something to change or a page made for them, in their groups, the settings files among those of the app.
// Everything there is to find once words are typed
func (s *settings) home(searching bool) []row {
	if searching {
		found := append(s.found(""), s.pageRows()...)
		query := ""
		if s.areas != nil {
			query = s.areas.Query()
		}
		return append(s.ranked(found, query), row{act: s.filesAction()})
	}
	var rows []row
	for _, section := range s.sections {
		if section.GetArea() && s.holds(section.GetKey()) {
			rows = append(rows, row{section: section})
		}
	}
	at := len(rows)
	for i, r := range rows {
		if r.section.GetGroup() == filesGroup {
			at = i + 1
		}
	}
	return slices.Insert(rows, at, row{act: s.filesAction()})
}

// Row of the settings files, which opens their page
func (s *settings) filesAction() *action {
	return &action{title: "Settings files", about: "Open config.yaml and your keys file in an editor", verb: "open",
		run: func() tea.Cmd { return s.menu.Go(filesArea, true) }}
}

// Heading of the group a row of the areas is listed under
func (s *settings) groupOf(r row) string {
	id := filesGroup
	if r.section != nil {
		id = r.section.GetGroup()
	}
	for _, group := range s.groups {
		if group.GetId() == id {
			return group.GetTitle()
		}
	}
	return ""
}

// Cells of a row of the areas filling the menu: its name, what is set there and how many settings inside were changed
func (s *settings) homeCells(r row) []string {
	t := s.kit.Env.Theme
	switch {
	case r.section != nil:
		badge := ""
		if n := s.changed(r.section.GetKey()); n > 0 {
			badge = render.Cool(strconv.Itoa(n)+" changed", t)
		}
		return []string{r.section.GetTitle(), render.Dim(r.section.GetDescription(), t), badge}
	case r.act != nil:
		return []string{r.act.title, render.Dim(r.act.about, t), ""}
	}
	return []string{s.name(r), "", ""}
}

func (s *settings) section(key string) *configv1.Section {
	return s.byKey[key]
}

func (s *settings) find(key string) *configv1.Setting {
	if key == "" {
		return nil
	}
	for _, setting := range s.all {
		if setting.GetKey() == key {
			return setting
		}
	}
	return nil
}

// Section a section is listed under in the menu: the area its settings go into, none for an area, else the section its key sits under
func (s *settings) up(section *configv1.Section) string {
	switch {
	case section.GetInto() != "":
		return section.GetInto()
	case section.GetArea():
		return ""
	}
	return section.GetParent()
}

// Area of the menu a section belongs to, nil for none
func (s *settings) areaOf(section *configv1.Section) *configv1.Section {
	for at := section; at != nil; at = s.section(s.up(at)) {
		if at.GetArea() {
			return at
		}
	}
	return nil
}

// Says why something failed under the hub, or in a toast while none shows
func (s *settings) fail(text string) tea.Cmd {
	if s.hub != nil && s.hub.Shown() {
		return s.hub.Fail(text)
	}
	return s.kit.WM.Toast(text, wm.Error, wm.DefaultToastTTL)
}

func decode(value string) any {
	var decoded any
	if yaml.Unmarshal([]byte(value), &decoded) != nil {
		return nil
	}
	return decoded
}

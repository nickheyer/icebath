// Page of Autofill vault in the settings menu: the logins, cards and addresses the agent may fill in, each with a page of its fields, adding
// one as a job in steps, the password managers with their pages, locking them all, and the page of the vault key
package app

import (
	"cmp"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/vault"
	vaultconfig "github.com/nickheyer/icebath/internal/vault/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	vaultv1 "github.com/nickheyer/icebath/pkg/gen/icebath/vault/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/vault/v1/vaultv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Screen of the vault, the page of its area
const Screen = "vault"

// Arguments of the screen that open a page inside it: the vault key, and before its handle an item of this vault; the name of a password
// manager opens its page
const (
	KeyOf  = "key"
	ItemOf = "item:"
)

// Words of the kinds of items, their headings and what each is for
var (
	kinds    = map[string]string{"login": "login", "payment": "card", "address": "address"}
	titles   = map[string]string{"login": "Login", "payment": "Card", "address": "Address"}
	headings = map[string]string{"login": "Logins", "payment": "Cards", "address": "Addresses"}
	purposes = map[string]string{"login": "A name and a password the agent may sign in with on a site", "payment": "A card the agent may pay with on a site",
		"address": "An address the agent may type into the forms of a site"}
)

const managersHeading = "Password managers"

type screens struct{}

func Register(host *app.App) {
	host.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := vaultv1connect.NewVaultServiceClient(conn)
	settings := configv1connect.NewConfigServiceClient(conn)
	return []tui.Screen{{
		ID:    Screen,
		Group: tui.GroupSecrets,
		Order: 20,
		Title: "Autofill vault",
		Short: "Keep the logins and cards the agent may fill in",
		Slash: []string{"vault"},
		Usage: "[@profile] [password manager, key or words to search]",
		Area:  vaultconfig.Key,
		Pages: func(kit *tui.Kit, name string) []tui.Page { return vaultPages(kit, service, name) },
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			name, target := profile.Split(argument)
			return (&page{kit: kit, service: service, settings: settings, profile: name}).open(target)
		},
	}}
}

// The page of the vault for one profile, and what every page inside it reads
type page struct {
	kit      *tui.Kit
	service  vaultv1connect.VaultServiceClient
	settings configv1connect.ConfigServiceClient
	// Profile whose vault is shown, the one in use when empty
	profile string
	hub     *cards.Hub
	root    *cards.List
	options *configapp.Rows
	views   cards.Watched
	state   *vaultv1.ListResponse
	loaded  bool
	err     error
	opened  bool
}

// Row of the first view: an item, the row that adds one, a password manager, the row that locks them all, or the vault key
type row struct {
	item   *vaultv1.Item
	add    bool
	source *vaultv1.Source
	lock   bool
	key    bool
}

func (p *page) open(target string) tea.Cmd {
	t := p.kit.Env.Theme
	var shown []row
	p.root = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Autofill vault",
		Glyph:   t.Glyphs.Keys,
		Key:     Screen,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   p.intro,
		Columns: configapp.Columns,
		Rows: func() [][]string {
			shown = p.rows()
			cells := make([][]string, len(shown))
			for i, r := range shown {
				cells[i] = p.cells(r)
			}
			return cells
		},
		Group: func(i int) string {
			switch r := shown[i]; {
			case r.item != nil:
				return headings[r.item.GetKind()]
			case r.add && i > 0 && shown[i-1].item != nil:
				return headings[shown[i-1].item.GetKind()]
			case r.add:
				return ""
			case r.source != nil, r.lock:
				return managersHeading
			}
			return "This vault"
		},
		Words: func(i int) string {
			if item := shown[i].item; item != nil {
				return kinds[item.GetKind()] + " " + item.GetSource() + " " + item.GetOrigin() + " " + item.GetIdentifier()
			}
			return ""
		},
		Detail: func(i int) []string { return p.detail(shown[i]) },
		Empty:  func() string { return tui.Waiting(p.loaded, p.err, "") },
		Free:   map[string]bool{"hub.add": true, "hub.refresh": true},
		OnFocus: func(*cards.List) tea.Cmd {
			if !p.opened || p.hub.Depth() > 0 {
				return nil
			}
			p.opened = false
			return p.load(nil)
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				return []wm.Hint{{Action: "hub.add", Label: "add item"}}
			}
			return p.hints(shown[i])
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				switch r := shown[i]; {
				case r.item != nil && r.item.GetLocal():
					p.opened = true
					return p.itemPage(r.item)
				case r.item != nil:
					return nil
				case r.add:
					return p.add()
				case r.source != nil:
					p.opened = true
					return p.managerPage(r.source.GetName())
				case r.lock:
					return p.change("Locking every password manager…", func() (string, error) {
						_, err := p.service.Lock(p.kit.Env.Ctx, &vaultv1.LockRequest{Profile: p.profile})
						return "Every password manager is locked. The agent asks you to unlock one when it needs a login.", err
					})
				}
				p.opened = true
				return p.keyPage()
			},
			"hub.add":     func(*cards.List, int) tea.Cmd { return p.add() },
			"hub.remove":  func(l *cards.List, i int) tea.Cmd { return p.remove(l, shown[i].item) },
			"hub.refresh": func(*cards.List, int) tea.Cmd { return p.load(nil) },
		},
	})
	p.root.ToggleDetail()
	hub, cmd := p.kit.Show(Screen, p.root)
	p.hub = hub
	p.options = configapp.NewRows(p.kit, hub, p.settings, p.profile)
	p.watch(p.root)
	return tea.Batch(cmd, p.load(func() tea.Cmd { return p.reach(target) }))
}

// What the area is and why it is safe, then while it holds nothing that nothing is saved yet
func (p *page) intro() []string {
	lines := []string{"Logins and cards the agent may fill into web pages. It never sees them."}
	if p.loaded && p.err == nil && len(p.state.GetItems()) == 0 {
		lines = append(lines, "No items yet.")
	}
	return lines
}

// Rows of the first view: the items by kind, the row that adds one, the password managers with the row that locks them all while one is
// unlocked, then the vault key
func (p *page) rows() []row {
	if !p.loaded || p.err != nil {
		return nil
	}
	items := slices.Clone(p.state.GetItems())
	slices.SortStableFunc(items, func(a, b *vaultv1.Item) int {
		return cmp.Compare(place(a.GetKind()), place(b.GetKind()))
	})
	var rows []row
	for _, item := range items {
		rows = append(rows, row{item: item})
	}
	rows = append(rows, row{add: true})
	unlocked := false
	for _, source := range p.state.GetSources() {
		rows = append(rows, row{source: source})
		unlocked = unlocked || source.GetState() == "on" && !source.GetLocked() && !source.GetReady()
	}
	if unlocked {
		rows = append(rows, row{lock: true})
	}
	return append(rows, row{key: true})
}

// Place of a kind of item among the kinds a vault keeps, in their order
func place(kind string) int {
	if at := slices.Index(vault.Kinds, kind); at >= 0 {
		return at
	}
	return len(vault.Kinds)
}

func (p *page) cells(r row) []string {
	t := p.kit.Env.Theme
	opens := render.Dim(t.Glyphs.Forward, t)
	switch {
	case r.item != nil:
		parts := []string{r.item.GetIdentifier(), r.item.GetOrigin()}
		if !r.item.GetLocal() {
			parts = append(parts, "from "+r.item.GetSource())
		}
		return []string{r.item.GetLabel(), "", render.Dim(strings.Join(filled(parts), " · "), t)}
	case r.add:
		return []string{"Add an item", opens, ""}
	case r.source != nil:
		return []string{r.source.GetDisplayName(), "", managerWords(r.source, t)}
	case r.lock:
		return []string{"Lock every password manager", "", ""}
	}
	return []string{"Vault key", opens, keyWords(p.state.GetKey())}
}

// Word of a kind of item after the article a sentence starts it with: A login, An address
func withArticle(word string) string {
	if word != "" && strings.ContainsRune("aeiou", rune(word[0])) {
		return "An " + word
	}
	return "A " + word
}

// Texts that are not empty
func filled(texts []string) []string {
	var out []string
	for _, text := range texts {
		if text != "" {
			out = append(out, text)
		}
	}
	return out
}

// Where the vault key is, in a few words
func keyWords(key *vaultv1.Key) string {
	switch key.GetStorage() {
	case "file":
		return "In a file"
	case "keychain":
		return "In the keychain"
	}
	return "Not made yet"
}

// Lines under the rows about the row in hand
func (p *page) detail(r row) []string {
	t := p.kit.Env.Theme
	switch {
	case r.item != nil:
		item := r.item
		lines := []string{withArticle(kinds[item.GetKind()]) + " the agent may fill in on " + item.GetOrigin() + "."}
		if item.GetOtp() {
			lines = append(lines, "It fills the authenticator code too.")
		}
		if !item.GetLocal() {
			lines = append(lines, render.Dim("Kept in "+item.GetSource()+". It is changed there.", t))
		}
		return lines
	case r.add:
		return []string{"Save a login, a card or an address the agent may fill in."}
	case r.source != nil:
		return []string{"The logins kept in " + r.source.GetDisplayName() + ", which the agent may fill in while it is in use."}
	case r.lock:
		return []string{"Forget every unlock, so the agent asks you again when it needs a login."}
	}
	return []string{"The key that encrypts what this vault keeps."}
}

func (p *page) hints(r row) []wm.Hint {
	switch {
	case r.item != nil && r.item.GetLocal():
		return []wm.Hint{{Action: "hub.use", Label: "open"}, {Action: "hub.add", Label: "add item"}, {Action: "hub.remove"}}
	case r.item != nil:
		return []wm.Hint{{Action: "hub.add", Label: "add item"}}
	case r.add:
		return []wm.Hint{{Action: "hub.use", Label: "add"}}
	case r.lock:
		return []wm.Hint{{Action: "hub.use", Label: "lock all"}, {Action: "hub.add", Label: "add item"}}
	}
	return []wm.Hint{{Action: "hub.use", Label: "open"}, {Action: "hub.add", Label: "add item"}}
}

// Goes where the page was opened at: the vault key, a password manager, else the vault searched for the words
func (p *page) reach(target string) tea.Cmd {
	if target = strings.TrimSpace(target); target == "" {
		return nil
	}
	if strings.EqualFold(target, KeyOf) {
		p.opened = true
		return p.keyPage()
	}
	if handle, ok := strings.CutPrefix(target, ItemOf); ok {
		for _, item := range p.state.GetItems() {
			if item.GetHandle() == handle && item.GetLocal() {
				p.opened = true
				return p.itemPage(item)
			}
		}
		return p.hub.Fail("That item is no longer in this vault.")
	}
	for _, source := range p.state.GetSources() {
		if strings.EqualFold(source.GetName(), target) || strings.EqualFold(source.GetDisplayName(), target) {
			p.opened = true
			return p.managerPage(source.GetName())
		}
	}
	p.root.Filter(target)
	return nil
}

// Reads the vault, then the option rows, then draws every list again and runs then
func (p *page) load(then func() tea.Cmd) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := p.service.List(p.kit.Env.Ctx, &vaultv1.ListRequest{Profile: p.profile})
		return func() tea.Cmd {
			return p.options.Load(func() tea.Cmd {
				p.loaded, p.err = true, err
				if err == nil {
					p.state = response
				}
				p.refresh()
				switch {
				case err != nil && p.hub.Top() != p.root:
					// The first view says why in place of its rows; a view over it has the reason under it
					return p.hub.Fail(tui.Reason(err))
				case then == nil || err != nil:
					return nil
				}
				return then()
			})
		}
	})
}

// Draws every list of the page again
func (p *page) refresh() {
	p.views.Refresh()
}

// Keeps a list among those drawn again on every read
func (p *page) watch(view *cards.List) {
	p.views.Add(view)
	p.options.Watch(view)
}

// Runs a change of the vault with the hub saying it waits, then says what it did and reads the vault again
func (p *page) change(busy string, work func() (string, error)) tea.Cmd {
	p.hub.Busy(busy)
	return tui.Call(p.hub, work, func(note string) tea.Cmd {
		return tea.Batch(p.hub.Done(note), p.load(nil))
	})
}

// Password manager of the name as last read, nil when it is gone
func (p *page) source(name string) *vaultv1.Source {
	for _, source := range p.state.GetSources() {
		if source.GetName() == name {
			return source
		}
	}
	return nil
}

// Removes an item of this vault after a question that says it is gone for good; an item of a password manager is not removed here
func (p *page) remove(l *cards.List, item *vaultv1.Item) tea.Cmd {
	if item == nil || !item.GetLocal() {
		return nil
	}
	question := cards.Question{Text: "Remove " + item.GetLabel() + "? The agent can no longer fill it in, and what it holds is gone for good.",
		Verb: "Remove", Deletes: true}
	l.Confirm(question, func() tea.Cmd {
		return p.change("Removing "+item.GetLabel()+"…", func() (string, error) {
			_, err := p.service.Remove(p.kit.Env.Ctx, &vaultv1.RemoveRequest{Profile: p.profile, Handle: item.GetHandle()})
			return "Removed " + item.GetLabel() + ".", err
		})
	})
	return nil
}

// Options of a password manager or of the vault, the settings of its own section with its advanced ones, those in place while they are few
func (p *page) sectionOptions(section string, leave ...string) (shown []*configv1.Setting, advanced []string, open func() tea.Cmd) {
	everyday, rare := p.options.Own(section, leave...)
	if cells, open, ok := p.options.OwnAdvanced(section, leave...); ok {
		return everyday, cells, open
	}
	return append(everyday, rare...), nil, nil
}

// Extension interface and loader, one registration per extension point linked into the binary
package ext

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Package linked into the binary that adds to icebath through a host
type Extension interface {
	Manifest() Manifest
	// Declares everything the extension provides, called once at start
	Attach(h *Host) error
}

// Identity of an extension
type Manifest struct {
	Name        string
	Version     string
	Description string
}

// One thing an extension provided
type Contribution struct {
	// Kind of point, such as kind, workflow, tool or panel
	Point  string
	Name   string
	Detail string
}

// Extension after loading with what it provided
type Loaded struct {
	Manifest
	Contributions []Contribution
}

// Command an extension adds to the palette, the slash line and the keymap
type Command struct {
	// Keymap action id, such as review.start
	ID string
	// Palette title, the command staying out of the palette when empty
	Title string
	// Slash command name, none when empty
	Slash string
	// One line for the keybind editor and the slash list
	Desc string
	// Argument placeholder, the command asks for one when none came
	Arg string
	// Default chords
	Keys []string
	// Reports whether the command applies now, always when nil
	When func(c *Call) bool
	Run  func(c *Call) tea.Cmd
	// Extension the command came from
	Source string
}

// What a command runs with
type Call struct {
	Ctx context.Context
	App *harness.App
	// Session on the stage, nil while none is
	Live *harness.Live
	Args string
	UI   UI
}

// What a command may do to the interface
type UI interface {
	// Opens a card in the workspace on the stage, focusing it when already open
	Open(card wm.Card) tea.Cmd
	Toast(text string, level wm.Level) tea.Cmd
	// Asks for a line of text
	Prompt(placeholder, value string, submit func(text string) tea.Cmd) tea.Cmd
	// Offers a choice
	Pick(placeholder string, items []wm.Item) tea.Cmd
	// Sends input to the session, steering a run going on it and starting one otherwise
	Submit(live *harness.Live, parts []protocol.Part) tea.Cmd
	// Starts a registered workflow from the session on the stage
	Start(workflow, arg string) tea.Cmd
	// Live state cards read
	Env() *cards.Env
}

// Card an extension adds, shared by every workspace and opened by key, slash and palette
type Panel struct {
	// Card key, slash command name and the end of the open.<key> action
	Key    string
	Title  string
	Detail string
	// Default chords of the open action
	Keys []string
	New  func(env *cards.Env) wm.Card
	// Extension the panel came from
	Source string
}

// Host one extension declares into, recording the first failure
type Host struct {
	name  string
	gifts []Contribution
	err   error
}

func (h *Host) fail(err error) {
	if err != nil && h.err == nil {
		h.err = err
	}
}

func (h *Host) gave(point, name, detail string) {
	h.gifts = append(h.gifts, Contribution{Point: point, Name: name, Detail: detail})
}

// Adds a wire protocol profiles may name
func (h *Host) Protocol(p protocol.Protocol) {
	if p == nil {
		h.fail(errors.New("protocol: nil"))
		return
	}
	if _, dup := protocol.Lookup(p.ID()); dup {
		h.fail(fmt.Errorf("protocol %s is already registered", p.ID()))
		return
	}
	protocol.Register(p)
	h.gave("protocol", string(p.ID()), "")
}

// Adds a built in endpoint profile
func (h *Host) Profile(p profiles.Profile) {
	if err := p.Validate(); err != nil {
		h.fail(err)
		return
	}
	if _, dup := profiles.Lookup(p.Name); dup {
		h.fail(fmt.Errorf("profile %s is already registered", p.Name))
		return
	}
	profiles.Register(p)
	h.gave("profile", p.Name, string(p.Protocol)+" at "+p.BaseURL)
}

// Adds a vendor that lists the models of an endpoint family
func (h *Host) Vendor(v profiles.Vendor) {
	if v.Name == "" || v.Models == nil {
		h.fail(errors.New("vendor needs a name and a model lister"))
		return
	}
	if _, dup := profiles.LookupVendor(v.Name); dup {
		h.fail(fmt.Errorf("vendor %s is already registered", v.Name))
		return
	}
	profiles.RegisterVendor(v)
	h.gave("vendor", v.Name, "")
}

// Adds an auth kind profiles may name
func (h *Host) Auth(kind profiles.AuthKind, build profiles.AuthBuilder) {
	if kind == "" || build == nil {
		h.fail(errors.New("auth needs a kind and a builder"))
		return
	}
	for _, have := range profiles.AuthKinds() {
		if have == kind {
			h.fail(fmt.Errorf("auth kind %s is already registered", kind))
			return
		}
	}
	profiles.RegisterAuth(kind, build)
	h.gave("auth", string(kind), "")
}

// Adds models whose metadata wins over the built in catalog
func (h *Host) Catalog(models ...profiles.Model) {
	for _, m := range models {
		if m.ID == "" {
			h.fail(errors.New("catalog entry has no id"))
			return
		}
	}
	profiles.AddCatalog(models...)
	for _, m := range models {
		h.gave("model", m.ID, m.Name)
	}
}

// Adds a kind sessions and child agents may run as
func (h *Host) Kind(k harness.Kind) {
	k.Source = h.name
	if err := harness.RegisterKind(k); err != nil {
		h.fail(err)
		return
	}
	h.gave("kind", k.Name, k.Description)
}

// Adds a workflow, offered as the palette command and slash command of its name
func (h *Host) Workflow(w harness.Workflow) {
	w.Source = h.name
	if err := harness.RegisterWorkflow(w); err != nil {
		h.fail(err)
		return
	}
	if err := keys.Add(keys.Action{ID: "workflow." + w.Name, Context: keys.Global, Label: w.Name, Doc: w.Description}); err != nil {
		h.fail(err)
		return
	}
	h.gave("workflow", w.Name, w.Description)
}

// Adds a setting read through the configuration, shown in the app tab and taken by the -k flag
func (h *Host) Setting(s harness.Setting) {
	s.Source = h.name
	if err := harness.RegisterSetting(s); err != nil {
		h.fail(err)
		return
	}
	h.gave("setting", s.Key, s.Help)
}

// Adds tools built once the app stands
func (h *Host) Tools(build func(a *harness.App) ([]tool.Tool, error)) {
	if build == nil {
		h.fail(errors.New("tools: a builder is required"))
		return
	}
	name := h.name
	err := harness.RegisterTools(func(a *harness.App) ([]tool.Tool, error) {
		tools, err := build(a)
		if err != nil {
			return nil, fmt.Errorf("extension %s: %w", name, err)
		}
		return tools, nil
	})
	if err != nil {
		h.fail(err)
		return
	}
	h.gave("tools", h.name, "built when the app starts")
}

// Adds a hook built for every agent
func (h *Host) Hook(build func(info harness.AgentInfo) agent.Hook) {
	if err := harness.RegisterHook(build); err != nil {
		h.fail(err)
		return
	}
	h.gave("hook", h.name, "runs with every agent")
}

// Adds text to the system prompt of every run
func (h *Host) Prompt(fragment func(ctx context.Context, a *harness.App, ep harness.Endpoint) string) {
	if err := harness.RegisterPrompt(fragment); err != nil {
		h.fail(err)
		return
	}
	h.gave("prompt", h.name, "appended to the system prompt")
}

// Adds a command to the palette, the slash line and the keymap
func (h *Host) Command(c Command) {
	c.Source = h.name
	if err := c.validate(); err != nil {
		h.fail(err)
		return
	}
	if err := keys.Add(keys.Action{ID: c.ID, Context: keys.Global, Label: label(c), Doc: c.Desc, Default: c.Keys}); err != nil {
		h.fail(err)
		return
	}
	mu.Lock()
	commands = append(commands, c)
	mu.Unlock()
	h.gave("command", c.ID, c.Desc)
}

// Checks the id, the slash name and the run of a command, and that no other extension command holds either
func (c Command) validate() error {
	if c.ID == "" || c.Run == nil {
		return errors.New("command needs an id and a run")
	}
	if c.Slash != "" && !slashName(c.Slash) {
		return fmt.Errorf("command %s: slash name %q must be lower case letters, digits and dashes", c.ID, c.Slash)
	}
	mu.RLock()
	defer mu.RUnlock()
	for _, have := range commands {
		if have.ID == c.ID {
			return fmt.Errorf("command %s is already registered", c.ID)
		}
		if c.Slash != "" && have.Slash == c.Slash {
			return fmt.Errorf("slash command %s is already registered", c.Slash)
		}
	}
	return nil
}

// Word legends show for a command, its title after any colon or its slash name
func label(c Command) string {
	if c.Title != "" {
		if _, after, ok := strings.Cut(c.Title, ": "); ok {
			return strings.TrimSuffix(after, "…")
		}
		return c.Title
	}
	if c.Slash != "" {
		return c.Slash
	}
	return c.ID
}

// Reports whether a name is lower case letters, digits and dashes starting with a letter
func slashName(name string) bool {
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' {
			continue
		}
		return false
	}
	return true
}

// Adds a panel openable by key, slash and palette
func (h *Host) Panel(p Panel) {
	p.Source = h.name
	if !slashName(p.Key) || p.Title == "" || p.New == nil {
		h.fail(errors.New("panel needs a lower case key, a title and a constructor"))
		return
	}
	mu.RLock()
	for _, have := range panels {
		if have.Key == p.Key {
			mu.RUnlock()
			h.fail(fmt.Errorf("panel %s is already registered", p.Key))
			return
		}
	}
	mu.RUnlock()
	if err := keys.Add(keys.Action{ID: "open." + p.Key, Context: keys.Global, Label: strings.ToLower(p.Title), Doc: "open the " + strings.ToLower(p.Title) + " panel", Default: p.Keys}); err != nil {
		h.fail(err)
		return
	}
	mu.Lock()
	panels = append(panels, p)
	mu.Unlock()
	h.gave("panel", p.Key, p.Detail)
}

// Adds a theme choosable by name
func (h *Host) Theme(fn func() theme.Theme) {
	if err := theme.Register(fn); err != nil {
		h.fail(err)
		return
	}
	h.gave("theme", fn().Name, "")
}

// Adds a keymap context the cards of the extension answer in
func (h *Host) Context(name, doc string) {
	if err := keys.AddContext(name, doc); err != nil {
		h.fail(err)
		return
	}
	h.gave("context", name, doc)
}

// Adds a key action in a context the extension added, fired at the focused card
func (h *Host) Action(a keys.Action) {
	if err := keys.Add(a); err != nil {
		h.fail(err)
		return
	}
	h.gave("action", a.ID, a.Doc)
}

// Adds a listener for every message the harness sends the program
func (h *Host) Observe(fn func(msg any)) {
	if fn == nil {
		h.fail(errors.New("observe: nil listener"))
		return
	}
	mu.Lock()
	observers = append(observers, fn)
	mu.Unlock()
	h.gave("observer", h.name, "hears every harness message")
}

var (
	mu         sync.RWMutex
	registered []Extension
	loaded     []Loaded
	commands   []Command
	panels     []Panel
	observers  []func(any)
	once       sync.Once
	loadErr    error
)

// Makes an extension part of the next Load, panicking on a nameless or duplicate one
func Register(e Extension) {
	if e == nil {
		panic("ext: nil extension")
	}
	name := e.Manifest().Name
	if !profiles.ValidName(name) {
		panic("ext: extension has an invalid name " + fmt.Sprintf("%q", name))
	}
	mu.Lock()
	defer mu.Unlock()
	for _, have := range registered {
		if have.Manifest().Name == name {
			panic("ext: duplicate extension " + name)
		}
	}
	registered = append(registered, e)
}

// Attaches every registered extension once, in registration order, stopping at the first failure
func Load() error {
	once.Do(func() {
		mu.RLock()
		list := append([]Extension(nil), registered...)
		mu.RUnlock()
		for _, e := range list {
			m := e.Manifest()
			h := &Host{name: m.Name}
			err := e.Attach(h)
			if h.err != nil {
				err = errors.Join(h.err, err)
			}
			if err != nil {
				loadErr = fmt.Errorf("extension %s: %w", m.Name, err)
				return
			}
			mu.Lock()
			loaded = append(loaded, Loaded{Manifest: m, Contributions: h.gifts})
			mu.Unlock()
		}
	})
	return loadErr
}

// Extensions loaded so far, sorted by name
func Extensions() []Loaded {
	mu.RLock()
	defer mu.RUnlock()
	out := append([]Loaded(nil), loaded...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Commands extensions added, in registration order
func Commands() []Command {
	mu.RLock()
	defer mu.RUnlock()
	return append([]Command(nil), commands...)
}

// Panels extensions added, in registration order
func Panels() []Panel {
	mu.RLock()
	defer mu.RUnlock()
	return append([]Panel(nil), panels...)
}

// Listeners extensions added
func Observers() []func(any) {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]func(any), len(observers))
	copy(out, observers)
	return out
}

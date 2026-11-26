package tui

import (
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
	ID      string
	Title   string
	Short   string
	Slash   []string
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

	screens   []Screen
	reports   []func(status *wm.Status)
	terminals int
	// Settings of the kit by the names its cards read them by
	settings map[string]string
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

func (k *Kit) Hub(key string, root wm.Card) *cards.Hub {
	return cards.NewHub(k.Env, key, root, k.WM.Size)
}

func (k *Kit) Screens() []Screen {
	return slices.Clone(k.screens)
}

func (k *Kit) Open(id, argument string) tea.Cmd {
	for _, screen := range k.screens {
		if screen.ID == id {
			return screen.Open(k, argument)
		}
	}
	return nil
}

func (k *Kit) Slash(line string) (tea.Cmd, bool) {
	word, argument, _ := strings.Cut(strings.TrimSpace(line), " ")
	name, ok := strings.CutPrefix(word, "/")
	if !ok || name == "" {
		return nil, false
	}
	for _, screen := range k.screens {
		if slices.Contains(screen.Slash, name) && (screen.Ready == nil || screen.Ready(k)) {
			return screen.Open(k, strings.TrimSpace(argument)), true
		}
	}
	return nil, false
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
		screens:  screens,
		History:  new(history),
		settings: map[string]string{},
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

// Opens the screens that open when the app starts
func (m *model) startup() tea.Cmd {
	var cmds []tea.Cmd
	for _, screen := range m.kit.screens {
		if screen.Startup {
			cmds = append(cmds, screen.Open(m.kit, ""))
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
	return tea.Batch(m.kit.WM.Fire("session.stop"), m.kit.WM.Toast("Press "+m.kit.Env.Keys.Display("interrupt")+" again to quit", wm.Info, again))
}

func (m *model) commands() []wm.Command {
	commands := []wm.Command{
		{ID: "interrupt", Run: m.interrupt},
	}
	for _, screen := range m.kit.screens {
		if screen.Title == "" {
			continue
		}
		var ready func() bool
		if screen.Ready != nil {
			ready = func() bool { return screen.Ready(m.kit) }
		}
		commands = append(commands, wm.Command{ID: "open." + screen.ID, Title: screen.Title, Detail: screen.Short, When: ready, Run: func() tea.Cmd { return screen.Open(m.kit, "") }})
	}
	return append(commands,
		wm.Command{ID: "workspace.new", Title: "Space: new", Run: m.space},
		wm.Command{
			ID:    "workspace.close",
			Title: "Space: close",
			When:  func() bool { return len(m.kit.WM.Workspaces()) > 1 },
			Run:   func() tea.Cmd { return m.kit.WM.RemoveWorkspace(m.kit.WM.ActiveIndex()) },
		},
		wm.Command{ID: "quit", Title: "Quit", Last: true, Run: m.quit},
	)
}

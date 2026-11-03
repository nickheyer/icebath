package cards

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Plugins installed and offered, toggled, installed, updated, configured and removed in place
type Plugins struct {
	*lister
	env       *Env
	rows      []harness.PluginRow
	offers    []harness.Offer
	available bool
}

// Outcome of a plugin operation run off the event loop
type pluginDoneMsg struct {
	text string
	err  error
	// Id of a plugin the operation installed, empty for other operations
	installed string
}

// Plugin that was just installed, so the app may run its onboarding skill
type PluginInstalledMsg struct {
	ID string
}

func NewPlugins(env *Env) *Plugins {
	p := &Plugins{env: env}
	t := env.Theme
	l := &lister{env: env, title: "PLUGINS", glyph: t.Glyphs.Ext, id: "plugins", ctx: keys.Plugins, primary: "plugins.toggle", cols: []render.Column{{Title: ""}, {Title: "PLUGIN", Max: 28}, {Title: "VERSION", Max: 14}, {Title: "FROM", Max: 14}, {Title: "HOLDS", Max: 26}, {Title: "DESCRIPTION", Flex: 1}}}
	l.rows = p.table
	l.detail = p.detailOf
	l.empty = func() string {
		if p.available {
			return "no marketplace offers anything " + t.Glyphs.Sep + " " + env.keyed("plugins.market.add", "add marketplace")
		}
		return "no plugins " + t.Glyphs.Sep + " " + env.keyed("plugins.install", "install") + " " + t.Glyphs.Sep + " " + env.keyed("plugins.market.add", "add marketplace")
	}
	l.free = map[string]bool{"plugins.install": true, "plugins.market.add": true, "plugins.market.remove": true, "plugins.available": true, "plugins.reload": true}
	l.actions = map[string]func(*lister, int) tea.Cmd{
		"plugins.toggle":        p.toggle,
		"plugins.install":       p.install,
		"plugins.uninstall":     p.uninstall,
		"plugins.update":        p.update,
		"plugins.configure":     p.configure,
		"plugins.market.add":    p.addMarket,
		"plugins.market.remove": p.removeMarket,
		"plugins.available":     func(l *lister, i int) tea.Cmd { p.available = !p.available; l.sel = 0; l.refresh(); return nil },
		"plugins.reload": func(l *lister, i int) tea.Cmd {
			return p.run("reloaded plugins", func(ctx context.Context) error { return env.Editor.ReloadPlugins(ctx) })
		},
		"plugins.open": p.open,
	}
	l.status = func() string {
		if p.available {
			return "available from every marketplace"
		}
		return "installed and discovered"
	}
	p.lister = l
	return p
}

func (p *Plugins) Title() string { return "PLUGINS" }
func (p *Plugins) Key() string   { return "plugins" }

// Rows of the table, the installed plugins or the offers of every marketplace
func (p *Plugins) table() [][]string {
	t := p.env.Theme
	var rows [][]string
	if p.available {
		p.offers, _ = p.env.Editor.Offers()
		for _, o := range p.offers {
			mark := render.Dim(t.Glyphs.Hollow, t)
			if o.Installed {
				mark = render.Good(t.Glyphs.Live, t)
			}
			rows = append(rows, []string{mark, o.Name, o.Version, o.Marketplace, o.Category, render.Summary(o.Description, 80, t)})
		}
		return rows
	}
	p.rows = p.env.Editor.PluginRows()
	for _, r := range p.rows {
		mark := render.Dim(t.Glyphs.Hollow, t)
		switch {
		case len(r.Problems) > 0 || len(r.Missing) > 0:
			mark = render.Warm(t.Glyphs.Live, t)
		case r.Enabled:
			mark = render.Good(t.Glyphs.Live, t)
		}
		from := r.Marketplace
		if from == "" {
			from = r.From
		}
		holds := strings.Join(nonZero(map[string]int{"skills": r.Skills, "agents": r.Agents, "hooks": r.Hooks, "servers": r.Servers}), " ")
		rows = append(rows, []string{mark, r.Name, r.Version, from, holds, render.Summary(r.Description, 80, t)})
	}
	return rows
}

// Counts that are not zero as words, in a fixed order
func nonZero(counts map[string]int) []string {
	var out []string
	for _, k := range []string{"skills", "agents", "hooks", "servers"} {
		if counts[k] > 0 {
			out = append(out, fmt.Sprintf("%d %s", counts[k], k))
		}
	}
	return out
}

// Detail lines of the row under the cursor
func (p *Plugins) detailOf(i int) []string {
	t := p.env.Theme
	if p.available {
		if i < 0 || i >= len(p.offers) {
			return nil
		}
		o := p.offers[i]
		state := "not installed"
		if o.Installed {
			state = "installed"
		}
		return []string{render.KV("id", o.ID, t), render.KV("state", state, t), render.KV("description", o.Description, t)}
	}
	if i < 0 || i >= len(p.rows) {
		return nil
	}
	r := p.rows[i]
	state := "disabled"
	if r.Enabled {
		state = "enabled"
	}
	out := []string{render.KV("id", r.ID, t), render.KV("state", state, t), render.KV("root", r.Root, t), render.KV("description", r.Description, t)}
	for _, m := range r.Missing {
		out = append(out, render.Warm("needs option "+m, t))
	}
	for _, prob := range r.Problems {
		out = append(out, render.Warm(prob, t))
	}
	return out
}

// Runs an operation off the event loop, reporting its outcome as a toast
func (p *Plugins) run(done string, fn func(ctx context.Context) error) tea.Cmd {
	ctx := p.env.Ctx
	return func() tea.Msg {
		return pluginDoneMsg{text: done, err: fn(ctx)}
	}
}

func (p *Plugins) current(i int) (harness.PluginRow, bool) {
	if p.available || i < 0 || i >= len(p.rows) {
		return harness.PluginRow{}, false
	}
	return p.rows[i], true
}

func (p *Plugins) toggle(l *lister, i int) tea.Cmd {
	if p.available {
		if i < 0 || i >= len(p.offers) {
			return nil
		}
		o := p.offers[i]
		if o.Installed {
			return wm.Notify(o.ID+" is already installed", wm.Info)
		}
		return p.install1(o.ID)
	}
	r, ok := p.current(i)
	if !ok {
		return nil
	}
	word := "enabled"
	if r.Enabled {
		word = "disabled"
	}
	return p.run(word+" "+r.ID, func(ctx context.Context) error { return p.env.Editor.EnablePlugin(ctx, r.ID, !r.Enabled, "") })
}

func (p *Plugins) install(l *lister, i int) tea.Cmd {
	return wm.RequestOverlay(wm.NewPrompt(p.env.Theme, "plugin, name or name@marketplace", "", func(text string) tea.Cmd {
		text = strings.TrimSpace(text)
		if text == "" {
			return nil
		}
		return p.install1(text)
	}))
}

// Installs a plugin off the event loop, reporting the id it got
func (p *Plugins) install1(spec string) tea.Cmd {
	ctx := p.env.Ctx
	return func() tea.Msg {
		id, err := p.env.Editor.InstallPlugin(ctx, spec, "", nil)
		if err != nil {
			return pluginDoneMsg{err: err}
		}
		return pluginDoneMsg{text: "installed " + id, installed: id}
	}
}

func (p *Plugins) uninstall(l *lister, i int) tea.Cmd {
	r, ok := p.current(i)
	if !ok {
		return nil
	}
	l.confirm("uninstall "+r.ID+"?", func() tea.Cmd {
		return p.run("uninstalled "+r.ID, func(ctx context.Context) error { return p.env.Editor.UninstallPlugin(ctx, r.ID, false) })
	})
	return nil
}

func (p *Plugins) update(l *lister, i int) tea.Cmd {
	r, ok := p.current(i)
	if !ok {
		return nil
	}
	return func() tea.Msg {
		changed, err := p.env.Editor.UpdatePlugin(p.env.Ctx, r.ID)
		if err != nil {
			return pluginDoneMsg{err: err}
		}
		if !changed {
			return pluginDoneMsg{text: r.ID + " is up to date"}
		}
		return pluginDoneMsg{text: "updated " + r.ID}
	}
}

func (p *Plugins) configure(l *lister, i int) tea.Cmd {
	r, ok := p.current(i)
	if !ok {
		return nil
	}
	return wm.RequestOverlay(wm.NewPrompt(p.env.Theme, "option of "+r.ID+", key=value", "", func(text string) tea.Cmd {
		key, value, found := strings.Cut(strings.TrimSpace(text), "=")
		if !found || key == "" {
			return wm.Notify("give key=value", wm.Warn)
		}
		return p.run("set "+key+" of "+r.ID, func(ctx context.Context) error {
			return p.env.Editor.ConfigurePlugin(r.ID, map[string]string{strings.TrimSpace(key): strings.TrimSpace(value)})
		})
	}))
}

func (p *Plugins) addMarket(l *lister, i int) tea.Cmd {
	return wm.RequestOverlay(wm.NewPrompt(p.env.Theme, "marketplace, owner/repo, git url, http url or path", "", func(text string) tea.Cmd {
		text = strings.TrimSpace(text)
		if text == "" {
			return nil
		}
		return func() tea.Msg {
			m, err := p.env.Editor.AddMarketplace(p.env.Ctx, text, "")
			if err != nil {
				return pluginDoneMsg{err: err}
			}
			return pluginDoneMsg{text: fmt.Sprintf("added marketplace %s with %d plugins", m.Name, len(m.Plugins))}
		}
	}))
}

func (p *Plugins) removeMarket(l *lister, i int) tea.Cmd {
	names, err := p.env.Editor.Marketplaces()
	if err != nil {
		return wm.Notify(err.Error(), wm.Error)
	}
	if len(names) == 0 {
		return wm.Notify("no marketplace is registered", wm.Info)
	}
	items := make([]wm.Item, 0, len(names))
	for _, name := range names {
		name := name
		items = append(items, wm.Item{ID: name, Title: name, Run: func() tea.Cmd {
			return p.run("removed marketplace "+name, func(context.Context) error { return p.env.Editor.RemoveMarketplace(name) })
		}})
	}
	pal := wm.NewPalette(p.env.Theme, items)
	pal.Placeholder = "remove marketplace"
	return wm.RequestOverlay(pal)
}

func (p *Plugins) open(l *lister, i int) tea.Cmd {
	r, ok := p.current(i)
	if !ok {
		return nil
	}
	return func() tea.Msg { return CommandMsg{Name: "terminal", Args: "cd " + shellQuote(r.Root) + " && ls"} }
}

// Text quoted for the shell
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (p *Plugins) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	if done, ok := msg.(pluginDoneMsg); ok {
		p.lister.refresh()
		if done.err != nil {
			return p, wm.Notify(done.err.Error(), wm.Error)
		}
		if done.installed != "" {
			id := done.installed
			return p, tea.Batch(wm.Notify(done.text, wm.Success), func() tea.Msg { return PluginInstalledMsg{ID: id} })
		}
		return p, wm.Notify(done.text, wm.Success)
	}
	_, cmd := p.lister.Update(msg)
	return p, cmd
}

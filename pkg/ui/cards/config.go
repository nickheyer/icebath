package cards

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/nickheyer/icebath/pkg/agent/hooks"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/policy/mode"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/tool"
	"github.com/nickheyer/icebath/pkg/tool/mcp"
	"github.com/nickheyer/icebath/pkg/ui/deck"
	"github.com/nickheyer/icebath/pkg/ui/render"
)

// Table card with a selection, an optional detail pane, key actions and an inline prompt
type lister struct {
	env      *Env
	title    string
	glyph    string
	id       string
	cols     []render.Column
	rows     func() [][]string
	detail   func(i int) []string
	actions  map[string]func(l *lister, i int) tea.Cmd
	hints    []key.Binding
	claims   []string
	data     [][]string
	sel      int
	expanded bool
	prompt   string
	buf      string
	onDone   func(l *lister, text string) tea.Cmd
	note     string
	w, h     int
}

func (l *lister) Title() string        { return l.title }
func (l *lister) Glyph() string        { return l.glyph }
func (l *lister) Key() string          { return l.id }
func (l *lister) Init() tea.Cmd        { l.refresh(); return nil }
func (l *lister) Capturing() bool      { return l.prompt != "" }
func (l *lister) Hints() []key.Binding { return l.hints }
func (l *lister) Claims() []string     { return append([]string{"j", "k", "g", "G", "enter"}, l.claims...) }
func (l *lister) Selectable() bool     { return true }

func (l *lister) refresh() {
	if l.rows != nil {
		l.data = l.rows()
	}
	if l.sel >= len(l.data) {
		l.sel = max(0, len(l.data)-1)
	}
}

// Opens an inline prompt whose answer is handed to done
func (l *lister) ask(prompt string, done func(l *lister, text string) tea.Cmd) {
	l.prompt, l.buf, l.onDone = prompt, "", done
}

func (l *lister) Update(msg tea.Msg) (deck.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case deck.FocusMsg:
		if msg.Focused {
			l.refresh()
		}
	case tea.KeyPressMsg:
		if l.prompt != "" {
			switch msg.String() {
			case "enter":
				text := strings.TrimSpace(l.buf)
				done := l.onDone
				l.prompt, l.buf, l.onDone = "", "", nil
				if done != nil && text != "" {
					return l, done(l, text)
				}
			case "esc":
				l.prompt, l.buf, l.onDone = "", "", nil
			case "backspace":
				if len(l.buf) > 0 {
					l.buf = l.buf[:len(l.buf)-1]
				}
			default:
				if msg.Text != "" {
					l.buf += msg.Text
				}
			}
			return l, nil
		}
		switch msg.String() {
		case "j", "down":
			l.sel = min(l.sel+1, max(0, len(l.data)-1))
		case "k", "up":
			l.sel = max(0, l.sel-1)
		case "g":
			l.sel = 0
		case "G":
			l.sel = max(0, len(l.data)-1)
		default:
			if act, ok := l.actions[msg.String()]; ok {
				return l, act(l, l.sel)
			}
		}
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft && msg.Y >= 1 && msg.Y-1 < len(l.data) {
			l.sel = msg.Y - 1
			if act, ok := l.actions["enter"]; ok {
				return l, act(l, l.sel)
			}
		}
	}
	return l, nil
}

func (l *lister) View(w, h int) string {
	l.w, l.h = w, h
	t := l.env.Theme
	var lines []string
	if l.prompt != "" {
		lines = append(lines, lipgloss.NewStyle().Foreground(t.Accent).Render(l.prompt+" ")+lipgloss.NewStyle().Foreground(t.Text).Render(l.buf)+lipgloss.NewStyle().Foreground(t.Ice).Render("▏"))
	} else if l.note != "" {
		lines = append(lines, render.Truncate(l.note, w))
	}
	if len(l.data) == 0 {
		return strings.Join(append(lines, render.Dim("nothing here", t)), "\n")
	}
	table := strings.Split(render.Table(l.cols, l.data, w, l.sel, t), "\n")
	room := h - len(lines)
	if l.expanded && l.detail != nil {
		room = max(room/2, 3)
	}
	if len(table) > room {
		start := 0
		if l.sel+1 >= room {
			start = l.sel + 2 - room
		}
		table = append(table[:1], table[1+start:min(len(table), 1+start+room-1)]...)
	}
	lines = append(lines, table...)
	if l.expanded && l.detail != nil && l.sel < len(l.data) {
		lines = append(lines, render.Dim(strings.Repeat("┄", min(w, 30)), t))
		for _, d := range l.detail(l.sel) {
			lines = append(lines, render.Truncate(d, w))
		}
	}
	if len(lines) > h {
		lines = lines[:h]
	}
	return strings.Join(lines, "\n")
}

func (l *lister) Status() string { return fmt.Sprint(len(l.data)) }

func (l *lister) Cursor() *tea.Cursor {
	if l.prompt == "" {
		return nil
	}
	return tea.NewCursor(len([]rune(l.prompt))+1+len([]rune(l.buf)), 0)
}

var (
	cUse     = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "use"))
	cDetail  = key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "detail"))
	cAdd     = key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "add"))
	cDel     = key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "delete"))
	cRefresh = key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh"))
	cEdit    = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "edit"))
	cCycle   = key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "cycle mode"))
	cReconn  = key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "reconnect"))
	cCheck   = key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "check keys"))
)

func toggleDetail(l *lister, i int) tea.Cmd {
	l.expanded = !l.expanded
	return nil
}

// Configuration surfaces behind tabs, profiles, models, knobs, rules, hooks, servers, tools and the server log
type Config struct {
	*deck.Group
	env      *Env
	profiles *lister
	models   *lister
	list     []profiles.Model
	creds    map[string]string
	err      error
}

type modelsLoadedMsg struct {
	profile string
	models  []profiles.Model
	err     error
}

// Credential texts by profile name from one background check
type credentialsMsg struct {
	texts map[string]string
}

// Credential state of a profile in words, checked through its auth kind without revealing the key
func credential(p profiles.Profile) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	state, source, err := p.Credential(ctx)
	switch {
	case err != nil:
		return "error: " + render.Truncate(err.Error(), 30)
	case state == profiles.CredentialUnneeded:
		return string(state)
	}
	return string(state) + ", " + source
}

func NewConfig(env *Env) *Config {
	c := &Config{env: env, creds: map[string]string{}}
	c.Group = deck.NewGroup("CONFIG", "⚙", c.profilesTab(), c.modelsTab(), c.knobsTab(), c.rulesTab(), c.hooksTab(), c.mcpTab(), c.toolsTab(), c.logTab())
	c.Group.SetTheme(env.Theme)
	return c
}

func (c *Config) Key() string { return "config" }

// Starts the tabs and the first credential check
func (c *Config) Init() tea.Cmd { return tea.Batch(c.Group.Init(), c.checkCredentials()) }

// Checks every profile credential off the UI thread, since key sources may run commands or reach the network
func (c *Config) checkCredentials() tea.Cmd {
	if c.env.Profiles == nil {
		return nil
	}
	ps := c.env.Profiles()
	return func() tea.Msg {
		texts := make(map[string]string, len(ps))
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, p := range ps {
			wg.Add(1)
			go func(p profiles.Profile) {
				defer wg.Done()
				text := credential(p)
				mu.Lock()
				texts[p.Name] = text
				mu.Unlock()
			}(p)
		}
		wg.Wait()
		return credentialsMsg{texts: texts}
	}
}

// Credential text last checked for the profile, or that the check is still running
func (c *Config) credentialText(name string) string {
	if text, ok := c.creds[name]; ok {
		return text
	}
	return render.Dim("checking", c.env.Theme)
}

func (c *Config) current() string {
	if c.env.Primary == nil {
		return ""
	}
	return c.env.Primary().Profile.Name
}

func (c *Config) profilesTab() *lister {
	l := &lister{env: c.env, title: "profiles", glyph: "⎔", id: "config:profiles", cols: []render.Column{{Title: ""}, {Title: "NAME"}, {Title: "VENDOR"}, {Title: "PROTOCOL"}, {Title: "ENDPOINT", Flex: 1}, {Title: "CREDENTIAL", Flex: 1}}, hints: []key.Binding{cUse, cDetail, cCheck}, claims: []string{"o", "r"}}
	c.profiles = l
	l.rows = func() [][]string {
		var rows [][]string
		if c.env.Profiles == nil {
			return rows
		}
		for _, p := range c.env.Profiles() {
			mark := " "
			if p.Name == c.current() {
				mark = render.Cool("●", c.env.Theme)
			}
			rows = append(rows, []string{mark, p.Name, p.Vendor, string(p.Protocol), p.Resolved().BaseURL, c.credentialText(p.Name)})
		}
		return rows
	}
	l.detail = func(i int) []string {
		ps := c.env.Profiles()
		if i >= len(ps) {
			return nil
		}
		p := ps[i]
		out := []string{render.KV("models", strings.Join(p.Models, ", "), c.env.Theme), render.KV("auth", string(p.Auth)+" "+p.AuthName, c.env.Theme)}
		if p.BaseURLEnv != "" {
			out = append(out, render.KV("base url env", p.BaseURLEnv, c.env.Theme))
		}
		for k, v := range p.Options {
			out = append(out, render.KV(k, v, c.env.Theme))
		}
		return out
	}
	l.actions = map[string]func(*lister, int) tea.Cmd{
		"o": toggleDetail,
		"r": func(l *lister, i int) tea.Cmd {
			c.creds = map[string]string{}
			l.refresh()
			return c.checkCredentials()
		},
		"enter": func(l *lister, i int) tea.Cmd {
			ps := c.env.Profiles()
			if i >= len(ps) {
				return nil
			}
			name := ps[i].Name
			return func() tea.Msg { return SetProfileMsg{Profile: name} }
		},
	}
	return l
}

func (c *Config) modelsTab() *lister {
	l := &lister{env: c.env, title: "models", glyph: "◇", id: "config:models", cols: []render.Column{{Title: ""}, {Title: "ID", Flex: 2}, {Title: "NAME", Flex: 1}, {Title: "CONTEXT", Right: true}, {Title: "OUT", Right: true}, {Title: "IN $/M", Right: true}, {Title: "OUT $/M", Right: true}}, hints: []key.Binding{cUse, cRefresh}, claims: []string{"r"}}
	c.models = l
	l.rows = func() [][]string {
		var rows [][]string
		cur := ""
		if c.env.Primary != nil {
			cur = c.env.Primary().Model
		}
		for _, m := range c.list {
			mark := " "
			if m.ID == cur {
				mark = render.Cool("●", c.env.Theme)
			}
			in, out := "", ""
			if m.Pricing != nil {
				in, out = fmt.Sprintf("%.2f", m.Pricing.Input), fmt.Sprintf("%.2f", m.Pricing.Output)
			}
			ctx, mo := "", ""
			if m.Context > 0 {
				ctx = render.Tokens(m.Context)
			}
			if m.MaxOutput > 0 {
				mo = render.Tokens(m.MaxOutput)
			}
			rows = append(rows, []string{mark, m.ID, m.Name, ctx, mo, in, out})
		}
		return rows
	}
	l.actions = map[string]func(*lister, int) tea.Cmd{
		"r": func(l *lister, i int) tea.Cmd { return c.fetchModels() },
		"enter": func(l *lister, i int) tea.Cmd {
			if i >= len(c.list) {
				return nil
			}
			id := c.list[i].ID
			return func() tea.Msg { return SetModelMsg{Model: id} }
		},
	}
	l.note = render.Dim("r fetches the model list from the endpoint, catalog prices checked "+profiles.CatalogUpdated(), c.env.Theme)
	return l
}

// Fetches the model list of the current profile in the background
func (c *Config) fetchModels() tea.Cmd {
	if c.env.Primary == nil || c.env.Models == nil {
		return nil
	}
	p := c.env.Primary().Profile
	fetch := c.env.Models
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		list, err := fetch(ctx, p)
		return modelsLoadedMsg{profile: p.Name, models: profiles.Enrich(list), err: err}
	}
}

func (c *Config) knobsTab() *lister {
	l := &lister{env: c.env, title: "knobs", glyph: "⚌", id: "config:knobs", cols: []render.Column{{Title: "KNOB"}, {Title: "VALUE", Flex: 1}}, hints: []key.Binding{cEdit, cCycle}, claims: []string{"m"}}
	names := []string{"mode", "max_tokens", "max_rounds", "temperature", "compact_threshold", "theme", "motion"}
	l.rows = func() [][]string {
		values := map[string]string{}
		if c.env.Knobs != nil {
			values = c.env.Knobs()
		}
		if c.env.Mode != nil {
			values["mode"] = string(c.env.Mode.Mode())
		}
		var rows [][]string
		for _, n := range names {
			rows = append(rows, []string{n, values[n]})
		}
		return rows
	}
	l.actions = map[string]func(*lister, int) tea.Cmd{
		"m": func(l *lister, i int) tea.Cmd {
			if c.env.Mode == nil {
				return nil
			}
			next := c.env.Mode.Mode().Next()
			return func() tea.Msg { return SetModeMsg{Mode: next} }
		},
		"enter": func(l *lister, i int) tea.Cmd {
			if i >= len(names) {
				return nil
			}
			name := names[i]
			if name == "mode" {
				l.ask("mode (default, acceptEdits, plan, bypassPermissions):", func(l *lister, text string) tea.Cmd {
					m, err := mode.Parse(text)
					if err != nil {
						return deck.Notify(err.Error(), deck.Error)
					}
					return func() tea.Msg { return SetModeMsg{Mode: m} }
				})
				return nil
			}
			l.ask(name+":", func(l *lister, text string) tea.Cmd {
				return func() tea.Msg { return KnobMsg{Key: name, Value: text} }
			})
			return nil
		},
	}
	return l
}

func (c *Config) rulesTab() *lister {
	l := &lister{env: c.env, title: "rules", glyph: "⚖", id: "config:rules", cols: []render.Column{{Title: "DECISION"}, {Title: "RULE", Flex: 1}}, hints: []key.Binding{cAdd, cDel}, claims: []string{"a", "d"}}
	type row struct {
		decision policy.Decision
		rule     string
	}
	var current []row
	l.rows = func() [][]string {
		current = current[:0]
		var rows [][]string
		if c.env.Rules == nil {
			return rows
		}
		for _, d := range []policy.Decision{policy.Deny, policy.Ask, policy.Allow} {
			for _, r := range c.env.Rules.Rules(d) {
				current = append(current, row{d, r})
				label := string(d)
				switch d {
				case policy.Deny:
					label = render.Bad(label, c.env.Theme)
				case policy.Ask:
					label = render.Warm(label, c.env.Theme)
				default:
					label = render.Good(label, c.env.Theme)
				}
				rows = append(rows, []string{label, r})
			}
		}
		return rows
	}
	l.actions = map[string]func(*lister, int) tea.Cmd{
		"a": func(l *lister, i int) tea.Cmd {
			l.ask("add (allow|deny|ask) Tool(pattern):", func(l *lister, text string) tea.Cmd {
				decision, rule, ok := strings.Cut(text, " ")
				if !ok {
					return deck.Notify("write a decision then a rule, such as allow Bash(git *)", deck.Warn)
				}
				d := policy.Decision(strings.ToLower(strings.TrimSpace(decision)))
				if d != policy.Allow && d != policy.Deny && d != policy.Ask {
					return deck.Notify("decision must be allow, deny or ask", deck.Warn)
				}
				if c.env.Rules == nil {
					return nil
				}
				if err := c.env.Rules.Add(d, strings.TrimSpace(rule)); err != nil {
					return deck.Notify(err.Error(), deck.Error)
				}
				l.refresh()
				set := c.env.Rules.Set()
				return func() tea.Msg { return RulesMsg{Set: set} }
			})
			return nil
		},
		"d": func(l *lister, i int) tea.Cmd {
			if c.env.Rules == nil || i >= len(current) {
				return nil
			}
			r := current[i]
			if !c.env.Rules.Remove(r.decision, r.rule) {
				return deck.Notify("rule not found", deck.Warn)
			}
			l.refresh()
			set := c.env.Rules.Set()
			return func() tea.Msg { return RulesMsg{Set: set} }
		},
	}
	return l
}

func (c *Config) hooksTab() *lister {
	l := &lister{env: c.env, title: "hooks", glyph: "⚙", id: "config:hooks", cols: []render.Column{{Title: "EVENT"}, {Title: "MATCHER"}, {Title: "COMMAND", Flex: 1}, {Title: "TIMEOUT", Right: true}}}
	l.rows = func() [][]string {
		var rows [][]string
		if c.env.Hooks == nil {
			return rows
		}
		cfg := c.env.Hooks()
		for _, ev := range hooks.Events {
			for _, g := range cfg[ev] {
				for _, h := range g.Hooks {
					timeout := ""
					if h.Timeout > 0 {
						timeout = fmt.Sprintf("%ds", h.Timeout)
					}
					rows = append(rows, []string{string(ev), g.Matcher, h.Command, timeout})
				}
			}
		}
		return rows
	}
	return l
}

func (c *Config) mcpTab() *lister {
	l := &lister{env: c.env, title: "mcp", glyph: "⇄", id: "config:mcp", cols: []render.Column{{Title: ""}, {Title: "SERVER"}, {Title: "STATE"}, {Title: "TOOLS", Right: true}, {Title: "TRIES", Right: true}, {Title: "INFO", Flex: 1}}, hints: []key.Binding{cReconn, cDetail}, claims: []string{"R", "o"}}
	l.rows = func() [][]string {
		var rows [][]string
		if c.env.MCP == nil {
			return rows
		}
		for _, s := range c.env.MCP.Status() {
			glyph := render.Dim("●", c.env.Theme)
			switch s.State {
			case mcp.StateConnected:
				glyph = render.Good("●", c.env.Theme)
			case mcp.StateConnecting:
				glyph = render.Warm("●", c.env.Theme)
			case mcp.StateDisconnected:
				glyph = render.Bad("●", c.env.Theme)
			}
			info := s.Info.Name
			if s.Info.Version != "" {
				info += " " + s.Info.Version
			}
			if s.Err != nil {
				info = render.Bad(render.Truncate(s.Err.Error(), 40), c.env.Theme)
			}
			rows = append(rows, []string{glyph, s.Name, string(s.State), fmt.Sprint(len(s.Tools)), fmt.Sprint(s.Attempts), info})
		}
		return rows
	}
	l.detail = func(i int) []string {
		st := c.env.MCP.Status()
		if i >= len(st) {
			return nil
		}
		s := st[i]
		out := []string{render.KV("transport", s.Config.Command+" "+strings.Join(s.Config.Args, " ")+s.Config.URL, c.env.Theme)}
		if s.Info.Instructions != "" {
			out = append(out, strings.Split(render.Wrap(s.Info.Instructions, l.w), "\n")...)
		}
		for _, tl := range s.Tools {
			out = append(out, "  "+tl)
		}
		return out
	}
	l.actions = map[string]func(*lister, int) tea.Cmd{
		"o": toggleDetail,
		"R": func(l *lister, i int) tea.Cmd {
			st := c.env.MCP.Status()
			if i >= len(st) {
				return nil
			}
			name := st[i].Name
			return func() tea.Msg { return ReconnectMsg{Server: name} }
		},
	}
	return l
}

func (c *Config) toolsTab() *lister {
	l := &lister{env: c.env, title: "tools", glyph: "▸", id: "config:tools", cols: []render.Column{{Title: "TOOL"}, {Title: "HINTS"}, {Title: "DESCRIPTION", Flex: 1}}, hints: []key.Binding{cDetail}, claims: []string{"o"}}
	var defs []tool.Tool
	l.rows = func() [][]string {
		defs = defs[:0]
		var rows [][]string
		if c.env.Tools == nil {
			return rows
		}
		names := c.env.Tools.Names()
		sort.Strings(names)
		for _, n := range names {
			t, ok := c.env.Tools.Lookup(n)
			if !ok {
				continue
			}
			defs = append(defs, t)
			a := tool.Describe(t)
			var hs []string
			if a.ReadOnly {
				hs = append(hs, "ro")
			}
			if a.Destructive {
				hs = append(hs, "destructive")
			}
			if a.Idempotent {
				hs = append(hs, "idem")
			}
			if a.OpenWorld {
				hs = append(hs, "open")
			}
			rows = append(rows, []string{render.ToolGlyph(n) + " " + n, strings.Join(hs, ","), render.Summary(t.Definition().Description, 80)})
		}
		return rows
	}
	l.detail = func(i int) []string {
		if i >= len(defs) {
			return nil
		}
		d := defs[i].Definition()
		out := strings.Split(render.Wrap(d.Description, max(l.w, 20)), "\n")
		out = append(out, strings.Split(render.Code(prettyArgs(d.Parameters), "json", "", c.env.Theme), "\n")...)
		return out
	}
	l.actions = map[string]func(*lister, int) tea.Cmd{"o": toggleDetail}
	return l
}

func (c *Config) logTab() *lister {
	l := &lister{env: c.env, title: "log", glyph: "≣", id: "config:log", cols: []render.Column{{Title: "TIME"}, {Title: "SERVER"}, {Title: "LEVEL"}, {Title: "MESSAGE", Flex: 1}}}
	l.rows = func() [][]string {
		var rows [][]string
		if c.env.MCP == nil {
			return rows
		}
		for _, line := range c.env.MCP.Log() {
			rows = append(rows, []string{line.Time.Local().Format("15:04:05"), line.Server, line.Level, line.Text})
		}
		return rows
	}
	return l
}

func (c *Config) Update(msg tea.Msg) (deck.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case credentialsMsg:
		c.creds = msg.texts
		c.profiles.refresh()
		return c, nil
	case deck.FocusMsg:
		_, cmd := c.Group.Update(msg)
		if msg.Focused {
			return c, tea.Batch(cmd, c.checkCredentials())
		}
		return c, cmd
	case modelsLoadedMsg:
		c.err = msg.err
		if msg.err != nil {
			c.models.note = render.Bad(render.Truncate(msg.err.Error(), 80), c.env.Theme)
		} else {
			c.list = msg.models
			c.models.note = render.Dim(fmt.Sprintf("%d models from %s, catalog prices checked %s", len(msg.models), msg.profile, profiles.CatalogUpdated()), c.env.Theme)
		}
		c.models.refresh()
		return c, nil
	case harness.MCPMsg, harness.MCPLogMsg, SetModelMsg, SetModeMsg, SetProfileMsg, KnobMsg, RulesMsg:
		for _, card := range c.Group.Cards() {
			if l, ok := card.(*lister); ok {
				l.refresh()
			}
		}
		if _, ok := msg.(SetProfileMsg); ok {
			c.list = nil
			c.models.refresh()
		}
	}
	_, cmd := c.Group.Update(msg)
	return c, cmd
}

// Models last fetched
func (c *Config) ModelList() []profiles.Model { return c.list }

// Begins fetching models, for the app to call once the deck shows the card
func (c *Config) FetchModels() tea.Cmd { return c.fetchModels() }

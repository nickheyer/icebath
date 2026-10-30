package cards

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/shell"
	"github.com/nickheyer/icebath/pkg/tool"
	"github.com/nickheyer/icebath/pkg/tool/mcp"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Configuration surfaces behind tabs, profiles, models, app settings, rules, hooks, servers, tools and the keybinds
type Config struct {
	*wm.Tabs
	env      *Env
	profiles *lister
	models   *lister
	app      *settings
	keys     *lister
	list     []profiles.Model
	loaded   string
	loading  string
	modelErr error
	creds    map[string]string
	ruleList []harness.RuleEntry
	hookList []harness.HookEntry
	scopes   map[string]string
}

type modelsLoadedMsg struct {
	profile string
	models  []profiles.Model
	err     error
}

// Credential texts by profile from one background check
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
		return "error, " + err.Error()
	case state == profiles.CredentialUnneeded:
		return string(state)
	}
	return string(state) + ", " + source
}

func NewConfig(env *Env) *Config {
	c := &Config{env: env, creds: map[string]string{}, scopes: map[string]string{}}
	c.app = newSettings(env)
	c.Tabs = wm.NewTabs(env.Theme, "CONFIG", env.Theme.Glyphs.Config, c.profilesTab(), c.modelsTab(), c.app, c.rulesTab(), c.hooksTab(), c.mcpTab(), c.toolsTab(), c.keysTab(), c.extTab())
	return c
}

func (c *Config) Key() string { return "config" }

// Starts the tabs and the first credential check
func (c *Config) Init() tea.Cmd { return tea.Batch(c.Tabs.Init(), c.checkCredentials()) }

// Shows the keybind tab
func (c *Config) ShowKeys() tea.Cmd {
	for i, card := range c.Tabs.Cards() {
		if card == wm.Card(c.keys) {
			return c.Tabs.Select(i)
		}
	}
	return nil
}

// Profiles the profile files hold, none without an editor
func (c *Config) instances() []harness.Instance {
	if c.env.Editor == nil {
		return nil
	}
	return c.env.Editor.Instances()
}

// Name of the profile answering now
func (c *Config) current() string {
	if c.env.Primary == nil {
		return ""
	}
	return c.env.Primary().Profile.Name
}

// Toast for an editor outcome, the success text or the error
func outcome(err error, done string) tea.Cmd {
	if err != nil {
		return wm.Notify(err.Error(), wm.Error)
	}
	return wm.Notify(done, wm.Success)
}

// Work on servers off the UI, since closing one waits for it to exit, reporting a failure as a toast
func background(work func() error) tea.Cmd {
	return func() tea.Msg {
		if err := work(); err != nil {
			return wm.Notify(err.Error(), wm.Error)()
		}
		return nil
	}
}

// Checks every profile credential off the UI thread, since key sources may run commands or reach the network
func (c *Config) checkCredentials() tea.Cmd {
	var list []profiles.Profile
	for _, in := range c.instances() {
		list = append(list, in.Profile)
	}
	return func() tea.Msg {
		texts := make(map[string]string, len(list))
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, p := range list {
			wg.Add(1)
			go func() {
				defer wg.Done()
				text := credential(p)
				mu.Lock()
				texts[p.Name] = text
				mu.Unlock()
			}()
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

// Marker of the row in use
func (c *Config) inUse(on bool) string {
	if !on {
		return " "
	}
	return render.Cool(c.env.Theme.Glyphs.Live, c.env.Theme)
}

// Names of the headers, the values left out since they may carry credentials
func headerNames(h map[string][]string) string {
	return strings.Join(slices.Sorted(maps.Keys(h)), ", ")
}

// Headers and body fields a directive sends, nothing while the profile names none
func heard(d *provider.Directive) string {
	if d == nil {
		return "nothing"
	}
	names := append(slices.Sorted(maps.Keys(d.Header)), slices.Sorted(maps.Keys(d.Extra))...)
	if len(names) == 0 {
		return "nothing"
	}
	return strings.Join(names, ", ")
}

// Detail lines of the endpoint a profile resolves to
func (c *Config) endpointDetail(p profiles.Profile) []string {
	t := c.env.Theme
	out := []string{
		render.KV("vendor", p.Vendor, t),
		render.KV("protocol", string(p.Protocol), t),
		render.KV("endpoint", p.Resolved().BaseURL, t),
		render.KV("auth", strings.TrimSpace(string(p.Auth)+" "+p.AuthName), t),
		render.KV("key", p.Key.Describe(), t),
		render.KV("models", strings.Join(p.Models, ", "), t),
	}
	if p.BaseURLEnv != "" {
		out = append(out, render.KV("endpoint from", p.BaseURLEnv, t))
	}
	if len(p.Header) > 0 {
		out = append(out, render.KV("headers", headerNames(p.Header), t))
	}
	if len(p.Query) > 0 {
		out = append(out, render.KV("query", headerNames(p.Query), t))
	}
	for _, k := range slices.Sorted(maps.Keys(p.Options)) {
		out = append(out, render.KV(k, p.Options[k], t))
	}
	if p.System.Kind != "" {
		system := p.System.Describe()
		if p.System.Kind == profiles.SourceStatic {
			system = render.Summary(p.System.Value, 60, t)
		}
		out = append(out, render.KV("system", system, t))
	}
	if p.NoContext {
		out = append(out, render.KV("context", "left out", t))
	}
	if p.Bare {
		out = append(out, render.KV("bare", "sends "+heard(p.BareDirective), t))
	}
	if p.Ghost {
		out = append(out, render.KV("ghost", "off disk, sends "+heard(p.GhostDirective), t))
	}
	if !p.ShowsThinking() {
		out = append(out, render.KV("thinking", "kept out of the window", t))
	}
	return out
}

// Written profiles with their provider, model, endpoint, credential and switch, each kept in the file that holds it
func (c *Config) profilesTab() *lister {
	t := c.env.Theme
	l := &lister{env: c.env, title: "profiles", glyph: c.env.Theme.Glyphs.Profiles, id: "config:profiles", ctx: keys.Profiles, primary: "config.profile.use", cols: []render.Column{{Title: ""}, {Title: "NAME"}, {Title: "PROVIDER"}, {Title: "MODEL"}, {Title: "ENDPOINT", Flex: 1}, {Title: "CREDENTIAL", Flex: 1}, {Title: "STATE"}}}
	c.profiles = l
	var list []harness.Instance
	l.rows = func() [][]string {
		list = c.instances()
		var rows [][]string
		for _, in := range list {
			p := in.Profile
			provider := in.Written.Template
			if provider == "" {
				provider = "custom"
			}
			state := render.Good("on", t)
			name := p.Name
			if p.Disabled {
				state, name = render.Dim("off", t), render.Dim(p.Name, t)
			}
			rows = append(rows, []string{c.inUse(p.Name == c.current()), name, provider, p.Model(), p.Resolved().BaseURL, c.credentialText(p.Name), state})
		}
		return rows
	}
	pick := func(i int) (harness.Instance, bool) {
		if i < 0 || i >= len(list) {
			return harness.Instance{}, false
		}
		return list[i], true
	}
	l.detail = func(i int) []string {
		in, ok := pick(i)
		if !ok {
			return nil
		}
		return append([]string{render.KV("file", homed(in.Path), t), render.KV("credential", c.credentialText(in.Profile.Name), t)}, c.endpointDetail(in.Profile)...)
	}
	l.empty = func() string {
		return "no profiles " + c.env.Theme.Glyphs.Sep + " " + c.env.keyed("config.profile.add", "new")
	}
	l.status = func() string {
		path, err := profiles.UserPath()
		if err != nil {
			return ""
		}
		return homed(path)
	}
	move := func(delta int) func(*lister, int) tea.Cmd {
		return func(l *lister, i int) tea.Cmd {
			in, ok := pick(i)
			if !ok {
				return nil
			}
			if err := c.env.Editor.MoveProfile(in.Profile.Name, delta); err != nil {
				return wm.Notify(err.Error(), wm.Error)
			}
			l.refresh()
			for j, idx := range l.shown {
				if idx < len(list) && list[idx].Profile.Name == in.Profile.Name {
					l.sel = j
				}
			}
			return nil
		}
	}
	l.free = map[string]bool{"config.profile.add": true}
	l.actions = map[string]func(*lister, int) tea.Cmd{
		"config.profile.use": func(l *lister, i int) tea.Cmd {
			in, ok := pick(i)
			if !ok {
				return nil
			}
			if in.Profile.Disabled {
				return wm.Notify(in.Profile.Name+" is off", wm.Warn)
			}
			name := in.Profile.Name
			return func() tea.Msg { return SetProfileMsg{Profile: name} }
		},
		"config.profile.add": func(l *lister, i int) tea.Cmd { return wm.RequestOverlay(c.profileForm(nil, "")) },
		"config.profile.edit": func(l *lister, i int) tea.Cmd {
			if in, ok := pick(i); ok {
				return wm.RequestOverlay(c.profileForm(&in, ""))
			}
			return nil
		},
		"config.profile.delete": func(l *lister, i int) tea.Cmd {
			in, ok := pick(i)
			if !ok {
				return nil
			}
			name := in.Profile.Name
			l.confirm("delete profile "+name+"?", func() tea.Cmd {
				err := c.env.Editor.RemoveProfile(name)
				l.refresh()
				return outcome(err, "deleted profile "+name)
			})
			return nil
		},
		"config.profile.toggle": func(l *lister, i int) tea.Cmd {
			in, ok := pick(i)
			if !ok {
				return nil
			}
			on := in.Profile.Disabled
			err := c.env.Editor.SetProfileEnabled(in.Profile.Name, on)
			l.refresh()
			word := "off"
			if on {
				word = "on"
			}
			return outcome(err, in.Profile.Name+" turned "+word)
		},
		"config.profile.up":     move(-1),
		"config.profile.down":   move(1),
		"config.profile.detail": toggleDetail,
		"config.profile.recheck": func(l *lister, i int) tea.Cmd {
			c.creds = map[string]string{}
			l.refresh()
			return c.checkCredentials()
		},
	}
	return l
}

// Models the endpoint in use serves with limits and prices, fetched when the tab shows a list of another profile
func (c *Config) modelsTab() *lister {
	t := c.env.Theme
	l := &lister{env: c.env, title: "models", glyph: c.env.Theme.Glyphs.Models, id: "config:models", ctx: keys.Models, primary: "config.model.use", cols: []render.Column{{Title: ""}, {Title: "ID", Flex: 2}, {Title: "NAME", Flex: 1}, {Title: "CONTEXT", Right: true}, {Title: "OUT", Right: true}, {Title: "IN $/M", Right: true}, {Title: "OUT $/M", Right: true}}}
	c.models = l
	l.rows = func() [][]string {
		var rows [][]string
		cur := ""
		if c.env.Primary != nil {
			cur = c.env.Primary().Model
		}
		for _, m := range c.list {
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
			rows = append(rows, []string{c.inUse(m.ID == cur), m.ID, m.Name, ctx, mo, in, out})
		}
		return rows
	}
	l.detail = func(i int) []string {
		if i < 0 || i >= len(c.list) {
			return nil
		}
		return c.modelDetail(c.list[i])
	}
	l.empty = func() string {
		switch {
		case c.loading != "":
			return "loading models"
		case c.modelErr != nil:
			return render.Bad(c.modelErr.Error(), t)
		}
		return "no models"
	}
	l.onFocus = func(l *lister) tea.Cmd {
		if c.current() != c.loaded && c.current() != c.loading {
			return c.fetchModels()
		}
		return nil
	}
	l.free = map[string]bool{"config.model.refresh": true}
	l.actions = map[string]func(*lister, int) tea.Cmd{
		"config.model.refresh": func(l *lister, i int) tea.Cmd { return c.fetchModels() },
		"config.model.detail":  toggleDetail,
		"config.model.use": func(l *lister, i int) tea.Cmd {
			if i < 0 || i >= len(c.list) {
				return nil
			}
			id := c.list[i].ID
			return func() tea.Msg { return SetModelMsg{Model: id} }
		},
	}
	return l
}

// Every fact the catalog and the endpoint know of a model
func (c *Config) modelDetail(m profiles.Model) []string {
	t := c.env.Theme
	known := func(n int) string {
		if n <= 0 {
			return "-"
		}
		return render.Tokens(n)
	}
	out := []string{render.KV("id", m.ID, t), render.KV("name", m.Name, t), render.KV("vendor", m.Vendor, t), render.KV("context", known(m.Context), t), render.KV("max output", known(m.MaxOutput), t)}
	if m.Created > 0 {
		out = append(out, render.KV("created", time.Unix(m.Created, 0).UTC().Format("2006-01-02"), t))
	}
	if p := m.Pricing; p != nil {
		out = append(out, render.KV("dollars per million", fmt.Sprintf("input %.2f, output %.2f, cache read %.2f, cache write %.2f", p.Input, p.Output, p.CacheRead, p.CacheWrite), t))
	}
	if cp := m.Capabilities; cp != nil {
		yes := func(b bool) string {
			if b {
				return "yes"
			}
			return "no"
		}
		out = append(out, render.KV("takes", fmt.Sprintf("tools %s, images %s, thinking %s, documents %s", yes(cp.Tools), yes(cp.Vision), yes(cp.Thinking), yes(cp.Documents)), t))
	}
	return out
}

// Fetches the model list of the current profile in the background
func (c *Config) fetchModels() tea.Cmd {
	if c.env.Primary == nil || c.env.Models == nil {
		return nil
	}
	p := c.env.Primary().Profile
	if c.loaded != p.Name {
		c.list, c.loaded = nil, ""
	}
	c.loading, c.modelErr = p.Name, nil
	c.models.refresh()
	fetch := c.env.Models
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		list, err := fetch(ctx, p)
		return modelsLoadedMsg{profile: p.Name, models: profiles.Enrich(list), err: err}
	}
}

// Reads the rules files again, reporting a file that does not parse
func (c *Config) loadRules() tea.Cmd {
	if c.env.Editor == nil {
		return nil
	}
	list, err := c.env.Editor.RuleEntries()
	if err != nil {
		return wm.Notify(err.Error(), wm.Error)
	}
	c.ruleList = list
	return nil
}

// Permission rules of every file with the scope each lives in, edited through a dialog
func (c *Config) rulesTab() *lister {
	t := c.env.Theme
	l := &lister{env: c.env, title: "rules", glyph: c.env.Theme.Glyphs.Rules, id: "config:rules", ctx: keys.Rules, primary: "config.rule.edit", cols: []render.Column{{Title: "DECISION"}, {Title: "TOOL"}, {Title: "PATTERN", Flex: 1}, {Title: "SCOPE"}}}
	l.rows = func() [][]string {
		var rows [][]string
		for _, e := range c.ruleList {
			label := string(e.Decision)
			switch e.Decision {
			case "deny":
				label = render.Bad(label, t)
			case "ask":
				label = render.Warm(label, t)
			default:
				label = render.Good(label, t)
			}
			tl, pattern := splitRule(e.Rule)
			if pattern == "" {
				pattern = render.Dim("every call", t)
			}
			rows = append(rows, []string{label, tl, pattern, c.env.Editor.ScopeOf(e.Path)})
		}
		return rows
	}
	pick := func(i int) (harness.RuleEntry, bool) {
		if i < 0 || i >= len(c.ruleList) {
			return harness.RuleEntry{}, false
		}
		return c.ruleList[i], true
	}
	l.empty = func() string {
		return "no rules " + c.env.Theme.Glyphs.Sep + " " + c.env.keyed("config.rule.add", "new")
	}
	l.onFocus = func(l *lister) tea.Cmd { return c.loadRules() }
	l.free = map[string]bool{"config.rule.add": true}
	l.actions = map[string]func(*lister, int) tea.Cmd{
		"config.rule.add": func(l *lister, i int) tea.Cmd { return wm.RequestOverlay(c.ruleForm(nil)) },
		"config.rule.edit": func(l *lister, i int) tea.Cmd {
			if e, ok := pick(i); ok {
				return wm.RequestOverlay(c.ruleForm(&e))
			}
			return nil
		},
		"config.rule.delete": func(l *lister, i int) tea.Cmd {
			e, ok := pick(i)
			if !ok {
				return nil
			}
			l.confirm("delete "+string(e.Decision)+" "+e.Rule+"?", func() tea.Cmd {
				err := c.env.Editor.RemoveRule(e)
				return tea.Batch(c.loadRules(), outcome(err, "deleted "+e.Rule))
			})
			return nil
		},
	}
	return l
}

// Reads the hook files again, reporting a file that does not parse
func (c *Config) loadHooks() tea.Cmd {
	if c.env.Editor == nil {
		return nil
	}
	list, err := c.env.Editor.HookEntries()
	if err != nil {
		return wm.Notify(err.Error(), wm.Error)
	}
	c.hookList = list
	return nil
}

// Hook commands of every file with their event, matcher, timeout and scope, edited through a dialog
func (c *Config) hooksTab() *lister {
	t := c.env.Theme
	l := &lister{env: c.env, title: "hooks", glyph: c.env.Theme.Glyphs.Hooks, id: "config:hooks", ctx: keys.Hooks, primary: "config.hook.edit", cols: []render.Column{{Title: "EVENT"}, {Title: "MATCHER"}, {Title: "COMMAND", Flex: 1}, {Title: "TIMEOUT", Right: true}, {Title: "SCOPE"}}}
	l.rows = func() [][]string {
		var rows [][]string
		for _, e := range c.hookList {
			matcher := e.Matcher
			if matcher == "" {
				matcher = render.Dim("every tool", t)
			}
			timeout := render.Dim("-", t)
			if e.Timeout > 0 {
				timeout = fmt.Sprintf("%ds", e.Timeout)
			}
			rows = append(rows, []string{string(e.Event), matcher, e.Command, timeout, c.env.Editor.ScopeOf(e.Path)})
		}
		return rows
	}
	pick := func(i int) (harness.HookEntry, bool) {
		if i < 0 || i >= len(c.hookList) {
			return harness.HookEntry{}, false
		}
		return c.hookList[i], true
	}
	l.empty = func() string {
		return "no hooks " + c.env.Theme.Glyphs.Sep + " " + c.env.keyed("config.hook.add", "new")
	}
	l.onFocus = func(l *lister) tea.Cmd { return c.loadHooks() }
	l.free = map[string]bool{"config.hook.add": true}
	l.actions = map[string]func(*lister, int) tea.Cmd{
		"config.hook.add": func(l *lister, i int) tea.Cmd { return wm.RequestOverlay(c.hookForm(nil)) },
		"config.hook.edit": func(l *lister, i int) tea.Cmd {
			if e, ok := pick(i); ok {
				return wm.RequestOverlay(c.hookForm(&e))
			}
			return nil
		},
		"config.hook.delete": func(l *lister, i int) tea.Cmd {
			e, ok := pick(i)
			if !ok {
				return nil
			}
			l.confirm("delete the "+string(e.Event)+" hook "+render.Truncate(e.Command, 40, t)+"?", func() tea.Cmd {
				err := c.env.Editor.RemoveHook(e)
				return tea.Batch(c.loadHooks(), outcome(err, "deleted the "+string(e.Event)+" hook"))
			})
			return nil
		},
	}
	return l
}

// Reads which file configures each server, reporting a file that does not parse
func (c *Config) loadServers() tea.Cmd {
	if c.env.Editor == nil {
		return nil
	}
	list, err := c.env.Editor.ServerEntries()
	if err != nil {
		return wm.Notify(err.Error(), wm.Error)
	}
	c.scopes = map[string]string{}
	for _, e := range list {
		c.scopes[e.Config.Name] = c.env.Editor.ScopeOf(e.Path)
	}
	return nil
}

// Servers with their transport, connection state, tools and scope, added, edited, switched and reconnected in place
func (c *Config) mcpTab() *lister {
	t := c.env.Theme
	l := &lister{env: c.env, title: "mcp", glyph: c.env.Theme.Glyphs.MCP, id: "config:mcp", ctx: keys.MCP, primary: "config.mcp.detail", cols: []render.Column{{Title: ""}, {Title: "SERVER"}, {Title: "TRANSPORT"}, {Title: "TARGET", Flex: 1}, {Title: "STATE"}, {Title: "TOOLS", Right: true}, {Title: "SCOPE"}}}
	var list []harness.ServerStatus
	l.rows = func() [][]string {
		list = nil
		if c.env.MCP != nil {
			list = c.env.MCP.Status()
		}
		var rows [][]string
		for _, s := range list {
			glyph, state := render.Dim(t.Glyphs.Live, t), string(s.State)
			switch {
			case s.Config.Disabled:
				state = render.Dim("off", t)
			case s.State == mcp.StateConnected:
				glyph = render.Good(t.Glyphs.Live, t)
			case s.State == mcp.StateConnecting:
				glyph = render.Warm(t.Glyphs.Live, t)
			case s.State == mcp.StateDisconnected:
				glyph = render.Bad(t.Glyphs.Live, t)
			}
			transport, target := "stdio", commandLine(s.Config)
			if s.Config.URL != "" {
				transport, target = "http", s.Config.URL
			}
			rows = append(rows, []string{glyph, s.Name, transport, target, state, fmt.Sprint(len(s.Tools)), c.scopes[s.Name]})
		}
		return rows
	}
	pick := func(i int) (harness.ServerStatus, bool) {
		if i < 0 || i >= len(list) {
			return harness.ServerStatus{}, false
		}
		return list[i], true
	}
	l.detail = func(i int) []string {
		s, ok := pick(i)
		if !ok {
			return nil
		}
		return c.serverDetail(s)
	}
	l.empty = func() string {
		return "no servers " + c.env.Theme.Glyphs.Sep + " " + c.env.keyed("config.mcp.add", "new")
	}
	l.onFocus = func(l *lister) tea.Cmd { return c.loadServers() }
	l.free = map[string]bool{"config.mcp.add": true}
	l.actions = map[string]func(*lister, int) tea.Cmd{
		"config.mcp.detail": toggleDetail,
		"config.mcp.add":    func(l *lister, i int) tea.Cmd { return c.openServerForm(nil) },
		"config.mcp.edit": func(l *lister, i int) tea.Cmd {
			if s, ok := pick(i); ok {
				return c.openServerForm(&s.Config)
			}
			return nil
		},
		"config.mcp.delete": func(l *lister, i int) tea.Cmd {
			s, ok := pick(i)
			if !ok {
				return nil
			}
			l.confirm("delete mcp server "+s.Name+"?", func() tea.Cmd {
				if err := c.env.Editor.RemoveServer(s.Name); err != nil {
					return wm.Notify(err.Error(), wm.Error)
				}
				return tea.Batch(c.loadServers(), background(func() error { return c.env.Editor.CloseServer(s.Name) }), outcome(nil, "deleted mcp server "+s.Name))
			})
			return nil
		},
		"config.mcp.toggle": func(l *lister, i int) tea.Cmd {
			s, ok := pick(i)
			if !ok {
				return nil
			}
			on := s.Config.Disabled
			word := "off"
			if on {
				word = "on"
			}
			cfg, err := c.env.Editor.SetServerEnabled(s.Name, on)
			if err != nil {
				return wm.Notify(err.Error(), wm.Error)
			}
			return tea.Batch(c.loadServers(), background(func() error { return c.env.Editor.RestartServer(c.env.Ctx, s.Name, cfg) }), outcome(nil, s.Name+" turned "+word))
		},
		"config.mcp.reconnect": func(l *lister, i int) tea.Cmd {
			s, ok := pick(i)
			if !ok {
				return nil
			}
			if s.Config.Disabled {
				return wm.Notify(s.Name+" is off", wm.Warn)
			}
			name := s.Name
			return func() tea.Msg { return ReconnectMsg{Server: name} }
		},
	}
	return l
}

// Transport, settings, state, instructions and tools of a server
func (c *Config) serverDetail(s harness.ServerStatus) []string {
	t := c.env.Theme
	cfg := s.Config
	var out []string
	if cfg.URL != "" {
		out = append(out, render.KV("url", cfg.URL, t))
		if len(cfg.Headers) > 0 {
			out = append(out, render.KV("headers", strings.Join(slices.Sorted(maps.Keys(cfg.Headers)), ", "), t))
		}
	} else {
		out = append(out, render.KV("command", commandLine(cfg), t))
		if cfg.Dir != "" {
			out = append(out, render.KV("dir", cfg.Dir, t))
		}
		if len(cfg.Env) > 0 {
			out = append(out, render.KV("env", strings.Join(slices.Sorted(maps.Keys(cfg.Env)), ", "), t))
		}
	}
	if cfg.Timeout > 0 {
		out = append(out, render.KV("timeout", fmt.Sprintf("%ds", cfg.Timeout), t))
	}
	out = append(out, render.KV("state", fmt.Sprintf("%s since %s after %d tries", s.State, s.Since.Local().Format("15:04:05"), s.Attempts), t))
	if s.Info.Name != "" {
		out = append(out, render.KV("server", strings.TrimSpace(s.Info.Name+" "+s.Info.Version), t))
	}
	if s.Info.ProtocolVersion != "" {
		out = append(out, render.KV("protocol", s.Info.ProtocolVersion, t))
	}
	if s.Err != nil {
		out = append(out, render.Bad(s.Err.Error(), t))
	}
	if s.Info.Instructions != "" {
		out = append(out, strings.Split(s.Info.Instructions, "\n")...)
	}
	for _, tl := range s.Tools {
		out = append(out, "  "+tl)
	}
	return out
}

// Every tool with whether the model gets it, where it comes from, its traits and its schema
func (c *Config) toolsTab() *lister {
	t := c.env.Theme
	l := &lister{env: c.env, title: "tools", glyph: c.env.Theme.Glyphs.Tools, id: "config:tools", ctx: keys.Tools, primary: "config.tool.toggle", cols: []render.Column{{Title: "STATE"}, {Title: "TOOL"}, {Title: "SOURCE"}, {Title: "TRAITS"}, {Title: "DESCRIPTION", Flex: 1}}}
	var defs []tool.Tool
	l.rows = func() [][]string {
		defs = defs[:0]
		var rows [][]string
		if c.env.Tools == nil {
			return rows
		}
		source := map[string]string{}
		if c.env.MCP != nil {
			for _, s := range c.env.MCP.Status() {
				for _, n := range s.Tools {
					source[n] = "mcp " + s.Name
				}
			}
		}
		names := c.env.Tools.Names()
		sort.Strings(names)
		for _, n := range names {
			tl, ok := c.env.Tools.Lookup(n)
			if !ok {
				continue
			}
			defs = append(defs, tl)
			a := tool.Describe(tl)
			var hs []string
			if a.ReadOnly {
				hs = append(hs, "read only")
			}
			if a.Destructive {
				hs = append(hs, "destructive")
			}
			if a.Idempotent {
				hs = append(hs, "idempotent")
			}
			if a.OpenWorld {
				hs = append(hs, "open world")
			}
			state := render.Good("on", t)
			if !c.env.Tools.Enabled(n) {
				state = render.Dim("off", t)
			}
			from := source[n]
			if from == "" {
				from = "built in"
			}
			rows = append(rows, []string{state, n, from, strings.Join(hs, ", "), render.Summary(tl.Definition().Description, 80, t)})
		}
		return rows
	}
	l.detail = func(i int) []string {
		if i < 0 || i >= len(defs) {
			return nil
		}
		d := defs[i].Definition()
		out := strings.Split(d.Description, "\n")
		return append(out, strings.Split(render.Code(prettyArgs(d.Parameters), "json", "", t), "\n")...)
	}
	l.actions = map[string]func(*lister, int) tea.Cmd{
		"config.tool.detail": toggleDetail,
		"config.tool.toggle": func(l *lister, i int) tea.Cmd {
			if i < 0 || i >= len(defs) || c.env.Editor == nil {
				return nil
			}
			name := defs[i].Definition().Name
			on := !c.env.Tools.Enabled(name)
			word := "off"
			if on {
				word = "on"
			}
			err := c.env.Editor.SetToolEnabled(name, on)
			l.refresh()
			return outcome(err, name+" turned "+word)
		},
	}
	return l
}

// Every action with its keys by typed name, rebound by a key press or typing and saved at once
// Tab listing the extensions linked into the binary and what each provides
func (c *Config) extTab() *lister {
	t := c.env.Theme
	l := &lister{env: c.env, title: "ext", glyph: t.Glyphs.Ext, id: "config:ext", ctx: keys.Extensions, primary: "config.ext.detail", cols: []render.Column{{Title: "EXTENSION", Max: 24}, {Title: "VERSION", Max: 12}, {Title: "PROVIDES", Max: 10}, {Title: "DESCRIPTION", Flex: 1}}}
	var list []Extension
	l.rows = func() [][]string {
		list = list[:0]
		if c.env.Extensions != nil {
			list = c.env.Extensions()
		}
		var rows [][]string
		for _, e := range list {
			rows = append(rows, []string{e.Name, e.Version, render.Count(len(e.Provides), "item"), render.Summary(e.Description, 80, t)})
		}
		return rows
	}
	l.detail = func(i int) []string {
		if i < 0 || i >= len(list) {
			return nil
		}
		var out []string
		for _, p := range list[i].Provides {
			out = append(out, render.KV(p[0]+" "+p[1], p[2], t))
		}
		return out
	}
	l.actions = map[string]func(*lister, int) tea.Cmd{"config.ext.detail": toggleDetail}
	l.empty = func() string { return "no extensions linked, add imports to cmd/icebath/extensions.go" }
	return l
}

func (c *Config) keysTab() *lister {
	km := c.env.Keys
	t := c.env.Theme
	l := &lister{env: c.env, title: "keys", glyph: c.env.Theme.Glyphs.Keys, id: "config:keys", ctx: keys.Keybinds, primary: "config.key.rebind", cols: []render.Column{{Title: ""}, {Title: "ACTION", Max: 26}, {Title: "KEYS", Max: 24}, {Title: "WHERE", Max: 16}, {Title: "DOES", Flex: 1}}}
	c.keys = l
	var ids []string
	l.rows = func() [][]string {
		ids = ids[:0]
		var rows [][]string
		for _, a := range km.Actions() {
			ids = append(ids, a.ID)
			bound := km.DisplayAll(a.ID)
			if bound == "" {
				bound = render.Dim("-", t)
			}
			mark := " "
			if !km.AtDefault(a.ID) {
				mark = render.Warm("*", t)
			}
			rows = append(rows, []string{mark, a.ID, bound, a.Context, a.Doc})
		}
		return rows
	}
	pick := func(i int) (string, bool) {
		if i < 0 || i >= len(ids) {
			return "", false
		}
		return ids[i], true
	}
	l.detail = func(i int) []string {
		id, ok := pick(i)
		if !ok {
			return nil
		}
		a, _ := km.Action(id)
		defaults := make([]string, 0, len(a.Default))
		for _, d := range a.Default {
			defaults = append(defaults, keys.Format(d))
		}
		return []string{
			render.KV("action", a.ID+", shown as "+a.Label, t),
			render.KV("where", a.Context+", "+keys.ContextDocs[a.Context], t),
			render.KV("keys", or(km.DisplayAll(id), "-"), t),
			render.KV("default", or(strings.Join(defaults, " "), "-"), t),
			render.KV("does", a.Doc, t),
		}
	}
	l.status = func() string { return homed(km.Path()) }
	l.actions = map[string]func(*lister, int) tea.Cmd{
		"config.key.rebind": func(l *lister, i int) tea.Cmd {
			if id, ok := pick(i); ok {
				l.wait("press a key for "+id, func(l *lister, chord string) tea.Cmd {
					return c.rebound(id, func() error { return km.Rebind(id, chord) })
				})
			}
			return nil
		},
		"config.key.add": func(l *lister, i int) tea.Cmd {
			if id, ok := pick(i); ok {
				l.wait("press a key to add to "+id, func(l *lister, chord string) tea.Cmd { return c.rebound(id, func() error { return km.Add(id, chord) }) })
			}
			return nil
		},
		"config.key.type": func(l *lister, i int) tea.Cmd {
			id, ok := pick(i)
			if !ok {
				return nil
			}
			l.editCell(2, km.DisplayAll(id), func(l *lister, text string) tea.Cmd {
				chords := strings.FieldsFunc(text, func(r rune) bool { return r == ' ' || r == ',' })
				return c.rebound(id, func() error { return km.Set(id, chords) })
			})
			return nil
		},
		"config.key.unbind": func(l *lister, i int) tea.Cmd {
			if id, ok := pick(i); ok {
				return c.rebound(id, func() error { return km.Unbind(id) })
			}
			return nil
		},
		"config.key.reset": func(l *lister, i int) tea.Cmd {
			if id, ok := pick(i); ok {
				return c.rebound(id, func() error { return km.Reset(id) })
			}
			return nil
		},
		"config.key.detail": toggleDetail,
	}
	return l
}

// Text, or the fallback when it is empty
func or(text, fallback string) string {
	if text == "" {
		return fallback
	}
	return text
}

// Applies a keymap change, writes the keybind file and tells every card, or reports what refused it
func (c *Config) rebound(id string, change func() error) tea.Cmd {
	km := c.env.Keys
	if err := change(); err != nil {
		return wm.Notify(err.Error(), wm.Error)
	}
	c.keys.refresh()
	changed := func() tea.Msg { return keys.ChangedMsg{} }
	if err := km.Save(); err != nil {
		return tea.Batch(changed, wm.Notify("keybinds: "+err.Error(), wm.Error))
	}
	text := id + " unbound"
	if bound := km.DisplayAll(id); bound != "" {
		text = id + " = " + bound
	}
	return tea.Batch(changed, wm.Notify(text, wm.Success))
}

func (c *Config) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case credentialsMsg:
		c.creds = msg.texts
		c.profiles.refresh()
		return c, nil
	case wm.FocusMsg:
		_, cmd := c.Tabs.Update(msg)
		if msg.Focused {
			return c, tea.Batch(cmd, c.checkCredentials())
		}
		return c, cmd
	case modelsLoadedMsg:
		if msg.profile != c.loading {
			return c, nil
		}
		c.loading = ""
		c.modelErr = msg.err
		if msg.err == nil {
			c.list, c.loaded = msg.models, msg.profile
		}
		c.models.refresh()
		return c, nil
	}
	_, cmd := c.Tabs.Update(msg)
	return c, cmd
}

// Models last fetched
func (c *Config) ModelList() []profiles.Model { return c.list }

// Begins fetching models, for the app to call once the endpoint changed
func (c *Config) FetchModels() tea.Cmd { return c.fetchModels() }

// Path with the home directory written as a tilde
func homed(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || !strings.HasPrefix(path, home+string(filepath.Separator)) {
		return path
	}
	return "~" + strings.TrimPrefix(path, home)
}

// Opens the server dialog, or reports why the stored server cannot be shown in it
func (c *Config) openServerForm(old *harness.ServerConfig) tea.Cmd {
	form, err := c.serverForm(old)
	if err != nil {
		return wm.Notify(err.Error(), wm.Error)
	}
	return wm.RequestOverlay(form)
}

// Program and arguments of a stdio server as one shell line, the quoting error standing in for arguments it cannot show
func commandLine(cfg harness.ServerConfig) string {
	args, err := shell.Join(cfg.Args)
	if err != nil {
		args = err.Error()
	}
	return strings.TrimSpace(cfg.Command + " " + args)
}

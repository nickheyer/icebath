package harness

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/plugin"
)

// One plugin as the terminal and the command line list it
type PluginRow struct {
	ID          string
	Name        string
	Title       string
	Version     string
	Description string
	Marketplace string
	From        string
	Enabled     bool
	Skills      int
	Agents      int
	Hooks       int
	Servers     int
	Root        string
	Problems    []string
	Missing     []string
}

// Every plugin known, enabled or not, in id order
func (a *App) PluginRows() []PluginRow {
	lib := a.Library()
	if lib == nil {
		return nil
	}
	var out []PluginRow
	for _, p := range lib.Plugins {
		hooks := 0
		for _, groups := range p.Hooks {
			for _, g := range groups {
				hooks += len(g.Hooks)
			}
		}
		out = append(out, PluginRow{ID: p.ID, Name: p.Name, Title: p.Title(), Version: p.Version, Description: p.Manifest.Description, Marketplace: p.Marketplace, From: p.From, Enabled: p.Enabled, Skills: len(p.Skills), Agents: len(p.Agents), Hooks: hooks, Servers: len(p.Servers), Root: p.Root, Problems: p.Problems, Missing: p.Missing})
	}
	return out
}

// Scope a plugin setting is saved under, the user or the project configuration file
func (a *App) pluginScopeDir(scope string) (string, error) {
	switch scope {
	case "", plugin.ScopeUser:
		return "", nil
	case plugin.ScopeProject, plugin.ScopeLocal:
		return a.Dir, nil
	}
	return "", fmt.Errorf("scope %q is not user, project or local", scope)
}

// Turns a plugin on or off under the scope, saving the choice and loading the plugins again
func (a *App) EnablePlugin(ctx context.Context, id string, on bool, scope string) error {
	lib := a.Library()
	if lib == nil {
		return ErrNoStore
	}
	p, ok := lib.Plugin(id)
	if !ok {
		return fmt.Errorf("plugin %s is not installed", id)
	}
	if !on && p.Enabled == false {
		return fmt.Errorf("plugin %s is already disabled", p.ID)
	}
	if on && p.Enabled {
		return fmt.Errorf("plugin %s is already enabled", p.ID)
	}
	if _, err := a.pluginScopeDir(scope); err != nil {
		return err
	}
	cfg := a.Config
	enabled := map[string]bool{}
	for k, v := range cfg.Plugins.Enabled {
		enabled[k] = v
	}
	enabled[p.ID] = on
	cfg.Plugins.Enabled = enabled
	a.Config = cfg
	sources := a.Config.Sources
	if scope == plugin.ScopeProject || scope == plugin.ScopeLocal {
		layers, err := configLayers(a.Dir)
		if err != nil {
			return err
		}
		sources = []string{layers[len(layers)-1]}
	}
	if err := Save(a.Dir, sources, Field{Path: []string{"plugins", "enabled", p.ID}, Value: on}); err != nil {
		return err
	}
	return a.ReloadPlugins(ctx)
}

// Installs a plugin from a registered marketplace, enabling it, and loads the plugins again
func (a *App) InstallPlugin(ctx context.Context, spec, scope string, options map[string]string) (string, error) {
	store := a.PluginStore()
	if store == nil {
		return "", ErrNoStore
	}
	if scope == "" {
		scope = plugin.ScopeUser
	}
	if _, err := a.pluginScopeDir(scope); err != nil {
		return "", err
	}
	store.Progress = func(text string) { a.send(NoticeMsg{Level: Info, Text: text}) }
	id, _, err := store.Install(ctx, spec, scope)
	if err != nil {
		return "", err
	}
	store.EnsureData(id)
	if len(options) > 0 {
		if err := a.ConfigurePlugin(id, options); err != nil {
			return id, err
		}
	}
	cfg := a.Config
	enabled := map[string]bool{}
	for k, v := range cfg.Plugins.Enabled {
		enabled[k] = v
	}
	enabled[id] = true
	cfg.Plugins.Enabled = enabled
	a.Config = cfg
	sources := a.Config.Sources
	if scope != plugin.ScopeUser {
		layers, err := configLayers(a.Dir)
		if err != nil {
			return id, err
		}
		sources = []string{layers[len(layers)-1]}
	}
	if err := Save(a.Dir, sources, Field{Path: []string{"plugins", "enabled", id}, Value: true}); err != nil {
		return id, err
	}
	if err := a.ReloadPlugins(ctx); err != nil {
		return id, err
	}
	if p, ok := a.Library().Plugin(id); ok && len(p.Missing) > 0 {
		return id, fmt.Errorf("plugin %s needs values for %s, set them with plugin configure", id, strings.Join(p.Missing, ", "))
	}
	return id, nil
}

// Removes an installed plugin, forgetting its enablement, and loads the plugins again
func (a *App) UninstallPlugin(ctx context.Context, id string, keepData bool) error {
	store := a.PluginStore()
	if store == nil {
		return ErrNoStore
	}
	lib := a.Library()
	if p, ok := lib.Plugin(id); ok {
		id = p.ID
		if p.From != plugin.FromMarketplace {
			return fmt.Errorf("plugin %s was not installed by icebath, it comes from %s", id, p.From)
		}
	}
	if err := store.Uninstall(id, keepData); err != nil {
		return err
	}
	cfg := a.Config
	enabled := map[string]bool{}
	for k, v := range cfg.Plugins.Enabled {
		if k != id {
			enabled[k] = v
		}
	}
	cfg.Plugins.Enabled = enabled
	a.Config = cfg
	if err := Save(a.Dir, a.Config.Sources, Field{Path: []string{"plugins", "enabled", id}, Unset: true}); err != nil {
		return err
	}
	return a.ReloadPlugins(ctx)
}

// Fetches an installed plugin again, true when its version changed, and loads the plugins again
func (a *App) UpdatePlugin(ctx context.Context, id string) (bool, error) {
	store := a.PluginStore()
	if store == nil {
		return false, ErrNoStore
	}
	if p, ok := a.Library().Plugin(id); ok {
		id = p.ID
	}
	store.Progress = func(text string) { a.send(NoticeMsg{Level: Info, Text: text}) }
	_, changed, err := store.Update(ctx, id)
	if err != nil {
		return false, err
	}
	return changed, a.ReloadPlugins(ctx)
}

// Saves option values of a plugin and loads the plugins again
func (a *App) ConfigurePlugin(id string, values map[string]string) error {
	lib := a.Library()
	var p *plugin.Plugin
	if lib != nil {
		p, _ = lib.Plugin(id)
	}
	if p != nil {
		id = p.ID
		for key, value := range values {
			opt, ok := p.Manifest.UserConfig[key]
			if !ok {
				return fmt.Errorf("plugin %s has no option %s", id, key)
			}
			if err := checkOption(key, opt, value); err != nil {
				return err
			}
		}
	}
	secrets := map[string]string{}
	plain := map[string]string{}
	for key, value := range values {
		if p != nil && p.Manifest.UserConfig[key].Sensitive {
			secrets[key] = value
			continue
		}
		plain[key] = value
	}
	if len(secrets) > 0 {
		store := a.PluginStore()
		if store == nil {
			return ErrNoStore
		}
		if err := store.SaveSecrets(id, secrets); err != nil {
			return err
		}
	}
	cfg := a.Config
	all := map[string]map[string]string{}
	for pid, vals := range cfg.Plugins.Config {
		copied := map[string]string{}
		for k, v := range vals {
			copied[k] = v
		}
		all[pid] = copied
	}
	if all[id] == nil {
		all[id] = map[string]string{}
	}
	var fields []Field
	for key, value := range plain {
		all[id][key] = value
		fields = append(fields, Field{Path: []string{"plugins", "config", id, key}, Value: value})
	}
	cfg.Plugins.Config = all
	a.Config = cfg
	if len(fields) > 0 {
		if err := Save(a.Dir, a.Config.Sources, fields...); err != nil {
			return err
		}
	}
	return a.ReloadPlugins(context.Background())
}

// Checks a value against the type, choices and bounds of an option
func checkOption(key string, opt plugin.Option, value string) error {
	if opt.Required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("option %s is required", key)
	}
	switch opt.Type {
	case "number":
		if value != "" {
			var f float64
			if _, err := fmt.Sscanf(value, "%g", &f); err != nil {
				return fmt.Errorf("option %s takes a number, got %q", key, value)
			}
			if opt.Min != nil && f < *opt.Min || opt.Max != nil && f > *opt.Max {
				return fmt.Errorf("option %s is out of its range", key)
			}
		}
	case "boolean":
		switch strings.ToLower(value) {
		case "", "true", "false", "1", "0", "yes", "no":
		default:
			return fmt.Errorf("option %s takes true or false, got %q", key, value)
		}
	}
	if len(opt.Options) > 0 && value != "" {
		found := false
		for _, c := range opt.Options {
			found = found || c == value
		}
		if !found {
			return fmt.Errorf("option %s must be one of %s", key, strings.Join(opt.Options, ", "))
		}
	}
	return nil
}

// Registers a marketplace from a source text and saves it in the user configuration
func (a *App) AddMarketplace(ctx context.Context, text, name string) (*plugin.Marketplace, error) {
	store := a.PluginStore()
	if store == nil {
		return nil, ErrNoStore
	}
	src, err := plugin.ParseMarketSource(text)
	if err != nil {
		return nil, err
	}
	store.Progress = func(text string) { a.send(NoticeMsg{Level: Info, Text: text}) }
	m, err := store.AddMarketplace(ctx, src, name)
	if err != nil {
		return nil, err
	}
	cfg := a.Config
	markets := map[string]plugin.MarketSource{}
	for k, v := range cfg.Plugins.Marketplaces {
		markets[k] = v
	}
	markets[m.Name] = src
	cfg.Plugins.Marketplaces = markets
	a.Config = cfg
	if err := Save(a.Dir, a.Config.Sources, Field{Path: []string{"plugins", "marketplaces", m.Name}, Value: src}); err != nil {
		return m, err
	}
	return m, nil
}

// Forgets a marketplace in the store and the configuration
func (a *App) RemoveMarketplace(name string) error {
	store := a.PluginStore()
	if store == nil {
		return ErrNoStore
	}
	if err := store.RemoveMarketplace(name); err != nil {
		return err
	}
	cfg := a.Config
	markets := map[string]plugin.MarketSource{}
	for k, v := range cfg.Plugins.Marketplaces {
		if k != name {
			markets[k] = v
		}
	}
	cfg.Plugins.Marketplaces = markets
	a.Config = cfg
	return Save(a.Dir, a.Config.Sources, Field{Path: []string{"plugins", "marketplaces", name}, Unset: true})
}

// One plugin a marketplace offers, with whether it is installed
type Offer struct {
	ID          string
	Name        string
	Marketplace string
	Description string
	Version     string
	Category    string
	Installed   bool
}

// Every plugin the registered marketplaces offer, in id order
func (a *App) Offers() ([]Offer, error) {
	store := a.PluginStore()
	if store == nil {
		return nil, ErrNoStore
	}
	markets, errs, err := store.Marketplaces()
	if err != nil {
		return nil, err
	}
	installed, err := store.Installed()
	if err != nil {
		return nil, err
	}
	var out []Offer
	for _, m := range markets {
		for _, e := range m.Plugins {
			id := e.Name + "@" + m.Name
			_, have := installed[id]
			out = append(out, Offer{ID: id, Name: e.Name, Marketplace: m.Name, Description: e.Description, Version: e.Version, Category: e.Category, Installed: have})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	var problems []error
	for name, err := range errs {
		problems = append(problems, fmt.Errorf("marketplace %s: %w", name, err))
	}
	return out, errors.Join(problems...)
}

// Every marketplace registered, in name order
func (a *App) Marketplaces() ([]string, error) {
	store := a.PluginStore()
	if store == nil {
		return nil, ErrNoStore
	}
	known, err := store.Known()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(known))
	for name := range known {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// Sets the output style of the configuration, none for an empty name, and saves it
func (a *App) SetOutputStyle(name string) error {
	if name != "" {
		lib := a.Library()
		if lib == nil {
			return errors.New("no styles are loaded")
		}
		if _, ok := lib.Style(name); !ok {
			return fmt.Errorf("output style %q is not known, the styles are %s", name, strings.Join(a.StyleNames(), ", "))
		}
	}
	cfg := a.Config
	cfg.OutputStyle = name
	a.Config = cfg
	return cfg.SaveSetting(a.Dir, "output_style", name)
}

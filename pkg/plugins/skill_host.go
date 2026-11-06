package plugins

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/skills"
)

func init() {
	skills.RegisterPluginSkillHost(newSkillHost)
}

// Plugin skill surface pkg/skills consults, answered by the plugin manager of one home
type skillHost struct {
	ctx context.Context
	h   string
}

// Skill host whose lookups resolve the plugin manager and memory provider of the given home
func newSkillHost(ctx context.Context, h string) skills.PluginSkillHost {
	if h != "" {
		ctx = home.WithOverride(ctx, h)
	}
	return &skillHost{ctx: ctx, h: home.Dir(ctx)}
}

// Plugin manager of the bound home, looked up per call like get_plugin_manager
func (s *skillHost) manager() *Manager { return GetPluginManager(s.ctx) }

// Discovers and loads the plugins of the bound home, a no op once discovered
func (s *skillHost) Discover(ctx context.Context) {
	DiscoverPlugins(home.WithOverride(ctx, s.h), false)
}

func (s *skillHost) FindPluginSkill(qualified string) (string, bool) {
	return s.manager().FindPluginSkill(qualified)
}

func (s *skillHost) ListPluginSkills(namespace string) []string {
	return s.manager().ListPluginSkills(namespace)
}

func (s *skillHost) ListPluginSkillMetadata() []map[string]any {
	return s.manager().ListPluginSkillMetadata()
}

func (s *skillHost) RemovePluginSkill(qualified string) { s.manager().RemovePluginSkill(qualified) }

// Current qualified name of a skill saved under the hashed agent-plugin namespace of a portable plugin
func (s *skillHost) LegacyPortableSkillName(namespace, bare string) string {
	if !strings.HasPrefix(namespace, "agent-plugin-") {
		return ""
	}
	m := s.manager()
	m.mu.RLock()
	defer m.mu.RUnlock()
	keys := map[string]bool{}
	for key, loaded := range m.plugins {
		if loaded.Manifest != nil && loaded.Manifest.Portable && loaded.Manifest.DataNamespace == namespace {
			keys[key] = true
		}
	}
	for _, qualified := range sortedKeys(m.pluginSkills) {
		if entry := m.pluginSkills[qualified]; keys[entry.PluginKey] && entry.BareName == bare {
			return qualified
		}
	}
	return ""
}

// Active memory provider name from memory.provider of the bound home
func (s *skillHost) ActiveMemoryProvider() (string, error) {
	fn, err := needService("ActiveMemoryProvider", func(v *Services) func(context.Context, string) string { return v.ActiveMemoryProvider })
	if err != nil {
		return "", err
	}
	var active string
	err = callGuarded(func() error {
		active = fn(s.ctx, s.h)
		return nil
	})
	return active, err
}

// Drops tracked memory provider skills that belong to a provider other than the active one
func (s *skillHost) PruneInactiveMemoryProviderSkills(active string) error {
	fn, err := needService("PruneInactiveMemoryProviderSkills", func(v *Services) func(context.Context, string, string) {
		return v.PruneInactiveMemoryProviderSkills
	})
	if err != nil {
		return err
	}
	return callGuarded(func() error {
		fn(s.ctx, s.h, active)
		return nil
	})
}

// Loads a memory provider by name so the skills it registers become resolvable
func (s *skillHost) LoadMemoryProvider(name string) error {
	fn, err := needService("LoadMemoryProvider", func(v *Services) func(context.Context, string, string) { return v.LoadMemoryProvider })
	if err != nil {
		return err
	}
	return callGuarded(func() error {
		fn(s.ctx, s.h, name)
		return nil
	})
}

// Sorted plugin keys and names plugins.disabled may use for the plugin owning a skill
func (s *skillHost) SkillOwners(qualified string) []string {
	return sortedKeys(pluginSkillOwners(s.manager(), qualified))
}

// Names parked in plugins.disabled of the bound home
func (s *skillHost) DisabledPlugins() map[string]bool { return DisabledPlugins(s.h) }

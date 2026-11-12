package plugins

import (
	"context"
	"path/filepath"
	"testing"
)

// Catalog world of one plugin over a two commit file repo, its catalog entry pinned to the first commit
type LoginWorld struct{ w *catWorldT }

// World whose plugin carries body then body with a v2 line, its host run by the test Python
func NewLoginWorld(t *testing.T, name, body string) *LoginWorld {
	t.Helper()
	w := catWorldNamed(t, name, body, body+"# v2\n")
	python := pubPython(t)
	installHostEnvHooks()
	previous := hostInterpreter
	hostInterpreter = func(context.Context) (string, error) { return python, nil }
	t.Cleanup(func() { hostInterpreter = previous })
	return &LoginWorld{w: w}
}

func (l *LoginWorld) Ctx() context.Context { return l.w.ctx }
func (l *LoginWorld) Home() string         { return l.w.h }
func (l *LoginWorld) Repo() string         { return l.w.repo }
func (l *LoginWorld) PluginsDir() string   { return l.w.pluginsDir }
func (l *LoginWorld) Sha1() string         { return l.w.sha1 }
func (l *LoginWorld) Sha2() string         { return l.w.sha2 }
func (l *LoginWorld) SetTier(tier string)  { l.w.tier = tier }

func (l *LoginWorld) Root() string                 { return l.w.root }
func (l *LoginWorld) SetCategory(category string)  { l.w.category = category }
func (l *LoginWorld) MemoryProvider() string       { return getCurrentMemoryProvider(l.w.h) }
func (l *LoginWorld) EnabledSet() map[string]bool  { return getEnabledSet(l.w.h) }
func (l *LoginWorld) DisabledSet() map[string]bool { return getDisabledSet(l.w.h) }

// Selects the memory provider in the profile config
func (l *LoginWorld) SaveMemoryProvider(t *testing.T, name string) {
	t.Helper()
	if err := saveMemoryProvider(l.w.ctx, l.w.h, name); err != nil {
		t.Fatal(err)
	}
}

// Replaces the plugin body, commits it and pins the catalog entry to that commit
func (l *LoginWorld) RepinTo(t *testing.T, body, msg string) {
	t.Helper()
	c6Write(t, filepath.Join(l.w.repo, "__init__.py"), body)
	l.w.pin = c6Commit(t, l.w.repo, msg)
}

// Sets one list of the plugins section, the rest of the config kept
func (l *LoginWorld) SetPluginsList(t *testing.T, key string, names ...string) {
	t.Helper()
	if err := writeConfigValue(l.w.ctx, l.w.h, "plugins", key, anyList(names)); err != nil {
		t.Fatal(err)
	}
}

// Commits every change of the repo and returns the new HEAD
func (l *LoginWorld) Commit(t *testing.T, msg string) string { return c6Commit(t, l.w.repo, msg) }

// Initializes a git repo over the files of the directory and commits them
func (l *LoginWorld) InitRepo(t *testing.T, repo, msg string) string {
	t.Helper()
	c6Git(t, repo, "init", "-q")
	return c6Commit(t, repo, msg)
}

// Installs the catalog entry of the world at the given ref, empty for the reviewed pin
func (l *LoginWorld) InstallCatalogEntry(ref string, allowRemoved bool) error {
	_, _, _, err := installCatalogEntry(l.w.ctx, inTreeCatalogEntries()[0], CatalogInstallOptions{Ref: ref, AllowRemoved: allowRemoved})
	return err
}

// Installs a Git URL through the installer core at the given ref
func (l *LoginWorld) InstallPluginCore(identifier, ref string) error {
	_, _, _, err := installPluginCore(l.w.ctx, identifier, installCoreOptions{Ref: ref})
	return err
}

// Writes the config enabling the plugin and its vault section, then loads a fresh manager of the home
func (l *LoginWorld) Load(t *testing.T, config string) *Manager {
	t.Helper()
	writeFile(t, filepath.Join(l.w.h, "config.yaml"), config)
	m := NewManager(l.w.h)
	t.Cleanup(func() {
		m.Unload(nil)
		if m.pluginHostInstance != nil {
			m.pluginHostInstance.Shutdown()
		}
	})
	m.DiscoverAndLoad(l.w.ctx, false)
	return m
}

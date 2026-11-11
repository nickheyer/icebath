package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
)

const unknownPluginRescanSecs = 10.0

type unknownPluginKey struct{ home, name string }

var (
	profilePluginsMu sync.Mutex
	profilePlugins   = map[string][]*pyjson.Object{}
	profileRescans   = map[string]int{}
	unknownPlugins   = map[unknownPluginKey]float64{}
)

// Active scope home when it is a profile other than the launch profile, false otherwise
func secondaryProfileHome(ctx context.Context) (string, bool) {
	h := home.Dir(ctx)
	if home.Key(h) == home.Key(home.Process()) {
		return "", false
	}
	return h, true
}

// Dashboard plugins of the profile home read from disk, its own plugins then the root's then bundled
func scanProfilePlugins(profileHome string) []*pyjson.Object {
	bundled, own := plugins.GetBundledPluginsDir(), filepath.Join(profileHome, "plugins")
	dirs := []string{own}
	root := filepath.Join(profiles.DefaultHome(), "plugins")
	a, errA := pypath.Realpath(root, pypath.Lax)
	b, errB := pypath.Realpath(own, pypath.Lax)
	if errA != nil || errB != nil || a != b {
		dirs = append(dirs, root)
	}
	search := []pluginSearchDir{}
	for _, d := range dirs {
		search = append(search, pluginSearchDir{d, "user", false})
	}
	search = append(search, pluginSearchDir{filepath.Join(bundled, "memory"), "bundled", false}, pluginSearchDir{bundled, "bundled", true})
	return discoverDashboardPlugins(search)
}

// Caches plugins for the key unless an explicit rescan began after this scan, returning them
func publishedProfilePlugins(key string, rescans int, list []*pyjson.Object) []*pyjson.Object {
	profilePluginsMu.Lock()
	defer profilePluginsMu.Unlock()
	if profileRescans[key] == rescans {
		profilePlugins[key] = list
	}
	return list
}

// True when any cached entry's plugin dir is gone
func pluginDirsMissing(list []*pyjson.Object) bool {
	for _, p := range list {
		if _, compiled := compiledDashboard(p); compiled {
			continue
		}
		dir, _ := p.GetOr("_dir", "").(string)
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			return true
		}
	}
	return false
}

// Dashboard plugins of the profile at home cached per home, an explicit rescan forgetting its cached misses
func profileDashboardPlugins(profileHome string, forceRescan bool) []*pyjson.Object {
	key := home.Key(profileHome)
	profilePluginsMu.Lock()
	if forceRescan {
		profileRescans[key]++
		for k := range unknownPlugins {
			if k.home == key {
				delete(unknownPlugins, k)
			}
		}
	}
	cached, has := profilePlugins[key]
	rescans := profileRescans[key]
	profilePluginsMu.Unlock()
	if !has || forceRescan || pluginDirsMissing(cached) {
		cached = publishedProfilePlugins(key, rescans, scanProfilePlugins(profileHome))
	}
	return cached
}

// Entry of the plugin named name, nil when absent
func namedPlugin(list []*pyjson.Object, name string) *pyjson.Object {
	for _, p := range list {
		if n, ok := p.GetOr("name", nil).(string); ok && n == name {
			return p
		}
	}
	return nil
}

var pluginClockStart = time.Now()

// Monotonic seconds of the unknown plugin miss cache
var pluginMonotonic = func() float64 { return time.Since(pluginClockStart).Seconds() }

// Profile's entry for name after a rescan, a name found missing within the window answered without one
func rescannedPlugin(profileHome, name string) *pyjson.Object {
	homeKey, now := home.Key(profileHome), pluginMonotonic()
	key := unknownPluginKey{homeKey, name}
	profilePluginsMu.Lock()
	if until, ok := unknownPlugins[key]; ok && until > now {
		profilePluginsMu.Unlock()
		return nil
	}
	rescans := profileRescans[homeKey]
	profilePluginsMu.Unlock()
	plugin := namedPlugin(publishedProfilePlugins(homeKey, rescans, scanProfilePlugins(profileHome)), name)
	profilePluginsMu.Lock()
	defer profilePluginsMu.Unlock()
	for k, until := range unknownPlugins {
		if until <= now {
			delete(unknownPlugins, k)
		}
	}
	if plugin != nil {
		delete(unknownPlugins, key)
	} else if profileRescans[homeKey] == rescans {
		unknownPlugins[key] = now + unknownPluginRescanSecs
	}
	return plugin
}

package plugins

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Plugins dir of the active home when it exists
func UserPluginsDir(ctx context.Context) (string, bool) {
	d := filepath.Join(home.Dir(ctx), "plugins")
	if st, err := os.Stat(d); err == nil && st.IsDir() {
		return d, true
	}
	return "", false
}

// Stat behind the plugin directory probes, a seam tests use to deny one child even as root
var pluginDirStat = os.Stat

// Directory listing behind the plugin scan, a seam tests use to deny a plugins root even as root
var pluginDirRead = os.ReadDir

// pathlib is_dir or exists of a path, the errors pathlib swallows answering false and the rest raised
func pathlibProbe(p string, wantDir bool) (bool, error) {
	st, err := pluginDirStat(p)
	if err != nil {
		if pypath.PathlibIgnoredError(err) {
			return false, nil
		}
		return false, err
	}
	return !wantDir || st.IsDir(), nil
}

// Sorted child dirs holding an __init__.py, skipping underscore and dot names and warning past an unreadable child
func IterPluginDirs(root string) ([]string, error) {
	if isDir, err := pathlibProbe(root, true); err != nil || !isDir {
		return nil, err
	}
	children, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(children))
	for _, c := range children {
		names = append(names, c.Name())
	}
	sort.Strings(names)
	var dirs []string
	for _, name := range names {
		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		child := filepath.Join(root, name)
		isDir, err := pathlibProbe(child, true)
		hasInit := false
		if err == nil && isDir {
			hasInit, err = pathlibProbe(filepath.Join(child, "__init__.py"), false)
		}
		if err != nil {
			logWarn(fmt.Sprintf("Skipping unreadable plugin directory %s: %s", child, pypath.OSErrorText(err)))
			continue
		}
		if hasInit {
			dirs = append(dirs, child)
		}
	}
	return dirs, nil
}

// Description of a plugin.yaml, empty when absent, falsy or unreadable
func ReadPluginDescription(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
	if err != nil {
		return ""
	}
	var meta any
	if err := yaml.Unmarshal([]byte(strings.TrimPrefix(string(raw), "\ufeff")), &meta); err != nil {
		return ""
	}
	if meta == nil {
		return ""
	}
	m, ok := meta.(map[string]any)
	if !ok {
		return ""
	}
	if v := m["description"]; pyrepr.Truthy(v) {
		return pyrepr.Str(v)
	}
	return ""
}

// Whether load returns an instance whose is_available answers truthy, any failure counting as unavailable
func ProbeAvailability(load func() (any, error)) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	instance, err := load()
	if err != nil || instance == nil {
		return false
	}
	switch v := instance.(type) {
	case *HostedObject:
		if v == nil {
			return false
		}
		ctx := context.Background()
		if v.HasMethod("is_available") {
			res, err := v.Call(ctx, "is_available", nil, nil)
			return err == nil && pyrepr.Truthy(res)
		}
		if _, err := v.Get(ctx, "is_available"); err == nil {
			return false
		}
		return true
	case interface{ IsAvailable() bool }:
		return v.IsAvailable()
	}
	return true
}

// Plugin host of this manager, started on first use
func (m *Manager) PluginHost() (*PluginHost, error) { return m.pluginHost() }

// Parameter names of a hosted method in order and whether it takes var keywords, an unknown signature taking anything
func (o *HostedObject) MethodParams(name string) (names []string, varKeyword bool) {
	sig := o.methods[name].Sig
	if sig == nil {
		return nil, true
	}
	for _, p := range sig {
		switch p.Kind {
		case paramVarKeyword:
			varKeyword = true
		case paramVarPositional:
		default:
			names = append(names, p.Name)
		}
	}
	return names, varKeyword
}

// Shared body of a named category load, a warning and nil when it fails or yields no instance
func LoadNamed(name, pluginDir string, loadFromDir func(dir string) (any, error), kind, noun string) any {
	instance, err := loadFromDir(pluginDir)
	if err != nil {
		logWarn(fmt.Sprintf("Failed to load %s '%s': %s", strings.ToLower(kind), name, err))
		return nil
	}
	if instance == nil || !config.PyTruthy(instance) {
		logWarn(fmt.Sprintf("%s '%s' loaded but no %s instance found", kind, name, noun))
		return nil
	}
	return instance
}

package plugins

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/home"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
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
		if pathutil.IsMissing(err) {
			return false, nil
		}
		return false, err
	}
	return !wantDir || st.IsDir(), nil
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
	if v := m["description"]; anyutil.IsTrue(v) {
		return anyutil.String(v)
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
			return err == nil && anyutil.IsTrue(res)
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

// Shared body of a named category load, a warning on the category logger and nil when it fails or yields no instance
func LoadNamed(name, pluginDir string, loadFromDir func(dir string) (any, error), kind, noun string) any {
	log := pkgLog()
	instance, err := loadFromDir(pluginDir)
	if err != nil {
		log.Warn(fmt.Sprintf("Failed to load %s '%s': %s", strings.ToLower(kind), name, err))
		return nil
	}
	if instance == nil || !anyutil.IsTrue(instance) {
		log.Warn(fmt.Sprintf("%s '%s' loaded but no %s instance found", kind, name, noun))
		return nil
	}
	return instance
}

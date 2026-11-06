package plugins

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
)

var pluginStorageNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// Plugin name the install command accepts, a ValueError for separators, traversal or overlong names
func validateStorageName(name string) (string, error) {
	if !pluginStorageNameRe.MatchString(name) || strings.Contains(name, "..") {
		return "", &HostBuiltinError{Type: "ValueError", Message: fmt.Sprintf("invalid plugin name for storage: %s", config.PyRepr(name))}
	}
	return name, nil
}

// Creates and returns the plugin data directory under the active home, never the install tree
func PluginStorageDir(ctx context.Context, name string) (string, error) {
	clean, err := validateStorageName(name)
	if err != nil {
		return "", err
	}
	root := filepath.Join(home.PluginDataDir(home.Dir(ctx)), clean)
	if err := os.MkdirAll(root, 0o777); err != nil {
		return "", err
	}
	return root, nil
}

// Final component of a POSIX path as pathlib names it, dot components and repeated slashes dropped
// Opens a database inside the plugin data directory with the shared journal mode verdict and foreign keys on
func PluginDB(ctx context.Context, name, filename string) (state.SQLiteConn, error) {
	if purePathName(filename) != filename || filename == "" {
		return nil, &HostBuiltinError{Type: "ValueError", Message: fmt.Sprintf("invalid plugin db filename: %s", config.PyRepr(filename))}
	}
	dir, err := PluginStorageDir(ctx, name)
	if err != nil {
		return nil, err
	}
	return state.OpenDB(ctx, filepath.Join(dir, filename), fmt.Sprintf("plugin-data/%s/%s", name, filename), state.WithForeignKeys())
}

// Prepares a plugin database for a hosted plugin, a child build with the WAL reset bug keeps a new file out of WAL
func sdkPluginDBPrepare(ctx context.Context, h *PluginHost, args []any, kwargs map[string]any) (any, error) {
	a, _, err := bindPy("plugin_db_prepare", []pyParam{req("name"), req("filename"), req("sqlite_version_info")}, args, kwargs)
	if err != nil {
		return nil, err
	}
	name, filename := argStr(a["name"]), argStr(a["filename"])
	ctx = home.WithOverride(ctx, h.home)
	opts := []state.OpenDBOption{state.WithForeignKeys()}
	if childVersion, ok := childSQLiteVersion(a["sqlite_version_info"]); ok && state.IsSQLiteWALResetVulnerable(childVersion) {
		opts = append(opts, state.WithWAL(false))
	}
	if purePathName(filename) != filename || filename == "" {
		return nil, &HostBuiltinError{Type: "ValueError", Message: fmt.Sprintf("invalid plugin db filename: %s", config.PyRepr(filename))}
	}
	dir, err := PluginStorageDir(ctx, name)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, filename)
	conn, err := state.OpenDB(ctx, path, fmt.Sprintf("plugin-data/%s/%s", name, filename), opts...)
	if err != nil {
		return nil, err
	}
	return path, conn.Close()
}

// Version tuple of the child sqlite3 module from its three part sqlite_version_info
func childSQLiteVersion(v any) (state.VersionTuple, bool) {
	items, ok := v.([]any)
	if !ok || len(items) != 3 {
		return state.VersionTuple{}, false
	}
	parts := make([]int64, 3)
	for i, item := range items {
		n, ok := item.(int64)
		if !ok {
			return state.VersionTuple{}, false
		}
		parts[i] = n
	}
	return state.NewVersionTuple(parts...), true
}

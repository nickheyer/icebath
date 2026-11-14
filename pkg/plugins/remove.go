package plugins

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Cache subdirectory holding trees a running process still held at removal
const pendingDeleteDir = "plugin-pending-delete"

// Pending delete area under the home cache
func pendingDeleteRoot(h string) string { return filepath.Join(h, "cache", pendingDeleteDir) }

// Deletes what an earlier remove parked because another process held it
func SweepPendingPluginDeletes(ctx context.Context) {
	root := pendingDeleteRoot(home.Dir(ctx))
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		name := entry.Name()
		if i := strings.LastIndex(name, "."); i > 0 && i < len(name)-1 && name[i:] == ".path" && isFilePath(path) {
			raw, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			held := pystr.Strip(strings.TrimPrefix(string(raw), "\uFEFF"))
			if held == "" {
				held = "."
			}
			_ = tools.RmtreeReadonly(held, true)
			if _, err := os.Stat(held); err != nil {
				_ = os.Remove(path)
			}
			continue
		}
		_ = tools.RmtreeReadonly(path, true)
	}
}

// Whether an error is an OSError in the Python sense
func isOSError(err error) bool {
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	var sysErr *os.SyscallError
	var errno syscall.Errno
	return errors.As(err, &pathErr) || errors.As(err, &linkErr) || errors.As(err, &sysErr) || errors.As(err, &errno)
}

// Deletes a tree, parking what a windows process still holds in the pending delete area
func discardTree(h, tree string) error {
	err := tools.RmtreeReadonly(tree, false)
	if err == nil {
		return nil
	}
	if !errors.Is(err, fs.ErrPermission) {
		return err
	}
	if _, statErr := os.Stat(tree); runtime.GOOS != "windows" || statErr != nil {
		return err
	}
	root := pendingDeleteRoot(h)
	if err := os.MkdirAll(root, 0o777); err != nil {
		return err
	}
	holder, err := os.MkdirTemp(root, filepath.Base(tree)+"-")
	if err != nil {
		return err
	}
	if err := os.Rename(tree, filepath.Join(holder, "plugin")); err != nil {
		for _, name := range []string{"plugin.yaml", "plugin.yml", "plugin.json", "__init__.py"} {
			if err := os.Remove(filepath.Join(tree, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		note := filepath.Join(root, filepath.Base(filepath.Dir(tree))+"-"+filepath.Base(tree)+".path")
		return os.WriteFile(note, []byte(tree), 0o666)
	}
	return nil
}

// Removes one plugin and its install metadata without splitting their state
func removePluginCore(ctx context.Context, h, target string) error {
	metadata, err := readInstallMetadata(h)
	if err != nil {
		return err
	}
	name := filepath.Base(target)
	if _, recorded := metadata[name]; !recorded {
		return discardTree(h, target)
	}
	forget := func(map[string]any) map[string]any { return nil }
	staging, err := os.MkdirTemp(filepath.Dir(target), "."+name+".remove-")
	if err != nil {
		return err
	}
	backup := filepath.Join(staging, "plugin")
	if err := os.Rename(target, backup); err != nil {
		_ = tools.RmtreeReadonly(staging, true)
		if !errors.Is(err, fs.ErrPermission) || runtime.GOOS != "windows" {
			return err
		}
		if err := updateInstallRecord(ctx, h, name, forget); err != nil {
			return err
		}
		return discardTree(h, target)
	}
	if err := updateInstallRecord(ctx, h, name, forget); err != nil {
		if restoreErr := os.Rename(backup, target); restoreErr != nil {
			return &PluginOperationError{Msg: fmt.Sprintf("Plugin metadata update failed and '%s' could not be "+
				"restored automatically; recovery copy remains at %s.", name, backup)}
		}
		_ = tools.RmtreeReadonly(staging, true)
		return err
	}
	return discardTree(h, staging)
}

// Removes an installed plugin by name
func CmdRemove(ctx context.Context, name string, out io.Writer) error {
	console := NewConsole(out)
	h := home.Dir(ctx)
	dir, err := pluginsDir(h)
	if err != nil {
		return err
	}
	target, err := requireInstalledPlugin(ctx, name, dir, console)
	if err != nil {
		return err
	}
	result, err := removeUserPlugin(ctx, dir, name, target)
	if err != nil {
		var opErr *PluginOperationError
		if isOSError(err) || errors.As(err, &opErr) {
			return fail(console, fmt.Sprintf("[red]Error:[/red] Could not remove plugin '%s': %s", name, pyErrorText(err)))
		}
		return err
	}
	console.Print("")
	console.Print(fmt.Sprintf("[red]✗[/red] Plugin [bold]%s[/bold] removed from %s", name, dir))
	if cleared, _ := result["cleared_memory_provider"].(bool); cleared {
		console.Print("[yellow]memory.provider pointed at this plugin and was reset; " +
			"run `icebath memory setup` to pick another.[/yellow]")
	}
	console.Print("")
	return nil
}

// Shared remove tail for the CLI, the dashboard and the manage RPC, unlinking a symlinked alias only
func removeUserPlugin(ctx context.Context, dir, name, target string) (map[string]any, error) {
	h := home.Dir(ctx)
	link := filepath.Join(dir, strings.Trim(name, "/"))
	if st, err := os.Lstat(link); err == nil && st.Mode()&fs.ModeSymlink != 0 {
		if err := os.Remove(link); err != nil {
			return nil, err
		}
		forgotten, err := forgetPluginConfig(ctx, h, map[string]bool{filepath.Base(link): true})
		if err != nil {
			return nil, err
		}
		return mergeResult(map[string]any{"ok": true, "name": name}, forgotten), nil
	}
	entries, err := DiscoverAllPlugins(h)
	if err != nil {
		return nil, err
	}
	key := filepath.Base(target)
	for _, e := range entries {
		if filepath.Clean(e.Dir) == filepath.Clean(target) {
			key = e.Key
			break
		}
	}
	aliases, err := pluginAliases(h, key)
	if err != nil {
		return nil, err
	}
	aliases[filepath.Base(target)] = true
	svc := installedServices()
	if svc == nil || svc.PluginOAuthKey == nil || svc.ForgetPluginGrants == nil {
		return nil, &ServiceMissingError{Name: "PluginOAuthKey"}
	}
	grantKey := svc.PluginOAuthKey(target)
	if err := togglePluginToolset(ctx, h, key, false, ""); err != nil {
		return nil, err
	}
	if err := removePluginCore(ctx, h, target); err != nil {
		return nil, err
	}
	if err := svc.ForgetPluginGrants(grantKey); err != nil {
		return nil, err
	}
	forgotten, err := forgetPluginConfig(ctx, h, aliases)
	if err != nil {
		return nil, err
	}
	return mergeResult(map[string]any{"ok": true, "name": name}, forgotten), nil
}

// Text of a caught OSError or plugin operation error as Python formats it
func pyErrorText(err error) string {
	if isOSError(err) {
		return pypath.OSErrorText(err)
	}
	return err.Error()
}

// Result map with the extra keys merged in
func mergeResult(base, extra map[string]any) map[string]any {
	for k, v := range extra {
		base[k] = v
	}
	return base
}

// Deletes a plugin tree under the user plugins dir only, for the dashboard
func DashboardRemoveUserPlugin(ctx context.Context, name string) (map[string]any, error) {
	h := home.Dir(ctx)
	dir, err := pluginsDir(h)
	if err != nil {
		return nil, err
	}
	entries, err := DiscoverAllPlugins(h)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Name == name && e.Source == "bundled" {
			return map[string]any{"ok": false, "error": "Bundled plugins cannot be removed from the dashboard."}, nil
		}
	}
	target, err := userInstalledPluginDir(ctx, name)
	if err != nil {
		return nil, err
	}
	if target == "" {
		return map[string]any{"ok": false, "error": fmt.Sprintf("Plugin '%s' was not found under %s.", name, dir)}, nil
	}
	result, err := removeUserPlugin(ctx, dir, name, target)
	if err != nil {
		var opErr *PluginOperationError
		if isOSError(err) || errors.As(err, &opErr) {
			return map[string]any{"ok": false, "error": fmt.Sprintf("Could not remove plugin '%s': %s", name, pyErrorText(err))}, nil
		}
		return nil, err
	}
	return result, nil
}

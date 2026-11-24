package pm

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/errchain"
	fsutil "github.com/nickheyer/icebath/pkg/utils/fs"
)

// Copies a tree like shutil.copytree with copy2, links copied as links or followed, failures gathered
func copyTreeLike(src, dst string, symlinks bool, ignore func(dir string, names []string) map[string]bool) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	ignored := map[string]bool{}
	if ignore != nil {
		ignored = ignore(src, names)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
		return err
	}
	if err := os.Mkdir(dst, 0o777); err != nil {
		return err
	}
	var failures []string
	for _, name := range names {
		if ignored[name] {
			continue
		}
		from, to := filepath.Join(src, name), filepath.Join(dst, name)
		st, err := os.Lstat(from)
		if err != nil {
			failures = append(failures, fmt.Sprintf("could not copy %s -> %s: %s", from, to, errchain.OSText(err)))
			continue
		}
		if st.Mode()&fs.ModeSymlink != 0 && symlinks {
			target, err := os.Readlink(from)
			if err == nil {
				err = os.Symlink(target, to)
			}
			if err == nil {
				err = fsutil.CopyLinkMetadata(from, to)
			}
			if err != nil {
				failures = append(failures, fmt.Sprintf("could not copy %s -> %s: %s", from, to, errchain.OSText(err)))
			}
			continue
		}
		if resolved, statErr := os.Stat(from); statErr == nil && resolved.IsDir() {
			err = copyTreeLike(from, to, symlinks, ignore)
		} else if statErr == nil {
			err = copy2Entry(from, to, resolved)
		} else {
			err = copy2(from, to)
		}
		if err != nil {
			failures = append(failures, fmt.Sprintf("could not copy %s -> %s: %s", from, to, err.Error()))
		}
	}
	if err := copyStat(src, dst, nil); err != nil {
		failures = append(failures, fmt.Sprintf("could not copy %s -> %s: %s", src, dst, errchain.OSText(err)))
	}
	if len(failures) > 0 {
		return &PyError{Class: "Error", Message: strings.Join(failures, "; ")}
	}
	return nil
}

// PluginEntry is one selected plugin, the home plugins dir naming it, its selection key and its tree
type PluginEntry struct {
	PluginsDir string
	Name       string
	PluginDir  string
}

// Every selected plugin in config order with the key its home names it by
func enabledPluginEntries(ctx context.Context, opts EnabledPluginsOptions) ([]PluginEntry, error) {
	selection, err := EnabledPluginsOrdered(ctx, opts)
	if err != nil {
		return nil, err
	}
	var entries []PluginEntry
	for _, home := range selection {
		for _, name := range home.Names {
			pluginDir, err := SelectedPluginDir(home.PluginsDir, name, opts.Installing)
			if err != nil {
				return nil, err
			}
			if pluginDir != "" {
				entries = append(entries, PluginEntry{PluginsDir: home.PluginsDir, Name: name, PluginDir: pluginDir})
			}
		}
	}
	return entries, nil
}

// Tree a selection key names, the home's own plugins dir first then the bundled plugins, empty when neither has it
func SelectedPluginDir(pluginsDir, name, installing string) (string, error) {
	relative := filepath.FromSlash(name)
	if filepath.IsAbs(relative) || strings.HasPrefix(name, "/") {
		return "", NewInstallError("venv", "invalid plugin key: "+name, "")
	}
	for _, part := range strings.Split(filepath.ToSlash(relative), "/") {
		if part == ".." {
			return "", NewInstallError("venv", "invalid plugin key: "+name, "")
		}
	}
	pluginDir := filepath.Join(pluginsDir, relative)
	if installing != "" && resolvePath(pluginDir) == resolvePath(installing) {
		return pluginDir, nil
	}
	isDir, err := pluginIsDirectory(pluginDir)
	if err != nil {
		return "", err
	}
	if !isDir {
		pluginDir = filepath.Join(RepoRoot(), "plugins", relative)
	}
	if isDir, err = pluginIsDirectory(pluginDir); err != nil || !isDir {
		return "", err
	}
	return pluginDir, nil
}

// Effective plugin selection as distinct plugin trees for the plugin installer
func EnabledPluginDirs(ctx context.Context, opts EnabledPluginsOptions) ([]string, error) {
	return enabledPluginDirs(ctx, opts)
}

// Effective plugin selection as distinct plugin trees, declarations not yet consulted
func enabledPluginDirs(ctx context.Context, opts EnabledPluginsOptions) ([]string, error) {
	entries, err := enabledPluginEntries(ctx, opts)
	if err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	for _, entry := range entries {
		if !seen[entry.PluginDir] {
			seen[entry.PluginDir] = true
			out = append(out, entry.PluginDir)
		}
	}
	return out, nil
}

// Package acquisition the sidecar install goes through, replaceable by tests the way Python patches pm ensure
var sidecarEnsure = Ensure

// Installs a plugin's Node dependencies with PM's npm, returning a diagnostic or empty on success
func InstallNodeSidecar(ctx context.Context, pluginDir string, explicit bool) string {
	if !isRegularFile(filepath.Join(pluginDir, "package.json")) {
		return ""
	}
	if !explicit && !LazyInstallsAllowedFor(ctx) {
		return "lazy installs are disabled — run `icebath plugins install` and approve Node dependencies"
	}
	installCmd := "install"
	if isRegularFile(filepath.Join(pluginDir, "package-lock.json")) {
		installCmd = "ci"
	}
	runner, err := sidecarEnsure(ctx, "npm", EnsureOptions{Explicit: explicit})
	if err != nil {
		return fmt.Sprintf("npm %s failed to run: %s", installCmd, err)
	}
	searchPath := runner.Env["PATH"]
	npm := whichOn("npm", &searchPath)
	if npm == "" {
		return "npm is missing from the prepared PM environment"
	}
	argv := []string{npm, installCmd, "--no-audit", "--no-fund"}
	runCtx, cancel := context.WithTimeout(ctx, 900*time.Second)
	defer cancel()
	cmd := runner.Command(runCtx, argv[0], argv[1:]...)
	cmd.Dir = pluginDir
	cmd.WaitDelay = time.Second
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if runCtx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("npm %s failed to run: %s timed out after 900 seconds", installCmd, strings.Join(argv, " "))
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return fmt.Sprintf("npm %s failed to run: %s", installCmd, errchain.OSText(err))
	}
	if exitErr == nil {
		return ""
	}
	decode := func(raw string) string {
		text, _ := decodeUTF8Replace([]byte(raw), true)
		return universalNewlines(text)
	}
	tail := decode(stderr.String())
	if tail == "" {
		tail = decode(stdout.String())
	}
	tail = strings.TrimSpace(tail)
	if n := utf8.RuneCountInString(tail); n > 300 {
		tail = string([]rune(tail)[n-300:])
	}
	return fmt.Sprintf("npm %s exited %d: %s", installCmd, pyReturnCode(exitErr), tail)
}

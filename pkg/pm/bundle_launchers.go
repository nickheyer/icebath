package pm

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
)

//go:embed bundle_scripts/launcher_wrapper.py
var launcherWrapperTemplate string

//go:embed bundle_scripts/mint_launchers.py
var mintLaunchersScript string

var unresolvedLauncherPlaceholder = regexp.MustCompile(`__ICEBATH_\w+?__`)

// Launcher wrapper source with the entry and payload relative roots substituted
func renderWrapper(entry, repo, site string) (string, error) {
	module, function, ok := strings.Cut(entry, ":")
	if !ok {
		return "", &PyError{Class: "ValueError", Message: "not enough values to unpack (expected 2, got 1)"}
	}
	text := launcherWrapperTemplate
	for _, pair := range [][2]string{{"ENTRY_MODULE", module}, {"ENTRY_FUNC", function}, {"REPO_REL", repo}, {"SITE_REL", site}} {
		if strings.Contains(pair[1], `"`) || strings.Contains(pair[1], "\n") || strings.Contains(pair[1], "__") {
			return "", &PyError{Class: "ValueError", Message: "invalid launcher value: " + pair[0]}
		}
		text = strings.ReplaceAll(text, "__ICEBATH_"+pair[0]+"__", pair[1])
	}
	if unresolved := unresolvedLauncherPlaceholder.FindString(text); unresolved != "" {
		return "", &PyError{Class: "ValueError", Message: "unresolved launcher placeholder: " + unresolved}
	}
	return text, nil
}

// Shell launcher that resolves its own links, pins the payload interpreter and runs the entry point
func posixLauncher(name, entry, python, repo, site, target string) string {
	shellPath := func(value string) string {
		if filepath.IsAbs(value) {
			return pyshlex.Quote(value)
		}
		return `"$root"/` + pyshlex.Quote(value)
	}
	module, function, _ := strings.Cut(entry, ":")
	bionic := strings.HasSuffix(target, "-bionic")
	header := "#!/usr/bin/env bash"
	extra := ""
	if bionic {
		header = "#!/data/data/com.termux/files/usr/bin/sh"
		extra = `PREFIX="${PREFIX:-/data/data/com.termux/files/usr}"
export PREFIX
export LD_LIBRARY_PATH="$root/tools/python/data/data/com.termux/files/usr/lib:$root/tools/node/data/data/com.termux/files/usr/lib:$root/tools/ffmpeg/data/data/com.termux/files/usr/lib:$root/runtime-libs/lib:$PREFIX/lib"
export ICEBATH_PYTHON_SRC_ROOT="$REPO"
export ICEBATH_PYTHON="$PYTHON"
export ICEBATH_NODE="$root/tools/node/data/data/com.termux/files/usr/bin/node"
export ICEBATH_RUNTIME_DIR="$root/tools"
export PATH="$root/tools/npm/bin:$root/tools/node/data/data/com.termux/files/usr/bin:$root/tools/ffmpeg/data/data/com.termux/files/usr/bin:$root/tools/ripgrep:$PATH"
`
	}
	code := fmt.Sprintf("import os, site, sys; sys.argv[0]=%s; site.addsitedir(os.environ['ICEBATH_SITE']); from %s import %s; sys.exit(%s())",
		pyrepr.StrRepr(name), module, function, function)
	return header + `
set -eu
self="$0"
while [ -L "$self" ]; do
    target="$(readlink "$self")"
    case "$target" in
        /*) self="$target" ;;
        *) self="$(dirname "$self")/$target" ;;
    esac
done
root="$(cd "$(dirname "$self")/.." && pwd)"
PYTHON=` + shellPath(python) + `
REPO=` + shellPath(repo) + `
SITE=` + shellPath(site) + `
[ -x "$PYTHON" ] || { printf '%s\n' 'Bundled interpreter missing; reinstall icebath.' >&2; exit 2; }
unset PYTHONPATH PYTHONHOME
export PYTHONPATH="$REPO:$SITE"
# PYTHONPATH cannot process .pth files (only site.addsitedir() can), and
# the venv's .pth files are load-bearing (pywin32.pth -> win32\lib ->
# ` + "`import pywintypes`" + ` on Windows bundles; the win32 wrapper mirrors this
# in launcher_wrapper.py). The -c bootstrap below runs addsitedir() on it.
export ICEBATH_SITE="$SITE"
export PYTHONPYCACHEPREFIX="${PYTHONPYCACHEPREFIX:-${XDG_CACHE_HOME:-$HOME/.cache}/icebath-pycache}"
` + extra + `exec "$PYTHON" -P -c ` + pyshlex.Quote(code) + ` "$@"
`
}

// Runs the launcher mint child, failing like subprocess.run with check on a nonzero exit
var mintLauncherRun = func(ctx context.Context, argv []string, env []string) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env, cmd.Stdout, cmd.Stderr = env, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return &CalledProcessError{Argv: argv, Code: pyReturnCode(exitErr)}
		}
		return err
	}
	return nil
}

// Mints one launcher per declared command and returns each command's output relative path
func writeLaunchers(ctx context.Context, root string, entries *OrderedObject, python, repo, site, target, binDir string) (*OrderedObject, error) {
	bindir := filepath.Join(root, binDir)
	if err := os.MkdirAll(bindir, 0o777); err != nil {
		return nil, err
	}
	windows := strings.HasPrefix(target, "win32")
	for _, name := range entries.Keys {
		entry := pyrepr.Str(entries.Values[name])
		if windows {
			if err := mintWindowsLauncher(ctx, root, bindir, name, entry, python, repo, site); err != nil {
				return nil, err
			}
			continue
		}
		output := filepath.Join(bindir, name)
		if err := writeTextFile(output, posixLauncher(name, entry, python, repo, site, target)); err != nil {
			return nil, err
		}
		if err := os.Chmod(output, 0o755); err != nil {
			return nil, err
		}
	}
	commands := NewOrderedObject()
	suffix := ""
	if windows {
		suffix = ".exe"
	}
	for _, name := range entries.Keys {
		commands.Set(name, binDir+"/"+name+suffix)
	}
	for _, name := range commands.Keys {
		command := commands.Values[name].(string)
		if !isRegularFile(filepath.Join(root, filepath.FromSlash(command))) {
			return nil, &PyError{Class: "FileNotFoundError", Message: "payload launcher missing: " + command}
		}
	}
	return commands, nil
}

// Mints one Windows exe launcher through the payload interpreter and the distlib mint script
func mintWindowsLauncher(ctx context.Context, root, bindir, name, entry, python, repo, site string) (err error) {
	temp, err := os.MkdirTemp("", "icebath-mint-")
	if err != nil {
		return err
	}
	defer pyfs.CloseTempDir(temp, &err)
	wrapperText, err := renderWrapper(entry, "../"+repo, "../"+site)
	if err != nil {
		return err
	}
	wrapper := filepath.Join(temp, "wrapper.py")
	if err := writeTextFile(wrapper, wrapperText); err != nil {
		return err
	}
	script := filepath.Join(temp, "mint_launchers.py")
	if err := os.WriteFile(script, []byte(mintLaunchersScript), 0o666); err != nil {
		return err
	}
	module, function, _ := strings.Cut(entry, ":")
	spec := pyjson.NewObject()
	spec.Set("name", name)
	spec.Set("module", module)
	spec.Set("func", function)
	specs, err := pyjson.Dumps([]any{spec})
	if err != nil {
		return err
	}
	env := append(os.Environ(), "ICEBATH_MINT_BIN_DIR="+bindir, "ICEBATH_MINT_SPECS="+specs,
		"ICEBATH_MINT_WRAPPER="+wrapper, `ICEBATH_MINT_PYTHON=<launcher_dir>\..\`+strings.ReplaceAll(python, "/", `\`))
	return mintLauncherRun(ctx, []string{filepath.Join(root, filepath.FromSlash(python)), script}, env)
}

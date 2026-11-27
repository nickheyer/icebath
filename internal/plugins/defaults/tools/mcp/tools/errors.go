package tools

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/internal/mcp"
	"github.com/nickheyer/icebath/internal/packages"
)

// permanent for failures no retry mends: sign in, an address that is no MCP endpoint, a bad address, a missing command, a native addon of
// another Node; transient otherwise
func classify(err error) string {
	if err == nil {
		return "transient"
	}
	if mcp.AuthFailure(err) {
		return "permanent"
	}
	_, foreign := errors.AsType[*mcp.NonMCPError](err)
	_, invalid := errors.AsType[*mcp.InvalidURLError](err)
	_, abi := errors.AsType[*abiError](err)
	_, unset := errors.AsType[*unsetError](err)
	if foreign || invalid || abi || unset || missing(err) != "" {
		return "permanent"
	}
	if status, ok := errors.AsType[*mcp.HTTPStatusError](err); ok && (status.Status == 401 || status.Status == 403) {
		return "permanent"
	}
	return "transient"
}

// File or command a failure says is missing, empty when it says none
func missing(err error) string {
	if exe, ok := errors.AsType[*exec.Error](err); ok && (errors.Is(exe.Err, exec.ErrNotFound) || errors.Is(exe.Err, fs.ErrNotExist)) {
		return exe.Name
	}
	if path, ok := errors.AsType[*fs.PathError](err); ok && (errors.Is(path.Err, fs.ErrNotExist) || errors.Is(path.Err, syscall.ENOENT)) {
		return path.Path
	}
	return ""
}

var nodeBins = map[string]bool{"npx": true, "npm": true, "node": true}

// Short message of a failed connect the person can act on: a missing command, a native addon of another Node, else the text of the failure
func describe(err error) string {
	if abi, ok := errors.AsType[*abiError](err); ok {
		return mcp.Redact(abi.Error())
	}
	if name := missing(err); name != "" {
		message := fmt.Sprintf("missing executable '%s'", name)
		if nodeBins[filepath.Base(name)] {
			message += " (ensure Node.js is installed and PATH includes its bin directory, or set the server's command to an absolute path and include " +
				"that directory in its env PATH)"
		}
		return mcp.Redact(message)
	}
	text := strings.TrimSpace(err.Error())
	if text == "" {
		text = "(empty error message)"
	}
	return mcp.Redact(text)
}

// Native addon of a server that does not load under the Node it runs on; every retry loads the same binary
type abiError struct{ msg string }

func (e *abiError) Error() string { return e.msg }

var (
	abiModule   = regexp.MustCompile(`'([^'\n]+?\.node)'|(\S+?\.node):`)
	abiVersions = regexp.MustCompile(`(?s)using\s+NODE_MODULE_VERSION (\d+)\..*?requires\s+NODE_MODULE_VERSION (\d+)`)
)

// The native addon failure a server's stderr shows in place of err, err itself when it shows none. Servers run on the package manager's
// Node, so the remedy rebuilds under it; on a machine it has no Node for, under the Node.js on PATH
func (p *pool) abiOr(server, stderr string, err error) error {
	if !strings.Contains(stderr, "NODE_MODULE_VERSION") && !strings.Contains(stderr, "ERR_DLOPEN_FAILED") {
		return err
	}
	module := ""
	if m := abiModule.FindStringSubmatch(stderr); m != nil {
		module = cmp.Or(m[1], m[2])
	}
	pm := p.m.helpers.Packages
	if missing, merr := pm.Missing(packages.NPM); merr == nil && missing == "" {
		node, _ := pm.Installed(packages.Node, false)
		npm, _ := pm.Installed(packages.NPM, false)
		ours := "Icebath's Node"
		if node != nil && node.Version != "" {
			ours += " " + node.Version
		}
		built := "it does not load under " + ours
		if v := abiVersions.FindStringSubmatch(stderr); v != nil {
			built = fmt.Sprintf("NODE_MODULE_VERSION %s; %s needs %s", v[1], ours, v[2])
		}
		head := fmt.Sprintf("native addon built for a different Node.js (%s): %s. Icebath runs MCP servers on its own Node; rebuild the server under it",
			built, cmp.Or(module, "see logs/mcp-stderr.log"))
		if node == nil || node.Binary == "" || npm == nil || npm.Binary == "" {
			return &abiError{msg: fmt.Sprintf("%s: install Icebath's Node with `icebath pm install npm`, then run `icebath mcp test %s`", head, server)}
		}
		root, pkg := packageOf(module)
		return &abiError{msg: head + ": " + rebuild(root, pkg, node.Binary, npm.Binary)}
	}
	built := "it does not load under the Node.js on PATH"
	if v := abiVersions.FindStringSubmatch(stderr); v != nil {
		built = fmt.Sprintf("NODE_MODULE_VERSION %s; the Node.js on PATH needs %s", v[1], v[2])
	}
	head := fmt.Sprintf("native addon built for a different Node.js (%s): %s. Rebuild the server under the Node.js it runs on",
		built, cmp.Or(module, "see logs/mcp-stderr.log"))
	node, nodeErr := exec.LookPath("node")
	npm, npmErr := exec.LookPath("npm")
	if nodeErr != nil || npmErr != nil {
		return &abiError{msg: fmt.Sprintf("%s: install Node.js and npm, then run `icebath mcp test %s`", head, server)}
	}
	root, pkg := packageOf(module)
	return &abiError{msg: head + ": " + rebuild(root, pkg, node, npm)}
}

// Package folder and name a module path sits in under node_modules, empty when it sits in none
func packageOf(module string) (string, string) {
	parts := strings.Split(filepath.ToSlash(module), "/")
	for i := len(parts) - 2; i >= 0; i-- {
		if parts[i] != "node_modules" || i+1 >= len(parts) {
			continue
		}
		name := parts[i+1]
		if strings.HasPrefix(name, "@") && i+2 < len(parts) {
			name += "/" + parts[i+2]
		}
		return filepath.FromSlash(strings.Join(parts[:i], "/")), name
	}
	return "", ""
}

// Command that rebuilds the package under the Node given, or that removes an npx copy to have it reinstalled
func rebuild(root, pkg, node, npm string) string {
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }
	if runtime.GOOS == "windows" {
		quote = func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	}
	target := ""
	if root != "" {
		target = fmt.Sprintf(" %s --prefix %s", quote(pkg), quote(root))
	}
	command := fmt.Sprintf("PATH=%s:\"$PATH\" %s rebuild%s", quote(filepath.Dir(node)), quote(npm), target)
	if runtime.GOOS == "windows" {
		command = fmt.Sprintf("$env:Path = %s + $env:Path; & %s rebuild%s", quote(filepath.Dir(node)+";"), quote(npm), target)
	}
	if root == "" {
		return "in the server's package directory run " + command
	}
	if filepath.Base(filepath.Dir(root)) != "_npx" {
		return command
	}
	remove := "rm -rf " + quote(root)
	if runtime.GOOS == "windows" {
		remove = "Remove-Item -Recurse -Force " + quote(root)
	}
	return remove + " (Icebath reinstalls it on the next start), or " + command
}

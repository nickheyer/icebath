package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// Stderr markers of a native addon that failed to load under this Node
var nodeABIMarkers = []string{"NODE_MODULE_VERSION", "ERR_DLOPEN_FAILED"}

// Quoted or colon terminated path of the failing .node module
var nodeABIModulePath = regexp.MustCompile(`'([^'\n]+?\.node)'|(\S+?\.node):`)

// Module version the addon was built for and the one this Node requires
var nodeABIVersions = regexp.MustCompile(`(?s)using\s+NODE_MODULE_VERSION (\d+)\..*?requires\s+NODE_MODULE_VERSION (\d+)`)

// NodeABIMismatchError is a native addon that does not load under the managed Node, every retry loads the same binary
type NodeABIMismatchError struct{ Msg string }

func (e *NodeABIMismatchError) Error() string { return e.Msg }

// Binary and version of the package manager node or npm, empty when it has none, swapped by tests
var nodeABIManaged = func(name string) (binary, version string) {
	b, v, err := pmInstalledBinary(context.Background(), name)
	if err != nil {
		return "", ""
	}
	return b, v
}

// Path components the way pathlib splits them, the anchor first
func pathParts(p string) []string {
	vol := filepath.VolumeName(p)
	rest := p[len(vol):]
	var parts []string
	switch {
	case rest != "" && os.IsPathSeparator(rest[0]):
		parts = append(parts, vol+string(filepath.Separator))
	case vol != "":
		parts = append(parts, vol)
	}
	for _, c := range strings.FieldsFunc(rest, func(r rune) bool { return r < 0x80 && os.IsPathSeparator(uint8(r)) }) {
		if c != "." {
			parts = append(parts, c)
		}
	}
	return parts
}

// Project root above the innermost node_modules and the possibly scoped package under it
func nodePackageOf(modulePath string) (root string, pkg string, ok bool) {
	parts := pathParts(modulePath)
	at := -1
	for i, part := range parts {
		if part == "node_modules" {
			at = i
		}
	}
	if at < 0 || at+1 >= len(parts) {
		return "", "", false
	}
	pkg = parts[at+1]
	if strings.HasPrefix(pkg, "@") && at+2 < len(parts) {
		pkg = pkg + "/" + parts[at+2]
	}
	root = filepath.Join(parts[:at]...)
	if root == "" {
		root = "."
	}
	return root, pkg, true
}

// Characters a POSIX shell word may hold without quoting
var shellSafeWord = regexp.MustCompile(`^[\w@%+=:,./-]+$`)

// Quotes one word for a POSIX shell the way shlex.quote does
func shlexQuote(s string) string {
	if s == "" {
		return "''"
	}
	if shellSafeWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// Quotes one word for PowerShell single quoted literals
func powershellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// Fix in the host shell syntax, deleting an npx cache entry or rebuilding with the managed npm first on PATH
func nodeABIRemedy(root, pkg string, hasRoot bool, node, npm string) string {
	windows := runtime.GOOS == "windows"
	q := shlexQuote
	if windows {
		q = powershellQuote
	}
	target := ""
	if hasRoot {
		target = fmt.Sprintf(" %s --prefix %s", q(pkg), q(root))
	}
	var rebuild string
	if windows {
		rebuild = fmt.Sprintf("$env:Path = %s + $env:Path; & %s rebuild%s", q(filepath.Dir(node)+";"), q(npm), target)
	} else {
		rebuild = fmt.Sprintf("PATH=%s:\"$PATH\" %s rebuild%s", q(filepath.Dir(node)), q(npm), target)
	}
	if !hasRoot {
		return "in the server's package directory run " + rebuild
	}
	if filepath.Base(filepath.Dir(root)) != "_npx" {
		return rebuild
	}
	del := "rm -rf " + q(root)
	if windows {
		del = "Remove-Item -Recurse -Force " + q(root)
	}
	return del + " (Icebath reinstalls it on the next start), or " + rebuild
}

// Remedy naming error for a server whose stderr shows a native addon load failure, nil otherwise
func nodeABIError(serverName, stderrText string) *NodeABIMismatchError {
	if stderrText == "" || !containsAnyMarker(stderrText, nodeABIMarkers) {
		return nil
	}
	module := ""
	if m := nodeABIModulePath.FindStringSubmatch(stderrText); m != nil {
		module = m[1]
		if module == "" {
			module = m[2]
		}
	}
	node, nodeVersion := nodeABIManaged("node")
	npm, _ := nodeABIManaged("npm")
	ours := "Icebath's Node"
	if nodeVersion != "" {
		ours = strings.TrimSpace("Icebath's Node " + nodeVersion)
	}
	built := "it does not load under " + ours
	if v := nodeABIVersions.FindStringSubmatch(stderrText); v != nil {
		built = fmt.Sprintf("NODE_MODULE_VERSION %s; %s needs %s", v[1], ours, v[2])
	}
	shown := module
	if shown == "" {
		shown = "see logs/mcp-stderr.log"
	}
	head := fmt.Sprintf("native addon built for a different Node.js (%s): %s. Icebath runs MCP servers on its own Node; rebuild the server under it", built, shown)
	if node == "" || npm == "" {
		return &NodeABIMismatchError{Msg: fmt.Sprintf("%s: install Icebath's Node with `icebath pm install npm`, then run `icebath mcp test %s`", head, serverName)}
	}
	root, pkg, hasRoot := "", "", false
	if module != "" {
		root, pkg, hasRoot = nodePackageOf(module)
	}
	return &NodeABIMismatchError{Msg: head + ": " + nodeABIRemedy(root, pkg, hasRoot, node, npm)}
}

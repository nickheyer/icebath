package mcp

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Installs a managed node and npm the way the package manager reports them
func managedNode(t *testing.T) (node, npm string) {
	store := filepath.Join(t.TempDir(), "tools")
	node = filepath.Join(store, "node-26.7.0-linux-x64", "bin", "node")
	npm = filepath.Join(store, "npm-12.0.2-linux-x64", "bin", "npm")
	prev := nodeABIManaged
	nodeABIManaged = func(name string) (string, string) {
		switch name {
		case "node":
			return node, "26.7.0"
		case "npm":
			return npm, "12.0.2"
		}
		return "", ""
	}
	t.Cleanup(func() { nodeABIManaged = prev })
	return node, npm
}

func abiStderr(addon string) string {
	return "node:internal/modules/cjs/loader:1921\n  return process.dlopen(module, path.toNamespacedPath(filename));\n\n" +
		"Error: The module '" + addon + "'\nwas compiled against a different Node.js version using\n" +
		"NODE_MODULE_VERSION 127. This version of Node.js requires\nNODE_MODULE_VERSION 147. Please try " +
		"re-compiling or re-installing\n  code: 'ERR_DLOPEN_FAILED'\n}\n\nNode.js v26.7.0\n"
}

func TestNodeABINpxCacheEntryNamesDeleteAndRebuild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell remedy")
	}
	node, npm := managedNode(t)
	entry := filepath.Join(t.TempDir(), ".npm", "_npx", "0e2eab1c")
	addon := filepath.Join(entry, "node_modules", "better-sqlite3", "build", "Release", "better_sqlite3.node")
	err := nodeABIError("sqlite", abiStderr(addon))
	if err == nil {
		t.Fatal("ABI stderr must produce an error")
	}
	reason := formatConnectError(err)
	for _, want := range []string{
		"NODE_MODULE_VERSION 127; Icebath's Node 26.7.0 needs 147",
		"rm -rf " + entry,
		"PATH=" + filepath.Dir(node) + ":\"$PATH\" " + npm + " rebuild better-sqlite3 --prefix " + entry,
		"(Icebath reinstalls it on the next start), or ",
		": " + addon + ". Icebath runs MCP servers on its own Node; rebuild the server under it: ",
	} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason lacks %q:\n%s", want, reason)
		}
	}
	if classifyMCPFailure(err) != "permanent" {
		t.Fatal("every retry loads the same binary, the failure must be permanent")
	}
}

func TestNodeABIRemedyNeverPointsTheServerAtAnotherNode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell remedy")
	}
	node, npm := managedNode(t)
	pkgRoot := "/usr/lib/node_modules/@acme/mcp-server"
	stderr := "Error: The module '" + pkgRoot + "/node_modules/sharp/build/Release/sharp.node'\nwas compiled against a " +
		"different Node.js version using\nNODE_MODULE_VERSION 127. This version of Node.js requires\n" +
		"NODE_MODULE_VERSION 147.\n  code: 'ERR_DLOPEN_FAILED'\n"
	message := nodeABIError("imgs", stderr).Error()
	if !strings.Contains(message, npm) || !strings.Contains(message, filepath.Dir(node)) || !strings.Contains(message, " rebuild sharp --prefix ") {
		t.Fatalf("rebuild under the managed node: %s", message)
	}
	if !strings.Contains(message, pkgRoot) || strings.Contains(message, "rm -rf") || strings.Contains(message, "command:") {
		t.Fatalf("no cache delete and no command pin: %s", message)
	}
}

func TestNodeABIShapes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell remedy")
	}
	if nodeABIError("s", "") != nil || nodeABIError("s", "Error: Cannot find module 'x'") != nil {
		t.Fatal("stderr without ABI markers is not an ABI error")
	}
	node, npm := managedNode(t)
	scoped := nodeABIError("s", "Error: /srv/app/node_modules/@scope/pkg/build/addon.node: undefined symbol\n code: 'ERR_DLOPEN_FAILED'").Error()
	if !strings.Contains(scoped, "it does not load under Icebath's Node 26.7.0") || !strings.Contains(scoped, " rebuild @scope/pkg --prefix /srv/app") ||
		!strings.Contains(scoped, ": /srv/app/node_modules/@scope/pkg/build/addon.node. ") {
		t.Fatalf("scoped package without versions: %s", scoped)
	}
	bare := nodeABIError("s", "NODE_MODULE_VERSION mismatch").Error()
	if !strings.Contains(bare, ": see logs/mcp-stderr.log. ") ||
		!strings.HasSuffix(bare, ": in the server's package directory run PATH="+filepath.Dir(node)+":\"$PATH\" "+npm+" rebuild") {
		t.Fatalf("module unknown: %s", bare)
	}
	spaced := nodeABIError("s", "Error: The module '/home/a b/node_modules/x/x.node'\nERR_DLOPEN_FAILED").Error()
	if !strings.Contains(spaced, " rebuild x --prefix '/home/a b'") {
		t.Fatalf("paths are shell quoted: %s", spaced)
	}
	nodeABIManaged = func(string) (string, string) { return "", "" }
	missing := nodeABIError("sqlite", abiStderr("/x/node_modules/y/y.node")).Error()
	if !strings.Contains(missing, "NODE_MODULE_VERSION 127; Icebath's Node needs 147") ||
		!strings.HasSuffix(missing, ": install Icebath's Node with `icebath pm install npm`, then run `icebath mcp test sqlite`") {
		t.Fatalf("no managed node: %s", missing)
	}
	if root, pkg, ok := nodePackageOf("node_modules/x/x.node"); !ok || root != "." || pkg != "x" {
		t.Fatalf("relative module %q %q %v", root, pkg, ok)
	}
	if _, _, ok := nodePackageOf("/a/node_modules"); ok {
		t.Fatal("node_modules without a package names none")
	}
}

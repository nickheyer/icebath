package pm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
)

// Pins and records an already installed tool as the PM store entry of name, linking its companions beside it
func registerInstalledNodeTool(t *testing.T, name, executable string, companions ...string) string {
	t.Helper()
	ctx := context.Background()
	out, err := exec.Command(executable, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	version := strings.TrimPrefix(strings.TrimSpace(string(out)), "v")
	pkg, err := GetPackage(name)
	if err != nil {
		t.Fatal(err)
	}
	target, err := CurrentTarget()
	if err != nil {
		t.Fatal(err)
	}
	store, err := StoreRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(store, pkg.StoreEntry(version, target))
	binary := pkg.Binary(entry, target)
	if binary == "" {
		t.Fatalf("%s has no binary", name)
	}
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, binary); err != nil {
		t.Fatal(err)
	}
	for _, companion := range companions {
		if err := os.Symlink(companion, filepath.Join(filepath.Dir(binary), filepath.Base(companion))); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	lock := NewLockfile(LockfilePath())
	lock.SetPin(name, version, map[string]any{target: map[string]any{"url": (&url.URL{Scheme: "file", Path: executable}).String(), "sha256": digest}})
	if err := lock.Save(); err != nil {
		t.Fatal(err)
	}
	factsPath, err := FactsPath(ctx)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := NewFacts(factsPath, false)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := TreeDigest(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := facts.Record(name, version, filepath.Base(entry), pkg.Env(entry, target), store,
		FactRecord{Target: &target, Artifacts: []string{digest}, Digest: &tree}); err != nil {
		t.Fatal(err)
	}
	return binary
}

// Home whose PM store holds the Node already on PATH, with the lockfile moved under the test dir
func nodeStore(t *testing.T) (string, string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("platforms(posix)")
	}
	node, ok := pyshutil.Which("node", nil)
	if !ok {
		t.Skip("requires an already-installed Node executable")
	}
	absolute, _ := filepath.Abs(node)
	if realHome, err := os.UserHomeDir(); err == nil && strings.HasPrefix(absolute, filepath.Join(realHome, ".icebath")+string(filepath.Separator)) {
		t.Skip("requires a Node binary outside the real Icebath home")
	}
	tmp := t.TempDir()
	t.Setenv("PATH", filepath.Dir(node))
	h := filepath.Join(tmp, "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv(EnvRuntimeDir, filepath.Join(h, "tools"))
	t.Setenv(EnvDisableLazyInstalls, "1")
	previous := lockfilePath
	lockPath := filepath.Join(tmp, "lock.json")
	lockfilePath = func() string { return lockPath }
	t.Cleanup(func() { lockfilePath = previous })
	return h, registerInstalledNodeTool(t, "node", node), node
}

// Store facts bytes
func factsBytes(t *testing.T) []byte {
	t.Helper()
	p, err := FactsPath(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPmNodeWinsOverLegacyTreeAndComposesChildEnvironment(t *testing.T) {
	h, binary, external := nodeStore(t)
	ctx := context.Background()
	legacy := filepath.Join(h, "node", "bin", "node")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, legacy); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(external))
	before := factsBytes(t)
	selected, err := Installed(ctx, "node", false)
	if err != nil || selected == nil || selected.Binary != binary {
		t.Fatalf("selected %v %v", selected, err)
	}
	resolved, err := FindNodeExecutable(ctx, "node")
	if err != nil || resolved != binary {
		t.Fatalf("resolved %q %v", resolved, err)
	}
	base := map[string]string{"PATH": filepath.Dir(external), "CALLER_VALUE": "preserved"}
	environment := WithIcebathNodePath(base)
	if !maps.Equal(environment, EnvFor([]string{"npm"}, base)) {
		t.Fatalf("environment %v", environment)
	}
	searchPath := environment["PATH"]
	if found, _ := pyshutil.Which("node", &searchPath); found != binary {
		t.Fatalf("node on the composed PATH %q", found)
	}
	child := exec.Command(resolved, "-p", "process.version")
	for k, v := range environment {
		child.Env = append(child.Env, k+"="+v)
	}
	out, err := child.Output()
	if err != nil || strings.TrimPrefix(strings.TrimSpace(string(out)), "v") != selected.Version {
		t.Fatalf("child %q %v want %s", out, err, selected.Version)
	}
	if !bytes.Equal(factsBytes(t), before) {
		t.Fatal("lookup changed the store facts")
	}
	if st, err := os.Stat(legacy); err != nil || !st.Mode().IsRegular() {
		t.Fatal("legacy tree was touched")
	}
	if !maps.Equal(base, map[string]string{"PATH": filepath.Dir(external), "CALLER_VALUE": "preserved"}) {
		t.Fatalf("base mutated %v", base)
	}
}

// Contents of every file under root keyed by relative path
func treeFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			data, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(root, p)
			out[rel] = string(data)
		}
		return nil
	})
	return out
}

func TestPassiveDiscoveryNeverInstallsOrRepairsALegacyTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platforms(posix)")
	}
	ctx := context.Background()
	tmp := t.TempDir()
	h := filepath.Join(tmp, "home")
	legacy := filepath.Join(h, "node", "bin", "node")
	writeFile(t, legacy, []byte("#!/bin/sh\nexit 1\n"), 0o755)
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv(EnvRuntimeDir, filepath.Join(h, "tools"))
	t.Setenv(EnvDisableLazyInstalls, "1")
	t.Setenv("PATH", "")
	before := treeFiles(t, h)
	for _, command := range []string{"node", "npm"} {
		if found, err := FindNodeExecutable(ctx, command); err != nil || found != "" {
			t.Fatalf("%s %q %v", command, found, err)
		}
	}
	if env := WithIcebathNodePath(map[string]string{"PATH": ""}); !maps.Equal(env, map[string]string{"PATH": ""}) {
		t.Fatalf("environment %v", env)
	}
	if !maps.Equal(treeFiles(t, h), before) {
		t.Fatal("passive lookup changed the home")
	}
	if _, err := os.Stat(filepath.Join(h, "tools")); err == nil {
		t.Fatal("passive lookup created the store")
	}
	external := filepath.Join(tmp, "external")
	writeFile(t, filepath.Join(external, "npm"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	t.Setenv("PATH", external)
	if found, err := FindNodeExecutable(ctx, "npm"); err != nil || found != "" {
		t.Fatalf("user PATH npm selected %q %v", found, err)
	}
	if _, err := os.Stat(filepath.Join(h, "tools")); err == nil {
		t.Fatal("passive lookup created the store")
	}
}

func TestExplicitNodePathIsNotReplacedByManagedName(t *testing.T) {
	_, _, external := nodeStore(t)
	ctx := context.Background()
	if found, err := FindNodeExecutable(ctx, external); err != nil || found != external {
		t.Fatalf("explicit %q %v", found, err)
	}
	if found, err := FindNodeExecutable(ctx, filepath.Join(filepath.Dir(external), "missing-node")); err != nil || found != "" {
		t.Fatalf("missing %q %v", found, err)
	}
	if found, err := FindNodeExecutable(ctx, "not-a-node-command"); err != nil || found != "" {
		t.Fatalf("unknown %q %v", found, err)
	}
}

func TestNpmAndNpxUseThePairedPmEntry(t *testing.T) {
	h, node, external := nodeStore(t)
	ctx := context.Background()
	npm := filepath.Join(filepath.Dir(external), "npm")
	npx := filepath.Join(filepath.Dir(external), "npx")
	if !regularFile(npm) || !regularFile(npx) {
		t.Skip("requires already-installed npm and npx")
	}
	binary := registerInstalledNodeTool(t, "npm", npm, npx)
	companion := filepath.Join(filepath.Dir(binary), "npx")
	legacy := filepath.Join(h, "node", "bin", "npm")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(npm, legacy); err != nil {
		t.Fatal(err)
	}
	before := factsBytes(t)
	t.Setenv("PATH", "")
	if found, err := FindNodeExecutable(ctx, "npm"); err != nil || found != binary {
		t.Fatalf("npm %q %v", found, err)
	}
	if found, err := FindNodeExecutable(ctx, "npx"); err != nil || found != companion {
		t.Fatalf("npx %q %v", found, err)
	}
	environment := WithIcebathNodePath(map[string]string{"PATH": ""})
	searchPath := environment["PATH"]
	if found, _ := pyshutil.Which("npm", &searchPath); found != binary {
		t.Fatalf("npm on the composed PATH %q", found)
	}
	if found, _ := pyshutil.Which("node", &searchPath); found != node {
		t.Fatalf("node on the composed PATH %q", found)
	}
	pinned, _ := NewLockfile(LockfilePath()).Version("npm")
	for _, command := range []string{"npm", "npx"} {
		resolved, err := FindNodeExecutable(ctx, command)
		if err != nil || resolved == "" {
			t.Fatalf("%s %q %v", command, resolved, err)
		}
		child := exec.Command(resolved, "--version")
		for k, v := range environment {
			child.Env = append(child.Env, k+"="+v)
		}
		out, err := child.Output()
		if err != nil || strings.TrimSpace(string(out)) != pinned {
			t.Fatalf("%s --version %q %v want %s", command, out, err, pinned)
		}
	}
	if !bytes.Equal(factsBytes(t), before) {
		t.Fatal("lookup changed the store facts")
	}
	if err := os.Remove(companion); err != nil {
		t.Fatal(err)
	}
	if found, err := FindNodeExecutable(ctx, "npx"); err != nil || found != "" {
		t.Fatalf("npx without its companion %q %v", found, err)
	}
}

func TestWindowsPathPrefersLaunchableCmdButNeverForPmTools(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("platforms(windows)")
	}
	tmp := t.TempDir()
	t.Setenv(EnvRuntimeDir, filepath.Join(tmp, "absent-store"))
	t.Setenv("PATH", tmp)
	for _, tool := range []string{"npm", "pnpm"} {
		for _, name := range []string{tool, tool + ".ps1", tool + ".cmd"} {
			writeFile(t, filepath.Join(tmp, name), []byte("@exit /b 0\n"), 0o644)
		}
	}
	ctx := context.Background()
	if found, err := FindNodeExecutable(ctx, "pnpm"); err != nil || found != filepath.Join(tmp, "pnpm.cmd") {
		t.Fatalf("pnpm %q %v", found, err)
	}
	if found, err := FindNodeExecutable(ctx, "npm"); err != nil || found != "" {
		t.Fatalf("npm %q %v", found, err)
	}
}

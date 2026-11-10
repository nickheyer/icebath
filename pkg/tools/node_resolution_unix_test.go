//go:build unix

package tools

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/lsp"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Trimmed stdout of argv run under env within ten seconds, failing the test on a nonzero exit
func nodeToolOutput(t *testing.T, env map[string]string, argv ...string) string {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	if env != nil {
		cmd.Env = []string{}
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%v: %v", argv, err)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("%v timed out", argv)
	}
	return strings.TrimSpace(out.String())
}

// Registers an already installed tool as a PM store entry with its lock pin and installed fact
func registerInstalledTool(t *testing.T, name, executable string, companions ...string) string {
	t.Helper()
	ctx := t.Context()
	version := strings.TrimPrefix(nodeToolOutput(t, nil, executable, "--version"), "v")
	pkg, err := pm.GetPackage(name)
	if err != nil {
		t.Fatal(err)
	}
	target, err := pm.CurrentTarget()
	if err != nil {
		t.Fatal(err)
	}
	store, err := pm.StoreRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(store, pkg.StoreEntry(version, target))
	binary := pkg.Binary(entry, target)
	if binary == "" {
		t.Fatalf("%s has no binary", name)
	}
	if _, err := os.Lstat(filepath.Dir(binary)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s exists already: %v", filepath.Dir(binary), err)
	}
	if err := os.MkdirAll(filepath.Dir(binary), 0o777); err != nil {
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
	uri, err := pypath.AsURI(executable)
	if err != nil {
		t.Fatal(err)
	}
	lock := pm.NewLockfile(pm.LockfilePath())
	lock.SetPin(name, version, map[string]any{target: map[string]any{"url": uri, "sha256": digest}})
	if err := lock.Save(); err != nil {
		t.Fatal(err)
	}
	factsPath, err := pm.FactsPath(ctx)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := pm.NewFacts(factsPath, false)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := pm.TreeDigest(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := facts.Record(name, version, filepath.Base(entry), pkg.Env(entry, target), store,
		pm.FactRecord{Target: &target, Artifacts: []string{digest}, Digest: &tree}); err != nil {
		t.Fatal(err)
	}
	return binary
}

// Temp home whose PM store holds the Node found on PATH, with home, store binary and external path
func nodeStore(t *testing.T) (string, string, string) {
	t.Helper()
	node, ok := pyshutil.Which("node", nil)
	if !ok {
		t.Skip("requires an already-installed Node executable")
	}
	abs, err := filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, inside := pypath.PathRelativeTo(abs, filepath.Join(userHome, ".icebath")); inside {
		t.Skip("requires a Node binary outside the real Icebath home")
	}
	t.Setenv("PATH", filepath.Dir(node))
	tmp := t.TempDir()
	h := filepath.Join(tmp, "home")
	if err := os.Mkdir(h, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv(pm.EnvRuntimeDir, filepath.Join(h, "tools"))
	t.Setenv(pm.EnvDisableLazyInstalls, "1")
	lockPath := filepath.Join(tmp, "lock.json")
	prev := pm.SwapLockfilePath(func() string { return lockPath })
	t.Cleanup(func() { pm.SwapLockfilePath(prev) })
	return h, registerInstalledTool(t, "node", node), node
}

// Node finder of the LSP layer wired to PM the way a session wires it
func findNode(t *testing.T, rt *lsp.Runtime, command string) string {
	t.Helper()
	found, err := lsp.FindNodeExecutable(rt, command)
	if err != nil {
		t.Fatalf("find %s: %v", command, err)
	}
	return found
}

// Bytes of the installed facts file
func factsBytes(t *testing.T) []byte {
	t.Helper()
	path, err := pm.FactsPath(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Search of an explicit PATH value, empty when the command is absent
func whichOn(command, path string) string {
	found, _ := pyshutil.Which(command, &path)
	return found
}

// Contents of every regular file below root keyed by its relative path
func fileSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			out[rel] = string(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPmNodeWinsOverLegacyTreeAndComposesChildEnvironment(t *testing.T) {
	h, binary, external := nodeStore(t)
	legacy := filepath.Join(h, "node", "bin", "node")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, legacy); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(external))
	before := factsBytes(t)

	selected, err := pm.Installed(t.Context(), "node", false)
	if err != nil || selected == nil || selected.Binary != binary {
		t.Fatalf("selected %+v, %v, want binary %s", selected, err, binary)
	}
	rt := LSPRuntime(&Env{Home: h})
	resolved := findNode(t, rt, "node")
	if resolved == "" || resolved != binary {
		t.Fatalf("resolved %q, want %q", resolved, binary)
	}
	base := map[string]string{"PATH": filepath.Dir(external), "CALLER_VALUE": "preserved"}
	environment := pm.WithIcebathNodePath(base)
	if want := pm.EnvFor([]string{"npm"}, base); !reflect.DeepEqual(environment, want) {
		t.Fatalf("environment %v, want %v", environment, want)
	}
	if got := whichOn("node", environment["PATH"]); got != binary {
		t.Fatalf("node on the composed PATH is %q, want %q", got, binary)
	}
	if got := strings.TrimPrefix(nodeToolOutput(t, environment, resolved, "-p", "process.version"), "v"); got != selected.Version {
		t.Fatalf("child reports %q, want %q", got, selected.Version)
	}
	if !bytes.Equal(factsBytes(t), before) {
		t.Fatal("discovery rewrote the facts file")
	}
	if st, err := os.Stat(legacy); err != nil || !st.Mode().IsRegular() {
		t.Fatalf("legacy node gone: %v", err)
	}
	if !reflect.DeepEqual(base, map[string]string{"PATH": filepath.Dir(external), "CALLER_VALUE": "preserved"}) {
		t.Fatalf("base mutated: %v", base)
	}
}

func TestPassiveDiscoveryNeverInstallsOrRepairsALegacyTree(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, "home")
	legacy := filepath.Join(h, "node", "bin", "node")
	writeExecutable(t, legacy, "#!/bin/sh\nexit 1\n")
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv(pm.EnvRuntimeDir, filepath.Join(h, "tools"))
	t.Setenv(pm.EnvDisableLazyInstalls, "1")
	t.Setenv("PATH", "")
	before := fileSnapshot(t, h)
	var attempts []string
	rt := LSPRuntime(&Env{Home: h})
	rt.EnsureEnv = func(name string, _ map[string]string) (map[string]string, error) {
		attempts = append(attempts, name)
		return nil, errors.New("passive lookup must not provision anything")
	}
	if got := findNode(t, rt, "node"); got != "" {
		t.Fatalf("node = %q", got)
	}
	if got := findNode(t, rt, "npm"); got != "" {
		t.Fatalf("npm = %q", got)
	}
	if got := pm.WithIcebathNodePath(map[string]string{"PATH": ""}); !reflect.DeepEqual(got, map[string]string{"PATH": ""}) {
		t.Fatalf("composed env %v", got)
	}
	if len(attempts) != 0 {
		t.Fatalf("install attempts %v", attempts)
	}
	if after := fileSnapshot(t, h); !reflect.DeepEqual(after, before) {
		t.Fatalf("home changed: %v", after)
	}
	if _, err := os.Lstat(filepath.Join(h, "tools")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("store created: %v", err)
	}

	external := filepath.Join(tmp, "external")
	writeExecutable(t, filepath.Join(external, "npm"), "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", external)
	if got := findNode(t, rt, "npm"); got != "" {
		t.Fatalf("a PATH npm was selected: %q", got)
	}
	if len(attempts) != 0 {
		t.Fatalf("install attempts %v", attempts)
	}
	if _, err := os.Lstat(filepath.Join(h, "tools")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("store created: %v", err)
	}
}

func TestExplicitNodePathIsNotReplacedByManagedName(t *testing.T) {
	h, _, external := nodeStore(t)
	rt := LSPRuntime(&Env{Home: h})
	if got := findNode(t, rt, external); got != external {
		t.Fatalf("explicit path = %q, want %q", got, external)
	}
	if got := findNode(t, rt, filepath.Join(filepath.Dir(external), "missing-node")); got != "" {
		t.Fatalf("missing explicit path = %q", got)
	}
	if got := findNode(t, rt, "not-a-node-command"); got != "" {
		t.Fatalf("unknown command = %q", got)
	}
}

func TestNpmAndNpxUseThePairedPmEntry(t *testing.T) {
	h, node, external := nodeStore(t)
	npm, npx := filepath.Join(filepath.Dir(external), "npm"), filepath.Join(filepath.Dir(external), "npx")
	for _, p := range []string{npm, npx} {
		if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() {
			t.Skip("requires already-installed npm and npx")
		}
	}
	binary := registerInstalledTool(t, "npm", npm, npx)
	companion := filepath.Join(filepath.Dir(binary), "npx")
	legacy := filepath.Join(h, "node", "bin", "npm")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(npm, legacy); err != nil {
		t.Fatal(err)
	}
	before := factsBytes(t)
	t.Setenv("PATH", "")

	rt := LSPRuntime(&Env{Home: h})
	if got := findNode(t, rt, "npm"); got != binary {
		t.Fatalf("npm = %q, want %q", got, binary)
	}
	if got := findNode(t, rt, "npx"); got != companion {
		t.Fatalf("npx = %q, want %q", got, companion)
	}
	environment := pm.WithIcebathNodePath(map[string]string{"PATH": ""})
	if got := whichOn("npm", environment["PATH"]); got != binary {
		t.Fatalf("npm on the composed PATH is %q, want %q", got, binary)
	}
	if got := whichOn("node", environment["PATH"]); got != node {
		t.Fatalf("node on the composed PATH is %q, want %q", got, node)
	}
	for _, command := range []string{"npm", "npx"} {
		resolved := findNode(t, rt, command)
		if resolved == "" {
			t.Fatalf("%s unresolved", command)
		}
		want, _ := pm.NewLockfile(pm.LockfilePath()).Version("npm")
		if got := nodeToolOutput(t, environment, resolved, "--version"); got != want {
			t.Fatalf("%s --version = %q, want %q", command, got, want)
		}
	}
	if !bytes.Equal(factsBytes(t), before) {
		t.Fatal("discovery rewrote the facts file")
	}
	if err := os.Remove(companion); err != nil {
		t.Fatal(err)
	}
	if got := findNode(t, rt, "npx"); got != "" {
		t.Fatalf("npx without its companion = %q", got)
	}
}

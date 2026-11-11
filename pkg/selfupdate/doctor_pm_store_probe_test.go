package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Tool store fixture pinning the host ripgrep, with a hostile rg as the only PATH entry
type toolStore struct {
	primary       string
	versionOutput string
	hostile       string
	publish       func(root string) string
}

// Pins the host ripgrep in a temporary lockfile under a sealed payload store layout
func newToolStore(t *testing.T) toolStore {
	t.Helper()
	rg, err := exec.LookPath("rg")
	if err != nil {
		t.Fatal("this executable parity test requires ripgrep")
	}
	rawVersion, err := exec.Command(rg, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	versionOutput := string(rawVersion)
	version := strings.Fields(strings.SplitN(versionOutput, "\n", 2)[0])[1]
	tmp := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = resolved
	}
	icebathHome := filepath.Join(tmp, "home")
	primary := filepath.Join(tmp, "payload", "tools")
	if err := os.MkdirAll(primary, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(filepath.Dir(primary), "manifest.json"), "{}", 0o644)
	t.Setenv("ICEBATH_HOME", icebathHome)
	t.Setenv(pm.EnvRuntimeDir, primary)
	t.Setenv(pm.EnvDisableLazyInstalls, "1")
	t.Setenv("HOME", tmp)
	t.Cleanup(home.PatchUserHome(tmp))
	home.ResetKeyCache()
	lockPath := filepath.Join(tmp, "lock.json")
	previous := pm.SwapLockfilePath(func() string { return lockPath })
	t.Cleanup(func() { pm.SwapLockfilePath(previous) })
	target, err := pm.CurrentTarget()
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := pm.GetPackage("ripgrep")
	if err != nil {
		t.Fatal(err)
	}
	rgBytes, err := os.ReadFile(rg)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(rgBytes)
	digest := hex.EncodeToString(sum[:])
	lock := pm.NewLockfile(lockPath)
	lock.SetPin("ripgrep", version, map[string]any{target: map[string]any{"url": (&url.URL{Scheme: "file", Path: rg}).String(), "sha256": digest}})
	if err := lock.Save(); err != nil {
		t.Fatal(err)
	}
	publish := func(root string) string {
		entry := filepath.Join(root, pkg.StoreEntry(version, target))
		binary := pkg.Binary(entry, target)
		if binary == "" {
			t.Fatal("ripgrep has no binary for this target")
		}
		if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(rg, binary); err != nil {
			t.Fatal(err)
		}
		facts, err := pm.NewFacts(filepath.Join(root, "facts.json"), false)
		if err != nil {
			t.Fatal(err)
		}
		tree, err := pm.TreeDigest(entry)
		if err != nil {
			t.Fatal(err)
		}
		if err := facts.Record("ripgrep", version, filepath.Base(entry), pkg.Env(entry, target), root,
			pm.FactRecord{Target: &target, Artifacts: []string{digest}, Digest: &tree}); err != nil {
			t.Fatal(err)
		}
		return binary
	}
	hostile := filepath.Join(tmp, "hostile", "rg")
	writeTestFile(t, hostile, "#!/bin/sh\nprintf 'hostile PATH ripgrep\\n'\n", 0o755)
	t.Setenv("PATH", filepath.Dir(hostile))
	return toolStore{primary, versionOutput, hostile, publish}
}

// Output of rg --version resolved and run under a composed environment
func runStoreRg(t *testing.T, env map[string]string) string {
	t.Helper()
	path := env["PATH"]
	rg, ok := pyshutil.Which("rg", &path)
	if !ok {
		t.Fatalf("rg is not on %q", path)
	}
	cmd := exec.Command(rg, "--version")
	cmd.Env = doctorEnvList(env)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestSealedStoreExtensionMatchesLaunchedTool(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix only")
	}
	for _, primaryState := range []string{"absent", "stale"} {
		t.Run(primaryState, func(t *testing.T) {
			store := newToolStore(t)
			ctx := context.Background()
			if primaryState == "stale" {
				store.publish(store.primary)
				factsPath := filepath.Join(store.primary, "facts.json")
				var data map[string]any
				raw, _ := os.ReadFile(factsPath)
				if err := json.Unmarshal(raw, &data); err != nil {
					t.Fatal(err)
				}
				data["packages"].(map[string]any)["ripgrep"].(map[string]any)["version"] = "0.0.0"
				raw, _ = json.Marshal(data)
				writeTestFile(t, factsPath, string(raw), 0o644)
			}
			writable, err := pm.WritableStoreRoot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			binary := store.publish(writable)
			snapshot := func() map[string]string {
				files := treeBytes(t, store.primary)
				for path, body := range treeBytes(t, writable) {
					files[path] = body
				}
				return files
			}
			before := snapshot()
			modes := map[string]fs.FileMode{}
			filepath.WalkDir(store.primary, func(path string, entry fs.DirEntry, err error) error {
				if err == nil && entry.Type()&fs.ModeSymlink == 0 {
					if info, statErr := os.Stat(path); statErr == nil {
						modes[path] = info.Mode().Perm()
					}
				}
				return nil
			})
			for path, mode := range modes {
				os.Chmod(path, mode&^0o222)
			}
			defer func() {
				for path, mode := range modes {
					os.Chmod(path, mode)
				}
			}()
			selected, err := pm.Installed(ctx, "ripgrep", false)
			if err != nil || selected == nil || selected.Binary != binary {
				t.Fatalf("selected = %+v, err = %v, want binary %q", selected, err, binary)
			}
			env := pm.EnvFor([]string{"ripgrep"}, map[string]string{"PATH": filepath.Dir(store.hostile)})
			path := env["PATH"]
			if found, _ := pyshutil.Which("rg", &path); found != binary {
				t.Fatalf("rg on the composed PATH = %q", found)
			}
			if got := runStoreRg(t, env); got != store.versionOutput {
				t.Fatalf("launched rg printed %q", got)
			}
			if tool, detail := doctorTool(ctx, "rg"); tool != binary || detail != "(pm store)" {
				t.Fatalf("doctor tool = %q %q", tool, detail)
			}
			d, out := toolsDoctorKeepingEnv(t)
			if _, err := checkGitAndRg(ctx, d, false); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "✓ ripgrep (rg) (pm store)") {
				t.Fatalf("output:\n%s", out.String())
			}
			if after := snapshot(); !reflect.DeepEqual(before, after) {
				t.Fatal("reading the selection changed a store")
			}
		})
	}
}

func TestRejectedStoreIsNotReportedAsRuntimeAvailable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix only")
	}
	for _, damage := range []string{"version", "target", "artifacts", "missing-binary", "sparse", "malformed"} {
		t.Run(damage, func(t *testing.T) {
			store := newToolStore(t)
			ctx := context.Background()
			binary := store.publish(store.primary)
			factsPath := filepath.Join(store.primary, "facts.json")
			var data map[string]any
			raw, _ := os.ReadFile(factsPath)
			if err := json.Unmarshal(raw, &data); err != nil {
				t.Fatal(err)
			}
			packages := data["packages"].(map[string]any)
			fact := packages["ripgrep"].(map[string]any)
			switch damage {
			case "missing-binary":
				if err := os.Remove(binary); err != nil {
					t.Fatal(err)
				}
			case "sparse":
				packages["ripgrep"] = map[string]any{"entry": fact["entry"]}
			case "artifacts":
				fact[damage] = []any{"wrong-artifact"}
			case "version", "target":
				fact[damage] = "outdated"
			}
			body := "not JSON"
			if damage != "malformed" {
				encoded, _ := json.Marshal(data)
				body = string(encoded)
			}
			writeTestFile(t, factsPath, body, 0o644)
			if selected, err := pm.Installed(ctx, "ripgrep", false); err != nil || selected != nil {
				t.Fatalf("selected = %+v, err = %v", selected, err)
			}
			if staged := doctorPMToolPath(ctx, "ripgrep"); staged != "" {
				t.Fatalf("pm tool path = %q", staged)
			}
			if tool, detail := doctorTool(ctx, "rg"); tool != store.hostile || detail != "" {
				t.Fatalf("doctor tool = %q %q", tool, detail)
			}
			if got := runStoreRg(t, pm.EnvFor([]string{"ripgrep"}, nil)); strings.TrimSpace(got) != "hostile PATH ripgrep" {
				t.Fatalf("launched rg printed %q", got)
			}
			d, out := toolsDoctorKeepingEnv(t)
			if _, err := checkGitAndRg(ctx, d, false); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "✓ ripgrep (rg)") || strings.Contains(out.String(), "(pm store)") {
				t.Fatalf("output:\n%s", out.String())
			}
		})
	}
}

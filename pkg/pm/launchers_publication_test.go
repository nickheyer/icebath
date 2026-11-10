package pm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Root an install binary stand in boots from, printing what it ran with and exiting 7
const publicationProbe = "ICEBATH_PM_PUBLICATION_PROBE"

// Boots the selected generation of root and reports its probe value, argv, home and executable
func publicationProbeMain(root string) int {
	PatchRepoRoot(root)
	if err := ActivateDependencies(context.Background(), root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	value := ""
	for _, entry := range importPathEntries() {
		if data, err := os.ReadFile(filepath.Join(entry, "selected_probe.py")); err == nil {
			value = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(string(data)), "VALUE = '"), "'")
		}
	}
	encoded, _ := json.Marshal(map[string]any{"value": value, "argv": os.Args[1:], "home": os.Getenv("ICEBATH_HOME"), "exe": os.Args[0]})
	fmt.Println(string(encoded))
	return 7
}

// Selects a generation of repo whose site packages carry the probe value
func pubSelectGeneration(t *testing.T, repo, name, value string) string {
	t.Helper()
	ctx := context.Background()
	selected := filepath.Join(InstallStateDir(ctx, repo), "environments", name, "venv")
	site, err := SitePackages(selected)
	if err != nil {
		t.Fatal(err)
	}
	peWrite(t, filepath.Join(selected, "pyvenv.cfg"), "home = fixture\n")
	peWrite(t, filepath.Join(site, "selected_probe.py"), fmt.Sprintf("VALUE = '%s'\n", value))
	encoded, _ := json.Marshal(map[string]any{"packages": map[string]any{"venv": map[string]any{"environment": selected}}})
	peWrite(t, filepath.Join(InstallStateDir(ctx, repo), "facts.json"), string(encoded))
	return selected
}

func TestSyncMigratesOldStoreWrapperBeforePythonCollection(t *testing.T) {
	for _, create := range []bool{true, false} {
		t.Run(fmt.Sprint(create), func(t *testing.T) {
			tmp := t.TempDir()
			repo := filepath.Join(tmp, "source 'café repo")
			h := filepath.Join(tmp, ".icebath")
			t.Setenv("HOME", h)
			t.Setenv("ICEBATH_HOME", h)
			t.Setenv(EnvRuntimeDir, "")
			os.Unsetenv(EnvRuntimeDir)
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(repo, "icebath")
			if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o777); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(exe, binary); err != nil {
				data, _ := os.ReadFile(exe)
				peWrite(t, binary, string(data))
				os.Chmod(binary, 0o755)
			}
			pubSelectGeneration(t, repo, "shared", "ready")
			out := filepath.Join(h, ".local", "bin")
			if err := os.MkdirAll(out, 0o777); err != nil {
				t.Fatal(err)
			}
			store := filepath.Join(h, "tools")
			interpreter, _ := filepath.EvalSymlinks(peHostPython(t))
			for _, version := range []string{"python-A", "python-B"} {
				python := filepath.Join(store, version, "bin", "python3")
				if err := os.MkdirAll(filepath.Dir(python), 0o777); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(interpreter, python); err != nil {
					t.Fatal(err)
				}
				peWrite(t, filepath.Join(store, "facts.json"), fmt.Sprintf(`{"schema": 1, "packages": {"python": {"entry": %q}}}`, version))
				if version == "python-A" {
					if mintLauncher("icebath", repo, out, binary, store) == "" {
						t.Fatal("the earlier publication wrote no wrapper")
					}
					continue
				}
				if err := publishLaunchers(context.Background(), repo, create); err != nil {
					t.Fatal(err)
				}
				if !create {
					entries, _ := os.ReadDir(out)
					var names []string
					for _, e := range entries {
						names = append(names, e.Name())
					}
					sort.Strings(names)
					if strings.Join(names, ",") != "icebath" {
						t.Fatalf("repair published %v", names)
					}
					for _, absent := range []string{filepath.Join(h, "bin"), filepath.Join(h, "config.yaml"), filepath.Join(h, "skills")} {
						if _, err := os.Stat(absent); err == nil {
							t.Fatalf("bootstrap publication created %s", absent)
						}
					}
				}
			}
			if err := os.RemoveAll(filepath.Join(store, "python-A")); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(filepath.Join(out, "icebath"))
			cmd.Dir = tmp
			cmd.Env = append(os.Environ(), publicationProbe+"="+repo)
			stdout, err := cmd.Output()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 7 {
				t.Fatalf("wrapper run %v %s", err, stdout)
			}
			var report map[string]any
			if err := json.Unmarshal(stdout, &report); err != nil {
				t.Fatalf("wrapper output %q", stdout)
			}
			if report["value"] != "ready" || report["exe"] != binary {
				t.Fatalf("wrapper report %v", report)
			}
		})
	}
}

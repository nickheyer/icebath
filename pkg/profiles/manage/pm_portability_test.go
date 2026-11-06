package manage

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Points ICEBATH_HOME at a source-home under the test dir and returns it
func transferHome(t *testing.T) string {
	t.Helper()
	tmp := manageEnv(t)
	src := filepath.Join(tmp, "source-home")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	switchHome(t, src)
	return src
}

// Switches ICEBATH_HOME and drops the caches keyed by the previous home
func switchHome(t *testing.T, h string) {
	t.Helper()
	t.Setenv(home.EnvHome, h)
	profiles.ResetFileCache()
	profiles.ResetStandaloneMemo()
	home.ResetKeyCache()
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		writeFile(t, filepath.Join(root, filepath.FromSlash(rel)), content)
	}
}

// Top level components of every relative path, sorted
func topLevelParts(sets ...map[string]string) []string {
	seen := map[string]bool{}
	for _, s := range sets {
		for rel := range s {
			seen[strings.Split(rel, "/")[0]] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

func writeDistManifest(t *testing.T, dir, name string, owned []string) {
	t.Helper()
	m := profiles.NewDistributionManifest(name)
	m.DistributionOwned = owned
	if _, err := profiles.WriteManifest(dir, m); err != nil {
		t.Fatal(err)
	}
}

func TestTransfersPreserveUserFilesWithoutPortingPMState(t *testing.T) {
	for _, route := range []string{"default", "named", "distribution", "explicit"} {
		t.Run(route, func(t *testing.T) {
			src := transferHome(t)
			tmp := filepath.Dir(src)
			captureOut(t)
			ctx := context.Background()
			portable := map[string]string{
				"config.yaml":                    "model:\n  model: portable\n",
				"SOUL.md":                        "Be kind.\n",
				"plugins/example/facts.json":     "{\"owner\": \"plugin\"}\n",
				"skills/example/tools/helper.py": "print('portable tool')\n",
				"skills/example/cache/notes.txt": "user-authored data\n",
			}
			runtime := map[string]string{
				"installs/install-id/facts.json":                         "{\"environment\": \"C:/other-machine/generation\"}\n",
				"installs/install-id/environments/generation/pyvenv.cfg": "home = other-machine\n",
				"tools/uv/bin/uv":                 "machine executable\n",
				"cache/partials/transfer.partial": "unfinished transfer\n",
			}
			source := src
			if route == "named" {
				dir, err := profiles.GetProfileDir("porter")
				if err != nil {
					t.Fatal(err)
				}
				source = dir
			}
			writeFiles(t, source, portable)
			writeFiles(t, source, runtime)
			var archive string
			if route == "default" || route == "named" {
				name := map[bool]string{true: "porter", false: "default"}[route == "named"]
				out, err := ExportProfile(name, filepath.Join(tmp, "portable.tar.gz"), nil)
				if err != nil {
					t.Fatal(err)
				}
				archive = out
				files, _ := archiveMembers(t, archive)
				names := map[string]bool{}
				for n := range files {
					names[strings.TrimPrefix(n, name+"/")] = true
				}
				for rel := range portable {
					if !names[rel] {
						t.Fatalf("%s missing from the archive", rel)
					}
				}
				for rel := range runtime {
					if names[rel] {
						t.Fatalf("%s shipped in the archive", rel)
					}
				}
			} else {
				var owned []string
				if route == "explicit" {
					owned = topLevelParts(portable, runtime)
				}
				writeDistManifest(t, source, "portable", owned)
			}
			target := filepath.Join(tmp, "target-home")
			if err := os.Mkdir(target, 0o755); err != nil {
				t.Fatal(err)
			}
			switchHome(t, target)
			var restored string
			if archive != "" {
				out, err := ImportProfile(archive, "restored")
				if err != nil {
					t.Fatal(err)
				}
				restored = out
			} else {
				plan, err := InstallDistribution(ctx, source, "restored", false, false)
				if err != nil {
					t.Fatal(err)
				}
				restored = plan.TargetDir
			}
			for rel, content := range portable {
				if got := readText(t, filepath.Join(restored, filepath.FromSlash(rel))); got != content {
					t.Fatalf("%s %s = %q", route, rel, got)
				}
			}
			for rel, content := range runtime {
				if exists(filepath.Join(restored, filepath.FromSlash(rel))) {
					t.Fatalf("%s %s restored", route, rel)
				}
				if readText(t, filepath.Join(source, filepath.FromSlash(rel))) != content {
					t.Fatalf("%s source %s changed", route, rel)
				}
			}
		})
	}
}

// Writes a tar.gz of dir with every member under arcname like tarfile.add
func tarDirectory(t *testing.T, dir, arcname, out string) {
	t.Helper()
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		hdr.Name = arcname
		if rel != "." {
			hdr.Name = arcname + "/" + filepath.ToSlash(rel)
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.Type().IsRegular() {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			_, err = tw.Write(data)
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []interface{ Close() error }{tw, gz, f} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIncomingRuntimeStateNeverReplacesTargetRuntime(t *testing.T) {
	for _, route := range []string{"profile", "profile-denied", "distribution", "explicit"} {
		t.Run(route, func(t *testing.T) {
			src := transferHome(t)
			tmp := filepath.Dir(src)
			captureOut(t)
			ctx := context.Background()
			incoming := map[string]string{
				"config.yaml":                    "model:\n  model: restored\n",
				"plugins/example/facts.json":     "{\"owner\": \"plugin\"}\n",
				"skills/example/tools/helper.py": "print('user tool')\n",
				"skills/example/cache/notes.txt": "keep user files\n",
			}
			machine := map[string]string{
				"installs/key/facts.json": "foreign generation\n",
				"tools/uv/bin/uv":         "foreign executable\n",
				"cache":                   "foreign cache file\n",
			}
			staged := filepath.Join(tmp, "archive-source")
			writeFiles(t, staged, incoming)
			writeFiles(t, staged, machine)
			target, err := profiles.GetProfileDir("restored")
			if err != nil {
				t.Fatal(err)
			}
			profileRoute := strings.HasPrefix(route, "profile")
			if !profileRoute {
				for rel := range machine {
					writeFile(t, filepath.Join(target, filepath.FromSlash(rel)), "target-owned runtime\n")
				}
			}
			if profileRoute {
				archive := filepath.Join(tmp, "legacy.tar.gz")
				tarDirectory(t, staged, "legacy", archive)
				if route == "profile-denied" {
					old := rmtreeStagedRuntime
					rmtreeStagedRuntime = func(p string) error {
						if filepath.Base(p) == "installs" {
							return fmt.Errorf("staged runtime is locked: %w", fs.ErrPermission)
						}
						return old(p)
					}
					_, err := ImportProfile(archive, "restored")
					rmtreeStagedRuntime = old
					if !errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), "staged runtime is locked") {
						t.Fatalf("import: %v", err)
					}
					if exists(target) {
						t.Fatal("a refused import left the target behind")
					}
					return
				}
				if target, err = ImportProfile(archive, "restored"); err != nil {
					t.Fatal(err)
				}
			} else {
				var owned []string
				if route == "explicit" {
					owned = topLevelParts(incoming, machine)
				}
				writeDistManifest(t, staged, "legacy", owned)
				plan, err := InstallDistribution(ctx, staged, "restored", true, false)
				if err != nil {
					t.Fatal(err)
				}
				target = plan.TargetDir
			}
			for rel, content := range incoming {
				if got := readText(t, filepath.Join(target, filepath.FromSlash(rel))); got != content {
					t.Fatalf("%s %s = %q", route, rel, got)
				}
			}
			for rel, content := range machine {
				p := filepath.Join(target, filepath.FromSlash(rel))
				if route == "profile" {
					if exists(p) {
						t.Fatalf("%s imported", rel)
					}
				} else if readText(t, p) != "target-owned runtime\n" {
					t.Fatalf("%s %s replaced", route, rel)
				}
				if readText(t, filepath.Join(staged, filepath.FromSlash(rel))) != content {
					t.Fatalf("staged %s changed", rel)
				}
			}
			if route == "explicit" {
				var owned []string
				for rel := range incoming {
					owned = append(owned, strings.ReplaceAll(rel, "/", "\\"))
				}
				for rel := range machine {
					owned = append(owned, strings.ReplaceAll(rel, "/", "\\"))
				}
				writeDistManifest(t, staged, "legacy", owned)
				if _, err := InstallDistribution(ctx, staged, "restored", true, false); err != nil {
					t.Fatal(err)
				}
				for rel := range machine {
					if readText(t, filepath.Join(target, filepath.FromSlash(rel))) != "target-owned runtime\n" {
						t.Fatalf("explicit backslash owned %s replaced", rel)
					}
				}
			}
		})
	}
}
